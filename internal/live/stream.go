package live

import (
	"bytes"
	"slices"

	lithlive "github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"
)

// Tab is the newest messages of one tab's outbox, oldest first: what the
// page is told about its own actions.
type Tab struct {
	Messages []Message
}

// Message is one entry of a tab's outbox, formatted to send.
type Message struct {
	ID int64
	// After is the board version a frame must have shown before the
	// message goes out: an outcome never arrives before the change it
	// reports.
	After  int64
	Events [][]byte
}

// Presence is who is on a board, and its patch.
type Presence struct {
	Users []int64
	Event []byte
}

var online = sse.PatchSignals([]byte(`{"_offline":false}`), false)

// Stream is one page's stream: what it has been sent of the shared frames.
// Its WriteTick is the hub's Spec.WriteTick, called on the stream's own
// goroutine.
type Stream struct {
	user int64
	seat *Seat
	pos  int64 // the newest message of the tab's outbox the page has

	board    *Board // the last board frame sent
	card     int64  // the card whose dialog was sent last (-1: none)
	detail   []byte
	presence *Presence
	tab      *Tab // the newest tab frame: its messages wait for a board frame
	online   bool
	events   [][]byte
}

// NewStream starts a stream for the user's seat. pos is the last message
// of the tab's outbox that the page reports having.
func NewStream(user int64, seat *Seat, pos int64) *Stream {
	return &Stream{user: user, seat: seat, pos: pos, card: -1}
}

// WriteTick sends what the page lacks of a tick's frames, in one
// compressed write: the lanes it hasn't seen, its card's dialog when that
// changed, presence when it changed, and the tab's messages once a board
// frame shows their outcome.
func (s *Stream) WriteTick(st *sse.Stream, frames []lithlive.Frame) error {
	s.events = s.events[:0]
	for _, f := range frames {
		s.add(f.Value)
	}
	err := st.Write(s.events...)
	clear(s.events)
	return err
}

func (s *Stream) add(frame any) {
	switch f := frame.(type) {
	case *Board:
		s.sendBoard(f)
		s.sendDue()
	case *Tab:
		s.tab = f
		s.sendDue()
	case *Presence:
		// A frame from before this user arrived would take their own
		// avatar away until the next one.
		if f != s.presence && slices.Contains(f.Users, s.user) {
			s.presence = f
			s.events = append(s.events, f.Event)
		}
	case []byte:
		s.events = append(s.events, f)
	}
}

func (s *Stream) sendBoard(f *Board) {
	switch {
	case s.board == nil:
		s.events = append(s.events, f.full())
	case s.board.seq == f.base: // the usual case: one event for every stream
		if f.delta != nil {
			s.events = append(s.events, f.delta)
		}
	default: // this stream skipped frames
		if b := f.Lanes.against(s.board.Lanes.Children); b != nil {
			s.events = append(s.events, sse.PatchElements(b))
		}
	}
	if !s.online {
		s.online = true
		s.events = append(s.events, online)
	}
	s.board = f
	card := s.seat.Card()
	detail, ok := f.details[card]
	if card == 0 {
		detail, ok = f.closed, true
	}
	// A card opened after the frame was rendered is in the next one.
	if ok && (card != s.card || !bytes.Equal(detail, s.detail)) {
		s.events = append(s.events, detail)
		s.card, s.detail = card, detail
	}
}

// sendDue sends the tab's messages that the page lacks, in order, as long
// as the board frame sent shows their outcome.
func (s *Stream) sendDue() {
	if s.tab == nil || s.board == nil {
		return
	}
	for _, m := range s.tab.Messages {
		if m.ID <= s.pos {
			continue
		}
		if m.After > s.board.Version {
			return
		}
		s.events = append(s.events, m.Events...)
		s.pos = m.ID
	}
}
