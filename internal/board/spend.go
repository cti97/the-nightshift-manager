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
	// runs.cost_estimate. Absent or a parseable "0" means unlimited, which is
	// the default: an un-configured board runs. A row that exists but cannot be
	// parsed — including an empty one — is an error, not unlimited (readCeiling).
	configDailySpendCeiling = "daily_spend_ceiling"

	// configBoardPaused is the OPERATOR's pause: spec §5 lists it as an
	// operator-writable key, and this package never writes it, rewrites it or
	// deletes it. Any non-empty value stops issuance.
	//
	// Ownership is a property of the KEY, never of the text. An earlier draft
	// of this file inferred ownership from a "spend_ceiling: " prefix inside
	// this key, which meant an operator who edited the pause text the board
	// itself displays — "spend_ceiling: hold for the deploy" — handed their
	// pause to the arithmetic, and the next claim lifted it. A stop a person
	// typed must not be clearable by a sum.
	configBoardPaused = "board_paused"

	// configCeilingPaused is the ceiling's own record, and only this package
	// touches it. It is a materialised projection of the ledger (the ceiling
	// reaches, so the board refuses work), kept on disk so the state is visible
	// to `sqlite3` and survives a restart, and rewritten or deleted by
	// reconcileCeiling whenever the projection changes.
	//
	// The VALUE is the reason as of the report that tripped the ceiling. Nothing
	// reads its text to decide anything: the key's presence is the projection,
	// paused_by is derived from the ledger, and the reason /why and the spend
	// view serve is recomputed from today's spend by effectivePause. The stored
	// sentence therefore describes the trip, not the current total, and can
	// differ from what is served — it is also the "was" recorded on the resume
	// event. It is not a second status, and the two must not be compared.
	configCeilingPaused = "spend_ceiling_paused"

	// pauseByCeiling / pauseByOperator / pauseByBoth are the paused_by
	// vocabulary shared by /why and /admin/spend/today. "Both" is a real state,
	// not a convenience: the two keys have independent lifecycles, so clearing
	// the operator's hold does not resume a board whose ceiling still reaches,
	// and raising the ceiling does not clear a pause a person typed. An operator
	// who is told only "operator" would clear their own key and be surprised.
	pauseByCeiling  = "ceiling"
	pauseByOperator = "operator"
	pauseByBoth     = "ceiling+operator"

	// eventSourceSpendCeiling is the `source` recorded on the pause/resume
	// events. It is deliberately not the paused_by value: events name the
	// subsystem that reconciled, while paused_by names the owner an operator
	// must clear. Existing audit rows carry "spend_ceiling" and stay readable.
	eventSourceSpendCeiling = "spend_ceiling"

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
//
// The day is a half-open interval, [start, start+86400): see sumSpend for why
// the upper bound exists.
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
//
// The prices and the result must be finite. A NaN price is the dangerous case:
// `cost < 0` is false for it (which is how it got stored), the NaN it puts in
// cost_estimate makes ceilingReached false, and `cost_estimate = 0` is false for
// it too, so unpriced_runs cannot see it either. ±Inf is the same class of
// non-amount: +Inf makes the day's total infinity, and -Inf makes
// `spent >= ceiling` false forever. Refusing here is the last line before a
// number enters the ledger: the model write path refuses these values, but a
// row can predate that guard or arrive by raw SQL.
//
// An error means "do not price this run", never "price it at zero": zero is a
// legitimate price (a free local model), and quietly turning corruption into a
// free run is the failure this function exists to stop.
func RunCost(tokensIn, tokensOut, reservedTokens int, costInPerMtok, costOutPerMtok float64) (float64, error) {
	if err := checkPrice("cost_in_per_mtok", costInPerMtok); err != nil {
		return 0, err
	}
	if err := checkPrice("cost_out_per_mtok", costOutPerMtok); err != nil {
		return 0, err
	}
	in := float64(tokensIn) + float64(reservedTokens)
	cost := (in*costInPerMtok + float64(tokensOut)*costOutPerMtok) / 1e6
	// Two finite prices can still multiply into infinity when the token counts
	// and the prices are both extreme; the ledger only accepts a finite amount.
	if math.IsNaN(cost) || math.IsInf(cost, 0) {
		return 0, fmt.Errorf("run cost is not a finite amount (got %v); refusing to put it in the ledger", cost)
	}
	return cost, nil
}

