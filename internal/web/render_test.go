package web

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/andybalholm/brotli"

	"kanban/internal/app"
	"kanban/internal/db"
	"kanban/internal/live"
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
	return a, NewRenderer()
}

func frame(tb testing.TB, a *app.App, r *Renderer, board int64, open map[int64][]int64) *live.Board {
	tb.Helper()
	var f *live.Board
	err := a.DB.View(context.Background(), func(tx *sql.Tx) (err error) {
		f, err = r.Board(context.Background(), tx, board, open)
		return err
	})
	if err != nil {
		tb.Fatal(err)
	}
	return f
}

// A frame of the 200-card board after one move: rendered from the cache,
// and a few hundred bytes on a brotli stream that already carried the last one.
func TestFrameAfterAMove(t *testing.T) {
	a, r := bigBoard(t)
	ctx := context.Background()
	f1 := frame(t, a, r, 2, nil)
	b, _ := a.LoadBoard(ctx, 2)
	c := b.Lists[0].Cards[0]
	if out, err := a.MoveCard(ctx, app.Actor{User: 1}, 2, app.Move{Card: c.ID, List: b.Lists[3].ID, Seen: c.MoveVersion}); err != nil || !out.OK {
		t.Fatalf("move: %+v %v", out, err)
	}
	missBefore := r.cards.Stats().Misses
	f2 := frame(t, a, r, 2, nil)
	if rendered := r.cards.Stats().Misses - missBefore; rendered > 1 {
		t.Fatalf("%d cards rendered again after one move", rendered)
	}
	if f2.Version != f1.Version+1 {
		t.Fatalf("versions %d, %d", f1.Version, f2.Version)
	}

	changed := 0
	for i, c := range f2.Lanes.Children {
		if !bytes.Equal(c.HTML, f1.Lanes.Children[i].HTML) {
			changed++
		}
	}
	if changed != 2 {
		t.Fatalf("%d lanes changed after a move between two lanes", changed)
	}
	full := f2.Lanes.Full()
	var wire bytes.Buffer
	w := brotli.NewWriterOptions(&wire, brotli.WriterOptions{Quality: 4, LGWin: 18})
	w.Write(f1.Lanes.Full())
	w.Flush()
	t.Logf("board HTML %d bytes, %d on the wire at first", len(full), wire.Len())
}

func BenchmarkBoardFrame(b *testing.B) {
	a, r := bigBoard(b)
	frame(b, a, r, 2, nil) // warm the cache
	open := map[int64][]int64{1: {1}, 2: {2}}
	b.ResetTimer()
	for b.Loop() {
		frame(b, a, r, 2, open)
	}
}
