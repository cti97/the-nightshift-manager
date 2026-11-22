package board

import (
	"context"
	"database/sql"
	"fmt"
)

// BoardItem is one row on the operator's board page: enough to recognise the work and click into it.
type BoardItem struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	CreatedBy string `json:"created_by"`
	ClaimedBy string `json:"claimed_by,omitempty"`
	CreatedAt int64  `json:"created_at"`
	Depth     int    `json:"depth"`
}

// BoardView is the board as a person reads it: the items, the counts, and today's ledger. The JSON
// route /admin/board answers the counts alone, because it predates items being worth listing; this
// is the shape the page needs.
type BoardView struct {
	Items []BoardItem `json:"items"`
	// Counts is every status the schema knows, including the zeroes: a status that vanished from the
	// map would read as "this board cannot have them" rather than "there are none today".
	Counts map[string]int `json:"counts"`
	Spend  SpendStatus    `json:"spend"`
	// Truncated says the list hit its limit, so the page can say so instead of quietly showing less.
	Truncated bool `json:"truncated"`
}

// boardPageLimit bounds the page. A board with more than this wants the approvals queue and /why, not
// one enormous page; the limit exists so the page stays fast on a phone.
const boardPageLimit = 200

// ReadBoard loads the page's data in one place, so the page and the JSON answer cannot disagree.
func ReadBoard(ctx context.Context, db *sql.DB, clock Clock, limit int) (BoardView, error) {
	if limit <= 0 || limit > boardPageLimit {
		limit = boardPageLimit
	}
	view := BoardView{Counts: map[string]int{}, Items: []BoardItem{}}

	// Parked first, then work in flight, then the rest: the same order the page renders in, so the
	// operator's eye lands on the things that need them.
	rows, err := db.QueryContext(ctx, `
		SELECT id, title, status, created_by, COALESCE(claimed_by,''), created_at, depth
		  FROM work_items
		 ORDER BY CASE status
		            WHEN 'needs_approval' THEN 0
		            WHEN 'running' THEN 1
		            WHEN 'claimed' THEN 2
		            WHEN 'queued' THEN 3
		            WHEN 'failed' THEN 4
		            WHEN 'done' THEN 5
		            ELSE 6 END,
		          priority DESC, created_at DESC
		 LIMIT ?`, limit+1)
	if err != nil {
		return BoardView{}, err
	}
	// Closed here rather than deferred: the pool is one connection (db.SetMaxOpenConns(1)), and the
	// counts query below would wait forever on this result set while holding nothing.
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var it BoardItem
		if err := rows.Scan(&it.ID, &it.Title, &it.Status, &it.CreatedBy, &it.ClaimedBy, &it.CreatedAt, &it.Depth); err != nil {
			return BoardView{}, err
		}
		if len(view.Items) == limit {
			// One row past the limit is enough to know there are more; the row itself is not shown.
			view.Truncated = true
			break
		}
		view.Items = append(view.Items, it)
	}
	if err := rows.Err(); err != nil {
		return BoardView{}, err
	}
	if err := rows.Close(); err != nil {
		return BoardView{}, err
	}

	countRows, err := db.QueryContext(ctx, `SELECT status, COUNT(*) FROM work_items GROUP BY status`)
	if err != nil {
		return BoardView{}, err
	}
	defer countRows.Close()
	for countRows.Next() {
		var status string
		var n int
		if err := countRows.Scan(&status, &n); err != nil {
			return BoardView{}, err
		}
		view.Counts[status] = n
	}
	if err := countRows.Err(); err != nil {
		return BoardView{}, err
	}
	for _, status := range workItemStatuses {
		if _, ok := view.Counts[status]; !ok {
			view.Counts[status] = 0
		}
	}

	if view.Spend, err = TodaySpend(ctx, db, clock); err != nil {
		return BoardView{}, fmt.Errorf("spend: %w", err)
	}
	return view, nil
}
