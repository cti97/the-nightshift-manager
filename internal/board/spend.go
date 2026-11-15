// internal/board/spend.go
package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// This file is the daily spend ledger and the ceiling that gates issuance.
//
// The one rule it is built around: SPEND IS COMPUTED FROM `runs`, NEVER FROM A
// COUNTER. A counter drifts from the ledger — a corrected row, a re-priced
// model, a run settled by the reaper, a process that died between two writes —
// and the day it disagrees with the rows an operator can see is the day the
// board stops being believable. Every number here is a projection of `runs`, so
// the ceiling, the dashboard and `sqlite3` cannot disagree.
//
// The pause follows from the same rule: it is re-derived on every claim from
// today's rows and the configured ceiling, and the config row `board_paused` is
// the recorded outcome of that derivation rather than an independent authority.
// That is why a corrected cost un-pauses the board on the next claim instead of
// leaving a flag stuck for the rest of the day.

const (
	secondsPerDay = 86400

	// configDailySpendCeiling holds the day's limit in the same unit as
	// runs.cost_estimate. Absent or "0" means unlimited, which is the default:
	// an un-configured board runs.
	configDailySpendCeiling = "daily_spend_ceiling"

	// configBoardPaused records the board's refusal to issue work. A value this
	// package wrote is prefixed with pausePrefixSpendCeiling and is managed by
	// the ceiling's own reconciliation; any other value is an operator's pause
	// and is never rewritten or cleared here. Same key, different owners, and
	// the prefix is what tells them apart (spec §5 lists board_paused in the
	// config table's own comment as an operator-writable key).
	configBoardPaused       = "board_paused"
	pausePrefixSpendCeiling = "spend_ceiling: "

	// pauseBySpendCeiling / pauseByOperator say whose pause is in force, so the
	// dashboard can tell "the ceiling stopped this" from "a human stopped it".
	pauseBySpendCeiling = "spend_ceiling"
	pauseByOperator     = "operator"

	// pricingRule is what the numbers on /admin/spend/today mean. It travels
	// with the response because a spend figure nobody can reproduce is a spend
	// figure nobody should trust.
	pricingRule = "input tokens and reserved tokens at the model's cost_in_per_mtok; output tokens at cost_out_per_mtok; priced once at report time and frozen on the run"
)

// SpendDay returns the calendar day containing the instant now (Unix seconds):
// its first second and its UTC label.
//
// "Today" is a UTC day, not a local one and not a rolling 24 hours:
//
//   - UTC, because every timestamp in this system is server-side Unix seconds
//     and the board has no timezone configuration. A local day would change
//     meaning when the process's TZ changed, and the same rows would then total
//     differently on two machines — the ledger must be reproducible from the
//     clock alone.
//   - a calendar day, not a rolling window, because the ceiling is named
//     `daily_spend_ceiling`: a busy board on a rolling window never gets a fresh
//     allowance, and "why is it still paused at 9am" has no answer. Both windows
//     read the same index (a range on runs.started_at), so the index is not part
//     of the argument; the difference is that a fixed boundary can be
//     pre-aggregated per day later, and within a day the boundary does not move.
//
// The day a run belongs to is decided by runs.started_at — the day the work was
// ISSUED — because that is the decision the ceiling gates, and started_at is
// never NULL while ended_at is. The consequence is stated where it matters: work
// issued at 23:59 and settled at 00:01 counts against the earlier day.
func SpendDay(now int64) (start int64, label string) {
	start = now - now%secondsPerDay
	return start, time.Unix(start, 0).UTC().Format("2006-01-02")
}

// RunCost prices one run's token counts against a model's per-Mtok prices. The
// result is in the same unit as the prices (dollars per million tokens in,
// dollars out).
//
// Reserved tokens are priced at the model's INPUT rate, and that choice is the
// meaning of the column:
//
//   - runs.reserved_tokens is the budget charge for attempts that reported no
//     usage (migration 2). It is not a measurement and it carries no in/out
//     split, so no exact price exists.
//   - It is charged at the input rate because the prompt is the part a failed
//     attempt certainly sent, and because the output portion of a reservation is
//     `max_tokens` that was never observed. Pricing an unobserved maximum at the
//     output rate would multiply a reservation artifact into the day's number —
//     a ledger that over-states by 10x is not more trustworthy than one that
//     under-states, both get ignored.
//   - The honest bound: this can under-count a failed attempt that really did
//     generate output before failing. The direction is deliberate and recorded
//     in the task report; the alternative (excluding reservations entirely)
//     under-counts every retried run, which is what Task 12 added the column to
//     prevent.
//
// Tokens are converted to float before multiplying so an extreme count cannot
// overflow the integer addition.
func RunCost(tokensIn, tokensOut, reservedTokens int, costInPerMtok, costOutPerMtok float64) float64 {
	in := float64(tokensIn) + float64(reservedTokens)
	return (in*costInPerMtok + float64(tokensOut)*costOutPerMtok) / 1e6
}

