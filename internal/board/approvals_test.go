package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
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
// claimed item. It returns the item id, the run id and the node id; the caller
// decides what the request says, and whether it is escalated at all.
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
	if err := Escalate(context.Background(), db, clock, runID, node, req, RunTokens{}); err != nil {
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
	}, RunTokens{}); err != nil {
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
	}, RunTokens{})
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

// parkEscalated is the fixture the decision tests share: one item parked with
// the given action, and the item id returned.
func parkEscalated(t *testing.T, db *sql.DB, clock *FixedClock, action string) (itemID, node string) {
	t.Helper()
	itemID, runID, node := parkOneRun(t, db, clock)
	if err := Escalate(context.Background(), db, clock, runID, node, ApprovalRequest{
		Action: action, Reason: "the run needs it", Options: []string{"yes", "no"},
	}, RunTokens{}); err != nil {
		t.Fatalf("Escalate: %v", err)
	}
	return itemID, node
}

// TestApprovalRequeuesWithTheActionItGranted is D3 and the second half of D5:
// the grant names the action it allows, the item goes back to queued with its
// spent attempt unchanged, and the request it answers is cleared.
func TestApprovalRequeuesWithTheActionItGranted(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	itemID, _ := parkEscalated(t, db, clock, "delete /var/cache/app")

	res, err := Decide(context.Background(), db, clock, itemID, Approve, "go ahead, the cache is regenerable")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if res.Status != "queued" {
		t.Fatalf("result status = %q, want queued", res.Status)
	}

	var status string
	var rawGrant, rawRequest sql.NullString
	var attempt int
	if err := db.QueryRow(
		`SELECT status, approval_granted, approval_request, attempt FROM work_items WHERE id=?`,
		itemID).Scan(&status, &rawGrant, &rawRequest, &attempt); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "queued" {
		t.Fatalf("item status = %q, want queued", status)
	}
	if !rawGrant.Valid {
		t.Fatal("approval_granted is NULL after an approval; the next run has nothing to honour")
	}
	var grant ApprovalGrant
	if err := json.Unmarshal([]byte(rawGrant.String), &grant); err != nil {
		t.Fatalf("decode grant: %v", err)
	}
	if grant.Action != "delete /var/cache/app" {
		t.Fatalf("grant action = %q, want the action the request named", grant.Action)
	}
	if grant.Note != "go ahead, the cache is regenerable" || grant.DecidedAt != clock.Now() {
		t.Fatalf("grant = %+v, want the operator's note at %d", grant, clock.Now())
	}
	if rawRequest.Valid {
		t.Fatalf("approval_request = %q on a queued item, want NULL: the question was answered", rawRequest.String)
	}
	if attempt != 1 {
		t.Fatalf("attempt = %d after a re-queue, want 1: the decision does not charge or refund (D5)", attempt)
	}

	var approved, denied int
	if err := db.QueryRow(
		`SELECT count(*) FILTER (WHERE type='approved'), count(*) FILTER (WHERE type='denied')
		   FROM events WHERE entity='work_item' AND entity_id=?`, itemID).Scan(&approved, &denied); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if approved != 1 || denied != 0 {
		t.Fatalf("approved=%d denied=%d, want exactly one approved event", approved, denied)
	}
}

// TestDenialCancelsWithTheDecidersNote is D7: a decline is a cancellation that
// says who declined and why, not a failure. Nothing went wrong with the run.
func TestDenialCancelsWithTheDecidersNote(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	itemID, _ := parkEscalated(t, db, clock, "delete /var/cache/app")

	res, err := Decide(context.Background(), db, clock, itemID, Deny, "that cache is not regenerable")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if res.Status != "cancelled" {
		t.Fatalf("result status = %q, want cancelled", res.Status)
	}

	var status string
	var reason, grant, request sql.NullString
	if err := db.QueryRow(
		`SELECT status, blocked_reason, approval_granted, approval_request FROM work_items WHERE id=?`,
		itemID).Scan(&status, &reason, &grant, &request); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "cancelled" {
		t.Fatalf("item status = %q, want cancelled", status)
	}
	if !reason.Valid || !strings.Contains(reason.String, "declined") {
		t.Fatalf("blocked_reason = %v, want it to say the item was declined", reason)
	}
	if !strings.Contains(reason.String, "that cache is not regenerable") {
		t.Fatalf("blocked_reason = %q, want the decider's note in it", reason.String)
	}
	if grant.Valid {
		t.Fatalf("approval_granted = %q on a denied item, want NULL", grant.String)
	}
	if request.Valid {
		t.Fatalf("approval_request = %q on a denied item, want NULL", request.String)
	}

	var approved, denied int
	if err := db.QueryRow(
		`SELECT count(*) FILTER (WHERE type='approved'), count(*) FILTER (WHERE type='denied')
		   FROM events WHERE entity='work_item' AND entity_id=?`, itemID).Scan(&approved, &denied); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if denied != 1 || approved != 0 {
		t.Fatalf("approved=%d denied=%d, want exactly one denied event", approved, denied)
	}
}

