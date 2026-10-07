// Package live keeps every open board page up to date: view = f(state).
//
// Commands change the database and mark boards dirty. Each board with
// viewers has a room whose loop renders the whole board once (at once when
// it was idle, then at most every Throttle) and offers the same bytes to
// every viewer. A viewer's stream only ever sends the newest frame: a slow
// client skips frames instead of queueing them. What is only for one tab
// (the outcome of its own action, its toasts) goes into that tab's outbox
// and is sent after a frame that shows the change, on the same stream.
package live

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Frame is one rendering of a board.
type Frame struct {
	Version int64
	// Parts are elements patched for every viewer (by id), in order.
	Parts [][]byte
	// Details are the card dialogs open in any tab, by card id; every
	// requested card has an entry.
	Details map[int64][]byte
	// Closed is the dialog when no card is open.
	Closed []byte
	// Board is patched after Parts, as one element, but a stream gets each
	// child in full only when it hasn't sent that exact child before:
	// otherwise the child's shell, which the morph skips. A change in one
	// lane then costs the browser one lane, not the whole board.
	Board *Keyed
	seq   int64

	// The events every stream sends, formatted once (see prepare).
	parts   []*Event
	details map[int64]*Event
	closed  *Event
	full    func() *Event // the board with every child, for a new stream
	delta   *Event        // the board against the frame before (nil: unchanged)
	base    int64         // seq of that frame
}

// prepare builds the frame's shared events; prev is the frame before.
func (f *Frame) prepare(prev *Frame) {
	f.parts = make([]*Event, len(f.Parts))
	for i, p := range f.Parts {
		f.parts[i] = Elements(p)
	}
	f.details = make(map[int64]*Event, len(f.Details))
	for k, d := range f.Details {
		f.details[k] = Elements(d)
	}
	f.closed = Elements(f.Closed)
	if f.Board == nil {
		return
	}
	f.full = sync.OnceValue(func() *Event { return Elements(f.Board.Full()) })
	if prev != nil && prev.Board != nil {
		f.base = prev.seq
		if b := f.Board.against(prev.Board.Children); b != nil {
			f.delta = Elements(b)
		}
	}
}

// Keyed is an element with keyed children (the board and its lanes).
type Keyed struct {
	Open, Close []byte
	Children    []Child
}

// Child is one keyed child: its markup, and the shell sent in its place
// when the stream already has it.
type Child struct {
	Key         int64
	HTML, Shell []byte
}

// Full is the element with every child in full (the page's first render).
func (k *Keyed) Full() []byte {
	n := len(k.Open) + len(k.Close)
	for _, c := range k.Children {
		n += len(c.HTML)
	}
	out := make([]byte, 0, n)
	out = append(out, k.Open...)
	for _, c := range k.Children {
		out = append(out, c.HTML...)
	}
	return append(out, k.Close...)
}