// ModelSpend is one model's share of today's ledger: the wire name its runs
// recorded and the aggregate over those rows, so an operator can see which model
// is spending the day rather than only that the day is being spent.
type ModelSpend struct {
	ModelID    string  `json:"model_id"`
	EndpointID string  `json:"endpoint_id"`
	Runs       int     `json:"runs"`
	Spend      float64 `json:"spent"`
}

// SpendStatus is today's ledger plus the ceiling's evaluation of it.
//
// Paused/OverCeiling are not the same fact. OverCeiling is the ledger's own
// verdict and is true the moment a report crosses the ceiling; Paused is what
// the next claim will do about it. They are reported separately so an operator
// can see the board heading for a stop before it stops.
type SpendStatus struct {
	Day      string `json:"day"`
	DayStart int64  `json:"day_start"`
	// RunCount is every run ISSUED today, including ones still in flight. They
	// carry no cost yet, so they do not inflate Spent; they are counted because
	// "how much work did the board start today" is the other half of the ledger.
	RunCount       int     `json:"runs"`
	Spent          float64 `json:"spent"`
	Ceiling        float64 `json:"ceiling"`
	OverCeiling    bool    `json:"over_ceiling"`
	Paused         bool    `json:"paused"`
	PausedBy       string  `json:"paused_by,omitempty"`
	PauseReason    string  `json:"pause_reason,omitempty"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	ReservedTokens int64   `json:"reserved_tokens"`
	// UnpricedRuns counts today's runs that consumed tokens and still cost
	// nothing: a free local model, or a model nobody priced. Both are legitimate
	// and only one is a mistake, so the ledger reports the count instead of
	// deciding which it was.
	UnpricedRuns int          `json:"unpriced_runs"`
	Pricing      string       `json:"pricing"`
	ByModel      []ModelSpend `json:"by_model"`
}

// spendReader is the read surface a ledger query needs. *sql.DB, *sql.Tx and
// *sql.Conn all satisfy it, which is what lets the SAME query run inside the
// claim transaction (where reading through db would deadlock: the pool holds
// one connection and the claim already owns it).
type spendReader interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// spendTx is the write surface the ceiling needs. Claim passes its *sql.Conn
// (inside BEGIN IMMEDIATE); an admin write passes its *sql.Tx. One function
// owns the pause in both places, so the two can never disagree about when the
// board stops.
type spendTx interface {
	Execer
	spendReader
}

// TodaySpend is today's ledger: what the runs table says was spent since UTC
// midnight, against the configured ceiling. It is a read and writes nothing.
func TodaySpend(ctx context.Context, db *sql.DB, clock Clock) (SpendStatus, error) {
	now := clock.Now()
	dayStart, day := SpendDay(now)

	st, err := sumSpend(ctx, db, dayStart)
	if err != nil {
		return SpendStatus{}, err
	}
	st.Day, st.DayStart = day, dayStart

	if st.Ceiling, err = readCeiling(ctx, db); err != nil {
		return SpendStatus{}, err
	}
	st.OverCeiling = ceilingReached(st.Ceiling, st.Spent)

	// The same function the claim gate's rule lives in, so the dashboard and
	// /why cannot describe a gate that behaves differently from the one that
	// will refuse the next claim.
	if st.Paused, st.PauseReason, st.PausedBy, err = pauseState(ctx, db, clock); err != nil {
		return SpendStatus{}, err
	}

	if st.ByModel, err = spendByModel(ctx, db, dayStart); err != nil {
		return SpendStatus{}, err
	}
	st.Pricing = pricingRule
	return st, nil
}

// SetDailySpendCeiling writes the day's limit and re-evaluates the pause in the
// same transaction, so raising the ceiling above today's spend resumes the board
// immediately and lowering it below today's spend stops it immediately.
//
// This is also the operator's un-pause: a pause the ceiling caused is cleared by
// a ceiling that no longer reaches, or by 0 (unlimited). Deliberately there is
// no bare "clear the flag" action — the flag is a projection of the ledger, so
// clearing it without changing the ledger would only make the next claim pause
// again. An operator pause (a board_paused value this package did not write) is
// not touched here at all.
func SetDailySpendCeiling(ctx context.Context, db *sql.DB, clock Clock, ceiling float64) (SpendStatus, error) {
	if math.IsNaN(ceiling) || math.IsInf(ceiling, 0) {
		return SpendStatus{}, fmt.Errorf("daily spend ceiling %v is not a number", ceiling)
	}
	if ceiling < 0 {
		return SpendStatus{}, fmt.Errorf("daily spend ceiling %v is negative (0 means unlimited)", ceiling)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return SpendStatus{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO config (key,value) VALUES (?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		configDailySpendCeiling, formatAmount(ceiling)); err != nil {
		return SpendStatus{}, fmt.Errorf("set %s: %w", configDailySpendCeiling, err)
	}
	if err := AppendEvent(ctx, tx, clock, "admin", "board", "board", "spend_ceiling_set",
		map[string]any{"ceiling": ceiling}); err != nil {
		return SpendStatus{}, err
	}
	if _, _, err := reconcileCeiling(ctx, tx, clock); err != nil {
		return SpendStatus{}, err
	}
	if err := tx.Commit(); err != nil {
		return SpendStatus{}, err
	}
	return TodaySpend(ctx, db, clock)
}

// reconcileCeiling evaluates the ceiling against today's runs and makes the
// recorded pause agree with the answer. It runs inside the caller's transaction
// — the claim's BEGIN IMMEDIATE, or the ceiling write's — so the decision it
// returns is the decision that transaction acts on.
//
// It returns whether the board is refusing work and why. Writes are the pause
// itself and the event that records it: a state change an operator cannot find
// in the audit spine is a state change they have to guess at.
func reconcileCeiling(ctx context.Context, q spendTx, clock Clock) (paused bool, reason string, err error) {
	now := clock.Now()
	dayStart, day := SpendDay(now)

	ceiling, err := readCeiling(ctx, q)
	if err != nil {
		return false, "", err
	}
	st, err := sumSpend(ctx, q, dayStart)
	if err != nil {
		return false, "", err
	}
	over := ceilingReached(ceiling, st.Spent)

	raw, err := readBoardPause(ctx, q)
	if err != nil {
		return false, "", err
	}
	paused, reason, _ = effectivePause(over, raw, ceiling, st.Spent, day)

	switch {
	case raw != "" && !isCeilingPause(raw):
		// An operator's pause. It is the authority on stopping work, and this
		// package neither rewrites nor clears it.
	case over && raw == "":
		value := pausePrefixSpendCeiling + pauseReason(ceiling, st.Spent, day)
		if _, err := q.ExecContext(ctx,
			`INSERT INTO config (key,value) VALUES (?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			configBoardPaused, value); err != nil {
			return false, "", fmt.Errorf("record the spend pause: %w", err)
		}
		if err := AppendEvent(ctx, q, clock, "system", "board", "board", "paused",
			map[string]any{"reason": reason, "spent": st.Spent, "ceiling": ceiling, "day": day}); err != nil {
			return false, "", err
		}
	case over:
		// Already paused by the ceiling. The stored reason records the report
		// that tripped it, which is history; rewriting it every claim would
		// make the record follow the total instead of the trip.
	case raw != "":
		if _, err := q.ExecContext(ctx,
			`DELETE FROM config WHERE key=?`, configBoardPaused); err != nil {
			return false, "", fmt.Errorf("clear the spend pause: %w", err)
		}
		if err := AppendEvent(ctx, q, clock, "system", "board", "board", "resumed",
			map[string]any{"spent": st.Spent, "ceiling": ceiling, "day": day, "was": raw}); err != nil {
			return false, "", err
		}
	}
	return paused, reason, nil
}

