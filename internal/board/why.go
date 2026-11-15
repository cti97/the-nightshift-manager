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
		return ex, fmt.Errorf("item %s: %w", itemID, err)
	}
	needs, err := decodeList(needsJSON)
	if err != nil {
		return ex, err
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
	return ex, nil
}
