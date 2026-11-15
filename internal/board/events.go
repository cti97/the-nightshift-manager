package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Execer is satisfied by both *sql.DB and *sql.Tx, so an event can be written
// inside the same transaction as the change it describes.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type Event struct {
	ID       int64  `json:"id"`
	At       int64  `json:"at"`
	Actor    string `json:"actor"`
	Entity   string `json:"entity"`
	EntityID string `json:"entity_id"`
	Type     string `json:"type"`
	Payload  string `json:"payload,omitempty"`
}

// AppendEvent writes one row to the append-only audit spine. The dashboard
// projects state from these rows (spec invariant 2).
func AppendEvent(ctx context.Context, q Execer, clock Clock, actor, entity, entityID, typ string, payload any) error {
	var raw sql.NullString
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal event payload: %w", err)
		}
		raw = sql.NullString{String: string(b), Valid: true}
	}
	_, err := q.ExecContext(ctx,
		`INSERT INTO events (at, actor, entity, entity_id, type, payload) VALUES (?,?,?,?,?,?)`,
		clock.Now(), actor, entity, entityID, typ, raw)
	return err
}

// ListEvents returns events for one entity in append order.
func ListEvents(ctx context.Context, db *sql.DB, entity, entityID string, limit int) ([]Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, at, actor, entity, entity_id, type, COALESCE(payload,'')
		   FROM events WHERE entity = ? AND entity_id = ? ORDER BY id LIMIT ?`,
		entity, entityID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Entity, &e.EntityID, &e.Type, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
