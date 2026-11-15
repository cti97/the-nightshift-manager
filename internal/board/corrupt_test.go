// internal/board/corrupt_test.go
package board

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"strings"
	"testing"
)

// captureLog redirects the process log for one test. A skipped corrupt row is
// reported through log.Printf (the board has no logger abstraction), so this is
// the only way to observe it — and observing it is the requirement: a skipped
// row must not be silent, because the operator has to learn that their database
// has a corrupt row.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// The M1 review's finding, exactly: one malformed row must not 500 every node's
// claim. The corrupt item is given a higher priority so it is first in the
// claim's scan order: the claim has to walk past it, not merely never reach it.
//
// The corrupt row is left queued and logged, and a second claim re-scans it
// without logging it again. That last assertion is the "no event per attempt"
// half of the requirement: a permanently corrupt row on a busy board must not
// produce a line per claim attempt.
func TestClaimSkipsACorruptRowAndStillClaimsTheNext(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	logged := captureLog(t)

	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")

	corruptID, err := CreateItem(ctx, db, clock, CreateItemRequest{
		Title: "corrupt row", Brief: "b", Needs: []string{"agentic"}, Priority: 10,
	})
	if err != nil {
		t.Fatalf("CreateItem (corrupt): %v", err)
	}
	validID, err := CreateItem(ctx, db, clock, CreateItemRequest{
		Title: "valid row", Brief: "b", Needs: []string{"agentic"},
	})
	if err != nil {
		t.Fatalf("CreateItem (valid): %v", err)
	}
	// A well-formed JSON value of the wrong shape: what a hand-edited or
	// half-migrated row looks like.
	if _, err := db.ExecContext(ctx,
		`UPDATE work_items SET needs='{"not":"a list"}' WHERE id=?`, corruptID); err != nil {
		t.Fatalf("corrupt the needs column: %v", err)
	}

	got, err := Claim(ctx, db, clock, node, 2)
	if err != nil {
		t.Fatalf("Claim aborted on one corrupt row: %v (one bad cell must not 500 every node's claim)", err)
	}
	if len(got) != 1 {
		t.Fatalf("claimed %d items, want only the valid one %s", len(got), validID)
	}
	if got[0].Item.ID != validID {
		t.Fatalf("claimed item %s, want the valid item %s", got[0].Item.ID, validID)
	}

	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM work_items WHERE id=?`, corruptID).Scan(&status); err != nil {
		t.Fatalf("read corrupt item: %v", err)
	}
	if status != "queued" {
		t.Fatalf("corrupt item status = %q, want queued: a row the board could not read must be left alone, not failed or cancelled", status)
	}

	out := logged.String()
	if !strings.Contains(out, corruptID) {
		t.Fatalf("corrupt row %s was skipped silently; log output: %q", corruptID, out)
	}
	if !strings.Contains(out, "needs") {
		t.Fatalf("skip report %q does not name the corrupt column (needs)", out)
	}

	// Second claim: the valid item is gone, the corrupt row is re-scanned and
	// skipped again. It must be reported once, not once per attempt.
	if _, err := Claim(ctx, db, clock, node, 2); err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if n := strings.Count(logged.String(), corruptID); n != 1 {
		t.Fatalf("corrupt row reported %d times across two claims, want 1 (one line per distinct corruption)", n)
	}
}

// The M1 review named a "malformed capability cell", which lives on a node or
// model row, not on a work item. A healthy node must still be able to claim when
// another node's live-capability cell is corrupt, and the corrupt node itself
// must get an empty answer rather than a 500.
func TestClaimSkipsACorruptCandidateRowInsteadOfFailingEveryNode(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	logged := captureLog(t)

	nodeA := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	nodeB := SeedNodeForTest(t, db, clock, "b", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")

	if _, err := db.ExecContext(ctx, `UPDATE nodes SET provides_live='not-json' WHERE id=?`, nodeB); err != nil {
		t.Fatalf("corrupt node B's provides_live: %v", err)
	}
	for _, title := range []string{"first", "second"} {
		if _, err := CreateItem(ctx, db, clock, CreateItemRequest{Title: title, Brief: "b", Needs: []string{"agentic"}}); err != nil {
			t.Fatalf("CreateItem(%s): %v", title, err)
		}
	}

	// A, whose own rows are fine, must claim both. Before the fix this call
	// aborted because B's row was in the same cross join.
	got, err := Claim(ctx, db, clock, nodeA, 2)
	if err != nil {
		t.Fatalf("healthy node A's claim failed because node B's row is corrupt: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("healthy node claimed %d items, want 2", len(got))
	}

	// B's own candidate rows are skipped, so B claims nothing — and it gets no
	// error, because "your capability cell is unreadable" is not a board outage.
	thirdID, err := CreateItem(ctx, db, clock, CreateItemRequest{Title: "third", Brief: "b", Needs: []string{"agentic"}})
	if err != nil {
		t.Fatalf("CreateItem(third): %v", err)
	}
	got, err = Claim(ctx, db, clock, nodeB, 1)
	if err != nil {
		t.Fatalf("corrupt node B's claim failed instead of skipping its row: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("corrupt node claimed %d items, want 0 (its capabilities are unreadable)", len(got))
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM work_items WHERE id=?`, thirdID).Scan(&status); err != nil {
		t.Fatalf("read third item: %v", err)
	}
	if status != "queued" {
		t.Fatalf("third item status = %q, want queued", status)
	}

	out := logged.String()
	if !strings.Contains(out, nodeB) || !strings.Contains(out, "provides_live") {
		t.Fatalf("corrupt candidate row was not reported with its row id and column; log output: %q", out)
	}
}

