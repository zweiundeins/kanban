package db

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zweiundeins/lith/writer"
)

func open(t *testing.T) *DB {
	t.Helper()
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), Options{Synchronous: "NORMAL"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func seedBoard(t *testing.T, d *DB) {
	t.Helper()
	err := d.W.Do(context.Background(), writer.Func(func(tx *writer.Tx) error {
		if _, err := tx.Exec("INSERT INTO projects (id, name, created_at) VALUES (1, 'p', 0)"); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO boards (id, project_id, name, position, created_at) VALUES (1, 1, 'b', 1, 0), (2, 1, 'c', 2, 0)")
		return err
	}))
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenMigrates(t *testing.T) {
	d := open(t)
	var version, tables int
	if err := d.R.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version < 3 {
		t.Fatalf("user_version %d, %v", version, err)
	}
	err := d.R.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name IN ('boards', 'tab_outbox', 'tab_state')").Scan(&tables)
	if err != nil || tables != 3 {
		t.Fatalf("%d of 3 tables, %v", tables, err)
	}
	if err := d.R.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name = 'ops'").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("ops is still there (%d), %v", tables, err)
	}
}

// TestReadStatements: reads outside a transaction share prepared statements.
func TestReadStatements(t *testing.T) {
	d := open(t)
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

func TestBackup(t *testing.T) {
	d := open(t)
	seedBoard(t, d)
	to := filepath.Join(t.TempDir(), "copy.db")
	if err := Backup(context.Background(), d.Path, to); err != nil {
		t.Fatal(err)
	}
	if err := Backup(context.Background(), d.Path, to); err == nil {
		t.Fatal("overwrote an existing backup")
	}
	c, err := Open(context.Background(), to, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var n int
	if err := c.R.QueryRow("SELECT count(*) FROM boards").Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d boards in the copy, %v", n, err)
	}
	if err := Backup(context.Background(), filepath.Join(t.TempDir(), "missing.db"), filepath.Join(t.TempDir(), "x.db")); err == nil {
		t.Fatal("backed up a database that doesn't exist")
	}
}
