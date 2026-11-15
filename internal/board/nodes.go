package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cti97/the-nightshift-manager/internal/caps"
)

const leaseSeconds = 300

// nodeStaleAfter is how long a node may go without a heartbeat before the
// reaper takes it offline: two leases, so one missed beat is not enough. A
// node that has not been heard from inside this window cannot be handed work —
// a laptop that closed its lid would otherwise stay 'online' forever and /why
// would answer "placed: true" for work nothing will ever claim.
const nodeStaleAfter = 2 * leaseSeconds

// ProtocolMin/Max are what this binary can speak. A node outside the range is
// marked incompatible rather than left crash-looping (spec §20).
const (
	ProtocolMin = 1
	ProtocolMax = 1
)

// emptyJSONList is how an empty list is stored. json.Marshal(nil) would write
// "null", and these columns are compared as raw strings, so every writer uses
// this one spelling instead.
const emptyJSONList = "[]"

// marshalList encodes a list, keeping the empty case as [] rather than null so
// the stored bytes do not depend on whether the caller passed nil or an empty
// slice.
func marshalList(v []string) string {
	if len(v) == 0 {
		return emptyJSONList
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// reachClassVocabulary is the closed set of reach classes a node may declare.
// Placement matches these by exact string equality, so a node that stores
// anything else — "Host", "wifi", "" — is eligible for nothing: it heartbeats
// happily and claims nothing forever. Rejecting at write time is the same rule
// the capability vocabulary follows (caps.Parse), and it makes the typo surface
// in the call that caused it.
var reachClassVocabulary = map[string]struct{}{
	"host": {}, "lan": {}, "wan": {},
}

// parseReachClasses validates and canonicalises a declared reach set: trimmed,
// de-duplicated and sorted so the stored bytes depend only on the set. An empty
// list is legitimate (a node that can reach nothing), but an entry outside the
// vocabulary is an error naming the offending value.
func parseReachClasses(raw []string) ([]string, error) {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, c := range raw {
		c = strings.TrimSpace(c)
		if _, ok := reachClassVocabulary[c]; !ok {
			return nil, fmt.Errorf("unknown reach class %q (want host, lan or wan)", c)
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

// nodeWriteFound checks that a heartbeat UPDATE matched a node. database/sql
// reports zero rows as success, so without this an unknown node id would look
// like a healthy heartbeat.
func nodeWriteFound(res sql.Result, nodeID string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("heartbeat: unknown node %q", nodeID)
	}
	return nil
}

type RegisterRequest struct {
	Name             string   `json:"name"`
	Role             string   `json:"role"`
	ProvidesDeclared []string `json:"provides_declared"`
	ReachClasses     []string `json:"reach_classes"`
	Concurrency      int      `json:"concurrency"`
}

type RegisterResponse struct {
	NodeID string `json:"node_id"`
	Token  string `json:"token"`
}

type HeartbeatRequest struct {
	ProvidesLive []string `json:"provides_live"`
	ReachClasses []string `json:"reach_classes"`
	FreeSlots    int      `json:"free_slots"`
	AgentVersion string   `json:"agent_version"`
	Protocol     int      `json:"protocol_version"`
}

type HeartbeatResponse struct {
	Status     string `json:"status"`
	LeaseUntil int64  `json:"lease_until,omitempty"`
	Queued     int    `json:"queued"`
	Approvals  int    `json:"pending_approvals"`
}

func RegisterNode(ctx context.Context, db *sql.DB, clock Clock, req RegisterRequest) (string, string, error) {
	switch req.Role {
	case "executor", "steward":
	case "manager":
		return "", "", errors.New("a manager does not register as a worker")
	default:
		return "", "", fmt.Errorf("unknown role %q", req.Role)
	}
	declared, err := caps.Parse(req.ProvidesDeclared)
	if err != nil {
		return "", "", err
	}
	reach, err := parseReachClasses(req.ReachClasses)
	if err != nil {
		return "", "", err
	}
	token, err := NewToken()
	if err != nil {
		return "", "", err
	}
	id, err := NewToken() // 32 random bytes as an id: no ordering, no guessing
	if err != nil {
		return "", "", err
	}

	declaredJSON := marshalList(declared)
	reachJSON := marshalList(reach)
	conc := req.Concurrency
	if conc < 1 {
		conc = 1
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO nodes (id,name,token_hash,role,provides_declared,provides_live,reach_classes,concurrency,created_at,status)
		 VALUES (?,?,?,?,?,?,?,?,?,'offline')`,
		id, req.Name, HashToken(token), req.Role, declaredJSON, emptyJSONList,
		reachJSON, conc, clock.Now()); err != nil {
		return "", "", fmt.Errorf("register %q: %w", req.Name, err)
	}
	if err := AppendEvent(ctx, tx, clock, "node", "node", id, "registered",
		map[string]string{"name": req.Name, "role": req.Role}); err != nil {
		return "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	return id, token, nil
}

func Heartbeat(ctx context.Context, db *sql.DB, clock Clock, nodeID string, req HeartbeatRequest) (HeartbeatResponse, error) {
	if req.Protocol < ProtocolMin || req.Protocol > ProtocolMax {
		res, err := db.ExecContext(ctx,
			`UPDATE nodes SET status = CASE WHEN status='paused' THEN 'paused' ELSE 'incompatible' END,
			 provides_live=?, last_seen_at=? WHERE id=?`,
			emptyJSONList, clock.Now(), nodeID)
		if err != nil {
			return HeartbeatResponse{}, err
		}
		if err := nodeWriteFound(res, nodeID); err != nil {
			return HeartbeatResponse{}, err
		}
		return HeartbeatResponse{Status: "incompatible"}, nil
	}

	live, err := caps.Parse(req.ProvidesLive)
	if err != nil {
		return HeartbeatResponse{}, err
	}
	// Validated before any write, so a heartbeat carrying a typo changes
	// nothing: the node keeps its last good live set and status.
	reach, err := parseReachClasses(req.ReachClasses)
	if err != nil {
		return HeartbeatResponse{}, err
	}
	liveJSON := marshalList(live)
	reachJSON := marshalList(reach)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return HeartbeatResponse{}, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE nodes SET provides_live=?, reach_classes=?, last_seen_at=?,
		 status = CASE WHEN status='paused' THEN 'paused' ELSE 'online' END,
		 agent_version=?, protocol_version=? WHERE id=?`,
		liveJSON, reachJSON, clock.Now(), req.AgentVersion, req.Protocol, nodeID)
	if err != nil {
		return HeartbeatResponse{}, err
	}
	if err := nodeWriteFound(res, nodeID); err != nil {
		return HeartbeatResponse{}, err
	}

	lease := clock.Now() + leaseSeconds
	if _, err := tx.ExecContext(ctx,
		`UPDATE work_items SET lease_expires_at=?
		  WHERE claimed_by=? AND status IN ('claimed','running')`,
		lease, nodeID); err != nil {
		return HeartbeatResponse{}, err
	}

	var queued, approvals int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_items WHERE status='queued'`).Scan(&queued); err != nil {
		return HeartbeatResponse{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_items WHERE status='needs_approval'`).Scan(&approvals); err != nil {
		return HeartbeatResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return HeartbeatResponse{}, err
	}

	return HeartbeatResponse{Status: "online", LeaseUntil: lease, Queued: queued, Approvals: approvals}, nil
}