// against is the element for a stream that has sent the children had:
// the children it has unchanged go as shells. Nil when nothing changed.
func (k *Keyed) against(had []Child) []byte {
	prev := make(map[int64][]byte, len(had))
	for _, c := range had {
		prev[c.Key] = c.HTML
	}
	changed := len(had) != len(k.Children)
	n := len(k.Open) + len(k.Close)
	for _, c := range k.Children {
		if h, ok := prev[c.Key]; ok && bytes.Equal(h, c.HTML) {
			n += len(c.Shell)
		} else {
			n += len(c.HTML)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	out := make([]byte, 0, n)
	out = append(out, k.Open...)
	for _, c := range k.Children {
		if h, ok := prev[c.Key]; ok && bytes.Equal(h, c.HTML) {
			out = append(out, c.Shell...)
		} else {
			out = append(out, c.HTML...)
		}
	}
	return append(out, k.Close...)
}

// Renderer renders a board for the present users and the open cards (each
// with the users who have it open).
type Renderer func(ctx context.Context, board int64, open map[int64][]int64, present []int64) (*Frame, error)

// Sink is one browser stream.
type Sink interface {
	Send(*Event) error
	Ping() error
}

// Msg is something for one tab only.
type Msg struct {
	After   int64  // send once a frame of at least this board version went out
	Signals []byte // a JSON merge patch for the page's signals
	HTML    []byte // elements to patch
	at      time.Time
}

// Tab is one open board page.
type Tab struct {
	ID    string
	User  int64
	Board int64

	mu     sync.Mutex
	open   int64
	outbox []Msg
	conn   *conn
	seen   time.Time
	State  any // the web layer's per-tab UI state (toasts)
}

// Open returns the card open in the tab (0: none).
func (t *Tab) Open() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.open
}

// Lock and Unlock guard State.
func (t *Tab) Lock()   { t.mu.Lock() }
func (t *Tab) Unlock() { t.mu.Unlock() }

// Stats count the hub's work since it started.
type Stats struct {
	Rooms       int   `json:"rooms"`
	Tabs        int   `json:"tabs"`
	Streams     int64 `json:"streams"`
	Renders     int64 `json:"renders"`
	RenderNanos int64 `json:"render_nanos"`
	Sent        int64 `json:"frames_sent"`
	Skipped     int64 `json:"frames_skipped"` // rendered but superseded before a slow stream sent them
}

// Hub holds the rooms and tabs.
type Hub struct {
	Render   Renderer
	Throttle time.Duration // minimum time between two renders of a board
	Grace    time.Duration // how long a user stays present after their last stream closed
	Log      *slog.Logger
	// Slots is how many streams may send a frame at once. A frame wakes
	// every stream of the board, and each compresses it: without a limit
	// that burst takes every core, and actions wait behind it.
	Slots chan struct{}

	mu    sync.Mutex
	rooms map[int64]*room
	tabs  map[string]*Tab

	streams, renders, renderNanos, sent, skipped atomic.Int64
}

// NewHub returns a hub; set Render before use.
func NewHub(render Renderer) *Hub {
	return &Hub{
		Render:   render,
		Throttle: 50 * time.Millisecond,
		Grace:    3 * time.Second,
		Log:      slog.Default(),
		Slots:    make(chan struct{}, max(1, runtime.GOMAXPROCS(0)/2)),
		rooms:    map[int64]*room{},
		tabs:     map[string]*Tab{},
	}
}

// Stats reads the counters.
func (h *Hub) Stats() Stats {
	h.mu.Lock()
	rooms, tabs := len(h.rooms), len(h.tabs)
	h.mu.Unlock()
	return Stats{
		Rooms: rooms, Tabs: tabs, Streams: h.streams.Load(),
		Renders: h.renders.Load(), RenderNanos: h.renderNanos.Load(),
		Sent: h.sent.Load(), Skipped: h.skipped.Load(),
	}
}

// Tab returns the tab with this id if it belongs to user and board, or
// registers a new one (open names the card the page shows).
func (h *Hub) Tab(id string, user, board, open int64) *Tab {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.tabs[id]; ok && t.User == user && t.Board == board {
		t.mu.Lock()
		t.seen = time.Now()
		t.mu.Unlock()
		return t
	}
	t := &Tab{ID: id, User: user, Board: board, open: open, seen: time.Now()}
	h.tabs[id] = t
	return t
}

// Lookup finds a tab of user.
func (h *Hub) Lookup(id string, user int64) *Tab {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.tabs[id]; ok && t.User == user {
		return t
	}
	return nil
}

// SetOpen opens a card in the tab (0 closes it) and re-renders its board.
func (h *Hub) SetOpen(t *Tab, card int64) {
	t.mu.Lock()
	changed := t.open != card
	t.open = card
	t.mu.Unlock()
	if changed {
		h.Changed(t.Board)
	}
}

// Send queues m for the tab.
func (h *Hub) Send(t *Tab, m Msg) {
	m.at = time.Now()
	t.mu.Lock()
	t.outbox = append(t.outbox, m)
	c := t.conn
	t.mu.Unlock()
	if c != nil {
		c.poke()
	}
}

// Changed marks boards dirty (the writer's commit hook).
func (h *Hub) Changed(boards ...int64) {
	h.mu.Lock()
	rs := make([]*room, 0, len(boards))
	for _, b := range boards {
		if r := h.rooms[b]; r != nil {
			rs = append(rs, r)
		}
	}
	h.mu.Unlock()
	for _, r := range rs {
		r.poke()
	}
}

// Sweep forgets tabs that have had no stream for maxIdle, and messages
// nobody picked up within ten minutes (a page offline that long gets the
// whole board when it is back, but not the outcome of its last action).
func (h *Hub) Sweep(maxIdle time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for id, t := range h.tabs {
		t.mu.Lock()
		if t.conn == nil && now.Sub(t.seen) > maxIdle {
			delete(h.tabs, id)
		}
		t.outbox = slices.DeleteFunc(t.outbox, func(m Msg) bool { return now.Sub(m.at) > 10*time.Minute })
		t.mu.Unlock()
	}
}

type room struct {
	hub  *Hub
	id   int64
	wake chan struct{}
	stop chan struct{}

	mu      sync.Mutex
	conns   map[*conn]struct{}
	present map[int64]int         // user -> open streams
	leaving map[int64]*time.Timer // users in their grace period

	frame  atomic.Pointer[Frame]
	seq    int64
	closed bool
}

func (r *room) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *room) loop() {
	var last time.Time
	for {
		select {
		case <-r.stop:
			return
		case <-r.wake:
		}
		if wait := r.hub.Throttle - time.Since(last); wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-r.stop:
				t.Stop()
				return
			case <-t.C:
			}
			select { // whatever arrived meanwhile is in this render
			case <-r.wake:
			default:
			}
		}
		last = time.Now()
		r.render()
	}
}

