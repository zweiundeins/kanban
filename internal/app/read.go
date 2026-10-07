// Package app holds the board's queries and commands: what the views read,
// and the only ways anything changes.
package app

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"kanban/internal/db"
)

// App is the domain layer over the database.
type App struct {
	DB *db.DB
}

// ErrNotFound means the thing doesn't exist, or the user may not see it.
var ErrNotFound = errors.New("not found")

type User struct {
	ID    int64
	Email string
	Name  string
	Color string
}

// Initials are what an avatar shows.
func (u User) Initials() string {
	parts := strings.Fields(u.Name)
	out := ""
	for _, p := range parts {
		r := []rune(p)
		if len(r) > 0 {
			out += strings.ToUpper(string(r[0]))
		}
		if len([]rune(out)) == 2 {
			break
		}
	}
	if out == "" {
		return "?"
	}
	return out
}

// FirstName is how others are named in messages.
func (u User) FirstName() string {
	if f := strings.Fields(u.Name); len(f) > 0 {
		return f[0]
	}
	return u.Name
}

type Label struct {
	ID    int64
	Name  string
	Color string
}

type Card struct {
	ID          int64
	ListID      int64
	Title       string
	Version     int64
	MoveVersion int64
	DueOn       string // YYYY-MM-DD or ""
	DueDone     bool
	HasDesc     bool
	Labels      []Label
	Comments    int
}

type List struct {
	ID          int64
	Name        string
	MoveVersion int64 // what a move from this page must still find
	Cards       []Card
}

type Board struct {
	ID          int64
	Name        string
	Version     int64
	ProjectID   int64
	ProjectName string
	Lists       []List
	Labels      []Label
	Members     []User
	Siblings    []BoardRef // the project's other boards, for switching
}

type BoardRef struct {
	ID   int64
	Name string
}

type Comment struct {
	ID        int64
	Author    User
	Body      string
	CreatedAt int64
}

type CardDetail struct {
	Card
	BoardID     int64
	ListName    string
	Description string
	CreatedBy   User
	CreatedAt   int64
	CommentList []Comment
	BoardLabels []Label
	Archived    bool
}

type Project struct {
	ID     int64
	Name   string
	Boards []BoardRef
}

