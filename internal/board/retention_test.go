package board

import (
	"context"
	"testing"
	"time"
)

func countEvents(t *testing.T, srv *Server) int {
	t.Helper()
	var n int
	if err := srv.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestRetentionPrunesOldEventsAndKeepsRuns: the growth is the audit spine; the ledger is not touched.
func TestRetentionPrunesOldEventsAndKeepsRuns(t *testing.T) {
	srv, clock := newServer(t)
	ctx := context.Background()
	if _, err := CreateItem(ctx, srv.db, clock, CreateItemRequest{Title: "old work", Brief: "b"}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	// A real run, so "retention never deletes runs" is a claim about a table with rows in it. The
	// fixture claims the item, which is what creates the run.
	if _, err := srv.db.Exec(`UPDATE work_items SET status='queued'`); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	node := SeedNodeForTest(t, srv.db, clock, "n", "executor", "agentic")
	SeedEndpointForTest(t, srv.db, clock, "local", "host", 0, "tools")
	if _, err := srv.db.Exec(`UPDATE work_items SET needs='["agentic"]'`); err != nil {
		t.Fatalf("needs: %v", err)
	}
	if claimed, err := Claim(ctx, srv.db, clock, node, 1); err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %d, %v", len(claimed), err)
	}

	eventsBefore := countEvents(t, srv)
	var runsBefore int
	if err := srv.db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runsBefore); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	// One event is ancient, one is recent.
	if _, err := srv.db.Exec(`INSERT INTO events (at,actor,entity,entity_id,type,payload) VALUES (?,?,?,?,?,?)`,
		clock.Now()-int64(100*24*time.Hour/time.Second), "admin", "work_item", "x", "created", "{}"); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	if _, err := srv.db.Exec(`INSERT INTO events (at,actor,entity,entity_id,type,payload) VALUES (?,?,?,?,?,?)`,
		clock.Now()-int64(time.Hour/time.Second), "admin", "work_item", "x", "created", "{}"); err != nil {
		t.Fatalf("seed new: %v", err)
	}
	res, err := PruneEvents(ctx, srv.db, clock, RetentionRequest{Keep: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("deleted %d, want the one event older than 90 days", res.Deleted)
	}
	if got := countEvents(t, srv); got != eventsBefore+1 {
		t.Fatalf("%d events remain, want %d (the recent one plus the ones the fixture wrote)", got, eventsBefore+1)
	}
	var runs int
	if err := srv.db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != runsBefore {
		t.Fatalf("retention touched %d runs", runsBefore-runs)
	}
}

// TestRetentionKeepsTheFloor: a unit file that says "--retention 1m" must not be able to wipe today.
func TestRetentionKeepsTheFloor(t *testing.T) {
	srv, clock := newServer(t)
	ctx := context.Background()
	if _, err := srv.db.Exec(`INSERT INTO events (at,actor,entity,entity_id,type,payload) VALUES (?,?,?,?,?,?)`,
		clock.Now()-int64(2*time.Hour/time.Second), "admin", "work_item", "x", "created", "{}"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res, err := PruneEvents(ctx, srv.db, clock, RetentionRequest{Keep: time.Minute})
	if err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}
	if res.Deleted != 0 {
		t.Fatalf("a one-minute retention deleted %d events from two hours ago", res.Deleted)
	}
	if res.Cutoff != clock.Now()-int64(retentionFloor/time.Second) {
		t.Fatalf("cutoff = %d, want now-24h", res.Cutoff)
	}
}

// TestZeroRetentionKeepsEverything: pruning is opt-in at the call site; "0" must mean "keep", not
// "delete everything older than zero seconds".
func TestZeroRetentionKeepsEverything(t *testing.T) {
	srv, clock := newServer(t)
	if _, err := srv.db.Exec(`INSERT INTO events (at,actor,entity,entity_id,type,payload) VALUES (?,?,?,?,?,?)`,
		clock.Now()-int64(1000*24*time.Hour/time.Second), "admin", "work_item", "x", "created", "{}"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res, err := PruneEvents(context.Background(), srv.db, clock, RetentionRequest{})
	if err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}
	if res.Deleted != 0 || countEvents(t, srv) != 1 {
		t.Fatalf("zero retention deleted %d events", res.Deleted)
	}
}

// TestRetentionPrunesInBatches: the first pass against a year-old board must not hold the write lock
// for as long as it takes to delete a year of rows.
func TestRetentionPrunesInBatches(t *testing.T) {
	srv, clock := newServer(t)
	ctx := context.Background()
	old := clock.Now() - int64(200*24*time.Hour/time.Second)
	for i := 0; i < 500; i++ {
		if _, err := srv.db.Exec(`INSERT INTO events (at,actor,entity,entity_id,type,payload) VALUES (?,?,?,?,?,?)`,
			old, "admin", "work_item", "x", "created", "{}"); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	res, err := PruneEvents(ctx, srv.db, clock, RetentionRequest{Keep: 90 * 24 * time.Hour, Batch: 100})
	if err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}
	if res.Deleted != 100 || !res.More {
		t.Fatalf("first pass = %+v, want 100 deleted and More", res)
	}
	last := res
	for last.More {
		last, err = PruneEvents(ctx, srv.db, clock, RetentionRequest{Keep: 90 * 24 * time.Hour, Batch: 100})
		if err != nil {
			t.Fatalf("next pass: %v", err)
		}
	}
	if got := countEvents(t, srv); got != 0 {
		t.Fatalf("%d events survived five passes", got)
	}
}

// TestTheCutoffIsExclusiveAtTheBoundary: "keep 90 days" means an event AT the boundary is kept; the
// window is [now-keep, now], and the same inclusive/exclusive decision as the watch dedupe window.
func TestTheCutoffIsExclusiveAtTheBoundary(t *testing.T) {
	srv, clock := newServer(t)
	ctx := context.Background()
	keep := 90 * 24 * time.Hour
	cutoff := clock.Now() - int64(keep/time.Second)
	for _, at := range []int64{cutoff - 1, cutoff, cutoff + 1} {
		if _, err := srv.db.Exec(`INSERT INTO events (at,actor,entity,entity_id,type,payload) VALUES (?,?,?,?,?,?)`,
			at, "admin", "work_item", "x", "created", "{}"); err != nil {
			t.Fatalf("seed at %d: %v", at, err)
		}
	}
	res, err := PruneEvents(ctx, srv.db, clock, RetentionRequest{Keep: keep})
	if err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("deleted %d, want only the event before the boundary", res.Deleted)
	}
}
