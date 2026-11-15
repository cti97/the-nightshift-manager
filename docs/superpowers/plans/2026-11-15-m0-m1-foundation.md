# The Night Shift Manager — M0/M1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the manager's board and the claim loop — a system where a node can register, be given work it is qualified for, hold a lease, and report a result that provably completes exactly once.

**Architecture:** One Go binary (`nsm`) with subcommands. The manager is an `net/http` server over SQLite (`modernc.org/sqlite`, pure Go, so `CGO_ENABLED=0` holds across the six-target matrix). Every layer is testable without a network: placement is a pure function, the board is a transaction, and the agent is exercised end-to-end against an `httptest` server with a stub executor.

**Tech Stack:** Go 1.24+, `modernc.org/sqlite`, `net/http` (std `ServeMux` with method patterns), `testing` from the standard library. No web framework, no assertion library, no ORM.

**Spec:** `docs/superpowers/specs/2026-11-15-nightshift-manager-design.md`

**Scope:** M0 (board foundation) and M1 (claim loop). M2–M6 get their own plans, written at the close of the preceding milestone — their interfaces depend on code that does not exist yet, and a plan that predicts M4's scheduler against M1's board would be fiction. The successor task list is at the end of this document so the shape stays visible.

## Global Constraints

Every task's requirements implicitly include these.

- **Module path:** `github.com/cti97/the-nightshift-manager`.
- **Go:** floor `1.24`. Toolchain in use is 1.27.1.
- **`CGO_ENABLED=0` always.** SQLite is `modernc.org/sqlite` and nothing else. Adding `mattn/go-sqlite3` breaks the release matrix, which is the whole reason the matrix is affordable.
- **One binary:** `nsm`, with subcommands `manager`, `agent`, `version`. No second executable in M0/M1.
- **Release targets, exactly:** `darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64`.
- **No prompt content ever transits the board.** Columns hold titles, briefs, result summaries, and paths — never a transcript.
- **All timestamps are `int64` Unix seconds, generated server-side through `board.Clock`.** `time.Now()` appears in exactly one file (`internal/board/clock.go`). A node's clock is never trusted.
- **One writer.** The DB pool is `SetMaxOpenConns(1)`. Claim is `BEGIN IMMEDIATE` on a single `*sql.Conn`. Do not raise this without redesigning the claim path.
- **Schema change = new migration.** `schemaVersion` increments; a DB whose `user_version` exceeds the binary's is a hard error, not a best-effort start.
- **Handlers never panic.** Errors are JSON `{"error":{"code":"...","message":"..."}}` with an appropriate status.
- **Tests are stdlib `testing` only** — no testify, no gomock. Fakes are hand-written structs.
- **Never log a token.** Registration returns the plaintext token once; only its SHA-256 is stored.

---

## File map

| File | Responsibility |
|---|---|
| `go.mod` | Module, Go floor, single dependency |
| `internal/version/version.go` | Version/commit, VCS-stamp fallback |
| `internal/caps/caps.go` | Closed vocabulary, parse, validate, subset match |
| `internal/board/schema.sql` | Migration 1 — embedded, applied verbatim |
| `internal/board/db.go` | Open, pragmas, `user_version` migration gate |
| `internal/board/clock.go` | The only place `time.Now()` is called |
| `internal/board/events.go` | Append + list, the audit spine |
| `internal/board/nodes.go` | Register, token, heartbeat, lease extension |
| `internal/board/items.go` | Create/get work items, status transitions |
| `internal/board/placement.go` | Pure eligibility + ranking + reason |
| `internal/board/claim.go` | The `BEGIN IMMEDIATE` claim transaction |
| `internal/board/reaper.go` | Lease expiry, `abandoned` vs `lost` |
| `internal/board/report.go` | Idempotent report, run start, completion |
| `internal/board/why.go` | Placement explanation for one item |
| `internal/board/http.go` | Routes, bearer auth, JSON helpers |
| `internal/agent/client.go` | Typed client for the board API |
| `internal/agent/agent.go` | Heartbeat/claim/report loop |
| `internal/agent/executor.go` | `Executor` interface, `Result` |
| `internal/agent/stub.go` | Deterministic executor for tests |
| `cmd/nsm/main.go` | Subcommand dispatch |
| `scripts/build-release.sh` | Six-target matrix, `dist/`, `SHA256SUMS` |
| `Makefile` | `build`, `test`, `vet`, `release` |

## Task index

| # | Milestone | Deliverable |
|---|---|---|
| 1 | M0 | Module, `nsm version`, subcommand dispatch |
| 2 | M0 | SQLite open, pragmas, migration gate |
| 3 | M0 | Clock injection + `events` append/list |
| 4 | M0 | `caps` — vocabulary, parse, subset match |
| 5 | M0 | `placement` — eligibility, ranking, reason |
| 6 | M0 | HTTP server, bearer auth, `/version`, `/board` |
| 7 | M1 | Register, token issue, heartbeat, lease extension |
| 8 | M1 | Claim transaction — exactly once |
| 9 | M1 | Reaper — `abandoned` vs `lost` |
| 10 | M1 | Start, idempotent report, `/why` |
| 11 | M1 | Agent loop + stub executor + end-to-end test |
| 12 | M1 | `make release` — six artifacts, checksums |

---

## Task 1: Module, version, and subcommand dispatch

**Files:**
- Create: `go.mod`, `internal/version/version.go`, `internal/version/version_test.go`, `cmd/nsm/main.go`, `cmd/nsm/main_test.go`

**Interfaces:**
- Produces: `version.Info() (version, commit string)`, `version.String() string`, `main.run(args []string, out, errOut io.Writer) int`

- [ ] **Step 1: Create the module**

```bash
cd the-nightshift-manager
go mod init github.com/cti97/the-nightshift-manager
```

Edit `go.mod` so the `go` line reads `go 1.24`.

- [ ] **Step 2: Write the failing version test**

```go
// internal/version/version_test.go
package version

import "testing"

func TestStringIncludesVersionAndCommit(t *testing.T) {
	Version, Commit = "v1.2.3", "abc1234"
	defer func() { Version, Commit = "dev", "unknown" }()

	if got, want := String(), "v1.2.3 (abc1234)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestStringOmitsUnknownCommit(t *testing.T) {
	Version, Commit = "v1.2.3", "unknown"
	defer func() { Version, Commit = "dev", "unknown" }()

	if got, want := String(), "v1.2.3"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
```

- [ ] **Step 3: Run it and watch it fail**

Run: `go test ./internal/version/ -v`
Expected: FAIL — `undefined: String`.

- [ ] **Step 4: Implement version**

```go
// internal/version/version.go
package version

import "runtime/debug"

// Overridden at build time:
//
//	go build -ldflags "-X github.com/cti97/the-nightshift-manager/internal/version.Version=v0.1.0"
var (
	Version = "dev"
	Commit  = "unknown"
)

// Info returns the version and the commit. When ldflags did not set a commit,
// the toolchain's own VCS stamp is used, so a local build can still name the
// commit it came from (§20 of the spec).
func Info() (string, string) {
	c := Commit
	if c == "unknown" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" && s.Value != "" {
					c = s.Value
					if len(c) > 12 {
						c = c[:12]
					}
				}
			}
		}
	}
	return Version, c
}

func String() string {
	v, c := Info()
	if c == "unknown" {
		return v
	}
	return v + " (" + c + ")"
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/version/ -v`
Expected: PASS (2 tests).

- [ ] **Step 6: Write the failing dispatch test**

```go
// cmd/nsm/main_test.go
package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunVersionExitsZero(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "dev") {
		t.Fatalf("stdout = %q, want it to contain the version", out.String())
	}
}

func TestRunUnknownCommandExitsTwo(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"nope"}, &out, &errOut); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "usage") {
		t.Fatalf("stderr = %q, want usage text", errOut.String())
	}
}
```

- [ ] **Step 7: Run it and watch it fail**

Run: `go test ./cmd/nsm/ -v`
Expected: FAIL — `undefined: run`.

- [ ] **Step 8: Implement dispatch**

```go
// cmd/nsm/main.go
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/cti97/the-nightshift-manager/internal/version"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		usage(errOut)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(out, version.String())
		return 0
	case "manager", "agent":
		fmt.Fprintf(errOut, "nsm %s: not implemented yet\n", args[0])
		return 2
	default:
		usage(errOut)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: nsm <version|manager|agent> [args]")
}
```

- [ ] **Step 9: Run all tests, vet, commit**

```bash
go test ./...
go vet ./...
git add go.mod internal/version cmd/nsm
git commit -m "feat: nsm skeleton with version and subcommand dispatch"
```

---

## Task 2: SQLite open, pragmas, and the migration gate

**Files:**
- Create: `internal/board/schema.sql`, `internal/board/db.go`, `internal/board/db_test.go`

**Interfaces:**
- Produces: `board.Open(path string) (*sql.DB, error)`, `board.SchemaVersion() int`, and package-level `const schemaVersion = 1`

- [ ] **Step 1: Add the dependency**

```bash
go get modernc.org/sqlite
```

- [ ] **Step 2: Write migration 1**

```sql
-- internal/board/schema.sql
-- Migration 1. Applied verbatim when PRAGMA user_version is 0.
-- Statements are separated by ';' at end of line; no statement contains ';'.

CREATE TABLE nodes (
  id                TEXT PRIMARY KEY,
  name              TEXT NOT NULL UNIQUE,
  token_hash        TEXT NOT NULL,
  role              TEXT NOT NULL CHECK (role IN ('manager','executor','steward')),
  provides_declared TEXT NOT NULL DEFAULT '[]',
  provides_live     TEXT NOT NULL DEFAULT '[]',
  reach_classes     TEXT NOT NULL DEFAULT '[]',
  concurrency       INTEGER NOT NULL DEFAULT 1,
  agent_version     TEXT NOT NULL DEFAULT '',
  protocol_version  INTEGER NOT NULL DEFAULT 0,
  last_seen_at      INTEGER,
  status            TEXT NOT NULL DEFAULT 'offline'
                    CHECK (status IN ('online','offline','paused','incompatible')),
  created_at        INTEGER NOT NULL
);

CREATE TABLE endpoints (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  provider      TEXT NOT NULL,
  base_url      TEXT NOT NULL,
  auth_ref      TEXT,
  reach         TEXT NOT NULL CHECK (reach IN ('host','lan','wan')),
  health        TEXT NOT NULL DEFAULT 'unknown'
                CHECK (health IN ('ok','warming','down','auth_failed','unknown')),
  health_reason TEXT,
  last_ok_at    INTEGER,
  latency_ms    INTEGER,
  created_at    INTEGER NOT NULL
);

CREATE TABLE models (
  id                TEXT PRIMARY KEY,
  endpoint_id       TEXT NOT NULL REFERENCES endpoints(id),
  model_id          TEXT NOT NULL,
  provides          TEXT NOT NULL DEFAULT '[]',
  features          TEXT NOT NULL DEFAULT '[]',
  context_window    INTEGER NOT NULL DEFAULT 0,
  cost_in_per_mtok  REAL NOT NULL DEFAULT 0,
  cost_out_per_mtok REAL NOT NULL DEFAULT 0,
  enabled           INTEGER NOT NULL DEFAULT 1,
  UNIQUE (endpoint_id, model_id)
);

CREATE TABLE work_items (
  id               TEXT PRIMARY KEY,
  parent_id        TEXT REFERENCES work_items(id),
  title            TEXT NOT NULL,
  brief            TEXT NOT NULL,
  needs            TEXT NOT NULL DEFAULT '[]',
  status           TEXT NOT NULL DEFAULT 'queued'
                   CHECK (status IN ('queued','claimed','running','needs_approval','done','failed','cancelled')),
  priority         INTEGER NOT NULL DEFAULT 0,
  deadline_at      INTEGER,
  claimed_by       TEXT REFERENCES nodes(id),
  lease_expires_at INTEGER,
  attempt          INTEGER NOT NULL DEFAULT 0,
  max_attempts     INTEGER NOT NULL DEFAULT 3,
  approval_granted TEXT,
  result_summary   TEXT,
  artifact_path    TEXT,
  blocked_reason   TEXT,
  created_by       TEXT NOT NULL,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);

CREATE INDEX idx_items_claimable ON work_items(status, priority DESC, created_at);

CREATE TABLE runs (
  id               TEXT PRIMARY KEY,
  work_item_id     TEXT NOT NULL REFERENCES work_items(id),
  attempt          INTEGER NOT NULL,
  node_id          TEXT NOT NULL,
  endpoint_id      TEXT REFERENCES endpoints(id),
  model_id         TEXT REFERENCES models(id),
  started_at       INTEGER NOT NULL,
  ended_at         INTEGER,
  status           TEXT NOT NULL
                   CHECK (status IN ('claimed','running','ok','error','lost','abandoned','escalated')),
  input_tokens     INTEGER NOT NULL DEFAULT 0,
  output_tokens    INTEGER NOT NULL DEFAULT 0,
  cost_estimate    REAL NOT NULL DEFAULT 0,
  placement_reason TEXT NOT NULL,
  transcript_path  TEXT,
  error            TEXT,
  UNIQUE (work_item_id, attempt)
);

CREATE TABLE config (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE events (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  at        INTEGER NOT NULL,
  actor     TEXT NOT NULL,
  entity    TEXT NOT NULL,
  entity_id TEXT NOT NULL,
  type      TEXT NOT NULL,
  payload   TEXT
);

CREATE INDEX idx_events_entity ON events(entity, entity_id, id);
```

