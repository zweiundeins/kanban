// Package db opens the SQLite database the way the whole app uses it: one
// write connection owned by a Writer, which batches commands, and a pool of
// read-only connections for everything else.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	_ "modernc.org/sqlite"
)

// Options tune the database. The zero value is what production runs.
type Options struct {
	// Synchronous is SQLite's synchronous pragma: FULL (default) is as
	// durable as PostgreSQL's defaults; NORMAL is faster and can lose the
	// last commits on power loss, never on a crash of the process.
	Synchronous string
	// Readers is the size of the read pool (default: one per CPU).
	Readers int
	// MaxBatch caps how many commands one write transaction takes (default 256).
	MaxBatch int
	// OnCommit is called after every commit with the boards it changed.
	OnCommit func(boards []int64)
}

// DB is the database: Read for queries, W for every write.
type DB struct {
	Path string
	Read *sql.DB
	W    *Writer
	wdb  *sql.DB

	stmtMu sync.Mutex
	stmts  map[string]*sql.Stmt // read statements, prepared once per pool connection
}

// QueryRow runs a read on the pool with a cached prepared statement. Not
// for use inside ReadTx: preparing takes a connection of its own, and a
// transaction holding one while the pool is exhausted would wait forever.
func (d *DB) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	if s := d.readStmt(ctx, query); s != nil {
		return s.QueryRowContext(ctx, args...)
	}
	return d.Read.QueryRowContext(ctx, query, args...)
}

// Query is QueryRow for many rows.
func (d *DB) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if s := d.readStmt(ctx, query); s != nil {
		return s.QueryContext(ctx, args...)
	}
	return d.Read.QueryContext(ctx, query, args...)
}

// readStmt prepares query on the pool once; database/sql then prepares it
// on each connection the first time it runs there and keeps it.
func (d *DB) readStmt(ctx context.Context, query string) *sql.Stmt {
	d.stmtMu.Lock()
	defer d.stmtMu.Unlock()
	if s := d.stmts[query]; s != nil {
		return s
	}
	if len(d.stmts) >= maxStmts {
		return nil
	}
	s, err := d.Read.PrepareContext(ctx, query)
	if err != nil {
		return nil
	}
	if d.stmts == nil {
		d.stmts = map[string]*sql.Stmt{}
	}
	d.stmts[query] = s
	return s
}

func dsn(path string, pragmas []string, extra url.Values) string {
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	return "file:" + path + "?" + q.Encode()
}

// Open opens (and creates) the database at path and runs the migrations.
func Open(ctx context.Context, path string, opts Options) (*DB, error) {
	if opts.Synchronous == "" {
		opts.Synchronous = "FULL"
	}
	if opts.Readers <= 0 {
		opts.Readers = runtime.NumCPU()
	}
	if opts.MaxBatch <= 0 {
		opts.MaxBatch = 256
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	common := []string{
		"busy_timeout(5000)",
		"foreign_keys(1)",
		"synchronous(" + opts.Synchronous + ")",
		"temp_store(MEMORY)",
		"cache_size(-32000)",
	}
	// The writer opens first, so the database is in WAL mode before any reader.
	wdb, err := sql.Open("sqlite", dsn(path, append([]string{"journal_mode(WAL)"}, common...), url.Values{"_txlock": {"immediate"}}))
	if err != nil {
		return nil, err
	}
	wdb.SetMaxOpenConns(1)
	wdb.SetMaxIdleConns(1)
	wdb.SetConnMaxLifetime(0)
	wdb.SetConnMaxIdleTime(0)
	if err := wdb.PingContext(ctx); err != nil {
		wdb.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	rdb, err := sql.Open("sqlite", dsn(path, append([]string{"query_only(1)"}, common...), nil))
	if err != nil {
		wdb.Close()
		return nil, err
	}
	rdb.SetMaxOpenConns(opts.Readers)
	rdb.SetMaxIdleConns(opts.Readers)
	rdb.SetConnMaxLifetime(0)
	rdb.SetConnMaxIdleTime(0)

	d := &DB{Path: path, Read: rdb, wdb: wdb}
	if err := migrate(ctx, wdb); err != nil {
		d.Read.Close()
		wdb.Close()
		return nil, err
	}
	if d.W, err = newWriter(wdb, opts.MaxBatch, opts.OnCommit); err != nil {
		d.Read.Close()
		wdb.Close()
		return nil, err
	}
	return d, nil
}

// Close drains the writer, then closes both pools.
func (d *DB) Close() error {
	d.W.Close()
	d.stmtMu.Lock()
	for _, s := range d.stmts {
		s.Close()
	}
	d.stmts = nil
	d.stmtMu.Unlock()
	// Fold the WAL back into the database file, so a copy of the file alone is complete.
	_, _ = d.wdb.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	err := d.Read.Close()
	if err2 := d.wdb.Close(); err == nil {
		err = err2
	}
	return err
}

// ReadTx runs fn in one read transaction: every query in it sees the same
// snapshot of the database.
func (d *DB) ReadTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(tx)
}

// Backup writes a consistent copy of the database to path (VACUUM INTO),
// while the server keeps running.
func Backup(ctx context.Context, path, to string) error {
	if _, err := os.Stat(to); err == nil {
		return fmt.Errorf("%s exists", to)
	}
	rdb, err := sql.Open("sqlite", dsn(path, []string{"busy_timeout(5000)"}, url.Values{"mode": {"ro"}}))
	if err != nil {
		return err
	}
	defer rdb.Close()
	_, err = rdb.ExecContext(ctx, "VACUUM INTO ?", to)
	return err
}