func (r *room) render() {
	r.mu.Lock()
	open := map[int64][]int64{}
	for c := range r.conns {
		if o := c.tab.Open(); o != 0 && !slices.Contains(open[o], c.tab.User) {
			open[o] = append(open[o], c.tab.User)
		}
	}
	for _, users := range open {
		slices.Sort(users)
	}
	present := slices.Sorted(maps.Keys(r.present))
	conns := slices.Collect(maps.Keys(r.conns))
	r.mu.Unlock()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	f, err := r.hub.Render(ctx, r.id, open, present)
	cancel()
	r.hub.renders.Add(1)
	r.hub.renderNanos.Add(int64(time.Since(start)))
	if err != nil {
		r.hub.Log.Error("render", "board", r.id, "err", err)
		return
	}
	r.seq++
	f.seq = r.seq
	f.prepare(r.frame.Load())
	r.frame.Store(f)
	for _, c := range conns {
		c.poke()
	}
}

func (h *Hub) join(c *conn) *room {
	h.mu.Lock()
	r := h.rooms[c.tab.Board]
	if r == nil {
		r = &room{
			hub: h, id: c.tab.Board,
			wake: make(chan struct{}, 1), stop: make(chan struct{}),
			conns: map[*conn]struct{}{}, present: map[int64]int{}, leaving: map[int64]*time.Timer{},
		}
		h.rooms[c.tab.Board] = r
		go r.loop()
	}
	// Still under h.mu: gone() can't close the room between finding it and
	// joining it.
	r.mu.Lock()
	r.conns[c] = struct{}{}
	u := c.tab.User
	if t := r.leaving[u]; t != nil {
		t.Stop()
		delete(r.leaving, u)
	}
	r.present[u]++
	r.mu.Unlock()
	h.mu.Unlock()
	r.poke() // a frame with this tab's card and this user present
	return r
}

func (h *Hub) leave(r *room, c *conn) {
	r.mu.Lock()
	delete(r.conns, c)
	u := c.tab.User
	r.present[u]--
	if r.present[u] <= 0 && r.leaving[u] == nil {
		r.leaving[u] = time.AfterFunc(h.Grace, func() { h.gone(r, u) })
	}
	r.mu.Unlock()
}

