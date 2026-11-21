// internal/board/claim.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ClaimRequest is what a node posts to /claim. The node id is never in the
// body: the bearer token is the only authority for who is asking.
type ClaimRequest struct {
	FreeSlots int `json:"free_slots"`
}

// ClaimedItem is the claim payload. Brief lives here, not on WorkItem:
// placement must never read prose, so the type it ranks stays prose-free,
// while the worker still gets the text it is being asked to execute.
type ClaimedItem struct {
	Item            WorkItem `json:"item"`
	Brief           string   `json:"brief"`
	RunID           string   `json:"run_id"`
	PlacementReason string   `json:"placement_reason"`

	// Endpoint and Model are the run's chosen execution target. The claim
	// transaction already writes their ids into the run row; they are repeated
	// here because the node that claimed the item has no other route to them
	// (the claim payload is the node's only read of the board's endpoint
	// tables). Placement chooses them once, and the node executes exactly that
	// choice rather than re-deriving it.
	Endpoint ClaimEndpoint `json:"endpoint"`
	Model    ClaimModel    `json:"model"`

	// PriorCheckpoint is the newest progress marker across the item's earlier runs (M5
	// D7): what the last attempt declared before it stopped. It is nil on a first attempt,
	// and the board makes no promise that resuming from it restores tool state — it is a
	// note from the previous attempt, which is exactly what spec §12 says it is.
	PriorCheckpoint *Checkpoint `json:"prior_checkpoint,omitempty"`
}

// ClaimEndpoint is the construction config for the endpoint that placement
// chose: the family, the URL, the reach the adapter must declare, and the name
// of the provider key. AuthRef is a name, never a value — the value is resolved
// by the node from its own environment under provider.AuthRefPrefix, so a
// credential never travels over the board's wire.
type ClaimEndpoint struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	Reach    string `json:"reach"`
	AuthRef  string `json:"auth_ref,omitempty"`
}

// ClaimModel names the model to send: ID is the board row (what runs.model_id
// records) and ModelID is the wire name the provider expects. They are
// different values and the node needs the second.
type ClaimModel struct {
	ID      string `json:"id"`
	ModelID string `json:"model_id"`
}

// LoadCandidates reads every online node, every endpoint that is healthy, and
// their enabled models: the input to claiming. Callers narrow it to the node
// that is asking, because capability eligibility is per node (spec §9).
//
// A candidate row whose capability cells cannot be decoded is SKIPPED and
// reported (see reportCorruptRow), not returned as an error. This loader feeds
// the claim path, and one malformed provides_live/features cell on one row must
// not 500 every node's claim. Skipping a candidate is safe in one direction
// only, and that is the direction that matters: it can remove a placement
// option, never add one, so the worst case is a node that claims less.
//
// These filters repeat rules Eligible enforces. That is intentional: here they
// are a prefilter so Claim does not scan rows it would discard, while the rule
// itself lives in one place. /why uses the unfiltered loader below for the rows
// this loader removes.
func LoadCandidates(ctx context.Context, db *sql.DB) ([]Candidate, error) {
	cands, _, err := loadCandidates(ctx, db, true, corruptSkipAndLog)
	return cands, err
}

// LoadAllCandidates is LoadCandidates without the availability prefilter: every
// node, every endpoint and every model, including offline nodes, unhealthy
// endpoints and disabled models. /why needs the rows the narrow loader drops,
// because a candidate that is never loaded is a candidate the explanation
// cannot name — "your only gpu node is offline" is exactly the answer an
// operator asks /why for.
//
// It is also STRICT about decode failures, unlike LoadCandidates: this loader
// is the exact set with no explanation attached, so a corrupt cell is returned
// as an error naming the row and column. Explain does NOT use it — A4 made the
// explainer degrade instead of 500, through loadAllCandidatesDegraded below.
func LoadAllCandidates(ctx context.Context, db *sql.DB) ([]Candidate, error) {
	cands, _, err := loadCandidates(ctx, db, false, corruptFatal)
	return cands, err
}

