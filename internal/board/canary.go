// internal/board/canary.go
package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CanaryTarget is one endpoint as an always-on node sees it: enough to probe it, and what the board
// currently believes. It is deliberately not the admin EndpointView: a canary needs no auth_ref (it
// probes reachability, not credentials) and no model list.
type CanaryTarget struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Provider     string `json:"provider"`
	BaseURL      string `json:"base_url"`
	Reach        string `json:"reach"`
	Health       string `json:"health"`
	HealthReason string `json:"health_reason,omitempty"`
	LastOKAt     *int64 `json:"last_ok_at,omitempty"`
	LatencyMS    *int64 `json:"latency_ms,omitempty"`
}

// CanaryReport is what the steward posts after one probe.
//
// `reachable` is the whole signal, and a canary may only DEMOTE: when the endpoint does not answer
// from the steward's network path the board marks it down, because a dead model that placement
// keeps choosing is the failure this exists to prevent. A success does not promote an endpoint to
// ok — that would let an unauthenticated probe overrule the manager's authenticated one, and a 401
// from a reachable service is not health. Promotion stays with the manager's own probe (spec §7.3).
type CanaryReport struct {
	Reachable bool   `json:"reachable"`
	LatencyMS int    `json:"latency_ms"`
	Reason    string `json:"reason"`
}

// ListCanaryTargets is GET /canary: the endpoints a steward may watch. All of them, because which
// of them a given box can reach is a fact only that box knows — it probes and discovers.
func ListCanaryTargets(ctx context.Context, db *sql.DB) ([]CanaryTarget, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, name, provider, base_url, reach, health, COALESCE(health_reason,''), last_ok_at, latency_ms
		   FROM endpoints ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CanaryTarget{}
	for rows.Next() {
		var t CanaryTarget
		var lastOK, latency sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Name, &t.Provider, &t.BaseURL, &t.Reach, &t.Health,
			&t.HealthReason, &lastOK, &latency); err != nil {
			return nil, err
		}
		if lastOK.Valid {
			t.LastOKAt = &lastOK.Int64
		}
		if latency.Valid {
			t.LatencyMS = &latency.Int64
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CanaryResult says what the report changed, because "nothing" is the common and correct answer.
type CanaryResult struct {
	EndpointID string `json:"endpoint_id"`
	Health     string `json:"health"`
	Changed    bool   `json:"changed"`
}

var ErrNoEndpoint = errors.New("no such endpoint")

// ReportCanary records one probe. It writes exactly one event when the endpoint's health changes,
// and none when it does not: an always-on box probing every minute would otherwise fill the audit
// spine with 1,440 entries a day per endpoint saying "still down".
func ReportCanary(ctx context.Context, db *sql.DB, clock Clock, endpointID, nodeID string, rep CanaryReport) (CanaryResult, error) {
	if rep.Reachable {
		// A success is recorded as liveness, never as a promotion: see CanaryReport's comment.
		res, err := db.ExecContext(ctx,
			`UPDATE endpoints SET latency_ms=?, last_ok_at=? WHERE id=?`,
			rep.LatencyMS, clock.Now(), endpointID)
		if err != nil {
			return CanaryResult{}, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return CanaryResult{}, err
		} else if n == 0 {
			return CanaryResult{}, fmt.Errorf("endpoint %s: %w", endpointID, ErrNoEndpoint)
		}
		var health string
		if err := db.QueryRowContext(ctx, `SELECT health FROM endpoints WHERE id=?`, endpointID).Scan(&health); err != nil {
			return CanaryResult{}, err
		}
		return CanaryResult{EndpointID: endpointID, Health: health, Changed: false}, nil
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return CanaryResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return CanaryResult{}, fmt.Errorf("begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var old string
	if err := conn.QueryRowContext(ctx, `SELECT health FROM endpoints WHERE id=?`, endpointID).Scan(&old); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CanaryResult{}, fmt.Errorf("endpoint %s: %w", endpointID, ErrNoEndpoint)
		}
		return CanaryResult{}, err
	}
	reason := rep.Reason
	if reason == "" {
		reason = fmt.Sprintf("the canary at %s could not reach it", nodeID)
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE endpoints SET health='down', health_reason=? WHERE id=?`, reason, endpointID); err != nil {
		return CanaryResult{}, err
	}
	out := CanaryResult{EndpointID: endpointID, Health: "down", Changed: old != "down"}
	if out.Changed {
		if err := AppendEvent(ctx, conn, clock, nodeID, "endpoint", endpointID, "down", map[string]any{
			"previous_health": old, "reason": reason, "latency_ms": rep.LatencyMS,
		}); err != nil {
			return CanaryResult{}, err
		}
	}
	if err := commitConn(ctx, conn, &committed); err != nil {
		return CanaryResult{}, err
	}
	return out, nil
}
