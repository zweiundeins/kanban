package live

import (
	"maps"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"kanban/internal/app"
)

// PresenceTopic names what changes with who is on a board.
func PresenceTopic(board int64) string { return "presence:" + strconv.FormatInt(board, 10) }

// Seats knows who has a stream open on which board, and which card each
// tab shows. It is about open connections, so it lives in memory: a page
// sends its open card again whenever its stream connects, and a restart
// loses nothing.
type Seats struct {
	grace   time.Duration
	changed func(topics []string)

	mu     sync.Mutex
	boards map[int64]*room
	byTab  map[seatKey]*Seat
}

type seatKey struct {
	user int64
	tab  string
}

type room struct {
	seats   map[*Seat]struct{}
	users   map[int64]int    // streams per user
	leaving map[int64]*grace // users in their grace period
}

type grace struct{ timer *time.Timer }

// Seat is one tab's place on a board: whose it is and the card it shows,
// shared by the tab's streams.
type Seat struct {
	User, Board int64
	key         seatKey
	card        atomic.Int64
	streams     int // guarded by Seats.mu
}

// Card is the card the tab shows, 0 for none.
func (s *Seat) Card() int64 { return s.card.Load() }

// NewSeats returns an empty registry. changed is the hub's Changed; grace
// is how long a user stays present after their last stream closed, so
// that a reload doesn't make them blink out for everyone else.
func NewSeats(changed func(topics []string), grace time.Duration) *Seats {
	return &Seats{grace: grace, changed: changed, boards: map[int64]*room{}, byTab: map[seatKey]*Seat{}}
}

// Join seats a stream of the user's tab on the board, showing card. Call
// Leave when the stream ends.
func (s *Seats) Join(user, board int64, tab string, card int64) *Seat {
	s.mu.Lock()
	r := s.boards[board]
	if r == nil {
		r = &room{seats: map[*Seat]struct{}{}, users: map[int64]int{}, leaving: map[int64]*grace{}}
		s.boards[board] = r
	}
	key := seatKey{user, tab}
	seat := s.byTab[key]
	if seat == nil || seat.Board != board {
		seat = &Seat{User: user, Board: board, key: key}
		s.byTab[key] = seat
		r.seats[seat] = struct{}{}
	}
	seat.streams++
	if g := r.leaving[user]; g != nil {
		g.timer.Stop()
		delete(r.leaving, user)
	}
	r.users[user]++
	s.mu.Unlock()
	if seat.card.Swap(card) != card {
		s.changed([]string{app.BoardTopic(board)})
	}
	return seat
}

// Leave ends one stream of the seat. Its user stays present for the grace
// period after their last stream.
func (s *Seats) Leave(seat *Seat) {
	s.mu.Lock()
	r := s.boards[seat.Board]
	seat.streams--
	open := false
	if seat.streams <= 0 {
		delete(r.seats, seat)
		if s.byTab[seat.key] == seat {
			delete(s.byTab, seat.key)
		}
		open = seat.Card() != 0
	}
	u := seat.User
	if r.users[u]--; r.users[u] <= 0 && r.leaving[u] == nil {
		g := &grace{}
		g.timer = time.AfterFunc(s.grace, func() { s.gone(seat.Board, u, g) })
		r.leaving[u] = g
	}
	s.mu.Unlock()
	if open {
		s.changed([]string{app.BoardTopic(seat.Board)})
	}
}

// gone ends a user's grace period on a board.
func (s *Seats) gone(board, user int64, g *grace) {
	s.mu.Lock()
	r := s.boards[board]
	if r == nil || r.leaving[user] != g { // came back meanwhile
		s.mu.Unlock()
		return
	}
	delete(r.leaving, user)
	if r.users[user] <= 0 {
		delete(r.users, user)
	}
	if len(r.seats) == 0 && len(r.leaving) == 0 {
		delete(s.boards, board)
	}
	s.mu.Unlock()
	s.changed([]string{PresenceTopic(board)})
}

// Open shows card in the user's tab (0 closes it). It reports whether the
// tab has a stream; without one, the page sends the card when it connects.
func (s *Seats) Open(user int64, tab string, card int64) bool {
	s.mu.Lock()
	seat := s.byTab[seatKey{user, tab}]
	s.mu.Unlock()
	if seat == nil {
		return false
	}
	if seat.card.Swap(card) != card {
		s.changed([]string{app.BoardTopic(seat.Board)})
	}
	return true
}

// Present lists the users on the board: with a stream, or in their grace
// period.
func (s *Seats) Present(board int64) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.boards[board]
	if r == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(r.users))
}

// OpenCards maps the cards open in the board's tabs to the users who have
// them open.
func (s *Seats) OpenCards(board int64) map[int64][]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	open := map[int64][]int64{}
	r := s.boards[board]
	if r == nil {
		return open
	}
	for seat := range r.seats {
		if c := seat.Card(); c != 0 && !slices.Contains(open[c], seat.User) {
			open[c] = append(open[c], seat.User)
		}
	}
	for _, users := range open {
		slices.Sort(users)
	}
	return open
}
