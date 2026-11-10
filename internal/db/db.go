// Package db opens kanban's database on lith: one write connection owned by
// lith's writer, which batches commands, and a pool of read-only
// connections for everything else.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"testing"

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
	// last commits on power loss, never on a crash of the process. OFF
	// syncs nothing, and Open takes it only under testing.Testing().
	Synchronous string
	// Readers is the size of the read pool (default lith's: GOMAXPROCS
	// + 4, at least 8, which leaves four or more beside the live hub's
	// render workers, one per CPU, each holding one).
	Readers int
	// OnCommit gets the topics of every committed batch (the live hub's
	// Changed).
	OnCommit func(topics []string)
	Log      *slog.Logger
	// Driver is the database/sql driver both pools open through (default
	// modernc's "sqlite"); serve names one that dbmetrics wraps.
	Driver string
}

// DB is the database: R for queries, W for every write. lith's read pool
// keeps each query's prepared statement on its connection, so a read on R,
// in a transaction or not, is parsed once per connection.
type DB struct {
	*sqlite.DB
	W *writer.Writer
}

// Open opens (and creates) the database at path, runs the migrations and
// starts the writer.
func Open(ctx context.Context, path string, opts Options) (*DB, error) {
	if opts.Synchronous == "" {
		opts.Synchronous = "FULL"
	}
	if strings.EqualFold(opts.Synchronous, "OFF") && !testing.Testing() {
		return nil, errors.New("synchronous OFF is for tests: a crash would corrupt the database")
	}
	sd, err := sqlite.Open(ctx, path, sqlite.Options{
		Synchronous: opts.Synchronous,
		Migrations:  migrations(),
		Readers:     opts.Readers,
		Checkpoint:  sqlite.CheckpointWriter,
		Driver:      opts.Driver,
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
	return errors.Join(d.W.Close(), d.DB.Close())
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
