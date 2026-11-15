// internal/board/corrupt.go
package board

import (
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
// never mutated, so the operator also still finds it with `sqlite3`, and /why
// still answers 500 naming the column.
type skippedRowLog struct {
	mu   sync.Mutex
	seen map[uint64]struct{}
}

// skippedRows is process-wide because Claim and LoadCandidates are package
// functions, not methods on Server: the claim path has no logger object to hang
// this on, and the dedupe has to survive across calls to be useful.
var skippedRows = skippedRowLog{seen: make(map[uint64]struct{})}

// reportSkippedRow logs one distinct corrupt-row message. Identical messages are
// logged once per process; the hash keeps the map small even if a cell's raw
// value is large.
func reportSkippedRow(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)

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
