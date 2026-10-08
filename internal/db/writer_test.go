package db

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func open(t *testing.T, opts Options) *DB {
	t.Helper()
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func seedBoard(t *testing.T, d *DB) {
	t.Helper()
	err := d.W.Do(context.Background(), func(tx *Tx) error {
		if _, err := tx.Exec("INSERT INTO projects (id, name, created_at) VALUES (1, 'p', 0)"); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO boards (id, project_id, name, position, created_at) VALUES (1, 1, 'b', 1, 0), (2, 1, 'c', 2, 0)")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, d *DB, q string) int {
	t.Helper()
	var n int
	if err := d.Read.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFailedCommandRollsBackAlone(t *testing.T) {
	d := open(t, Options{})
	seedBoard(t, d)
	ctx := context.Background()
	boom := errors.New("rejected")
	var wg sync.WaitGroup
	errs := make([]error, 3)
	// Hold the writer busy so the three land in one batch.
	block := make(chan struct{})
	go d.W.Do(ctx, func(*Tx) error { <-block; return nil })
	time.Sleep(20 * time.Millisecond)
	for i, cmd := range []Command{
		func(tx *Tx) error {
			_, err := tx.Exec("INSERT INTO projects (name, created_at) VALUES ('a', 0)")
			return err
		},
		func(tx *Tx) error {
			if _, err := tx.Exec("INSERT INTO projects (name, created_at) VALUES ('b', 0)"); err != nil {
				return err
			}
			return boom
		},
		func(tx *Tx) error { panic("broken command") },
	} {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = d.W.Do(ctx, cmd) }()
	}
	time.Sleep(20 * time.Millisecond)
	close(block)
	wg.Wait()
	if errs[0] != nil || !errors.Is(errs[1], boom) || errs[2] == nil {
		t.Fatalf("errors: %v", errs)
	}
	if n := count(t, d, "SELECT count(*) FROM projects WHERE name IN ('a', 'b')"); n != 1 {
		t.Fatalf("want only a's insert, got %d rows", n)
	}
	if s := d.W.Stats(); s.MaxBatch < 3 {
		t.Fatalf("want one batch of at least 3, stats %+v", s)
	}
}

func TestTouchAndOnCommit(t *testing.T) {
	var got [][]int64
	var mu sync.Mutex
	d := open(t, Options{OnCommit: func(b []int64) { mu.Lock(); got = append(got, b); mu.Unlock() }})
	seedBoard(t, d)
	ctx := context.Background()
	var v1, v2 int64
	err := d.W.Do(ctx, func(tx *Tx) error {
		var err error
		if v1, err = tx.Touch(2); err != nil {
			return err
		}
		v2, err = tx.Touch(2) // once per command
		return err
	})
	if err != nil || v1 != 1 || v2 != 1 {
		t.Fatalf("touch: %v %d %d", err, v1, v2)
	}
	// A rejected command publishes nothing and keeps the version.
	_ = d.W.Do(ctx, func(tx *Tx) error { tx.Touch(1); return errors.New("no") })
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0][0] != 2 {
		t.Fatalf("published %v", got)
	}
	if n := count(t, d, "SELECT version FROM boards WHERE id = 1"); n != 0 {
		t.Fatalf("rejected touch kept: version %d", n)
	}
}

func TestSavepointUndoesInnerOnly(t *testing.T) {
	d := open(t, Options{})
	seedBoard(t, d)
	err := d.W.Do(context.Background(), func(tx *Tx) error {
		if _, err := tx.Exec("INSERT INTO projects (name, created_at) VALUES ('outer', 0)"); err != nil {
			return err
		}
		inner := tx.Savepoint(func() error {
			tx.Touch(1)
			tx.Exec("INSERT INTO projects (name, created_at) VALUES ('inner', 0)")
			return errors.New("rejected")
		})
		if inner == nil {
			t.Error("inner error lost")
		}
		if len(tx.boards) != 0 {
			t.Error("rolled-back touch still recorded")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, "SELECT count(*) FROM projects WHERE name = 'inner'"); n != 0 {
		t.Fatal("inner insert kept")
	}
	if n := count(t, d, "SELECT count(*) FROM projects WHERE name = 'outer'"); n != 1 {
		t.Fatal("outer insert lost")
	}
}

// Many writers on one hot row: every increment lands, none is lost or
// refused, and the batches grow under the load.
func TestContention(t *testing.T) {
	d := open(t, Options{Synchronous: "NORMAL"})
	seedBoard(t, d)
	ctx := context.Background()
	const workers, each = 64, 50
	var failed atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				if err := d.W.Do(ctx, func(tx *Tx) error { _, err := tx.Touch(1); return err }); err != nil {
					failed.Add(1)
				}
			}
		}()
	}
	// Readers at the same time never see SQLITE_BUSY.
	stop := make(chan struct{})
	var readErr atomic.Value
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				var v int
				if err := d.Read.QueryRow("SELECT version FROM boards WHERE id = 1").Scan(&v); err != nil {
					readErr.Store(err)
				}
			}
		}
	}()
	wg.Wait()
	close(stop)
	if failed.Load() != 0 {
		t.Fatalf("%d commands failed", failed.Load())
	}
	if err, _ := readErr.Load().(error); err != nil {
		t.Fatalf("reader: %v", err)
	}
	if n := count(t, d, "SELECT version FROM boards WHERE id = 1"); n != workers*each {
		t.Fatalf("version %d, want %d", n, workers*each)
	}
	s := d.W.Stats()
	if s.Batches >= int64(workers*each) {
		t.Fatalf("no batching: %+v", s)
	}
	t.Logf("%d commands in %d batches (largest %d)", s.Commands, s.Batches, s.MaxBatch)
}

