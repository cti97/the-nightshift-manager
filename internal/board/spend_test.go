// internal/board/spend_test.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- fixtures -------------------------------------------------------------
//
// These seed rows directly rather than driving the HTTP API, because the tests
// below are about what the LEDGER says: a run that started yesterday at a cost
// only the report path can produce is not reachable by any sequence of public
// calls, and a test that cannot create yesterday's row could not check the one
// rule this file exists for.

// seedRunForTest inserts one settled run row. Attempts must differ per item:
// runs is append-only with UNIQUE(work_item_id, attempt).
func seedRunForTest(t *testing.T, db *sql.DB, nodeID, itemID, runID string, attempt int, startedAt int64, cost float64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO runs (id,work_item_id,attempt,node_id,started_at,ended_at,status,cost_estimate,placement_reason)
		 VALUES (?,?,?,?,?,?,'ok',?,'seeded by spend_test')`,
		runID, itemID, attempt, nodeID, startedAt, startedAt+10, cost); err != nil {
		t.Fatalf("seed run %s: %v", runID, err)
	}
}

// seedPricedEndpointForTest inserts a healthy endpoint and an enabled model
// with BOTH prices set. SeedEndpointForTest deliberately leaves cost_out at 0,
// which would make an output-pricing mistake invisible here.
func seedPricedEndpointForTest(t *testing.T, db *sql.DB, clock *FixedClock, name string, costIn, costOut float64, features ...string) (endpointID, modelID string) {
	t.Helper()
	endpointID, modelID = "ep-"+name, "m-"+name
	feat := "[]"
	if len(features) > 0 {
		feat = `["` + strings.Join(features, `","`) + `"]`
	}
	if _, err := db.Exec(
		`INSERT INTO endpoints (id,name,provider,base_url,reach,health,created_at)
		 VALUES (?,?,'stub',?, 'host','ok',?)`,
		endpointID, name, "http://"+name, clock.Now()); err != nil {
		t.Fatalf("insert endpoint %s: %v", name, err)
	}
	if _, err := db.Exec(
		`INSERT INTO models (id,endpoint_id,model_id,provides,features,context_window,
		                     cost_in_per_mtok,cost_out_per_mtok,enabled)
		 VALUES (?,?,?,'[]',?,200000,?,?,1)`,
		modelID, endpointID, name+"-model", feat, costIn, costOut); err != nil {
		t.Fatalf("insert model %s: %v", name, err)
	}
	return endpointID, modelID
}

// seedClaimableBoard is the minimum a claim needs: one online node, one healthy
// endpoint with an enabled tools model, and one queued item.
func seedClaimableBoard(t *testing.T, db *sql.DB, clock *FixedClock) (nodeID, itemID string) {
	t.Helper()
	nodeID = SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	itemID = mustCreateItem(t, db, clock, "claimable")
	return nodeID, itemID
}

func mustCreateItem(t *testing.T, db *sql.DB, clock *FixedClock, title string) string {
	t.Helper()
	id, err := CreateItem(context.Background(), db, clock, CreateItemRequest{
		Title: title, Brief: "b", Needs: []string{"agentic"},
	})
	if err != nil {
		t.Fatalf("CreateItem %q: %v", title, err)
	}
	return id
}

func mustClaim(t *testing.T, db *sql.DB, clock *FixedClock, nodeID string, slots int) []ClaimedItem {
	t.Helper()
	got, err := Claim(context.Background(), db, clock, nodeID, slots)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return got
}

// mustSetCeiling both writes the ceiling and fails the test if the write is
// refused, so every test below reads as the operator action it models.
func mustSetCeiling(t *testing.T, db *sql.DB, clock *FixedClock, ceiling float64) SpendStatus {
	t.Helper()
	st, err := SetDailySpendCeiling(context.Background(), db, clock, ceiling)
	if err != nil {
		t.Fatalf("SetDailySpendCeiling(%v): %v", ceiling, err)
	}
	return st
}

func mustSpend(t *testing.T, db *sql.DB, clock *FixedClock) SpendStatus {
	t.Helper()
	st, err := TodaySpend(context.Background(), db, clock)
	if err != nil {
		t.Fatalf("TodaySpend: %v", err)
	}
	return st
}

// ---- judgement call 2: what "today" means --------------------------------

// TestSpendDayIsAUTCCalendarDay pins the window: the UTC calendar day
// containing the injected clock's instant. Not a local day (the board has no
// timezone and every timestamp it stores is server-side Unix seconds) and not a
// rolling 24 hours (a busy board would never get a fresh allowance, and the key
// is named daily).
func TestSpendDayIsAUTCCalendarDay(t *testing.T) {
	// 1_700_000_000 = 2023-11-14T22:13:20Z.
	start, label := SpendDay(1_700_000_000)
	if start != 1_699_920_000 {
		t.Fatalf("day start = %d, want 1699920000 (the UTC midnight of 2023-11-14)", start)
	}
	if label != "2023-11-14" {
		t.Fatalf("day label = %q, want 2023-11-14", label)
	}
	// The second before midnight belongs to the previous day; the first second
	// of the day belongs to it. A window that got either side wrong would let a
	// day's spend leak into its neighbour.
	if s, l := SpendDay(start - 1); s != start-86400 || l != "2023-11-13" {
		t.Fatalf("SpendDay(midnight-1) = %d/%q, want the previous day", s, l)
	}
	if s, _ := SpendDay(start); s != start {
		t.Fatalf("SpendDay(midnight) = %d, want %d", s, start)
	}
	// And it is a pure function of the clock, so a FixedClock pins it.
	clock := NewFixedClock(1_700_000_000)
	if s, _ := SpendDay(clock.Now()); s != start {
		t.Fatalf("SpendDay(clock.Now()) = %d, want %d", s, start)
	}
}

// ---- acceptance: spend sums only today's runs ----------------------------

func TestSpendSumsOnlyTodaysRuns(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	yesterdayItem := mustCreateItem(t, db, clock, "yesterday")
	todayItem := mustCreateItem(t, db, clock, "today")

	dayStart, _ := SpendDay(clock.Now())
	seedRunForTest(t, db, node, yesterdayItem, "run-yesterday", 1, dayStart-1, 1.5)
	seedRunForTest(t, db, node, todayItem, "run-today", 1, dayStart, 0.25)

	st := mustSpend(t, db, clock)
	if st.Spent != 0.25 {
		t.Fatalf("spent today = %v, want 0.25 — yesterday's 1.5 must not count", st.Spent)
	}
	if st.RunCount != 1 {
		t.Fatalf("runs today = %d, want 1", st.RunCount)
	}
	// The boundary is a boundary, not a rough cut: a run one second before
	// midnight is yesterday, and the first second of today is today.
	if _, err := db.Exec(`UPDATE runs SET started_at=? WHERE id='run-today'`, dayStart-1); err != nil {
		t.Fatalf("move the run before midnight: %v", err)
	}
	if st := mustSpend(t, db, clock); st.Spent != 0 {
		t.Fatalf("spent = %v after moving the only run before midnight, want 0", st.Spent)
	}
	_ = ctx
}

// ---- acceptance: the ceiling stops claims and pauses the board -----------

func TestCeilingStopsClaimsAndPausesTheBoard(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node, _ := seedClaimableBoard(t, db, clock)
	spentItem := mustCreateItem(t, db, clock, "already spent")
	clock.Advance(0)

	// One unit of a previous run, against a ceiling of one unit: at or above
	// the ceiling stops issuance, so exactly-at counts as over.
	mustSetCeiling(t, db, clock, 1)
	dayStart, _ := SpendDay(clock.Now())
	seedRunForTest(t, db, node, spentItem, "run-spent", 1, dayStart, 1)

	got := mustClaim(t, db, clock, node, 1)
	if len(got) != 0 {
		t.Fatalf("claimed %d items at the ceiling, want 0", len(got))
	}
	// Nothing was issued: no run row, and the item is still queued and
	// unowned. "Claim returns nothing" has to mean nothing happened.
	var runs, queued int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM runs), (SELECT COUNT(*) FROM work_items WHERE status='queued' AND claimed_by IS NULL)`).
		Scan(&runs, &queued); err != nil {
		t.Fatalf("read board: %v", err)
	}
	if runs != 1 || queued != 2 {
		t.Fatalf("runs=%d queued-ownerless=%d after the refusal, want 1 and 2 (only the seeded run exists and both items are untouched)", runs, queued)
	}

	st := mustSpend(t, db, clock)
	if !st.Paused {
		t.Fatal("the board did not mark itself paused at the ceiling")
	}
	if !strings.Contains(st.PauseReason, "ceiling") {
		t.Fatalf("pause reason = %q, want it to name the ceiling", st.PauseReason)
	}
	if st.PauseReason == "" || !strings.Contains(st.PauseReason, st.Day) {
		t.Fatalf("pause reason = %q, want it to name the day %s", st.PauseReason, st.Day)
	}
	if !st.OverCeiling {
		t.Fatal("over_ceiling is false while paused at the ceiling")
	}

	// The pause is a state change and the audit spine has to show it: an
	// operator reading the event log must see the board stop, once.
	var pausedEvents int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE entity='board' AND type='paused'`).Scan(&pausedEvents); err != nil {
		t.Fatalf("count paused events: %v", err)
	}
	if pausedEvents != 1 {
		t.Fatalf("paused events = %d, want 1", pausedEvents)
	}
	_ = ctx
}

// ---- acceptance: a ceiling of zero is unlimited --------------------------

func TestCeilingOfZeroMeansUnlimited(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, db *sql.DB, clock *FixedClock)
	}{
		{
			name:  "no ceiling configured at all",
			setup: func(t *testing.T, db *sql.DB, clock *FixedClock) {},
		},
		{
			name: "explicitly zero",
			setup: func(t *testing.T, db *sql.DB, clock *FixedClock) {
				mustSetCeiling(t, db, clock, 0)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, clock := newTestDB(t)
			node, _ := seedClaimableBoard(t, db, clock)
			spentItem := mustCreateItem(t, db, clock, "a very expensive day")
			dayStart, _ := SpendDay(clock.Now())
			seedRunForTest(t, db, node, spentItem, "run-spent", 1, dayStart, 5)
			tc.setup(t, db, clock)

			got := mustClaim(t, db, clock, node, 1)
			if len(got) != 1 {
				t.Fatalf("claimed %d items with an unlimited ceiling, want 1", len(got))
			}
			st := mustSpend(t, db, clock)
			if st.Paused || st.OverCeiling {
				t.Fatalf("board paused/unlimited state = %+v, want running", st)
			}
		})
	}
}

// ---- the design point: the ledger is the truth, not a counter ------------

// TestSpendIsComputedFromRunsNotACounter is the task's headline invariant. A
// counter would keep the board paused after the rows were corrected; deriving
// the pause from `runs` means the next claim agrees with what an operator can
// see in the table.
func TestSpendIsComputedFromRunsNotACounter(t *testing.T) {
	db, clock := newTestDB(t)
	node, _ := seedClaimableBoard(t, db, clock)
	spentItem := mustCreateItem(t, db, clock, "spent")
	dayStart, _ := SpendDay(clock.Now())
	mustSetCeiling(t, db, clock, 1)
	seedRunForTest(t, db, node, spentItem, "run-spent", 1, dayStart, 1.2)

	if got := mustClaim(t, db, clock, node, 1); len(got) != 0 {
		t.Fatalf("claimed %d items over the ceiling, want 0", len(got))
	}
	if st := mustSpend(t, db, clock); !st.Paused {
		t.Fatal("board is not paused after tripping the ceiling")
	}

	// Correct the ledger — the row was mispriced — and the next claim must
	// follow the row, not a stored total.
	if _, err := db.Exec(`UPDATE runs SET cost_estimate=0.2 WHERE id='run-spent'`); err != nil {
		t.Fatalf("correct the run cost: %v", err)
	}
	if st := mustSpend(t, db, clock); st.Spent != 0.2 {
		t.Fatalf("spent = %v after correcting the only run, want 0.2", st.Spent)
	}
	if got := mustClaim(t, db, clock, node, 1); len(got) != 1 {
		t.Fatalf("claimed %d items after the ledger fell below the ceiling, want 1 — the pause followed a stored total", len(got))
	}
	st := mustSpend(t, db, clock)
	if st.Paused || st.PauseReason != "" {
		t.Fatalf("board still paused (%q) after the ledger fell below the ceiling", st.PauseReason)
	}
	var resumed int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE entity='board' AND type='resumed'`).Scan(&resumed); err != nil {
		t.Fatalf("count resumed events: %v", err)
	}
	if resumed != 1 {
		t.Fatalf("resumed events = %d, want 1", resumed)
	}
}

