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
	if err := StartRun(ctx, db, clock, runID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := StartRun(ctx, db, clock, runID, node); err == nil {
		t.Fatal("StartRun started the same run twice")
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
