package app

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"kanban/internal/db"
)

// SeedUser is a demo account.
type SeedUser struct {
	Email, Name, Color string
}

// SeedUsers are the demo accounts; they all use DemoPassword.
var SeedUsers = []SeedUser{
	{"anna@example.com", "Anna Keller", "#0f766e"},
	{"ben@example.com", "Ben Okafor", "#b45309"},
	{"chiara@example.com", "Chiara Rossi", "#7c3aed"},
	{"dev@example.com", "Dev Patel", "#be123c"},
}

const DemoPassword = "demo"

var seedLabels = []struct{ Name, Color string }{
	{"Bug", "red"}, {"Feature", "green"}, {"Design", "purple"}, {"Docs", "sky"},
	{"Blocked", "orange"}, {"Quick win", "yellow"}, {"Research", "blue"}, {"Ops", "grey"},
}

var bigLists = []string{"Ideas", "Backlog", "Ready", "In progress", "Review", "Testing", "Done", "Released"}

var smallLists = []string{"To do", "Doing", "Done"}

var verbs = []string{"Fix", "Add", "Polish", "Refactor", "Document", "Test", "Measure", "Remove", "Speed up", "Design"}
var things = []string{
	"login form", "board header", "card drag preview", "keyboard moves", "label picker", "due date reminder",
	"comment editor", "archive view", "presence avatars", "SQLite backup", "Caddy config", "systemd unit",
	"error page", "search", "export to CSV", "dark mode", "focus ring", "empty states", "onboarding tour",
	"rate limiting", "session expiry", "audit log", "markdown preview", "mobile layout", "load test",
}

// Seed fills an empty database with the demo data: four users, one
// project, a small board and a big one (8 lists, 200 cards). The same
// seed always gives the same data, so ours and the reference can be
// compared on identical boards.
func (a *App) Seed(ctx context.Context, adminEmail, adminPassword string) error {
	demoHash, err := HashPassword(DemoPassword)
	if err != nil {
		return err
	}
	users := append([]SeedUser(nil), SeedUsers...)
	hashes := make([]string, len(users))
	for i := range hashes {
		hashes[i] = demoHash
	}
	if adminEmail != "" {
		h, err := HashPassword(adminPassword)
		if err != nil {
			return err
		}
		users = append(users, SeedUser{adminEmail, "Admin", "#005546"})
		hashes = append(hashes, h)
	}
	return a.DB.W.Do(ctx, func(tx *db.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM users`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("seed: the database isn't empty")
		}
		ids := make([]int64, len(users))
		for i, u := range users {
			if err := tx.QueryRow(`INSERT INTO users (email, name, color, password_hash, created_at) VALUES (?, ?, ?, ?, ?) RETURNING id`,
				u.Email, u.Name, u.Color, hashes[i], seedTime).Scan(&ids[i]); err != nil {
				return err
			}
		}
		_, err := seedBoards(tx, ids, 0)
		return err
	})
}

// Reset puts the demo boards back to the seed, keeping the accounts and
// their sessions. Board versions continue from where they were, so open
// pages simply get the next frame. It returns the boards to refresh.
func (a *App) Reset(ctx context.Context) ([]int64, error) {
	var boards []int64
	err := a.DB.W.Do(ctx, func(tx *db.Tx) error {
		var maxVersion int64
		if err := tx.QueryRow(`SELECT coalesce(max(version), 0) FROM boards`).Scan(&maxVersion); err != nil {
			return err
		}
		for _, table := range []string{"card_labels", "comments", "cards", "labels", "lists", "board_members", "boards", "projects", "ops"} {
			if _, err := tx.Exec(`DELETE FROM ` + table); err != nil {
				return err
			}
		}
		rows, err := tx.Query(`SELECT id FROM users ORDER BY id`)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		boards, err = seedBoards(tx, ids, maxVersion)
		if err != nil {
			return err
		}
		for _, b := range boards {
			if _, err := tx.Touch(b); err != nil {
				return err
			}
		}
		return nil
	})
	return boards, err
}

var seedTime = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC).Unix()

func seedBoards(tx *db.Tx, ids []int64, version int64) ([]int64, error) {
	rng := rand.New(rand.NewPCG(42, 7))
	base := seedTime
	var project int64
	if err := tx.QueryRow(`INSERT INTO projects (name, created_at) VALUES ('Product launch', ?) RETURNING id`, base).Scan(&project); err != nil {
		return nil, err
	}
	specs := []struct {
		name  string
		lists []string
		cards int
	}{{"Website", smallLists, 9}, {"Release 2.0", bigLists, 200}}
	var out []int64
	for bi, bd := range specs {
		var board int64
		if err := tx.QueryRow(`INSERT INTO boards (project_id, name, position, version, created_at) VALUES (?, ?, ?, ?, ?) RETURNING id`,
			project, bd.name, (bi+1)*gap, version, base).Scan(&board); err != nil {
			return nil, err
		}
		out = append(out, board)
		for _, u := range ids {
			if _, err := tx.Exec(`INSERT INTO board_members (board_id, user_id) VALUES (?, ?)`, board, u); err != nil {
				return nil, err
			}
		}
		labels := make([]int64, len(seedLabels))
		for i, l := range seedLabels {
			if err := tx.QueryRow(`INSERT INTO labels (board_id, name, color) VALUES (?, ?, ?) RETURNING id`, board, l.Name, l.Color).Scan(&labels[i]); err != nil {
				return nil, err
			}
		}
		lists := make([]int64, len(bd.lists))
		for i, name := range bd.lists {
			if err := tx.QueryRow(`INSERT INTO lists (board_id, name, position) VALUES (?, ?, ?) RETURNING id`, board, name, (i+1)*gap).Scan(&lists[i]); err != nil {
				return nil, err
			}
		}
		perList := make([]int, len(lists))
		for c := 0; c < bd.cards; c++ {
			li := c % len(lists)
			if bd.cards > 20 {
				li = rng.IntN(len(lists))
			}
			perList[li]++
			title := verbs[rng.IntN(len(verbs))] + " " + things[rng.IntN(len(things))]
			desc := ""
			if rng.IntN(3) == 0 {
				desc = "Why: readers asked for it.\n\n- [ ] agree on the scope\n- [ ] build it\n- [ ] **measure** before and after\n\nSee the notes in the wiki."
			}
			var due any
			if rng.IntN(4) == 0 {
				due = time.Date(2026, 10, 1+rng.IntN(60), 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
			}
			author := ids[rng.IntN(len(ids))]
			var card int64
			if err := tx.QueryRow(`INSERT INTO cards (board_id, list_id, title, description, position, due_on, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
				board, lists[li], title, desc, perList[li]*gap, due, author, base+int64(c)*600).Scan(&card); err != nil {
				return nil, err
			}
			for _, l := range rng.Perm(len(labels))[:rng.IntN(3)] {
				if _, err := tx.Exec(`INSERT INTO card_labels (card_id, label_id) VALUES (?, ?)`, card, labels[l]); err != nil {
					return nil, err
				}
			}
			for k := 0; k < rng.IntN(4); k++ {
				body := []string{"Looks good to me.", "Can we split this in two?", "I'll pick this up tomorrow.", "Measured it: 40 ms faster.", "Blocked on the API change."}[rng.IntN(5)]
				if _, err := tx.Exec(`INSERT INTO comments (card_id, user_id, body, created_at) VALUES (?, ?, ?, ?)`,
					card, ids[rng.IntN(len(ids))], body, base+int64(c)*600+int64(k+1)*300); err != nil {
					return nil, err
				}
			}
		}
	}
	return out, nil
}
