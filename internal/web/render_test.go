package web

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/andybalholm/brotli"

	"kanban/internal/app"
	"kanban/internal/db"
)

func bigBoard(tb testing.TB) (*app.App, *Renderer) {
	tb.Helper()
	d, err := db.Open(context.Background(), filepath.Join(tb.TempDir(), "r.db"), db.Options{Synchronous: "NORMAL"})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { d.Close() })
	a := &app.App{DB: d}
	if err := a.Seed(context.Background(), "", ""); err != nil {
		tb.Fatal(err)
	}
	return a, NewRenderer(a)
}

// A frame of the 200-card board after one move: rendered from the cache,
// and a few hundred bytes on a brotli stream that already carried the last one.
func TestFrameAfterAMove(t *testing.T) {
	a, r := bigBoard(t)
	ctx := context.Background()
	f1, err := r.Frame(ctx, 2, nil, []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := a.LoadBoard(ctx, 2)
	c := b.Lists[0].Cards[0]
	if out, err := a.MoveCard(ctx, app.Actor{User: 1}, 2, app.Move{Card: c.ID, List: b.Lists[3].ID, Seen: c.MoveVersion}); err != nil || !out.OK {
		t.Fatalf("move: %+v %v", out, err)
	}
	r.cards.mu.Lock()
	missBefore := r.cards.miss
	r.cards.mu.Unlock()
	f2, err := r.Frame(ctx, 2, nil, []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	r.cards.mu.Lock()
	rendered := r.cards.miss - missBefore
	r.cards.mu.Unlock()
	if rendered > 1 {
		t.Fatalf("%d cards rendered again after one move", rendered)
	}

	changed := 0
	for i, c := range f2.Board.Children {
		if !bytes.Equal(c.HTML, f1.Board.Children[i].HTML) {
			changed++
		}
	}
	if changed != 2 {
		t.Fatalf("%d lanes changed after a move between two lanes", changed)
	}
	full := f2.Board.Full()
	var wire bytes.Buffer
	w := brotli.NewWriterOptions(&wire, brotli.WriterOptions{Quality: 4, LGWin: 18})
	w.Write(f1.Board.Full())
	w.Flush()
	t.Logf("board HTML %d bytes, %d on the wire at first", len(full), wire.Len())
}

func BenchmarkFrame(b *testing.B) {
	_, r := bigBoard(b)
	ctx := context.Background()
	r.Frame(ctx, 2, nil, nil) // warm the cache
	open := map[int64][]int64{1: {1}, 2: {2}}
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.Frame(ctx, 2, open, []int64{1, 2}); err != nil {
			b.Fatal(err)
		}
	}
}
