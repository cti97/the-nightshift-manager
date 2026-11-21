// internal/board/checkpoints_test.go
package board

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestACheckpointIsStoredWithTheNextSeq: seq is per run and monotonic, and each write is an
// event the dashboard can read.
func TestACheckpointIsStoredWithTheNextSeq(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	_, runID, nodeID := parkOneRun(t, db, clock)

	for i, progress := range []string{"half done", "nearly done"} {
		cp, err := WriteCheckpoint(ctx, db, clock, runID, nodeID, CheckpointRequest{DeclaredProgress: progress})
		if err != nil {
			t.Fatalf("WriteCheckpoint %d: %v", i, err)
		}
		if cp.Seq != i+1 {
			t.Fatalf("seq = %d, want %d", cp.Seq, i+1)
		}
		if cp.DeclaredProgress != progress {
			t.Fatalf("progress = %q, want %q", cp.DeclaredProgress, progress)
		}
	}
	list, err := CheckpointsForRun(ctx, db, runID)
	if err != nil {
		t.Fatalf("CheckpointsForRun: %v", err)
	}
	if len(list) != 2 || list[0].Seq != 1 || list[1].Seq != 2 {
		t.Fatalf("checkpoints = %+v, want two in seq order", list)
	}
	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE entity='run' AND entity_id=? AND type='checkpoint'`, runID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 2 {
		t.Fatalf("checkpoint events = %d, want 2", events)
	}
}

// TestOnlyTheRunsNodeMayCheckpoint: the node token is the board's only claim of identity,
// and it must bound this write as it bounds a report.
func TestOnlyTheRunsNodeMayCheckpoint(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	_, runID, _ := parkOneRun(t, db, clock)
	other := SeedNodeForTest(t, db, clock, "b", "executor", "agentic")

	_, err := WriteCheckpoint(ctx, db, clock, runID, other, CheckpointRequest{DeclaredProgress: "mine now"})
	if !errors.Is(err, ErrNotYourRun) {
		t.Fatalf("another node's checkpoint = %v, want ErrNotYourRun", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM checkpoints`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("checkpoints = %d after a refused write, want none", n)
	}
}

// TestACheckpointOnAFinishedRunIsRefused: a run that has reported has nothing to resume
// from, and attaching progress to it would describe an attempt that already ended.
func TestACheckpointOnAFinishedRunIsRefused(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	_, runID, nodeID := parkOneRun(t, db, clock)
	if _, err := Report(ctx, db, clock, runID, nodeID, ReportRequest{Status: "ok", Summary: "done"}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	_, err := WriteCheckpoint(ctx, db, clock, runID, nodeID, CheckpointRequest{DeclaredProgress: "too late"})
	if !errors.Is(err, ErrRunNotLive) {
		t.Fatalf("a checkpoint after the report = %v, want ErrRunNotLive", err)
	}
}

// TestACheckpointNeedsProgress: the empty checkpoint is refused before any transaction, so
// a run that writes one per tool call cannot fill the table with markers that say nothing.
func TestACheckpointNeedsProgress(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	_, runID, nodeID := parkOneRun(t, db, clock)
	if _, err := WriteCheckpoint(ctx, db, clock, runID, nodeID, CheckpointRequest{DeclaredProgress: "   "}); err == nil {
		t.Fatal("an empty checkpoint was accepted")
	}
}

// TestAFirstClaimCarriesNoCheckpoint: a first attempt has nothing to resume from, and a
// payload with an empty checkpoint object would read as a marker the run must honour.
func TestAFirstClaimCarriesNoCheckpoint(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	if _, err := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	claimed, err := Claim(ctx, db, clock, node, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim = %d, err %v", len(claimed), err)
	}
	if claimed[0].PriorCheckpoint != nil {
		t.Fatalf("a first claim carries a checkpoint: %+v", claimed[0].PriorCheckpoint)
	}
}

// TestTheSecondClaimCarriesTheFirstCheckpoint: the requeue path is the one checkpoints
// exist for. Attempt 1 writes a marker and dies; attempt 2's claim must carry it, and it
// must be the newest marker across the item's runs, not the newest seq.
func TestTheSecondClaimCarriesTheFirstCheckpoint(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	itemID, runID, nodeID := parkOneRun(t, db, clock)

	for _, progress := range []string{"parsed the input", "wrote half the output"} {
		if _, err := WriteCheckpoint(ctx, db, clock, runID, nodeID, CheckpointRequest{DeclaredProgress: progress}); err != nil {
			t.Fatalf("WriteCheckpoint: %v", err)
		}
	}
	// The run dies without reporting; the reaper requeues it.
	clock.Advance(10 * time.Minute)
	if _, err := Reap(ctx, db, clock); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	claimed, err := Claim(ctx, db, clock, nodeID, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("second Claim = %d, err %v", len(claimed), err)
	}
	got := claimed[0].PriorCheckpoint
	if got == nil {
		t.Fatal("the second claim carries no checkpoint")
	}
	if got.DeclaredProgress != "wrote half the output" || got.Seq != 2 {
		t.Fatalf("prior checkpoint = %+v, want the newest marker of the first attempt", got)
	}
	if claimed[0].Item.ID != itemID {
		t.Fatalf("claimed item = %s, want %s", claimed[0].Item.ID, itemID)
	}
}

// TestTwoCheckpointsAtOnceGetDistinctSeqs: seq is allocated inside BEGIN IMMEDIATE, so two
// writes racing on one run serialize into 1 and 2 rather than colliding on the unique index.
func TestTwoCheckpointsAtOnceGetDistinctSeqs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	db, clock := OpenForTest(t, path)
	ctx := context.Background()
	_, runID, nodeID := parkOneRun(t, db, clock)
	db2, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open second handle: %v", err)
	}
	defer db2.Close()

	errs := make(chan error, 2)
	seqs := make(chan int, 2)
	for i, handle := range []*sql.DB{db, db2} {
		go func(i int, handle *sql.DB) {
			cp, err := WriteCheckpoint(ctx, handle, clock, runID, nodeID, CheckpointRequest{
				DeclaredProgress: []string{"half", "three quarters"}[i],
			})
			if err != nil {
				errs <- err
				return
			}
			seqs <- cp.Seq
			errs <- nil
		}(i, handle)
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("a racing checkpoint failed: %v", err)
		}
	}
	close(seqs)
	var got []int
	for seq := range seqs {
		got = append(got, seq)
	}
	slices.Sort(got)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("seqs = %v, want 1 and 2", got)
	}
}
