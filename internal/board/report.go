// internal/board/report.go
package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ReportRequest is what a node posts when a run finishes. Status is "ok" or
// "error"; every other field is recorded for the audit trail and the spend
// view, never interpreted by the board's state machine.
//
// TokensIn and TokensOut are the provider-reported actuals. ReservedTokens is
// the separate budget charge for attempts that failed and reported no usage; it
// is not a measurement, but it is what the run actually cost in reservation, and
// the spend ledger needs both to avoid under-counting a retried run. Any other Status value
// is rejected before any write, so a body missing the field cannot fail an item.
type ReportRequest struct {
	Status         string `json:"status"` // "ok" | "error"
	Summary        string `json:"summary"`
	TokensIn       int    `json:"tokens_in"`
	TokensOut      int    `json:"tokens_out"`
	ReservedTokens int    `json:"reserved_tokens"`
	Error          string `json:"error"`
	Transcript     string `json:"transcript_path"`
}

// ReportResult says whether this call was the one that settled the run.
// Applied is false for a retry that arrived after the first report, which is a
// success, not an error: the item is already where the caller wants it.
type ReportResult struct {
	Applied    bool   `json:"applied"`
	ItemStatus string `json:"item_status"`
}

// Report completes a run and its item in one transaction. The first report
// wins; later ones are accepted and ignored, so a node that dies before reading
// the reply cannot double-complete an item (spec §15).
//
// The run and the item move together because a non-terminal run for a settled
// item is exactly the state the reaper cannot recover: it only scans claimed
// and running items, so a run left behind would stay live forever.
func Report(ctx context.Context, db *sql.DB, clock Clock, runID, nodeID string, req ReportRequest) (ReportResult, error) {
	// An unknown status is a malformed report, not a failure. Treating "" (a
	// body that omitted the field) as "error" would permanently fail an item
	// that may well have succeeded.
	if req.Status != "ok" && req.Status != "error" {
		return ReportResult{}, fmt.Errorf("report status %q is neither ok nor error", req.Status)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ReportResult{}, err
	}
	defer func() { _ = tx.Rollback() }() // a commit makes this a no-op

	// The node_id in the WHERE clause is the ownership check: a node reports
	// only its own runs, and someone else's run is an error, not a no-op.
	var itemID, runStatus string
	var attempt int
	if err := tx.QueryRowContext(ctx,
		`SELECT work_item_id, status, attempt FROM runs WHERE id=? AND node_id=?`,
		runID, nodeID).Scan(&itemID, &runStatus, &attempt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ReportResult{}, fmt.Errorf("run %s is not a live run owned by node %s", runID, nodeID)
		}
		return ReportResult{}, fmt.Errorf("run %s: %w", runID, err)
	}

	// Anything that is no longer claimed or running has already been settled —
	// by an earlier report, or by the reaper, which settles the run and the item
	// in one transaction. Returning here writes nothing at all.
	if runStatus != "claimed" && runStatus != "running" {
		return settledResult(ctx, tx, itemID)
	}

	finalRun, itemStatus := "ok", "done"
	if req.Status != "ok" {
		finalRun, itemStatus = "error", "failed"
	}

	// Both writes carry the live-status predicate and both check their row
	// count. The read above and these writes are separate statements, so a
	// writer that commits in between (a reaper pass, another report) must win:
	// zero rows means this call settled nothing, and the deferred rollback
	// discards any partial write. That is what makes exactly-once true on this
	// function's own logic, rather than on the connection pool's serialisation.
	res, err := tx.ExecContext(ctx,
		`UPDATE runs SET status=?, ended_at=?, input_tokens=?, output_tokens=?, reserved_tokens=?, error=?, transcript_path=?
		  WHERE id=? AND status IN ('claimed','running')`,
		finalRun, clock.Now(), req.TokensIn, req.TokensOut, req.ReservedTokens,
		nullable(req.Error), nullable(req.Transcript), runID)
	if err != nil {
		return ReportResult{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ReportResult{}, err
	}
	if n == 0 {
		return settledResult(ctx, tx, itemID)
	}

	res, err = tx.ExecContext(ctx,
		`UPDATE work_items SET status=?, result_summary=?, claimed_by=NULL, lease_expires_at=NULL, updated_at=?
		  WHERE id=? AND status IN ('claimed','running')`,
		itemStatus, nullable(req.Summary), clock.Now(), itemID)
	if err != nil {
		return ReportResult{}, err
	}
	n, err = res.RowsAffected()
	if err != nil {
		return ReportResult{}, err
	}
	if n == 0 {
		return settledResult(ctx, tx, itemID)
	}

	// The run attempt is namespaced in the payload: work_items.attempt counts
	// started runs while runs.attempt is the per-item claim ordinal, and a bare
	// "attempt" here is exactly the two-counter confusion this project has
	// already paid for.
	if err := AppendEvent(ctx, tx, clock, "node:"+nodeID, "work_item", itemID,
		"reported", map[string]any{"run": runID, "status": finalRun, "run_attempt": attempt}); err != nil {
		return ReportResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReportResult{}, err
	}
	return ReportResult{Applied: true, ItemStatus: itemStatus}, nil
}

// settledResult answers a report whose guarded write matched nothing, or whose
// run was already terminal when it was read: an earlier report, the reaper, or
// another writer got there first. The caller's deferred rollback discards any
// partial write, so this is a pure read of the outcome that already stands.
func settledResult(ctx context.Context, tx *sql.Tx, itemID string) (ReportResult, error) {
	var itemStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM work_items WHERE id=?`, itemID).
		Scan(&itemStatus); err != nil {
		return ReportResult{}, err
	}
	return ReportResult{Applied: false, ItemStatus: itemStatus}, nil
}