// loadAllCandidatesDegraded is /why's loader (A4): the same unfiltered set as
// LoadAllCandidates, but a corrupt candidate row is removed from the candidate
// slice and returned as a CorruptRow instead of aborting the call. One bad
// provides_live cell must not make the explainer useless for every item on the
// board; the caller answers for the requested item and surfaces the skipped
// rows as warnings.
//
// Skipping here is safe in the same one-way sense as Claim: it can remove a
// placement option, never add one. The warnings are what keep the removal from
// being silent.
func loadAllCandidatesDegraded(ctx context.Context, db *sql.DB) ([]Candidate, []CorruptRow, error) {
	return loadCandidates(ctx, db, false, corruptCollect)
}

// corruptPolicy says what a loader does with a candidate row it cannot decode.
// It is an enum rather than a bool because "skip and log" and "skip and return
// to the caller" are different answers to the same event, and a bool named
// skipCorrupt would hide the third (fatal) case at the call site.
type corruptPolicy int

const (
	corruptFatal      corruptPolicy = iota // return an error naming the row and column
	corruptSkipAndLog                      // the claim path: report once, keep loading
	corruptCollect                         // /why: return the rows to the caller as warnings
)

func loadCandidates(ctx context.Context, db *sql.DB, placeableOnly bool, policy corruptPolicy) ([]Candidate, []CorruptRow, error) {
	query := `
		SELECT n.id, n.name, n.role, n.provides_live, n.reach_classes, n.concurrency,
		       n.status, n.agent_version,
		       e.id, e.name, e.provider, e.reach, e.health, COALESCE(e.health_reason,''),
		       e.base_url, COALESCE(e.auth_ref,''),
		       m.id, m.model_id, m.provides, m.features, m.context_window, m.cost_in_per_mtok, m.enabled
		  FROM nodes n
		  JOIN endpoints e ON 1=1
		  JOIN models m ON m.endpoint_id = e.id`
	if placeableOnly {
		query += ` WHERE n.status = 'online' AND m.enabled = 1 AND e.health = 'ok'`
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var out []Candidate
	var corrupt []CorruptRow
	seenCorrupt := map[CorruptRow]struct{}{}
	for rows.Next() {
		var c Candidate
		var live, reach, provides, features string
		// A Scan failure is deliberately fatal, unlike the JSON decode below.
		// SQLite is dynamically typed, so a value the driver cannot convert to
		// the destination type (an INTEGER in a TEXT cell, say) fails the scan
		// for exactly one row while every other row scans fine — the same
		// one-bad-row shape as a corrupt JSON cell. It is
		// still not skipped, because a value the write path cannot produce is a
		// schema or write-path breach, not a cell a human can repair like
		// malformed JSON, and swallowing it would turn a broken board into a
		// board that silently claims nothing. Unreachable today: every writer
		// marshals these cells and the columns are NOT NULL. If this SELECT ever
		// gains a nullable or unconstrained column, revisit this with the
		// one-row-must-not-deny-everyone rule that decodeLists applies.
		if err := rows.Scan(
			&c.Node.ID, &c.Node.Name, &c.Node.Role, &live, &reach, &c.Node.Concurrency,
			&c.Node.Status, &c.Node.AgentVersion,
			&c.Endpoint.ID, &c.Endpoint.Name, &c.Endpoint.Provider, &c.Endpoint.Reach, &c.Endpoint.Health,
			&c.Endpoint.HealthReason,
			&c.Connection.BaseURL, &c.Connection.AuthRef,
			&c.Model.ID, &c.Model.ModelID, &provides, &features, &c.Model.ContextWindow,
			&c.Model.CostInPerMtok, &c.Model.Enabled,
		); err != nil {
			return nil, nil, err
		}
		if cr := c.decodeLists(live, reach, provides, features); cr != nil {
			switch policy {
			case corruptFatal:
				// The strict loader keeps its old error text: "nodes/<id>
				// column provides_live: decode ...". Callers distinguish this
				// from sql.ErrNoRows by errors.Is; nothing wraps ErrNoRows here.
				return nil, nil, errors.New(cr.message())
			case corruptSkipAndLog:
				reportCorruptRow(*cr)
			case corruptCollect:
				// The cross join repeats the same node/model row once per
				// endpoint × model, so the same corrupt cell would otherwise
				// appear several times. A warning list that repeats itself makes
				// an operator count corruptions that are one cell.
				if _, ok := seenCorrupt[*cr]; !ok {
					seenCorrupt[*cr] = struct{}{}
					corrupt = append(corrupt, *cr)
				}
			}
			continue
		}
		out = append(out, c)
	}
	return out, corrupt, rows.Err()
}

// decodeLists decodes the four capability cells on one candidate row. A failure
// is returned as the CorruptRow the log report, /why's warnings and
// /admin/corrupt-rows all speak, so the row and column cannot be described
// three different ways.
func (c *Candidate) decodeLists(live, reach, provides, features string) *CorruptRow {
	var err error
	if c.Node.ProvidesLive, err = decodeList(live); err != nil {
		return &CorruptRow{Table: "nodes", RowID: c.Node.ID, Column: "provides_live", Reason: err.Error()}
	}
	if c.Node.ReachClasses, err = decodeList(reach); err != nil {
		return &CorruptRow{Table: "nodes", RowID: c.Node.ID, Column: "reach_classes", Reason: err.Error()}
	}
	if c.Model.Provides, err = decodeList(provides); err != nil {
		return &CorruptRow{Table: "models", RowID: c.Model.ID, Column: "provides", Reason: err.Error()}
	}
	if c.Model.Features, err = decodeList(features); err != nil {
		return &CorruptRow{Table: "models", RowID: c.Model.ID, Column: "features", Reason: err.Error()}
	}
	return nil
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
//
// Two counters move here, and they mean different things. `work_items.attempt`
// counts attempts that actually started: it is incremented at claim and
// refunded by the reaper when a claim is abandoned without starting. A run's
// `attempt` is its per-item claim ordinal, which must never be reused because
// runs is append-only with UNIQUE(work_item_id, attempt) — deriving it from the
// refundable counter would collide on the claim after an abandonment.
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

	// The spend ceiling is evaluated HERE, inside the claim transaction and
	// before any row is written, and its verdict is final for this call. Two
	// properties follow, and they are the reason it is not evaluated in the
	// handler:
	//
	//   - The ledger read and the claim's writes cannot be interleaved by
	//     another writer. BEGIN IMMEDIATE already serialises writers, and the
	//     read going through `conn` (not `db`, which would deadlock on the
	//     one-connection pool) keeps it inside that transaction.
	//   - The pause it records is written in the same transaction as whatever
	//     this claim does, so the event log and the config row agree.
	//
	// A ceiling the board cannot read is an error, not unlimited: a typo must
	// not silently disable the safety mechanism. This is the fail-CLOSED half of
	// the claim path's failure policy, and it is deliberately not the same
	// policy as the corrupt-row skip below: a row that cannot be decoded is
	// skipped (a lost placement option), while a safety value that cannot be
	// read refuses the claim (a silently unlimited budget would be a lost
	// guard). Do not route this read through the skip-and-log helper.
	paused, _, err := reconcileCeiling(ctx, conn, clock)
	if err != nil {
		return nil, err
	}
	if paused {
		// A paused board still answers the claim — with nothing. An error would
		// tell the node the board is broken; this tells it, truthfully, that
		// there is no work for it. The reason travels on /admin/spend/today and
		// in the event log.
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, err
		}
		committed = true
		return nil, nil
	}

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
		`SELECT id,title,brief,needs,status,attempt,approval_granted FROM work_items
		  WHERE status='queued' ORDER BY priority DESC, created_at ASC LIMIT 50`)
	if err != nil {
		return nil, err
	}

	type row struct {
		id, title, brief, needs, status string
		attempt                         int
		grant                           sql.NullString
	}
	var queued []row
	for rows.Next() {
		var r row
		// Fatal for the same reason as the scan in loadCandidates: a wrong
		// storage class in a NOT NULL column is a write-path breach, and
		// skipping the row would hide it. The repairable cell-level case is the
		// JSON decode below, which is skipped and reported.
		if err := rows.Scan(&r.id, &r.title, &r.brief, &r.needs, &r.status, &r.attempt, &r.grant); err != nil {
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
			// Skip and report, do not abort: one hand-edited `needs` cell must
			// not 500 every node's claim (M1's final review). The item is left
			// queued, not failed or cancelled — the board has no authority to
			// destroy work whose contents it could not read, and a manual SQL
			// typo is repairable. It is therefore skipped forever until the
			// operator fixes it; the report names the row and column, /why
			// keeps answering 500 naming the column too, and GET
			// /admin/corrupt-rows lists it without depending on log retention.
			reportCorruptRow(CorruptRow{Table: "work_items", RowID: r.id, Column: "needs", Reason: err.Error()})
			continue
		}
		item := WorkItem{ID: r.id, Title: r.title, Needs: needs, Status: r.status, Attempt: r.attempt}

		// A corrupt grant is reported and left nil, not fatal and not guessed.
		// The run then escalates again rather than acting on a permission
		// nobody can read: same one-way rule as the needs cell above, except
		// that dropping the grant can only make the board ask a human, never
		// authorise something.
		if r.grant.Valid && r.grant.String != "" {
			var g ApprovalGrant
			if err := json.Unmarshal([]byte(r.grant.String), &g); err != nil {
				reportCorruptRow(CorruptRow{Table: "work_items", RowID: r.id, Column: "approval_granted", Reason: err.Error()})
			} else {
				item.ApprovalGranted = &g
			}
		}

		d := Place(item, mine)
		if !d.OK {
			continue
		}
		runID, err := NewToken()
		if err != nil {
			return nil, err
		}
		lease := clock.Now() + leaseSeconds

		// The grant is consumed by this claim (M3 D3): it authorised one run, and
		// that run is starting now. Clearing it here, rather than at report time,
		// is what makes a third run ask again instead of inheriting a permission
		// an earlier run may already have acted on — the safe direction when the
		// run is interrupted before the board hears from it.
		res, err := conn.ExecContext(ctx,
			`UPDATE work_items SET status='claimed', claimed_by=?, lease_expires_at=?, attempt=attempt+1,
			        approval_granted=NULL, updated_at=?
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
			 VALUES (?,?,(SELECT COALESCE(MAX(attempt),0)+1 FROM runs WHERE work_item_id=?),?,?,?,?,'claimed',?)`,
			runID, r.id, r.id, nodeID, d.Chosen.Endpoint.ID, d.Chosen.Model.ID,
			clock.Now(), d.Reason); err != nil {
			return nil, err
		}
		if err := AppendEvent(ctx, conn, clock, "system", "work_item", r.id, "claimed",
			map[string]any{"node": nodeID, "run": runID, "reason": d.Reason}); err != nil {
			return nil, err
		}

		item.Status, item.Attempt = "claimed", r.attempt+1
		prior, err := latestCheckpoint(ctx, conn, item.ID)
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, ClaimedItem{
			PriorCheckpoint: prior,
			Item:            item, Brief: r.brief, RunID: runID, PlacementReason: d.Reason,
			Endpoint: ClaimEndpoint{
				ID:       d.Chosen.Endpoint.ID,
				Name:     d.Chosen.Endpoint.Name,
				Provider: d.Chosen.Endpoint.Provider,
				BaseURL:  d.Chosen.Connection.BaseURL,
				Reach:    d.Chosen.Endpoint.Reach,
				AuthRef:  d.Chosen.Connection.AuthRef,
			},
			Model: ClaimModel{ID: d.Chosen.Model.ID, ModelID: d.Chosen.Model.ModelID},
		})
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return claimed, nil
}
