package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cti97/the-nightshift-manager/internal/provider"
)

// TestApprovalRequestRoundTrips stores each request in work_items.approval_request
// and reads it back: the bytes the board holds must be the bytes it would write
// again, and an empty option set must come back as [] -- a page iterating it
// must not meet null.
func TestApprovalRequestRoundTrips(t *testing.T) {
	db, _ := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	if _, err := db.Exec(`INSERT INTO work_items
		 (id,title,brief,status,created_by,created_at,updated_at)
		 VALUES ('w1','delete the cache','do it','needs_approval','admin',1,1)`); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	cases := []struct {
		name string
		req  ApprovalRequest
	}{
		{
			name: "empty options",
			req: ApprovalRequest{
				Action:      "delete the cache",
				Reason:      "the cache is 40GB and the run needs the disk",
				Options:     nil,
				RunID:       "r1",
				RequestedBy: "node-a",
				RequestedAt: 1_700_000_000,
			},
		},
		{
			name: "explicitly empty options",
			req: ApprovalRequest{
				Action:      "delete the cache",
				Reason:      "the cache is 40GB and the run needs the disk",
				Options:     []string{},
				RunID:       "r1",
				RequestedBy: "node-a",
				RequestedAt: 1_700_000_000,
			},
		},
		{
			name: "two options",
			req: ApprovalRequest{
				Action:      "delete the cache",
				Reason:      "the cache is 40GB and the run needs the disk",
				Options:     []string{"delete it", "keep it"},
				RunID:       "r2",
				RequestedBy: "node-b",
				RequestedAt: 1_700_000_099,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.req)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if _, err := db.Exec(`UPDATE work_items SET approval_request = ? WHERE id = 'w1'`,
				string(encoded)); err != nil {
				t.Fatalf("store: %v", err)
			}

			var stored string
			if err := db.QueryRow(`SELECT approval_request FROM work_items WHERE id = 'w1'`).
				Scan(&stored); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if stored != string(encoded) {
				t.Fatalf("stored %s, want the encoded bytes %s", stored, encoded)
			}

			var got ApprovalRequest
			if err := json.Unmarshal([]byte(stored), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			again, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if string(again) != stored {
				t.Fatalf("re-encoded %s, want the stored bytes %s", again, stored)
			}

			if got.Action != tc.req.Action || got.Reason != tc.req.Reason ||
				got.RunID != tc.req.RunID || got.RequestedBy != tc.req.RequestedBy ||
				got.RequestedAt != tc.req.RequestedAt {
				t.Fatalf("round-tripped %+v, want %+v", got, tc.req)
			}
			if !slices.Equal(got.Options, tc.req.Options) {
				t.Fatalf("options = %v, want %v", got.Options, tc.req.Options)
			}
			if len(tc.req.Options) == 0 {
				if got.Options == nil {
					t.Fatal("empty options read back as nil; a page ranging them gets null")
				}
				if !strings.Contains(stored, `"options":[]`) || strings.Contains(stored, `"options":null`) {
					t.Fatalf("empty options stored as %s, want \"options\":[]", stored)
				}
			}
		})
	}
}