// The model half of the same bug: a corrupt features cell on the cheapest model
// must remove that row from placement, not abort the claim or (worse) treat the
// model as featureless-and-still-eligible.
func TestClaimSkipsACorruptModelRowAndUsesAnotherEndpoint(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	logged := captureLog(t)

	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "cheap", "host", 0, "tools")
	SeedEndpointForTest(t, db, clock, "spare", "host", 1, "tools")
	if _, err := db.ExecContext(ctx, `UPDATE models SET features='not-json' WHERE id='m-cheap'`); err != nil {
		t.Fatalf("corrupt the cheap model's features: %v", err)
	}
	if _, err := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	got, err := Claim(ctx, db, clock, node, 1)
	if err != nil {
		t.Fatalf("Claim aborted on one corrupt model row: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("claimed %d items, want 1 on the healthy endpoint", len(got))
	}
	if got[0].Endpoint.ID != "ep-spare" {
		t.Fatalf("claimed endpoint %s, want ep-spare (the corrupt model row must be skipped, not used)", got[0].Endpoint.ID)
	}
	if out := logged.String(); !strings.Contains(out, "m-cheap") || !strings.Contains(out, "features") {
		t.Fatalf("corrupt model row was not reported with its row id and column; log output: %q", out)
	}
}

// The boundary Task 13 handed over, pinned so a later "make the claim path
// tolerant" change cannot widen the skip to configuration that gates safety.
// A corrupt daily_spend_ceiling must keep failing the claim CLOSED: skipping it
// would silently mean "unlimited spend", which is the opposite of safe.
func TestClaimFailsClosedOnACorruptSpendCeiling(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()

	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	itemID, err := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO config (key,value) VALUES (?,?)`, configDailySpendCeiling, "ten dollars"); err != nil {
		t.Fatalf("write a corrupt ceiling: %v", err)
	}

	got, err := Claim(ctx, db, clock, node, 1)
	if err == nil {
		t.Fatalf("Claim succeeded with a corrupt %s and returned %d items; skipping the ceiling would silently mean unlimited spend",
			configDailySpendCeiling, len(got))
	}
	if !strings.Contains(err.Error(), configDailySpendCeiling) {
		t.Fatalf("Claim error %q does not name the corrupt key %q", err, configDailySpendCeiling)
	}

	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM work_items WHERE id=?`, itemID).Scan(&status); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "queued" {
		t.Fatalf("item status = %q after a refused claim, want queued", status)
	}
	var runs int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("%d runs were written by a claim that failed closed, want 0", runs)
	}
}

// The skip is scoped to the claim path. /why exists to explain a board; when a
// candidate row cannot be read, swallowing it would hide a corrupt row from the
// one endpoint whose job is to surface exactly that. It keeps answering 500 and
// names the column.
func TestWhyStillFailsLoudlyOnACorruptCandidateRow(t *testing.T) {
	srv, clock := newServer(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, srv.db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, srv.db, clock, "local", "host", 0, "tools")
	itemID, err := CreateItem(ctx, srv.db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := srv.db.ExecContext(ctx, `UPDATE nodes SET provides_live='not-json' WHERE id=?`, node); err != nil {
		t.Fatalf("corrupt provides_live: %v", err)
	}

	rec := do(t, srv.Handler(), "GET", "/admin/why/"+itemID, testAdminToken)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("why on a corrupt candidate row = %d, want 500 (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "provides_live") {
		t.Fatalf("why error %s does not name the offending column", rec.Body)
	}
}
