package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"

	lithlive "github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"

	"kanban/internal/app"
	"kanban/internal/live"
	"kanban/internal/views"
)

// stream is the page's live view: the whole board after every change, who
// is on it, its card's dialog, and its own messages. The page sends the
// card it shows and the last message it has in Datastar's signals.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	if u == nil {
		http.Error(w, "Log in again.", http.StatusUnauthorized)
		return
	}
	board := pathID(r, "b")
	tab := r.URL.Query().Get("tab")
	if !tabRe.MatchString(tab) {
		http.Error(w, "Bad tab.", http.StatusBadRequest)
		return
	}
	if ok, err := s.App.IsMember(r.Context(), board, u.ID); err != nil || !ok {
		http.Error(w, "Not a member of this board.", http.StatusForbidden)
		return
	}
	var page struct {
		Card   int64 `json:"card"`
		Outbox int64 `json:"outbox"`
	}
	_ = json.Unmarshal([]byte(r.URL.Query().Get("datastar")), &page)
	seat := s.Seats.Join(u.ID, board, tab, max(page.Card, 0))
	defer s.Seats.Leave(seat)
	err := s.Hub.Serve(w, r, lithlive.Spec{
		ID:        app.TabTopic(u.ID, tab),
		Views:     []lithlive.View{s.tabView(u.ID, tab), s.boardView(board), s.presenceView(board)},
		WriteTick: live.NewStream(u.ID, seat, page.Outbox).WriteTick,
	})
	if err != nil && r.Context().Err() == nil {
		s.Log.Debug("stream ended", "board", board, "err", err)
	}
}

// boardView is the board's frame, the same for everyone on it.
func (s *Server) boardView(board int64) lithlive.View {
	key := app.BoardTopic(board)
	var prev *live.Board
	return lithlive.View{Key: key, Topics: []string{key}, Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
		f, err := s.Render.Board(ctx, tx, board, s.Seats.OpenCards(board))
		if err != nil {
			return nil, err
		}
		if !f.Prepare(prev) {
			return nil, nil
		}
		prev = f
		return f, nil
	}}
}

// presenceView is who is on the board. It renders again whenever a stream
// comes or goes, and when a user's grace period ends.
func (s *Server) presenceView(board int64) lithlive.View {
	key := live.PresenceTopic(board)
	var prev *live.Presence
	return lithlive.View{Key: key, Topics: []string{key}, Presence: true, Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
		members, err := app.ReadMembers(ctx, tx, board)
		if err != nil {
			return nil, err
		}
		present := s.Seats.Present(board)
		var users []int64
		for _, m := range members {
			if slices.Contains(present, m.ID) {
				users = append(users, m.ID)
			}
		}
		if prev != nil && slices.Equal(prev.Users, users) {
			return nil, nil
		}
		prev = &live.Presence{Users: users, Event: sse.PatchElements(s.Render.Presence(members, present))}
		return prev, nil
	}}
}

// keepMessages is how many of a tab's newest messages its frame carries:
// a stream that fell further behind loses the oldest outcomes, not the
// board.
const keepMessages = 32

// tabView is the newest messages of the tab's outbox, each formatted once.
// A render reads only the messages since the one before.
func (s *Server) tabView(user int64, tab string) lithlive.View {
	key := app.TabTopic(user, tab)
	var msgs []live.Message
	var last int64
	return lithlive.View{Key: key, Topics: []string{key}, Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
		rows, err := tx.QueryContext(ctx, `SELECT id, board_id, after, signals, status, toasts FROM tab_outbox
			WHERE tab = ? AND user_id = ? AND id > ? ORDER BY id DESC LIMIT ?`, tab, user, last, keepMessages)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var fresh []live.Message
		for rows.Next() {
			var id, board, after int64
			var signals string
			var status, toasts sql.NullString
			if err := rows.Scan(&id, &board, &after, &signals, &status, &toasts); err != nil {
				return nil, err
			}
			fresh = append(fresh, message(id, board, after, signals, status, toasts))
		}
		if err := rows.Err(); err != nil || len(fresh) == 0 {
			return nil, err
		}
		slices.Reverse(fresh)
		next := make([]live.Message, 0, keepMessages)
		next = append(next, msgs[max(0, len(msgs)+len(fresh)-keepMessages):]...)
		msgs, last = append(next, fresh...), fresh[len(fresh)-1].ID
		return &live.Tab{Messages: msgs}, nil
	}}
}

func message(id, board, after int64, signals string, status, toasts sql.NullString) live.Message {
	m := live.Message{ID: id, After: after}
	var html []byte
	if status.Valid {
		html = append(html, render(views.Status(status.String))...)
	}
	if toasts.Valid {
		html = append(html, render(views.Toasts(board, toasts.String))...)
	}
	if html != nil {
		m.Events = append(m.Events, sse.PatchElements(html))
	}
	m.Events = append(m.Events, sse.PatchSignals(withOutbox(signals, id), false))
	return m
}

// withOutbox adds the message's id to its signals: the page sends it back
// as the last message it has when its stream reconnects.
func withOutbox(signals string, id int64) []byte {
	b := strconv.AppendInt([]byte(`{"outbox":`), id, 10)
	rest := strings.TrimPrefix(strings.TrimSpace(signals), "{")
	if strings.TrimSpace(rest) != "}" {
		b = append(b, ',')
	}
	return append(b, rest...)
}
