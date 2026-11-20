// internal/board/watches_test.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestMigrationFiveAddsTheWatchesTable upgrades a real version-4 board and checks
// the table, the columns M4's decisions add, the due index the scheduler scans
// through, and the trigger vocabulary the schema enforces.
func TestMigrationFiveAddsTheWatchesTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	ctx := context.Background()

	// The fixture is a version-4 database, built from the list rather than Open
	// (M3's lesson): Open would stamp whatever this binary implements, and the
	// "v4 board" under test would silently be a v5 one.
	if err := migrate(ctx, raw, migrations[:4]); err != nil {
		t.Fatalf("migrate to version 4: %v", err)
	}
	if err := migrate(ctx, raw, migrations); err != nil {
		t.Fatalf("migrate to version 5: %v", err)
	}

	var v int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d after migrating, want %d (schemaVersion)", v, schemaVersion)
	}

	// Every column a fire, a skip or a condition edge writes. last_result and
	// the two skip columns are M4's additions to the spec's table; losing one is
	// losing D5 or D7, so the test names them.
	for _, want := range []struct {
		name    string
		notNull int
	}{
		{"id", 0}, {"name", 1}, {"trigger", 1}, {"schedule", 0}, {"probe", 0},
		{"template", 1}, {"dedupe_window", 1}, {"cooldown_until", 0}, {"enabled", 1},
		{"last_fired_at", 0}, {"next_fire_at", 0}, {"last_result", 0},
		{"last_skip_at", 0}, {"last_skip_reason", 0}, {"created_at", 1},
	} {
		var name, typ string
		var notNull, pk int
		var dflt sql.NullString
		err := raw.QueryRow(`SELECT name, type, "notnull", COALESCE(dflt_value,''), pk
		                       FROM pragma_table_info('watches') WHERE name = ?`, want.name).
			Scan(&name, &typ, &notNull, &dflt, &pk)
		if err != nil {
			t.Fatalf("watches.%s: %v", want.name, err)
		}
		if notNull != want.notNull {
			t.Fatalf("watches.%s notnull = %d, want %d", want.name, notNull, want.notNull)
		}
	}

	// The due scan is `enabled=1 AND next_fire_at <= ?`, and the index has to be
	// the partial one: a full index would carry every disabled watch forever.
	rows, err := raw.Query(`PRAGMA index_list('watches')`)
	if err != nil {
		t.Fatalf("index_list: %v", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatalf("scan index_list: %v", err)
		}
		if name == "idx_watches_due" {
			found = true
			if partial != 1 {
				t.Fatalf("idx_watches_due is not partial; disabled watches would occupy it forever")
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("index_list rows: %v", err)
	}
	if !found {
		t.Fatal("idx_watches_due is missing; the scheduler scan has no index")
	}

	// The trigger vocabulary is a database constraint, not a Go convention: a
	// fourth trigger must fail at INSERT, because a scheduler that meets one
	// would have to guess what to do with it.
	if _, err := raw.Exec(`INSERT INTO watches (id,name,trigger,schedule,template,created_at)
	                       VALUES ('w1','bad','hourly','x','{}',1)`); err == nil {
		t.Fatal("the schema accepted trigger 'hourly'; the vocabulary is enforced in Go only")
	}
}

// TestAVersionFiveDatabaseRefusesAnOlderBinary hands a real version-5 database to
// the migration list a version-4 binary ships. It must refuse before applying
// anything: a v4 binary that "upgraded" a v5 board would not know about watches
// and would answer "no watches" to every route that reads them.
func TestAVersionFiveDatabaseRefusesAnOlderBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	ctx := context.Background()

	if err := migrate(ctx, raw, migrations[:5]); err != nil {
		t.Fatalf("migrate to version 5: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO watches (id,name,trigger,schedule,template,created_at)
	                       VALUES ('w1','nightly','cron','30 2 * * *','{}',1)`); err != nil {
		t.Fatalf("seed watch: %v", err)
	}

	err = migrate(ctx, raw, migrations[:4])
	if err == nil {
		t.Fatal("a version-4 migration list accepted a version-5 database, want a refusal")
	}
	if !strings.Contains(err.Error(), "newer than this binary") || !strings.Contains(err.Error(), "v4") {
		t.Fatalf("err = %q, want a newer-than-this-binary refusal naming v4", err)
	}

	var v int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != 5 {
		t.Fatalf("user_version = %d after the refusal, want 5 (nothing applied)", v)
	}
}

// TestWatchTemplateRoundTrips stores a template and reads it back: the bytes the
// board holds must be the bytes it would write again, an empty need set must
// survive as [] rather than null, and the fields a fire needs must come back
// unchanged.
func TestWatchTemplateRoundTrips(t *testing.T) {
	db, _ := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))

	cases := []struct {
		name string
		tmpl WatchTemplate
	}{
		{"no needs", WatchTemplate{Title: "nightly", Brief: "check the thing"}},
		{"explicitly empty needs", WatchTemplate{Title: "nightly", Brief: "check the thing", Needs: []string{}}},
		{"two needs", WatchTemplate{
			Title: "nightly", Brief: "check the thing", Needs: []string{"agentic", "network"},
			Priority: 5, MaxAttempts: 2,
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.tmpl)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			id := "w" + string(rune('a'+i))
			if _, err := db.Exec(`INSERT INTO watches (id,name,trigger,schedule,template,created_at)
			                      VALUES (?,?,?,?,?,1)`, id, "name-"+id, "interval", "5m", string(encoded)); err != nil {
				t.Fatalf("store: %v", err)
			}
			var stored string
			if err := db.QueryRow(`SELECT template FROM watches WHERE id=?`, id).Scan(&stored); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if stored != string(encoded) {
				t.Fatalf("stored %s, want the encoded bytes %s", stored, encoded)
			}
			var got WatchTemplate
			if err := json.Unmarshal([]byte(stored), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Title != tc.tmpl.Title || got.Brief != tc.tmpl.Brief ||
				got.Priority != tc.tmpl.Priority || got.MaxAttempts != tc.tmpl.MaxAttempts {
				t.Fatalf("round-tripped %+v, want %+v", got, tc.tmpl)
			}
			if !slices.Equal(got.Needs, tc.tmpl.Needs) {
				t.Fatalf("needs = %v, want %v", got.Needs, tc.tmpl.Needs)
			}
			if len(tc.tmpl.Needs) == 0 {
				if got.Needs == nil {
					t.Fatal("empty needs read back as nil; a reader ranging them gets null")
				}
				if !strings.Contains(stored, `"needs":[]`) {
					t.Fatalf("empty needs stored as %s, want \"needs\":[]", stored)
				}
			}
		})
	}
}