- [ ] **Step 3: Write the failing DB test**

```go
// internal/board/db_test.go
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
```

- [ ] **Step 4: Run it and watch it fail**

Run: `go test ./internal/board/ -v`
Expected: FAIL — `undefined: Open`.

- [ ] **Step 5: Implement open and migrate**

```go
// internal/board/db.go
package board

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

const schemaVersion = 1

// SchemaVersion is exported for tests and for the migrate command.
func SchemaVersion() int { return schemaVersion }

// Open opens (creating if needed) the board database, applies pending
// migrations, and returns a pool capped at ONE connection.
//
// The single connection is deliberate: it serialises writers so the claim
// transaction (§8 of the spec) needs no clever locking, and it makes
// "who changed this row" answerable. Raise it only with a claim redesign.
func Open(path string) (*sql.DB, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	switch {
	case v == schemaVersion:
		return nil
	case v > schemaVersion:
		return fmt.Errorf("database schema v%d is newer than this binary (v%d)", v, schemaVersion)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, stmt := range strings.Split(schema, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration 1: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/board/ -v`
Expected: PASS (4 tests).

- [ ] **Step 7: Commit**

```bash
git add internal/board go.mod go.sum
git commit -m "feat(board): sqlite open, pragmas, migration gate"
```

---

## Task 3: Clock injection and the events spine

**Files:**
- Create: `internal/board/clock.go`, `internal/board/events.go`, `internal/board/events_test.go`

**Interfaces:**
- Produces:
  - `type Clock interface { Now() int64 }`
  - `board.NewFixedClock(t int64) *FixedClock`, `(*FixedClock).Advance(d time.Duration)`, `(*FixedClock).Set(t int64)`
  - `type Execer interface { ExecContext(context.Context, string, ...any) (sql.Result, error) }`
  - `board.AppendEvent(ctx, q Execer, clock Clock, actor, entity, entityID, typ string, payload any) error`
  - `type Event struct { ID, At int64; Actor, Entity, EntityID, Type, Payload string }`
  - `board.ListEvents(ctx, db *sql.DB, entity, entityID string, limit int) ([]Event, error)`

- [ ] **Step 1: Write the failing events test**

```go
// internal/board/events_test.go
package board

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestDB(t *testing.T) (*sql.DB, *FixedClock) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, NewFixedClock(1_700_000_000)
}

func TestAppendEventStoresPayloadAndTime(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()

	if err := AppendEvent(ctx, db, clock, "admin", "work_item", "w1", "created",
		map[string]any{"title": "nightly report"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	events, err := ListEvents(ctx, db, "work_item", "w1", 10)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].At != clock.Now() {
		t.Fatalf("At = %d, want %d", events[0].At, clock.Now())
	}
	if events[0].Payload != `{"title":"nightly report"}` {
		t.Fatalf("Payload = %q", events[0].Payload)
	}
}

func TestAdvanceMovesTheClock(t *testing.T) {
	clock := NewFixedClock(100)
	clock.Advance(30 * time.Second)
	if clock.Now() != 130 {
		t.Fatalf("Now() = %d, want 130", clock.Now())
	}
}
```

Add `"database/sql"` to the imports.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/board/ -run Event -v`
Expected: FAIL — `undefined: NewFixedClock`.

- [ ] **Step 3: Implement clock, events, and the shared test fixture**

```go
// internal/board/clock.go
package board

import "time"

// Clock is the only source of time in the system. Every timestamp and lease
// is computed from it, server-side; a node's clock is never consulted.
type Clock interface{ Now() int64 }

type realClock struct{}

func (realClock) Now() int64 { return time.Now().Unix() }

// SystemClock is what production uses.
func SystemClock() Clock { return realClock{} }

// FixedClock is a controllable clock for tests.
type FixedClock struct{ t int64 }

func NewFixedClock(t int64) *FixedClock { return &FixedClock{t: t} }

func (f *FixedClock) Now() int64              { return f.t }
func (f *FixedClock) Advance(d time.Duration) { f.t += int64(d.Seconds()) }
func (f *FixedClock) Set(t int64)             { f.t = t }
```

```go
// internal/board/testing.go
package board

import (
	"database/sql"
	"testing"
)

// OpenForTest opens a board on a fixed clock. It lives in a non-_test file so
// the agent package can build the same fixture without duplicating schema
// knowledge. It is the only exported test helper in this package.
func OpenForTest(t *testing.T, path string) (*sql.DB, *FixedClock) {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open board: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, NewFixedClock(1_700_000_000)
}
```

And in `events_test.go`, replace the hand-rolled helper:

```go
func newTestDB(t *testing.T) (*sql.DB, *FixedClock) {
	t.Helper()
	return OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
}
```

```go
// internal/board/events.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Execer is satisfied by both *sql.DB and *sql.Tx, so an event can be written
// inside the same transaction as the change it describes.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type Event struct {
	ID       int64  `json:"id"`
	At       int64  `json:"at"`
	Actor    string `json:"actor"`
	Entity   string `json:"entity"`
	EntityID string `json:"entity_id"`
	Type     string `json:"type"`
	Payload  string `json:"payload,omitempty"`
}

// AppendEvent writes one row to the append-only audit spine. The dashboard
// projects state from these rows (spec invariant 2).
func AppendEvent(ctx context.Context, q Execer, clock Clock, actor, entity, entityID, typ string, payload any) error {
	var raw sql.NullString
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal event payload: %w", err)
		}
		raw = sql.NullString{String: string(b), Valid: true}
	}
	_, err := q.ExecContext(ctx,
		`INSERT INTO events (at, actor, entity, entity_id, type, payload) VALUES (?,?,?,?,?,?)`,
		clock.Now(), actor, entity, entityID, typ, raw)
	return err
}

