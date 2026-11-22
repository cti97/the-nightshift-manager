package board

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Retention bounds how long the audit spine grows.
//
// The events table is append-only and nothing used to delete from it, so a board that has been doing
// its job for a year carries a year of rows in a file the snapshot and replica paths copy whole. That
// is not a bug anyone sees on day one; it is one they see on day four hundred.
//
// What is pruned and what is not:
//
//   - events, oldest first, in batches. This is the growth.
//   - runs, never. They are the ledger's only source (spend/today sums them), and work_items.filed_by_run
//     references them; deleting a run would either break the reference or require rewriting history.
//   - work_items, never. An item is the record of the work.
//
// The floor is deliberate: no retention setting may delete anything younger than retentionFloor (24h),
// because a typo in a unit file ("--retention 1m") must not be able to wipe today's spine.
const (
	defaultEventRetention = 90 * 24 * time.Hour
	retentionFloor        = 24 * time.Hour
	retentionBatch        = 2000
)

// DefaultEventRetention is what the daemon keeps when the operator says nothing: ninety days of audit
// history. Pruning by default is a judgement about a file that otherwise grows forever, and ninety
// days is long enough to answer "what happened to this item" for any item anyone is still asking about.
func DefaultEventRetention() time.Duration { return defaultEventRetention }

// RetentionRequest is one pruning pass.
type RetentionRequest struct {
	// Keep is how much history to keep. Zero means "keep everything" and prunes nothing; a value below
	// the floor is raised to it rather than refused, because a too-aggressive setting is a
	// configuration mistake and the safe answer is to keep more, not to fail the daemon's start.
	Keep time.Duration
	// Batch bounds one pass so a first run against a large table does not hold the write lock for a
	// minute. The next pass continues from where this one stopped.
	Batch int
}

// RetentionResult says what happened, because "nothing" is the normal answer and a first run against a
// year-old board must be distinguishable from a run that is broken.
type RetentionResult struct {
	// Deleted is how many events this pass removed.
	Deleted int
	// More is true when the batch filled, meaning there is more to prune on the next pass.
	More bool
	// Cutoff is the timestamp below which events were removed; zero when nothing was pruned.
	Cutoff int64
}

// PruneEvents deletes a batch of events older than the retention window.
//
// It runs its own BEGIN IMMEDIATE on a dedicated connection: the delete is a write, and the board's
// rule is that a read-then-write takes the write lock up front rather than upgrading into
// `database is locked (517)`.
func PruneEvents(ctx context.Context, db *sql.DB, clock Clock, req RetentionRequest) (RetentionResult, error) {
	if req.Keep <= 0 {
		return RetentionResult{}, nil
	}
	keep := req.Keep
	if keep < retentionFloor {
		keep = retentionFloor
	}
	batch := req.Batch
	if batch <= 0 || batch > retentionBatch {
		batch = retentionBatch
	}
	cutoff := clock.Now() - int64(keep/time.Second)
	if cutoff <= 0 {
		// A clock set before the retention window began (a fresh database with a fake clock) has no
		// history to prune, and a negative cutoff would match every row.
		return RetentionResult{}, nil
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return RetentionResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return RetentionResult{}, fmt.Errorf("begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	// id < (SELECT ...) rather than a plain at < ?: the index is (entity, entity_id, id), so deleting
	// by id range lets SQLite walk the primary key backwards, and a batch is bounded by rows rather
	// than by time. The subquery picks the highest id inside the window, so the first batch to prune
	// is the oldest.
	res, err := conn.ExecContext(ctx, `
		DELETE FROM events
		 WHERE id IN (SELECT id FROM events WHERE at < ? ORDER BY id LIMIT ?)`,
		cutoff, batch)
	if err != nil {
		return RetentionResult{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return RetentionResult{}, err
	}
	if err := commitConn(ctx, conn, &committed); err != nil {
		return RetentionResult{}, err
	}
	return RetentionResult{Deleted: int(n), More: int(n) == batch, Cutoff: cutoff}, nil
}

// RunRetention prunes on the reaper's clock. It is a method on Server so the daemon can start it
// beside the reaper and the watch scheduler, and so a test can call one pass directly.
func (s *Server) RunRetention(ctx context.Context, every time.Duration, keep time.Duration) {
	if keep <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// One tick may need several batches; More says so, and looping here keeps the pass
			// bounded without waiting a whole interval for the next chunk.
			for {
				res, err := PruneEvents(ctx, s.db, s.clock, RetentionRequest{Keep: keep})
				if err != nil {
					if ctx.Err() == nil {
						s.logf("retention: %v", err)
					}
					break
				}
				if !res.More {
					if res.Deleted > 0 {
						s.logf("retention: pruned %d events older than %s", res.Deleted, keep)
					}
					break
				}
			}
		}
	}
}