// TestASecondDecisionIsRefused is D4: a decision is one-way and first-wins. The
// second is a conflict, the first stands, and the audit spine has one decision
// rather than two contradictory ones.
func TestASecondDecisionIsRefused(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	itemID, _ := parkEscalated(t, db, clock, "delete /var/cache/app")

	if _, err := Decide(context.Background(), db, clock, itemID, Approve, "yes"); err != nil {
		t.Fatalf("first decision: %v", err)
	}

	_, err := Decide(context.Background(), db, clock, itemID, Deny, "no, changed my mind")
	if err == nil {
		t.Fatal("a second decision was accepted; the first must stand")
	}
	if !errors.Is(err, ErrNotParked) {
		t.Fatalf("error = %v, want ErrNotParked so the route can answer 409", err)
	}

	var status, reason string
	var grant sql.NullString
	if err := db.QueryRow(`SELECT status, COALESCE(blocked_reason,''), approval_granted FROM work_items WHERE id=?`, itemID).
		Scan(&status, &reason, &grant); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "queued" || reason != "" || !grant.Valid {
		t.Fatalf("item = status %q, reason %q, grant valid %v; want the first decision intact", status, reason, grant.Valid)
	}

	var approved, denied int
	if err := db.QueryRow(
		`SELECT count(*) FILTER (WHERE type='approved'), count(*) FILTER (WHERE type='denied')
		   FROM events WHERE entity='work_item' AND entity_id=?`, itemID).Scan(&approved, &denied); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if approved != 1 || denied != 0 {
		t.Fatalf("approved=%d denied=%d, want one decision event and no contradiction", approved, denied)
	}
}

// TestAnApprovedItemCanBeClaimedAgain follows the whole loop through the claim
// path: the requeued item is claimed as a new run, with a new run ordinal, and
// the grant travels on the payload where the executor will read it (D3, D5).
func TestAnApprovedItemCanBeClaimedAgain(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	itemID, node := parkEscalated(t, db, clock, "delete /var/cache/app")

	if _, err := Decide(context.Background(), db, clock, itemID, Approve, "yes"); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	claimed, err := Claim(context.Background(), db, clock, node, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim = %d items, err %v", len(claimed), err)
	}
	got := claimed[0]
	if got.Item.ID != itemID {
		t.Fatalf("claimed %s, want the approved item %s", got.Item.ID, itemID)
	}
	if got.Item.ApprovalGranted == nil {
		t.Fatal("the claim payload carries no grant; the executor cannot honour the decision")
	}
	if got.Item.ApprovalGranted.Action != "delete /var/cache/app" {
		t.Fatalf("grant on the payload = %q, want the approved action", got.Item.ApprovalGranted.Action)
	}
	if got.Item.Attempt != 2 {
		t.Fatalf("item attempt = %d after the second claim, want 2: the claim charges, not the decision", got.Item.Attempt)
	}
	// runs.attempt is the claim ordinal and is unique per item; the re-run is a
	// second attempt of the item, not a reuse of the first run's row.
	var ordinal int
	if err := db.QueryRow(`SELECT attempt FROM runs WHERE id=?`, got.RunID).Scan(&ordinal); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if ordinal != 2 {
		t.Fatalf("run ordinal = %d, want 2", ordinal)
	}
}