// ListEvents returns events for one entity in append order.
func ListEvents(ctx context.Context, db *sql.DB, entity, entityID string, limit int) ([]Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, at, actor, entity, entity_id, type, COALESCE(payload,'')
		   FROM events WHERE entity = ? AND entity_id = ? ORDER BY id LIMIT ?`,
		entity, entityID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Entity, &e.EntityID, &e.Type, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/board/ -run 'Event|Clock' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/board/clock.go internal/board/events.go internal/board/events_test.go
git commit -m "feat(board): clock injection and the events spine"
```

---

## Task 4: `caps` — vocabulary, parse, subset match

**Files:**
- Create: `internal/caps/caps.go`, `internal/caps/caps_test.go`

**Interfaces:**
- Produces:
  - `caps.Parse(raw []string) ([]string, error)` — canonical, sorted, de-duplicated
  - `caps.Satisfies(needs, provides []string) bool`
  - `caps.Missing(needs, provides []string) []string`

- [ ] **Step 1: Write the failing table test**

```go
// internal/caps/caps_test.go
package caps

import (
	"reflect"
	"testing"
)

func TestParseRejectsUnknownCapability(t *testing.T) {
	if _, err := Parse([]string{"gpuu"}); err == nil {
		t.Fatal("Parse(unknown) returned nil error")
	}
}

func TestParseCanonicalises(t *testing.T) {
	got, err := Parse([]string{"shell", "gpu", "shell"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := []string{"gpu", "shell"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse = %v, want %v", got, want)
	}
}

func TestParseRejectsMalformedParameters(t *testing.T) {
	for _, bad := range []string{"local_fs:", "local_fs:relative/path", "tokens:0", "tokens:abc", "lan:"} {
		if _, err := Parse([]string{bad}); err == nil {
			t.Fatalf("Parse(%q) returned nil error", bad)
		}
	}
}

func TestSatisfies(t *testing.T) {
	cases := []struct {
		name     string
		needs    []string
		provides []string
		want     bool
	}{
		{"empty needs", nil, []string{"shell"}, true},
		{"exact", []string{"shell"}, []string{"shell", "gpu"}, true},
		{"absent", []string{"shell"}, []string{"gpu"}, false},
		{"local_fs prefix", []string{"local_fs:/a/b"}, []string{"local_fs:/a"}, true},
		{"local_fs too broad", []string{"local_fs:/a"}, []string{"local_fs:/a/b"}, false},
		{"local_fs must respect path boundary", []string{"local_fs:/ab"}, []string{"local_fs:/a"}, false},
		{"tokens sufficient", []string{"tokens:120000"}, []string{"tokens:200000"}, true},
		{"tokens insufficient", []string{"tokens:200000"}, []string{"tokens:120000"}, false},
		{"lan service", []string{"lan:nas"}, []string{"lan:nas"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Satisfies(tc.needs, tc.provides); got != tc.want {
				t.Fatalf("Satisfies(%v, %v) = %v, want %v", tc.needs, tc.provides, got, tc.want)
			}
		})
	}
}

func TestMissingNamesTheGap(t *testing.T) {
	got := Missing([]string{"agentic", "gpu"}, []string{"agentic"})
	if want := []string{"gpu"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Missing = %v, want %v", got, want)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/caps/ -v`
Expected: FAIL — `undefined: Parse`.

- [ ] **Step 3: Implement**

```go
// internal/caps/caps.go
package caps

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The closed vocabulary (spec §6). Unknown names are rejected at write time so
// a typo surfaces immediately instead of producing permanently unplaceable work.
var vocabulary = map[string]struct{}{
	"agentic": {}, "reflex": {}, "shell": {}, "network": {},
	"no_egress": {}, "strict_local": {}, "long_context": {},
	"vision": {}, "gpu": {},
}

const (
	localFSPrefix = "local_fs:"
	tokensPrefix  = "tokens:"
	lanPrefix     = "lan:"
)

// Parse validates and canonicalises a capability list: sorted, de-duplicated,
// and free of unknown names or malformed parameters.
func Parse(raw []string) ([]string, error) {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))

	for _, c := range raw {
		c = strings.TrimSpace(c)
		if c == "" {
			return nil, fmt.Errorf("empty capability")
		}
		if err := validate(c); err != nil {
			return nil, err
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

func validate(c string) error {
	switch {
	case strings.HasPrefix(c, localFSPrefix):
		p := strings.TrimPrefix(c, localFSPrefix)
		if !strings.HasPrefix(p, "/") {
			return fmt.Errorf("capability %q: local_fs path must be absolute", c)
		}
		if len(p) < 2 {
			return fmt.Errorf("capability %q: local_fs path must not be root", c)
		}
		return nil
	case strings.HasPrefix(c, tokensPrefix):
		n, err := strconv.Atoi(strings.TrimPrefix(c, tokensPrefix))
		if err != nil || n <= 0 {
			return fmt.Errorf("capability %q: tokens must be a positive integer", c)
		}
		return nil
	case strings.HasPrefix(c, lanPrefix):
		if strings.TrimSpace(strings.TrimPrefix(c, lanPrefix)) == "" {
			return fmt.Errorf("capability %q: lan needs a service name", c)
		}
		return nil
	default:
		if _, ok := vocabulary[c]; !ok {
			return fmt.Errorf("unknown capability %q", c)
		}
		return nil
	}
}

// Satisfies reports whether anything providing `provides` can serve `needs`.
// Parameterised capabilities do not match by string equality (spec §6).
func Satisfies(needs, provides []string) bool { return len(Missing(needs, provides)) == 0 }

// Missing returns the needs that `provides` does not cover, in need order.
func Missing(needs, provides []string) []string {
	var miss []string
	for _, n := range needs {
		if !satisfiedBy(n, provides) {
			miss = append(miss, n)
		}
	}
	return miss
}

func satisfiedBy(need string, provides []string) bool {
	for _, p := range provides {
		switch {
		case p == need:
			return true
		case strings.HasPrefix(need, localFSPrefix) && strings.HasPrefix(p, localFSPrefix):
			np := strings.TrimPrefix(need, localFSPrefix)
			pp := strings.TrimPrefix(p, localFSPrefix)
			if pp == np || strings.HasPrefix(np, strings.TrimSuffix(pp, "/")+"/") {
				return true
			}
		case strings.HasPrefix(need, tokensPrefix) && strings.HasPrefix(p, tokensPrefix):
			n, e1 := strconv.Atoi(strings.TrimPrefix(need, tokensPrefix))
			m, e2 := strconv.Atoi(strings.TrimPrefix(p, tokensPrefix))
			if e1 == nil && e2 == nil && m >= n {
				return true
			}
		}
	}
	return false
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/caps/ -v`
Expected: PASS (all subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/caps
git commit -m "feat(caps): closed vocabulary, parsing, and subset matching"
```

---

## Task 5: `placement` — eligibility, ranking, reason

**Files:**
- Create: `internal/board/placement.go`, `internal/board/placement_test.go`

**Interfaces:**
- Produces:
  - `type Candidate struct { Node Node; Endpoint Endpoint; Model Model }`
  - `type Reject struct { NodeName, Why string }`
  - `type Decision struct { OK bool; Chosen *Candidate; Reason string; Rejects []Reject }`
  - `board.Place(item WorkItem, cands []Candidate) Decision`
  - `board.Eligible(item WorkItem, c Candidate) (bool, string)`
  - Types `Node`, `Endpoint`, `Model`, `WorkItem` (structs mirroring the tables, with `Needs`, `Provides`, `Features`, `ReachClasses`, `Health`, `Role`, `Status` fields)

This task is the heart of the design and is a **pure function** — no DB, no network. That is why it can be tested exhaustively.

- [ ] **Step 1: Write the failing eligibility test**

```go
// internal/board/placement_test.go
package board

import "testing"

func cand(nodeName, role string, provides []string, endpointName, reach, health string, features []string, cost float64) Candidate {
	return Candidate{
		Node:     Node{Name: nodeName, Role: role, ProvidesLive: provides, ReachClasses: []string{"host", "lan", "wan"}},
		Endpoint: Endpoint{Name: endpointName, Reach: reach, Health: health},
		Model:    Model{ModelID: endpointName + "-m", Features: features, CostInPerMtok: cost, ContextWindow: 200000},
	}
}

func item(needs ...string) WorkItem {
	return WorkItem{ID: "w1", Needs: needs, Status: "queued"}
}

func TestPlaceRejectsEndpointTheNodeCannotReach(t *testing.T) {
	c := cand("laptop", "executor", []string{"agentic", "shell"}, "cloud", "wan", "ok", []string{"tools"}, 1)
	c.Node.ReachClasses = []string{"host"}

	d := Place(item("agentic"), []Candidate{c})
	if d.OK {
		t.Fatal("placed on an unreachable endpoint")
	}
	if len(d.Rejects) != 1 {
		t.Fatalf("got %d rejects, want 1", len(d.Rejects))
	}
}

func TestPlaceNeverCrossesWANForNoEgress(t *testing.T) {
	cheapWAN := cand("laptop", "executor", []string{"agentic"}, "cloud", "wan", "ok", []string{"tools"}, 0.1)
	lanPi := cand("pi", "steward", []string{"reflex", "no_egress"}, "local", "host", "ok", nil, 0)

	d := Place(item("reflex", "no_egress"), []Candidate{cheapWAN, lanPi})
	if !d.OK {
		t.Fatalf("expected a placement, rejects: %+v", d.Rejects)
	}
	if d.Chosen.Endpoint.Reach != "host" {
		t.Fatalf("chose reach %q, want host", d.Chosen.Endpoint.Reach)
	}
}

func TestPlaceStrictLocalRequiresHost(t *testing.T) {
	lan := cand("pi", "steward", []string{"reflex", "strict_local"}, "lan-ollama", "lan", "ok", nil, 0)
	if d := Place(item("reflex", "strict_local"), []Candidate{lan}); d.OK {
		t.Fatal("strict_local placed on a lan endpoint")
	}
}

func TestAgenticRequiresToolsAndExecutor(t *testing.T) {
	noTools := cand("laptop", "executor", []string{"agentic"}, "llamacpp", "host", "ok", nil, 0)
	if d := Place(item("agentic"), []Candidate{noTools}); d.OK {
		t.Fatal("agentic placed on a model without the tools feature")
	}

	stewardWithTools := cand("pi", "steward", []string{"agentic"}, "local", "host", "ok", []string{"tools"}, 0)
	if d := Place(item("agentic"), []Candidate{stewardWithTools}); d.OK {
		t.Fatal("agentic placed on a steward")
	}
}

func TestPlaceSkipsUnhealthyAndPicksCheapest(t *testing.T) {
	expensive := cand("a", "executor", []string{"agentic"}, "claude", "wan", "ok", []string{"tools"}, 9)
	cheap := cand("b", "executor", []string{"agentic"}, "gpt", "wan", "ok", []string{"tools"}, 1)
	warming := cand("c", "executor", []string{"agentic"}, "llamacpp", "host", "warming", []string{"tools"}, 0)

	d := Place(item("agentic"), []Candidate{expensive, warming, cheap})
	if !d.OK {
		t.Fatalf("no placement: %+v", d.Rejects)
	}
	if d.Chosen.Endpoint.Name != "gpt" {
		t.Fatalf("chose %q, want gpt (cheapest healthy)", d.Chosen.Endpoint.Name)
	}
	if d.Reason == "" {
		t.Fatal("Reason is empty; every placement must explain itself")
	}
}

func TestUnsatisfiableExplainsWhatIsMissing(t *testing.T) {
	pi := cand("pi", "steward", []string{"reflex"}, "local", "host", "ok", nil, 0)

	d := Place(item("reflex", "gpu"), []Candidate{pi})
	if d.OK {
		t.Fatal("placed work nothing can do")
	}
	if d.Reason == "" {
		t.Fatal("unsatisfiable placement has no reason")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/board/ -run Place -v`
Expected: FAIL — `undefined: Candidate`.

- [ ] **Step 3: Implement placement**

```go
// internal/board/placement.go
package board

import (
	"fmt"
	"sort"
	"strings"

	"github.com/cti97/the-nightshift-manager/internal/caps"
)

type Node struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Role           string   `json:"role"`
	ProvidesLive   []string `json:"provides_live"`
	ReachClasses   []string `json:"reach_classes"`
	Concurrency    int      `json:"concurrency"`
	Status         string   `json:"status"`
	AgentVersion   string   `json:"agent_version"`
}

type Endpoint struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Provider string `json:"provider"`
	Reach  string `json:"reach"`
	Health string `json:"health"`
}

type Model struct {
	ID            string   `json:"id"`
	ModelID       string   `json:"model_id"`
	Provides      []string `json:"provides"`
	Features      []string `json:"features"`
	ContextWindow int      `json:"context_window"`
	CostInPerMtok float64  `json:"cost_in_per_mtok"`
}

type WorkItem struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Needs   []string `json:"needs"`
	Status  string   `json:"status"`
	Attempt int      `json:"attempt"`
}

type Candidate struct {
	Node     Node
	Endpoint Endpoint
	Model    Model
}

type Reject struct {
	NodeName string `json:"node"`
	Why      string `json:"why"`
}

type Decision struct {
	OK      bool
	Chosen  *Candidate
	Reason  string
	Rejects []Reject
}

// Eligible applies spec §9. It returns false plus a human-readable reason;
// the reason is what /why prints, so it must name the rule that failed.
func Eligible(item WorkItem, c Candidate) (bool, string) {
	provides := append(append([]string{}, c.Node.ProvidesLive...), c.Model.Provides...)
	if miss := caps.Missing(item.Needs, provides); len(miss) > 0 {
		return false, "no provider for " + strings.Join(miss, ",")
	}
	if c.Endpoint.Health != "ok" {
		return false, "endpoint " + c.Endpoint.Name + " is " + c.Endpoint.Health
	}
	if !contains(c.Node.ReachClasses, c.Endpoint.Reach) {
		return false, fmt.Sprintf("node cannot reach %s endpoints", c.Endpoint.Reach)
	}
	for _, need := range item.Needs {
		switch need {
		case "no_egress":
			if c.Endpoint.Reach != "host" && c.Endpoint.Reach != "lan" {
				return false, "no_egress forbids wan"
			}
		case "strict_local":
			if c.Endpoint.Reach != "host" {
				return false, "strict_local requires a host endpoint"
			}
		}
	}
	if contains(item.Needs, "agentic") {
		if c.Node.Role != "executor" {
			return false, "agentic requires an executor"
		}
		if !contains(c.Model.Features, "tools") {
			return false, "agentic requires the model tools feature"
		}
	}
	if contains(item.Needs, "reflex") && c.Node.Role != "executor" && c.Node.Role != "steward" {
		return false, "reflex requires an executor or steward"
	}
	if contains(item.Needs, "vision") && !contains(c.Model.Features, "vision") {
		return false, "vision requires the model vision feature"
	}
	for _, need := range item.Needs {
		if n, ok := tokenNeed(need); ok && c.Model.ContextWindow < n {
			return false, fmt.Sprintf("context %d < %d", c.Model.ContextWindow, n)
		}
	}
	return true, ""
}

// Place picks the cheapest eligible candidate. Ranking is cost, then node name
// for determinism — never a model's opinion (spec D5).
func Place(item WorkItem, cands []Candidate) Decision {
	sorted := append([]Candidate{}, cands...)
	sort.Slice(sorted, func(i, j int) bool {
		ci, cj := sorted[i].Model.CostInPerMtok, sorted[j].Model.CostInPerMtok
		if ci != cj {
			return ci < cj
		}
		return sorted[i].Node.Name < sorted[j].Node.Name
	})

	var rejects []Reject
	for i := range sorted {
		ok, why := Eligible(item, sorted[i])
		if ok {
			chosen := sorted[i]
			return Decision{
				OK:      true,
				Chosen:  &chosen,
				Reason:  reasonFor(item, chosen, len(rejects)),
				Rejects: rejects,
			}
		}
		rejects = append(rejects, Reject{NodeName: sorted[i].Node.Name, Why: why})
	}

	return Decision{
		OK:      false,
		Reason:  fmt.Sprintf("no eligible candidate for [%s]", strings.Join(item.Needs, ",")),
		Rejects: rejects,
	}
}

func reasonFor(item WorkItem, c Candidate, skipped int) string {
	return fmt.Sprintf("%s on %s via %s @%s (%.2f/Mtok), %d cheaper/earlier rejected",
		item.ID, c.Node.Name, c.Model.ModelID, c.Endpoint.Reach, c.Model.CostInPerMtok, skipped)
}

func tokenNeed(c string) (int, bool) {
	const p = "tokens:"
	if !strings.HasPrefix(c, p) {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimPrefix(c, p), "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/board/ -run Place -v`
Expected: PASS (6 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/board/placement.go internal/board/placement_test.go
git commit -m "feat(board): placement eligibility, ranking, and explanation"
```

---

## Task 6: HTTP server, bearer auth, `/version`, `/board`

**Files:**
- Create: `internal/board/http.go`, `internal/board/http_test.go`
- Modify: `cmd/nsm/main.go` (wire `nsm manager serve`)

**Interfaces:**
- Produces:
  - `board.NewServer(db *sql.DB, clock Clock, adminTokenHash string) *Server`
  - `(*Server).Handler() http.Handler`
  - `board.HashToken(tok string) string`
  - `board.NewToken() (string, error)`
  - `board.WriteJSON(w, status int, v any)` and `board.WriteError(w, status int, code, msg string)`

- [ ] **Step 1: Write the failing auth test**

```go
// internal/board/http_test.go
package board

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newServer(t *testing.T) (*Server, *FixedClock) {
	t.Helper()
	db, clock := newTestDB(t)
	srv := NewServer(db, clock, HashToken("admin-secret"))
	return srv, clock
}

func do(t *testing.T, h http.Handler, method, path, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestVersionIsPublic(t *testing.T) {
	srv, _ := newServer(t)
	rec := do(t, srv.Handler(), "GET", "/version", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestBoardRequiresToken(t *testing.T) {
	srv, _ := newServer(t)
	if rec := do(t, srv.Handler(), "GET", "/admin/board", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec := do(t, srv.Handler(), "GET", "/admin/board", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a wrong token", rec.Code)
	}
}

func TestBoardWithAdminTokenReturnsCounts(t *testing.T) {
	srv, _ := newServer(t)
	rec := do(t, srv.Handler(), "GET", "/admin/board", "admin-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["queued"]; !ok {
		t.Fatalf("board counts = %v, want a queued key", body)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/board/ -run 'Version|Board' -v`
Expected: FAIL — `undefined: NewServer`.

- [ ] **Step 3: Implement the server**

```go
// internal/board/http.go
package board

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/cti97/the-nightshift-manager/internal/version"
)

type Server struct {
	db             *sql.DB
	clock          Clock
	adminTokenHash string
}

func NewServer(db *sql.DB, clock Clock, adminTokenHash string) *Server {
	return &Server{db: db, clock: clock, adminTokenHash: adminTokenHash}
}

// HashToken is the only form in which a credential is stored or compared.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// NewToken returns a 32-byte random credential. It is shown to the caller once.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", s.handleVersion)
	mux.Handle("GET /admin/board", s.adminAuth(http.HandlerFunc(s.handleBoard)))
	return mux
}

func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearer(r)
		if !ok || HashToken(tok) != s.adminTokenHash {
			WriteError(w, http.StatusUnauthorized, "unauthorized", "admin token required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearer(r *http.Request) (string, bool) {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(p) || h[:len(p)] != p {
		return "", false
	}
	return h[len(p):], true
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	v, c := version.Info()
	WriteJSON(w, http.StatusOK, map[string]any{
		"version":          v,
		"commit":           c,
		"protocol_min":     1,
		"protocol_max":     1,
		"schema_version":   SchemaVersion(),
	})
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT status, COUNT(*) FROM work_items GROUP BY status`)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			WriteError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		counts[status] = n
	}
	for _, s := range []string{"queued", "claimed", "running", "needs_approval", "done", "failed"} {
		if _, ok := counts[s]; !ok {
			counts[s] = 0
		}
	}
	WriteJSON(w, http.StatusOK, counts)
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func WriteError(w http.ResponseWriter, status int, code, msg string) {
	WriteJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// Errorf keeps handler error construction uniform.
func Errorf(format string, a ...any) string { return fmt.Sprintf(format, a...) }
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/board/ -run 'Version|Board' -v`
Expected: PASS.

- [ ] **Step 5: Wire `nsm manager serve`**

Add to `cmd/nsm/main.go`:

```go
case "manager":
	if len(args) < 2 || args[1] != "serve" {
		usage(errOut)
		return 2
	}
	return managerServe(args[2:], errOut)
```

and a new function in the same file:

```go
// managerServe reads the token hash from the environment so the plaintext is
// never on a command line (spec §6 of AGENTS.md: names, never values).
func managerServe(args []string, errOut io.Writer) int {
	dbPath := flag.NewFlagSet("manager serve", flag.ContinueOnError)
	dbPath.SetOutput(errOut)
	addr := dbPath.String("addr", "127.0.0.1:8081", "listen address")
	path := dbPath.String("db", "nsm.db", "sqlite path")
	if err := dbPath.Parse(args); err != nil {
		return 2
	}
	admin := os.Getenv("NSM_ADMIN_TOKEN")
	if admin == "" {
		fmt.Fprintln(errOut, "NSM_ADMIN_TOKEN is required")
		return 2
	}
	db, err := board.Open(*path)
	if err != nil {
		fmt.Fprintf(errOut, "open board: %v\n", err)
		return 1
	}
	defer db.Close()

	srv := board.NewServer(db, board.SystemClock(), board.HashToken(admin))
	fmt.Fprintf(errOut, "nsm manager listening on %s (db %s)\n", *addr, *path)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		fmt.Fprintf(errOut, "serve: %v\n", err)
		return 1
	}
	return 0
}
```

Add imports `flag`, `net/http`, and the board package.

- [ ] **Step 6: Build and smoke it**

```bash
go build -o /tmp/nsm ./cmd/nsm
NSM_ADMIN_TOKEN=dev NSM_ENROLL_TOKEN=enroll /tmp/nsm manager serve --addr 127.0.0.1:8099 --db /tmp/nsm-smoke.db &
sleep 1
curl -s http://127.0.0.1:8099/version
curl -s -H 'Authorization: Bearer dev' http://127.0.0.1:8099/admin/board
kill %1
```

Expected: version JSON, then `{"claimed":0,...}`.

- [ ] **Step 7: Run everything and commit**

```bash
go test ./... && go vet ./...
git add internal/board/http.go internal/board/http_test.go cmd/nsm/main.go
git commit -m "feat(board): http server with bearer auth, /version, /admin/board"
```

**M0 exit criteria:** `go test ./...` green; `nsm manager serve` starts; `/version` answers without a token; `/admin/board` refuses without one. Nothing claims work yet.

---

## Task 7: Register, token issue, heartbeat, lease extension

**Files:**
- Create: `internal/board/nodes.go`, `internal/board/nodes_test.go`
- Modify: `internal/board/http.go` (routes + `nodeAuth`)

**Interfaces:**
- Produces:
  - `(*Server) handleRegister`, `(*Server) handleHeartbeat`, `(*Server) nodeAuth`
  - `board.RegisterNode(ctx, db, clock, req RegisterRequest) (nodeID, token string, err error)`
  - `board.Heartbeat(ctx, db, clock, nodeID string, req HeartbeatRequest) (HeartbeatResponse, error)`
  - `const leaseSeconds = 300`

- [ ] **Step 1: Write the failing register/heartbeat test**

```go
// internal/board/nodes_test.go
package board

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRegisterIssuesAWorkingTokenAndStoresOnlyItsHash(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()

	id, token, err := RegisterNode(ctx, db, clock, RegisterRequest{
		Name: "home-laptop", Role: "executor",
		ProvidesDeclared: []string{"agentic", "shell"},
		ReachClasses:     []string{"host", "lan", "wan"},
		Concurrency:      1,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if token == "" || id == "" {
		t.Fatal("register returned an empty id or token")
	}

	var stored string
	if err := db.QueryRow(`SELECT token_hash FROM nodes WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatalf("read token_hash: %v", err)
	}
	if stored == token {
		t.Fatal("the plaintext token was stored; only its hash may be")
	}
	if stored != HashToken(token) {
		t.Fatal("stored hash does not match HashToken(token)")
	}
}

func TestRegisterRejectsDuplicateName(t *testing.T) {
	db, clock := newTestDB(t)
	req := RegisterRequest{Name: "pi", Role: "steward", ProvidesDeclared: []string{"reflex"}, ReachClasses: []string{"host"}}
	if _, _, err := RegisterNode(context.Background(), db, clock, req); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if _, _, err := RegisterNode(context.Background(), db, clock, req); err == nil {
		t.Fatal("duplicate name accepted, want an error")
	}
}

func TestHeartbeatUpdatesLiveCapabilitiesAndLease(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	id, _, err := RegisterNode(ctx, db, clock, RegisterRequest{
		Name: "home-laptop", Role: "executor",
		ProvidesDeclared: []string{"agentic"}, ReachClasses: []string{"host"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// An item this node holds, with a lease that expires soon.
	clock.Set(1_700_000_000)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO work_items (id,title,brief,needs,status,claimed_by,lease_expires_at,created_by,created_at,updated_at)
		VALUES ('w1','t','b','["agentic"]','claimed',?,?, 'admin',?,?)`,
		id, clock.Now()+10, clock.Now(), clock.Now()); err != nil {
		t.Fatalf("insert item: %v", err)
	}

	resp, err := Heartbeat(ctx, db, clock, id, HeartbeatRequest{
		ProvidesLive:  []string{"agentic", "lan:nas"},
		ReachClasses:  []string{"host", "lan"},
		FreeSlots:     1,
		AgentVersion:  "v0.1.0",
		Protocol:      1,
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if resp.Status != "online" {
		t.Fatalf("status = %q, want online", resp.Status)
	}

	var lease int64
	if err := db.QueryRow(`SELECT lease_expires_at FROM work_items WHERE id='w1'`).Scan(&lease); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if lease != clock.Now()+leaseSeconds {
		t.Fatalf("lease = %d, want %d", lease, clock.Now()+leaseSeconds)
	}

	var live string
	if err := db.QueryRow(`SELECT provides_live FROM nodes WHERE id=?`, id).Scan(&live); err != nil {
		t.Fatalf("read provides_live: %v", err)
	}
	if live != `["agentic","lan:nas"]` {
		t.Fatalf("provides_live = %s", live)
	}
}

func TestHeartbeatMarksIncompatibleProtocol(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	id, _, _ := RegisterNode(ctx, db, clock, RegisterRequest{
		Name: "old-node", Role: "executor", ProvidesDeclared: []string{}, ReachClasses: []string{},
	})

	resp, err := Heartbeat(ctx, db, clock, id, HeartbeatRequest{Protocol: 99})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if resp.Status != "incompatible" {
		t.Fatalf("status = %q, want incompatible", resp.Status)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/board/ -run 'Register|Heartbeat' -v`
Expected: FAIL — `undefined: RegisterNode`.

- [ ] **Step 3: Implement**

```go
// internal/board/nodes.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cti97/the-nightshift-manager/internal/caps"
)

const leaseSeconds = 300

// ProtocolMin/Max are what this binary can speak. A node outside the range is
// marked incompatible rather than left crash-looping (spec §20).
const (
	ProtocolMin = 1
	ProtocolMax = 1
)

type RegisterRequest struct {
	Name             string   `json:"name"`
	Role             string   `json:"role"`
	ProvidesDeclared []string `json:"provides_declared"`
	ReachClasses     []string `json:"reach_classes"`
	Concurrency      int      `json:"concurrency"`
}

type RegisterResponse struct {
	NodeID string `json:"node_id"`
	Token  string `json:"token"`
}

type HeartbeatRequest struct {
	ProvidesLive  []string `json:"provides_live"`
	ReachClasses  []string `json:"reach_classes"`
	FreeSlots     int      `json:"free_slots"`
	AgentVersion  string   `json:"agent_version"`
	Protocol      int      `json:"protocol_version"`
}

type HeartbeatResponse struct {
	Status     string `json:"status"`
	LeaseUntil int64  `json:"lease_until,omitempty"`
	Queued     int    `json:"queued"`
	Approvals  int    `json:"pending_approvals"`
}

func RegisterNode(ctx context.Context, db *sql.DB, clock Clock, req RegisterRequest) (string, string, error) {
	switch req.Role {
	case "executor", "steward":
	case "manager":
		return "", "", errors.New("a manager does not register as a worker")
	default:
		return "", "", fmt.Errorf("unknown role %q", req.Role)
	}
	declared, err := caps.Parse(req.ProvidesDeclared)
	if err != nil {
		return "", "", err
	}
	token, err := NewToken()
	if err != nil {
		return "", "", err
	}
	id, err := NewToken() // 32 random bytes as an id: no ordering, no guessing
	if err != nil {
		return "", "", err
	}

	idJSON, _ := json.Marshal(declared)
	reachJSON, _ := json.Marshal(req.ReachClasses)
	conc := req.Concurrency
	if conc < 1 {
		conc = 1
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO nodes (id,name,token_hash,role,provides_declared,provides_live,reach_classes,concurrency,created_at,status)
		 VALUES (?,?,?,?,?,?,?,?,?,'offline')`,
		id, req.Name, HashToken(token), req.Role, string(idJSON), string(idJSON),
		string(reachJSON), conc, clock.Now()); err != nil {
		return "", "", fmt.Errorf("register %q: %w", req.Name, err)
	}
	if err := AppendEvent(ctx, tx, clock, "admin", "node", id, "registered",
		map[string]string{"name": req.Name, "role": req.Role}); err != nil {
		return "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	return id, token, nil
}

func Heartbeat(ctx context.Context, db *sql.DB, clock Clock, nodeID string, req HeartbeatRequest) (HeartbeatResponse, error) {
	if req.Protocol < ProtocolMin || req.Protocol > ProtocolMax {
		_, err := db.ExecContext(ctx, `UPDATE nodes SET status='incompatible', last_seen_at=? WHERE id=?`,
			clock.Now(), nodeID)
		if err != nil {
			return HeartbeatResponse{}, err
		}
		return HeartbeatResponse{Status: "incompatible"}, nil
	}

	live, err := caps.Parse(req.ProvidesLive)
	if err != nil {
		return HeartbeatResponse{}, err
	}
	liveJSON, _ := json.Marshal(live)
	reachJSON, _ := json.Marshal(req.ReachClasses)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return HeartbeatResponse{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`UPDATE nodes SET provides_live=?, reach_classes=?, last_seen_at=?, status='online',
		 agent_version=?, protocol_version=? WHERE id=?`,
		string(liveJSON), string(reachJSON), clock.Now(), req.AgentVersion, req.Protocol, nodeID); err != nil {
		return HeartbeatResponse{}, err
	}

	lease := clock.Now() + leaseSeconds
	if _, err := tx.ExecContext(ctx,
		`UPDATE work_items SET lease_expires_at=?
		  WHERE claimed_by=? AND status IN ('claimed','running')`,
		lease, nodeID); err != nil {
		return HeartbeatResponse{}, err
	}

	var queued int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_items WHERE status='queued'`).Scan(&queued); err != nil {
		return HeartbeatResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return HeartbeatResponse{}, err
	}

	return HeartbeatResponse{Status: "online", LeaseUntil: lease, Queued: queued}, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/board/ -run 'Register|Heartbeat' -v`
Expected: PASS.

- [ ] **Step 5: Wire the routes and handlers**

In `Handler()`:

```go
mux.HandleFunc("POST /nodes/register", s.handleRegister)
mux.Handle("POST /nodes/heartbeat", s.nodeAuth(http.HandlerFunc(s.handleHeartbeat)))
```

In `http.go`:

```go
type ctxKey string

const nodeIDKey ctxKey = "node_id"

// nodeAuth resolves a bearer token to a node id. The token itself is never
// stored, so this compares hashes and nothing else.
func (s *Server) nodeAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearer(r)
		if !ok {
			WriteError(w, http.StatusUnauthorized, "unauthorized", "node token required")
			return
		}
		var id string
		err := s.db.QueryRowContext(r.Context(),
			`SELECT id FROM nodes WHERE token_hash = ?`, HashToken(tok)).Scan(&id)
		if err != nil {
			WriteError(w, http.StatusUnauthorized, "unauthorized", "unknown node token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), nodeIDKey, id)))
	})
}

func nodeIDFrom(r *http.Request) string {
	id, _ := r.Context().Value(nodeIDKey).(string)
	return id
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	id, token, err := RegisterNode(r.Context(), s.db, s.clock, req)
	if err != nil {
		WriteError(w, http.StatusConflict, "register", err.Error())
		return
	}
	// The plaintext token is returned exactly once, here.
	WriteJSON(w, http.StatusCreated, RegisterResponse{NodeID: id, Token: token})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	resp, err := Heartbeat(r.Context(), s.db, s.clock, nodeIDFrom(r), req)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "heartbeat", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, resp)
}
```

Add `"context"` to `http.go`'s imports.

- [ ] **Step 6: Run everything and commit**

```bash
go test ./... && go vet ./...
git add internal/board/nodes.go internal/board/nodes_test.go internal/board/http.go
git commit -m "feat(board): node registration, tokens, heartbeat, lease extension"
```

---

## Task 8: The claim transaction — exactly once

**Files:**
- Create: `internal/board/claim.go`, `internal/board/claim_test.go`
- Modify: `internal/board/http.go` (route `POST /claim`), `internal/board/items.go`

**Interfaces:**
- Produces:
  - `board.CreateItem(ctx, db, clock, req CreateItemRequest) (string, error)`
  - `board.LoadCandidates(ctx, db *sql.DB) ([]Candidate, error)`
  - `board.Claim(ctx, db, clock, nodeID string, slots int) ([]ClaimedItem, error)`
  - `type ClaimedItem struct { Item WorkItem; RunID string; PlacementReason string }`

- [ ] **Step 1: Write the failing claim tests**

First, extend `internal/board/testing.go` (created in Task 3) with the two seeding helpers — they live in the non-test file so the agent package can use them too:

```go
// internal/board/testing.go — appended
func SeedNodeForTest(t *testing.T, db *sql.DB, clock *FixedClock, name, role string, provides ...string) string {
	t.Helper()
	id, _, err := RegisterNode(context.Background(), db, clock, RegisterRequest{
		Name: name, Role: role, ProvidesDeclared: provides,
		ReachClasses: []string{"host", "lan", "wan"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("seed node %s: %v", name, err)
	}
	if _, err := Heartbeat(context.Background(), db, clock, id, HeartbeatRequest{
		ProvidesLive: provides, ReachClasses: []string{"host", "lan", "wan"}, Protocol: 1,
	}); err != nil {
		t.Fatalf("heartbeat %s: %v", name, err)
	}
	return id
}

func SeedEndpointForTest(t *testing.T, db *sql.DB, clock *FixedClock, name, reach string, cost float64, features ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO endpoints (id,name,provider,base_url,reach,health,created_at) VALUES (?,?,?,?,?,'ok',?)`,
		"ep-"+name, name, "stub", "http://"+name, reach, clock.Now()); err != nil {
		t.Fatalf("insert endpoint: %v", err)
	}
	feat := `["` + strings.Join(features, `","`) + `"]`
	if len(features) == 0 {
		feat = "[]"
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO models (id,endpoint_id,model_id,provides,features,context_window,cost_in_per_mtok)
		 VALUES (?,?,?,'[]',?,200000,?)`,
		"m-"+name, "ep-"+name, name+"-model", feat, cost); err != nil {
		t.Fatalf("insert model: %v", err)
	}
}
```

Add `"database/sql"` and `"strings"` to `testing.go`'s imports. Then the tests, in `internal/board/claim_test.go`:

```go
// internal/board/claim_test.go
package board

import (
	"context"
	"sync"
	"testing"
)

func TestClaimIsExactlyOnceUnderConcurrency(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	nodeA := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	nodeB := SeedNodeForTest(t, db, clock, "b", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")

	if _, err := CreateItem(ctx, db, clock, CreateItemRequest{
		Title: "only once", Brief: "b", Needs: []string{"agentic"},
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	var wg sync.WaitGroup
	results := make([][]ClaimedItem, 2)
	nodes := []string{nodeA, nodeB}
	for i := range nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := Claim(ctx, db, clock, nodes[i], 1)
			if err != nil {
				t.Errorf("Claim(%d): %v", i, err)
				return
			}
			results[i] = got
		}(i)
	}
	wg.Wait()

	total := len(results[0]) + len(results[1])
	if total != 1 {
		t.Fatalf("two claimants got %d items, want exactly 1", total)
	}
}

func TestClaimSkipsWorkTheNodeCannotDo(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	steward := SeedNodeForTest(t, db, clock, "pi", "steward", "reflex")
	SeedEndpointForTest(t, db, clock, "local", "host", 0)

	if _, err := CreateItem(ctx, db, clock, CreateItemRequest{
		Title: "needs tools", Brief: "b", Needs: []string{"agentic"},
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	got, err := Claim(ctx, db, clock, steward, 1)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("steward claimed %d agentic items, want 0", len(got))
	}
}

func TestClaimRecordsRunAndAttempt(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})

	got, err := Claim(ctx, db, clock, node, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("Claim: got %d items, err %v", len(got), err)
	}
	if got[0].Item.ID != id {
		t.Fatalf("claimed %q, want %q", got[0].Item.ID, id)
	}
	if got[0].PlacementReason == "" {
		t.Fatal("run has no placement reason")
	}

	var attempt int
	var status string
	if err := db.QueryRow(`SELECT attempt, status FROM work_items WHERE id=?`, id).Scan(&attempt, &status); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if attempt != 1 || status != "claimed" {
		t.Fatalf("item attempt=%d status=%s, want 1/claimed", attempt, status)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/board/ -run Claim -v`
Expected: FAIL — `undefined: Claim`.

- [ ] **Step 3: Implement items and claim**

```go
// internal/board/items.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/cti97/the-nightshift-manager/internal/caps"
)

type CreateItemRequest struct {
	Title      string   `json:"title"`
	Brief      string   `json:"brief"`
	Needs      []string `json:"needs"`
	Priority   int      `json:"priority"`
	MaxAttempts int     `json:"max_attempts"`
	ParentID   string   `json:"parent_id"`
}

func CreateItem(ctx context.Context, db *sql.DB, clock Clock, req CreateItemRequest) (string, error) {
	if req.Title == "" || req.Brief == "" {
		return "", fmt.Errorf("title and brief are required")
	}
	needs, err := caps.Parse(req.Needs)
	if err != nil {
		return "", err
	}
	maxAttempts := req.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 3
	}
	id, err := NewToken()
	if err != nil {
		return "", err
	}
	needsJSON, _ := json.Marshal(needs)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO work_items (id,parent_id,title,brief,needs,status,priority,max_attempts,created_by,created_at,updated_at)
		 VALUES (?,?,?,?,?,'queued',?,?,'admin',?,?)`,
		id, nullable(req.ParentID), req.Title, req.Brief, string(needsJSON),
		req.Priority, maxAttempts, clock.Now(), clock.Now()); err != nil {
		return "", err
	}
	if err := AppendEvent(ctx, tx, clock, "admin", "work_item", id, "created",
		map[string]any{"title": req.Title, "needs": needs}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func nullable(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
```

```go
// internal/board/claim.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

type ClaimedItem struct {
	Item            WorkItem `json:"item"`
	RunID           string   `json:"run_id"`
	PlacementReason string   `json:"placement_reason"`
}

// LoadCandidates reads every online node but `nodeID`, every endpoint that is
// healthy, and their enabled models: the input to placement.
func LoadCandidates(ctx context.Context, db *sql.DB) ([]Candidate, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT n.id, n.name, n.role, n.provides_live, n.reach_classes, n.concurrency,
		       n.status, n.agent_version,
		       e.id, e.name, e.provider, e.reach, e.health,
		       m.id, m.model_id, m.provides, m.features, m.context_window, m.cost_in_per_mtok
		  FROM nodes n
		  JOIN endpoints e ON 1=1
		  JOIN models m ON m.endpoint_id = e.id
		 WHERE n.status = 'online' AND m.enabled = 1 AND e.health = 'ok'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var c Candidate
		var live, reach, provides, features string
		if err := rows.Scan(
			&c.Node.ID, &c.Node.Name, &c.Node.Role, &live, &reach, &c.Node.Concurrency,
			&c.Node.Status, &c.Node.AgentVersion,
			&c.Endpoint.ID, &c.Endpoint.Name, &c.Endpoint.Provider, &c.Endpoint.Reach, &c.Endpoint.Health,
			&c.Model.ID, &c.Model.ModelID, &provides, &features, &c.Model.ContextWindow, &c.Model.CostInPerMtok,
		); err != nil {
			return nil, err
		}
		var err error
		if c.Node.ProvidesLive, err = decodeList(live); err != nil {
			return nil, err
		}
		if c.Node.ReachClasses, err = decodeList(reach); err != nil {
			return nil, err
		}
		if c.Model.Provides, err = decodeList(provides); err != nil {
			return nil, err
		}
		if c.Model.Features, err = decodeList(features); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func decodeList(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decode capability list %q: %w", raw, err)
	}
	return out, nil
}

// Claim takes up to `slots` eligible items for this node, atomically.
//
// BEGIN IMMEDIATE on a dedicated connection plus the claimed_by IS NULL guard
// plus a rowcount check is the whole concurrency story (spec §8). Do not
// replace it with optimistic retries; the single writer is the design.
func Claim(ctx context.Context, db *sql.DB, clock Clock, nodeID string, slots int) ([]ClaimedItem, error) {
	if slots < 1 {
		slots = 1
	}

	// Order matters. Candidates are loaded BEFORE the connection is checked
	// out, because the pool is capped at one connection: holding `conn` while
	// querying through `db` blocks forever waiting for a free connection.
	// Slightly stale capabilities are an acceptable price; a deadlock is not.
	cands, err := LoadCandidates(ctx, db)
	if err != nil {
		return nil, err
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	// Eligible candidates for this node only.
	var mine []Candidate
	for _, c := range cands {
		if c.Node.ID == nodeID {
			mine = append(mine, c)
		}
	}
	if len(mine) == 0 {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, err
		}
		committed = true
		return nil, nil
	}

	rows, err := conn.QueryContext(ctx,
		`SELECT id,title,needs,status,attempt FROM work_items
		  WHERE status='queued' ORDER BY priority DESC, created_at ASC LIMIT 50`)
	if err != nil {
		return nil, err
	}

	type row struct {
		id, title, needs, status string
		attempt                  int
	}
	var queued []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.title, &r.needs, &r.status, &r.attempt); err != nil {
			rows.Close()
			return nil, err
		}
		queued = append(queued, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var claimed []ClaimedItem
	for _, r := range queued {
		if len(claimed) >= slots {
			break
		}
		needs, err := decodeList(r.needs)
		if err != nil {
			return nil, err
		}
		item := WorkItem{ID: r.id, Title: r.title, Needs: needs, Status: r.status, Attempt: r.attempt}

		d := Place(item, mine)
		if !d.OK {
			continue
		}
		runID, err := NewToken()
		if err != nil {
			return nil, err
		}
		lease := clock.Now() + leaseSeconds

		res, err := conn.ExecContext(ctx,
			`UPDATE work_items SET status='claimed', claimed_by=?, lease_expires_at=?, attempt=attempt+1, updated_at=?
			  WHERE id=? AND claimed_by IS NULL AND status='queued'`,
			nodeID, lease, clock.Now(), r.id)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n != 1 {
			continue // lost a race; the guard did its job
		}

		if _, err := conn.ExecContext(ctx,
			`INSERT INTO runs (id,work_item_id,attempt,node_id,endpoint_id,model_id,started_at,status,placement_reason)
			 VALUES (?,?,?,?,?,?,?,'claimed',?)`,
			runID, r.id, r.attempt+1, nodeID, d.Chosen.Endpoint.ID, d.Chosen.Model.ID,
			clock.Now(), d.Reason); err != nil {
			return nil, err
		}
		if err := AppendEvent(ctx, conn, clock, "system", "work_item", r.id, "claimed",
			map[string]any{"node": nodeID, "run": runID, "reason": d.Reason}); err != nil {
			return nil, err
		}

		item.Status, item.Attempt = "claimed", r.attempt+1
		claimed = append(claimed, ClaimedItem{Item: item, RunID: runID, PlacementReason: d.Reason})
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return claimed, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/board/ -run Claim -v`
Expected: PASS (3 tests). The concurrency test is the one that matters: if `BEGIN IMMEDIATE` or the guard is wrong, two items get claimed.

- [ ] **Step 5: Add the route and a create endpoint**

```go
mux.Handle("POST /claim", s.nodeAuth(http.HandlerFunc(s.handleClaim)))
mux.Handle("POST /admin/items", s.adminAuth(http.HandlerFunc(s.handleCreateItem)))
```

- [ ] **Step 6: Run everything and commit**

```bash
go test ./... && go vet ./...
git add internal/board
git commit -m "feat(board): atomic claim with placement, run records, and audit events"
```

---

## Task 9: Reaper — `abandoned` versus `lost`

**Files:**
- Create: `internal/board/reaper.go`, `internal/board/reaper_test.go`

**Interfaces:**
- Produces:
  - `board.StartRun(ctx, db, clock, runID, nodeID string) error` — `claimed` → `running`
  - `board.Reap(ctx, db, clock) (ReapResult, error)`
  - `type ReapResult struct { Abandoned, Lost, Failed int }`
  - `(*Server) RunReaper(ctx, every time.Duration)` — a goroutine loop, started by `manager serve`

- [ ] **Step 1: Write the failing reaper tests**

```go
// internal/board/reaper_test.go
package board

import (
	"context"
	"testing"
)

func TestReapRewardsAClaimThatNeverStarted(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})

	if _, err := Claim(ctx, db, clock, node, 1); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	clock.Advance(leaseSeconds + 1)
	res, err := Reap(ctx, db, clock)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if res.Abandoned != 1 {
		t.Fatalf("Abandoned = %d, want 1", res.Abandoned)
	}

	var status string
	var attempt int
	if err := db.QueryRow(`SELECT status, attempt FROM work_items WHERE id=?`, id).Scan(&status, &attempt); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "queued" {
		t.Fatalf("status = %q, want queued", status)
	}
	if attempt != 1 {
		t.Fatalf("attempt = %d, want 1 (a claim that never started must not burn an attempt)", attempt)
	}

	var runStatus string
	if err := db.QueryRow(`SELECT status FROM runs WHERE work_item_id=?`, id).Scan(&runStatus); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if runStatus != "abandoned" {
		t.Fatalf("run status = %q, want abandoned", runStatus)
	}
}

func TestReapMarksAStartedRunLostAndBurnsAnAttempt(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})

	got, err := Claim(ctx, db, clock, node, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := StartRun(ctx, db, clock, got[0].RunID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	clock.Advance(leaseSeconds + 1)
	if _, err := Reap(ctx, db, clock); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	var status string
	var attempt int
	if err := db.QueryRow(`SELECT status, attempt FROM work_items WHERE id=?`, id).Scan(&status, &attempt); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "queued" || attempt != 1 {
		t.Fatalf("item status=%q attempt=%d, want queued/1", status, attempt)
	}

	var runStatus string
	if err := db.QueryRow(`SELECT status FROM runs WHERE work_item_id=?`, id).Scan(&runStatus); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if runStatus != "lost" {
		t.Fatalf("run status = %q, want lost", runStatus)
	}
}

func TestReapFailsAnItemThatExhaustedItsAttempts(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{
		Title: "t", Brief: "b", Needs: []string{"agentic"}, MaxAttempts: 1,
	})

	got, _ := Claim(ctx, db, clock, node, 1)
	if _, err := StartRun(ctx, db, clock, got[0].RunID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	clock.Advance(leaseSeconds + 1)
	if _, err := Reap(ctx, db, clock); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM work_items WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/board/ -run Reap -v`
Expected: FAIL — `undefined: Reap`.

- [ ] **Step 3: Implement the reaper**

```go
// internal/board/reaper.go
package board

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// StartRun moves a run from claimed to running. This single transition is what
// lets the reaper tell a claim that never started from a run that died
// mid-flight, and therefore whether the attempt should be charged.
func StartRun(ctx context.Context, db *sql.DB, clock Clock, runID, nodeID string) error {
	res, err := db.ExecContext(ctx,
		`UPDATE runs SET status='running' WHERE id=? AND node_id=? AND status='claimed'`,
		runID, nodeID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("run %s is not a claimable run for node %s", runID, nodeID)
	}
	return nil
}

type ReapResult struct {
	Abandoned int `json:"abandoned"`
	Lost      int `json:"lost"`
	Failed    int `json:"failed"`
}

// Reap requeues work whose lease expired. The run's status decides whether the
// attempt is spent: `claimed` never started, so it costs nothing; `running`
// may have had side effects, so it burns an attempt (spec §8).
func Reap(ctx context.Context, db *sql.DB, clock Clock) (ReapResult, error) {
	var res ReapResult
	now := clock.Now()

	rows, err := db.QueryContext(ctx,
		`SELECT id, attempt, max_attempts FROM work_items
		  WHERE status IN ('claimed','running') AND lease_expires_at IS NOT NULL AND lease_expires_at < ?`,
		now)
	if err != nil {
		return res, err
	}
	type expiring struct {
		id          string
		attempt     int
		maxAttempts int
	}
	var list []expiring
	for rows.Next() {
		var e expiring
		if err := rows.Scan(&e.id, &e.attempt, &e.maxAttempts); err != nil {
			rows.Close()
			return res, err
		}
		list = append(list, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	for _, e := range list {
		var runID, runStatus string
		err := db.QueryRowContext(ctx,
			`SELECT id, status FROM runs WHERE work_item_id=? AND attempt=?`, e.id, e.attempt).
			Scan(&runID, &runStatus)
		if err != nil && err != sql.ErrNoRows {
			return res, err
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return res, err
		}

		nextRunStatus := "abandoned"
		if runStatus == "running" {
			nextRunStatus = "lost"
			res.Lost++
		} else {
			res.Abandoned++
		}
		if runID != "" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE runs SET status=?, ended_at=?, error=? WHERE id=?`,
				nextRunStatus, now, "lease expired", runID); err != nil {
				tx.Rollback()
				return res, err
			}
		}

		itemStatus := "queued"
		if e.attempt >= e.maxAttempts {
			itemStatus = "failed"
			res.Failed++
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE work_items SET status=?, claimed_by=NULL, lease_expires_at=NULL, updated_at=? WHERE id=?`,
			itemStatus, now, e.id); err != nil {
			tx.Rollback()
			return res, err
		}
		if err := AppendEvent(ctx, tx, clock, "system", "work_item", e.id, "reaped",
			map[string]any{"run": nextRunStatus, "item": itemStatus}); err != nil {
			tx.Rollback()
			return res, err
		}
		if err := tx.Commit(); err != nil {
			return res, err
		}
	}
	return res, nil
}

// RunReaper ticks forever; cancel ctx to stop it.
func (s *Server) RunReaper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := Reap(ctx, s.db, s.clock); err != nil {
				s.logf("reaper: %v", err)
			}
		}
	}
}
```

Add `func (s *Server) logf(format string, a ...any) { log.Printf(format, a...) }` to `http.go`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/board/ -run Reap -v`
Expected: PASS (3 tests). The second and third tests are the contract: `StartRun` must be what
makes an attempt expensive.

- [ ] **Step 5: Commit**

```bash
git add internal/board/reaper.go internal/board/reaper_test.go
git commit -m "feat(board): reaper distinguishing abandoned claims from lost runs"
```

---

## Task 10: Start, idempotent report, and `/why`

**Files:**
- Create: `internal/board/report.go`, `internal/board/why.go`, `internal/board/report_test.go`
- Modify: `internal/board/http.go`

**Interfaces:**
**Consumes:** `board.StartRun` from Task 9 — do not re-declare it here.

- Produces:
  - `board.Report(ctx, db, clock, runID, nodeID string, req ReportRequest) (ReportResult, error)`
  - `type ReportResult struct { Applied bool; ItemStatus string }`
  - `board.Explain(ctx, db, clock, itemID string) (Explanation, error)`

- [ ] **Step 1: Write the failing report tests**

```go
// internal/board/report_test.go
package board

import (
	"context"
	"testing"
)

func TestReportIsIdempotent(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	got, _ := Claim(ctx, db, clock, node, 1)
	runID := got[0].RunID

	if err := StartRun(ctx, db, clock, runID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	first, err := Report(ctx, db, clock, runID, node, ReportRequest{Status: "ok", Summary: "done once"})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if !first.Applied {
		t.Fatal("first report was not applied")
	}

	second, err := Report(ctx, db, clock, runID, node, ReportRequest{Status: "error", Summary: "second try"})
	if err != nil {
		t.Fatalf("second Report: %v", err)
	}
	if second.Applied {
		t.Fatal("second report was applied; it must be a no-op")
	}

	var status, summary string
	if err := db.QueryRow(`SELECT status, result_summary FROM work_items WHERE id=?`, id).Scan(&status, &summary); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "done" || summary != "done once" {
		t.Fatalf("item = %q/%q, want done/done once", status, summary)
	}
}

func TestReportRejectsAWrongNode(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	other := SeedNodeForTest(t, db, clock, "b", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	got, _ := Claim(ctx, db, clock, node, 1)

	if _, err := Report(ctx, db, clock, got[0].RunID, other, ReportRequest{Status: "ok"}); err == nil {
		t.Fatal("a node reported a run it does not own")
	}
}

func TestExplainSaysWhyNothingRan(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	SeedNodeForTest(t, db, clock, "pi", "steward", "reflex")
	SeedEndpointForTest(t, db, clock, "local", "host", 0)
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"gpu"}})

	ex, err := Explain(ctx, db, clock, id)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if ex.Placed {
		t.Fatal("Explain says the item is placeable; nothing provides gpu")
	}
	if ex.BlockedReason == "" {
		t.Fatal("Explain returned no blocked_reason")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/board/ -run 'Report|Explain' -v`
Expected: FAIL — `undefined: Report`.

- [ ] **Step 3: Implement**

```go
// internal/board/report.go
package board

import (
	"context"
	"database/sql"
	"fmt"
)

type ReportRequest struct {
	Status     string `json:"status"`     // "ok" | "error"
	Summary    string `json:"summary"`
	TokensIn   int    `json:"tokens_in"`
	TokensOut  int    `json:"tokens_out"`
	Error      string `json:"error"`
	Transcript string `json:"transcript_path"`
}

type ReportResult struct {
	Applied    bool   `json:"applied"`
	ItemStatus string `json:"item_status"`
}

// Report completes a run. The first report wins; later ones are accepted and
// ignored, so a node that dies before reading the reply cannot double-complete
// an item (spec §15).
func Report(ctx context.Context, db *sql.DB, clock Clock, runID, nodeID string, req ReportRequest) (ReportResult, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ReportResult{}, err
	}
	defer tx.Rollback()

	var itemID, runStatus string
	var attempt int
	if err := tx.QueryRowContext(ctx,
		`SELECT work_item_id, status, attempt FROM runs WHERE id=? AND node_id=?`,
		runID, nodeID).Scan(&itemID, &runStatus, &attempt); err != nil {
		return ReportResult{}, fmt.Errorf("run %s: %w", runID, err)
	}

	if runStatus == "ok" || runStatus == "error" || runStatus == "lost" || runStatus == "abandoned" {
		var itemStatus string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM work_items WHERE id=?`, itemID).Scan(&itemStatus); err != nil {
			return ReportResult{}, err
		}
		return ReportResult{Applied: false, ItemStatus: itemStatus}, nil
	}

	finalRun := "ok"
	itemStatus := "done"
	if req.Status != "ok" {
		finalRun, itemStatus = "error", "failed"
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE runs SET status=?, ended_at=?, input_tokens=?, output_tokens=?, error=?, transcript_path=?
		  WHERE id=?`,
		finalRun, clock.Now(), req.TokensIn, req.TokensOut,
		nullString(req.Error), nullString(req.Transcript), runID); err != nil {
		return ReportResult{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE work_items SET status=?, result_summary=?, claimed_by=NULL, lease_expires_at=NULL, updated_at=?
		  WHERE id=?`,
		itemStatus, nullString(req.Summary), clock.Now(), itemID); err != nil {
		return ReportResult{}, err
	}
	if err := AppendEvent(ctx, tx, clock, "node:"+nodeID, "work_item", itemID,
		"reported", map[string]any{"run": runID, "status": finalRun, "attempt": attempt}); err != nil {
		return ReportResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReportResult{}, err
	}
	return ReportResult{Applied: true, ItemStatus: itemStatus}, nil
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
```

```go
// internal/board/why.go
package board

import (
	"context"
	"database/sql"
	"fmt"
)

// Explanation is the answer to "why is this item not running" (spec §14).
type Explanation struct {
	ItemID        string   `json:"item_id"`
	Status        string   `json:"status"`
	Needs         []string `json:"needs"`
	Placed        bool     `json:"placed"`
	Reason        string   `json:"reason"`
	BlockedReason string   `json:"blocked_reason"`
	Rejects       []Reject `json:"rejects,omitempty"`
}

func Explain(ctx context.Context, db *sql.DB, clock Clock, itemID string) (Explanation, error) {
	var ex Explanation
	var needsJSON string
	if err := db.QueryRowContext(ctx,
		`SELECT id, status, needs FROM work_items WHERE id=?`, itemID).
		Scan(&ex.ItemID, &ex.Status, &needsJSON); err != nil {
		return ex, fmt.Errorf("item %s: %w", itemID, err)
	}
	needs, err := decodeList(needsJSON)
	if err != nil {
		return ex, err
	}
	ex.Needs = needs

	cands, err := LoadCandidates(ctx, db)
	if err != nil {
		return ex, err
	}
	d := Place(WorkItem{ID: ex.ItemID, Needs: needs, Status: ex.Status}, cands)
	ex.Placed = d.OK
	ex.Reason = d.Reason
	ex.Rejects = d.Rejects
	if !d.OK {
		ex.BlockedReason = d.Reason
	}
	return ex, nil
}
```

- [ ] **Step 4: Wire the routes and handlers**

```go
mux.Handle("POST /runs/{id}/start", s.nodeAuth(http.HandlerFunc(s.handleStartRun)))
mux.Handle("POST /runs/{id}/report", s.nodeAuth(http.HandlerFunc(s.handleReport)))
mux.Handle("GET /admin/why/{id}", s.adminAuth(http.HandlerFunc(s.handleWhy)))
```

```go
func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if err := StartRun(r.Context(), s.db, s.clock, runID, nodeIDFrom(r)); err != nil {
		WriteError(w, http.StatusConflict, "start", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	var req ReportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	res, err := Report(r.Context(), s.db, s.clock, r.PathValue("id"), nodeIDFrom(r), req)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "report", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

func (s *Server) handleWhy(w http.ResponseWriter, r *http.Request) {
	ex, err := Explain(r.Context(), s.db, s.clock, r.PathValue("id"))
	if err != nil {
		WriteError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, ex)
}
```

Use `r.PathValue("id")` for the path parameter.

- [ ] **Step 5: Run everything and commit**

```bash
go test ./... && go vet ./...
git add internal/board
git commit -m "feat(board): run start, idempotent report, and /why explanations"
```

---

## Task 11: Agent loop, stub executor, end-to-end test

**Files:**
- Create: `internal/agent/client.go`, `internal/agent/agent.go`, `internal/agent/executor.go`, `internal/agent/stub.go`, `internal/agent/agent_test.go`
- Modify: `cmd/nsm/main.go` (`nsm agent run`)

**Interfaces:**
- Produces:
  - `agent.NewClient(baseURL, token string) *Client` with `Heartbeat`, `Claim`, `Start`, `Report`
  - `type Executor interface { Run(ctx context.Context, item board.WorkItem) (Result, error) }`
  - `type Result struct { Summary string; TokensIn, TokensOut int }`
  - `agent.New(cfg Config) *Agent`, `(*Agent).Run(ctx context.Context) error`

- [ ] **Step 1: Write the failing end-to-end test**

```go
// internal/agent/agent_test.go
package agent

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

// TestAgentTakesAnItemFromQueuedToDone is the test that makes M1 real: a real
// board over HTTP, a real claim transaction, a stub executor, one result.
func TestAgentTakesAnItemFromQueuedToDone(t *testing.T) {
	db, clock := board.OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	nodeID, token, err := board.RegisterNode(context.Background(), db, clock, board.RegisterRequest{
		Name: "a", Role: "executor", ProvidesDeclared: []string{"agentic"},
		ReachClasses: []string{"host", "lan", "wan"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	board.SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")

	itemID, err := board.CreateItem(context.Background(), db, clock, board.CreateItemRequest{
		Title: "t", Brief: "b", Needs: []string{"agentic"},
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	srv := httptest.NewServer(board.NewServer(db, clock, board.HashToken("admin")).Handler())
	defer srv.Close()

	done := make(chan Result, 1)
	exec := StubExecutor{OnRun: func(item board.WorkItem) (Result, error) {
		res := Result{Summary: "stub did it"}
		done <- res
		return res, nil
	}}

	a := New(Config{
		BaseURL:  srv.URL,
		Token:    token,
		NodeID:   nodeID,
		Executor: exec,
		Protocol: 1,
		// These must match what the node can do. The agent re-sends
		// provides_live on every heartbeat, so an empty list here would
		// overwrite the registration and make the node unqualified for
		// its own item.
		Provides: []string{"agentic"},
		Reaches:  []string{"host", "lan", "wan"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go a.Run(ctx)

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("agent never executed the item")
	}

	// The agent reports AFTER the executor returns, so wait for the board to
	// reflect it. A fixed sleep here would be a flake generator.
	deadline := time.Now().Add(3 * time.Second)
	for {
		var st string
		if err := db.QueryRow(`SELECT status FROM work_items WHERE id=?`, itemID).Scan(&st); err != nil {
			t.Fatalf("read item: %v", err)
		}
		if st == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("item still %q after 3s, want done", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	var status, summary string
	if err := db.QueryRow(`SELECT status, result_summary FROM work_items WHERE id=?`, itemID).
		Scan(&status, &summary); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "done" {
		t.Fatalf("item status = %q, want done", status)
	}
	if summary != "stub did it" {
		t.Fatalf("summary = %q", summary)
	}

	var runStatus string
	if err := db.QueryRow(`SELECT status FROM runs WHERE work_item_id=?`, itemID).Scan(&runStatus); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if runStatus != "ok" {
		t.Fatalf("run status = %q, want ok", runStatus)
	}
}
```

- [ ] **Step 2: Confirm the fixture already exists**

`internal/board/testing.go` (Task 3, extended in Task 8) already exports `OpenForTest`,
`SeedNodeForTest`, and `SeedEndpointForTest`, and Task 9 added `StartRun`. Nothing new is needed
here — that is the point of keeping the fixture in a non-`_test.go` file. If `go test
./internal/agent/` reports an undefined `board.OpenForTest`, the earlier task was skipped; go
back and add it rather than reimplementing it here.

- [ ] **Step 3: Run it and watch it fail**

Run: `go test ./internal/agent/ -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 4: Implement the client, executor, and loop**

```go
// internal/agent/client.go
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{baseURL: baseURL, token: token, http: &http.Client{Timeout: 35 * time.Second}}
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, msg)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Heartbeat(ctx context.Context, req board.HeartbeatRequest) (board.HeartbeatResponse, error) {
	var out board.HeartbeatResponse
	err := c.post(ctx, "/nodes/heartbeat", req, &out)
	return out, err
}

func (c *Client) Claim(ctx context.Context, slots int) ([]board.ClaimedItem, error) {
	var out []board.ClaimedItem
	err := c.post(ctx, "/claim", map[string]any{"free_slots": slots}, &out)
	return out, err
}

func (c *Client) Start(ctx context.Context, runID string) error {
	return c.post(ctx, "/runs/"+runID+"/start", map[string]any{}, nil)
}

func (c *Client) Report(ctx context.Context, runID string, req board.ReportRequest) (board.ReportResult, error) {
	var out board.ReportResult
	err := c.post(ctx, "/runs/"+runID+"/report", req, &out)
	return out, err
}
```

```go
// internal/agent/executor.go
package agent

import (
	"context"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

type Result struct {
	Summary   string
	TokensIn  int
	TokensOut int
}

// Executor does the work. AgenticExecutor (M2+) and ReflexExecutor (M6) both
// implement this; StubExecutor is the reference implementation the tests use.
type Executor interface {
	Run(ctx context.Context, item board.WorkItem) (Result, error)
}
```

```go
// internal/agent/stub.go
package agent

import (
	"context"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

type StubExecutor struct {
	OnRun func(item board.WorkItem) (Result, error)
}

func (s StubExecutor) Run(ctx context.Context, item board.WorkItem) (Result, error) {
	if s.OnRun == nil {
		return Result{Summary: "stub"}, nil
	}
	return s.OnRun(item)
}
```

```go
// internal/agent/agent.go
package agent

import (
	"context"
	"log"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

type Config struct {
	BaseURL   string
	Token     string
	NodeID    string
	Executor  Executor
	Protocol  int
	Provides  []string
	Reaches   []string
	Heartbeat time.Duration // default 15s
	Poll      time.Duration // default 25s
}

type Agent struct {
	cfg Config
	c   *Client
}

func New(cfg Config) *Agent {
	if cfg.Heartbeat == 0 {
		cfg.Heartbeat = 15 * time.Second
	}
	if cfg.Poll == 0 {
		cfg.Poll = 25 * time.Second
	}
	return &Agent{cfg: cfg, c: NewClient(cfg.BaseURL, cfg.Token)}
}

// Run is the loop from spec §10: heartbeat, claim, execute, report. It returns
// only when ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	hb := time.NewTicker(a.cfg.Heartbeat)
	defer hb.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if _, err := a.c.Heartbeat(ctx, board.HeartbeatRequest{
			ProvidesLive: a.cfg.Provides,
			ReachClasses: a.cfg.Reaches,
			FreeSlots:    1,
			Protocol:     a.cfg.Protocol,
		}); err != nil {
			log.Printf("heartbeat: %v", err)
		}

		items, err := a.c.Claim(ctx, 1)
		if err != nil {
			log.Printf("claim: %v", err)
		}
		for _, it := range items {
			a.execute(ctx, it)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-hb.C:
		case <-time.After(a.cfg.Poll):
		}
	}
}

func (a *Agent) execute(ctx context.Context, it board.ClaimedItem) {
	if err := a.c.Start(ctx, it.RunID); err != nil {
		log.Printf("start run %s: %v", it.RunID, err)
		return
	}
	res, runErr := a.cfg.Executor.Run(ctx, it.Item)

	req := board.ReportRequest{
		Status:    "ok",
		Summary:   res.Summary,
		TokensIn:  res.TokensIn,
		TokensOut: res.TokensOut,
	}
	if runErr != nil {
		req.Status = "error"
		req.Error = runErr.Error()
	}
	if _, err := a.c.Report(ctx, it.RunID, req); err != nil {
		log.Printf("report run %s: %v", it.RunID, err)
	}
}
```

- [ ] **Step 5: Run the end-to-end test**

Run: `go test ./internal/agent/ -run EndToEnd -v` (or `-run TestAgent`)
Expected: PASS.

- [ ] **Step 6: Wire `nsm agent run`**

Extend `cmd/nsm/main.go`:

```go
case "agent":
	if len(args) < 2 || args[1] != "run" {
		usage(errOut)
		return 2
	}
	return agentRun(args[2:], errOut)
```

with flags `--board`, `--node`, `--provides`, `--reach`, and the token from `NSM_NODE_TOKEN`. Use `StubExecutor` for now with a comment that M2 replaces it with the agentic executor.

- [ ] **Step 7: Run everything and commit**

```bash
go test ./... -count=1 && go vet ./...
git add internal/agent cmd/nsm
git commit -m "feat(agent): heartbeat/claim/report loop with stub executor and end-to-end test"
```

**M1 exit criteria:** the end-to-end test passes; `go test ./... -race` is clean; killing a node mid-run requeues the item (a test worth adding here — advance the clock past the lease and assert a second claim succeeds).

---

## Task 12: `make release` — six artifacts and checksums

**Files:**
- Create: `scripts/build-release.sh`, `Makefile`
- Modify: `.gitignore` (already ignores `dist/`)

**Interfaces:**
- Produces: `dist/nsm-<version>-<os>-<arch>[.exe]` × 6, `dist/SHA256SUMS`, `dist/BUILD-INFO`

- [ ] **Step 1: Write the Makefile**

```make
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test vet race release

build:
	go build -o bin/nsm ./cmd/nsm

test:
	go test ./... -count=1

race:
	go test ./... -race -count=1

vet:
	go vet ./...

release:
	./scripts/build-release.sh $(VERSION)
```

- [ ] **Step 2: Write the release script**

Model it on `b10coincom/scripts/build-release.sh`, keeping the rules that exist because something went wrong once:

```sh
#!/bin/sh
# Build nsm for the six supported targets into dist/, checksum what was built,
# and refuse to call a partial build a release.
#
#   dist/nsm-<version>-<os>-<arch>[.exe]
#   dist/SHA256SUMS
#   dist/BUILD-INFO
#
# Rules:
#   - dist/ starts empty and is swapped in only after EVERYTHING succeeds.
#   - The artifact count must equal the target count.
#   - Every artifact's architecture is asserted with file(1), so "compiled"
#     cannot mean "host build renamed".
#   - CGO is off: pure-Go SQLite is what makes one-command cross-compiling work.
set -eu

version="${1:-dev}"
root="$(cd "$(dirname "$0")/.." && pwd)"
stage="$root/dist.new"
dist="$root/dist"

targets="darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64"
count=0
for t in $targets; do count=$((count + 1)); done

rm -rf "$stage"
mkdir -p "$stage"

build_one() {
	os="$1"; arch="$2"
	out="$stage/nsm-$version-$os-$arch"
	if [ "$os" = "windows" ]; then
		out="$out.exe"
	fi
	( cd "$root" && env CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
		go build -trimpath \
			-ldflags "-s -w -X github.com/cti97/the-nightshift-manager/internal/version.Version=$version" \
			-o "$out" ./cmd/nsm )
}

for t in $targets; do
	os="${t%/*}"; arch="${t#*/}"
	build_one "$os" "$arch"
done

built=$(find "$stage" -type f -name 'nsm-*' | wc -l | tr -d ' ')
[ "$built" -eq "$count" ] || { echo "built $built artifacts, expected $count" >&2; exit 1; }

# The host artifact must actually run and name its version.
host="$stage/nsm-$version-$(go env GOOS)-$(go env GOARCH)"
[ -x "$host" ] || { echo "host artifact missing: $host" >&2; exit 1; }
"$host" version | grep -q "$version" || { echo "host artifact does not report $version" >&2; exit 1; }

# Architecture assertion: format and arch tokens, in any order (Apple's file(1)
# and libmagic word the same Mach-O differently).
for t in $targets; do
	os="${t%/*}"; arch="${t#*/}"
	f="$stage/nsm-$version-$os-$arch"
	if [ "$os" = "windows" ]; then
		f="$f.exe"
	fi
	desc=$(file -b "$f")
	case "$os/$arch" in
		darwin/amd64)  echo "$desc" | grep -q 'Mach-O' && echo "$desc" | grep -qiE 'x86[-_]64' || { echo "bad arch: $f: $desc" >&2; exit 1; } ;;
		darwin/arm64)  echo "$desc" | grep -q 'Mach-O' && echo "$desc" | grep -qiE 'arm64|aarch64' || { echo "bad arch: $f: $desc" >&2; exit 1; } ;;
		linux/amd64)   echo "$desc" | grep -q 'ELF'    && echo "$desc" | grep -qiE 'x86[-_]64' || { echo "bad arch: $f: $desc" >&2; exit 1; } ;;
		linux/arm64)   echo "$desc" | grep -q 'ELF'    && echo "$desc" | grep -qiE 'arm64|aarch64' || { echo "bad arch: $f: $desc" >&2; exit 1; } ;;
		windows/amd64) echo "$desc" | grep -q 'PE32+'  && echo "$desc" | grep -qiE 'x86[-_]64' || { echo "bad arch: $f: $desc" >&2; exit 1; } ;;
		windows/arm64) echo "$desc" | grep -q 'PE32+'  && echo "$desc" | grep -qiE 'arm64|aarch64' || { echo "bad arch: $f: $desc" >&2; exit 1; } ;;
	esac
done

# shasum is macOS, sha256sum is Linux; the CI runner and the laptop disagree.
if command -v shasum >/dev/null 2>&1; then
	( cd "$stage" && shasum -a 256 nsm-* > SHA256SUMS )
else
	( cd "$stage" && sha256sum nsm-* > SHA256SUMS )
fi
( cd "$stage" && { echo "version: $version"; go version; echo "targets: $targets"; } > BUILD-INFO )

rm -rf "$dist"
mv "$stage" "$dist"
echo "released $version: $count artifacts in dist/"
```

Make it executable: `chmod +x scripts/build-release.sh`.

- [ ] **Step 3: Run it**

```bash
make release
ls dist/
cat dist/SHA256SUMS
```

Expected: 6 `nsm-*` files (one `.exe`), `SHA256SUMS`, `BUILD-INFO`.

- [ ] **Step 4: Verify the Linux artifact really is Linux ARM64**

```bash
file dist/nsm-dev-linux-arm64
dist/nsm-dev-linux-arm64 version || echo "cannot execute here (expected on macOS)"
```

Expected: an ELF aarch64 description; execution is expected to fail on macOS, which is why the *host* artifact is the one asserted to run.

- [ ] **Step 5: Commit**

```bash
git add Makefile scripts/build-release.sh
git commit -m "build: six-target release matrix with checksums and arch assertions"
```

**M1 complete.** `make release` produces the matrix, `make test` is green with `-race`, and the claim/lease/report path is proven end to end over HTTP.

---

## Successor plans (M2–M6)

Not written yet, deliberately. Each depends on interfaces that M0/M1 creates.

| Milestone | Tasks it will contain |
|---|---|
| **M2** providers | Registry + `Provider` interface; `stub`, `openai`, `anthropic`, `ollama` providers; error taxonomy mapping; token estimation; conformance suite against a fake server; endpoint/model CRUD; health probing with `warming`/`auth_failed`; sandbox; six tools; `AgenticExecutor`; cost accounting and the daily ceiling |
| **M3** escalation | `request_approval` tool; `needs_approval` transition; approvals API; approvals page |
| **M4** watches | `watches` table migration; cron/interval/condition scheduler; dedupe window; cooldown; template emission |
| **M5** threads | `threads`/`messages` migration; reserve-then-settle budgets; `turn_owner` alternation; thread page |
| **M6** steward | `ReflexExecutor`; steward registration and dynamic `lan:*` capabilities; endpoint canary; read-only replica; `nsm agent install` for launchd/systemd/Task Scheduler; `/dist` channel with pinned-key verification |

The order is not negotiable in one respect only: M2's sandbox must land before any item can run
against a real model, because the first thing a real model will do is try to read a file.

## Verification for the whole plan

Before declaring M1 done, these must be true and shown:

```bash
go test ./... -count=1          # green
go test ./... -race -count=1    # green, no races
go vet ./...                    # clean
make release && ls dist/        # 6 artifacts + SHA256SUMS + BUILD-INFO

# The manager needs two credentials, and they must differ: the admin token drives
# the admin API, the enrollment token can only register a node.
NSM_ADMIN_TOKEN=dev NSM_ENROLL_TOKEN=enroll ./bin/nsm manager serve --db /tmp/nsm.db &
curl -s localhost:8081/version   # version JSON, no token required

# Enrollment is authenticated: the node gets its own token, shown once.
NSM_ENROLL_TOKEN=enroll ./bin/nsm node register \
    --name laptop --role executor --provides agentic --reach host,lan
NSM_NODE_TOKEN=<printed token> ./bin/nsm agent run \
    --node <printed node id> --provides agentic --reach host,lan
```

And one manual proof that the design claim holds: register two nodes with the enrollment token,
create one item, start one agent on each, and confirm exactly one `run` row exists for the item.

---

## Corrections applied during execution

This plan was written before it was run. Running it found defects in the plan itself, not just in
the code. They are recorded here rather than silently edited, because a plan that has been
quietly rewritten teaches nobody anything.

| Where | The plan said | What was true | Consequence if unnoticed |
|---|---|---|---|
| Task 3, `git add` line | commit the four named files | `events_test.go` calls `OpenForTest` from `testing.go`, which the `git add` omitted | a commit that does not build |
| Task 2, `schema.sql` header comment | "no statement contains `;`" | that comment contained three `;`, and the splitter shredded it | migration 1 fails on every first open |
| Task 5, six tests | six tests cover the eligibility rules | four of nine rules and the tie-break were unexercised; a WAN candidate for a `no_egress` item dies at rule 1, so rule 4 was never reached | a rule can be deleted with the suite green |
| Task 6, zero-fill list | six statuses | the schema permits seven (`cancelled` was missing) | response shape changes with the data |
| Task 8, `StartRun` | defined in Task 10 | the reaper needs it in Task 9; Task 10 also re-declared it | duplicate declaration |
| Task 9, `Advance(leaseSeconds + 1)` | 301 seconds | 301 **nanoseconds**; `FixedClock.Advance` truncates | reaper tests that never expire a lease |
| Task 9, attempt arithmetic | "requeue with `attempt+1`" | the claim already increments; the spec's prose and pseudocode contradicted each other | abandoned claims burn budget, or the item becomes unclaimable |
| Task 10, `Executor.Run` | takes `board.WorkItem` | `Brief` lives on `ClaimedItem`; `WorkItem` is deliberately prose-free | a worker that cannot read its own brief |
| Task 12, arch `case` | explicit arms per target | no `*)` catch-all, so an unknown target exits 0, ships unasserted, and drops another platform | a successful-looking release missing a platform |
| Task 12, `[ ... ] && ...` under `set -e` | used for the `.exe` suffix | a failing `&&` list exits the script on every non-Windows target | release aborts on target 1 |
| Global constraints | `go 1.24` | `go mod tidy` writes `go 1.24.0`; the newest `modernc.org/sqlite` requires 1.26 | the floor silently rises |

Two patterns are worth carrying into the next plan:

1. **Enumerations are wrong by the time you finish writing them.** Status lists, capability lists,
   case arms, and attempt arithmetic were each correct for the states the author remembered and
   wrong for at least one they forgot. Where an enumeration can be derived from the schema or the
   constant beside it, derive it.
2. **A test can be green for a reason other than the one it names.** Rule ordering makes an early
   rule shadow a later one; a single-connection pool serialises the concurrency the test claims to
   prove; a `path.Clean` upstream made a trim test pass with trimming broken. Mutation testing —
   delete the thing and confirm the test dies — was the only reliable way to tell the difference.
