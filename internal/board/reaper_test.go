// internal/board/reaper_test.go
package board

import (
	"context"
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

	if _, err := Claim(ctx, db, clock, node, 1); err != nil {
		t.Fatalf("Claim: %v", err)
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
// between the scan and the per-item write. That interleaving cannot be forced
// from a unit test, so this reproduces the state it leaves behind: a terminal
// run whose item still looks reapable. The reaper must neither rewind the run
// nor requeue the item — resurrecting settled work is the silent failure this
// board exists to avoid.
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

	res, err := Reap(ctx, db, clock)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if res != (ReapResult{}) {
		t.Fatalf("Reap = %+v, want nothing (a settled run is not reapable)", res)
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
	if itemStatus != "claimed" {
		t.Fatalf("item status = %q, want it left untouched", itemStatus)
	}
}

// The budget check cannot be limited to started runs: Claim does not look at
// max_attempts, and a requeued item whose attempt is already at the cap would
// otherwise cycle claimed↔queued forever while its counter grew. Reaping an
// over-budget claim as failed is what bounds a node that keeps dying between
// claim and start.
func TestReapFailsAnAbandonedClaimThatExhaustedItsAttempts(t *testing.T) {
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
	if res.Failed != 1 || res.Abandoned != 1 {
		t.Fatalf("result = %+v, want one abandoned run and one failed item", res)
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM work_items WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
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
