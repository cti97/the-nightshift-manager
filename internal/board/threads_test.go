// internal/board/threads_test.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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

// TestCreateThreadStoresTheBudget: the defaults are the board's, not the caller's, and the
// row carries them so the reservation is computable from the row alone.
func TestCreateThreadStoresTheBudget(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()

	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "cache policy", BudgetTokens: 5000})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if th.Status != ThreadOpen || th.MaxOutputTokens != defaultMaxOutput || th.MaxTurns != defaultMaxTurns {
		t.Fatalf("thread = %+v, want open with the defaults", th)
	}
	view, err := GetThread(ctx, db, th.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if view.BudgetTokens != 5000 || view.Remaining != 5000 {
		t.Fatalf("budget %d remaining %d, want 5000/5000", view.BudgetTokens, view.Remaining)
	}
	if view.Messages == nil {
		t.Fatal("a thread with no messages returned nil, want []")
	}
	if view.TurnOwner != "" || view.ClosedAt != nil {
		t.Fatalf("a fresh thread has an owner or a close time: %+v", view)
	}
}

// TestThreadBudgetIsValidated: every refusal names the field and creates nothing.
func ptrInt(v int) *int { return &v }

func TestThreadBudgetIsValidated(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	zero := 0
	negative := -5

	cases := []struct {
		name string
		req  CreateThreadRequest
		want string
	}{
		{"no subject", CreateThreadRequest{BudgetTokens: 5000}, "subject"},
		{"subject too long", CreateThreadRequest{Subject: strings.Repeat("s", maxSubjectBytes+1), BudgetTokens: 5000}, "subject is a line"},
		{"zero budget", CreateThreadRequest{Subject: "x", BudgetTokens: 0}, "budget_tokens must be positive"},
		{"negative budget", CreateThreadRequest{Subject: "x", BudgetTokens: -1}, "budget_tokens must be positive"},
		{"budget past the limit", CreateThreadRequest{Subject: "x", BudgetTokens: maxBudgetTokens + 1}, "past the board's limit"},
		{"zero max output", CreateThreadRequest{Subject: "x", BudgetTokens: 100, MaxOutputTokens: &zero}, "max_output_tokens must be positive"},
		{"negative max output", CreateThreadRequest{Subject: "x", BudgetTokens: 100, MaxOutputTokens: &negative}, "max_output_tokens must be positive"},
		{"max output past the limit", CreateThreadRequest{Subject: "x", BudgetTokens: maxBudgetTokens, MaxOutputTokens: ptrInt(maxOutputTokens + 1)}, "past the board's limit"},
		{"zero turns", CreateThreadRequest{Subject: "x", BudgetTokens: 100, MaxTurns: &zero}, "max_turns must be positive"},
		{"turns past the limit", CreateThreadRequest{Subject: "x", BudgetTokens: 5000, MaxTurns: ptrInt(maxTurnsLimit + 1)}, "past the board's limit"},
		{"budget cannot hold one turn", CreateThreadRequest{Subject: "x", BudgetTokens: 1000}, "cannot hold a single turn"},
		{"unknown item", CreateThreadRequest{Subject: "x", BudgetTokens: 5000, WorkItemID: "nope"}, "does not exist"},
	}
	for _, c := range cases {
		if _, err := CreateThread(ctx, db, clock, c.req); err == nil {
			t.Fatalf("%s: accepted", c.name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %q, want %q", c.name, err, c.want)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("threads = %d after refusals, want none", n)
	}
}

// TestGetThreadReturnsMessagesInOrder: oldest first, and the read is a read — nothing on
// the row moves.
func TestGetThreadReturnsMessagesInOrder(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 5000})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	before, err := GetThread(ctx, db, th.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	// Inserted directly: posting is Task 3's subject, and this test is about the read.
	// The ids deliberately do not sort in posting order: ids are random tokens in
	// production, and a fixture whose ids happen to match insertion order would let an
	// ORDER BY id mutation pass.
	for i, m := range []struct {
		id     string
		author string
		body   string
		at     int64
	}{
		{"c-id", "agent-a", "first", clock.Now()},
		{"a-id", "agent-b", "second", clock.Now()},
		{"b-id", "agent-a", "third", clock.Now()},
	} {
		if _, err := db.Exec(`INSERT INTO messages (id,thread_id,author,body,tokens_charged,created_at)
		                      VALUES (?,?,?,?,?,?)`, m.id, th.ID, m.author, m.body, 10, m.at); err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
	}
	view, err := GetThread(ctx, db, th.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	var bodies []string
	for _, m := range view.Messages {
		bodies = append(bodies, m.Body)
	}
	if !slices.Equal(bodies, []string{"first", "second", "third"}) {
		t.Fatalf("messages = %v, want insertion order", bodies)
	}
	after, err := GetThread(ctx, db, th.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if after.TokensUsed != before.TokensUsed || after.TokensReserved != before.TokensReserved || after.Status != before.Status {
		t.Fatalf("the read changed the thread: before %+v after %+v", before.Thread, after.Thread)
	}
	if _, err := GetThread(ctx, db, "nope"); !errors.Is(err, ErrNoThread) {
		t.Fatalf("a missing thread gave %v, want ErrNoThread", err)
	}
}

// TestAMessageReservesAndSettles: the first post charges its reservation (no report), the
// thread's counters move, and the message is readable.
func TestAMessageReservesAndSettles(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 100_000})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	res, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "agent-a", Body: "hello"})
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if res.Charged != res.Reserved {
		t.Fatalf("charged %d reserve %d: an unreported post settles at its reservation", res.Charged, res.Reserved)
	}
	if res.Reserved != estimateTokens("hello")+defaultMaxOutput {
		t.Fatalf("reserve = %d, want prompt %d + output %d", res.Reserved, estimateTokens("hello"), defaultMaxOutput)
	}
	view, err := GetThread(ctx, db, th.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if view.TokensUsed != res.Charged || view.TurnsUsed != 1 || view.TurnOwner != "agent-a" {
		t.Fatalf("thread after post = %+v, want the charge and the owner", view.Thread)
	}
	if view.TokensReserved != 0 {
		t.Fatalf("tokens_reserved = %d after the post, want 0: the settle is in the same transaction", view.TokensReserved)
	}
	if len(view.Messages) != 1 || view.Messages[0].TokensCharged != res.Charged {
		t.Fatalf("messages = %+v, want one carrying the charge", view.Messages)
	}
}