// IsMember reports whether user may see and change board.
func (a *App) IsMember(ctx context.Context, board, user int64) (bool, error) {
	var one int
	err := a.DB.Read.QueryRowContext(ctx, "SELECT 1 FROM board_members WHERE board_id = ? AND user_id = ?", board, user).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Projects lists the projects and boards user is a member of.
func (a *App) Projects(ctx context.Context, user int64) ([]Project, error) {
	rows, err := a.DB.Read.QueryContext(ctx, `
		SELECT p.id, p.name, b.id, b.name FROM boards b
		JOIN projects p ON p.id = b.project_id
		JOIN board_members m ON m.board_id = b.id AND m.user_id = ?
		ORDER BY p.name, p.id, b.position`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var pid int64
		var pname string
		var b BoardRef
		if err := rows.Scan(&pid, &pname, &b.ID, &b.Name); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != pid {
			out = append(out, Project{ID: pid, Name: pname})
		}
		out[len(out)-1].Boards = append(out[len(out)-1].Boards, b)
	}
	return out, rows.Err()
}

// LoadBoard reads the whole board in one snapshot.
func (a *App) LoadBoard(ctx context.Context, id int64) (*Board, error) {
	var b Board
	err := a.DB.ReadTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT b.id, b.name, b.version, p.id, p.name FROM boards b JOIN projects p ON p.id = b.project_id WHERE b.id = ?`, id).
			Scan(&b.ID, &b.Name, &b.Version, &b.ProjectID, &b.ProjectName)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if b.Siblings, err = boardRefs(ctx, tx, b.ProjectID); err != nil {
			return err
		}
		if b.Labels, err = boardLabels(ctx, tx, id); err != nil {
			return err
		}
		labelByID := make(map[int64]Label, len(b.Labels))
		for _, l := range b.Labels {
			labelByID[l.ID] = l
		}
		if b.Members, err = boardMembers(ctx, tx, id); err != nil {
			return err
		}

		rows, err := tx.QueryContext(ctx, `SELECT id, name, move_version FROM lists WHERE board_id = ? AND archived_at IS NULL ORDER BY position, id`, id)
		if err != nil {
			return err
		}
		listIdx := map[int64]int{}
		for rows.Next() {
			var l List
			if err := rows.Scan(&l.ID, &l.Name, &l.MoveVersion); err != nil {
				rows.Close()
				return err
			}
			listIdx[l.ID] = len(b.Lists)
			b.Lists = append(b.Lists, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		cardLabels := map[int64][]Label{}
		rows, err = tx.QueryContext(ctx, `
			SELECT cl.card_id, cl.label_id FROM card_labels cl
			JOIN cards c ON c.id = cl.card_id
			WHERE c.board_id = ? AND c.archived_at IS NULL
			ORDER BY cl.card_id, cl.label_id`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c, l int64
			if err := rows.Scan(&c, &l); err != nil {
				rows.Close()
				return err
			}
			if lab, ok := labelByID[l]; ok {
				cardLabels[c] = append(cardLabels[c], lab)
			}
		}
		rows.Close()

		comments := map[int64]int{}
		rows, err = tx.QueryContext(ctx, `
			SELECT cm.card_id, count(*) FROM comments cm
			JOIN cards c ON c.id = cm.card_id
			WHERE c.board_id = ? AND c.archived_at IS NULL
			GROUP BY cm.card_id`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c int64
			var n int
			if err := rows.Scan(&c, &n); err != nil {
				rows.Close()
				return err
			}
			comments[c] = n
		}
		rows.Close()

		rows, err = tx.QueryContext(ctx, `
			SELECT id, list_id, title, version, move_version, coalesce(due_on, ''), due_done, description != ''
			FROM cards WHERE board_id = ? AND archived_at IS NULL
			ORDER BY list_id, position, id`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c Card
			if err := rows.Scan(&c.ID, &c.ListID, &c.Title, &c.Version, &c.MoveVersion, &c.DueOn, &c.DueDone, &c.HasDesc); err != nil {
				return err
			}
			c.Labels = cardLabels[c.ID]
			c.Comments = comments[c.ID]
			if i, ok := listIdx[c.ListID]; ok {
				b.Lists[i].Cards = append(b.Lists[i].Cards, c)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func boardRefs(ctx context.Context, tx *sql.Tx, project int64) ([]BoardRef, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name FROM boards WHERE project_id = ? ORDER BY position, id`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BoardRef
	for rows.Next() {
		var r BoardRef
		if err := rows.Scan(&r.ID, &r.Name); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func boardLabels(ctx context.Context, tx *sql.Tx, board int64) ([]Label, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, color FROM labels WHERE board_id = ? ORDER BY name, id`, board)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Label
	for rows.Next() {
		var l Label
		if err := rows.Scan(&l.ID, &l.Name, &l.Color); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func boardMembers(ctx context.Context, tx *sql.Tx, board int64) ([]User, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT u.id, u.email, u.name, u.color FROM users u
		JOIN board_members m ON m.user_id = u.id AND m.board_id = ?
		ORDER BY u.name`, board)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.Color); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// LoadCard reads one card with its comments, for the detail view.
func (a *App) LoadCard(ctx context.Context, board, id int64) (*CardDetail, error) {
	var c CardDetail
	err := a.DB.ReadTx(ctx, func(tx *sql.Tx) error {
		var archived sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			SELECT c.id, c.board_id, c.list_id, l.name, c.title, c.description, c.version, c.move_version,
			       coalesce(c.due_on, ''), c.due_done, c.archived_at, c.created_at,
			       u.id, u.email, u.name, u.color
			FROM cards c JOIN lists l ON l.id = c.list_id JOIN users u ON u.id = c.created_by
			WHERE c.id = ? AND c.board_id = ?`, id, board).
			Scan(&c.ID, &c.BoardID, &c.ListID, &c.ListName, &c.Title, &c.Description, &c.Version, &c.MoveVersion,
				&c.DueOn, &c.DueDone, &archived, &c.CreatedAt,
				&c.CreatedBy.ID, &c.CreatedBy.Email, &c.CreatedBy.Name, &c.CreatedBy.Color)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		c.Archived = archived.Valid
		c.HasDesc = c.Description != ""
		if c.BoardLabels, err = boardLabels(ctx, tx, board); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT l.id, l.name, l.color FROM card_labels cl JOIN labels l ON l.id = cl.label_id WHERE cl.card_id = ? ORDER BY l.name, l.id`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var l Label
			if err := rows.Scan(&l.ID, &l.Name, &l.Color); err != nil {
				rows.Close()
				return err
			}
			c.Labels = append(c.Labels, l)
		}
		rows.Close()
		rows, err = tx.QueryContext(ctx, `
			SELECT cm.id, cm.body, cm.created_at, u.id, u.email, u.name, u.color
			FROM comments cm JOIN users u ON u.id = cm.user_id
			WHERE cm.card_id = ? ORDER BY cm.id`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var cm Comment
			if err := rows.Scan(&cm.ID, &cm.Body, &cm.CreatedAt, &cm.Author.ID, &cm.Author.Email, &cm.Author.Name, &cm.Author.Color); err != nil {
				return err
			}
			c.CommentList = append(c.CommentList, cm)
		}
		c.Comments = len(c.CommentList)
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// UserByID reads one user.
func (a *App) UserByID(ctx context.Context, id int64) (User, error) {
	var u User
	err := a.DB.Read.QueryRowContext(ctx, `SELECT id, email, name, color FROM users WHERE id = ?`, id).Scan(&u.ID, &u.Email, &u.Name, &u.Color)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}
