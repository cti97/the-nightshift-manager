package board

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

	_, err = Open(path)
	if err == nil {
		t.Fatal("Open on a newer schema returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "newer") {
		t.Fatalf("Open error = %q, want an error naming the schema as newer", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("Open error = %q, want it to name the database %q", err, path)
	}
}

func TestOpenPathWithSpecialCharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bo?rd #1&x.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	db.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	db.Close()
}

func TestMigrationCreatesExpectedObjects(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT name, type FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		got = append(got, typ+" "+name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	want := []string{
		"index idx_events_entity",
		"index idx_items_claimable",
		"table config",
		"table endpoints",
		"table events",
		"table models",
		"table nodes",
		"table runs",
		"table work_items",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("sqlite_master objects = %v, want %v", got, want)
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

func TestMigrateAppliesOnlyNewerMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO nodes (id, name, token_hash, role, created_at)
		 VALUES ('n1','a','t','manager',0)`); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	db.Close()

	orig := migrations
	defer func() { migrations = orig }()
	migrations = append(orig, migration{
		version: schemaVersion + 1,
		script: `CREATE TABLE watches (
  id   TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE
);

CREATE INDEX idx_watches_name ON watches(name);`,
	})

	db, err = Open(path)
	if err != nil {
		t.Fatalf("re-Open at v1 with a v2 migration: %v", err)
	}
	defer db.Close()

	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != schemaVersion+1 {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion+1)
	}
	var nodes int
	if err := db.QueryRow(`SELECT count(*) FROM nodes`).Scan(&nodes); err != nil {
		t.Fatalf("nodes: %v", err)
	}
	if nodes != 1 {
		t.Fatalf("nodes rows = %d, want 1 (migration 1 re-applied?)", nodes)
	}
	var watches int
	if err := db.QueryRow(`SELECT count(*) FROM watches`).Scan(&watches); err != nil {
		t.Fatalf("watches: %v", err)
	}
}

func TestSplitStatementsIsCommentAndEscapeAware(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "semicolon in comment does not split",
			in:   "-- no statement contains ';' here\nCREATE TABLE t (id INTEGER NOT NULL);",
			want: []string{"-- no statement contains ';' here\nCREATE TABLE t (id INTEGER NOT NULL)"},
		},
		{
			name: "apostrophe in comment does not open a literal",
			in:   "-- don't worry\nSELECT 1;",
			want: []string{"-- don't worry\nSELECT 1"},
		},
		{
			name: "semicolon after an escaped-quote literal splits once",
			in:   `CREATE TABLE t (v TEXT NOT NULL DEFAULT 'don''t');` + "\n" + `CREATE INDEX i ON t(v);`,
			want: []string{
				`CREATE TABLE t (v TEXT NOT NULL DEFAULT 'don''t')`,
				"CREATE INDEX i ON t(v)",
			},
		},
		{
			name: "blank padding between statements is dropped",
			in:   "CREATE TABLE t (id INTEGER NOT NULL);  \n\n   ;",
			want: []string{"CREATE TABLE t (id INTEGER NOT NULL)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := splitStatements(tt.in)
			if err != nil {
				t.Fatalf("splitStatements: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("statements = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSplitStatementsRejectsSemicolonInLiteral(t *testing.T) {
	_, err := splitStatements("INSERT INTO t VALUES ('a;b');")
	if err == nil {
		t.Fatal("splitStatements of a script with ';' in a literal returned nil, want an error")
	}
	if !errors.Is(err, errLiteralSemicolon) {
		t.Fatalf("err = %v, want errors.Is(err, errLiteralSemicolon)", err)
	}
}

func TestSplitStatementsRejectsUnterminatedLiteral(t *testing.T) {
	_, err := splitStatements("INSERT INTO t VALUES ('oops")
	if !errors.Is(err, errUnterminatedLiteral) {
		t.Fatalf("err = %v, want errors.Is(err, errUnterminatedLiteral)", err)
	}
}

func TestSplitStatementsSplitsMigrationOne(t *testing.T) {
	stmts, err := splitStatements(schema)
	if err != nil {
		t.Fatalf("splitStatements(schema): %v", err)
	}
	if len(stmts) != 9 {
		t.Fatalf("migration 1 statement count = %d, want 9", len(stmts))
	}
	if !strings.HasPrefix(stmts[0], "-- internal/board/schema.sql") {
		t.Fatalf("first statement lacks the schema header comment: %.40q", stmts[0])
	}
	last := stmts[len(stmts)-1]
	if last != "CREATE INDEX idx_events_entity ON events(entity, entity_id, id)" {
		t.Fatalf("last statement = %q", last)
	}
}
