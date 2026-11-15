// internal/board/items.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/cti97/the-nightshift-manager/internal/caps"
)

type CreateItemRequest struct {
	Title       string   `json:"title"`
	Brief       string   `json:"brief"`
	Needs       []string `json:"needs"`
	Priority    int      `json:"priority"`
	MaxAttempts int      `json:"max_attempts"`
	ParentID    string   `json:"parent_id"`
}

func CreateItem(ctx context.Context, db *sql.DB, clock Clock, req CreateItemRequest) (string, error) {
	if req.Title == "" || req.Brief == "" {
		return "", fmt.Errorf("title and brief are required")
	}
	needs, err := caps.Parse(req.Needs)
	if err != nil {
		return "", err
	}
	maxAttempts := req.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 3
	}
	id, err := NewToken()
	if err != nil {
		return "", err
	}
	needsJSON, _ := json.Marshal(needs)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO work_items (id,parent_id,title,brief,needs,status,priority,max_attempts,created_by,created_at,updated_at)
		 VALUES (?,?,?,?,?,'queued',?,?,'admin',?,?)`,
		id, nullable(req.ParentID), req.Title, req.Brief, string(needsJSON),
		req.Priority, maxAttempts, clock.Now(), clock.Now()); err != nil {
		return "", err
	}
	if err := AppendEvent(ctx, tx, clock, "admin", "work_item", id, "created",
		map[string]any{"title": req.Title, "needs": needs}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func nullable(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
