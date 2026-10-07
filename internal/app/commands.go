package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"kanban/internal/db"
)

// Rejection is a command's refusal, with the reason the user reads. The
// command's changes are undone; the outcome is still recorded for its op.
type Rejection struct{ Msg string }

func (r *Rejection) Error() string { return r.Msg }

func reject(format string, args ...any) error { return &Rejection{Msg: fmt.Sprintf(format, args...)} }

// Outcome is what an action came to.
type Outcome struct {
	OK       bool   `json:"ok"`
	Msg      string `json:"msg,omitempty"`    // why it was refused
	Notice   string `json:"notice,omitempty"` // what happened, for the live region
	Version  int64  `json:"-"`                // the board version that shows it
	Replayed bool   `json:"-"`
}

// Actor is who acts, and the op id their page gave the action.
type Actor struct {
	User int64
	Op   string
}

var opRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// act runs fn as one writer command on board for actor: membership is
// checked, a replayed op gets its first outcome, a rejection is recorded
// like a success. Only real errors (the database) fail the command.
func (a *App) act(ctx context.Context, actor Actor, board int64, fn func(tx *db.Tx) (notice string, err error)) (Outcome, error) {
	if actor.Op != "" && !opRe.MatchString(actor.Op) {
		return Outcome{}, fmt.Errorf("bad op id")
	}
	var out Outcome
	err := a.DB.W.Do(ctx, func(tx *db.Tx) error {
		out = Outcome{}
		if actor.Op != "" {
			var result string
			err := tx.QueryRow(`SELECT result, version FROM ops WHERE user_id = ? AND op = ?`, actor.User, actor.Op).Scan(&result, &out.Version)
			if err == nil {
				out.Replayed = true
				return json.Unmarshal([]byte(result), &out)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		var member int
		if err := tx.QueryRow(`SELECT count(*) FROM board_members WHERE board_id = ? AND user_id = ?`, board, actor.User).Scan(&member); err != nil {
			return err
		}
		var notice string
		inner := error(&Rejection{Msg: "You are not a member of this board."})
		if member == 1 {
			inner = tx.Savepoint(func() error {
				var err error
				notice, err = fn(tx)
				return err
			})
		}
		var rej *Rejection
		switch {
		case inner == nil:
			out.OK, out.Notice = true, notice
		case errors.As(inner, &rej):
			out.Msg = rej.Msg
		default:
			return inner
		}
		if err := tx.QueryRow(`SELECT version FROM boards WHERE id = ?`, board).Scan(&out.Version); err != nil {
			return err
		}
		if actor.Op != "" {
			result, _ := json.Marshal(out)
			if _, err := tx.Exec(`INSERT INTO ops (user_id, op, board_id, result, version, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
				actor.User, actor.Op, board, string(result), out.Version, time.Now().Unix()); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// PruneOps forgets outcomes older than a day.
func (a *App) PruneOps(ctx context.Context) error {
	return a.DB.W.Do(ctx, func(tx *db.Tx) error {
		_, err := tx.Exec(`DELETE FROM ops WHERE created_at < ?`, time.Now().Add(-24*time.Hour).Unix())
		return err
	})
}

// cleanText trims s and refuses empty, too long or control characters.
func cleanText(what, s string, max int, multiline bool) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", reject("%s can't be empty.", what)
	}
	if !utf8.ValidString(s) {
		return "", reject("%s isn't valid text.", what)
	}
	if n := utf8.RuneCountInString(s); n > max {
		return "", reject("%s is too long (%d characters, at most %d).", what, n, max)
	}
	for _, r := range s {
		if (multiline && (r == '\n' || r == '\t')) || r == ' ' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", reject("%s contains invisible characters.", what)
		}
	}
	return s, nil
}

func quote(s string) string {
	r := []rune(s)
	if len(r) > 40 {
		s = string(r[:39]) + "…"
	}
	return "‘" + s + "’"
}

// cardOn checks that card is on board and active, and returns its list and title.
func cardOn(tx *db.Tx, board, card int64) (list int64, title string, err error) {
	var archived sql.NullInt64
	err = tx.QueryRow(`SELECT list_id, title, archived_at FROM cards WHERE id = ? AND board_id = ?`, card, board).Scan(&list, &title, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", reject("This card no longer exists.")
	}
	if err == nil && archived.Valid {
		return 0, "", reject("This card was archived meanwhile.")
	}
	return list, title, err
}

func listOn(tx *db.Tx, board, list int64) (name string, err error) {
	var archived sql.NullInt64
	err = tx.QueryRow(`SELECT name, archived_at FROM lists WHERE id = ? AND board_id = ?`, list, board).Scan(&name, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return "", reject("This list no longer exists.")
	}
	if err == nil && archived.Valid {
		return "", reject("The list %s was archived meanwhile.", quote(name))
	}
	return name, err
}

func bumpCard(tx *db.Tx, board, card int64) error {
	if _, err := tx.Exec(`UPDATE cards SET version = version + 1 WHERE id = ?`, card); err != nil {
		return err
	}
	_, err := tx.Touch(board)
	return err
}

// CreateList adds a list at the end of the board.
func (a *App) CreateList(ctx context.Context, actor Actor, board int64, name string) (Outcome, error) {
	return a.act(ctx, actor, board, func(tx *db.Tx) (string, error) {
		name, err := cleanText("A list name", name, 100, false)
		if err != nil {
			return "", err
		}
		pos, err := endPosition(tx, "lists", "board_id", board)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(`INSERT INTO lists (board_id, name, position) VALUES (?, ?, ?)`, board, name, pos); err != nil {
			return "", err
		}
		_, err = tx.Touch(board)
		return "Added the list " + quote(name) + ".", err
	})
}

// RenameList renames a list.
func (a *App) RenameList(ctx context.Context, actor Actor, board, list int64, name string) (Outcome, error) {
	return a.act(ctx, actor, board, func(tx *db.Tx) (string, error) {
		if _, err := listOn(tx, board, list); err != nil {
			return "", err
		}
		name, err := cleanText("A list name", name, 100, false)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(`UPDATE lists SET name = ? WHERE id = ?`, name, list); err != nil {
			return "", err
		}
		_, err = tx.Touch(board)
		return "Renamed the list to " + quote(name) + ".", err
	})
}

// MoveList puts a list before another one (0: at the end), unless it was
// moved since the page showed it (seen is its move_version there; -1
// skips the check).
func (a *App) MoveList(ctx context.Context, actor Actor, board, list, before, seen int64) (Outcome, error) {
	return a.act(ctx, actor, board, func(tx *db.Tx) (string, error) {
		name, err := listOn(tx, board, list)
		if err != nil {
			return "", err
		}
		var moveVersion int64
		var movedBy sql.NullInt64
		if err := tx.QueryRow(`SELECT move_version, moved_by FROM lists WHERE id = ?`, list).Scan(&moveVersion, &movedBy); err != nil {
			return "", err
		}
		if seen >= 0 && moveVersion != seen {
			who := "Someone"
			if movedBy.Valid {
				if movedBy.Int64 == actor.User {
					who = "You"
				} else {
					var n string
					if tx.QueryRow(`SELECT name FROM users WHERE id = ?`, movedBy.Int64).Scan(&n) == nil {
						who = User{Name: n}.FirstName()
					}
				}
			}
			return "", reject("%s moved the list %s meanwhile. It stays where it is.", who, quote(name))
		}
		if before == list {
			return "", reject("A list can't move before itself.")
		}
		if before != 0 {
			if _, err := listOn(tx, board, before); err != nil {
				return "", reject("The list it should go before is gone. Nothing moved.")
			}
		}
		pos, err := positionBefore(tx, "lists", "board_id", board, list, before)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(`UPDATE lists SET position = ?, moved_by = ?, move_version = move_version + 1 WHERE id = ?`, pos, actor.User, list); err != nil {
			return "", err
		}
		_, err = tx.Touch(board)
		return "Moved the list " + quote(name) + ".", err
	})
}

// ArchiveList hides a list and its cards.
func (a *App) ArchiveList(ctx context.Context, actor Actor, board, list int64) (Outcome, error) {
	return a.act(ctx, actor, board, func(tx *db.Tx) (string, error) {
		name, err := listOn(tx, board, list)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(`UPDATE lists SET archived_at = ? WHERE id = ?`, time.Now().Unix(), list); err != nil {
			return "", err
		}
		_, err = tx.Touch(board)
		return "Archived the list " + quote(name) + ".", err
	})
}

// CreateCard adds a card at the end of a list.
func (a *App) CreateCard(ctx context.Context, actor Actor, board, list int64, title string) (Outcome, error) {
	return a.act(ctx, actor, board, func(tx *db.Tx) (string, error) {
		lname, err := listOn(tx, board, list)
		if err != nil {
			return "", err
		}
		title, err := cleanText("A card title", title, 200, false)
		if err != nil {
			return "", err
		}
		pos, err := endPosition(tx, "cards", "list_id", list)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(`INSERT INTO cards (board_id, list_id, title, position, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			board, list, title, pos, actor.User, time.Now().Unix()); err != nil {
			return "", err
		}
		_, err = tx.Touch(board)
		return "Added " + quote(title) + " to " + quote(lname) + ".", err
	})
}

// RenameCard changes a card's title.
func (a *App) RenameCard(ctx context.Context, actor Actor, board, card int64, title string) (Outcome, error) {
	return a.act(ctx, actor, board, func(tx *db.Tx) (string, error) {
		if _, _, err := cardOn(tx, board, card); err != nil {
			return "", err
		}
		title, err := cleanText("A card title", title, 200, false)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(`UPDATE cards SET title = ? WHERE id = ?`, title, card); err != nil {
			return "", err
		}
		return "Renamed the card to " + quote(title) + ".", bumpCard(tx, board, card)
	})
}

// Move is a card move as the page asks for it.
type Move struct {
	Card   int64
	List   int64 // the target lane
	Before int64 // the card it should precede in that lane, 0 for the end
	Seen   int64 // the card's move_version on the page
}

// MoveCard applies a move, unless the card or its anchor moved since the
// page was drawn: then it says who moved what, and nothing changes.
func (a *App) MoveCard(ctx context.Context, actor Actor, board int64, m Move) (Outcome, error) {
	return a.act(ctx, actor, board, func(tx *db.Tx) (string, error) {
		var (
			from, moveVersion int64
			title             string
			archived          sql.NullInt64
			movedBy           sql.NullInt64
		)
		err := tx.QueryRow(`SELECT list_id, title, move_version, archived_at, moved_by FROM cards WHERE id = ? AND board_id = ?`, m.Card, board).
			Scan(&from, &title, &moveVersion, &archived, &movedBy)
		if errors.Is(err, sql.ErrNoRows) {
			return "", reject("This card no longer exists.")
		} else if err != nil {
			return "", err
		}
		if archived.Valid {
			return "", reject("%s was archived meanwhile. Nothing moved.", quote(title))
		}
		if moveVersion != m.Seen {
			who := "Someone"
			if movedBy.Valid {
				if movedBy.Int64 == actor.User {
					who = "You"
				} else {
					var name string
					if tx.QueryRow(`SELECT name FROM users WHERE id = ?`, movedBy.Int64).Scan(&name) == nil {
						who = User{Name: name}.FirstName()
					}
				}
			}
			var where string
			_ = tx.QueryRow(`SELECT name FROM lists WHERE id = ?`, from).Scan(&where)
			return "", reject("%s moved %s to %s meanwhile. It stays there.", who, quote(title), quote(where))
		}
		to, err := listOn(tx, board, m.List)
		if err != nil {
			return "", err
		}
		if m.Before == m.Card {
			return "", reject("A card can't go before itself.")
		}
		if m.Before != 0 {
			var bl int64
			var barch sql.NullInt64
			err := tx.QueryRow(`SELECT list_id, archived_at FROM cards WHERE id = ? AND board_id = ?`, m.Before, board).Scan(&bl, &barch)
			if err != nil || barch.Valid || bl != m.List {
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return "", err
				}
				return "", reject("The board changed under this move: the card it should go before isn't in %s any more. Nothing moved.", quote(to))
			}
		}
		pos, err := positionBefore(tx, "cards", "list_id", m.List, m.Card, m.Before)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(`UPDATE cards SET list_id = ?, position = ?, move_version = move_version + 1, version = version + 1, moved_by = ? WHERE id = ?`,
			m.List, pos, actor.User, m.Card); err != nil {
			return "", err
		}
		if _, err := tx.Touch(board); err != nil {
			return "", err
		}
		if from == m.List {
			return "Moved " + quote(title) + " within " + quote(to) + ".", nil
		}
		return "Moved " + quote(title) + " to " + quote(to) + ".", nil
	})
}

// ArchiveCard hides a card.
func (a *App) ArchiveCard(ctx context.Context, actor Actor, board, card int64) (Outcome, error) {
	return a.act(ctx, actor, board, func(tx *db.Tx) (string, error) {
		_, title, err := cardOn(tx, board, card)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(`UPDATE cards SET archived_at = ? WHERE id = ?`, time.Now().Unix(), card); err != nil {
			return "", err
		}
		return "Archived " + quote(title) + ".", bumpCard(tx, board, card)
	})
}

// SetDescription saves a card's Markdown description ("" removes it).
func (a *App) SetDescription(ctx context.Context, actor Actor, board, card int64, text string) (Outcome, error) {
	return a.act(ctx, actor, board, func(tx *db.Tx) (string, error) {
		if _, _, err := cardOn(tx, board, card); err != nil {
			return "", err
		}
		text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
		if text != "" {
			var err error
			if text, err = cleanText("The description", text, 20000, true); err != nil {
				return "", err
			}
		}
		if _, err := tx.Exec(`UPDATE cards SET description = ? WHERE id = ?`, text, card); err != nil {
			return "", err
		}
		return "Saved the description.", bumpCard(tx, board, card)
	})
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// SetDue sets or clears ("") a card's due date.
func (a *App) SetDue(ctx context.Context, actor Actor, board, card int64, day string) (Outcome, error) {
	return a.act(ctx, actor, board, func(tx *db.Tx) (string, error) {
		if _, _, err := cardOn(tx, board, card); err != nil {
			return "", err
		}
		var due any
		notice := "Removed the due date."
		if day != "" {
			t, err := time.Parse(time.DateOnly, day)
			if !dateRe.MatchString(day) || err != nil || t.Year() < 2000 || t.Year() > 2100 {
				return "", reject("%q isn't a date.", day)
			}
			due, notice = day, "Due on "+t.Format("2 January 2006")+"."
		}
		if _, err := tx.Exec(`UPDATE cards SET due_on = ?, due_done = CASE WHEN ? IS NULL THEN 0 ELSE due_done END WHERE id = ?`, due, due, card); err != nil {
			return "", err
		}
		return notice, bumpCard(tx, board, card)
	})
}

// SetDueDone marks a due date as done or not.
func (a *App) SetDueDone(ctx context.Context, actor Actor, board, card int64, done bool) (Outcome, error) {
	return a.act(ctx, actor, board, func(tx *db.Tx) (string, error) {
		if _, _, err := cardOn(tx, board, card); err != nil {
			return "", err
		}
		var due sql.NullString
		if err := tx.QueryRow(`SELECT due_on FROM cards WHERE id = ?`, card).Scan(&due); err != nil {
			return "", err
		}
		if !due.Valid {
			return "", reject("The card has no due date any more.")
		}
		if _, err := tx.Exec(`UPDATE cards SET due_done = ? WHERE id = ?`, done, card); err != nil {
			return "", err
		}
		if done {
			return "Marked the due date as done.", bumpCard(tx, board, card)
		}
		return "Marked the due date as not done.", bumpCard(tx, board, card)
	})
}

// SetLabel puts a board label on a card or takes it off.
func (a *App) SetLabel(ctx context.Context, actor Actor, board, card, label int64, on bool) (Outcome, error) {
	return a.act(ctx, actor, board, func(tx *db.Tx) (string, error) {
		if _, _, err := cardOn(tx, board, card); err != nil {
			return "", err
		}
		var name string
		if err := tx.QueryRow(`SELECT name FROM labels WHERE id = ? AND board_id = ?`, label, board).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", reject("This label no longer exists.")
			}
			return "", err
		}
		q := `INSERT OR IGNORE INTO card_labels (card_id, label_id) VALUES (?, ?)`
		notice := "Added the label " + quote(name) + "."
		if !on {
			q = `DELETE FROM card_labels WHERE card_id = ? AND label_id = ?`
			notice = "Removed the label " + quote(name) + "."
		}
		if _, err := tx.Exec(q, card, label); err != nil {
			return "", err
		}
		return notice, bumpCard(tx, board, card)
	})
}

// LabelColors are the colours a label can have.
var LabelColors = []string{"green", "yellow", "orange", "red", "purple", "blue", "sky", "grey"}

// CreateLabel adds a label to the board and puts it on card (if card != 0).
func (a *App) CreateLabel(ctx context.Context, actor Actor, board, card int64, name, color string) (Outcome, error) {
	return a.act(ctx, actor, board, func(tx *db.Tx) (string, error) {
		name, err := cleanText("A label name", name, 40, false)
		if err != nil {
			return "", err
		}
		ok := false
		for _, c := range LabelColors {
			ok = ok || c == color
		}
		if !ok {
			return "", reject("Pick one of the label colours.")
		}
		var exists int
		if err := tx.QueryRow(`SELECT count(*) FROM labels WHERE board_id = ? AND name = ? COLLATE NOCASE`, board, name).Scan(&exists); err != nil {
			return "", err
		}
		if exists > 0 {
			return "", reject("The board already has a label %s.", quote(name))
		}
		var id int64
		if err := tx.QueryRow(`INSERT INTO labels (board_id, name, color) VALUES (?, ?, ?) RETURNING id`, board, name, color).Scan(&id); err != nil {
			return "", err
		}
		if card != 0 {
			if _, _, err := cardOn(tx, board, card); err != nil {
				return "", err
			}
			if _, err := tx.Exec(`INSERT INTO card_labels (card_id, label_id) VALUES (?, ?)`, card, id); err != nil {
				return "", err
			}
			return "Created the label " + quote(name) + ".", bumpCard(tx, board, card)
		}
		_, err = tx.Touch(board)
		return "Created the label " + quote(name) + ".", err
	})
}

// AddComment adds a comment to a card.
func (a *App) AddComment(ctx context.Context, actor Actor, board, card int64, body string) (Outcome, error) {
	return a.act(ctx, actor, board, func(tx *db.Tx) (string, error) {
		if _, _, err := cardOn(tx, board, card); err != nil {
			return "", err
		}
		body, err := cleanText("A comment", strings.ReplaceAll(body, "\r\n", "\n"), 5000, true)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(`INSERT INTO comments (card_id, user_id, body, created_at) VALUES (?, ?, ?, ?)`, card, actor.User, body, time.Now().Unix()); err != nil {
			return "", err
		}
		return "Added your comment.", bumpCard(tx, board, card)
	})
}

// DeleteComment deletes one of the actor's own comments.
func (a *App) DeleteComment(ctx context.Context, actor Actor, board, comment int64) (Outcome, error) {
	return a.act(ctx, actor, board, func(tx *db.Tx) (string, error) {
		var card, author int64
		err := tx.QueryRow(`SELECT cm.card_id, cm.user_id FROM comments cm JOIN cards c ON c.id = cm.card_id WHERE cm.id = ? AND c.board_id = ?`, comment, board).Scan(&card, &author)
		if errors.Is(err, sql.ErrNoRows) {
			return "", reject("This comment no longer exists.")
		} else if err != nil {
			return "", err
		}
		if author != actor.User {
			return "", reject("You can only delete your own comments.")
		}
		if _, err := tx.Exec(`DELETE FROM comments WHERE id = ?`, comment); err != nil {
			return "", err
		}
		return "Deleted your comment.", bumpCard(tx, board, card)
	})
}