// pauseState answers "is the board issuing work, and if not, why" from a plain
// read. /why and the spend view use it; the claim path evaluates the same rule
// (effectivePause) inside its own transaction via reconcileCeiling, because a
// claim must act on what it read.
func pauseState(ctx context.Context, db *sql.DB, clock Clock) (paused bool, reason, by string, err error) {
	dayStart, day := SpendDay(clock.Now())
	ceiling, err := readCeiling(ctx, db)
	if err != nil {
		return false, "", "", err
	}
	st, err := sumSpend(ctx, db, dayStart)
	if err != nil {
		return false, "", "", err
	}
	raw, err := readBoardPause(ctx, db)
	if err != nil {
		return false, "", "", err
	}
	paused, reason, by = effectivePause(ceilingReached(ceiling, st.Spent), raw, ceiling, st.Spent, day)
	return paused, reason, by, nil
}

// effectivePause is the single rule both the claim gate and the spend view use:
// an operator's pause always stops work; a ceiling's pause stops work only while
// the ceiling is reached. Deriving the ceiling's half is what makes a corrected
// ledger resume the board without anybody clearing a flag.
func effectivePause(over bool, raw string, ceiling, spent float64, day string) (paused bool, reason, by string) {
	switch {
	case raw != "" && !isCeilingPause(raw):
		return true, raw, pauseByOperator
	case over:
		return true, pauseReason(ceiling, spent, day), pauseBySpendCeiling
	default:
		return false, "", ""
	}
}