// TestSettlementReleasesTheDifference: a reported 10 against a 1000-token reservation
// charges 10, and the rest of the budget is available again. That release is the whole
// point of reserving.
func TestSettlementReleasesTheDifference(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 100_000})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	ten := 10
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "agent-a", Body: "hi", Tokens: &ten}); err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	view, err := GetThread(ctx, db, th.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if view.TokensUsed != 10 {
		t.Fatalf("tokens_used = %d, want the reported 10", view.TokensUsed)
	}
	if view.Remaining != 100_000-10 {
		t.Fatalf("remaining = %d, want the reservation released", view.Remaining)
	}
}

// TestTheThreadFreezesAtTheReservationBoundary is §17's third test: one token short of the
// boundary does not freeze, exactly at the boundary refuses. "Not before and not after" is
// the property — an off-by-one here either freezes a thread with room or lets one exceed.
func TestTheThreadFreezesAtTheReservationBoundary(t *testing.T) {
	ctx0 := context.Background()
	// The 64-token ceiling makes each short message reserve 65 (1 prompt token + 64). The
	// first message fits exactly at 65; the second needs 66 more, so the boundary is the
	// budget where 65 (used) + 66 (reserve) lands: 130 refuses, 131 accepts. "Not before and
	// not after" is the property — an off-by-one either freezes a thread with room or lets one
	// exceed.
	for _, c := range []struct {
		name    string
		budget  int
		wantErr bool
	}{
		{"one short", 130, true},
		{"exact", 131, false},
	} {
		db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
		out := 64
		th, err := CreateThread(ctx0, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: c.budget, MaxOutputTokens: &out})
		if err != nil {
			t.Fatalf("%s: CreateThread: %v", c.name, err)
		}
		if _, err := PostMessage(ctx0, db, clock, th.ID, PostMessageRequest{Author: "a", Body: "hi"}); err != nil {
			t.Fatalf("%s: the first message was refused: %v", c.name, err)
		}
		_, err = PostMessage(ctx0, db, clock, th.ID, PostMessageRequest{Author: "b", Body: "hi"})
		switch {
		case c.wantErr && err == nil:
			t.Fatalf("%s: the second message was accepted with a budget of %d", c.name, c.budget)
		case c.wantErr && !errors.Is(err, ErrThreadFrozen):
			t.Fatalf("%s: err = %v, want ErrThreadFrozen", c.name, err)
		case !c.wantErr && err != nil:
			t.Fatalf("%s: the post was refused at the boundary: %v", c.name, err)
		}
		view, err := GetThread(ctx0, db, th.ID)
		if err != nil {
			t.Fatalf("%s: GetThread: %v", c.name, err)
		}
		wantStatus := ThreadOpen
		if c.wantErr {
			wantStatus = ThreadFrozen
		}
		if view.Status != wantStatus {
			t.Fatalf("%s: status = %q, want %q", c.name, view.Status, wantStatus)
		}
	}
}

