// internal/board/reaper_test.go
package board

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// stepPastLease moves the clock just beyond an expired lease. The brief writes
// clock.Advance(leaseSeconds + 1), but leaseSeconds counts seconds (nodes.go),
// so a bare Duration there is 301ns and FixedClock.Advance — which stores whole
// seconds — truncates it to zero. The multiplication keeps the intended step.
func stepPastLease(clock *FixedClock) {
	clock.Advance(time.Duration(leaseSeconds+1) * time.Second)
}

func TestReapRewardsAClaimThatNeverStarted(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})

	first, err := Claim(ctx, db, clock, node, 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("Claim: got %d items, err %v", len(first), err)
	}

	stepPastLease(clock)
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
	if attempt != 0 {
		t.Fatalf("attempt = %d, want 0 (a claim that never started must be refunded, not left spent)", attempt)
	}

	var runStatus string
	if err := db.QueryRow(`SELECT status FROM runs WHERE work_item_id=?`, id).Scan(&runStatus); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if runStatus != "abandoned" {
		t.Fatalf("run status = %q, want abandoned", runStatus)
	}

	// The refund is visible in the audit log with both numbers: that is where an
	// operator would explain why an item's budget went back down.
	events, err := ListEvents(ctx, db, "work_item", id, 50)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var reaped string
	for _, e := range events {
		if e.Type == "reaped" {
			reaped = e.Payload
		}
	}
	if !strings.Contains(reaped, `"attempt_from":1`) || !strings.Contains(reaped, `"attempt_to":0`) {
		t.Fatalf("reaped event payload %q, want attempt_from 1 and attempt_to 0", reaped)
	}

	// The refund only means something if the budget is usable again: claim the
	// item a second time and it must arrive as attempt 1 with a fresh run, while
	// the abandoned run keeps its own ordinal.
	second, err := Claim(ctx, db, clock, node, 1)
	if err != nil || len(second) != 1 {
		t.Fatalf("second Claim: got %d items, err %v", len(second), err)
	}
	if second[0].Item.Attempt != 1 {
		t.Fatalf("second claim attempt = %d, want 1 (the abandoned claim cost the item nothing)", second[0].Item.Attempt)
	}
	if second[0].RunID == first[0].RunID {
		t.Fatalf("second claim reused run %q", first[0].RunID)
	}
	if got := itemAttempt(t, db, id); got != 1 {
		t.Fatalf("item attempt after reclaim = %d, want 1", got)
	}
	if got := runOrdinals(t, db, id); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("run ordinals = %v, want [1 2]: the abandoned run keeps ordinal 1 and the new claim takes 2", got)
	}
	if got, want := itemAttempt(t, db, id), nonAbandonedRuns(t, db, id); got != want {
		t.Fatalf("item attempt = %d, non-abandoned runs = %d; the attempt counter must equal what was actually spent", got, want)
	}
}

