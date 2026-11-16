// internal/board/why.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Explanation is the answer to "why is this item not running" (spec §14).
//
// Reason summarises the decision; Rejects carries the per-candidate rule text.
// Both are returned because the summary names only the unsatisfied needs, which
// is not an explanation: the human needs to see which rule rejected which node.
type Explanation struct {
	ItemID string   `json:"item_id"`
	Status string   `json:"status"`
	Needs  []string `json:"needs"`
	// Placed reports the placement verdict: whether a candidate would take the
	// item now. It is false for a terminal item even when one would, because a
	// done/failed/cancelled item cannot be claimed (C13); that item's placement
	// verdict is in Reason.
	Placed        bool     `json:"placed"`
	Reason        string   `json:"reason"`
	BlockedReason string   `json:"blocked_reason"`
	Rejects       []Reject `json:"rejects,omitempty"`

	// Paused and PauseReason are the second half of the answer, and without
	// them /why lies: placement can say "placed: true, this node would take it"
	// while the board is refusing to issue anything because the day's spend
	// ceiling was reached. The item is explainable and unexplained at the same
	// time, which is the one outcome this endpoint exists to prevent.
	//
	// PausedBy is the third piece, and it is a separate field rather than a
	// sentence inside PauseReason because ownership is now load-bearing: "who
	// stopped this" decides which action resumes the board. It is "ceiling",
	// "operator", or "ceiling+operator" when the operator's key and the
	// ceiling's own evaluation both stop the board; an operator who clears
	// their hold while the ceiling also reaches will see the board stay
	// stopped, and this field is how the explanation says so in advance. Empty
	// when nothing is paused.
	Paused      bool   `json:"paused"`
	PausedBy    string `json:"paused_by,omitempty"`
	PauseReason string `json:"pause_reason,omitempty"`

	// Warnings are candidate rows that could not be decoded and were removed
	// from the placement input (A4). They are part of the answer, not a log
	// detail: removing a candidate can change the reason the item does not
	// place, and an explanation that quietly answered from the readable subset
	// would be explaining a board that does not exist. The same shape is
	// returned by /admin/corrupt-rows, so an operator can go from one item's
	// warning to the full list.
	Warnings []CorruptRow `json:"warnings,omitempty"`
}

// corruptCandidatesClause is the blocked_reason half of an A4 warning. It names
// the corruption inline so an operator reading only blocked_reason is not told
// "no eligible candidate" while a candidate was actually skipped; Warnings
// carries every row in full. When an OPEN item still places, blocked_reason
// stays empty: the answer is not degraded in its verdict, only in the set of
// options behind it, and the warning list already says so. A terminal item
// always carries a blocked_reason (it is finished), so the clause is appended
// there too, even when placement would have succeeded: a finished status does
// not make a skipped candidate less broken.
func corruptCandidatesClause(rows []CorruptRow) string {
	switch len(rows) {
	case 0:
		return ""
	case 1:
		return "1 corrupt candidate row was skipped: " + rows[0].message() + " (see warnings)"
	default:
		return fmt.Sprintf("%d corrupt candidate rows were skipped, including %s (see warnings)",
			len(rows), rows[0].message())
	}
}

// terminalStatus reports whether status names a finished item. done, failed and
// cancelled are the terminal members of the board's status vocabulary (the set
// http.go's filter lists); an item in one of them is never claimable, so /why
// must not report it as placed (C13). A status outside the vocabulary is not
// treated as terminal: there is no defined finished state to report, so the
// placement verdict is left to speak for it.
func terminalStatus(status string) bool {
	switch status {
	case "done", "failed", "cancelled":
		return true
	}
	return false
}

// terminalReason is the answer for an already-finished item. It states the fact
// a caller must act on — the item is finished and cannot be claimed — and keeps
// the placement explanation, because an operator may still want to know whether
// it would have placed. The two facts are separated by punctuation, not merged:
// "already done" must not be readable as "done, and also placeable". The
// placement shown is computed against the board as it is now (Explain's own
// premise), not as it was when the item ran, and the wording says "today's
// board" so the tense cannot imply otherwise.
func terminalReason(itemID, status string, d Decision) string {
	reason := fmt.Sprintf("item %s is already %s, so it is finished and cannot be claimed", itemID, status)
	if d.OK {
		return reason + "; placement against today's board would give it: " + d.Reason
	}
	return reason + "; even against today's board it could not be placed: " + d.Reason
}

