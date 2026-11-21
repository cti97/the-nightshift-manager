// internal/board/threads_test.go
package board

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrationSixAddsThreadsAndCheckpoints upgrades a real version-5 board and checks the
// three tables, the columns M5's decisions add, the constraints the schema states for
// itself, and the two indexes the thread view and the checkpoint sequence read through.
func TestMigrationSixAddsThreadsAndCheckpoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	ctx := context.Background()

	// The fixture is a version-5 database built from the list: Open would stamp the
	// binary's version and the "v5 board" under test would silently be a v6 one.
	if err := migrate(ctx, raw, migrations[:5]); err != nil {
		t.Fatalf("migrate to version 5: %v", err)
	}
	if err := migrate(ctx, raw, migrations); err != nil {
		t.Fatalf("migrate to version 6: %v", err)
	}
	var v int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d after migrating, want %d", v, schemaVersion)
	}

	for _, want := range []struct {
		table   string
		columns []string
	}{
		{"threads", []string{"id", "work_item_id", "subject", "budget_tokens",
			"max_output_tokens", "tokens_reserved", "tokens_used", "max_turns",
			"turns_used", "turn_owner", "status", "created_at", "closed_at"}},
		{"messages", []string{"id", "thread_id", "author", "body", "tokens_charged", "created_at"}},
		{"checkpoints", []string{"id", "run_id", "seq", "declared_progress", "state", "artifacts", "created_at"}},
	} {
		for _, col := range want.columns {
			var name string
			if err := raw.QueryRow(`SELECT name FROM pragma_table_info(?) WHERE name=?`, want.table, col).Scan(&name); err != nil {
				t.Fatalf("%s.%s is missing: %v", want.table, col, err)
			}
		}
	}

	// The vocabulary is the schema's, not only Go's: a status outside it cannot be stored.
	now := int64(1_700_000_000)
	if _, err := raw.Exec(`INSERT INTO threads (id,subject,budget_tokens,status,created_at)
	                       VALUES ('t1','x',100,'open',?)`, now); err != nil {
		t.Fatalf("insert open thread: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO threads (id,subject,budget_tokens,status,created_at)
	                       VALUES ('t2','x',100,'abandoned',?)`, now); err == nil {
		t.Fatal("a status outside the vocabulary was accepted")
	}
	if _, err := raw.Exec(`INSERT INTO threads (id,subject,budget_tokens,status,created_at)
	                       VALUES ('t3','x',0,'open',?)`, now); err == nil {
		t.Fatal("a zero budget was accepted; the reservation could never fit")
	}

	// seq is unique per run: a retried checkpoint is a refusal, not a duplicate marker.
	// Foreign keys are off for this section only: checkpoints.run_id references a real run,
	// and the FK has its own test (TestForeignKeysAreOn). Building a node, endpoint, item
	// and run here would test the fixture rather than the index.
	if _, err := raw.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO checkpoints (id,run_id,seq,declared_progress,created_at)
	                       VALUES ('c1','r1',1,'half',?)`, now); err != nil {
		t.Fatalf("insert checkpoint: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO checkpoints (id,run_id,seq,declared_progress,created_at)
	                       VALUES ('c2','r1',1,'half again',?)`, now); err == nil {
		t.Fatal("a duplicate (run_id, seq) was accepted")
	}
	if _, err := raw.Exec(`INSERT INTO checkpoints (id,run_id,seq,declared_progress,created_at)
	                       VALUES ('c3','r2',1,'other run',?)`, now); err != nil {
		t.Fatalf("seq 1 on a second run was refused: %v", err)
	}
}

// TestAVersionSixDatabaseRefusesAnOlderBinary hands a real version-6 database to the
// migration list a version-5 binary ships. It must refuse before applying anything: a v5
// binary that "upgraded" a v6 board would not know about threads and would answer "no
// messages" to every route that reads them.
func TestAVersionSixDatabaseRefusesAnOlderBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	ctx := context.Background()

	if err := migrate(ctx, raw, migrations); err != nil {
		t.Fatalf("migrate to version 6: %v", err)
	}
	err = migrate(ctx, raw, migrations[:5])
	if err == nil {
		t.Fatal("a version-5 migration list accepted a version-6 database, want a refusal")
	}
	if !strings.Contains(err.Error(), "newer than this binary") || !strings.Contains(err.Error(), "v5") {
		t.Fatalf("err = %q, want a newer-than-this-binary refusal naming v5", err)
	}
}
