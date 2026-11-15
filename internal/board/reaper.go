// internal/board/reaper.go
package board

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// StartRun moves a run from claimed to running. This single transition is what
// lets the reaper tell a claim that never started from a run that died
// mid-flight, and therefore whether the attempt should be charged.
//
// The clock is accepted for signature parity with every other board writer and
// because Task 10's start handler passes it; nothing here needs a timestamp:
// started_at is the claim time, and the attempt is already recorded.
func StartRun(ctx context.Context, db *sql.DB, clock Clock, runID, nodeID string) error {
	res, err := db.ExecContext(ctx,
		`UPDATE runs SET status='running' WHERE id=? AND node_id=? AND status='claimed'`,
		runID, nodeID)
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
	return nil
}

// ReapResult counts what one pass changed. Abandoned and Lost are run
// outcomes and are mutually exclusive; Failed is an item outcome, so a lost
// run whose item also exhausted its budget increments both.
type ReapResult struct {
	Abandoned int `json:"abandoned"`
	Lost      int `json:"lost"`
	Failed    int `json:"failed"`
}

// Reap requeues work whose lease expired. The run's status decides whether the
// attempt is spent: `claimed` never started, so it costs nothing; `running`
// may have had side effects, so it burns an attempt (spec §8).
func Reap(ctx context.Context, db *sql.DB, clock Clock) (ReapResult, error) {
	var res ReapResult
	now := clock.Now()

	rows, err := db.QueryContext(ctx,
		`SELECT id, attempt, max_attempts FROM work_items
		  WHERE status IN ('claimed','running') AND lease_expires_at IS NOT NULL AND lease_expires_at < ?`,
		now)
	if err != nil {
		return res, err
	}
	type expiring struct {
		id          string
		attempt     int
		maxAttempts int
	}
	var list []expiring
	for rows.Next() {
		var e expiring
		if err := rows.Scan(&e.id, &e.attempt, &e.maxAttempts); err != nil {
			rows.Close()
			return res, err
		}
		list = append(list, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	for _, e := range list {
		// The scan above ran outside a transaction, so the run is re-read inside
		// this one: a report that landed in the gap must win, or a completed run
		// would be rewound to lost and its item requeued — the one thing this
		// board must never do silently. The pool holds a single connection, so
		// no other transaction can interleave once this one is open.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return res, err
		}

		var runID, runStatus string
		err = tx.QueryRowContext(ctx,
			`SELECT id, status FROM runs WHERE work_item_id=? AND attempt=?`, e.id, e.attempt).
			Scan(&runID, &runStatus)
		if err != nil && err != sql.ErrNoRows {
			tx.Rollback()
			return res, err
		}
		if runID != "" && runStatus != "claimed" && runStatus != "running" {
			tx.Rollback() // already settled by a report; leave it alone
			continue
		}

		nextRunStatus := "abandoned"
		if runStatus == "running" {
			nextRunStatus = "lost"
		}
		// Claim deliberately does not check the budget (see Claim), so this is
		// the only place an over-budget item is ever failed. Without it the row
		// would be requeued and then sit in queued forever, because the scan
		// only looks at claimed/running rows.
		itemStatus := "queued"
		if e.attempt >= e.maxAttempts {
			itemStatus = "failed"
		}

		if runID != "" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE runs SET status=?, ended_at=?, error=? WHERE id=?`,
				nextRunStatus, now, "lease expired", runID); err != nil {
				tx.Rollback()
				return res, err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE work_items SET status=?, claimed_by=NULL, lease_expires_at=NULL, updated_at=? WHERE id=?`,
			itemStatus, now, e.id); err != nil {
			tx.Rollback()
			return res, err
		}
		if err := AppendEvent(ctx, tx, clock, "system", "work_item", e.id, "reaped",
			map[string]any{"run": nextRunStatus, "item": itemStatus}); err != nil {
			tx.Rollback()
			return res, err
		}
		if err := tx.Commit(); err != nil {
			return res, err
		}

		// Counted only now: ReapResult must never claim work it did not commit.
		if nextRunStatus == "lost" {
			res.Lost++
		} else {
			res.Abandoned++
		}
		if itemStatus == "failed" {
			res.Failed++
		}
	}
	return res, nil
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