// TestARequestlessRowCanBeDeniedButNotApproved is the nullable column's other
// edge: without a readable action there is nothing for an approval to name, so
// approve is refused rather than writing a grant that matches nothing. Deny
// still works, because the operator's reason needs no action to name — and a
// queue an operator cannot clear is worse than one they can only decline.
func TestARequestlessRowCanBeDeniedButNotApproved(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	itemID, _ := parkEscalated(t, db, clock, "delete /var/cache/app")
	if _, err := db.Exec(`UPDATE work_items SET approval_request=NULL WHERE id=?`, itemID); err != nil {
		t.Fatalf("clear request: %v", err)
	}

	if _, err := Decide(context.Background(), db, clock, itemID, Approve, "yes"); !errors.Is(err, ErrNoRequest) {
		t.Fatalf("approve without a request = %v, want ErrNoRequest", err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM work_items WHERE id=?`, itemID).Scan(&status); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "needs_approval" {
		t.Fatalf("status = %q after a refused approval, want needs_approval (nothing written)", status)
	}

	if _, err := Decide(context.Background(), db, clock, itemID, Deny, "the request is unreadable, start over"); err != nil {
		t.Fatalf("deny with no request: %v", err)
	}
	var reason string
	if err := db.QueryRow(`SELECT status, blocked_reason FROM work_items WHERE id=?`, itemID).
		Scan(&status, &reason); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "cancelled" || !strings.Contains(reason, "start over") {
		t.Fatalf("item = %q with reason %q, want a cancellation naming the note", status, reason)
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
	}, RunTokens{}); err != nil {
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

// TestEscalationPricesTheRunIntoTheLedger closes the hole an escalation would
// otherwise leave in the spend guard. The run used tokens and never reports, and
// the day's ceiling sums runs.cost_estimate: an escalation that parked the item
// without pricing the run would make every stopped-to-ask run free on the very
// day the operator was watching the budget, and show up as unpriced in the
// spend view.
func TestEscalationPricesTheRunIntoTheLedger(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "priced", "host", 3.0, "tools")
	itemID, err := CreateItem(ctx, db, clock, CreateItemRequest{
		Title: "t", Brief: "b", Needs: []string{"agentic"},
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	claimed, err := Claim(ctx, db, clock, node, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim = %d items, err %v", len(claimed), err)
	}
	runID := claimed[0].RunID
	if err := StartRun(ctx, db, clock, runID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	tokens := RunTokens{TokensIn: 1000, TokensOut: 200, ReservedTokens: 500}
	if err := Escalate(ctx, db, clock, runID, node, ApprovalRequest{
		Action: "delete /var/cache/app", Reason: "needs the disk",
	}, tokens); err != nil {
		t.Fatalf("Escalate: %v", err)
	}

	var in, out, reserved int
	var cost float64
	if err := db.QueryRow(
		`SELECT input_tokens, output_tokens, reserved_tokens, cost_estimate FROM runs WHERE id=?`,
		runID).Scan(&in, &out, &reserved, &cost); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if in != 1000 || out != 200 || reserved != 500 {
		t.Fatalf("run tokens = %d/%d/%d, want 1000/200/500: the run's own report of what it spent", in, out, reserved)
	}
	// (tokens_in + reserved) at the input rate: 1500 × 3.0 / 1e6.
	if want := 0.0045; math.Abs(cost-want) > 1e-12 {
		t.Fatalf("cost_estimate = %v, want %v", cost, want)
	}

	st, err := TodaySpend(ctx, db, clock)
	if err != nil {
		t.Fatalf("TodaySpend: %v", err)
	}
	if math.Abs(st.Spent-cost) > 1e-12 || st.RunCount != 1 {
		t.Fatalf("spend = %v over %d runs, want %v over 1: the escalated run is in the day's ledger", st.Spent, st.RunCount, cost)
	}

	var payload string
	if err := db.QueryRow(
		`SELECT payload FROM events WHERE entity='work_item' AND entity_id=? AND type='escalated'`,
		itemID).Scan(&payload); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if !strings.Contains(payload, `"cost":0.0045`) {
		t.Fatalf("escalated event payload = %s, want the priced cost in it", payload)
	}
}

// TestEscalationRefusesNegativeTokens applies Report's spend rule to the
// escalation path: a negative count is refused before any write, not parked as a
// credit against the day.
func TestEscalationRefusesNegativeTokens(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	itemID, runID, node := parkOneRun(t, db, clock)

	err := Escalate(context.Background(), db, clock, runID, node, ApprovalRequest{
		Action: "delete /var/cache/app", Reason: "needs the disk",
	}, RunTokens{TokensIn: -1})
	if err == nil {
		t.Fatal("Escalate accepted a negative token count")
	}

	var itemStatus, runStatus string
	if err := db.QueryRow(`SELECT status FROM work_items WHERE id=?`, itemID).Scan(&itemStatus); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM runs WHERE id=?`, runID).Scan(&runStatus); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if itemStatus != "running" || runStatus != "running" {
		t.Fatalf("item %q / run %q after a refused escalation, want both still running", itemStatus, runStatus)
	}
	var events int
	if err := db.QueryRow(
		`SELECT count(*) FROM events WHERE entity='work_item' AND entity_id=? AND type='escalated'`,
		itemID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Fatalf("escalated events = %d, want 0 (nothing written)", events)
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
