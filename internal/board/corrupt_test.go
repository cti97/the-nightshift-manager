// internal/board/corrupt_test.go
package board

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
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

// A4 replaced the old "one corrupt candidate makes /why a 500" contract: a
// board-wide outage of the explainer is worse than a degraded answer. With one
// corrupt node and one healthy node, the item is still explained and placed,
// and the corrupt row is named in warnings rather than silently removed from
// the candidate set.
func TestWhyDegradesOnACorruptCandidateAndStillExplainsTheItem(t *testing.T) {
	srv, clock := newServer(t)
	ctx := context.Background()
	SeedNodeForTest(t, srv.db, clock, "healthy", "executor", "agentic")
	corrupt := SeedNodeForTest(t, srv.db, clock, "corrupt", "executor", "agentic")
	SeedEndpointForTest(t, srv.db, clock, "local", "host", 0, "tools")
	itemID, err := CreateItem(ctx, srv.db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := srv.db.ExecContext(ctx, `UPDATE nodes SET provides_live='not-json' WHERE id=?`, corrupt); err != nil {
		t.Fatalf("corrupt provides_live: %v", err)
	}

	rec := do(t, srv.Handler(), "GET", "/admin/why/"+itemID, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("why on a corrupt candidate row = %d, want a degraded 200 (body %s)", rec.Code, rec.Body)
	}
	var ex Explanation
	if err := json.Unmarshal(rec.Body.Bytes(), &ex); err != nil {
		t.Fatalf("decode why body: %v", err)
	}
	if !ex.Placed {
		t.Fatal("explanation says the item is unplaceable although the healthy node still provides agentic")
	}
	if len(ex.Warnings) != 1 {
		t.Fatalf("warnings = %+v, want exactly the one corrupt candidate row", ex.Warnings)
	}
	w := ex.Warnings[0]
	if w.Table != "nodes" || w.RowID != corrupt || w.Column != "provides_live" {
		t.Fatalf("warning = %+v, want nodes/%s provides_live", w, corrupt)
	}
	if !strings.Contains(w.Reason, "decode capability list") {
		t.Fatalf("warning reason = %q, want the claim path's own decode error", w.Reason)
	}
	if !strings.Contains(rec.Body.String(), corrupt) || !strings.Contains(rec.Body.String(), "provides_live") {
		t.Fatalf("why body does not name the skipped row and column: %s", rec.Body)
	}
}

// The other half of A4: when the corrupt row is the only candidate, the answer
// must not imply the item simply has no candidate. It says placed:false (never
// placed:true on the strength of a row that was skipped) and names the corruption
// in blocked_reason as well as in warnings.
func TestWhyWithOnlyACorruptCandidateNamesItInsteadOfJustSayingNoCandidate(t *testing.T) {
	srv, clock := newServer(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, srv.db, clock, "only", "executor", "agentic")
	SeedEndpointForTest(t, srv.db, clock, "local", "host", 0, "tools")
	itemID, err := CreateItem(ctx, srv.db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := srv.db.ExecContext(ctx, `UPDATE nodes SET provides_live='not-json' WHERE id=?`, node); err != nil {
		t.Fatalf("corrupt provides_live: %v", err)
	}

	rec := do(t, srv.Handler(), "GET", "/admin/why/"+itemID, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("why with only a corrupt candidate = %d, want a degraded 200 (body %s)", rec.Code, rec.Body)
	}
	var ex Explanation
	if err := json.Unmarshal(rec.Body.Bytes(), &ex); err != nil {
		t.Fatalf("decode why body: %v", err)
	}
	if ex.Placed {
		t.Fatal("explanation says placed:true although the only candidate was skipped as corrupt")
	}
	if len(ex.Warnings) != 1 || ex.Warnings[0].RowID != node || ex.Warnings[0].Column != "provides_live" {
		t.Fatalf("warnings = %+v, want the corrupt node's provides_live row", ex.Warnings)
	}
	if !strings.Contains(ex.BlockedReason, "corrupt candidate row") ||
		!strings.Contains(ex.BlockedReason, node) ||
		!strings.Contains(ex.BlockedReason, "provides_live") {
		t.Fatalf("blocked_reason = %q, want the skipped corrupt row named alongside the placement failure", ex.BlockedReason)
	}
	if !strings.Contains(ex.BlockedReason, "no eligible candidate") {
		t.Fatalf("blocked_reason = %q, want the placement failure kept", ex.BlockedReason)
	}
}

// A2: corrupt rows must be enumerable without log retention or sqlite3. The
// route lists every cell the claim path decodes, one entry per column, and it
// is read-only: the cells, the row counts and the event log are unchanged after
// the call.
func TestCorruptRowsEndpointEnumeratesKnownColumnsAndDoesNotMutate(t *testing.T) {
	srv, clock := newServer(t)
	ctx := context.Background()
	h := srv.Handler()

	node := SeedNodeForTest(t, srv.db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, srv.db, clock, "local", "host", 0, "tools")
	SeedEndpointForTest(t, srv.db, clock, "spare", "host", 1, "tools")
	itemID, err := CreateItem(ctx, srv.db, clock, CreateItemRequest{Title: "corrupt", Brief: "b", Needs: []string{"agentic"}})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := CreateItem(ctx, srv.db, clock, CreateItemRequest{Title: "healthy", Brief: "b", Needs: []string{"agentic"}}); err != nil {
		t.Fatalf("CreateItem (healthy): %v", err)
	}

	for _, tc := range []struct {
		stmt string
		args []any
	}{
		{`UPDATE work_items SET needs='{"not":"a list"}' WHERE id=?`, []any{itemID}},
		{`UPDATE nodes SET provides_live='not-json', reach_classes='[1,2]' WHERE id=?`, []any{node}},
		{`UPDATE models SET provides='{"x":1}', features='nope' WHERE id='m-local'`, nil},
	} {
		if _, err := srv.db.ExecContext(ctx, tc.stmt, tc.args...); err != nil {
			t.Fatalf("corrupt a cell: %v", err)
		}
	}

	before := snapshotCorruptCells(t, srv.db, itemID, node)

	rec := do(t, h, "GET", "/admin/corrupt-rows", testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("corrupt-rows = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var got []CorruptRow
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode corrupt-rows: %v (body %s)", err, rec.Body)
	}

	want := []struct{ table, row, column string }{
		{"work_items", itemID, "needs"},
		{"nodes", node, "provides_live"},
		{"nodes", node, "reach_classes"},
		{"models", "m-local", "provides"},
		{"models", "m-local", "features"},
	}
	if len(got) != len(want) {
		t.Fatalf("corrupt rows = %+v, want %d entries (one per corrupt column); valid rows must be absent", got, len(want))
	}
	for i, w := range want {
		if got[i].Table != w.table || got[i].RowID != w.row || got[i].Column != w.column {
			t.Fatalf("entry %d = %+v, want %s/%s column %s", i, got[i], w.table, w.row, w.column)
		}
		if !strings.Contains(got[i].Reason, "decode capability list") {
			t.Fatalf("entry %d reason = %q, want the claim path's own decode error", i, got[i].Reason)
		}
	}

	// Read-only means read-only: the cells and the audit spine are unchanged.
	after := snapshotCorruptCells(t, srv.db, itemID, node)
	if before != after {
		t.Fatalf("corrupt-rows mutated the board:\nbefore %s\nafter  %s", before, after)
	}
}

// The empty case is an empty list, not null and not an error: "nothing is
// corrupt" is an answer the operator runs the route to get.
func TestCorruptRowsEndpointIsAnEmptyListWhenNothingIsCorrupt(t *testing.T) {
	srv, clock := newServer(t)
	SeedNodeForTest(t, srv.db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, srv.db, clock, "local", "host", 0, "tools")

	rec := do(t, srv.Handler(), "GET", "/admin/corrupt-rows", testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("corrupt-rows = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Fatalf("corrupt-rows body = %q, want []", body)
	}
}

// The route is an operator tool: it names corrupt cells and the operator
// repairs them, so it must not answer without the admin credential.
func TestCorruptRowsEndpointRequiresAdminToken(t *testing.T) {
	srv, _ := newServer(t)
	rec := do(t, srv.Handler(), "GET", "/admin/corrupt-rows", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("corrupt-rows without a token = %d, want 401", rec.Code)
	}
}

// snapshotCorruptCells renders the five corruptible cells plus the audit-log
// size, so a test can prove the enumeration route read and did not write.
func snapshotCorruptCells(t *testing.T, db *sql.DB, itemID, nodeID string) string {
	t.Helper()
	var needs, live, reach, provides, features string
	if err := db.QueryRow(`SELECT needs FROM work_items WHERE id=?`, itemID).Scan(&needs); err != nil {
		t.Fatalf("read needs: %v", err)
	}
	if err := db.QueryRow(`SELECT provides_live, reach_classes FROM nodes WHERE id=?`, nodeID).Scan(&live, &reach); err != nil {
		t.Fatalf("read node cells: %v", err)
	}
	if err := db.QueryRow(`SELECT provides, features FROM models WHERE id='m-local'`).Scan(&provides, &features); err != nil {
		t.Fatalf("read model cells: %v", err)
	}
	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return needs + "|" + live + "|" + reach + "|" + provides + "|" + features + "|" + strconv.Itoa(events)
}

// The empty-but-healthy edge: when the only queued item is the corrupt one, the
// claim answers 200 with [] — the same answer an idle board gives. An error here
// would tell every node the board is broken because one row is unreadable, which
// is exactly the failure this task removes; a node cannot distinguish "no work"
// from "board down" otherwise.
func TestClaimOfOnlyACorruptItemIsAnEmptySuccess(t *testing.T) {
	// The report is asserted by the tests above; capture it here so this
	// test's expected skip does not print over the suite's output.
	captureLog(t)
	srv, clock := newServer(t)
	h := srv.Handler()
	ctx := context.Background()

	reg := postJSON(t, h, "/nodes/register", testEnrollToken, RegisterRequest{
		Name: "a", Role: "executor", ProvidesDeclared: []string{"agentic"},
		ReachClasses: []string{"host", "lan", "wan"}, Concurrency: 1,
	})
	if reg.Code != http.StatusCreated {
		t.Fatalf("register = %d: %s", reg.Code, reg.Body)
	}
	var issued RegisterResponse
	if err := json.Unmarshal(reg.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if rec := postJSON(t, h, "/nodes/heartbeat", issued.Token, HeartbeatRequest{
		ProvidesLive: []string{"agentic"},
		ReachClasses: []string{"host", "lan", "wan"},
		Protocol:     ProtocolMax,
	}); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d: %s", rec.Code, rec.Body)
	}
	SeedEndpointForTest(t, srv.db, clock, "local", "host", 0, "tools")

	itemID, err := CreateItem(ctx, srv.db, clock, CreateItemRequest{
		Title: "only", Brief: "b", Needs: []string{"agentic"},
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := srv.db.ExecContext(ctx,
		`UPDATE work_items SET needs='not-json' WHERE id=?`, itemID); err != nil {
		t.Fatalf("corrupt the only item: %v", err)
	}

	rec := postJSON(t, h, "/claim", issued.Token, ClaimRequest{FreeSlots: 1})
	if rec.Code != http.StatusOK {
		t.Fatalf("claim whose only item is corrupt = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("claim body = %q, want [] (an unreadable row is not a board outage)", got)
	}
}

// And the whole-board edge: every candidate row corrupt is still an empty
// success, not a 500. Each skipped row is reported, and the queued item is left
// alone for the operator to repair rather than destroyed.
func TestClaimReturnsEmptyWhenEveryCandidateRowIsCorrupt(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	logged := captureLog(t)

	nodeA := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	nodeB := SeedNodeForTest(t, db, clock, "b", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	if _, err := db.ExecContext(ctx, `UPDATE nodes SET provides_live='not-json'`); err != nil {
		t.Fatalf("corrupt every candidate row: %v", err)
	}
	itemID, err := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	got, err := Claim(ctx, db, clock, nodeA, 1)
	if err != nil {
		t.Fatalf("Claim failed when every candidate row is corrupt: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("claimed %d items with no readable candidate, want 0", len(got))
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM work_items WHERE id=?`, itemID).Scan(&status); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "queued" {
		t.Fatalf("item status = %q, want queued", status)
	}

	out := logged.String()
	for _, id := range []string{nodeA, nodeB} {
		if !strings.Contains(out, id) {
			t.Fatalf("corrupt candidate row %s was skipped silently; log output: %q", id, out)
		}
	}
}