// checkPrice is the one rule a model's per-Mtok price must satisfy: a finite,
// non-negative float. It is shared by the model write path — refusals belong
// where the operator typed the value, answered as a 400 — and by RunCost (via
// Report), because the write path is not the last line: a row can predate the
// guard or be written by raw SQL, the same layering as checkReach in the
// provider package.
//
// Negatives are refused because a negative price makes spend fall (and wins
// placement's cost ordering); NaN and ±Inf because they are not amounts. NaN is
// the one the old `v < 0` test could not see.
//
// Finiteness is not the same as usability. math.MaxFloat64 passes this rule, and
// it is deliberately left unguarded: a run of up to 1e6 tokens prices finitely,
// between about 1.8e302 for one token and 1.8e308 for a million (enough to trip
// any positive ceiling for the rest of the day), while a run above 1e6 tokens
// overflows and Report refuses to ledger it through RunCost's finite-result
// check. A bound here would have to be price ≤ MaxFloat64×1e6/MaxInt64 ≈
// 1.9e295 per Mtok to keep the arithmetic finite for every possible token count,
// which is still absurd; anything tighter is an arbitrary business limit, not a
// correctness rule. The finite-result check in RunCost is the actual backstop.
func checkPrice(name string, v float64) error {
	switch {
	case math.IsNaN(v):
		return fmt.Errorf("%s must be a finite number (got NaN)", name)
	case math.IsInf(v, 0):
		return fmt.Errorf("%s must be a finite number (got %v)", name, v)
	case v < 0:
		return fmt.Errorf("%s must not be negative", name)
	}
	return nil
}

