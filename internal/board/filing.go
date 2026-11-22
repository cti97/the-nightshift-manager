package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cti97/the-nightshift-manager/internal/caps"
)

// Filing is an agent asking the board for more work: a child item, created by the run that is
// currently executing (M7). The board — not the agent's own loop — decides whether that is allowed,
// because the whole point is that the extra work becomes a row with a lease, a price and an audit.
const (
	// maxFilingDepth is how far below the operator's item agent-filed work may go. A child of a
	// child is useful (a task that decomposes once); a grandchild of a grandchild is a chain nobody
	// asked for and nobody priced.
	maxFilingDepth = 2
	// maxFilingsPerRun bounds fan-out. Five is a decomposition, not a work queue: an agent that
	// wants more is describing a project, and a human should see it.
	maxFilingsPerRun = 5
	// filingApprovalAction is the grant that authorises ONE filing above either limit. It is a
	// named action like any other (D3), so an approval says exactly what it permitted.
	filingApprovalAction = "file_item"

	// maxTitleBytes and maxBriefBytes bound an agent's input the way the thread limits bound a
	// poster's: the item table is read on every claim, and an agent with a 10 MB brief is a node
	// that can make the board slow for everyone else.
	maxTitleBytes = 512
	maxBriefBytes = 64 << 10
	// maxClientRefBytes and maxNeedsPerFiling bound the rest of an agent's filing. The ref becomes an
	// index key, and the needs list is parsed and stored — an agent that sends either without a bound
	// is an agent that can make every later filing slower.
	maxClientRefBytes = 128
	maxNeedsPerFiling = 16
)

// FileItemRequest is what a run asks for.
type FileItemRequest struct {
	Title     string   `json:"title"`
	Brief     string   `json:"brief"`
	Needs     []string `json:"needs"`
	Priority  int      `json:"priority"`
	ClientRef string   `json:"client_ref"`
}

