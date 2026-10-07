package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// ErrClosed is returned for a command sent after Close.
var ErrClosed = errors.New("db: writer closed")

// A Command is one unit of work for the writer. It runs inside its own
// savepoint: returning an error (or panicking) undoes exactly its changes.
// A command must not block on anything but the database: every other
// command waits for it.
type Command func(*Tx) error

// Tx is what a Command gets: the batch's transaction, and the boards the
// command changed.
type Tx struct {
	tx     *sql.Tx
	ctx    context.Context
	boards map[int64]int64 // board id -> its version after this command
}

func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(t.ctx, query, args...)
}

func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(t.ctx, query, args...)
}

func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(t.ctx, query, args...)
}

// Touch records that the command changed a board: it bumps the board's
// version once per command and returns the new version. The writer
// publishes the board after the commit.
func (t *Tx) Touch(board int64) (int64, error) {
	if v, ok := t.boards[board]; ok {
		return v, nil
	}
	var v int64
	if err := t.QueryRow("UPDATE boards SET version = version + 1 WHERE id = ? RETURNING version", board).Scan(&v); err != nil {
		return 0, fmt.Errorf("touch board %d: %w", board, err)
	}
	t.boards[board] = v
	return v, nil
}

// Savepoint runs fn in a nested savepoint: an error from fn undoes fn's
// changes only, and is returned.
func (t *Tx) Savepoint(fn func() error) (err error) {
	if _, err := t.Exec("SAVEPOINT inner"); err != nil {
		return err
	}
	saved := make(map[int64]int64, len(t.boards))
	for k, v := range t.boards {
		saved[k] = v
	}
	if err = call(func() error { return fn() }); err != nil {
		if _, rbErr := t.Exec("ROLLBACK TO inner"); rbErr != nil {
			return errors.Join(err, rbErr)
		}
		t.boards = saved
	}
	if _, relErr := t.Exec("RELEASE inner"); relErr != nil {
		return errors.Join(err, relErr)
	}
	return err
}

type request struct {
	cmd  Command
	done chan error
	at   time.Time
}

// Stats are the writer's counters since it started.
type Stats struct {
	Commands    int64    `json:"commands"`
	Batches     int64    `json:"batches"`
	Failed      int64    `json:"failed"`
	MaxBatch    int64    `json:"max_batch"`
	QueueDepth  int      `json:"queue_depth"`
	CommitNanos int64    `json:"commit_nanos"` // total time spent in batches
	WaitNanos   int64    `json:"wait_nanos"`   // total time commands waited in the queue
	BatchSizes  [6]int64 `json:"batch_sizes"`  // 1, 2-4, 5-16, 17-64, 65-256, more
}

// Writer owns the only write connection. Commands queue up while a batch
// runs, and the next batch takes all of them (up to MaxBatch) into one
// transaction: under load the batch grows, and one commit (one fsync)
// covers many commands. Nobody waits for a batch to fill.
type Writer struct {
	db       *sql.DB
	queue    chan *request
	maxBatch int
	onCommit func([]int64)

	mu      sync.RWMutex
	closing bool
	done    chan struct{}

	commands, batches, failed, maxSeen, commitNanos, waitNanos atomic.Int64
	sizes                                                      [6]atomic.Int64
}

func newWriter(db *sql.DB, maxBatch int, onCommit func([]int64)) *Writer {
	w := &Writer{
		db:       db,
		queue:    make(chan *request, 4*maxBatch),
		maxBatch: maxBatch,
		onCommit: onCommit,
		done:     make(chan struct{}),
	}
	go w.run()
	return w
}

// SetOnCommit replaces the commit hook (wiring order: the hub needs the
// database, the database's writer needs the hub).
func (w *Writer) SetOnCommit(fn func([]int64)) {
	w.mu.Lock()
	w.onCommit = fn
	w.mu.Unlock()
}

// Do runs cmd in the next batch and returns its error once the batch is
// committed. A cancelled ctx stops waiting for a place in the queue, but
// a queued command always runs: Do then still reports its outcome, so a
// caller never mistakes a committed change for a failed one.
func (w *Writer) Do(ctx context.Context, cmd Command) error {
	req := &request{cmd: cmd, done: make(chan error, 1), at: time.Now()}
	w.mu.RLock()
	if w.closing {
		w.mu.RUnlock()
		return ErrClosed
	}
	select {
	case w.queue <- req:
	case <-ctx.Done():
		w.mu.RUnlock()
		return ctx.Err()
	}
	w.mu.RUnlock()
	return <-req.done
}

