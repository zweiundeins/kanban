package live

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sse"
)

// recorder is a page's stream without compression: its events as written.
type recorder struct {
	w  *httptest.ResponseRecorder
	st *sse.Stream
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	w := httptest.NewRecorder()
	st, err := sse.Open(w, httptest.NewRequest("GET", "/", nil), sse.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &recorder{w: w, st: st}
}

// take returns the data of the events written since the last take.
func (r *recorder) take() []string {
	var out []string
	for ev := range strings.SplitSeq(r.w.Body.String(), "\n\n") {
		var data []string
		for line := range strings.SplitSeq(ev, "\n") {
			if d, ok := strings.CutPrefix(line, "data: "); ok {
				data = append(data, d)
			}
		}
		if len(data) > 0 {
			out = append(out, strings.Join(data, "\n"))
		}
	}
	r.w.Body.Reset()
	return out
}

// board is a frame of a board with three lanes; lane 2 shows v.
func board(version int64, v string) *Board {
	f := &Board{Version: version, Lanes: &Keyed{Open: []byte("<b>"), Close: []byte("</b>")}, Details: map[int64][]byte{}, Closed: []byte("closed")}
	for k := int64(1); k <= 3; k++ {
		html := fmt.Sprintf("<l%d>", k)
		if k == 2 {
			html = "<l2 " + v + ">"
		}
		f.Lanes.Children = append(f.Lanes.Children, Child{Key: k, HTML: []byte(html), Shell: []byte(fmt.Sprintf("<s%d>", k))})
	}
	return f
}

func msg(id, after int64, text string) Message {
	return Message{ID: id, After: after, Events: [][]byte{sse.PatchSignals([]byte(text), false)}}
}

func noTopics([]string) {}

func seat(t *testing.T, card int64) *Seat {
	s := NewSeats(noTopics, time.Hour)
	st := s.Join(1, 7, t.Name(), card)
	t.Cleanup(func() { s.Leave(st) })
	return st
}

// An outcome waits for a board frame of at least its version, even when
// the tab's frame comes first.
func TestOutcomeWaitsForTheFrame(t *testing.T) {
	r := newRecorder(t)
	s := NewStream(1, seat(t, 0), 0)
	write := func(f any) []string {
		t.Helper()
		if err := s.Write(r.st, "", f); err != nil {
			t.Fatal(err)
		}
		return r.take()
	}
	f1 := board(1, "a")
	f1.Prepare(nil)
	if got := write(&Tab{Messages: []Message{msg(1, 2, `{"ack":1}`)}}); len(got) != 0 {
		t.Fatalf("sent before any frame: %q", got)
	}
	got := write(f1)
	if len(got) != 3 || got[0] != "elements <b><l1><l2 a><l3></b>" || got[1] != `signals {"_offline":false}` || got[2] != "elements closed" {
		t.Fatalf("first frame: %q", got)
	}
	f2 := board(2, "b")
	f2.Prepare(f1)
	got = write(f2)
	if len(got) != 2 || got[0] != "elements <b><s1><l2 b><s3></b>" || got[1] != `signals {"ack":1}` {
		t.Fatalf("frame and outcome: %q", got)
	}
	// Later messages go in order, and only once.
	got = write(&Tab{Messages: []Message{msg(1, 2, `{"ack":1}`), msg(2, 2, `{"ack":2}`), msg(3, 3, `{"ack":3}`), msg(4, 0, `{"ack":4}`)}})
	if !slices.Equal(got, []string{`signals {"ack":2}`}) {
		t.Fatalf("in order: %q", got)
	}
	f3 := board(3, "b")
	f3.Prepare(f2)
	if got = write(f3); !slices.Equal(got, []string{`signals {"ack":3}`, `signals {"ack":4}`}) {
		t.Fatalf("after the frame of version 3: %q", got)
	}
}

// A page that reconnects says which message it has: it gets the ones
// after, and the whole board.
func TestReconnectSendsWhatThePageLacks(t *testing.T) {
	r := newRecorder(t)
	s := NewStream(1, seat(t, 0), 1)
	f := board(5, "x")
	f.Prepare(nil)
	s.Write(r.st, "", &Tab{Messages: []Message{msg(1, 4, `{"ack":1}`), msg(2, 5, `{"ack":2}`)}})
	s.Write(r.st, "", f)
	got := r.take()
	if len(got) != 4 || got[0] != "elements <b><l1><l2 x><l3></b>" || got[3] != `signals {"ack":2}` {
		t.Fatalf("got %q", got)
	}
}

// Streams that had the frame before share one event with only the changed
// lane; a stream that skipped frames gets the lanes it lacks.
func TestChangedLanesOnce(t *testing.T) {
	f1, f2, f3 := board(1, "a"), board(2, "b"), board(3, "c")
	f1.Prepare(nil)
	f2.Prepare(f1)
	f3.Prepare(f2)
	if board(1, "a").Prepare(f1) {
		t.Fatal("an unchanged frame was prepared")
	}
	var events [2][]byte
	for i, frames := range [][]*Board{{f1, f2}, {f1, f2}} {
		s := NewStream(1, seat(t, 0), 0)
		for _, f := range frames {
			s.events = s.events[:0]
			s.sendBoard(f)
		}
		events[i] = s.events[0]
	}
	if string(events[0]) != string(sse.PatchElements([]byte("<b><s1><l2 b><s3></b>"))) || &events[0][0] != &events[1][0] {
		t.Fatalf("delta %q, shared %v", events[0], &events[0][0] == &events[1][0])
	}
	s := NewStream(1, seat(t, 0), 0)
	s.sendBoard(f1)
	s.events = s.events[:0]
	s.sendBoard(f3)
	if got := string(s.events[0]); got != string(sse.PatchElements([]byte("<b><s1><l2 c><s3></b>"))) {
		t.Fatalf("catch-up: %q", got)
	}
}

// A stream sends its tab's card dialog when it opens, changes or closes,
// and nothing when the dialog stays the same.
func TestCardDialog(t *testing.T) {
	r := newRecorder(t)
	st := seat(t, 0)
	s := NewStream(1, st, 0)
	frame := func(prev *Board, version int64, details map[int64]string) *Board {
		f := board(version, "a")
		for c, d := range details {
			f.Details[c] = []byte(d)
		}
		f.Prepare(prev)
		s.Write(r.st, "", f)
		return f
	}
	f := frame(nil, 1, nil)
	r.take()
	st.card.Store(9)
	// Opened after the frame was rendered: the next frame has it.
	if s.Write(r.st, "", f); len(r.take()) != 0 {
		t.Fatal("sent something for a card the frame lacks")
	}
	f = frame(f, 2, map[int64]string{9: "card 9"})
	if got := r.take(); !slices.Equal(got, []string{"elements card 9"}) {
		t.Fatalf("opened: %q", got)
	}
	f = frame(f, 3, map[int64]string{9: "card 9"})
	if got := r.take(); len(got) != 0 {
		t.Fatalf("unchanged: %q", got)
	}
	f = frame(f, 4, map[int64]string{9: "card 9, renamed"})
	if got := r.take(); !slices.Equal(got, []string{"elements card 9, renamed"}) {
		t.Fatalf("changed: %q", got)
	}
	st.card.Store(0)
	frame(f, 5, nil)
	if got := r.take(); !slices.Equal(got, []string{"elements closed"}) {
		t.Fatalf("closed: %q", got)
	}
}

// A presence frame from before this user arrived isn't sent to them.
func TestPresenceKeepsSelf(t *testing.T) {
	r := newRecorder(t)
	s := NewStream(1, seat(t, 0), 0)
	s.Write(r.st, "", &Presence{Users: []int64{2}, Event: []byte("data: elements 2\n\n")})
	if got := r.take(); len(got) != 0 {
		t.Fatalf("sent %q", got)
	}
	p := &Presence{Users: []int64{1, 2}, Event: []byte("data: elements 1 2\n\n")}
	s.Write(r.st, "", p)
	s.Write(r.st, "", p)
	if got := r.take(); !slices.Equal(got, []string{"elements 1 2"}) {
		t.Fatalf("sent %q", got)
	}
}

type topics struct {
	mu   sync.Mutex
	seen []string
}

func (r *topics) add(ts []string) {
	r.mu.Lock()
	r.seen = append(r.seen, ts...)
	r.mu.Unlock()
}

func (r *topics) count(topic string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, t := range r.seen {
		if t == topic {
			n++
		}
	}
	return n
}

// A user stays present for the grace period after their last stream, and
// a reload within it keeps them.
func TestPresenceGrace(t *testing.T) {
	var seen topics
	s := NewSeats(seen.add, 30*time.Millisecond)
	a := s.Join(1, 7, "anna-tab-0000000001", 0)
	b := s.Join(2, 7, "ben-tab-00000000001", 0)
	if got := s.Present(7); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("present %v", got)
	}
	s.Leave(b)
	if got := s.Present(7); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("ben left before his grace period ended: %v", got)
	}
	time.Sleep(60 * time.Millisecond)
	if got := s.Present(7); !slices.Equal(got, []int64{1}) || seen.count(PresenceTopic(7)) != 1 {
		t.Fatalf("after the grace period: %v, %v", got, seen.seen)
	}
	s.Leave(a)
	a = s.Join(1, 7, "anna-tab-0000000002", 0) // a reload
	time.Sleep(60 * time.Millisecond)
	if got := s.Present(7); !slices.Equal(got, []int64{1}) {
		t.Fatalf("anna reloaded: %v", got)
	}
	s.Leave(a)
	time.Sleep(60 * time.Millisecond)
	if got := s.Present(7); len(got) != 0 || len(s.boards) != 0 {
		t.Fatalf("an empty board stays: %v", got)
	}
}