// TestASilentPosterIsChargedTheReserve: not reporting is not a refund (D5).
func TestASilentPosterIsChargedTheReserve(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 100_000})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	res, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "a", Body: "x"})
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	view, err := GetThread(ctx, db, th.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if view.TokensUsed != res.Reserved || view.TokensUsed == 0 {
		t.Fatalf("tokens_used = %d, want the reserve %d", view.TokensUsed, res.Reserved)
	}
}

// TestAnAgentCannotPostTwice: alternation refuses the second consecutive message from the
// same agent, and the refusal writes nothing.
func TestAnAgentCannotPostTwice(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 100_000})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "agent-a", Body: "one"}); err != nil {
		t.Fatalf("first post: %v", err)
	}
	_, err = PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "agent-a", Body: "two"})
	if !errors.Is(err, ErrSameAuthor) {
		t.Fatalf("err = %v, want ErrSameAuthor", err)
	}
	view, err := GetThread(ctx, db, th.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if len(view.Messages) != 1 || view.TurnsUsed != 1 {
		t.Fatalf("messages %d turns %d after a refused post, want 1 and 1", len(view.Messages), view.TurnsUsed)
	}
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "agent-b", Body: "two"}); err != nil {
		t.Fatalf("the other agent could not reply: %v", err)
	}
}

// TestAdminMayPostTwice: the exemption in D3, because a human correcting themselves is not
// a loop.
func TestAdminMayPostTwice(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 100_000})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	for _, body := range []string{"first", "second"} {
		if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "admin", Body: body}); err != nil {
			t.Fatalf("admin post %q: %v", body, err)
		}
	}
}

// TestTheLastTurnClosesTheThread: running out of turns ends the conversation (409) rather
// than freezing it, and the row says so.
func TestTheLastTurnClosesTheThread(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	two := 2
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 100_000, MaxTurns: &two})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "a", Body: "one"}); err != nil {
		t.Fatalf("post 1: %v", err)
	}
	res, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "b", Body: "two"})
	if err != nil {
		t.Fatalf("post 2: %v", err)
	}
	if res.Thread.Status != ThreadClosed || res.Thread.ClosedAt == nil {
		t.Fatalf("after the last turn status = %q closed_at %v, want closed", res.Thread.Status, res.Thread.ClosedAt)
	}
	_, err = PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "a", Body: "three"})
	if !errors.Is(err, ErrThreadClosed) {
		t.Fatalf("a post to a closed thread gave %v, want ErrThreadClosed", err)
	}
}

// TestTheReservationGrowsWithTheConversation: the prompt is the whole thread, not just the
// new message. That is why a long conversation freezes sooner than its message count
// suggests, and it is the difference between a budget that bounds context and one that
// bounds turns.
func TestTheReservationGrowsWithTheConversation(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	out := 10
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 100_000, MaxOutputTokens: &out})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	long := strings.Repeat("a", 200)
	first, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "agent-a", Body: long})
	if err != nil {
		t.Fatalf("first post: %v", err)
	}
	if want := estimateTokens(long) + out; first.Reserved != want {
		t.Fatalf("first reserve = %d, want the message alone (%d)", first.Reserved, want)
	}

	second, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "agent-b", Body: "hi"})
	if err != nil {
		t.Fatalf("second post: %v", err)
	}
	if want := estimateTokens(long) + estimateTokens("hi") + out; second.Reserved != want {
		t.Fatalf("second reserve = %d, want the conversation plus the new message (%d)", second.Reserved, want)
	}
}

// TestAFrozenThreadRefusesMessages: every post is refused with ErrThreadFrozen until the
// budget moves, and the refusal says so.
func TestAFrozenThreadRefusesMessages(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	out := 64
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 65, MaxOutputTokens: &out})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "a", Body: "hi"}); err != nil {
		t.Fatalf("the first message was refused: %v", err)
	}
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "b", Body: "hi"}); !errors.Is(err, ErrThreadFrozen) {
		t.Fatalf("the second message = %v, want ErrThreadFrozen", err)
	}
	for i, author := range []string{"a", "b"} {
		if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: author, Body: "x"}); !errors.Is(err, ErrThreadFrozen) {
			t.Fatalf("post %d to a frozen thread = %v, want ErrThreadFrozen", i, err)
		}
	}
}

