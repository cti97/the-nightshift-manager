// internal/board/threads_test.go
package board

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
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
		{"no subject", CreateThreadRequest{BudgetTokens: 100}, "subject"},
		{"zero budget", CreateThreadRequest{Subject: "x", BudgetTokens: 0}, "budget_tokens must be positive"},
		{"negative budget", CreateThreadRequest{Subject: "x", BudgetTokens: -1}, "budget_tokens must be positive"},
		{"zero max output", CreateThreadRequest{Subject: "x", BudgetTokens: 100, MaxOutputTokens: &zero}, "max_output_tokens must be positive"},
		{"negative max output", CreateThreadRequest{Subject: "x", BudgetTokens: 100, MaxOutputTokens: &negative}, "max_output_tokens"},
		{"zero turns", CreateThreadRequest{Subject: "x", BudgetTokens: 100, MaxTurns: &zero}, "max_turns must be positive"},
		{"unknown item", CreateThreadRequest{Subject: "x", BudgetTokens: 100, WorkItemID: "nope"}, "does not exist"},
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
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 1000})
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
	// The reservation for "hi" is estimateTokens("hi")+maxOutput = 1+64 = 65 with a 64-token
	// output ceiling (the estimate is 1+len/4, and len("hi")/4 is 0). A budget of 64 is one
	// short; 65 is exact.
	for _, c := range []struct {
		name    string
		budget  int
		wantErr bool
	}{
		{"one short", 64, true},
		{"exact", 65, false},
	} {
		db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
		out := 64
		th, err := CreateThread(ctx0, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: c.budget, MaxOutputTokens: &out})
		if err != nil {
			t.Fatalf("%s: CreateThread: %v", c.name, err)
		}
		_, err = PostMessage(ctx0, db, clock, th.ID, PostMessageRequest{Author: "a", Body: "hi"})
		switch {
		case c.wantErr && err == nil:
			t.Fatalf("%s: the post was accepted with a budget of %d", c.name, c.budget)
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
	out := 100
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 50, MaxOutputTokens: &out})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "a", Body: "x"}); !errors.Is(err, ErrThreadFrozen) {
		t.Fatalf("first post = %v, want ErrThreadFrozen (reserve 102 > budget 50)", err)
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
	out := 100
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 50, MaxOutputTokens: &out})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "a", Body: "x"}); !errors.Is(err, ErrThreadFrozen) {
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
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "a", Body: "x"}); err != nil {
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
	out := 100
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 50, MaxOutputTokens: &out})
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
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "a", Body: "x"}); !errors.Is(err, ErrThreadFrozen) {
		t.Fatalf("first post = %v", err)
	}
	if got := count(); got != 1 {
		t.Fatalf("frozen events = %d after the freeze, want 1", got)
	}
	for i := 0; i < 3; i++ {
		if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "b", Body: "x"}); !errors.Is(err, ErrThreadFrozen) {
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
	out := 100
	th, err := CreateThread(ctx, db, clock, CreateThreadRequest{Subject: "s", BudgetTokens: 50, MaxOutputTokens: &out})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := PostMessage(ctx, db, clock, th.ID, PostMessageRequest{Author: "a", Body: "x"}); !errors.Is(err, ErrThreadFrozen) {
		t.Fatalf("the thread did not freeze: %v", err)
	}
	small := 60
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

	// A closed thread's budget is not a lever: the conversation is over.
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
}
