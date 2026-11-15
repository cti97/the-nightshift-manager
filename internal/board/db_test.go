package board

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func TestOpenAppliesMigrationAndPragmas(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	for i := 0; i < 2; i++ {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		db.Close()
	}
}

func TestOpenRefusesFutureSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion+1)); err != nil {
		t.Fatalf("bump user_version: %v", err)
	}
	db.Close()

	if _, err := Open(path); err == nil {
		t.Fatal("Open on a newer schema returned nil, want an error")
	}
}

func TestForeignKeysAreOn(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO runs (id, work_item_id, attempt, node_id, started_at, status, placement_reason)
		 VALUES ('r1','missing',1,'n1',1,'claimed','x')`); err == nil {
		t.Fatal("insert with a dangling FK succeeded, want a constraint error")
	}
}