func TestCloseDrainsQueue(t *testing.T) {
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "x.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	seedBoard(t, d)
	var wg sync.WaitGroup
	var ok atomic.Int64
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d.W.Do(context.Background(), func(tx *Tx) error { _, err := tx.Touch(1); return err }) == nil {
				ok.Add(1)
			}
		}()
	}
	time.Sleep(5 * time.Millisecond)
	d.Close()
	wg.Wait()
	if err := d.W.Do(context.Background(), func(*Tx) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("after close: %v", err)
	}
	// Whatever was accepted was committed.
	d2, err := Open(context.Background(), d.Path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if n := count(t, d2, "SELECT version FROM boards WHERE id = 1"); int64(n) != ok.Load() {
		t.Fatalf("version %d, accepted %d", n, ok.Load())
	}
}

// TestStatementsPreparedOnce: running the same command again reuses its
// prepared statements; a statement with an error fails its command only.
func TestStatementsPreparedOnce(t *testing.T) {
	d := open(t, Options{})
	seedBoard(t, d)
	rename := func(name string) error {
		return d.W.Do(context.Background(), func(tx *Tx) error {
			_, err := tx.Exec("UPDATE boards SET name = ? WHERE id = ?", name, 1)
			return err
		})
	}
	if err := rename("x"); err != nil {
		t.Fatal(err)
	}
	var cached int
	_ = d.W.Do(context.Background(), func(*Tx) error { cached = len(d.W.stmts); return nil })
	for i := range 20 {
		if err := rename(string(rune('a' + i))); err != nil {
			t.Fatal(err)
		}
	}
	var after int
	_ = d.W.Do(context.Background(), func(*Tx) error { after = len(d.W.stmts); return nil })
	if after != cached {
		t.Fatalf("%d statements cached after one run, %d after 21", cached, after)
	}
	err := d.W.Do(context.Background(), func(tx *Tx) error {
		_, err := tx.Exec("UPDATE no_such_table SET x = 1")
		return err
	})
	if err == nil {
		t.Fatal("a statement on a missing table succeeded")
	}
	if err := rename("ok"); err != nil {
		t.Fatalf("the writer broke after a bad statement: %v", err)
	}
	if n := count(t, d, "SELECT count(*) FROM boards WHERE name = 'ok'"); n != 1 {
		t.Fatalf("rename after the bad statement: %d rows", n)
	}
}

// TestReadStatements: reads outside a transaction share prepared statements.
func TestReadStatements(t *testing.T) {
	d := open(t, Options{})
	seedBoard(t, d)
	for range 50 {
		var name string
		if err := d.QueryRow(context.Background(), "SELECT name FROM boards WHERE id = ?", 2).Scan(&name); err != nil || name != "c" {
			t.Fatalf("got %q, %v", name, err)
		}
	}
	if len(d.stmts) != 1 {
		t.Fatalf("%d read statements cached, want 1", len(d.stmts))
	}
}
