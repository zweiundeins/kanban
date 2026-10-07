package app

import (
	"context"
	"database/sql"
)

// Export is the boards as data, for loading the same boards into the app
// we compare with.
type Export struct {
	Users    []ExportUser    `json:"users"`
	Projects []ExportProject `json:"projects"`
}

type ExportUser struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

type ExportProject struct {
	Name   string        `json:"name"`
	Boards []ExportBoard `json:"boards"`
}

type ExportBoard struct {
	Name    string       `json:"name"`
	Members []string     `json:"members"`
	Labels  []Label      `json:"labels"`
	Lists   []ExportList `json:"lists"`
}

type ExportList struct {
	Name  string       `json:"name"`
	Cards []ExportCard `json:"cards"`
}

type ExportCard struct {
	Title       string          `json:"title"`
	Description string          `json:"description,omitempty"`
	DueOn       string          `json:"due_on,omitempty"`
	DueDone     bool            `json:"due_done,omitempty"`
	Labels      []string        `json:"labels,omitempty"`
	Comments    []ExportComment `json:"comments,omitempty"`
}

type ExportComment struct {
	Author string `json:"author"` // email
	Body   string `json:"body"`
}

// ExportAll reads every project, board, list and card.
func (a *App) ExportAll(ctx context.Context) (*Export, error) {
	var out Export
	err := a.DB.ReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT email, name FROM users ORDER BY id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var u ExportUser
			if err := rows.Scan(&u.Email, &u.Name); err != nil {
				return err
			}
			out.Users = append(out.Users, u)
		}
		rows.Close()
		projects, err := tx.QueryContext(ctx, `SELECT id, name FROM projects ORDER BY id`)
		if err != nil {
			return err
		}
		type pr struct {
			id   int64
			name string
		}
		var ps []pr
		for projects.Next() {
			var p pr
			if err := projects.Scan(&p.id, &p.name); err != nil {
				return err
			}
			ps = append(ps, p)
		}
		projects.Close()
		for _, p := range ps {
			ep := ExportProject{Name: p.name}
			refs, err := boardRefs(ctx, tx, p.id)
			if err != nil {
				return err
			}
			for _, ref := range refs {
				b, err := a.LoadBoard(ctx, ref.ID)
				if err != nil {
					return err
				}
				eb := ExportBoard{Name: b.Name, Labels: b.Labels}
				for _, m := range b.Members {
					eb.Members = append(eb.Members, m.Email)
				}
				for _, l := range b.Lists {
					el := ExportList{Name: l.Name}
					for _, c := range l.Cards {
						d, err := a.LoadCard(ctx, b.ID, c.ID)
						if err != nil {
							return err
						}
						ec := ExportCard{Title: d.Title, Description: d.Description, DueOn: d.DueOn, DueDone: d.DueDone}
						for _, lab := range d.Labels {
							ec.Labels = append(ec.Labels, lab.Name)
						}
						for _, cm := range d.CommentList {
							ec.Comments = append(ec.Comments, ExportComment{Author: cm.Author.Email, Body: cm.Body})
						}
						el.Cards = append(el.Cards, ec)
					}
					eb.Lists = append(eb.Lists, el)
				}
				ep.Boards = append(ep.Boards, eb)
			}
			out.Projects = append(out.Projects, ep)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