// The board knows which cards its tabs show, and who has them open.
func TestOpenCards(t *testing.T) {
	var seen topics
	s := NewSeats(seen.add, time.Hour)
	a := s.Join(1, 7, "anna-tab-0000000001", 5)
	a2 := s.Join(1, 7, "anna-tab-0000000001", 5) // the same tab's next stream
	b := s.Join(2, 7, "ben-tab-00000000001", 0)
	if !s.Open(2, "ben-tab-00000000001", 5) || s.Open(2, "no-such-tab-0000001", 5) {
		t.Fatal("Open")
	}
	if got := s.OpenCards(7); len(got) != 1 || !slices.Equal(got[5], []int64{1, 2}) {
		t.Fatalf("open %v", got)
	}
	s.Open(1, "anna-tab-0000000001", 6)
	s.Leave(a)
	if got := s.OpenCards(7); !slices.Equal(got[6], []int64{1}) || !slices.Equal(got[5], []int64{2}) {
		t.Fatalf("after one of anna's streams left: %v", got)
	}
	s.Leave(a2)
	s.Leave(b)
	if got := s.OpenCards(7); len(got) != 0 {
		t.Fatalf("open %v", got)
	}
	if n := seen.count("board:7"); n != 5 {
		t.Fatalf("the board was marked %d times, want 5 (anna opens, ben opens, anna switches, anna and ben leave)", n)
	}
}

func TestKeyedAgainst(t *testing.T) {
	f1, f3 := board(1, "a"), board(3, "c")
	if got := f3.Lanes.against(f1.Lanes.Children); !bytes.Equal(got, []byte("<b><s1><l2 c><s3></b>")) {
		t.Fatalf("against: %s", got)
	}
	if got := f1.Lanes.against(f1.Lanes.Children); got != nil {
		t.Fatalf("unchanged: %s", got)
	}
}