// gone ends a user's grace period: they leave the presence list, and an
// empty room stops.
func (h *Hub) gone(r *room, u int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r.mu.Lock()
	if r.leaving[u] == nil { // came back meanwhile
		r.mu.Unlock()
		return
	}
	delete(r.leaving, u)
	if r.present[u] <= 0 {
		delete(r.present, u)
	}
	empty := len(r.conns) == 0 && len(r.leaving) == 0
	if empty && !r.closed {
		r.closed = true
		close(r.stop)
		if h.rooms[r.id] == r {
			delete(h.rooms, r.id)
		}
	}
	r.mu.Unlock()
	if !empty {
		r.poke()
	}
}

type conn struct {
	tab    *Tab
	notify chan struct{}
}

func (c *conn) poke() {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// Serve streams the tab's board to sink until ctx ends or a write fails.
func (h *Hub) Serve(ctx context.Context, t *Tab, sink Sink, heartbeat time.Duration) error {
	c := &conn{tab: t, notify: make(chan struct{}, 1)}
	h.streams.Add(1)
	t.mu.Lock()
	t.conn = c
	t.seen = time.Now()
	t.mu.Unlock()
	r := h.join(c)
	defer func() {
		h.leave(r, c)
		t.mu.Lock()
		if t.conn == c {
			t.conn = nil
		}
		t.seen = time.Now()
		t.mu.Unlock()
	}()

	var (
		sentSeq     int64
		sentVersion int64 = -1
		sentParts   [][]byte
		sentOpen    int64 = -1
		sentDetail  []byte
		sentBoard   *Frame // the frame whose children the page has
	)
	// sendFrame sends what changed since this stream's last frame.
	sendFrame := func(f *Frame) error {
		if sentSeq != 0 && f.seq > sentSeq+1 {
			h.skipped.Add(f.seq - sentSeq - 1)
		}
		for i, p := range f.Parts {
			if i < len(sentParts) && bytes.Equal(sentParts[i], p) {
				continue
			}
			if err := sink.Send(f.parts[i]); err != nil {
				return err
			}
		}
		sentParts = f.Parts
		if f.Board != nil {
			var ev *Event
			switch {
			case sentBoard == nil || sentBoard.Board == nil:
				ev = f.full()
			case sentBoard.seq == f.base: // the usual case: shared
				ev = f.delta
			default: // this stream skipped frames
				if b := f.Board.against(sentBoard.Board.Children); b != nil {
					ev = Elements(b)
				}
			}
			if ev != nil {
				if err := sink.Send(ev); err != nil {
					return err
				}
			}
			sentBoard = f
		}
		open := t.Open()
		detail, ok := f.Details[open]
		ev := f.details[open]
		if open == 0 {
			detail, ok, ev = f.Closed, true, f.closed
		}
		// The card isn't in this frame yet (opened after it was rendered):
		// the next frame has it.
		if ok && (open != sentOpen || !bytes.Equal(detail, sentDetail)) {
			if err := sink.Send(ev); err != nil {
				return err
			}
			sentOpen, sentDetail = open, detail
		}
		sentSeq, sentVersion = f.seq, f.Version
		h.sent.Add(1)
		return nil
	}

	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := sink.Ping(); err != nil {
				return err
			}
			continue
		case <-c.notify:
		}

		if f := r.frame.Load(); f != nil && f.seq != sentSeq {
			select {
			case h.Slots <- struct{}{}:
			case <-ctx.Done():
				return nil
			}
			err := sendFrame(r.frame.Load()) // the newest, after the wait
			<-h.Slots
			if err != nil {
				return err
			}
		}

		t.mu.Lock()
		var due []Msg
		n := 0
		for _, m := range t.outbox {
			if m.After > sentVersion {
				break // in order: later messages wait for this one
			}
			due = append(due, m)
			n++
		}
		t.outbox = t.outbox[n:]
		t.mu.Unlock()
		for _, m := range due {
			if m.HTML != nil {
				if err := sink.Send(Elements(m.HTML)); err != nil {
					return err
				}
			}
			if m.Signals != nil {
				if err := sink.Send(Signals(m.Signals)); err != nil {
					return err
				}
			}
		}
	}
}

// Present lists the users with a stream on the board (or in their grace period).
func (h *Hub) Present(board int64) []int64 {
	h.mu.Lock()
	r := h.rooms[board]
	h.mu.Unlock()
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Sorted(maps.Keys(r.present))
}