// ---- pricing -------------------------------------------------------------

// TestReportPricesARunFromTheModelCosts is the half the plan states and the
// code did not do: cost_estimate comes from the run's usage and the model's
// per-Mtok prices, written by the report that settles the run.
func TestReportPricesARunFromTheModelCosts(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	seedPricedEndpointForTest(t, db, clock, "priced", 3, 15, "tools")
	itemID := mustCreateItem(t, db, clock, "priced work")

	got := mustClaim(t, db, clock, node, 1)
	if len(got) != 1 {
		t.Fatalf("claimed %d items, want 1", len(got))
	}
	runID := got[0].RunID
	if err := StartRun(ctx, db, clock, runID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := Report(ctx, db, clock, runID, node, ReportRequest{
		Status: "ok", Summary: "done", TokensIn: 1_000_000, TokensOut: 1_000_000,
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	var cost float64
	if err := db.QueryRow(`SELECT cost_estimate FROM runs WHERE id=?`, runID).Scan(&cost); err != nil {
		t.Fatalf("read cost_estimate: %v", err)
	}
	if cost != 18 {
		t.Fatalf("cost_estimate = %v, want 18 (1M in at 3 plus 1M out at 15)", cost)
	}
	if st := mustSpend(t, db, clock); st.Spent != 18 {
		t.Fatalf("spent today = %v, want the run's 18", st.Spent)
	}
	// The pricing has to reach the audit spine too: "what was this run charged"
	// is a question of the run's record, not of today's totals.
	events, err := ListEvents(ctx, db, "work_item", itemID, 50)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var reported string
	for _, e := range events {
		if e.Type == "reported" {
			reported = e.Payload
		}
	}
	if !strings.Contains(reported, `"cost":18`) {
		t.Fatalf("reported payload = %q, want the run's cost", reported)
	}
}

// TestCostEstimateIsFrozenAtReportTime pins the second self-review question.
// Frozen, not recomputed: a price edit must not rewrite what a day already
// cost, or an operator fixing a typo would retroactively trip the ceiling and
// the number the dashboard showed yesterday would change underneath them.
func TestCostEstimateIsFrozenAtReportTime(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	_, modelID := seedPricedEndpointForTest(t, db, clock, "priced", 3, 15, "tools")
	mustCreateItem(t, db, clock, "priced work")

	got := mustClaim(t, db, clock, node, 1)
	runID := got[0].RunID
	if err := StartRun(ctx, db, clock, runID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := Report(ctx, db, clock, runID, node, ReportRequest{
		Status: "ok", TokensIn: 1_000_000, TokensOut: 1_000_000,
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	// The operator re-prices the model, then disables it. Neither may move the
	// settled run's cost: it is history, and the ledger is what the ceiling
	// reads.
	newIn, newOut, off := 100.0, 100.0, false
	if _, err := PatchModel(ctx, db, clock, modelID, PatchModelRequest{
		CostInPerMtok: &newIn, CostOutPerMtok: &newOut, Enabled: &off,
	}); err != nil {
		t.Fatalf("PatchModel: %v", err)
	}

	var cost float64
	if err := db.QueryRow(`SELECT cost_estimate FROM runs WHERE id=?`, runID).Scan(&cost); err != nil {
		t.Fatalf("read cost_estimate: %v", err)
	}
	if cost != 18 {
		t.Fatalf("cost_estimate = %v after a price edit, want the frozen 18", cost)
	}
	if st := mustSpend(t, db, clock); st.Spent != 18 {
		t.Fatalf("spent today = %v after a price edit, want the frozen 18", st.Spent)
	}
}

// TestReservedTokensArePricedAtTheInputRate pins judgement call 1, which
// becomes the meaning of runs.reserved_tokens: a reservation carries no in/out
// split, so it is charged at the model's INPUT rate — the prompt is the part a
// failed attempt certainly sent, and inventing its unobserved output at the
// output rate would dominate the ledger with reservation artifacts.
func TestReservedTokensArePricedAtTheInputRate(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	seedPricedEndpointForTest(t, db, clock, "priced", 3, 15, "tools")
	mustCreateItem(t, db, clock, "one failed attempt")

	got := mustClaim(t, db, clock, node, 1)
	runID := got[0].RunID
	if err := StartRun(ctx, db, clock, runID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	// A retried run: one attempt reported no usage, so the executor charged its
	// reservation against the run budget. Input 0 / output 0 / reserved 1M.
	if _, err := Report(ctx, db, clock, runID, node, ReportRequest{
		Status: "ok", TokensIn: 0, TokensOut: 0, ReservedTokens: 1_000_000,
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	var cost float64
	if err := db.QueryRow(`SELECT cost_estimate FROM runs WHERE id=?`, runID).Scan(&cost); err != nil {
		t.Fatalf("read cost_estimate: %v", err)
	}
	if cost != 3 {
		t.Fatalf("cost_estimate = %v for 1M reserved tokens, want 3 (the input rate, not the output rate 15)", cost)
	}
}

func TestRunCostPricing(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		in, out, reserved     int
		costIn, costOut, want float64
	}{
		{"a free model costs nothing", 1_000_000, 1_000_000, 0, 0, 0, 0},
		{"in and out at their own rates", 1_000_000, 1_000_000, 0, 2, 10, 12},
		{"reserved is charged at the input rate", 0, 0, 1_000_000, 2, 10, 2},
		{"reserved rides alongside measured tokens", 1_000_000, 500_000, 250_000, 2, 10, 2 + 5 + 0.5},
		{"no tokens costs nothing", 0, 0, 0, 75, 75, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RunCost(tc.in, tc.out, tc.reserved, tc.costIn, tc.costOut); got != tc.want {
				t.Fatalf("RunCost = %v, want %v", got, tc.want)
			}
		})
	}
}

// An un-priced model does cost nothing forever, which is legitimate for a local
// model and wrong for a paid one the operator forgot to price. The ledger says
// which runs were free so the number can be trusted or questioned.
func TestUnpricedRunsAreVisible(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node, _ := seedClaimableBoard(t, db, clock) // cost 0/0
	mustCreateItem(t, db, clock, "free work")

	got := mustClaim(t, db, clock, node, 1)
	runID := got[0].RunID
	if err := StartRun(ctx, db, clock, runID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := Report(ctx, db, clock, runID, node, ReportRequest{
		Status: "ok", TokensIn: 4_000, TokensOut: 900,
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	st := mustSpend(t, db, clock)
	if st.Spent != 0 {
		t.Fatalf("spent = %v on an unpriced model, want 0", st.Spent)
	}
	if st.UnpricedRuns != 1 {
		t.Fatalf("unpriced runs = %d, want 1: a run that consumed tokens and cost nothing must not be invisible", st.UnpricedRuns)
	}
	if st.RunCount != 1 {
		t.Fatalf("runs = %d, want 1", st.RunCount)
	}
}

// Claiming is not spending. The ceiling gates issuance; an issued run costs
// money only when it reports. This is the honest bound of a settled-spend
// gate, and it is stated here rather than discovered in an incident.
func TestClaimingDoesNotSpend(t *testing.T) {
	db, clock := newTestDB(t)
	node, _ := seedClaimableBoard(t, db, clock)
	mustSetCeiling(t, db, clock, 1)
	mustCreateItem(t, db, clock, "issued work")

	if got := mustClaim(t, db, clock, node, 1); len(got) != 1 {
		t.Fatalf("claimed %d items below the ceiling, want 1", len(got))
	}
	if st := mustSpend(t, db, clock); st.Spent != 0 {
		t.Fatalf("spent = %v after a claim, want 0: only a report prices a run", st.Spent)
	}
}

// ---- judgement call 3: what the ceiling stops, and what a pause must not ----

func TestPausedBoardStillHeartbeatsReapsAndReports(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	seedPricedEndpointForTest(t, db, clock, "priced", 1, 1, "tools")
	for i := 0; i < 3; i++ {
		mustCreateItem(t, db, clock, fmt.Sprintf("item %d", i))
	}
	got := mustClaim(t, db, clock, node, 3)
	if len(got) != 3 {
		t.Fatalf("claimed %d items, want 3", len(got))
	}
	for _, it := range got {
		if err := StartRun(ctx, db, clock, it.RunID, node); err != nil {
			t.Fatalf("StartRun: %v", err)
		}
	}
	// Settle one run for one unit, then set a one-unit ceiling: the board
	// pauses immediately, with two runs still in flight.
	if _, err := Report(ctx, db, clock, got[0].RunID, node, ReportRequest{
		Status: "ok", TokensIn: 1_000_000,
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if st := mustSetCeiling(t, db, clock, 1); !st.Paused {
		t.Fatal("setting a ceiling at or below today's spend did not pause the board")
	}

	// 1. A paused board still heartbeats, and the heartbeat still extends the
	// leases of the runs in flight. A pause that stops heartbeats turns every
	// in-flight run into a lost one.
	clock.Advance(60 * time.Second)
	hb, err := Heartbeat(ctx, db, clock, node, HeartbeatRequest{
		ProvidesLive: []string{"agentic"}, ReachClasses: []string{"host", "lan", "wan"}, Protocol: ProtocolMax,
	})
	if err != nil {
		t.Fatalf("Heartbeat while paused: %v", err)
	}
	if hb.Status != "online" {
		t.Fatalf("heartbeat status = %q while paused, want online", hb.Status)
	}
	wantLease := clock.Now() + leaseSeconds
	if hb.LeaseUntil != wantLease {
		t.Fatalf("lease_until = %d, want %d: a paused board must keep extending leases", hb.LeaseUntil, wantLease)
	}

	// 2. A paused board still accepts reports, and the spend they record lands
	// in the ledger.
	if _, err := Report(ctx, db, clock, got[1].RunID, node, ReportRequest{
		Status: "ok", TokensIn: 1_000_000,
	}); err != nil {
		t.Fatalf("Report while paused: %v", err)
	}
	if st := mustSpend(t, db, clock); st.Spent != 2 {
		t.Fatalf("spent = %v after reporting while paused, want 2", st.Spent)
	}

	// 3. A paused board still reaps: the third run's lease expires, and the
	// work must go back to the queue instead of being stranded.
	clock.Advance(time.Duration(leaseSeconds+1) * time.Second)
	res, err := Reap(ctx, db, clock)
	if err != nil {
		t.Fatalf("Reap while paused: %v", err)
	}
	if res.Lost != 1 {
		t.Fatalf("reaped lost = %d, want 1 (the expired in-flight run)", res.Lost)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM work_items WHERE id=?`, got[2].Item.ID).Scan(&status); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "queued" {
		t.Fatalf("item status = %q after the reap, want queued", status)
	}

	// And the pause is still in force: none of the three cleared it, and the
	// requeued item must not be issued.
	if st := mustSpend(t, db, clock); !st.Paused {
		t.Fatal("the pause was cleared by a heartbeat, a report or a reap")
	}
	if again := mustClaim(t, db, clock, node, 1); len(again) != 0 {
		t.Fatalf("claimed %d items while still over the ceiling, want 0", len(again))
	}
}

// TestCeilingResumesAtTheNextUTCDay: a daily ceiling that paused at 23:59 must
// not keep the board dead all the next day. The pause is a projection of the
// ledger, so a new day with no spend is below the ceiling and the gate clears
// it — with an event, because a state change nobody can find in the log is a
// state change an operator has to guess at.
func TestCeilingResumesAtTheNextUTCDay(t *testing.T) {
	db, clock := newTestDB(t)
	node, _ := seedClaimableBoard(t, db, clock)
	spentItem := mustCreateItem(t, db, clock, "yesterday's spend")
	dayStart, _ := SpendDay(clock.Now())
	mustSetCeiling(t, db, clock, 1)
	seedRunForTest(t, db, node, spentItem, "run-spent", 1, dayStart, 1)
	if got := mustClaim(t, db, clock, node, 1); len(got) != 0 {
		t.Fatalf("claimed %d items at the ceiling, want 0", len(got))
	}
	if st := mustSpend(t, db, clock); !st.Paused {
		t.Fatal("board is not paused at the ceiling")
	}

	clock.Advance(24 * time.Hour)

	st := mustSpend(t, db, clock)
	if st.Spent != 0 {
		t.Fatalf("spent = %v on the following day, want 0", st.Spent)
	}
	if got := mustClaim(t, db, clock, node, 1); len(got) != 1 {
		t.Fatalf("claimed %d items on a fresh day, want 1 — the pause did not survive the day boundary", len(got))
	}
	if st := mustSpend(t, db, clock); st.Paused {
		t.Fatalf("board still paused on a fresh day: %q", st.PauseReason)
	}
}

// TestRaisingTheCeilingResumesTheBoard: raising the ceiling is how an operator
// un-pauses. The re-evaluation happens in the same transaction as the write, so
// the response already reports the board running again.
func TestRaisingTheCeilingResumesTheBoard(t *testing.T) {
	db, clock := newTestDB(t)
	node, _ := seedClaimableBoard(t, db, clock)
	spentItem := mustCreateItem(t, db, clock, "spent")
	dayStart, _ := SpendDay(clock.Now())
	mustSetCeiling(t, db, clock, 1)
	seedRunForTest(t, db, node, spentItem, "run-spent", 1, dayStart, 1.25)
	if got := mustClaim(t, db, clock, node, 1); len(got) != 0 {
		t.Fatalf("claimed %d items at the ceiling, want 0", len(got))
	}

	// 1.25 spent, so a ceiling of 1.2 is still reached and must not resume.
	if st := mustSetCeiling(t, db, clock, 1.2); !st.Paused {
		t.Fatal("raising the ceiling to below today's spend resumed the board")
	}
	st := mustSetCeiling(t, db, clock, 2)
	if st.Paused {
		t.Fatalf("board still paused (%q) after raising the ceiling above today's spend", st.PauseReason)
	}
	if got := mustClaim(t, db, clock, node, 1); len(got) != 1 {
		t.Fatalf("claimed %d items after raising the ceiling, want 1", len(got))
	}
	if st := mustSpend(t, db, clock); st.Ceiling != 2 {
		t.Fatalf("ceiling = %v, want the stored 2", st.Ceiling)
	}
}

// TestAForeignPauseIsNeverClearedByTheCeiling: board_paused is a key an
// operator may also write (spec §12). The ceiling owns only the pause it wrote
// itself, so a human "stopped for the deploy" must not be resumed by a spend
// check that finds the ceiling unexceeded.
func TestAForeignPauseIsNeverClearedByTheCeiling(t *testing.T) {
	db, clock := newTestDB(t)
	node, _ := seedClaimableBoard(t, db, clock)
	mustCreateItem(t, db, clock, "work during the deploy")
	if _, err := db.Exec(`INSERT INTO config (key,value) VALUES ('board_paused','deploying')`); err != nil {
		t.Fatalf("write a foreign pause: %v", err)
	}

	if got := mustClaim(t, db, clock, node, 1); len(got) != 0 {
		t.Fatalf("claimed %d items while an operator had paused the board, want 0", len(got))
	}
	st := mustSpend(t, db, clock)
	if !st.Paused || st.PauseReason != "deploying" {
		t.Fatalf("pause state = %+v, want paused with the operator's reason", st)
	}
	if st.PausedBy != pauseByOperator {
		t.Fatalf("paused_by = %q, want %q", st.PausedBy, pauseByOperator)
	}

	// Setting a ceiling must not clear it either.
	mustSetCeiling(t, db, clock, 5)
	if st := mustSpend(t, db, clock); !st.Paused {
		t.Fatal("setting a ceiling cleared an operator's pause")
	}
	if got := mustClaim(t, db, clock, node, 1); len(got) != 0 {
		t.Fatalf("claimed %d items after a ceiling change cleared nothing, want 0", len(got))
	}
	var value string
	if err := db.QueryRow(`SELECT value FROM config WHERE key='board_paused'`).Scan(&value); err != nil {
		t.Fatalf("read board_paused: %v", err)
	}
	if value != "deploying" {
		t.Fatalf("board_paused = %q, want the operator's value untouched", value)
	}
}

// TestConcurrentClaimsCannotBothPassTheCeiling: two nodes claim at once, on two
// real connections (Open caps the pool at one, so a second handle is what makes
// this contention rather than a queue). Both must be refused, and the board
// must write ONE pause, not one per caller.
func TestConcurrentClaimsCannotBothPassTheCeiling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	db, clock := OpenForTest(t, path)
	other, err := Open(path)
	if err != nil {
		t.Fatalf("open a second handle: %v", err)
	}
	t.Cleanup(func() { other.Close() })

	ctx := context.Background()
	nodeA := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	nodeB := SeedNodeForTest(t, db, clock, "b", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	mustCreateItem(t, db, clock, "one")
	mustCreateItem(t, db, clock, "two")
	mustSetCeiling(t, db, clock, 1)

	dayStart, _ := SpendDay(clock.Now())
	seedRunForTest(t, db, nodeA, mustCreateItem(t, db, clock, "spent"), "run-spent", 1, dayStart, 1)

	handles := []*sql.DB{db, other}
	nodes := []string{nodeA, nodeB}
	var wg sync.WaitGroup
	results := make([][]ClaimedItem, len(nodes))
	errs := make([]error, len(nodes))
	start := make(chan struct{})
	for i := range nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = Claim(ctx, handles[i], clock, nodes[i], 1)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range nodes {
		if errs[i] != nil {
			t.Fatalf("Claim(%d): %v", i, errs[i])
		}
		if len(results[i]) != 0 {
			t.Fatalf("claim %d got %d items past the ceiling", i, len(results[i]))
		}
	}
	var pausedEvents int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE entity='board' AND type='paused'`).Scan(&pausedEvents); err != nil {
		t.Fatalf("count paused events: %v", err)
	}
	if pausedEvents != 1 {
		t.Fatalf("paused events = %d, want 1 (two racing claims must not write two pauses)", pausedEvents)
	}
}

// ---- HTTP -----------------------------------------------------------------

func TestSpendRoutesRequireTheAdminToken(t *testing.T) {
	srv, _ := newServer(t)
	h := srv.Handler()
	for _, tc := range []struct{ method, path string }{
		{"GET", "/admin/spend/today"},
		{"PUT", "/admin/spend/ceiling"},
	} {
		if rec := do(t, h, tc.method, tc.path, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without a token = %d, want 401", tc.method, tc.path, rec.Code)
		}
		if rec := do(t, h, tc.method, tc.path, "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s with a wrong token = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestSpendTodayEndpointReportsTheLedger(t *testing.T) {
	srv, clock := newServer(t)
	node := SeedNodeForTest(t, srv.db, clock, "a", "executor", "agentic")
	dayStart, _ := SpendDay(clock.Now())
	seedRunForTest(t, srv.db, node, mustCreateItem(t, srv.db, clock, "seeded"), "run-seeded", 1, dayStart, 0.5)
	mustSetCeiling(t, srv.db, clock, 2)

	rec := do(t, srv.Handler(), "GET", "/admin/spend/today", testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Day      string  `json:"day"`
		DayStart int64   `json:"day_start"`
		Spent    float64 `json:"spent"`
		Ceiling  float64 `json:"ceiling"`
		Runs     int     `json:"runs"`
		Pricing  string  `json:"pricing"`
		ByModel  []struct {
			ModelID string  `json:"model_id"`
			Spent   float64 `json:"spent"`
			Runs    int     `json:"runs"`
		} `json:"by_model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Day != "2023-11-14" || body.DayStart != dayStart {
		t.Fatalf("day = %q/%d, want 2023-11-14/%d", body.Day, body.DayStart, dayStart)
	}
	if body.Spent != 0.5 || body.Ceiling != 2 || body.Runs != 1 {
		t.Fatalf("body = %+v, want spent 0.5 of ceiling 2 over 1 run", body)
	}
	if body.Pricing == "" {
		t.Fatal("the ledger does not describe how it prices; an operator cannot check a number that does not explain itself")
	}
}

func TestSetCeilingEndpointValidates(t *testing.T) {
	srv, _ := newServer(t)
	h := srv.Handler()
	for _, tc := range []struct {
		name string
		body any
		want int
	}{
		{"absent ceiling", map[string]any{}, http.StatusBadRequest},
		{"negative ceiling", map[string]any{"ceiling": -1}, http.StatusBadRequest},
		{"null ceiling", map[string]any{"ceiling": nil}, http.StatusBadRequest},
		{"zero is accepted as unlimited", map[string]any{"ceiling": 0}, http.StatusOK},
		{"a positive ceiling", map[string]any{"ceiling": 2.5}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, h, "PUT", "/admin/spend/ceiling", testAdminToken, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestTheCeilingCannotRecallInFlightWork states the honest bound of a
// settled-spend gate rather than leaving it to be discovered during an
// incident: the ceiling decides whether the board ISSUES work, and a run that
// was issued below the ceiling can report a cost that crosses it. What must
// then be true is that the ledger shows the overshoot and the very next claim
// stops — the failure this design refuses is a silent one, not an overshoot
// that the numbers explain.
func TestTheCeilingCannotRecallInFlightWork(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	seedPricedEndpointForTest(t, db, clock, "priced", 1, 1, "tools")
	mustCreateItem(t, db, clock, "expensive work")
	mustCreateItem(t, db, clock, "waiting work")
	mustSetCeiling(t, db, clock, 0.5)

	got := mustClaim(t, db, clock, node, 1)
	if len(got) != 1 {
		t.Fatalf("claimed %d items below the ceiling, want 1", len(got))
	}
	if err := StartRun(ctx, db, clock, got[0].RunID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := Report(ctx, db, clock, got[0].RunID, node, ReportRequest{
		Status: "ok", TokensIn: 1_000_000,
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	st := mustSpend(t, db, clock)
	if st.Spent != 1 || !st.OverCeiling {
		t.Fatalf("spent/over = %v/%v after the report, want 1 and over a 0.5 ceiling", st.Spent, st.OverCeiling)
	}
	if next := mustClaim(t, db, clock, node, 1); len(next) != 0 {
		t.Fatalf("claimed %d more items after the overshoot, want 0", len(next))
	}
	if st := mustSpend(t, db, clock); !st.Paused {
		t.Fatal("the board did not stop after the overshoot")
	}
}

// TestWhySaysTheBoardIsPausedAndWhy closes the gap between placement and the
// gate. Without it /why answers "placed: true" for an item the board will never
// issue, which sends an operator looking for a placement bug that does not
// exist.
func TestWhySaysTheBoardIsPausedAndWhy(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node, _ := seedClaimableBoard(t, db, clock)
	itemID := mustCreateItem(t, db, clock, "waiting")
	dayStart, _ := SpendDay(clock.Now())
	mustSetCeiling(t, db, clock, 1)
	seedRunForTest(t, db, node, itemID, "run-spent", 1, dayStart, 1)
	if got := mustClaim(t, db, clock, node, 1); len(got) != 0 {
		t.Fatalf("claimed %d items at the ceiling, want 0", len(got))
	}

	ex, err := Explain(ctx, db, clock, itemID)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !ex.Paused || ex.PauseReason == "" {
		t.Fatalf("explanation = %+v, want it to report the pause", ex)
	}
	if !strings.HasPrefix(ex.BlockedReason, "the board is not issuing work") {
		t.Fatalf("blocked_reason = %q, want the pause named as the reason the item is not running", ex.BlockedReason)
	}
	if !strings.Contains(ex.BlockedReason, "ceiling") {
		t.Fatalf("blocked_reason = %q, want the ceiling named", ex.BlockedReason)
	}
}

// TestAPauseTypedLikeTheCeilingsIsStillTheOperators pins the ownership rule
// (review finding 1). Ownership is a property of the key, not of the text: the
// most plausible operator pause is the board's own message edited to say what
// they mean, and an earlier draft treated a "spend_ceiling: " prefix as proof
// the ceiling owned the row — so an operator's stop was lifted by the next
// arithmetic check.
func TestAPauseTypedLikeTheCeilingsIsStillTheOperators(t *testing.T) {
	db, clock := newTestDB(t)
	node, _ := seedClaimableBoard(t, db, clock)
	mustCreateItem(t, db, clock, "work during a hold")

	_, day := SpendDay(clock.Now())
	// Deliberately the exact text this package writes, with the reason edited.
	typed := "spend_ceiling: daily spend ceiling 0.5 reached: hold for the deploy (UTC " + day + ")"
	if _, err := db.Exec(`INSERT INTO config (key,value) VALUES ('board_paused',?)`, typed); err != nil {
		t.Fatalf("write an operator pause: %v", err)
	}

	if got := mustClaim(t, db, clock, node, 1); len(got) != 0 {
		t.Fatalf("claimed %d items during an operator's hold, want 0", len(got))
	}
	st := mustSpend(t, db, clock)
	if !st.Paused || st.PausedBy != pauseByOperator || st.PauseReason != typed {
		t.Fatalf("pause state = %+v, want the operator's hold reported verbatim", st)
	}

	// Raise the ceiling: this un-pauses a ceiling pause and must not touch this
	// one. Without the two-key rule, the raise below would resume the board.
	mustSetCeiling(t, db, clock, 1000)
	if got := mustClaim(t, db, clock, node, 1); len(got) != 0 {
		t.Fatalf("claimed %d items after raising the ceiling, want 0: the ceiling lifted an operator's pause", len(got))
	}
	if st := mustSpend(t, db, clock); !st.Paused || st.PausedBy != pauseByOperator {
		t.Fatalf("pause state after raising the ceiling = %+v, want the operator's hold intact", st)
	}
	var value string
	if err := db.QueryRow(`SELECT value FROM config WHERE key='board_paused'`).Scan(&value); err != nil {
		t.Fatalf("read board_paused: %v", err)
	}
	if value != typed {
		t.Fatalf("board_paused = %q, want the operator's value untouched", value)
	}
}

// TestTheCeilingWritesItsOwnKeyNotTheOperatorsKey pins the other half of the
// ownership rule: the ceiling's projection lives in its own config row, so a
// human opening the config table (or a future operator-pause route) can tell
// which stops came from arithmetic and which came from a person.
func TestTheCeilingWritesItsOwnKeyNotTheOperatorsKey(t *testing.T) {
	db, clock := newTestDB(t)
	node, _ := seedClaimableBoard(t, db, clock)
	spentItem := mustCreateItem(t, db, clock, "spent")
	dayStart, _ := SpendDay(clock.Now())
	mustSetCeiling(t, db, clock, 1)
	seedRunForTest(t, db, node, spentItem, "run-spent", 1, dayStart, 1)
	if got := mustClaim(t, db, clock, node, 1); len(got) != 0 {
		t.Fatalf("claimed %d items at the ceiling, want 0", len(got))
	}

	var mine int
	if err := db.QueryRow(`SELECT COUNT(*) FROM config WHERE key='spend_ceiling_paused'`).Scan(&mine); err != nil {
		t.Fatalf("count: %v", err)
	}
	if mine != 1 {
		t.Fatalf("spend_ceiling_paused rows = %d, want 1", mine)
	}
	var operators int
	if err := db.QueryRow(`SELECT COUNT(*) FROM config WHERE key='board_paused'`).Scan(&operators); err != nil {
		t.Fatalf("count: %v", err)
	}
	if operators != 0 {
		t.Fatalf("board_paused rows = %d after a ceiling pause, want 0: the ceiling wrote the operator's key", operators)
	}
}

// TestSpendDayWindowHasAnUpperBound pins review finding 2: "since midnight" is
// not "today". A clock that steps backwards across midnight would otherwise fold
// the new day's rows into the old one and spend the same money twice.
func TestSpendDayWindowHasAnUpperBound(t *testing.T) {
	db, clock := newTestDB(t)
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	dayStart, _ := SpendDay(clock.Now())
	yesterdayItem := mustCreateItem(t, db, clock, "yesterday")
	todayItem := mustCreateItem(t, db, clock, "today")
	tomorrowItem := mustCreateItem(t, db, clock, "tomorrow")

	seedRunForTest(t, db, node, yesterdayItem, "run-yesterday", 1, dayStart-1, 9)
	seedRunForTest(t, db, node, todayItem, "run-today", 1, dayStart, 0.5)
	seedRunForTest(t, db, node, tomorrowItem, "run-tomorrow", 1, dayStart+secondsPerDay, 7)

	st := mustSpend(t, db, clock)
	if st.Spent != 0.5 || st.RunCount != 1 {
		t.Fatalf("spent/runs = %v/%d, want 0.5 and 1 (only the run inside today's half-open window)", st.Spent, st.RunCount)
	}
	if len(st.ByModel) != 1 || st.ByModel[0].Runs != 1 {
		t.Fatalf("by_model = %+v, want one entry covering exactly today's run", st.ByModel)
	}

	// Now step the clock back to 23:59 of the previous day, which is the shape a
	// backwards clock correction produces: the window is yesterday's, and today's
	// row is in its FUTURE. Without the upper bound it would be counted again.
	clock.Set(dayStart - 60)
	st = mustSpend(t, db, clock)
	if st.Spent != 9 || st.RunCount != 1 {
		t.Fatalf("spent/runs with the clock one minute before midnight = %v/%d, want 9 and 1: today's row folded into yesterday", st.Spent, st.RunCount)
	}
}

// TestWhyComposesAPauseWithAPlacementFailure pins review finding 3: an item can
// be unplaceable AND on a paused board, and /why must report both. Overriding
// the placement failure with the pause makes the second cause appear only after
// the operator raises the ceiling — an explanation that changes as you act on it.
func TestWhyComposesAPauseWithAPlacementFailure(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node, _ := seedClaimableBoard(t, db, clock) // provides agentic; nothing provides gpu
	itemID, err := CreateItem(ctx, db, clock, CreateItemRequest{
		Title: "needs a gpu", Brief: "b", Needs: []string{"agentic", "gpu"},
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	spentItem := mustCreateItem(t, db, clock, "spent")
	dayStart, _ := SpendDay(clock.Now())
	mustSetCeiling(t, db, clock, 1)
	seedRunForTest(t, db, node, spentItem, "run-spent", 1, dayStart, 1)
	if got := mustClaim(t, db, clock, node, 1); len(got) != 0 {
		t.Fatalf("claimed %d items at the ceiling, want 0", len(got))
	}

	ex, err := Explain(ctx, db, clock, itemID)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !ex.Paused {
		t.Fatal("explanation does not report the pause")
	}
	if ex.Placed {
		t.Fatal("explanation says the gpu item is placeable")
	}
	if !strings.Contains(ex.BlockedReason, "no eligible candidate") || !strings.Contains(ex.BlockedReason, "gpu") {
		t.Fatalf("blocked_reason = %q, want the placement failure named", ex.BlockedReason)
	}
	if !strings.Contains(ex.BlockedReason, "not issuing work") {
		t.Fatalf("blocked_reason = %q, want the pause named as well", ex.BlockedReason)
	}
}

// A3: a validation failure is the operator's mistake (400); a store failure is
// the board's (500). Both keep the same JSON error envelope, because a client
// decodes one shape regardless of which layer refused.
func TestSetCeilingValidationIs400AndStorageFailureIs500(t *testing.T) {
	srv, clock := newServer(t)
	h := srv.Handler()
	ctx := context.Background()

	// The sentinel is what makes the handler's mapping possible; without it the
	// two error classes are indistinguishable strings.
	if _, err := SetDailySpendCeiling(ctx, srv.db, clock, -1); !errors.Is(err, ErrInvalidCeiling) {
		t.Fatalf("negative ceiling error = %v, want ErrInvalidCeiling", err)
	}

	rec := doJSON(t, h, "PUT", "/admin/spend/ceiling", testAdminToken, map[string]any{"ceiling": -1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative ceiling = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	assertErrorCode(t, rec, "set_ceiling")

	// A closed pool is a storage failure that reaches the store and fails. It
	// must not read as "your ceiling was wrong".
	if err := srv.db.Close(); err != nil {
		t.Fatalf("close the board: %v", err)
	}
	rec = doJSON(t, h, "PUT", "/admin/spend/ceiling", testAdminToken, map[string]any{"ceiling": 1})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("ceiling write against a failed store = %d, want 500 (body %s)", rec.Code, rec.Body)
	}
	assertErrorCode(t, rec, "set_ceiling")
}
