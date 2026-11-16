package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ApprovalRequest is why a run stopped and what it wants. Stored, not evented,
// so the approvals page is one query rather than a replay (M3 D1).
type ApprovalRequest struct {
	Action      string   `json:"action"`
	Reason      string   `json:"reason"`
	Options     []string `json:"options"`
	RunID       string   `json:"run_id"`
	RequestedBy string   `json:"requested_by"` // node id
	RequestedAt int64    `json:"requested_at"`
}

// MarshalJSON writes an empty Options list as [] rather than null, matching
// emptyJSONList: the stored bytes of a request must not depend on whether the
// caller passed nil or an empty slice, and a JSON consumer iterating the
// options must not meet null.
//
// The wire type is a distinct type, not an alias, so it carries no MarshalJSON
// method and encoding the fields cannot recurse.
func (a ApprovalRequest) MarshalJSON() ([]byte, error) {
	type wire ApprovalRequest
	w := wire(a)
	if len(w.Options) == 0 {
		w.Options = []string{}
	}
	return json.Marshal(w)
}

// Approval is one parked item as the approvals page lists it: the request's
// fields lifted to the top level, so the HTML template and the JSON body read
// the same shape and a script cannot get a different answer from the page.
type Approval struct {
	ItemID      string   `json:"item_id"`
	Title       string   `json:"title"`
	Action      string   `json:"action"`
	Reason      string   `json:"reason"`
	Options     []string `json:"options"`
	RequestedBy string   `json:"requested_by"`
	RequestedAt int64    `json:"requested_at"`
	Attempt     int      `json:"attempt"`
}

// ListApprovals returns every parked item, oldest first. One query, because D1
// stored the request on the item instead of eventing it; the partial index
// idx_items_parked serves the WHERE and the ORDER BY together.
//
// An empty board returns an empty slice, not nil: the route answers [] so a
// caller can tell "nothing is waiting" from "the request failed".
//
// A parked row with no request — parked before migration 4 added the column, or
// written by a node whose body carried no action before the board refused it —
// is still listed. It is waiting on a human either way, and a queue that hides
// the rows it cannot describe is worse than one that shows an empty action. A
// corrupt JSON cell is reported through reportCorruptRow and listed the same
// way, for the same reason the claim path skips and reports rather than 500s:
// one hand-edited cell must not hide every other approval.
func ListApprovals(ctx context.Context, db *sql.DB) ([]Approval, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, title, approval_request, attempt
		   FROM work_items WHERE status='needs_approval' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Approval{}
	for rows.Next() {
		var a Approval
		var raw sql.NullString
		if err := rows.Scan(&a.ItemID, &a.Title, &raw, &a.Attempt); err != nil {
			return nil, err
		}
		a.Options = []string{}
		if raw.Valid && raw.String != "" {
			var req ApprovalRequest
			if err := json.Unmarshal([]byte(raw.String), &req); err != nil {
				reportCorruptRow(CorruptRow{Table: "work_items", RowID: a.ItemID, Column: "approval_request", Reason: err.Error()})
			} else {
				a.Action, a.Reason = req.Action, req.Reason
				a.Options = nonNilList(req.Options)
				a.RequestedBy, a.RequestedAt = req.RequestedBy, req.RequestedAt
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Escalate parks a run's item for a human and closes the run, in one
// transaction (spec §11; M3 D1, D5, D6).
//
// The item moves to needs_approval with the request stored on it, so the
// approvals page is one query rather than a replay; the run moves to
// escalated with ended_at, so the reaper's lease scan cannot see it; the lease
// is cleared, so no worker holds anything open while a human thinks. That last
// one is the whole point of the design: a sleeping worker is a lost node away
// from an item nobody can recover.
//
// The attempt is neither refunded nor grown (D5). The run spent model tokens
// before it asked, so the attempt stays spent — refunding it here would let a
// run that escalates on every attempt burn the item's budget forever. The run
// that follows a decision is not a retry of this one, so nothing increments
// either: the next claim charges its own attempt, by its own ordinal.
//
// Both writes carry a status guard and both check their row count. A row that
// settled between the claim and this call — a report that arrived first, a
// reaper pass — must win, and the deferred rollback discards whatever this call
// had already written.
func Escalate(ctx context.Context, db *sql.DB, clock Clock, runID, nodeID string, req ApprovalRequest) error {
	// A request with no action cannot be approved: a human is being asked to
	// allow something, and D3's grant names that something. Refusing here keeps
	// the approvals page from listing a question with no answer in it.
	if req.Action == "" {
		return fmt.Errorf("an approval request must name the action it wants")
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // a commit makes this a no-op

	// Re-read the run inside the transaction. It is the only evidence that this
	// node owns the work, and its status decides whether there is still anything
	// to park.
	var itemID, runStatus string
	if err := tx.QueryRowContext(ctx,
		`SELECT work_item_id, status FROM runs WHERE id=? AND node_id=?`,
		runID, nodeID).Scan(&itemID, &runStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run %s is not a live run owned by node %s", runID, nodeID)
		}
		return fmt.Errorf("run %s: %w", runID, err)
	}
	if runStatus != "claimed" && runStatus != "running" {
		return fmt.Errorf("run %s is already %s; an escalation cannot re-open it", runID, runStatus)
	}

	now := clock.Now()
	// The board stamps identity and time. The node supplies what it wants and
	// why; a node that named its own requester or timestamp would be writing the
	// audit spine, and D1's page reads these fields as fact.
	req.RunID = runID
	req.RequestedBy = nodeID
	req.RequestedAt = now
	raw, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal approval request: %w", err)
	}

	// approval_granted is cleared: a parked item holds a question, not an
	// answer. A grant left from an earlier decision names a different action
	// (D3), and leaving it on the row would let a later run match it.
	res, err := tx.ExecContext(ctx,
		`UPDATE work_items
		    SET status='needs_approval', approval_request=?, approval_granted=NULL,
		        claimed_by=NULL, lease_expires_at=NULL, updated_at=?
		  WHERE id=? AND status IN ('claimed','running') AND claimed_by=?`,
		string(raw), now, itemID, nodeID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("work item %s is no longer claimed by node %s; nothing was parked", itemID, nodeID)
	}

	res, err = tx.ExecContext(ctx,
		`UPDATE runs SET status='escalated', ended_at=? WHERE id=? AND status IN ('claimed','running')`,
		now, runID)
	if err != nil {
		return err
	}
	if n, err = res.RowsAffected(); err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("run %s is no longer live; nothing was parked", runID)
	}

	if err := AppendEvent(ctx, tx, clock, "node:"+nodeID, "work_item", itemID, "escalated",
		map[string]any{
			"run": runID, "action": req.Action, "reason": req.Reason,
			"options": nonNilList(req.Options),
		}); err != nil {
		return err
	}
	return tx.Commit()
}
