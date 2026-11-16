// internal/board/corrupt.go
package board

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"log"
	"sync"
)

// A corrupt row on the claim path is skipped, not fatal: one hand-edited
// capability cell or `needs` value must not 500 every node's claim (M1's final
// review). This file is the one place that records the skip, so the two
// different policies in the claim path stay visibly different:
//
//   - A ROW the board cannot decode is skipped. The row is left exactly as it
//     is: the board has no authority to fail, cancel or rewrite work it could
//     not read, and a skipped candidate can only remove a placement option,
//     never add one. The skip is reported here.
//   - A CONFIGURATION value that gates safety (the daily spend ceiling) still
//     fails the claim closed in spend.go. Skipping that one would silently mean
//     "unlimited spend", which is the opposite of safe. Nothing in this file is
//     called from the ceiling path, and readCeiling's error is untouched.
//
// "Logged" means a line on the process log (log.Printf, the board's only
// logging channel: s.logf delegates to it). It is deliberately NOT an event:
// the events table is the audit spine of STATE CHANGES, written inside the
// transaction that makes the change, while a decode skip is an observation that
// repeats on every claim scan and would be written by a read-only loader before
// the claim transaction even starts. An event per skip would either be one row
// per claim attempt (drowning a busy board, which the brief forbids) or need a
// second write path in the loader.
//
// What makes the log line discoverable without drowning the operator is the
// dedupe below: the FULL message — row id, column and the offending value's
// error — is the key, so each distinct corruption is reported once per process
// and a repair that corrupts the cell again reports again. The row itself is
// never mutated.
//
// A log line is still not a LIST: after rotation a long-lived manager cannot
// answer "what is broken" from it, and /why answers one item at a time. That is
// what ListCorruptRows is for — a read-only enumeration of the same cells the
// claim path decodes, for GET /admin/corrupt-rows.
type CorruptRow struct {
	// Table, RowID and Column locate the cell an operator has to repair.
	Table  string `json:"table"`
	RowID  string `json:"row_id"`
	Column string `json:"column"`
	// Reason is the claim path's own decode error for the cell (the text
	// decodeList produces, which names the offending raw value). The row and
	// column are separate fields rather than repeated in this sentence; the
	// log line is built from all four by message().
	Reason string `json:"reason"`
}

// message is the claim path's one-line report for this corruption. The log
// line and every test that checks "the row and column are named" use it, so the
// enumeration route and the skip report cannot describe the same row
// differently.
func (r CorruptRow) message() string {
	return fmt.Sprintf("%s/%s column %s: %s", r.Table, r.RowID, r.Column, r.Reason)
}

// corruptCell decodes one capability list for the enumeration route and turns a
// failure into the same entry the claim path would report. It is a read: the
// value is decoded, never written back.
func corruptCell(table, rowID, column, raw string) *CorruptRow {
	if _, err := decodeList(raw); err != nil {
		return &CorruptRow{Table: table, RowID: rowID, Column: column, Reason: err.Error()}
	}
	return nil
}

// ListCorruptRows returns every cell the claim path knows how to decode that
// cannot be decoded, across all rows (not only the subsets a particular claim
// scan happens to look at). It is READ-ONLY and must stay so: the route exists
// so an operator can find and repair corruption, not so the board can rewrite
// work it could not read.
//
// Each entry carries the table, the row id, the column and decodeList's own
// error. A row with two corrupt columns yields two entries, because each cell is
// separately repairable and the claim path stops at the first one it hits. A
// scan failure is fatal, like the claim path's scans: a value the write path
// cannot produce is a schema/write-path breach, not a repairable JSON cell.
//
// Order is deterministic (work_items, then nodes, then models; row id; column
// order) so an operator diffing two calls sees only real changes.
func ListCorruptRows(ctx context.Context, db *sql.DB) ([]CorruptRow, error) {
	out := []CorruptRow{}

	items, err := db.QueryContext(ctx, `SELECT id, needs FROM work_items ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list work_items: %w", err)
	}
	for items.Next() {
		var id, needs string
		if err := items.Scan(&id, &needs); err != nil {
			items.Close()
			return nil, fmt.Errorf("scan work_items: %w", err)
		}
		if cr := corruptCell("work_items", id, "needs", needs); cr != nil {
			out = append(out, *cr)
		}
	}
	items.Close()
	if err := items.Err(); err != nil {
		return nil, fmt.Errorf("list work_items: %w", err)
	}

	nodes, err := db.QueryContext(ctx, `SELECT id, provides_live, reach_classes FROM nodes ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	for nodes.Next() {
		var id, live, reach string
		if err := nodes.Scan(&id, &live, &reach); err != nil {
			nodes.Close()
			return nil, fmt.Errorf("scan nodes: %w", err)
		}
		if cr := corruptCell("nodes", id, "provides_live", live); cr != nil {
			out = append(out, *cr)
		}
		if cr := corruptCell("nodes", id, "reach_classes", reach); cr != nil {
			out = append(out, *cr)
		}
	}
	nodes.Close()
	if err := nodes.Err(); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	models, err := db.QueryContext(ctx, `SELECT id, provides, features FROM models ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	for models.Next() {
		var id, provides, features string
		if err := models.Scan(&id, &provides, &features); err != nil {
			models.Close()
			return nil, fmt.Errorf("scan models: %w", err)
		}
		if cr := corruptCell("models", id, "provides", provides); cr != nil {
			out = append(out, *cr)
		}
		if cr := corruptCell("models", id, "features", features); cr != nil {
			out = append(out, *cr)
		}
	}
	models.Close()
	if err := models.Err(); err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}

	return out, nil
}

// skippedRowLog is the process-local dedupe for the log report.
type skippedRowLog struct {
	mu   sync.Mutex
	seen map[uint64]struct{}
}

// skippedRows is process-wide because Claim and LoadCandidates are package
// functions, not methods on Server: the claim path has no logger object to hang
// this on, and the dedupe has to survive across calls to be useful.
var skippedRows = skippedRowLog{seen: make(map[uint64]struct{})}

// reportCorruptRow logs one distinct corrupt-row message. Identical messages are
// logged once per process; the hash keeps the map small even if a cell's raw
// value is large.
func reportCorruptRow(r CorruptRow) {
	msg := r.message()

	h := fnv.New64a()
	_, _ = h.Write([]byte(msg))
	key := h.Sum64()

	skippedRows.mu.Lock()
	_, duplicate := skippedRows.seen[key]
	if !duplicate {
		skippedRows.seen[key] = struct{}{}
	}
	skippedRows.mu.Unlock()

	if duplicate {
		return
	}
	log.Printf("board: skipped a corrupt row: %s", msg)
}