// parkedReason is the answer for a parked item. It names the action and how long
// the item has waited, because those are the two facts that decide whether the
// operator approves now, denies, or goes looking for the run that asked. A row
// whose request cannot be read says so and points at the only way out (deny):
// silence here would read as "nothing is waiting", which is the opposite of true.
func parkedReason(itemID string, raw sql.NullString, now int64) string {
	if !raw.Valid || raw.String == "" {
		return fmt.Sprintf("item %s is waiting on a human, but no approval request was recorded on it (it was parked before the column existed, or hand-edited); deny it from the approvals page to clear it", itemID)
	}
	var req ApprovalRequest
	if err := json.Unmarshal([]byte(raw.String), &req); err != nil {
		return fmt.Sprintf("item %s is waiting on a human, but its approval request cannot be read (%v); deny it from the approvals page to clear it", itemID, err)
	}
	asked := "at an unknown time"
	if req.RequestedAt > 0 {
		asked = fmt.Sprintf("%s ago", waitedFor(now-req.RequestedAt))
	}
	return fmt.Sprintf("item %s is waiting on a human: the action %q has to be approved or denied (asked %s by %s)",
		itemID, req.Action, asked, req.RequestedBy)
}

// waitedFor renders a wait in the unit an operator thinks in: seconds under a
// minute, minutes under an hour, then hours and days. It clamps a negative wait
// (a clock that moved backwards) to zero rather than printing "-3m", which would
// read as a bug in the board instead of a clock skew.
func waitedFor(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh%dm", seconds/3600, (seconds%3600)/60)
	default:
		return fmt.Sprintf("%dd%dh", seconds/86400, (seconds%86400)/3600)
	}
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

	// The unfiltered loader, not LoadCandidates: the narrow loader prefilters
	// offline nodes, unhealthy endpoints and disabled models, and a candidate
	// that is never loaded can never appear in Rejects. The explanation has to
	// be able to say "your only gpu node is offline".
	//
	// The degraded loader, not the strict one (A4): one corrupt capability cell
	// must not turn every item's explanation into a 500. The skipped rows come
	// back as warnings and stay part of the answer.
	cands, warnings, err := loadAllCandidatesDegraded(ctx, db)
	if err != nil {
		return ex, err
	}
	ex.Warnings = warnings
	d := Place(WorkItem{ID: ex.ItemID, Needs: needs, Status: ex.Status}, cands)
	ex.Rejects = d.Rejects
	switch {
	case ex.Status == "needs_approval":
		// A parked item is not waiting for a placement; it is waiting for a
		// person. Reporting the placement verdict as "placed: true" would say the
		// board could claim it, and it cannot: Claim only takes 'queued' rows, so
		// the board will not issue this item until a human decides (M3 D2, D6).
		// The parked branch therefore comes first, and the verdict is never
		// printed as placed.
		var rawRequest sql.NullString
		if err := db.QueryRowContext(ctx,
			`SELECT approval_request FROM work_items WHERE id=?`, itemID).Scan(&rawRequest); err != nil {
			return ex, fmt.Errorf("item %s: %w", itemID, err)
		}
		ex.Placed = false
		ex.Reason = parkedReason(ex.ItemID, rawRequest, clock.Now())
		ex.BlockedReason = ex.Reason
	case terminalStatus(ex.Status):
		// A finished item cannot be claimed, whatever placement would say about
		// it. Reporting placed:true here read as "this could be claimed" and
		// sent an operator looking for a run that will never happen (C13). The
		// placement verdict is still part of the answer — it is the second
		// question /why answers — so it moves into the reason instead of
		// vanishing.
		ex.Placed = false
		ex.Reason = terminalReason(ex.ItemID, ex.Status, d)
		ex.BlockedReason = ex.Reason
	default:
		ex.Placed = d.OK
		ex.Reason = d.Reason
		if !d.OK {
			ex.BlockedReason = d.Reason
		}
	}
	// Compose, do not replace: a placement failure (or a finished status) and
	// the skipped candidates are two facts. Saying only "no eligible candidate"
	// would send the operator looking for a capability hole when the row that
	// could have served the item is sitting there corrupt. This runs after BOTH
	// branches: A4's note belongs in a terminal item's blocked_reason too, and
	// dropping it there was C13's regression.
	if ex.BlockedReason != "" {
		if clause := corruptCandidatesClause(warnings); clause != "" {
			ex.BlockedReason += "; " + clause
		}
	}

	// The pause is the reason a queued item is not running, whether or not it
	// is placeable: without this, /why answers "placed: true" for an item the
	// board will never issue and the operator goes looking for a placement bug
	// that does not exist.
	paused, pauseWhy, pausedBy, err := pauseState(ctx, db, clock)
	if err != nil {
		return ex, err
	}
	ex.Paused, ex.PauseReason, ex.PausedBy = paused, pauseWhy, pausedBy
	if paused && (ex.Status == "queued" || ex.Status == "needs_approval") {
		// Compose, do not override: an item can be unplaceable AND on a paused
		// board, and replacing the placement failure with the pause would make
		// the second cause appear only after the operator raises the ceiling —
		// the answer changing under the person asking is what a read-only
		// explanation must not do.
		const pauseText = "the board is not issuing work: "
		if ex.BlockedReason == "" {
			ex.BlockedReason = pauseText + pauseWhy
		} else {
			ex.BlockedReason += "; " + pauseText + pauseWhy
		}
	}
	return ex, nil
}
