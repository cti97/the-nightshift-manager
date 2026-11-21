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
// ErrStaleLease is a write from a run that the reaper settled: the lease is gone, the item
// may already be running elsewhere, and the client must abandon the run rather than publish
// it. It is deliberately different from the idempotent "already settled" answer, because one
// is a retry and the other is a partition that healed (spec §22.1).
var ErrStaleLease = errors.New("the run's lease is gone: the run was reaped and its item requeued")

func Report(ctx context.Context, db *sql.DB, clock Clock, runID, nodeID string, req ReportRequest) (ReportResult, error) {
	// An unknown status is a malformed report, not a failure. Treating "" (a
	// body that omitted the field) as "error" would permanently fail an item
	// that may well have succeeded.
	if req.Status != "ok" && req.Status != "error" {
		return ReportResult{}, fmt.Errorf("report status %q is neither ok nor error", req.Status)
	}
	// Token counts are spend inputs, not display fields: a negative one would
	// subtract from the daily ceiling and under-count what a run cost. The
	// executor clamps provider-reported negatives, but the report boundary is
	// where a hand-written or buggy node is stopped — the same fail-closed rule
	// the status field follows.
	for _, field := range []struct {
		name  string
		value int
	}{
		{"tokens_in", req.TokensIn},
		{"tokens_out", req.TokensOut},
		{"reserved_tokens", req.ReservedTokens},
	} {
		if field.value < 0 {
			return ReportResult{}, fmt.Errorf("report %s %d is negative; token counts are spend inputs", field.name, field.value)
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ReportResult{}, err
	}
	defer func() { _ = tx.Rollback() }() // a commit makes this a no-op

	// The node_id in the WHERE clause is the ownership check: a node reports
	// only its own runs, and someone else's run is an error, not a no-op.
	//
	// The model's prices come back with the same read, because the cost is part
	// of settling the run. Left-joined: runs.model_id is nullable and a run
	// without a model predates pricing, which costs 0 rather than failing the
	// report of work that really happened.
	var itemID, runStatus string
	var attempt int
	var costIn, costOut float64
	if err := tx.QueryRowContext(ctx,
		`SELECT r.work_item_id, r.status, r.attempt,
		        COALESCE(m.cost_in_per_mtok,0), COALESCE(m.cost_out_per_mtok,0)
		   FROM runs r
		   LEFT JOIN models m ON m.id = r.model_id
		  WHERE r.id=? AND r.node_id=?`,
		runID, nodeID).Scan(&itemID, &runStatus, &attempt, &costIn, &costOut); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ReportResult{}, fmt.Errorf("run %s is not a live run owned by node %s", runID, nodeID)
		}
		return ReportResult{}, fmt.Errorf("run %s: %w", runID, err)
	}

	// Anything that is no longer claimed or running has already been settled —
	// by an earlier report, or by the reaper, which settles the run and the item
	// in one transaction. Either way nothing is written here, but the two cases
	// are not the same answer (M6 fence):
	//
	//   - settled by a report: this is a retry of the same call, and idempotency
	//     means 200 with applied:false. The client has nothing to fix.
	//   - settled by the reaper: the run was lost, its item requeued, and another
	//     node may already be running it. Telling that client "not applied" is too
	//     quiet — it may have pushed a branch already. It gets a 409 stale_lease,
	//     which the client's rule turns into "abandon this run and publish nothing".
	if runStatus != "claimed" && runStatus != "running" {
		res, err := settledResult(ctx, tx, itemID)
		if err != nil {
			return ReportResult{}, err
		}
		if runStatus == "lost" || runStatus == "abandoned" {
			return res, fmt.Errorf("%w (run %s is %s; item %s is %s)", ErrStaleLease, runID, runStatus, itemID, res.ItemStatus)
		}
		return res, nil
	}

	finalRun, itemStatus := "ok", "done"
	if req.Status != "ok" {
		finalRun, itemStatus = "error", "failed"
	}

	// The run's cost is priced here, once, and frozen on the row. Not derived
	// later from the model: a price edit must not rewrite a day that has already
	// been spent, or the number the dashboard showed yesterday would move
	// underneath the operator and a typo'd price would retroactively trip the
	// ceiling. See RunCost for how reserved tokens are priced.
	cost, err := RunCost(req.TokensIn, req.TokensOut, req.ReservedTokens, costIn, costOut)
	if err != nil {
		// The run is left live and unpriced rather than settled with a number
		// the ledger cannot add up. Report fails closed here; pricing at zero
		// would silently make a corrupt model free for the rest of the day.
		return ReportResult{}, fmt.Errorf("run %s: %w", runID, err)
	}

	// Both writes carry the live-status predicate and both check their row
	// count. The read above and these writes are separate statements, so a
	// writer that commits in between (a reaper pass, another report) must win:
	// zero rows means this call settled nothing, and the deferred rollback
	// discards any partial write. That is what makes exactly-once true on this
	// function's own logic, rather than on the connection pool's serialisation.
	res, err := tx.ExecContext(ctx,
		`UPDATE runs SET status=?, ended_at=?, input_tokens=?, output_tokens=?, reserved_tokens=?, cost_estimate=?, error=?, transcript_path=?
		  WHERE id=? AND status IN ('claimed','running')`,
		finalRun, clock.Now(), req.TokensIn, req.TokensOut, req.ReservedTokens, cost,
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
		"reported", map[string]any{"run": runID, "status": finalRun, "run_attempt": attempt, "cost": cost}); err != nil {
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
