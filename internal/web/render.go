package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"hash/maphash"
	"slices"
	"sync/atomic"
	"time"

	"github.com/a-h/templ"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	lithlive "github.com/zweiundeins/lith/live"

	"kanban/internal/app"
	"kanban/internal/live"
	"kanban/internal/views"
)

// Renderer turns board state into the fragments every viewer gets. Its
// caches are content-addressed: an unchanged card is copied, not rendered
// again.
type Renderer struct {
	Now     func() time.Time
	seed    maphash.Seed
	cards   *lithlive.Cache[uint64]
	lanes   *lithlive.Cache[uint64]
	details *lithlive.Cache[uint64]
	md      goldmark.Markdown

	renders, renderNanos atomic.Int64 // board frames, for the health check
}

func NewRenderer() *Renderer {
	return &Renderer{
		Now:     time.Now,
		seed:    maphash.MakeSeed(),
		cards:   lithlive.NewCache[uint64](32 << 20),
		lanes:   lithlive.NewCache[uint64](16 << 20),
		details: lithlive.NewCache[uint64](8 << 20),
		md:      goldmark.New(goldmark.WithExtensions(extension.GFM)), // raw HTML stays off
	}
}

func (r *Renderer) key(parts ...any) uint64 {
	var h maphash.Hash
	h.SetSeed(r.seed)
	enc := json.NewEncoder(&h)
	for _, p := range parts {
		_ = enc.Encode(p)
	}
	return h.Sum64()
}

func render(c templ.Component) []byte {
	var buf bytes.Buffer
	_ = c.Render(context.Background(), &buf)
	return buf.Bytes()
}

func (r *Renderer) today() time.Time {
	y, m, d := r.Now().UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Lanes renders the board's lists with cached cards and lanes, as keyed
// children: a stream sends only the lanes that changed.
func (r *Renderer) Lanes(b *app.Board) *live.Keyed {
	today := r.today()
	day := today.Format(time.DateOnly)
	card := func(c app.Card) string {
		return string(r.cards.Get(r.key("card", b.ID, c, day), func() []byte {
			return render(views.Card(b.ID, c, today))
		}))
	}
	wrap := render(views.LanesWrap())
	open, close, _ := bytes.Cut(wrap, []byte(views.LanesMarker))
	k := &live.Keyed{Open: open, Close: close}
	for i, l := range b.Lists {
		var prev, next int64
		if i > 0 {
			prev = b.Lists[i-1].ID
		}
		if i+1 < len(b.Lists) {
			next = b.Lists[i+1].ID
		}
		html := r.lanes.Get(r.key("lane", b.ID, i, prev, next, len(b.Lists), l, day), func() []byte {
			return render(views.Lane(b, i, l, card))
		})
		k.Children = append(k.Children, live.Child{Key: l.ID, HTML: html, Shell: views.LaneShell(l.ID)})
	}
	return k
}

// Presence renders who is on the board (only members count).
func (r *Renderer) Presence(members []app.User, present []int64) []byte {
	var users []app.User
	for _, m := range members {
		if slices.Contains(present, m.ID) {
			users = append(users, m)
		}
	}
	return render(views.Presence(users))
}

// Detail renders an open card, or the dialog saying it's gone; viewers
// are the users who have it open.
func (r *Renderer) Detail(ctx context.Context, tx *sql.Tx, board, card int64, viewers []app.User) ([]byte, error) {
	if card == 0 {
		return render(views.DetailClosed(board)), nil
	}
	c, err := app.ReadCard(ctx, tx, board, card)
	if errors.Is(err, app.ErrNotFound) || (err == nil && c.Archived) {
		return render(views.DetailGone(board)), nil
	}
	if err != nil {
		return nil, err
	}
	now := r.Now()
	// Comment ages say "5 minutes ago": a new minute is a new rendering.
	return r.details.Get(r.key("detail", c, viewers, now.Unix()/60), func() []byte {
		var desc bytes.Buffer
		if c.Description != "" {
			if err := r.md.Convert([]byte(c.Description), &desc); err != nil {
				desc.Reset()
				desc.WriteString("<p>")
				desc.WriteString(templ.EscapeString(c.Description))
				desc.WriteString("</p>")
			}
		}
		return render(views.Detail(c, desc.String(), viewers, now))
	}), nil
}

// Board renders the board's frame in tx: its lanes, and the dialogs of
// the cards open in its tabs (open maps each to the users who have it
// open).
func (r *Renderer) Board(ctx context.Context, tx *sql.Tx, board int64, open map[int64][]int64) (*live.Board, error) {
	start := time.Now()
	b, err := app.ReadBoard(ctx, tx, board)
	if err != nil {
		return nil, err
	}
	f := &live.Board{
		Version: b.Version,
		Lanes:   r.Lanes(b),
		Details: make(map[int64][]byte, len(open)),
	}
	if f.Closed, err = r.Detail(ctx, tx, board, 0, nil); err != nil {
		return nil, err
	}
	for card, users := range open {
		var viewers []app.User
		for _, m := range b.Members {
			if slices.Contains(users, m.ID) {
				viewers = append(viewers, m)
			}
		}
		if f.Details[card], err = r.Detail(ctx, tx, board, card, viewers); err != nil {
			return nil, err
		}
	}
	r.renders.Add(1)
	r.renderNanos.Add(int64(time.Since(start)))
	return f, nil
}