// TestRaisingTheBudgetReopensTheThread: a budget that fits reopens it, and the next post
// goes through. Without this a frozen thread would be indistinguishable from a closed one.
func TestRaisingTheBudgetReopensTheThread(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	out := 64
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 65, MaxOutputTokens: &out})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "a", Body: "hi"}); err != nil {
		t.Fatalf("first post: %v", err)
	}
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "b", Body: "hi"}); !errors.Is(err, ErrThreadFrozen) {
		t.Fatalf("the thread did not freeze: %v", err)
	}
	big := 10_000
	patched, err := PatchThread(ctx, db, clock, th.ID, PatchThreadRequest{BudgetTokens: &big})
	if err != nil {
		t.Fatalf("PatchThread: %v", err)
	}
	if patched.Status != ThreadOpen || patched.BudgetTokens != 10_000 {
		t.Fatalf("after the patch = %+v, want open at 10000", patched)
	}
	// The refused post wrote nothing, so the turn owner is still "a": the reply that proves the
	// thread is usable again must come from the other agent.
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "b", Body: "hi"}); err != nil {
		t.Fatalf("the reopened thread refused a post: %v", err)
	}
	// The reopen is an event: the dashboard's history reads events, and a status that
	// changed without one would look like a board that woke up different.
	var opens int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE entity='thread' AND entity_id=? AND type='open'`, th.ID).Scan(&opens); err != nil {
		t.Fatalf("count reopen events: %v", err)
	}
	if opens != 1 {
		t.Fatalf("reopen events = %d, want 1", opens)
	}
}

// TestFreezingWritesExactlyOneEvent: the freeze is one event, and a post to an already
// frozen thread writes nothing at all — a thread that stays frozen must not spam the spine.
func TestFreezingWritesExactlyOneEvent(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	out := 64
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 65, MaxOutputTokens: &out})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	count := func() int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE entity='thread' AND type='frozen'`).Scan(&n); err != nil {
			t.Fatalf("count events: %v", err)
		}
		return n
	}
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "a", Body: "hi"}); err != nil {
		t.Fatalf("first post: %v", err)
	}
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "b", Body: "hi"}); !errors.Is(err, ErrThreadFrozen) {
		t.Fatalf("the second post = %v, want ErrThreadFrozen", err)
	}
	if got := count(); got != 1 {
		t.Fatalf("frozen events = %d after the freeze, want 1", got)
	}
	for i := 0; i < 3; i++ {
		author := []string{"a", "b"}[i%2]
		if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: author, Body: "x"}); !errors.Is(err, ErrThreadFrozen) {
			t.Fatalf("post %d = %v", i, err)
		}
	}
	if got := count(); got != 1 {
		t.Fatalf("frozen events = %d after three refused posts, want still 1", got)
	}
}

// TestPatchNeverContradictsTheThread: a patch to a frozen thread whose numbers still do not
// fit leaves it frozen, and a closed thread is not patchable at all.
func TestPatchNeverContradictsTheThread(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	out := 64
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 130, MaxOutputTokens: &out})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "a", Body: "hi"}); err != nil {
		t.Fatalf("first post: %v", err)
	}
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "b", Body: "hi"}); !errors.Is(err, ErrThreadFrozen) {
		t.Fatalf("the thread did not freeze: %v", err)
	}
	// 65 used + 64 ceiling + 1 prompt token = the cheapest turn is 130; a patch to 129 still
	// cannot hold one, so the thread must stay frozen.
	small := 129
	if _, err := PatchThread(ctx, db, clock, th.ID, PatchThreadRequest{BudgetTokens: &small}); err != nil {
		t.Fatalf("PatchThread: %v", err)
	}
	view, err := GetThread(ctx, db, th.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if view.Status != ThreadFrozen {
		t.Fatalf("status = %q after a patch that does not fit, want frozen", view.Status)
	}

	// The same rule the other way: lowering an open thread's budget below the cheapest turn
	// freezes it, because the status is a function of the numbers rather than a flag kept in
	// step by hand.
	two := 1
	closed, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 10_000, MaxTurns: &two})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := PostMessage(ctx, db, clock, closed.ID, PostMessageRequest{Author: "a", Body: "x"}); err != nil {
		t.Fatalf("the single turn was refused: %v", err)
	}
	if _, err := PatchThread(ctx, db, clock, closed.ID, PatchThreadRequest{BudgetTokens: &small}); !errors.Is(err, ErrThreadClosed) {
		t.Fatalf("patching a closed thread = %v, want ErrThreadClosed", err)
	}

	open, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 10_000, MaxOutputTokens: &out})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	low := 10
	patched, err := PatchThread(ctx, db, clock, open.ID, PatchThreadRequest{BudgetTokens: &low})
	if err != nil {
		t.Fatalf("lowering an open thread: %v", err)
	}
	if patched.Status != ThreadFrozen {
		t.Fatalf("an open thread lowered below one turn is %q, want frozen", patched.Status)
	}
}