// Reap runs every 30s for the life of the process, so a requeue that forgot to
// clear the lease would be reaped again on the next tick: the second pass must
// find nothing and must not append a second audit event.
func TestReapSecondPassFindsNothing(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})

	if _, err := Claim(ctx, db, clock, node, 1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	stepPastLease(clock)
	first, err := Reap(ctx, db, clock)
	if err != nil {
		t.Fatalf("first Reap: %v", err)
	}
	second, err := Reap(ctx, db, clock)
	if err != nil {
		t.Fatalf("second Reap: %v", err)
	}
	if first != (ReapResult{Abandoned: 1}) {
		t.Fatalf("first Reap = %+v, want one abandoned run", first)
	}
	if second != (ReapResult{}) {
		t.Fatalf("second Reap = %+v, want nothing left to reap", second)
	}

	events, err := ListEvents(ctx, db, "work_item", id, 50)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var reaped int
	for _, e := range events {
		if e.Type == "reaped" {
			reaped++
		}
	}
	if reaped != 1 {
		t.Fatalf("%d 'reaped' events, want exactly 1", reaped)
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
	if err := StartRun(ctx, db, clock, got[0].RunID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	stepPastLease(clock)
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

	// The contrast with the abandoned case: this attempt was spent, so the next
	// claim is genuinely attempt 2, on a new run ordinal.
	second, err := Claim(ctx, db, clock, node, 1)
	if err != nil || len(second) != 1 {
		t.Fatalf("second Claim: got %d items, err %v", len(second), err)
	}
	if second[0].Item.Attempt != 2 {
		t.Fatalf("second claim attempt = %d, want 2 (a started run keeps its attempt)", second[0].Item.Attempt)
	}
	if got := runOrdinals(t, db, id); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("run ordinals = %v, want [1 2]", got)
	}
	if got, want := itemAttempt(t, db, id), nonAbandonedRuns(t, db, id); got != want {
		t.Fatalf("item attempt = %d, non-abandoned runs = %d; the attempt counter must equal what was actually spent", got, want)
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
	if err := StartRun(ctx, db, clock, got[0].RunID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	stepPastLease(clock)
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

// Reap's scan runs outside a transaction, so a report can commit in the gap
// between the scan and this item's transaction. That interleaving cannot be
// forced from a unit test, so this reproduces the state it leaves behind: the
// run is final and the item is settled (Report writes both in one transaction).
// The per-item settle is called directly because Reap's scan would no longer
// select a settled item — the guard exists for the window, not for the scan.
// Rewinding settled work is the silent failure this board exists to avoid.
func TestReapNeverRewindsASettledRun(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	got, err := Claim(ctx, db, clock, node, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("Claim: %v", err)
	}
	stepPastLease(clock)
	if _, err := db.ExecContext(ctx,
		`UPDATE runs SET status='ok', ended_at=? WHERE id=?`, clock.Now(), got[0].RunID); err != nil {
		t.Fatalf("settle run: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE work_items SET status='done', result_summary='settled' WHERE id=?`, id); err != nil {
		t.Fatalf("settle item: %v", err)
	}

	out, err := reapItem(ctx, db, clock, id, clock.Now())
	if err != nil {
		t.Fatalf("reapItem: %v", err)
	}
	if out.run != "skipped" {
		t.Fatalf("outcome = %+v, want a skipped item (a settled run is not reapable)", out)
	}

	var runStatus, itemStatus string
	if err := db.QueryRow(`SELECT status FROM runs WHERE id=?`, got[0].RunID).Scan(&runStatus); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM work_items WHERE id=?`, id).Scan(&itemStatus); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if runStatus != "ok" {
		t.Fatalf("run status = %q, want ok (a settled run must not be rewound)", runStatus)
	}
	if itemStatus != "done" {
		t.Fatalf("item status = %q, want done (a settled item must not be requeued)", itemStatus)
	}
}

// The reaper finds the current run by status, so it depends on an item having at
// most one non-terminal run. Claim only inserts a run for a queued item, and
// every requeue happens in the same transaction that settles the run, so two is
// a corrupted board. Picking one would compound it; the reaper fails loudly.
func TestReapRefusesTwoNonTerminalRuns(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	if _, err := Claim(ctx, db, clock, node, 1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// A second non-terminal run: the state a bug would leave, never the API.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO runs (id,work_item_id,attempt,node_id,endpoint_id,model_id,started_at,status,placement_reason)
		 SELECT 'dup-run', work_item_id, 2, node_id, endpoint_id, model_id, started_at, 'claimed', placement_reason
		   FROM runs WHERE work_item_id=?`, id); err != nil {
		t.Fatalf("insert second run: %v", err)
	}
	stepPastLease(clock)

	if _, err := Reap(ctx, db, clock); err == nil {
		t.Fatal("Reap accepted two non-terminal runs for one item")
	} else if !strings.Contains(err.Error(), "non-terminal") {
		t.Fatalf("error = %v, want it to name the non-terminal runs", err)
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM work_items WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "claimed" {
		t.Fatalf("item status = %q, want claimed (the reaper must not guess)", status)
	}
}

// A claim that never started has spent nothing, so the budget must not fire for
// it even when the counter has reached max_attempts (spec §8: reaped without
// spending an attempt). Only a run that actually started can exhaust the budget.
func TestReapRequeuesAClaimThatNeverStartedEvenAtTheBudgetCap(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{
		Title: "t", Brief: "b", Needs: []string{"agentic"}, MaxAttempts: 1,
	})

	if _, err := Claim(ctx, db, clock, node, 1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	stepPastLease(clock)
	res, err := Reap(ctx, db, clock)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if res.Failed != 0 || res.Abandoned != 1 {
		t.Fatalf("result = %+v, want one abandoned run and no failure", res)
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM work_items WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "queued" {
		t.Fatalf("status = %q, want queued (nothing was spent, so nothing is exhausted)", status)
	}
}

// A heartbeat can extend a lease between Reap's scan and this item's
// transaction. The transaction re-reads the lease so a renewed claim is left
// alone: the node is still working on it. The settle is called directly because
// the scan would no longer select the item once the lease moved.
func TestReapSkipsAnItemWhoseLeaseWasRenewed(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	if _, err := Claim(ctx, db, clock, node, 1); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	stepPastLease(clock)
	now := clock.Now() // the moment the scan decided this lease was expired
	// A heartbeat lands in the gap and renews the lease.
	if _, err := db.ExecContext(ctx,
		`UPDATE work_items SET lease_expires_at=? WHERE id=?`, now+leaseSeconds, id); err != nil {
		t.Fatalf("renew lease: %v", err)
	}

	out, err := reapItem(ctx, db, clock, id, now)
	if err != nil {
		t.Fatalf("reapItem: %v", err)
	}
	if out.run != "skipped" {
		t.Fatalf("outcome = %+v, want skipped (the lease was renewed)", out)
	}

	var status, runStatus string
	var attempt int
	if err := db.QueryRow(`SELECT status, attempt FROM work_items WHERE id=?`, id).Scan(&status, &attempt); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM runs WHERE work_item_id=?`, id).Scan(&runStatus); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != "claimed" || attempt != 1 {
		t.Fatalf("item = %q/attempt %d, want claimed/1 (nothing was reaped)", status, attempt)
	}
	if runStatus != "claimed" {
		t.Fatalf("run status = %q, want claimed", runStatus)
	}
}

// The refund has a floor. SQLite has no CHECK on attempt, so a claimed item at 0
// would otherwise be written to -1. Only a bug or DB surgery gets an item there,
// and the floor is the difference between "refunded" and "corrupted".
func TestReapRefundNeverGoesNegative(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	if _, err := Claim(ctx, db, clock, node, 1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE work_items SET attempt=0 WHERE id=?`, id); err != nil {
		t.Fatalf("force attempt 0: %v", err)
	}
	stepPastLease(clock)
	if _, err := Reap(ctx, db, clock); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if got := itemAttempt(t, db, id); got != 0 {
		t.Fatalf("attempt = %d, want 0 (the refund must not go below zero)", got)
	}
}

// An item that says it is running must never be requeued on the strength of a
// missing run: its work may have started, and requeueing it would run that work
// twice. A claimed item with no run cannot have started (there is no run id to
// start), so that case is refunded instead — it is exercised in
// TestReapRewardsAClaimThatNeverStarted's sibling paths.
func TestReapRefusesToRequeueARunningItemWithNoRun(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	if _, err := Claim(ctx, db, clock, node, 1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// The corrupt state: the item says running, its run is already terminal.
	// Nothing on the board can produce this; a bug could.
	if _, err := db.ExecContext(ctx, `UPDATE runs SET status='lost' WHERE work_item_id=?`, id); err != nil {
		t.Fatalf("terminalise run: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE work_items SET status='running' WHERE id=?`, id); err != nil {
		t.Fatalf("mark item running: %v", err)
	}
	stepPastLease(clock)

	if _, err := Reap(ctx, db, clock); err == nil {
		t.Fatal("Reap requeued a running item whose run is gone")
	} else if !strings.Contains(err.Error(), "may have started") {
		t.Fatalf("error = %v, want it to name the risk", err)
	}

	var status string
	var attempt int
	if err := db.QueryRow(`SELECT status, attempt FROM work_items WHERE id=?`, id).Scan(&status, &attempt); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "running" || attempt != 1 {
		t.Fatalf("item = %q/attempt %d, want it left exactly as found (running/1)", status, attempt)
	}
}

// One corrupt item must not stop the pass. The scan order is stable, so an
// early return would starve every other lease forever, and the corrupt item is
// inserted first on purpose: it is the one the scan reaches first. Both items
// stay 'claimed' so the corrupt one really is first in scan order.
func TestReapKeepsGoingPastACorruptItem(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	corruptID, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "bad", Brief: "b", Needs: []string{"agentic"}})
	healthyID, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "ok", Brief: "b", Needs: []string{"agentic"}})
	got, err := Claim(ctx, db, clock, node, 2)
	if err != nil || len(got) != 2 {
		t.Fatalf("Claim: got %d items, err %v", len(got), err)
	}

	// Two non-terminal runs for one item: the corrupt state the loud guard exists
	// for. The item's status is untouched, so the scan order is unchanged.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO runs (id,work_item_id,attempt,node_id,endpoint_id,model_id,started_at,status,placement_reason)
		 SELECT 'dup-run', work_item_id, 2, node_id, endpoint_id, model_id, started_at, 'claimed', placement_reason
		   FROM runs WHERE work_item_id=?`, corruptID); err != nil {
		t.Fatalf("insert second run: %v", err)
	}

	stepPastLease(clock)
	res, err := Reap(ctx, db, clock)
	if err == nil {
		t.Fatal("Reap accepted two non-terminal runs for one item")
	}
	if !strings.Contains(err.Error(), "non-terminal") {
		t.Fatalf("error = %v, want it to name the non-terminal runs", err)
	}

	var healthyStatus string
	var healthyAttempt int
	if err := db.QueryRow(`SELECT status, attempt FROM work_items WHERE id=?`, healthyID).
		Scan(&healthyStatus, &healthyAttempt); err != nil {
		t.Fatalf("read healthy item: %v", err)
	}
	if healthyStatus != "queued" || healthyAttempt != 0 {
		t.Fatalf("healthy item = %q/attempt %d, want queued/0: the pass must keep going past the corrupt item",
			healthyStatus, healthyAttempt)
	}
	if res.Abandoned != 1 {
		t.Fatalf("Reap = %+v, want the healthy item counted", res)
	}

	var corruptStatus string
	if err := db.QueryRow(`SELECT status FROM work_items WHERE id=?`, corruptID).Scan(&corruptStatus); err != nil {
		t.Fatalf("read corrupt item: %v", err)
	}
	if corruptStatus != "claimed" {
		t.Fatalf("corrupt item status = %q, want it left alone as claimed", corruptStatus)
	}
}

// StartRun is the only thing that makes an attempt expensive, so it must not be
// a no-op for the wrong node or for a run that is already running: either would
// let a run be "started" without the transition that charges the attempt.
func TestStartRunRefusesARunItDoesNotOwnOrAlreadyStarted(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	other := SeedNodeForTest(t, db, clock, "b", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	got, err := Claim(ctx, db, clock, node, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("Claim: %v", err)
	}
	runID := got[0].RunID

	if err := StartRun(ctx, db, clock, runID, other); err == nil {
		t.Fatal("StartRun accepted a node that does not own the run")
	}
	if err := StartRun(ctx, db, clock, "no-such-run", node); err == nil {
		t.Fatal("StartRun accepted an unknown run id")
	}
	// Rejections are atomic: a refused start must leave the item exactly as
	// Claim left it, never half-moved to running.
	var itemStatus string
	if err := db.QueryRow(`SELECT status FROM work_items WHERE id=?`, got[0].Item.ID).Scan(&itemStatus); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if itemStatus != "claimed" {
		t.Fatalf("item status = %q after rejected starts, want claimed", itemStatus)
	}
	if err := StartRun(ctx, db, clock, runID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := StartRun(ctx, db, clock, runID, node); err == nil {
		t.Fatal("StartRun started the same run twice")
	}
}

// A start is only legitimate while the item is still claimed. Cancelled (or
// requeued) work must not be resurrected by a late start, and the refusal must
// leave the run exactly where it was — the item and its run still agree that
// nothing started.
func TestStartRunRefusesAnItemThatIsNoLongerClaimed(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	got, err := Claim(ctx, db, clock, node, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("Claim: %v", err)
	}
	// An operator cancels while the claim is still in flight: the run row has
	// not been settled yet, so only the item records the cancellation.
	if _, err := db.ExecContext(ctx, `UPDATE work_items SET status='cancelled' WHERE id=?`, id); err != nil {
		t.Fatalf("cancel item: %v", err)
	}

	if err := StartRun(ctx, db, clock, got[0].RunID, node); err == nil {
		t.Fatal("StartRun started a run for a cancelled item")
	}

	var itemStatus, runStatus string
	if err := db.QueryRow(`SELECT status FROM work_items WHERE id=?`, id).Scan(&itemStatus); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM runs WHERE id=?`, got[0].RunID).Scan(&runStatus); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if itemStatus != "cancelled" || runStatus != "claimed" {
		t.Fatalf("item/run = %q/%q, want cancelled/claimed (a refused start changes nothing)", itemStatus, runStatus)
	}
}

// StartRun is a state change like any other: it records the start time and the
// audit event in the same transaction as the transition, so a run can never be
// "running" without the board knowing when it started and that it happened.
func TestStartRunRecordsTheStartTimeAndAnEvent(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	got, err := Claim(ctx, db, clock, node, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("Claim: %v", err)
	}
	claimedAt := clock.Now()

	clock.Advance(5 * time.Second)
	if err := StartRun(ctx, db, clock, got[0].RunID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	var status string
	var startedAt int64
	if err := db.QueryRow(`SELECT status, started_at FROM runs WHERE id=?`, got[0].RunID).
		Scan(&status, &startedAt); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != "running" {
		t.Fatalf("run status = %q, want running", status)
	}
	if startedAt != clock.Now() || startedAt == claimedAt {
		t.Fatalf("started_at = %d, want the start time %d, not the claim time %d", startedAt, clock.Now(), claimedAt)
	}

	// The item and its run must never disagree about whether work has started.
	var itemStatus string
	if err := db.QueryRow(`SELECT status FROM work_items WHERE id=?`, got[0].Item.ID).Scan(&itemStatus); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if itemStatus != "running" {
		t.Fatalf("item status = %q, want running (the item must follow its run)", itemStatus)
	}

	events, err := ListEvents(ctx, db, "work_item", got[0].Item.ID, 50)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Type == "started" {
			found = true
			if !strings.Contains(e.Payload, got[0].RunID) {
				t.Fatalf("started event payload %q does not name run %q", e.Payload, got[0].RunID)
			}
		}
	}
	if !found {
		t.Fatalf("no 'started' event for item %s: %+v", got[0].Item.ID, events)
	}
}

// RunReaper is the only long-lived loop in the package: if it does not honour
// ctx it strands a goroutine for the life of the process.
func TestRunReaperStopsWhenTheContextIsCancelled(t *testing.T) {
	srv, _ := newServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.RunReaper(ctx, time.Millisecond)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunReaper did not return after its context was cancelled")
	}
}

// C1: nothing used to take a node offline after registration. A laptop that
// closed its lid stayed 'online' forever, so placement kept choosing it and
// /why answered "placed: true" with a happy reason for work nothing would ever
// claim — which inverts both the endpoint and the product.
//
// The sweep lives in the reaper, so this drives it through Reap and then asks
// the operator's own question through Explain.
func TestReapTakesASilentNodeOfflineAndWhySaysSo(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "laptop", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	itemID, err := CreateItem(ctx, db, clock, CreateItemRequest{
		Title: "t", Brief: "b", Needs: []string{"agentic"},
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	// An operator pause is a decision, not silence: the sweep must never
	// overwrite it, or pausing a node would last only until the next tick.
	paused := SeedNodeForTest(t, db, clock, "pi", "steward", "reflex")
	if _, err := db.ExecContext(ctx, `UPDATE nodes SET status='paused' WHERE id=?`, paused); err != nil {
		t.Fatalf("pause node: %v", err)
	}

	// While the laptop is beating, the work is placeable — the answer the
	// operator gets for a healthy board.
	ex, err := Explain(ctx, db, clock, itemID)
	if err != nil {
		t.Fatalf("Explain while online: %v", err)
	}
	if !ex.Placed {
		t.Fatalf("why says not placed while the node is online: %s %+v", ex.Reason, ex.Rejects)
	}

	// Exactly one window of silence is not yet stale: the rule is "older than".
	clock.Advance(nodeStaleAfter * time.Second)
	if res, err := Reap(ctx, db, clock); err != nil {
		t.Fatalf("Reap at the boundary: %v", err)
	} else if res.NodesOffline != 0 {
		t.Fatalf("reap at exactly one window took %d nodes offline, want 0", res.NodesOffline)
	}

	// A second past the window, the node is taken offline.
	clock.Advance(1 * time.Second)
	res, err := Reap(ctx, db, clock)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if res.NodesOffline != 1 {
		t.Fatalf("NodesOffline = %d, want 1 (the silent laptop; the paused node is not silence)", res.NodesOffline)
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM nodes WHERE id=?`, node).Scan(&status); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if status != "offline" {
		t.Fatalf("node status = %q, want offline after a silent window", status)
	}
	if err := db.QueryRow(`SELECT status FROM nodes WHERE id=?`, paused).Scan(&status); err != nil {
		t.Fatalf("read paused node: %v", err)
	}
	if status != "paused" {
		t.Fatalf("paused node status = %q, want the operator pause preserved", status)
	}

	// The sweep is idempotent: the next pass has nothing left to change, so a
	// slow heartbeat cannot be counted twice.
	if res, err := Reap(ctx, db, clock); err != nil {
		t.Fatalf("second Reap: %v", err)
	} else if res.NodesOffline != 0 {
		t.Fatalf("second reap took %d nodes offline, want 0", res.NodesOffline)
	}

	// /why, asked again, must now name the reason instead of pretending the
	// work is placeable.
	ex, err = Explain(ctx, db, clock, itemID)
	if err != nil {
		t.Fatalf("Explain after the sweep: %v", err)
	}
	if ex.Placed {
		t.Fatalf("why still says placed after the node went offline; reason %q", ex.Reason)
	}
	var named bool
	for _, r := range ex.Rejects {
		if r.NodeName == "laptop" && strings.Contains(r.Why, "offline") {
			named = true
		}
	}
	if !named {
		t.Fatalf("why rejects %+v do not name the silent node as offline", ex.Rejects)
	}
	if ex.BlockedReason == "" {
		t.Fatal("why has no blocked_reason after the node went offline")
	}
}
