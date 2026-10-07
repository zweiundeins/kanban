package live

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type event struct {
	kind string // patch, signals
	body string
}

type sink struct {
	mu     sync.Mutex
	events []event
	delay  time.Duration
}

func (s *sink) add(kind, body string) error {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	s.mu.Lock()
	s.events = append(s.events, event{kind, body})
	s.mu.Unlock()
	return nil
}
func (s *sink) Send(e *Event) error {
	if e.IsSignals() {
		return s.add("signals", string(e.Body()))
	}
	return s.add("patch", string(e.Body()))
}
func (s *sink) Ping() error { return nil }
func (s *sink) snapshot() []event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]event(nil), s.events...)
}

// A fake board: version goes up on every change; the frame names it.
type fake struct {
	version atomic.Int64
	renders atomic.Int64
}

func (f *fake) render(_ context.Context, board int64, open map[int64][]int64, present []int64) (*Frame, error) {
	f.renders.Add(1)
	v := f.version.Load()
	fr := &Frame{
		Version: v,
		Parts:   [][]byte{[]byte(fmt.Sprintf("presence %v", present)), []byte(fmt.Sprintf("board v%d", v))},
		Details: map[int64][]byte{},
		Closed:  []byte("closed"),
	}
	for o := range open {
		fr.Details[o] = []byte(fmt.Sprintf("card %d v%d", o, v))
	}
	return fr, nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func last(events []event, kind, body string) int {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].kind == kind && events[i].body == body {
			return i
		}
	}
	return -1
}

func has(events []event, kind, body string) int {
	for i, e := range events {
		if e.kind == kind && e.body == body {
			return i
		}
	}
	return -1
}

func TestOutboxWaitsForTheFrame(t *testing.T) {
	f := &fake{}
	h := NewHub(f.render)
	h.Throttle = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tab := h.Tab("t1", 1, 7, 0)
	s := &sink{}
	go h.Serve(ctx, tab, s, time.Hour)
	waitFor(t, "first frame", func() bool { return has(s.snapshot(), "patch", "board v0") >= 0 })

	// The command committed version 1; its acknowledgement is queued before
	// the board was rendered again, and must still come after the frame.
	f.version.Store(1)
	h.Send(tab, Msg{After: 1, Signals: []byte(`{"_acks":{"a":"ok"}}`)})
	time.Sleep(30 * time.Millisecond)
	if has(s.snapshot(), "signals", `{"_acks":{"a":"ok"}}`) >= 0 {
		t.Fatal("ack sent before any frame showed version 1")
	}
	h.Changed(7)
	waitFor(t, "ack", func() bool { return has(s.snapshot(), "signals", `{"_acks":{"a":"ok"}}`) >= 0 })
	ev := s.snapshot()
	if has(ev, "patch", "board v1") > has(ev, "signals", `{"_acks":{"a":"ok"}}`) {
		t.Fatalf("ack before frame: %v", ev)
	}
	// An unchanged part isn't sent again.
	n := 0
	for _, e := range ev {
		if strings.HasPrefix(e.body, "presence") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("presence sent %d times: %v", n, ev)
	}
}

func TestOpenCardAndPresence(t *testing.T) {
	f := &fake{}
	h := NewHub(f.render)
	h.Throttle = 5 * time.Millisecond
	h.Grace = 30 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := h.Tab("ta", 1, 7, 0)
	sa := &sink{}
	go h.Serve(ctx, a, sa, time.Hour)
	waitFor(t, "closed dialog", func() bool { return has(sa.snapshot(), "patch", "closed") >= 0 })

	h.SetOpen(a, 42)
	waitFor(t, "card", func() bool { return has(sa.snapshot(), "patch", "card 42 v0") >= 0 })

	// Ben joins: Anna sees him; Ben leaves: he stays for the grace period.
	bctx, bcancel := context.WithCancel(ctx)
	b := h.Tab("tb", 2, 7, 0)
	sb := &sink{}
	go h.Serve(bctx, b, sb, time.Hour)
	waitFor(t, "ben present", func() bool { return has(sa.snapshot(), "patch", "presence [1 2]") >= 0 })
	bcancel()
	time.Sleep(10 * time.Millisecond)
	if ev := sa.snapshot(); last(ev, "patch", "presence [1]") > last(ev, "patch", "presence [1 2]") {
		t.Fatal("ben left before the grace period ended")
	}
	waitFor(t, "ben gone", func() bool {
		ev := sa.snapshot()
		return last(ev, "patch", "presence [1]") > last(ev, "patch", "presence [1 2]")
	})
}

