// internal/board/reaper.go
package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// StartRun moves a run from claimed to running, and records the start time and
// the audit event in the same transaction as the transition. This single
// transition is what lets the reaper tell a claim that never started from a run
// that died mid-flight, and therefore whether the attempt should be charged.
//
// It is transactional for the same reason every other board write is: a report
// can land immediately after a bare UPDATE, and a transition without an event
// would be invisible to the dashboard that projects state from the audit spine.
func StartRun(ctx context.Context, db *sql.DB, clock Clock, runID, nodeID string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // a commit makes this a no-op

	// Read inside the transaction so the guard, the timestamp and the event all
	// describe the same row. The run is the only evidence that a start happened,
	// so a start that is not recorded here is a start the reaper cannot see.
	var itemID string
	if err := tx.QueryRowContext(ctx,
		`SELECT work_item_id FROM runs WHERE id=? AND node_id=? AND status='claimed'`,
		runID, nodeID).Scan(&itemID); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("run %s is not a claimable run for node %s", runID, nodeID)
		}
		return err
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE runs SET status='running', started_at=? WHERE id=? AND status='claimed'`,
		clock.Now(), runID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("run %s is not a claimable run for node %s", runID, nodeID)
	}
	if err := AppendEvent(ctx, tx, clock, "node:"+nodeID, "work_item", itemID, "started",
		map[string]any{"run": runID}); err != nil {
		return err
	}
	return tx.Commit()
}

// ReapResult counts what one pass changed. Abandoned and Lost are run
// outcomes and are mutually exclusive; Failed is an item outcome, so a lost
// run whose item also exhausted its budget increments both.
type ReapResult struct {
	Abandoned int `json:"abandoned"`
	Lost      int `json:"lost"`
	Failed    int `json:"failed"`
}

// reapOutcome is what settling one item did. A skipped item was settled or its
// lease was renewed between the scan and its transaction, and was left alone.
type reapOutcome struct {
	run  string // "abandoned", "lost", or "skipped"
	item string // "queued" or "failed"; empty when skipped
}

// Reap requeues work whose lease expired. The run's status decides whether the
// attempt is spent: `claimed` never started, so the claim's attempt is refunded;
// `running` may have had side effects, so it is kept and burns the budget
// (spec §8).
//
// An item that cannot be settled (a corrupted board) is reported through the
// returned error but does not stop the pass: the rest of the expired leases are
// still recovered.
func Reap(ctx context.Context, db *sql.DB, clock Clock) (ReapResult, error) {
	now := clock.Now()
	ids, err := expiredItemIDs(ctx, db, now)
	if err != nil {
		return ReapResult{}, err
	}

	var res ReapResult
	var errs []error
	for _, id := range ids {
		out, err := reapItem(ctx, db, clock, id, now)
		if err != nil {
			// One corrupt item must not stop the pass: the scan order is stable,
			// so returning here would starve every other lease forever. Collect
			// and keep going; RunReaper logs the joined error.
			errs = append(errs, err)
			continue
		}
		switch out.run {
		case "lost":
			res.Lost++
		case "abandoned":
			res.Abandoned++
		}
		if out.item == "failed" {
			res.Failed++
		}
	}
	return res, errors.Join(errs...)
}

// expiredItemIDs is the scan. It selects ids only: every column that decides an
// outcome is re-read inside reapItem's transaction, because anything read here
// can be stale by the time the write happens.
func expiredItemIDs(ctx context.Context, db *sql.DB, now int64) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id FROM work_items
		  WHERE status IN ('claimed','running') AND lease_expires_at IS NOT NULL AND lease_expires_at < ?`,
		now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// reapItem settles one expired claim in its own transaction.