// ceilingReached is the comparison the whole feature turns on. A ceiling of 0 is
// unlimited — "at or above it" against an absent limit would stop every board
// that was never configured, and an un-configured board must run.
func ceilingReached(ceiling, spent float64) bool {
	return ceiling > 0 && spent >= ceiling
}

// isCeilingPause says whether the stored pause value is one this package wrote.
func isCeilingPause(raw string) bool {
	return strings.HasPrefix(raw, pausePrefixSpendCeiling)
}

// pauseReason is the sentence an operator reads. It names the ceiling, the
// spend that reached it and the day, because "paused" without those three is a
// puzzle rather than a message.
func pauseReason(ceiling, spent float64, day string) string {
	return fmt.Sprintf("daily spend ceiling %s reached: %s spent today (UTC %s)",
		formatAmount(ceiling), formatAmount(spent), day)
}

// formatAmount renders an amount so it round-trips and stays readable: %g would
// print 0.000002 as 2e-06, and %.2f would print it as 0.00 — both lie about a
// number an operator is trying to check.
func formatAmount(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// sumSpend is the ledger query: one pass over the runs issued since dayStart.
//
// Every run counts, whatever its status, because an in-flight run carries no
// tokens and no cost yet and therefore contributes zero — filtering by status
// would only add a second definition of "spent" to keep in sync. A run the
// reaper settled is priced at 0 because it never reported usage: an abandoned
// attempt's tokens are unknowable, and inventing them would be worse than
// under-counting them.
func sumSpend(ctx context.Context, q spendReader, dayStart int64) (SpendStatus, error) {
	var st SpendStatus
	var runs, in, out, reserved, unpriced int64
	err := q.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(input_tokens),0),
		       COALESCE(SUM(output_tokens),0),
		       COALESCE(SUM(reserved_tokens),0),
		       COALESCE(SUM(cost_estimate),0),
		       COALESCE(SUM(CASE WHEN cost_estimate = 0
		                          AND input_tokens+output_tokens+reserved_tokens > 0
		                         THEN 1 ELSE 0 END),0)
		  FROM runs
		 WHERE started_at >= ?`, dayStart).
		Scan(&runs, &in, &out, &reserved, &st.Spent, &unpriced)
	if err != nil {
		return SpendStatus{}, fmt.Errorf("sum today's runs: %w", err)
	}
	st.RunCount, st.InputTokens, st.OutputTokens, st.ReservedTokens, st.UnpricedRuns =
		int(runs), in, out, reserved, int(unpriced)
	return st, nil
}

// spendByModel breaks today's spend down by the model that was used. It is the
// answer to "which model is eating the day", which a single total cannot give.
func spendByModel(ctx context.Context, db *sql.DB, dayStart int64) ([]ModelSpend, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(m.model_id,''), COALESCE(r.endpoint_id,''), COUNT(*),
		       COALESCE(SUM(r.cost_estimate),0)
		  FROM runs r
		  LEFT JOIN models m ON m.id = r.model_id
		 WHERE r.started_at >= ?
		 GROUP BY r.model_id
		 ORDER BY COALESCE(m.model_id,'')`, dayStart)
	if err != nil {
		return nil, fmt.Errorf("spend by model: %w", err)
	}
	defer rows.Close()

	out := []ModelSpend{}
	for rows.Next() {
		var m ModelSpend
		var runs int64
		if err := rows.Scan(&m.ModelID, &m.EndpointID, &runs, &m.Spend); err != nil {
			return nil, err
		}
		m.Runs = int(runs)
		out = append(out, m)
	}
	return out, rows.Err()
}

// readCeiling reads the configured ceiling. Absent means 0 (unlimited).
//
// A value that cannot be parsed is an ERROR, not unlimited: treating a typo as
// "no limit" would silently disable the safety mechanism, and the operator
// would find out from the bill. The error reaches the claim as a 500 that names
// the key, which is loud and local — the config row is writable only by this
// package and by hand, so a broken value is a bug or a raw-SQL edit, and both
// want to be seen.
func readCeiling(ctx context.Context, q spendReader) (float64, error) {
	var raw string
	err := q.QueryRowContext(ctx,
		`SELECT value FROM config WHERE key=?`, configDailySpendCeiling).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", configDailySpendCeiling, err)
	}
	return parseAmount(configDailySpendCeiling, raw)
}

func parseAmount(key, raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("config %s %q is not a number: %w", key, raw, err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, fmt.Errorf("config %s %q is not a non-negative amount", key, raw)
	}
	return v, nil
}

// readBoardPause reads the recorded pause, if any.
func readBoardPause(ctx context.Context, q spendReader) (string, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT value FROM config WHERE key=?`, configBoardPaused).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", configBoardPaused, err)
	}
	return raw, nil
}