// TestTwoPostersCannotBothTakeTheLastOfTheBudget: the reservation check and the insert are one
// BEGIN IMMEDIATE transaction, so two posters racing for the last of the budget cannot both see
// it free. One is served, the other freezes, and the thread never exceeds its budget.
func TestTwoPostersCannotBothTakeTheLastOfTheBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	db, clock := OpenForTest(t, path)
	ctx := context.Background()
	out := 64
	// 65 used by the first message, then room for exactly one 66-token turn.
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 131, MaxOutputTokens: &out})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "a", Body: "hi"}); err != nil {
		t.Fatalf("the first post: %v", err)
	}
	// A second handle on the same file is a second manager process as far as SQLite is
	// concerned.
	db2, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open second handle: %v", err)
	}
	defer db2.Close()

	// A third connection holds the write lock while both posters arrive. That is what makes
	// the overlap deterministic: the posters are certainly inside the same window, and a
	// deferred transaction would let both read the same counters before either writes.
	blocker, err := db2.Conn(ctx)
	if err != nil {
		t.Fatalf("blocker conn: %v", err)
	}
	defer blocker.Close()
	if _, err := blocker.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("blocker begin: %v", err)
	}

	started := make(chan struct{}, 2)
	errs := make(chan error, 2)
	for i, handle := range []*sql.DB{db, db2} {
		go func(i int, handle *sql.DB) {
			started <- struct{}{}
			_, err := PostMessage(ctx, handle, clock, th.ID, PostMessageRequest{
				Author: []string{"b", "c"}[i], Body: "hi",
			})
			errs <- err
		}(i, handle)
	}
	<-started
	<-started
	time.Sleep(150 * time.Millisecond) // both are now waiting on the lock
	if _, err := blocker.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatalf("blocker commit: %v", err)
	}

	first, second := <-errs, <-errs
	succeeded := 0
	for _, err := range []error{first, second} {
		switch {
		case err == nil:
			succeeded++
		case !errors.Is(err, ErrThreadFrozen):
			t.Fatalf("a racing post failed with %v, want ErrThreadFrozen", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d of two racing posts were served, want exactly one", succeeded)
	}
	view, err := GetThread(ctx, db, th.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if view.TokensUsed > view.BudgetTokens {
		t.Fatalf("tokens_used = %d > budget %d: both posters charged", view.TokensUsed, view.BudgetTokens)
	}
	if len(view.Messages) != 2 {
		t.Fatalf("messages = %d, want the first and exactly one of the racing two", len(view.Messages))
	}
}

// TestAnOversizedMessageIsRefused: a thread is not a disk the node does not own. The board caps
// the body itself, so a caller that reaches it another way is bounded the same.
func TestAnOversizedMessageIsRefused(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 100_000})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	huge := strings.Repeat("a", maxMessageBytes+1)
	_, err = PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "a", Body: huge})
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("an oversized body = %v, want a refusal naming the limit", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("messages = %d after an oversized body, want none", n)
	}
}

// TestAnOversizedRequestIsRefusedAtTheDoor: the transport cap answers 413 before the body is
// buffered, so a node cannot exhaust the manager's memory with one request.
func TestAnOversizedRequestIsRefusedAtTheDoor(t *testing.T) {
	f := newThreadFixture(t)
	rec := f.do(t, "POST", "/admin/threads", testAdminToken, CreateThreadRequest{Subject: "s", BudgetTokens: 5000})
	var th Thread
	if err := json.Unmarshal(rec.Body.Bytes(), &th); err != nil {
		t.Fatalf("decode: %v", err)
	}
	huge := strings.Repeat("a", requestBodyLimit+1024)
	body := `{"author":"a","body":"` + huge + `"}`
	req := httptest.NewRequest("POST", "/threads/"+th.ID+"/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.nodeToken)
	rec = httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "body_too_large") {
		t.Fatalf("oversized request: %d %s, want 413 body_too_large", rec.Code, rec.Body.String())
	}
}