// ModelSpend is one model's share of today's ledger: the models row id, its
// wire name and endpoint, and the aggregate over those rows, so an operator can
// see which model is spending the day AND act on its price without a second
// lookup. ID is the key PATCH /admin/models/{id} takes; model_id is the name the
// endpoint is called with and is not accepted by that route. ID is empty when
// the left join finds no model row (runs.model_id is nullable), and that is a
// case where there is no price to edit.
type ModelSpend struct {
	ID         string  `json:"id"`
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
	RunCount    int     `json:"runs"`
	Spent       float64 `json:"spent"`
	Ceiling     float64 `json:"ceiling"`
	OverCeiling bool    `json:"over_ceiling"`
	Paused      bool    `json:"paused"`
	// PausedBy is "ceiling", "operator", or "ceiling+operator": the owner(s)
	// the next claim is actually obeying. Absent when nothing is paused.
	PausedBy       string `json:"paused_by,omitempty"`
	PauseReason    string `json:"pause_reason,omitempty"`
	InputTokens    int64  `json:"input_tokens"`
	OutputTokens   int64  `json:"output_tokens"`
	ReservedTokens int64  `json:"reserved_tokens"`
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

// ErrInvalidCeiling marks a ceiling the store refused as a value, not a write
// that failed. The HTTP layer answers 400 for this and 500 for everything that
// reaches the database and fails: a locked or broken store is the board's
// problem, not the operator's typo.
var ErrInvalidCeiling = errors.New("invalid daily spend ceiling")

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
		return SpendStatus{}, fmt.Errorf("%w: %v is not a number", ErrInvalidCeiling, ceiling)
	}
	if ceiling < 0 {
		return SpendStatus{}, fmt.Errorf("%w: %v is negative (0 means unlimited)", ErrInvalidCeiling, ceiling)
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

	operator, err := readConfigValue(ctx, q, configBoardPaused)
	if err != nil {
		return false, "", err
	}
	mine, err := readConfigValue(ctx, q, configCeilingPaused)
	if err != nil {
		return false, "", err
	}
	paused, reason, _ = effectivePause(over, operator, ceiling, st.Spent, day)

	// The two keys have independent lifecycles: the operator's is never in this
	// switch at all, and the ceiling's is written or cleared by whether the
	// ceiling reaches. Doing it this way means a board paused by a person can
	// still record that the ceiling is *also* reached, and unpausing either one
	// leaves the other's state intact.
	switch {
	case over && mine == "":
		if _, err := q.ExecContext(ctx,
			`INSERT INTO config (key,value) VALUES (?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			configCeilingPaused, pauseReason(ceiling, st.Spent, day)); err != nil {
			return false, "", fmt.Errorf("record the spend pause: %w", err)
		}
		if err := AppendEvent(ctx, q, clock, "system", "board", "board", "paused",
			map[string]any{"source": eventSourceSpendCeiling, "reason": pauseReason(ceiling, st.Spent, day),
				"spent": st.Spent, "ceiling": ceiling, "day": day}); err != nil {
			return false, "", err
		}
	case over:
		// Already recorded. The stored reason is the report that tripped it, and
		// it stays as the trip record and the presence marker: rewriting it every
		// claim would make the record follow the total instead of the trip.
		// effectivePause does not read it — the reason /why serves is recomputed
		// from today's spend — so the stored sentence and the served one can
		// disagree, by design rather than by drift.
	case mine != "":
		if _, err := q.ExecContext(ctx,
			`DELETE FROM config WHERE key=?`, configCeilingPaused); err != nil {
			return false, "", fmt.Errorf("clear the spend pause: %w", err)
		}
		payload := map[string]any{"source": eventSourceSpendCeiling, "spent": st.Spent,
			"ceiling": ceiling, "day": day, "was": mine}
		if operator != "" {
			// The ceiling's projection cleared, but the board is still refusing
			// work because a person stopped it. Saying "resumed" without this
			// would be the log lying about a state the view reports correctly.
			payload["still_paused_by"] = pauseByOperator
		}
		if err := AppendEvent(ctx, q, clock, "system", "board", "board", "resumed", payload); err != nil {
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
	operator, err := readConfigValue(ctx, db, configBoardPaused)
	if err != nil {
		return false, "", "", err
	}
	paused, reason, by = effectivePause(ceilingReached(ceiling, st.Spent), operator, ceiling, st.Spent, day)
	return paused, reason, by, nil
}

// effectivePause is the single rule both the claim gate and the spend view use:
// an operator's pause always stops work; a ceiling's pause stops work only while
// the ceiling is reached. Deriving the ceiling's half is what makes a corrected
// ledger resume the board without anybody clearing a flag.
//
// operatorReason comes from configBoardPaused, which only a human writes. Its
// CONTENT is irrelevant: any non-empty value is a stop, so text an operator
// copied from the board's own pause message is still an operator's stop.
func effectivePause(over bool, operatorReason string, ceiling, spent float64, day string) (paused bool, reason, by string) {
	switch {
	case operatorReason != "" && over:
		// Both owners are holding: the operator's text is the reason a person
		// reads, but paused_by must say the ceiling also reaches so clearing
		// the operator key is not expected to resume anything.
		return true, operatorReason, pauseByBoth
	case operatorReason != "":
		return true, operatorReason, pauseByOperator
	case over:
		return true, pauseReason(ceiling, spent, day), pauseByCeiling
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

// sumSpend is the ledger query: one pass over the runs issued during the day
// that starts at dayStart.
//
// The window is half-open, [dayStart, dayStart+86400). The upper bound is not
// decoration: `started_at >= dayStart` alone means "since midnight", so a clock
// that stepped backwards across midnight would fold yesterday's rows into today
// and trip the ceiling twice for one day's money. A half-open window makes the
// calendar day the comment claims, whatever the clock does.
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
		 WHERE started_at >= ? AND started_at < ?`, dayStart, dayStart+secondsPerDay).
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
		SELECT COALESCE(m.id,''), COALESCE(m.model_id,''), COALESCE(r.endpoint_id,''), COUNT(*),
		       COALESCE(SUM(r.cost_estimate),0)
		  FROM runs r
		  LEFT JOIN models m ON m.id = r.model_id
		 WHERE r.started_at >= ? AND r.started_at < ?
		 GROUP BY r.model_id
		 ORDER BY COALESCE(m.model_id,'')`, dayStart, dayStart+secondsPerDay)
	if err != nil {
		return nil, fmt.Errorf("spend by model: %w", err)
	}
	defer rows.Close()

	out := []ModelSpend{}
	for rows.Next() {
		var m ModelSpend
		var runs int64
		if err := rows.Scan(&m.ID, &m.ModelID, &m.EndpointID, &runs, &m.Spend); err != nil {
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
//
// a row that EXISTS with an empty or whitespace value is a broken value, not a
// missing one: parsing it as 0 would turn a raw-SQL `INSERT ... VALUES
// ('daily_spend_ceiling',”)` into "unlimited" — the one fail-open left in a
// function built to fail closed.
func readCeiling(ctx context.Context, q spendReader) (float64, error) {
	raw, present, err := readConfigValuePresent(ctx, q, configDailySpendCeiling)
	if err != nil {
		return 0, err
	}
	if !present {
		return 0, nil
	}
	return parseAmount(configDailySpendCeiling, raw)
}

func parseAmount(key, raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("config %s is present but empty; 0 means unlimited and deleting the key restores the default", key)
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

// readConfigValue reads one config row. Absent is the empty string: every key
// this file reads spells its "not configured" state that way.
func readConfigValue(ctx context.Context, q spendReader, key string) (string, error) {
	raw, _, err := readConfigValuePresent(ctx, q, key)
	return raw, err
}

// readConfigValuePresent distinguishes absent from empty, which most callers do
// not need (a pause is "any non-empty value") but readCeiling does: an empty
// ceiling row is an error while a missing one is the default.
func readConfigValuePresent(ctx context.Context, q spendReader, key string) (value string, present bool, err error) {
	var raw string
	err = q.QueryRowContext(ctx, `SELECT value FROM config WHERE key=?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read config %s: %w", key, err)
	}
	return raw, true, nil
}
