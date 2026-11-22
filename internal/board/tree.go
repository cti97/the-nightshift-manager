package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// ItemNode is one item in a tree, with the children an agent filed under it (M7 T4).
//
// The tree exists because agent-filed work is otherwise indistinguishable from work that appeared
// overnight: five rows with a parent_id nobody renders. Depth is capped at maxFilingDepth, so the
// recursion here is bounded by construction — no cycle check is needed for a field the writer
// enforces, and a hand-edited cycle would still terminate at the depth cap.
type ItemNode struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	Needs     []string `json:"needs"`
	Depth     int      `json:"depth"`
	CreatedBy string   `json:"created_by"`
	FiledBy   string   `json:"filed_by_run,omitempty"`
	CreatedAt int64    `json:"created_at"`
	// Parked carries the question when a filing (or the work itself) is waiting for a human.
	Parked *ApprovalRequest `json:"approval_request,omitempty"`
	// Children are the items this one filed, oldest first. Empty is [], never null: a caller must be
	// able to tell "no children" from "the field was missing".
	Children []ItemNode `json:"children"`
}

// ItemTree reads one item and everything filed under it.
func ItemTree(ctx context.Context, db *sql.DB, id string) (ItemNode, error) {
	return itemNode(ctx, db, id, 0)
}

func itemNode(ctx context.Context, db *sql.DB, id string, depth int) (ItemNode, error) {
	// The depth argument is the recursion guard, not the item's own depth: a cycle written by hand
	// into the database would otherwise recurse forever, and the writer's cap is a promise the
	// reader is not obliged to trust.
	if depth > maxFilingDepth+1 {
		return ItemNode{}, errors.New("work items are nested deeper than the filing limit; the tree stops here")
	}
	var n ItemNode
	var needs string
	var parked sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT id,title,status,needs,depth,created_by,COALESCE(filed_by_run,''),created_at,approval_request
		   FROM work_items WHERE id=?`, id).
		Scan(&n.ID, &n.Title, &n.Status, &needs, &n.Depth, &n.CreatedBy, &n.FiledBy, &n.CreatedAt, &parked)
	if err != nil {
		return ItemNode{}, err
	}
	_ = json.Unmarshal([]byte(needs), &n.Needs)
	if n.Needs == nil {
		n.Needs = []string{}
	}
	if parked.Valid && parked.String != "" {
		var req ApprovalRequest
		// A malformed request is omitted rather than failing the tree: the operator asked what is
		// under this item, and one unreadable question must not hide five children.
		if json.Unmarshal([]byte(parked.String), &req) == nil {
			n.Parked = &req
		}
	}

	// created_at has second resolution, so siblings filed in the same second tie; rowid is the
	// insertion order, which is the order the operator watched them appear in.
	rows, err := db.QueryContext(ctx,
		`SELECT id FROM work_items WHERE parent_id=? ORDER BY created_at, rowid`, id)
	if err != nil {
		return ItemNode{}, err
	}
	var kids []string
	for rows.Next() {
		var kid string
		if err := rows.Scan(&kid); err != nil {
			rows.Close()
			return ItemNode{}, err
		}
		kids = append(kids, kid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ItemNode{}, err
	}
	n.Children = make([]ItemNode, 0, len(kids))
	for _, kid := range kids {
		child, err := itemNode(ctx, db, kid, depth+1)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// A dangling parent_id is a corrupt row, not a reason to fail the whole tree.
				continue
			}
			return ItemNode{}, err
		}
		n.Children = append(n.Children, child)
	}
	return n, nil
}

// CountDescendants is what the cap and the page both want: how much work exists under one item.
func CountDescendants(n ItemNode) int {
	total := 0
	for _, c := range n.Children {
		total += 1 + CountDescendants(c)
	}
	return total
}
