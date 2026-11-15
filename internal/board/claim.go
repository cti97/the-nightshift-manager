// internal/board/claim.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// ClaimRequest is what a node posts to /claim. The node id is never in the
// body: the bearer token is the only authority for who is asking.
type ClaimRequest struct {
	FreeSlots int `json:"free_slots"`
}

type ClaimedItem struct {
	Item            WorkItem `json:"item"`
	RunID           string   `json:"run_id"`
	PlacementReason string   `json:"placement_reason"`
}

// LoadCandidates reads every online node, every endpoint that is healthy, and
// their enabled models: the input to placement. Callers narrow it to the node
// that is asking, because capability eligibility is per node (spec §9).
func LoadCandidates(ctx context.Context, db *sql.DB) ([]Candidate, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT n.id, n.name, n.role, n.provides_live, n.reach_classes, n.concurrency,
		       n.status, n.agent_version,
		       e.id, e.name, e.provider, e.reach, e.health,
		       m.id, m.model_id, m.provides, m.features, m.context_window, m.cost_in_per_mtok
		  FROM nodes n
		  JOIN endpoints e ON 1=1
		  JOIN models m ON m.endpoint_id = e.id
		 WHERE n.status = 'online' AND m.enabled = 1 AND e.health = 'ok'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var c Candidate
		var live, reach, provides, features string
		if err := rows.Scan(
			&c.Node.ID, &c.Node.Name, &c.Node.Role, &live, &reach, &c.Node.Concurrency,
			&c.Node.Status, &c.Node.AgentVersion,
			&c.Endpoint.ID, &c.Endpoint.Name, &c.Endpoint.Provider, &c.Endpoint.Reach, &c.Endpoint.Health,
			&c.Model.ID, &c.Model.ModelID, &provides, &features, &c.Model.ContextWindow, &c.Model.CostInPerMtok,
		); err != nil {
			return nil, err
		}
		var err error
		if c.Node.ProvidesLive, err = decodeList(live); err != nil {
			return nil, err
		}
		if c.Node.ReachClasses, err = decodeList(reach); err != nil {
			return nil, err
		}
		if c.Model.Provides, err = decodeList(provides); err != nil {
			return nil, err
		}
		if c.Model.Features, err = decodeList(features); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func decodeList(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decode capability list %q: %w", raw, err)
	}
	return out, nil
}

// Claim takes up to `slots` eligible items for this node, atomically.
//
// BEGIN IMMEDIATE on a dedicated connection plus the claimed_by IS NULL guard
// plus a rowcount check is the whole concurrency story (spec §8). Do not
// replace it with optimistic retries; the single writer is the design.
func Claim(ctx context.Context, db *sql.DB, clock Clock, nodeID string, slots int) ([]ClaimedItem, error) {
	if slots < 1 {
		slots = 1
	}

	// Order matters. Candidates are loaded BEFORE the connection is checked
	// out, because the pool is capped at one connection: holding `conn` while
	// querying through `db` blocks forever waiting for a free connection.
	// Slightly stale capabilities are an acceptable price; a deadlock is not.
	cands, err := LoadCandidates(ctx, db)
	if err != nil {
		return nil, err
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	// Eligible candidates for this node only.
	var mine []Candidate
	for _, c := range cands {
		if c.Node.ID == nodeID {
			mine = append(mine, c)
		}
	}
	if len(mine) == 0 {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, err
		}
		committed = true
		return nil, nil
	}

	rows, err := conn.QueryContext(ctx,
		`SELECT id,title,needs,status,attempt FROM work_items
		  WHERE status='queued' ORDER BY priority DESC, created_at ASC LIMIT 50`)
	if err != nil {
		return nil, err
	}

	type row struct {
		id, title, needs, status string
		attempt                  int
	}
	var queued []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.title, &r.needs, &r.status, &r.attempt); err != nil {
			rows.Close()
			return nil, err
		}
		queued = append(queued, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var claimed []ClaimedItem
	for _, r := range queued {
		if len(claimed) >= slots {
			break
		}
		needs, err := decodeList(r.needs)
		if err != nil {
			return nil, err
		}
		item := WorkItem{ID: r.id, Title: r.title, Needs: needs, Status: r.status, Attempt: r.attempt}

		d := Place(item, mine)
		if !d.OK {
			continue
		}
		runID, err := NewToken()
		if err != nil {
			return nil, err
		}
		lease := clock.Now() + leaseSeconds

		res, err := conn.ExecContext(ctx,
			`UPDATE work_items SET status='claimed', claimed_by=?, lease_expires_at=?, attempt=attempt+1, updated_at=?
			  WHERE id=? AND claimed_by IS NULL AND status='queued'`,
			nodeID, lease, clock.Now(), r.id)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n != 1 {
			continue // lost a race; the guard did its job
		}

		if _, err := conn.ExecContext(ctx,
			`INSERT INTO runs (id,work_item_id,attempt,node_id,endpoint_id,model_id,started_at,status,placement_reason)
			 VALUES (?,?,?,?,?,?,?,'claimed',?)`,
			runID, r.id, r.attempt+1, nodeID, d.Chosen.Endpoint.ID, d.Chosen.Model.ID,
			clock.Now(), d.Reason); err != nil {
			return nil, err
		}
		if err := AppendEvent(ctx, conn, clock, "system", "work_item", r.id, "claimed",
			map[string]any{"node": nodeID, "run": runID, "reason": d.Reason}); err != nil {
			return nil, err
		}

		item.Status, item.Attempt = "claimed", r.attempt+1
		claimed = append(claimed, ClaimedItem{Item: item, RunID: runID, PlacementReason: d.Reason})
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return claimed, nil
}
