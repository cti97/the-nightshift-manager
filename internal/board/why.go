// internal/board/why.go
package board

import (
	"context"
	"database/sql"
	"fmt"
)

// Explanation is the answer to "why is this item not running" (spec §14).
//
// Reason summarises the decision; Rejects carries the per-candidate rule text.
// Both are returned because the summary names only the unsatisfied needs, which
// is not an explanation: the human needs to see which rule rejected which node.
type Explanation struct {
	ItemID        string   `json:"item_id"`
	Status        string   `json:"status"`
	Needs         []string `json:"needs"`
	Placed        bool     `json:"placed"`
	Reason        string   `json:"reason"`
	BlockedReason string   `json:"blocked_reason"`
	Rejects       []Reject `json:"rejects,omitempty"`

	// Paused and PauseReason are the second half of the answer, and without
	// them /why lies: placement can say "placed: true, this node would take it"
	// while the board is refusing to issue anything because the day's spend
	// ceiling was reached. The item is explainable and unexplained at the same
	// time, which is the one outcome this endpoint exists to prevent.
	Paused      bool   `json:"paused"`
	PauseReason string `json:"pause_reason,omitempty"`
}

// Explain answers for one item by re-running placement against the board as it
// is now. It is a read-only view: it writes no blocked_reason and settles
// nothing, so asking why cannot change the answer it is explaining. The clock
// is in the signature like every other board function — time is always
// server-side — but a read-only explanation has no timestamp to write.
func Explain(ctx context.Context, db *sql.DB, clock Clock, itemID string) (Explanation, error) {
	var ex Explanation
	var needsJSON string
	if err := db.QueryRowContext(ctx,
		`SELECT id, status, needs FROM work_items WHERE id=?`, itemID).
		Scan(&ex.ItemID, &ex.Status, &needsJSON); err != nil {
		// sql.ErrNoRows stays wrapped (not swallowed into a new error): the HTTP
		// layer tells "this item does not exist" from "this item cannot be read"
		// by errors.Is, and the two answers are 404 and 500.
		return ex, fmt.Errorf("item %s: %w", itemID, err)
	}
	needs, err := decodeList(needsJSON)
	if err != nil {
		// Name the column. A corrupt row must not read as "not found": the item
		// is right there, and the operator needs to know which column to look at.
		return ex, fmt.Errorf("work_items.needs for item %s: %w", itemID, err)
	}
	ex.Needs = needs

	// LoadAllCandidates, not LoadCandidates: the narrow loader prefilters
	// offline nodes, unhealthy endpoints and disabled models, and a candidate
	// that is never loaded can never appear in Rejects. The explanation has to
	// be able to say "your only gpu node is offline".
	cands, err := LoadAllCandidates(ctx, db)
	if err != nil {
		return ex, err
	}
	d := Place(WorkItem{ID: ex.ItemID, Needs: needs, Status: ex.Status}, cands)
	ex.Placed = d.OK
	ex.Reason = d.Reason
	ex.Rejects = d.Rejects
	if !d.OK {
		ex.BlockedReason = d.Reason
	}

	// The pause is the reason a queued item is not running, whether or not it
	// is placeable: without this, /why answers "placed: true" for an item the
	// board will never issue and the operator goes looking for a placement bug
	// that does not exist.
	paused, pauseWhy, _, err := pauseState(ctx, db, clock)
	if err != nil {
		return ex, err
	}
	ex.Paused, ex.PauseReason = paused, pauseWhy
	if paused && (ex.Status == "queued" || ex.Status == "needs_approval") {
		// Compose, do not override: an item can be unplaceable AND on a paused
		// board, and replacing the placement failure with the pause would make
		// the second cause appear only after the operator raises the ceiling —
		// the answer changing under the person asking is what a read-only
		// explanation must not do.
		const pauseText = "the board is not issuing work: "
		if d.OK {
			ex.BlockedReason = pauseText + pauseWhy
		} else {
			ex.BlockedReason = d.Reason + "; " + pauseText + pauseWhy
		}
	}
	return ex, nil
}