func reapItem(ctx context.Context, db *sql.DB, clock Clock, itemID string, now int64) (reapOutcome, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return reapOutcome{}, err
	}
	defer func() { _ = tx.Rollback() }() // a commit makes this a no-op

	// The scan ran outside a transaction. Anything that settled this item (a
	// report) or renewed its lease (a heartbeat) since then must win: rewinding
	// a settled run is the one thing this board must never do silently.
	var (
		status      string
		attempt     int
		maxAttempts int
		lease       sql.NullInt64
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT status, attempt, max_attempts, lease_expires_at FROM work_items WHERE id=?`, itemID).
		Scan(&status, &attempt, &maxAttempts, &lease); err != nil {
		if err == sql.ErrNoRows {
			return reapOutcome{run: "skipped"}, nil
		}
		return reapOutcome{}, err
	}
	if status != "claimed" && status != "running" {
		return reapOutcome{run: "skipped"}, nil
	}
	if !lease.Valid || lease.Int64 >= now {
		return reapOutcome{run: "skipped"}, nil
	}

	runID, runStatus, err := currentRun(ctx, tx, itemID)
	if err != nil {
		return reapOutcome{}, err
	}

	// No run to settle. A claimed item cannot have started — Claim and StartRun
	// both need the run row — so it is safe to refund the claim. An item that
	// says it is running is a different story: its work may have started, and
	// requeueing it would execute that work twice.
	if runID == "" && status != "claimed" {
		return reapOutcome{}, fmt.Errorf(
			"work item %s is %q but has no non-terminal run; refusing to requeue work that may have started",
			itemID, status)
	}
	started := runStatus == "running"
	nextRunStatus := "abandoned"
	if started {
		nextRunStatus = "lost"
	}

	// Attempt accounting (spec §8): work_items.attempt counts attempts that
	// actually started. A lost run keeps the attempt its claim charged; a claim
	// that never started is refunded, so a node that dies between claim and
	// start costs the item nothing. That is why the budget can only fire for a
	// started run — and Claim deliberately never checks the budget, making this
	// the one place an over-budget item is failed.
	itemStatus := "queued"
	nextAttempt := attempt
	if started {
		if attempt >= maxAttempts {
			itemStatus = "failed"
		}
	} else if nextAttempt > 0 {
		nextAttempt--
	}

	if runID != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE runs SET status=?, ended_at=?, error=? WHERE id=?`,
			nextRunStatus, now, "lease expired", runID); err != nil {
			return reapOutcome{}, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE work_items SET status=?, claimed_by=NULL, lease_expires_at=NULL, attempt=?, updated_at=? WHERE id=?`,
		itemStatus, nextAttempt, now, itemID); err != nil {
		return reapOutcome{}, err
	}
	// The attempt change is part of the event: a refund is the one audit question
	// an operator will ask ("why did the budget go back down?").
	if err := AppendEvent(ctx, tx, clock, "system", "work_item", itemID, "reaped",
		map[string]any{
			"run": nextRunStatus, "item": itemStatus,
			"attempt_from": attempt, "attempt_to": nextAttempt,
		}); err != nil {
		return reapOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return reapOutcome{}, err
	}
	return reapOutcome{run: nextRunStatus, item: itemStatus}, nil
}

// currentRun returns the item's single non-terminal run. An item is requeued
// only in the same transaction that settles its run (Claim inserts a run only
// for a queued item), so at most one run per item is ever non-terminal. Seeing
// two is a corrupted board: guessing which one owns the lease would corrupt it
// further, so the reaper fails loudly instead.
func currentRun(ctx context.Context, tx *sql.Tx, itemID string) (id, status string, err error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, status FROM runs WHERE work_item_id=? AND status IN ('claimed','running')`, itemID)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()

	var n int
	for rows.Next() {
		var runID, runStatus string
		if err := rows.Scan(&runID, &runStatus); err != nil {
			return "", "", err
		}
		n++
		if n == 1 {
			id, status = runID, runStatus
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	if n > 1 {
		return "", "", fmt.Errorf("work item %s has %d non-terminal runs; refusing to guess which one owns the lease", itemID, n)
	}
	return id, status, nil
}

// RunReaper ticks forever; cancel ctx to stop it.
func (s *Server) RunReaper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := Reap(ctx, s.db, s.clock); err != nil {
				s.logf("reaper: %v", err)
			}
		}
	}
}
