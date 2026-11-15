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
type ReportRequest struct {
	Status     string `json:"status"` // "ok" | "error"
	Summary    string `json:"summary"`
	TokensIn   int    `json:"tokens_in"`
	TokensOut  int    `json:"tokens_out"`
	Error      string `json:"error"`
	Transcript string `json:"transcript_path"`
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
	// in one transaction. This is the whole idempotency guard: it is read and
	// the write happens inside the same transaction, so two racing reports
	// cannot both see a live run.
	if runStatus != "claimed" && runStatus != "running" {
		var itemStatus string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM work_items WHERE id=?`, itemID).
			Scan(&itemStatus); err != nil {
			return ReportResult{}, err
		}
		return ReportResult{Applied: false, ItemStatus: itemStatus}, nil
	}

	finalRun, itemStatus := "ok", "done"
	if req.Status != "ok" {
		finalRun, itemStatus = "error", "failed"
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE runs SET status=?, ended_at=?, input_tokens=?, output_tokens=?, error=?, transcript_path=?
		  WHERE id=?`,
		finalRun, clock.Now(), req.TokensIn, req.TokensOut,
		nullable(req.Error), nullable(req.Transcript), runID); err != nil {
		return ReportResult{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE work_items SET status=?, result_summary=?, claimed_by=NULL, lease_expires_at=NULL, updated_at=?
		  WHERE id=?`,
		itemStatus, nullable(req.Summary), clock.Now(), itemID); err != nil {
		return ReportResult{}, err
	}
	// The attempt is in the payload because it is the run ordinal the operator
	// will use to line this event up with the claim and start events.
	if err := AppendEvent(ctx, tx, clock, "node:"+nodeID, "work_item", itemID,
		"reported", map[string]any{"run": runID, "status": finalRun, "attempt": attempt}); err != nil {
		return ReportResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReportResult{}, err
	}
	return ReportResult{Applied: true, ItemStatus: itemStatus}, nil
}
