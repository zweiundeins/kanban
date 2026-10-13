package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

// TabTopic names what changes with one user's tab (an open page): its
// outbox.
func TabTopic(user int64, tab string) string {
	return "tab:" + strconv.FormatInt(user, 10) + ":" + tab
}

// Toast is one message in a tab's toast region.
type Toast struct {
	ID      string `json:"id"`
	Variant string `json:"variant"`
	Title   string `json:"title,omitempty"`
	Text    string `json:"text"`
}

const maxToasts = 6

// reply queues an action's outcome for the actor's tab: the op's result,
// the signals for a success (an emptied input), the live region's text,
// and a toast for a refusal. A replayed refusal adds no second toast.
func reply(tx *writer.Tx, actor Actor, board int64, out Outcome) error {
	if actor.Tab == "" {
		return nil
	}
	sig := map[string]any{}
	if actor.Op != "" {
		result := "ok"
		if !out.OK {
			result = out.Msg
		}
		sig["_acks"] = map[string]string{actor.Op: result}
	}
	status := out.Notice
	var toasts any
	if out.OK {
		for k, v := range actor.Clear {
			sig[k] = v
		}
	} else {
		status = out.Msg
		if !out.Replayed {
			list, n, err := tabToasts(tx, actor.User, actor.Tab)
			if err != nil {
				return err
			}
			n++
			list = append(list, Toast{ID: "t" + strconv.FormatInt(n, 10), Variant: "warn", Title: "Not saved", Text: out.Msg})
			if len(list) > maxToasts {
				list = list[len(list)-maxToasts:]
			}
			if toasts, err = saveToasts(tx, actor.User, actor.Tab, list, n); err != nil {
				return err
			}
		}
	}
	js, err := json.Marshal(sig)
	if err != nil {
		return err
	}
	return queue(tx, actor.User, actor.Tab, board, out.Version, js, status, toasts)
}

// queue adds a message to a tab's outbox. It goes out once a frame of the
// board at version after or later did; status and toasts are strings or
// nil for unchanged.
func queue(tx *writer.Tx, user int64, tab string, board, after int64, signals []byte, status, toasts any) error {
	_, err := tx.Exec(`INSERT INTO tab_outbox (tab, user_id, board_id, after, signals, status, toasts, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		tab, user, board, after, string(signals), status, toasts, time.Now().Unix())
	if err != nil {
		return err
	}
	tx.Touch(TabTopic(user, tab))
	return nil
}

// OutboxEnd is the newest message of any tab's outbox: a page drawn now
// starts there, since its tab's messages all come later.
func OutboxEnd(ctx context.Context, q sqlite.Querier) (end int64, err error) {
	err = q.QueryRowContext(ctx, `SELECT coalesce(max(id), 0) FROM tab_outbox`).Scan(&end)
	return end, err
}

func tabToasts(tx *writer.Tx, user int64, tab string) ([]Toast, int64, error) {
	var js string
	var n int64
	err := tx.QueryRow(`SELECT toasts, toast_n FROM tab_state WHERE tab = ? AND user_id = ?`, tab, user).Scan(&js, &n)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	} else if err != nil {
		return nil, 0, err
	}
	var list []Toast
	return list, n, json.Unmarshal([]byte(js), &list)
}

// saveToasts stores a tab's toasts and returns them as JSON.
func saveToasts(tx *writer.Tx, user int64, tab string, list []Toast, n int64) (string, error) {
	if list == nil {
		list = []Toast{}
	}
	js, err := json.Marshal(list)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(`INSERT INTO tab_state (tab, user_id, toasts, toast_n, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (tab, user_id) DO UPDATE SET toasts = excluded.toasts, toast_n = excluded.toast_n, updated_at = excluded.updated_at`,
		tab, user, string(js), n, time.Now().Unix())
	return string(js), err
}

// DismissToast takes a toast off the tab's list.
func (a *App) DismissToast(ctx context.Context, user, board int64, tab, id string) error {
	return a.DB.W.Do(ctx, writer.Func(func(tx *writer.Tx) error {
		list, n, err := tabToasts(tx, user, tab)
		if err != nil {
			return err
		}
		js, err := saveToasts(tx, user, tab, slices.DeleteFunc(list, func(t Toast) bool { return t.ID == id }), n)
		if err != nil {
			return err
		}
		return queue(tx, user, tab, board, 0, []byte(`{}`), nil, js)
	}))
}

// Sweep forgets the messages for open pages older than ten minutes (a page
// offline that long gets the whole board when it is back, but not the
// outcome of its last action), and the toasts of tabs idle for a day.
func (a *App) Sweep(ctx context.Context) error {
	now := time.Now()
	return a.DB.W.Do(ctx, writer.Func(func(tx *writer.Tx) error {
		// Ids grow with time: the scan stops at the first message to keep.
		_, err := tx.Exec(`DELETE FROM tab_outbox WHERE id < coalesce(
			(SELECT id FROM tab_outbox WHERE created_at >= ? ORDER BY id LIMIT 1),
			(SELECT max(id) + 1 FROM tab_outbox))`, now.Add(-10*time.Minute).Unix())
		if err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM tab_state WHERE updated_at < ?`, now.Add(-24*time.Hour).Unix())
		return err
	}))
}
