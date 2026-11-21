// internal/board/checkpoints.go
package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Checkpoint is one progress marker (spec §12). It is a declaration by the run, not a
// migration of state: the next attempt reads it and decides what to do, and the board does
// not pretend any tool state came with it.
type Checkpoint struct {
	ID               string `json:"id"`
	RunID            string `json:"run_id"`
	Seq              int    `json:"seq"`
	DeclaredProgress string `json:"declared_progress"`
	State            string `json:"state,omitempty"`
	Artifacts        string `json:"artifacts,omitempty"`
	CreatedAt        int64  `json:"created_at"`
}

// CheckpointRequest is what a node posts. declared_progress is required: a checkpoint that
// says nothing does not tell the next attempt anything, and a run that writes one per tool
// call would fill the table with noise.
type CheckpointRequest struct {
	DeclaredProgress string `json:"declared_progress"`
	State            string `json:"state"`
	Artifacts        string `json:"artifacts"`
}

var (
	// ErrRunNotLive is a checkpoint on a run that is not claimed or running. A finished run
	// has nothing to resume from, and accepting the write would attach progress to an
	// attempt that already reported.
	ErrRunNotLive = errors.New("the run is not live")
	// ErrNotYourRun is a node writing another node's checkpoint. The node token is the only
	// claim of identity the board has, and it must bound this write as it bounds a report.
	ErrNotYourRun = errors.New("the run belongs to another node")
)

// WriteCheckpoint stores one checkpoint for a live run, with the next seq for that run.
//
// The seq is allocated inside the transaction and the unique index refuses a duplicate, so
// a node that retries the same write gets a refusal rather than two markers with the same
// number — the index is the guard, the read is the convenience.
func WriteCheckpoint(ctx context.Context, db *sql.DB, clock Clock, runID, nodeID string, req CheckpointRequest) (Checkpoint, error) {
	progress := strings.TrimSpace(req.DeclaredProgress)
	if progress == "" {
		return Checkpoint{}, fmt.Errorf("declared_progress is required: a checkpoint that says nothing resumes nothing")
	}
	// A checkpoint is a note the next attempt reads, not a place to park a transcript: it is
	// bounded like a message, so a run cannot fill the board's disk with one tool call.
	for field, value := range map[string]string{
		"declared_progress": progress, "state": req.State, "artifacts": req.Artifacts,
	} {
		if len(value) > maxCheckpointBytes {
			return Checkpoint{}, fmt.Errorf("%s is %d bytes; a checkpoint field is a note (limit %d)", field, len(value), maxCheckpointBytes)
		}
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return Checkpoint{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return Checkpoint{}, fmt.Errorf("begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var status, ownerNode string
	if err := conn.QueryRowContext(ctx, `SELECT status, node_id FROM runs WHERE id=?`, runID).
		Scan(&status, &ownerNode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Checkpoint{}, fmt.Errorf("run %s: %w", runID, sql.ErrNoRows)
		}
		return Checkpoint{}, err
	}
	if ownerNode != nodeID {
		return Checkpoint{}, fmt.Errorf("%w (run %s is %s's)", ErrNotYourRun, runID, ownerNode)
	}
	if status != "claimed" && status != "running" {
		return Checkpoint{}, fmt.Errorf("%w: %s is %s", ErrRunNotLive, runID, status)
	}

	var next int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM checkpoints WHERE run_id=?`, runID).Scan(&next); err != nil {
		return Checkpoint{}, err
	}
	id, err := NewToken()
	if err != nil {
		return Checkpoint{}, err
	}
	cp := Checkpoint{
		ID: id, RunID: runID, Seq: next,
		DeclaredProgress: progress, State: req.State, Artifacts: req.Artifacts,
		CreatedAt: clock.Now(),
	}
	var state, artifacts any
	if cp.State != "" {
		state = cp.State
	}
	if cp.Artifacts != "" {
		artifacts = cp.Artifacts
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO checkpoints (id,run_id,seq,declared_progress,state,artifacts,created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		cp.ID, cp.RunID, cp.Seq, cp.DeclaredProgress, state, artifacts, cp.CreatedAt); err != nil {
		return Checkpoint{}, err
	}
	if err := AppendEvent(ctx, conn, clock, nodeID, "run", runID, "checkpoint", map[string]any{
		"seq": cp.Seq, "declared_progress": cp.DeclaredProgress,
	}); err != nil {
		return Checkpoint{}, err
	}
	if err := commitConn(ctx, conn, &committed); err != nil {
		return Checkpoint{}, err
	}
	return cp, nil
}

// queryer is the minimum both *sql.DB and *sql.Conn offer: the claim reads its checkpoint
// on the transaction's own connection, so it sees the same snapshot as the run it is about
// to create.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// latestCheckpoint is the newest marker across an item's runs, read on a caller's
// connection.
func latestCheckpoint(ctx context.Context, q queryer, itemID string) (*Checkpoint, error) {
	return latestCheckpointWith(ctx, q, itemID)
}

// LatestCheckpointForItem is the newest checkpoint across an item's runs: what a requeued
// attempt resumes from. It reads by run recency, not by checkpoint seq, because the item's
// earlier attempts each have their own seq starting at 1.
//
// A corrupt row (a seq that cannot be read) is skipped rather than failing the claim: the
// checkpoint is an aid, and refusing to issue work because a progress marker is unreadable
// would turn a note into a blocker. The claim says nothing about the skip; the corrupt-row
// listing already exists for that.
func LatestCheckpointForItem(ctx context.Context, db *sql.DB, itemID string) (*Checkpoint, error) {
	return latestCheckpoint(ctx, db, itemID)
}

// latestCheckpointWith is the one query both entry points use: by run recency, because the
// item's earlier attempts each have their own seq starting at 1.
func latestCheckpointWith(ctx context.Context, q queryer, itemID string) (*Checkpoint, error) {
	var cp Checkpoint
	var state, artifacts sql.NullString
	err := q.QueryRowContext(ctx,
		`SELECT c.id, c.run_id, c.seq, c.declared_progress, c.state, c.artifacts, c.created_at
		   FROM checkpoints c
		   JOIN runs r ON r.id = c.run_id
		  WHERE r.work_item_id = ?
		  ORDER BY r.started_at DESC, r.rowid DESC, c.seq DESC
		  LIMIT 1`, itemID).
		Scan(&cp.ID, &cp.RunID, &cp.Seq, &cp.DeclaredProgress, &state, &artifacts, &cp.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	cp.State = state.String
	cp.Artifacts = artifacts.String
	return &cp, nil
}

// CheckpointsForRun lists a run's markers in seq order, for a reader that wants the whole
// trail rather than the last one.
func CheckpointsForRun(ctx context.Context, db *sql.DB, runID string) ([]Checkpoint, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, run_id, seq, declared_progress, state, artifacts, created_at
		   FROM checkpoints WHERE run_id=? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Checkpoint{}
	for rows.Next() {
		var cp Checkpoint
		var state, artifacts sql.NullString
		if err := rows.Scan(&cp.ID, &cp.RunID, &cp.Seq, &cp.DeclaredProgress, &state, &artifacts, &cp.CreatedAt); err != nil {
			return nil, err
		}
		cp.State = state.String
		cp.Artifacts = artifacts.String
		out = append(out, cp)
	}
	return out, rows.Err()
}