// A slow stream gets the newest frame, not a backlog.
func TestSlowStreamSkipsFrames(t *testing.T) {
	f := &fake{}
	h := NewHub(f.render)
	h.Throttle = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tab := h.Tab("slow", 1, 7, 0)
	s := &sink{delay: 20 * time.Millisecond}
	go h.Serve(ctx, tab, s, time.Hour)
	for i := 1; i <= 50; i++ {
		f.version.Store(int64(i))
		h.Changed(7)
		time.Sleep(2 * time.Millisecond)
	}
	waitFor(t, "last frame", func() bool { return has(s.snapshot(), "patch", "board v50") >= 0 })
	boards := 0
	for _, e := range s.snapshot() {
		if strings.HasPrefix(e.body, "board") {
			boards++
		}
	}
	if boards > 25 {
		t.Fatalf("slow stream got %d frames for 50 changes", boards)
	}
	if h.Stats().Skipped == 0 {
		t.Fatal("no skipped frames counted")
	}
}

// Many changes in a burst cost few renders.
func TestThrottleCoalesces(t *testing.T) {
	f := &fake{}
	h := NewHub(f.render)
	h.Throttle = 40 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tab := h.Tab("x", 1, 7, 0)
	go h.Serve(ctx, tab, &sink{}, time.Hour)
	time.Sleep(60 * time.Millisecond)
	before := f.renders.Load()
	for i := 0; i < 100; i++ {
		h.Changed(7)
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if n := f.renders.Load() - before; n > 6 {
		t.Fatalf("%d renders for a 100 ms burst", n)
	}
}

// A board with three lanes; each change rewrites lane 2.
func (f *fake) keyed(ctx context.Context, board int64, open map[int64][]int64, present []int64) (*Frame, error) {
	fr, _ := f.render(ctx, board, open, present)
	v := f.version.Load()
	fr.Board = &Keyed{Open: []byte("<b>"), Close: []byte("</b>")}
	for k := int64(1); k <= 3; k++ {
		html := fmt.Sprintf("<l%d>", k)
		if k == 2 {
			html = fmt.Sprintf("<l2 v%d>", v)
		}
		fr.Board.Children = append(fr.Board.Children, Child{Key: k, HTML: []byte(html), Shell: []byte(fmt.Sprintf("<s%d>", k))})
	}
	return fr, nil
}

// Streams that had the frame before share one event with only the changed
// lane; a stream that skipped frames gets the lanes it lacks.
func TestBoardSendsChangedLanesOnce(t *testing.T) {
	f := &fake{}
	h := NewHub(f.keyed)
	h.Throttle = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type rec struct {
		sink
		mu    sync.Mutex
		board []*Event
	}
	var a, b rec
	for i, r := range []*rec{&a, &b} {
		go h.Serve(ctx, h.Tab(fmt.Sprint("t", i), 1, 7, 0), sendFunc(func(e *Event) error {
			if strings.HasPrefix(string(e.Body()), "<b>") {
				r.mu.Lock()
				r.board = append(r.board, e)
				r.mu.Unlock()
			}
			return r.sink.Send(e)
		}), time.Hour)
	}
	boards := func(r *rec) []*Event {
		r.mu.Lock()
		defer r.mu.Unlock()
		return append([]*Event(nil), r.board...)
	}
	waitFor(t, "first frames", func() bool { return len(boards(&a)) > 0 && len(boards(&b)) > 0 })
	if got := string(boards(&a)[0].Body()); got != "<b><l1><l2 v0><l3></b>" {
		t.Fatalf("first frame: %s", got)
	}
	time.Sleep(10 * time.Millisecond)
	f.version.Store(1)
	h.Changed(7)
	waitFor(t, "change", func() bool { return len(boards(&a)) > 1 && len(boards(&b)) > 1 })
	ea, eb := boards(&a)[1], boards(&b)[1]
	if got := string(ea.Body()); got != "<b><s1><l2 v1><s3></b>" {
		t.Fatalf("delta: %s", got)
	}
	if ea != eb {
		t.Fatal("two streams formatted the same delta separately")
	}

	// A stream that has frame 1 and is then handed frame 3 compares
	// against frame 1.
	f1, _ := f.keyed(ctx, 7, nil, nil)
	f.version.Store(3)
	f3, _ := f.keyed(ctx, 7, nil, nil)
	f3.seq = 3
	f3.prepare(&Frame{seq: 2, Board: f3.Board})
	if f3.delta != nil || f3.base != 2 {
		t.Fatal("frame 3 against an identical frame 2 should be unchanged")
	}
	if got := string(f3.Board.against(f1.Board.Children)); got != "<b><s1><l2 v3><s3></b>" {
		t.Fatalf("catch-up: %s", got)
	}
}

type sendFunc func(*Event) error

func (f sendFunc) Send(e *Event) error { return f(e) }
func (f sendFunc) Ping() error         { return nil }