// parkOneRun is the fixture the escalation tests share: a started run on a
// claimed item, with a parked request already accepted. It returns the item id
// and the run id; the caller decides what the request said.
func parkOneRun(t *testing.T, db *sql.DB, clock *FixedClock) (itemID, runID, nodeID string) {
	t.Helper()
	ctx := context.Background()
	nodeID = SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")

	var err error
	itemID, err = CreateItem(ctx, db, clock, CreateItemRequest{
		Title: "delete the cache", Brief: "b", Needs: []string{"agentic"},
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	claimed, err := Claim(ctx, db, clock, nodeID, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim = %d items, err %v", len(claimed), err)
	}
	runID = claimed[0].RunID
	if err := StartRun(ctx, db, clock, runID, nodeID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	return itemID, runID, nodeID
}

// TestEscalationParksTheItemAndClosesTheRun is the transition M3 exists for:
// the item waits for a human with the request stored on it, the run is over,
// and no lease is left behind (spec §11, D1, D6).
func TestEscalationParksTheItemAndClosesTheRun(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	itemID, runID, node := parkOneRun(t, db, clock)

	req := ApprovalRequest{
		Action:  "delete /var/cache/app",
		Reason:  "the cache holds 40GB and the run needs the disk",
		Options: []string{"delete it", "keep it"},
	}
	if err := Escalate(context.Background(), db, clock, runID, node, req); err != nil {
		t.Fatalf("Escalate: %v", err)
	}

	var status string
	var claimedBy sql.NullString
	var lease sql.NullInt64
	var stored sql.NullString
	if err := db.QueryRow(
		`SELECT status, claimed_by, lease_expires_at, approval_request FROM work_items WHERE id=?`,
		itemID).Scan(&status, &claimedBy, &lease, &stored); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "needs_approval" {
		t.Fatalf("item status = %q, want needs_approval", status)
	}
	if claimedBy.Valid || lease.Valid {
		t.Fatalf("a parked item holds no lease: claimed_by=%v lease=%v", claimedBy, lease)
	}
	if !stored.Valid {
		t.Fatal("approval_request is NULL on a parked item; the page has nothing to list")
	}
	var got ApprovalRequest
	if err := json.Unmarshal([]byte(stored.String), &got); err != nil {
		t.Fatalf("decode stored request: %v", err)
	}
	if got.Action != req.Action || got.Reason != req.Reason {
		t.Fatalf("stored request = %+v, want action %q and reason %q", got, req.Action, req.Reason)
	}
	if !slices.Equal(got.Options, req.Options) {
		t.Fatalf("stored options = %v, want %v", got.Options, req.Options)
	}
	// The board stamps identity and time; a node that lies about either would
	// otherwise write the audit spine.
	if got.RunID != runID || got.RequestedBy != node || got.RequestedAt != clock.Now() {
		t.Fatalf("stored request = %+v, want run %s by %s at %d", got, runID, node, clock.Now())
	}

	var runStatus string
	var endedAt sql.NullInt64
	if err := db.QueryRow(`SELECT status, ended_at FROM runs WHERE id=?`, runID).
		Scan(&runStatus, &endedAt); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if runStatus != "escalated" {
		t.Fatalf("run status = %q, want escalated", runStatus)
	}
	if !endedAt.Valid || endedAt.Int64 != clock.Now() {
		t.Fatalf("run ended_at = %v, want %d: an escalated run is closed, not left live", endedAt, clock.Now())
	}

	var events int
	if err := db.QueryRow(
		`SELECT count(*) FROM events WHERE entity='work_item' AND entity_id=? AND type='escalated'`,
		itemID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Fatalf("escalated events = %d, want exactly 1", events)
	}
	var payload string
	if err := db.QueryRow(
		`SELECT payload FROM events WHERE entity='work_item' AND entity_id=? AND type='escalated'`,
		itemID).Scan(&payload); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if !strings.Contains(payload, `"action":"delete /var/cache/app"`) {
		t.Fatalf("escalated event payload = %s, want it to name the action", payload)
	}
}

// TestEscalationChargesTheAttemptButDoesNotGrowIt pins D5's two halves: the
// attempt that spent model tokens stays spent, and parking does not invent a
// new one. The re-queue half of D5 is asserted where the re-queue exists
// (TestApprovalRequeuesWithTheActionItGranted).
func TestEscalationChargesTheAttemptButDoesNotGrowIt(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	itemID, runID, node := parkOneRun(t, db, clock)

	if err := Escalate(context.Background(), db, clock, runID, node, ApprovalRequest{
		Action: "delete /var/cache/app", Reason: "needs the disk",
	}); err != nil {
		t.Fatalf("Escalate: %v", err)
	}

	var itemAttempt int
	if err := db.QueryRow(`SELECT attempt FROM work_items WHERE id=?`, itemID).Scan(&itemAttempt); err != nil {
		t.Fatalf("read item attempt: %v", err)
	}
	if itemAttempt != 1 {
		t.Fatalf("work_items.attempt = %d after an escalation, want 1: the claim was spent, not refunded", itemAttempt)
	}
	// runs.attempt is the per-item claim ordinal, a different counter. It must
	// not move either: the escalation is not a retry of the same work.
	var runAttempt int
	if err := db.QueryRow(`SELECT attempt FROM runs WHERE id=?`, runID).Scan(&runAttempt); err != nil {
		t.Fatalf("read run attempt: %v", err)
	}
	if runAttempt != 1 {
		t.Fatalf("runs.attempt = %d after an escalation, want 1", runAttempt)
	}
}

// TestAVanishedRowCannotBeEscalated is the re-read rule every guarded write in
// this package follows: the transaction decides on rows it read inside itself,
// and a row that settled between the claim and the escalation is left settled.
func TestAVanishedRowCannotBeEscalated(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	itemID, runID, node := parkOneRun(t, db, clock)

	// The run finishes before the escalation arrives: a lost race with a report.
	if _, err := Report(context.Background(), db, clock, runID, node, ReportRequest{
		Status: "ok", Summary: "finished",
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	err := Escalate(context.Background(), db, clock, runID, node, ApprovalRequest{
		Action: "delete /var/cache/app", Reason: "too late",
	})
	if err == nil {
		t.Fatal("Escalate accepted a settled run; the escalation must not rewind it")
	}
	// The refusal has to name the run, not fall through to the item guard: a
	// node reading "work item is no longer claimed" would retry its claim when
	// what actually happened is that its own report settled the run.
	if !strings.Contains(err.Error(), "already ok") {
		t.Fatalf("error = %q, want it to name the run's settled status", err)
	}

	var itemStatus string
	var stored sql.NullString
	if err := db.QueryRow(`SELECT status, approval_request FROM work_items WHERE id=?`, itemID).
		Scan(&itemStatus, &stored); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if itemStatus != "done" {
		t.Fatalf("item status = %q after a refused escalation, want done (nothing written)", itemStatus)
	}
	if stored.Valid {
		t.Fatalf("approval_request = %q after a refused escalation, want NULL (nothing written)", stored.String)
	}
	var runStatus string
	if err := db.QueryRow(`SELECT status FROM runs WHERE id=?`, runID).Scan(&runStatus); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if runStatus != "ok" {
		t.Fatalf("run status = %q after a refused escalation, want ok", runStatus)
	}
	var events int
	if err := db.QueryRow(
		`SELECT count(*) FROM events WHERE entity='work_item' AND entity_id=? AND type='escalated'`,
		itemID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Fatalf("escalated events = %d after a refused escalation, want 0", events)
	}
}

// TestTheReaperLeavesAParkedItemAlone pins D6: a parked item holds no lease,
// so the lease scan cannot see it, and the node sweep must not settle it
// either, even after the clock runs well past where a lease would have lived.
func TestTheReaperLeavesAParkedItemAlone(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	itemID, runID, node := parkOneRun(t, db, clock)

	if err := Escalate(context.Background(), db, clock, runID, node, ApprovalRequest{
		Action: "delete /var/cache/app", Reason: "needs the disk",
	}); err != nil {
		t.Fatalf("Escalate: %v", err)
	}

	stepPastLease(clock)
	res, err := Reap(context.Background(), db, clock)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if res != (ReapResult{}) {
		t.Fatalf("Reap changed something for a parked item: %+v", res)
	}

	var itemStatus, runStatus string
	var stored sql.NullString
	if err := db.QueryRow(`SELECT status, approval_request FROM work_items WHERE id=?`, itemID).
		Scan(&itemStatus, &stored); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if itemStatus != "needs_approval" || !stored.Valid {
		t.Fatalf("parked item = %q with request valid=%v after the reaper, want needs_approval with its request intact", itemStatus, stored.Valid)
	}
	if err := db.QueryRow(`SELECT status FROM runs WHERE id=?`, runID).Scan(&runStatus); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if runStatus != "escalated" {
		t.Fatalf("run status = %q after the reaper, want escalated", runStatus)
	}
	var reaped int
	if err := db.QueryRow(
		`SELECT count(*) FROM events WHERE entity='work_item' AND entity_id=? AND type='reaped'`,
		itemID).Scan(&reaped); err != nil {
		t.Fatalf("count reaped events: %v", err)
	}
	if reaped != 0 {
		t.Fatalf("reaped events = %d for a parked item, want 0", reaped)
	}
}

// TestTheEscalationRouteParksTheRun drives the node-facing transport for the
// transition above (spec §11: POST /runs/:id/approval): the node that owns the
// run parks it, a body with no action is refused before any write, and the
// admin credential — which can create items and read every brief — is not a
// node credential and cannot speak for one.
func TestTheEscalationRouteParksTheRun(t *testing.T) {
	db, clock := newTestDB(t)
	srv := NewServer(db, clock, provider.NewRegistry(), HashToken(testAdminToken), HashToken(testEnrollToken))
	ctx := context.Background()

	nodeID, token, err := RegisterNode(ctx, db, clock, RegisterRequest{
		Name: "a", Role: "executor", ProvidesDeclared: []string{"agentic"},
		ReachClasses: []string{"host", "lan", "wan"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if _, err := Heartbeat(ctx, db, clock, nodeID, HeartbeatRequest{
		ProvidesLive: []string{"agentic"}, ReachClasses: []string{"host", "lan", "wan"}, Protocol: ProtocolMax,
	}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")

	itemID, err := CreateItem(ctx, db, clock, CreateItemRequest{
		Title: "t", Brief: "b", Needs: []string{"agentic"},
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	claimed, err := Claim(ctx, db, clock, nodeID, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim = %d items, err %v", len(claimed), err)
	}
	runID := claimed[0].RunID
	if err := StartRun(ctx, db, clock, runID, nodeID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	post := func(bearer, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/runs/"+runID+"/approval", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	if rec := post(testAdminToken, `{"action":"delete the cache"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin token on the escalation route = %d, want 401: approval is a node's own act", rec.Code)
	}

	if rec := post(token, `{"reason":"no action given"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("request without an action = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM work_items WHERE id=?`, itemID).Scan(&status); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "running" {
		t.Fatalf("item status = %q after a refused body, want running (nothing written)", status)
	}

	rec := post(token, `{"action":"delete /var/cache/app","reason":"needs the disk","options":["delete","keep"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("escalation = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if err := db.QueryRow(`SELECT status FROM work_items WHERE id=?`, itemID).Scan(&status); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "needs_approval" {
		t.Fatalf("item status = %q after the route, want needs_approval", status)
	}
	var stored sql.NullString
	if err := db.QueryRow(`SELECT approval_request FROM work_items WHERE id=?`, itemID).Scan(&stored); err != nil {
		t.Fatalf("read request: %v", err)
	}
	if !stored.Valid || !strings.Contains(stored.String, `"action":"delete /var/cache/app"`) {
		t.Fatalf("stored request = %v, want the action the node posted", stored)
	}
}