// FileItemResult tells the caller what happened, because "nothing" is a real answer: either the item
// already existed (a retry), or the run has been parked awaiting a human, or it was created.
type FileItemResult struct {
	ItemID  string `json:"id,omitempty"`
	Created bool   `json:"created"`
	Parked  bool   `json:"parked,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

var (
	// ErrRunNotFiling means the run is not in a state that may file: it is not the caller's, or it
	// is over. Both are refusals, not retries.
	ErrRunNotFiling = errors.New("run is not filing")
	// ErrClientRefTaken means another run already used this idempotency key. Reusing a key across
	// runs would let one run's retry silently return another run's item.
	ErrClientRefTaken = errors.New("client_ref already used by another run")
)

// FileItem records a child item filed by a live run (M7 T2).
//
// The rules, in the order they are checked — the order is the contract, because the first refusal
// is the one the operator reads:
//
//  1. the run exists, belongs to this node, and is live (claimed or running);
//  2. the request is valid, and its needs are a subset of what THIS node can actually do;
//  3. the idempotency key, if given, has not been used;
//  4. depth and fan-out, which are the two limits, and the grant that lifts them by one;
//  5. insert and event, in the same transaction.
func FileItem(ctx context.Context, db *sql.DB, clock Clock, runID, nodeID string, req FileItemRequest) (FileItemResult, error) {
	title := strings.TrimSpace(req.Title)
	brief := strings.TrimSpace(req.Brief)
	if title == "" || brief == "" {
		return FileItemResult{}, fmt.Errorf("title and brief are required")
	}
	if len(title) > maxTitleBytes {
		return FileItemResult{}, fmt.Errorf("title is longer than %d bytes", maxTitleBytes)
	}
	if len(brief) > maxBriefBytes {
		return FileItemResult{}, fmt.Errorf("brief is longer than %d bytes", maxBriefBytes)
	}
	if len(req.ClientRef) > maxClientRefBytes {
		return FileItemResult{}, fmt.Errorf("client_ref is %d bytes and the limit is %d", len(req.ClientRef), maxClientRefBytes)
	}
	if len(req.Needs) > maxNeedsPerFiling {
		return FileItemResult{}, fmt.Errorf("%d capabilities and the limit is %d", len(req.Needs), maxNeedsPerFiling)
	}
	needs, err := caps.Parse(req.Needs)
	if err != nil {
		return FileItemResult{}, err
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return FileItemResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return FileItemResult{}, fmt.Errorf("begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var parentID, runNode, runStatus string
	if err := conn.QueryRowContext(ctx,
		`SELECT work_item_id, node_id, status FROM runs WHERE id=?`, runID).
		Scan(&parentID, &runNode, &runStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FileItemResult{}, fmt.Errorf("run %s: %w", runID, ErrRunNotFiling)
		}
		return FileItemResult{}, err
	}
	if runNode != nodeID {
		return FileItemResult{}, fmt.Errorf("run %s belongs to another node: %w", runID, ErrRunNotFiling)
	}
	if runStatus != "claimed" && runStatus != "running" {
		return FileItemResult{}, fmt.Errorf("run %s is %s: %w", runID, runStatus, ErrRunNotFiling)
	}

	var parentDepth int
	var parentGrant sql.NullString
	if err := conn.QueryRowContext(ctx,
		`SELECT depth, approval_granted FROM work_items WHERE id=?`, parentID).
		Scan(&parentDepth, &parentGrant); err != nil {
		return FileItemResult{}, err
	}

	// A child may want only what its node can do. Without this rule an agent with a shell mints work
	// for a more privileged box — and the board would place it, because the item's needs are just a
	// list. The node's LIVE set is the bound, not the parent item's needs: the parent was placed for
	// what it required, but its machine can often do more, and that is what it may delegate.
	var providesLive string
	if err := conn.QueryRowContext(ctx, `SELECT provides_live FROM nodes WHERE id=?`, nodeID).
		Scan(&providesLive); err != nil {
		return FileItemResult{}, err
	}
	var live []string
	_ = json.Unmarshal([]byte(providesLive), &live)
	have := make(map[string]struct{}, len(live))
	for _, c := range live {
		have[c] = struct{}{}
	}
	for _, need := range needs {
		if _, ok := have[need]; !ok {
			return FileItemResult{}, fmt.Errorf("this node cannot provide %q, so it cannot ask for it", need)
		}
	}

	// Idempotency before the limits: a retry of an ALLOWED filing must return the first result, not
	// park the run because the first one already counted.
	if req.ClientRef != "" {
		var existingID, existingRun string
		err := conn.QueryRowContext(ctx,
			`SELECT id, COALESCE(filed_by_run,'') FROM work_items WHERE client_ref=?`, req.ClientRef).
			Scan(&existingID, &existingRun)
		if err == nil {
			if existingRun != runID {
				return FileItemResult{}, fmt.Errorf("%s: %w", req.ClientRef, ErrClientRefTaken)
			}
			if err := commitConn(ctx, conn, &committed); err != nil {
				return FileItemResult{}, err
			}
			return FileItemResult{ItemID: existingID, Created: false}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return FileItemResult{}, err
		}
	}

	depth := parentDepth + 1
	// Fan-out is counted over the parent's CHILDREN, not over one run's filings. A re-claim after an
	// escalation, a denial or a crash creates a new run id, so counting per run would reset the cap
	// on every retry: five children per attempt is fifteen children for one item. The cap belongs to
	// the item being decomposed, which is the thing the operator can see.
	var filed int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_items WHERE parent_id=?`, parentID).Scan(&filed); err != nil {
		return FileItemResult{}, err
	}

	granted := grantAction(parentGrant) == filingApprovalAction
	reason := ""
	switch {
	case depth > maxFilingDepth:
		reason = fmt.Sprintf("this item is already %d deep, and agent-filed work stops at %d", parentDepth, maxFilingDepth)
	case filed >= maxFilingsPerRun && !granted:
		reason = fmt.Sprintf("this item has already filed %d children, which is the limit", filed)
	}
	if reason != "" {
		// The limit is where the human is asked, not where the agent is silenced. The run parks with
		// an approval request naming the filing, and the board's own machinery does the rest.
		if err := escalateForFiling(ctx, conn, clock, runID, nodeID, parentID, reason); err != nil {
			return FileItemResult{}, err
		}
		if err := commitConn(ctx, conn, &committed); err != nil {
			return FileItemResult{}, err
		}
		return FileItemResult{Parked: true, Reason: reason}, nil
	}
	if granted {
		// Consumed here, not at claim time: a filing grant is permission to file, not an action the
		// run performs, so it must survive the claim that delivers it.
		if _, err := conn.ExecContext(ctx,
			`UPDATE work_items SET approval_granted=NULL, updated_at=? WHERE id=?`, clock.Now(), parentID); err != nil {
			return FileItemResult{}, err
		}
	}

	id, err := NewToken()
	if err != nil {
		return FileItemResult{}, err
	}
	needsJSON, err := json.Marshal(needs)
	if err != nil {
		return FileItemResult{}, err
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO work_items (id,parent_id,title,brief,needs,status,priority,max_attempts,
		                        created_by,depth,filed_by_run,client_ref,created_at,updated_at)
		 VALUES (?,?,?,?,?,'queued',?,3,?,?,?,?,?,?)`,
		id, parentID, title, brief, string(needsJSON), req.Priority,
		"agent:"+nodeID, depth, runID, nullableStr(req.ClientRef), clock.Now(), clock.Now()); err != nil {
		return FileItemResult{}, err
	}
	actor := "agent:" + nodeID
	if err := AppendEvent(ctx, conn, clock, actor, "work_item", id, "created", map[string]any{
		"title": title, "needs": needs, "parent": parentID, "run": runID, "depth": depth,
	}); err != nil {
		return FileItemResult{}, err
	}
	if err := commitConn(ctx, conn, &committed); err != nil {
		return FileItemResult{}, err
	}
	return FileItemResult{ItemID: id, Created: true}, nil
}

// grantAction reads the action out of the stored grant. A malformed grant is treated as no grant:
// the safe direction is asking again, never acting on a permission nobody can read.
func grantAction(raw sql.NullString) string {
	if !raw.Valid || raw.String == "" {
		return ""
	}
	var g ApprovalGrant
	if err := json.Unmarshal([]byte(raw.String), &g); err != nil {
		return ""
	}
	return g.Action
}

// escalateForFiling parks the parent item with a request naming the filing. It is a small wrapper
// because the approval machinery is on the *item*, while the caller holds a run id.
func escalateForFiling(ctx context.Context, conn *sql.Conn, clock Clock, runID, nodeID, itemID, reason string) error {
	req := ApprovalRequest{
		Action:  filingApprovalAction,
		Reason:  reason,
		Options: []string{"file one more item", "decline and carry on"},
	}
	return escalateInTx(ctx, conn, clock, runID, nodeID, req, RunTokens{})
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
