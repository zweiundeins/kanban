// Package db opens kanban's database on lith: one write connection owned by
// lith's writer, which batches commands, and a pool of read-only
// connections for everything else.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sync"

	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

//go:embed migrations/*.sql
var files embed.FS

func migrations() fs.FS {
	sub, err := fs.Sub(files, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

// Options tune the database. The zero value is what production runs.
type Options struct {
	// Synchronous is SQLite's synchronous pragma: FULL (default) is as
	// durable as PostgreSQL's defaults; NORMAL is faster and can lose the
	// last commits on power loss, never on a crash of the process.
	Synchronous string
	// Readers is the size of the read pool (default lith's: GOMAXPROCS
	// + 4, at least 8, which leaves four or more beside the live hub's
	// render workers, one per CPU, each holding one).
	Readers int
	// OnCommit gets the topics of every committed batch (the live hub's
	// Changed).
	OnCommit func(topics []string)
	Log      *slog.Logger
}

// DB is the database: R for queries, W for every write.
type DB struct {
	*sqlite.DB
	W *writer.Writer

	stmtMu sync.Mutex
	stmts  map[string]*sql.Stmt // read statements, prepared once per pool connection
}

// Open opens (and creates) the database at path, runs the migrations and
// starts the writer.
func Open(ctx context.Context, path string, opts Options) (*DB, error) {
	if opts.Synchronous == "" {
		opts.Synchronous = "FULL"
	}
	sd, err := sqlite.Open(ctx, path, sqlite.Options{
		Synchronous: opts.Synchronous,
		Migrations:  migrations(),
		Readers:     opts.Readers,
		Checkpoint:  sqlite.CheckpointWriter,
	})
	if err != nil {
		return nil, err
	}
	w, err := writer.New(sd, writer.Options{OnCommit: opts.OnCommit, Log: opts.Log})
	if err != nil {
		sd.Close()
		return nil, err
	}
	go w.Run(context.Background())
	return &DB{DB: sd, W: w}, nil
}

// Close drains the writer, then closes both pools.
func (d *DB) Close() error {
	err := d.W.Close()
	d.stmtMu.Lock()
	for _, s := range d.stmts {
		s.Close()
	}
	d.stmts = nil
	d.stmtMu.Unlock()
	return errors.Join(err, d.DB.Close())
}

// QueryRow runs a read on the pool with a cached prepared statement. Not
// for use inside a read transaction: preparing takes a connection of its
// own, and a transaction holding one while the pool is exhausted would
// wait forever.
func (d *DB) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	if s := d.readStmt(ctx, query); s != nil {
		return s.QueryRowContext(ctx, args...)
	}
	return d.R.QueryRowContext(ctx, query, args...)
}

// Query is QueryRow for many rows.
func (d *DB) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if s := d.readStmt(ctx, query); s != nil {
		return s.QueryContext(ctx, args...)
	}
	return d.R.QueryContext(ctx, query, args...)
}

// maxStmts bounds the read statement cache. The queries are constants, so
// it is never reached; past it, statements are prepared for each call again.
const maxStmts = 512

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
	s, err := d.R.PrepareContext(ctx, query)
	if err != nil {
		return nil
	}
	if d.stmts == nil {
		d.stmts = map[string]*sql.Stmt{}
	}
	d.stmts[query] = s
	return s
}

// Backup writes a consistent copy of the database at path to the file to
// (VACUUM INTO), while the server keeps running.
func Backup(ctx context.Context, path, to string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	sd, err := sqlite.Open(ctx, path, sqlite.Options{Synchronous: "FULL", Readers: 1})
	if err != nil {
		return err
	}
	defer sd.Close()
	return sd.Backup(ctx, to)
}