// Close stops taking commands, runs the ones already queued and returns
// when the last batch is committed.
func (w *Writer) Close() {
	w.mu.Lock()
	if !w.closing {
		w.closing = true
		close(w.queue)
	}
	w.mu.Unlock()
	<-w.done
}

// Stats reads the counters.
func (w *Writer) Stats() Stats {
	s := Stats{
		Commands:    w.commands.Load(),
		Batches:     w.batches.Load(),
		Failed:      w.failed.Load(),
		MaxBatch:    w.maxSeen.Load(),
		QueueDepth:  len(w.queue),
		CommitNanos: w.commitNanos.Load(),
		WaitNanos:   w.waitNanos.Load(),
	}
	for i := range w.sizes {
		s.BatchSizes[i] = w.sizes[i].Load()
	}
	return s
}

func (w *Writer) run() {
	defer close(w.done)
	batch := make([]*request, 0, w.maxBatch)
	for req := range w.queue {
		batch = append(batch[:0], req)
	fill:
		for len(batch) < w.maxBatch {
			select {
			case r, ok := <-w.queue:
				if !ok {
					break fill
				}
				batch = append(batch, r)
			default:
				break fill
			}
		}
		w.exec(batch)
		clear(batch) // drop references to finished commands
	}
}

func sizeBucket(n int) int {
	switch {
	case n == 1:
		return 0
	case n <= 4:
		return 1
	case n <= 16:
		return 2
	case n <= 64:
		return 3
	case n <= 256:
		return 4
	}
	return 5
}

func (w *Writer) exec(batch []*request) {
	start := time.Now()
	for _, r := range batch {
		w.waitNanos.Add(int64(start.Sub(r.at)))
	}
	errs := make([]error, len(batch))
	changed := map[int64]struct{}{}
	fail := func(err error) {
		for i := range errs {
			if errs[i] == nil {
				errs[i] = err
			}
		}
		clear(changed)
	}

	ctx := context.Background()
	tx, err := w.db.BeginTx(ctx, nil) // BEGIN IMMEDIATE (_txlock=immediate)
	if err != nil {
		fail(fmt.Errorf("db: begin: %w", err))
	} else {
		broken := false
		for i, r := range batch {
			if _, err := tx.ExecContext(ctx, "SAVEPOINT cmd"); err != nil {
				fail(fmt.Errorf("db: savepoint: %w", err))
				broken = true
				break
			}
			t := &Tx{tx: tx, ctx: ctx, boards: map[int64]int64{}}
			if err := call(func() error { return r.cmd(t) }); err != nil {
				errs[i] = err
				if _, err := tx.ExecContext(ctx, "ROLLBACK TO cmd"); err != nil {
					fail(fmt.Errorf("db: rollback to savepoint: %w", err))
					broken = true
					break
				}
			} else {
				for b := range t.boards {
					changed[b] = struct{}{}
				}
			}
			if _, err := tx.ExecContext(ctx, "RELEASE cmd"); err != nil {
				fail(fmt.Errorf("db: release savepoint: %w", err))
				broken = true
				break
			}
		}
		if broken {
			_ = tx.Rollback()
		} else if err := tx.Commit(); err != nil {
			_ = tx.Rollback()
			fail(fmt.Errorf("db: commit: %w", err))
		}
	}

	n := int64(len(batch))
	w.commands.Add(n)
	w.batches.Add(1)
	w.sizes[sizeBucket(len(batch))].Add(1)
	for {
		m := w.maxSeen.Load()
		if n <= m || w.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	for _, e := range errs {
		if e != nil {
			w.failed.Add(1)
		}
	}
	w.commitNanos.Add(int64(time.Since(start)))

	// Publish before answering: whoever acts on the answer finds the boards
	// already marked as changed.
	if len(changed) > 0 {
		w.mu.RLock()
		hook := w.onCommit
		w.mu.RUnlock()
		if hook != nil {
			boards := make([]int64, 0, len(changed))
			for b := range changed {
				boards = append(boards, b)
			}
			slices.Sort(boards)
			hook(boards)
		}
	}
	for i, r := range batch {
		r.done <- errs[i]
	}
}

// call runs fn and turns a panic into an error, so one broken command
// can't take the writer (and every queued command) down with it.
func call(fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("db: command panicked: %v\n%s", p, debug.Stack())
		}
	}()
	return fn()
}
