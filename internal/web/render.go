package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"hash/maphash"
	"slices"
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"kanban/internal/app"
	"kanban/internal/live"
	"kanban/internal/views"
)

// cache keeps rendered fragments by the hash of what they show
// (content-addressed): an unchanged card is copied, not rendered again.
type cache struct {
	mu    sync.Mutex
	items map[uint64][]byte
	max   int
	hits  int64
	miss  int64
}

func newCache(max int) *cache { return &cache{items: map[uint64][]byte{}, max: max} }

func (c *cache) get(key uint64, render func() []byte) []byte {
	c.mu.Lock()
	if b, ok := c.items[key]; ok {
		c.hits++
		c.mu.Unlock()
		return b
	}
	c.miss++
	c.mu.Unlock()
	b := render()
	c.mu.Lock()
	if len(c.items) >= c.max {
		clear(c.items) // crude, but bounded; hot entries come back on the next frame
	}
	c.items[key] = b
	c.mu.Unlock()
	return b
}

// Renderer turns board state into the fragments every viewer gets.
type Renderer struct {
	App     *app.App
	Now     func() time.Time
	seed    maphash.Seed
	cards   *cache
	lanes   *cache
	details *cache
	md      goldmark.Markdown
}

func NewRenderer(a *app.App) *Renderer {
	return &Renderer{
		App:     a,
		Now:     time.Now,
		seed:    maphash.MakeSeed(),
		cards:   newCache(50_000),
		lanes:   newCache(5_000),
		details: newCache(2_000),
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
		return string(r.cards.get(r.key("card", b.ID, c, day), func() []byte {
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
		html := r.lanes.get(r.key("lane", b.ID, i, prev, next, len(b.Lists), l, day), func() []byte {
			return render(views.Lane(b, i, l, card))
		})
		k.Children = append(k.Children, live.Child{Key: l.ID, HTML: html, Shell: views.LaneShell(l.ID)})
	}
	return k
}

// Presence renders who is on the board (only members count).
func (r *Renderer) Presence(b *app.Board, present []int64) []byte {
	var users []app.User
	for _, m := range b.Members {
		if slices.Contains(present, m.ID) {
			users = append(users, m)
		}
	}
	return render(views.Presence(users))
}

// Detail renders an open card, or the dialog saying it's gone; viewers
// are the users who have it open.
func (r *Renderer) Detail(ctx context.Context, board, card int64, viewers []app.User) ([]byte, error) {
	if card == 0 {
		return render(views.DetailClosed(board)), nil
	}
	c, err := r.App.LoadCard(ctx, board, card)
	if errors.Is(err, app.ErrNotFound) || (err == nil && c.Archived) {
		return render(views.DetailGone(board)), nil
	}
	if err != nil {
		return nil, err
	}
	now := r.Now()
	// Comment ages say "5 minutes ago": a new minute is a new rendering.
	return r.details.get(r.key("detail", c, viewers, now.Unix()/60), func() []byte {
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

// Frame is the live.Renderer.
func (r *Renderer) Frame(ctx context.Context, board int64, open map[int64][]int64, present []int64) (*live.Frame, error) {
	b, err := r.App.LoadBoard(ctx, board)
	if err != nil {
		return nil, err
	}
	f := &live.Frame{
		Version: b.Version,
		Parts:   [][]byte{r.Presence(b, present)},
		Board:   r.Lanes(b),
		Details: make(map[int64][]byte, len(open)),
	}
	if f.Closed, err = r.Detail(ctx, board, 0, nil); err != nil {
		return nil, err
	}
	for o, users := range open {
		var viewers []app.User
		for _, m := range b.Members {
			if slices.Contains(users, m.ID) {
				viewers = append(viewers, m)
			}
		}
		if f.Details[o], err = r.Detail(ctx, board, o, viewers); err != nil {
			return nil, err
		}
	}
	return f, nil
}
