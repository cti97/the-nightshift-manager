package board

import (
	"context"
	"database/sql"
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
		"index idx_items_parked",
		"index idx_runs_started_at",
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

	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	ctx := context.Background()

	// The fixture is a version-3 board, exactly as the v3 binary left it. The
	// injected list then stands in for the next migration's author: the
	// shipping migrations keep their versions and one new script appends as
	// the next version -- here a watches table, because whether migrate applies
	// only the newer entries does not depend on what that script creates.
	if err := migrate(ctx, raw, migrations[:3]); err != nil {
		t.Fatalf("migrate to version 3: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO nodes (id, name, token_hash, role, created_at)
		 VALUES ('n1','a','t','manager',0)`); err != nil {
		t.Fatalf("seed node: %v", err)
	}

	next := append(slices.Clone(migrations[:3]), migration{version: 4, script: `CREATE TABLE watches (
  id   TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE
);

CREATE INDEX idx_watches_name ON watches(name);`})

	if err := migrate(ctx, raw, next); err != nil {
		t.Fatalf("migrate v3 -> v4: %v", err)
	}

	var v int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != 4 {
		t.Fatalf("user_version = %d, want 4", v)
	}
	var nodes int
	if err := raw.QueryRow(`SELECT count(*) FROM nodes`).Scan(&nodes); err != nil {
		t.Fatalf("nodes: %v", err)
	}
	if nodes != 1 {
		t.Fatalf("nodes rows = %d, want 1 (migration 1 re-applied?)", nodes)
	}
	var watches int
	if err := raw.QueryRow(`SELECT count(*) FROM watches`).Scan(&watches); err != nil {
		t.Fatalf("watches: %v", err)
	}
}

func TestMigrateRejectsGappedVersions(t *testing.T) {
	raw, err := sql.Open("sqlite", dsn(filepath.Join(t.TempDir(), "board.db")))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()

	// A typo'd gap (1, 3) must fail before anything is applied to the db.
	err = migrate(context.Background(), raw, []migration{
		{version: 1, script: schema},
		{version: 3, script: "CREATE TABLE watches (id TEXT PRIMARY KEY);"},
	})
	if err == nil {
		t.Fatal("migrate with gapped versions returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "not contiguous") {
		t.Fatalf("err = %q, want a not-contiguous error", err)
	}
	var v int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != 0 {
		t.Fatalf("user_version = %d on a gapped list, want 0 (nothing applied)", v)
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

// TestOpenUpgradesAVersionOneDatabase is the migration that matters for an
// existing board: M1 and the first eleven M2 tasks shipped schema version 1, so
// a version-2 binary must add reserved_tokens in place without touching rows.
func TestOpenUpgradesAVersionOneDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")

	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := migrate(context.Background(), raw, []migration{{version: 1, script: schema}}); err != nil {
		t.Fatalf("migrate to version 1: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO nodes (id, name, token_hash, role, created_at)
		VALUES ('n1','a','t','manager',0)`); err != nil {
		t.Fatalf("seed a version-1 node: %v", err)
	}
	raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open a version-1 database: %v", err)
	}
	defer db.Close()

	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d after Open, want %d", v, schemaVersion)
	}
	var column int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('runs') WHERE name='reserved_tokens'`).Scan(&column); err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	if column != 1 {
		t.Fatal("runs has no reserved_tokens column after the upgrade")
	}
	var nodes int
	if err := db.QueryRow(`SELECT count(*) FROM nodes`).Scan(&nodes); err != nil {
		t.Fatalf("nodes: %v", err)
	}
	if nodes != 1 {
		t.Fatalf("nodes rows = %d after the upgrade, want the existing 1", nodes)
	}
}

// TestMigrationFourAddsTheApprovalColumn upgrades a database in the shape the
// shipping v3 binary writes -- rows included -- and checks the parked-item
// index the approvals page lists through. Migration 4 is additive: the rows
// survive unchanged and the new column starts NULL.
func TestMigrationFourAddsTheApprovalColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")

	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	ctx := context.Background()
	if err := migrate(ctx, raw, migrations[:3]); err != nil {
		t.Fatalf("migrate to version 3: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO work_items
		 (id,title,brief,status,created_by,created_at,updated_at)
		 VALUES ('w-queued','queued work','do it','queued','admin',100,100),
		        ('w-parked','parked work','do it','needs_approval','admin',200,200)`); err != nil {
		t.Fatalf("seed version-3 items: %v", err)
	}
	raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open a version-3 database: %v", err)
	}

	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	// Open applies the whole migration list, so it stamps the version that
	// list ends at. Assert against that, not a literal, so appending
	// migration 5 cannot break a test about migration 4's column.
	if v != schemaVersion {
		t.Fatalf("user_version = %d after Open, want %d (SchemaVersion)", v, schemaVersion)
	}

	var typ string
	var notnull int
	var dflt sql.NullString
	if err := db.QueryRow(`SELECT type, "notnull", dflt_value FROM pragma_table_info('work_items')
		 WHERE name = 'approval_request'`).Scan(&typ, &notnull, &dflt); err != nil {
		t.Fatalf("work_items has no approval_request column: %v", err)
	}
	if typ != "TEXT" {
		t.Fatalf("approval_request type = %q, want TEXT", typ)
	}
	if notnull != 0 || dflt.Valid {
		t.Fatalf("approval_request notnull=%d default=%v, want a nullable column with no default", notnull, dflt)
	}

	var partial int
	if err := db.QueryRow(`SELECT partial FROM pragma_index_list('work_items')
		 WHERE name = 'idx_items_parked'`).Scan(&partial); err != nil {
		t.Fatalf("work_items has no idx_items_parked index: %v", err)
	}
	if partial != 1 {
		t.Fatalf("idx_items_parked partial = %d, want 1 (a full index would pay for every item)", partial)
	}
	keys, err := db.Query(`SELECT name FROM pragma_index_info('idx_items_parked') ORDER BY seqno`)
	if err != nil {
		t.Fatalf("pragma_index_info: %v", err)
	}
	var keyCols []string
	for keys.Next() {
		var name string
		if err := keys.Scan(&name); err != nil {
			t.Fatalf("scan index key: %v", err)
		}
		keyCols = append(keyCols, name)
	}
	if err := keys.Err(); err != nil {
		t.Fatalf("index keys: %v", err)
	}
	keys.Close()
	if !slices.Equal(keyCols, []string{"status", "created_at"}) {
		t.Fatalf("idx_items_parked keys = %v, want [status created_at]", keyCols)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM work_items`).Scan(&count); err != nil {
		t.Fatalf("work_items count: %v", err)
	}
	if count != 2 {
		t.Fatalf("work_items rows = %d after the upgrade, want the existing 2", count)
	}
	var title, status string
	var granted, request sql.NullString
	if err := db.QueryRow(`SELECT title, status, approval_granted, approval_request
		 FROM work_items WHERE id = 'w-parked'`).Scan(&title, &status, &granted, &request); err != nil {
		t.Fatalf("read the parked row back: %v", err)
	}
	if title != "parked work" || status != "needs_approval" || granted.Valid || request.Valid {
		t.Fatalf("parked row after upgrade = (%q, %q, granted=%v, request=%v), want it untouched and the new column NULL",
			title, status, granted, request)
	}

	// Forward-only: a second Open applies nothing. Re-running migration 4 would
	// fail on the duplicate column instead.
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatalf("re-Open a version-4 database: %v", err)
	}
	defer db.Close()
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version after re-Open: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d after re-Open, want %d", v, schemaVersion)
	}
	if err := db.QueryRow(`SELECT count(*) FROM work_items`).Scan(&count); err != nil {
		t.Fatalf("work_items count after re-Open: %v", err)
	}
	if count != 2 {
		t.Fatalf("work_items rows = %d after re-Open, want 2", count)
	}
}

// TestTheParkedIndexServesTheApprovalsQuery pins the index to the query the
// approvals page is built on, rather than trusting that an index of that name
// is consulted. A plan that scans work_items, or that sorts the result, fails.
func TestTheParkedIndexServesTheApprovalsQuery(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	rows, err := db.Query(`EXPLAIN QUERY PLAN
		SELECT id, title, approval_request, created_at, attempt
		FROM work_items
		WHERE status = 'needs_approval'
		ORDER BY created_at`)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("query plan: %v", err)
	}

	joined := strings.Join(plan, " | ")
	if !strings.Contains(joined, "idx_items_parked") {
		t.Fatalf("the approvals query does not use idx_items_parked; plan = %s", joined)
	}
	if strings.Contains(joined, "TEMP B-TREE") {
		t.Fatalf("the approvals query sorts instead of reading the index order; plan = %s", joined)
	}
}

// TestAVersionFourDatabaseRefusesAnOlderBinary hands a real version-4 database
// to the migration list a version-3 binary ships. It must refuse, before it
// applies anything -- a v3 binary that "upgrades" a v4 board would skip the
// column its own readers assume.
func TestAVersionFourDatabaseRefusesAnOlderBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")

	// Build the version-4 fixture from migrations[:4], the way
	// TestMigrateAppliesOnlyNewerMigrations builds its v3 fixture. Open would
	// stamp whichever version this binary implements, so the day migration 5
	// appends, the "v4 database" handed to the older binary would silently be
	// a v5 one.
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()

	ctx := context.Background()
	if err := migrate(ctx, raw, migrations[:4]); err != nil {
		t.Fatalf("migrate to version 4: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO work_items
		 (id,title,brief,status,created_by,created_at,updated_at)
		 VALUES ('w1','parked','do it','needs_approval','admin',1,1)`); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	err = migrate(ctx, raw, migrations[:3])
	if err == nil {
		t.Fatal("a version-3 migration list accepted a version-4 database, want a refusal")
	}
	if !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("err = %q, want a newer-than-this-binary refusal", err)
	}
	if !strings.Contains(err.Error(), "v3") {
		t.Fatalf("err = %q, want it to name the version 3 the older binary implements", err)
	}

	var v int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	// Still literally 4: the fixture is a version-4 database, and the refusal
	// is what must have left it untouched.
	if v != 4 {
		t.Fatalf("user_version = %d after the refusal, want 4 (nothing applied)", v)
	}
}
