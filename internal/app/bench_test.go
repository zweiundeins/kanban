package app

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"kanban/internal/db"
)

// BenchmarkMoveCard is the writer alone, without HTTP or streams: 1, 50 or
// 200 people moving cards on the 200-card board, with an fsync on every
// commit (synchronous=FULL). Each person moves their own cards, so every
// move is a write; many people make bigger batches. The moves come from no
// tab, so they write no outcome.
//
//	go test ./internal/app -run X -bench MoveCard -benchtime 5s
func BenchmarkMoveCard(b *testing.B) {
	for _, people := range []int{1, 50, 200} {
		b.Run(fmt.Sprint(people), func(b *testing.B) { benchMoves(b, people, false) })
	}
}

// BenchmarkMoveCardOutcome is BenchmarkMoveCard with a tab for each
// person: every move also writes its outcome to the tab's outbox.
func BenchmarkMoveCardOutcome(b *testing.B) {
	for _, people := range []int{1, 50, 200} {
		b.Run(fmt.Sprint(people), func(b *testing.B) { benchMoves(b, people, true) })
	}
}

func benchMoves(b *testing.B, people int, tabs bool) {
	d, err := db.Open(context.Background(), filepath.Join(b.TempDir(), "bench.db"), db.Options{Synchronous: "FULL"})
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	a := &App{DB: d}
	if err := a.Seed(context.Background(), "", ""); err != nil {
		b.Fatal(err)
	}
	board, err := a.LoadBoard(context.Background(), 2)
	if err != nil {
		b.Fatal(err)
	}
	var lists []int64
	mine := make([]map[int64]int64, people) // card -> move version, per person
	for i := range mine {
		mine[i] = map[int64]int64{}
	}
	i := 0
	for _, l := range board.Lists {
		lists = append(lists, l.ID)
		for _, c := range l.Cards {
			mine[i%people][c.ID] = c.MoveVersion
			i++
		}
	}
	var n, refused atomic.Int64
	s0, w0 := d.W.Stats(), written()
	b.ResetTimer()
	var wg sync.WaitGroup
	for p := range people {
		wg.Go(func() {
			var cards []int64
			for c := range mine[p] {
				cards = append(cards, c)
			}
			actor := Actor{User: int64(p%4 + 1)}
			if tabs {
				actor.Tab = fmt.Sprintf("bench-tab-%010d", rand.Int64N(1e10))
			}
			for i := n.Add(1); i <= int64(b.N); i = n.Add(1) {
				c := cards[rand.IntN(len(cards))]
				actor.Op = fmt.Sprint("b", i)
				out, err := a.MoveCard(context.Background(), actor, 2, Move{Card: c, List: lists[rand.IntN(len(lists))], Seen: mine[p][c]})
				if err != nil {
					b.Error(err)
					return
				}
				if out.OK {
					mine[p][c]++
				} else {
					refused.Add(1)
				}
			}
		})
	}
	wg.Wait()
	b.StopTimer()
	s := d.W.Stats()
	batches := float64(s.Batches - s0.Batches)
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "moves/s")
	b.ReportMetric(float64(s.Commands-s0.Commands)/batches, "moves/batch")
	b.ReportMetric(float64(s.Busy-s0.Busy)/batches/1e6, "ms/batch")
	if w := written(); w >= 0 && w0 >= 0 {
		b.ReportMetric(float64(w-w0)/1024/float64(b.N), "KiB-written/move")
	}
	if refused.Load() > 0 {
		b.Errorf("%d moves refused", refused.Load())
	}
}

// written is what the process has written to storage so far (Linux's
// /proc/self/io), or -1.
func written() int64 {
	io, err := os.ReadFile("/proc/self/io")
	if err != nil {
		return -1
	}
	for line := range strings.SplitSeq(string(io), "\n") {
		if v, ok := strings.CutPrefix(line, "write_bytes: "); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err == nil {
				return n
			}
		}
	}
	return -1
}
