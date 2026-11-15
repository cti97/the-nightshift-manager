package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

// fixture is one board with one registered executor node, one local endpoint
// that can run agentic work, and one queued agentic item. Every board-backed
// test starts from exactly this state.
type fixture struct {
	db     *sql.DB
	clock  *board.FixedClock
	nodeID string
	token  string
	itemID string
	srv    *httptest.Server
}

// newFixture builds the board fixture. wrap, when given, decorates the board
// handler so a test can observe traffic (the heartbeat test counts requests).
func newFixture(t *testing.T, wrap ...func(http.Handler) http.Handler) *fixture {
	t.Helper()
	db, clock := board.OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))

	nodeID, token, err := board.RegisterNode(context.Background(), db, clock, board.RegisterRequest{
		Name: "a", Role: "executor", ProvidesDeclared: []string{"agentic"},
		ReachClasses: []string{"host", "lan", "wan"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	board.SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")

	itemID, err := board.CreateItem(context.Background(), db, clock, board.CreateItemRequest{
		Title: "t", Brief: "b", Needs: []string{"agentic"},
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	var h http.Handler = board.NewServer(db, clock, board.HashToken("admin")).Handler()
	for _, w := range wrap {
		h = w(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return &fixture{db: db, clock: clock, nodeID: nodeID, token: token, itemID: itemID, srv: srv}
}

// config is what an agent needs to work this fixture, with the live capability
// set the node actually provides. An empty Provides list would overwrite the
// registration on the first heartbeat and leave the node unqualified for its
// own item, so the fixture never omits it.
func (f *fixture) config(exec Executor) Config {
	return Config{
		BaseURL:  f.srv.URL,
		Token:    f.token,
		NodeID:   f.nodeID,
		Executor: exec,
		Protocol: 1,
		Provides: []string{"agentic"},
		Reaches:  []string{"host", "lan", "wan"},
	}
}

func (f *fixture) itemStatus(t *testing.T) string {
	t.Helper()
	return f.scanString(t, `SELECT status FROM work_items WHERE id=?`, f.itemID)
}

// runStatus is the newest run for the item. After a reclaim there are two, and
// "the run" always means the current one.
func (f *fixture) runStatus(t *testing.T) string {
	t.Helper()
	return f.scanString(t, `SELECT status FROM runs WHERE work_item_id=? ORDER BY attempt DESC LIMIT 1`, f.itemID)
}

func (f *fixture) scanString(t *testing.T, query string, args ...any) string {
	t.Helper()
	var s string
	if err := f.db.QueryRow(query, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return s
}

// waitFor polls cond until it is true and fails at a deadline. It returns as
// soon as cond passes: the deadline is a guard against a hang, never a sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

// startAgent runs the loop in the background. Callers cancel and then wait for
// the returned channel, so a test can assert both that the item reached its
// state and that shutdown returned promptly.
func startAgent(a *Agent, ctx context.Context) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- a.Run(ctx) }()
	return errc
}

func waitAgent(t *testing.T, errc <-chan error, want error) {
	t.Helper()
	select {
	case err := <-errc:
		if !errors.Is(err, want) {
			t.Fatalf("Run = %v, want %v", err, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// TestClientHeartbeatSendsProtocolVersion pins the wire name of the protocol
// field. The board decodes `protocol_version`; a body carrying `protocol`
// leaves the value at 0 and the board marks the node incompatible. That failure
// presents as "the agent never claims", not as a JSON typo, so it is worth a
// test of its own. Marshalling board.HeartbeatRequest is the defence: the field
// name then lives in one place for both ends.
func TestClientHeartbeatSendsProtocolVersion(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bodies <- body
		board.WriteJSON(w, http.StatusOK, board.HeartbeatResponse{Status: "online"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "token")
	if _, err := c.Heartbeat(context.Background(), board.HeartbeatRequest{
		ProvidesLive: []string{"agentic"},
		ReachClasses: []string{"host"},
		FreeSlots:    1,
		Protocol:     1,
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	body := <-bodies
	if got, ok := body["protocol_version"]; !ok || got != float64(1) {
		t.Fatalf("heartbeat body protocol_version = %v (present %v), want 1", got, ok)
	}
	if _, ok := body["protocol"]; ok {
		t.Fatalf("heartbeat body carries a field named %q; the board reads protocol_version", "protocol")
	}
	if _, ok := body["provides_live"]; !ok {
		t.Fatalf("heartbeat body has no provides_live: %v", body)
	}
}

// TestAgentTakesAnItemFromQueuedToDone is the test that makes M1 real: a real
// board over HTTP, a real claim transaction, a stub executor, one result.
func TestAgentTakesAnItemFromQueuedToDone(t *testing.T) {
	f := newFixture(t)

	// observed records what the executor sees. Start must have run before the
	// work: if the agent skipped it, the run would still be 'claimed' here, and
	// without this assertion the test would pass anyway because Report settles
	// a claimed run too. The brief proves the executor gets the claim payload,
	// not just the prose-free placement item.
	type observed struct{ runStatus, brief string }
	seen := make(chan observed, 1)
	done := make(chan Result, 1)
	exec := StubExecutor{OnRun: func(item board.ClaimedItem) (Result, error) {
		var obs observed
		obs.brief = item.Brief
		if err := f.db.QueryRow(`SELECT status FROM runs WHERE id=?`, item.RunID).Scan(&obs.runStatus); err != nil {
			obs.runStatus = "query: " + err.Error()
		}
		seen <- obs
		res := Result{Summary: "stub did it"}
		done <- res
		return res, nil
	}}

	a := New(f.config(exec))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errc := startAgent(a, ctx)

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("agent never executed the item")
	}
	obs := <-seen
	if obs.runStatus != "running" {
		t.Fatalf("run status at execution = %q, want running (Start must precede the executor)", obs.runStatus)
	}
	if obs.brief != "b" {
		t.Fatalf("brief at execution = %q, want %q", obs.brief, "b")
	}

	// The agent reports AFTER the executor returns, so wait for the board to
	// reflect it. A fixed sleep here would be a flake generator.
	waitFor(t, "item done", func() bool { return f.itemStatus(t) == "done" })
	cancel()
	waitAgent(t, errc, context.Canceled)

	var status string
	var summary sql.NullString
	if err := f.db.QueryRow(`SELECT status, result_summary FROM work_items WHERE id=?`, f.itemID).
		Scan(&status, &summary); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "done" {
		t.Fatalf("item status = %q, want done", status)
	}
	if !summary.Valid || summary.String != "stub did it" {
		t.Fatalf("result_summary = %v, want %q", summary, "stub did it")
	}
	if got := f.runStatus(t); got != "ok" {
		t.Fatalf("run status = %q, want ok", got)
	}

	// The heartbeat carried the protocol version. A node whose heartbeat leaves
	// protocol_version at zero is marked incompatible and never claims anything.
	var nodeStatus string
	var protocol int
	if err := f.db.QueryRow(`SELECT status, protocol_version FROM nodes WHERE id=?`, f.nodeID).
		Scan(&nodeStatus, &protocol); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if nodeStatus != "online" || protocol != 1 {
		t.Fatalf("node = (%q, protocol %d), want (online, 1)", nodeStatus, protocol)
	}
}

// TestAgentReportsExecutorFailure pins the other terminal state: an executor
// error is reported as an error run and a failed item, not silently dropped.
func TestAgentReportsExecutorFailure(t *testing.T) {
	f := newFixture(t)
	exec := StubExecutor{OnRun: func(item board.ClaimedItem) (Result, error) {
		return Result{Summary: "partial"}, errors.New("model exploded")
	}}

	a := New(f.config(exec))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errc := startAgent(a, ctx)

	waitFor(t, "item failed", func() bool { return f.itemStatus(t) == "failed" })
	cancel()
	waitAgent(t, errc, context.Canceled)

	if got := f.runStatus(t); got != "error" {
		t.Fatalf("run status = %q, want error", got)
	}
	var runErr sql.NullString
	if err := f.db.QueryRow(`SELECT error FROM runs WHERE work_item_id=? ORDER BY attempt DESC LIMIT 1`, f.itemID).
		Scan(&runErr); err != nil {
		t.Fatalf("read run error: %v", err)
	}
	if !runErr.Valid || runErr.String != "model exploded" {
		t.Fatalf("run error = %v, want %q", runErr, "model exploded")
	}
}

// TestAgentRetriesATransientReportFailure pins the retry the board's
// idempotency makes safe: a report lost in transit must not discard a run that
// actually completed, or the reaper would requeue the item and charge the
// attempt for work that succeeded.
func TestAgentRetriesATransientReportFailure(t *testing.T) {
	var reports atomic.Int32
	const failFirst = 2
	f := newFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/report") && reports.Add(1) <= failFirst {
				board.WriteError(w, http.StatusInternalServerError, "report", "transient blip")
				return
			}
			next.ServeHTTP(w, r)
		})
	})

	exec := StubExecutor{OnRun: func(item board.ClaimedItem) (Result, error) {
		return Result{Summary: "made it"}, nil
	}}
	a := New(f.config(exec))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errc := startAgent(a, ctx)

	waitFor(t, "item done after the retry", func() bool { return f.itemStatus(t) == "done" })
	cancel()
	waitAgent(t, errc, context.Canceled)

	if got := reports.Load(); got != failFirst+1 {
		t.Fatalf("report calls = %d, want %d (two failures, then success)", got, failFirst+1)
	}
	if got := f.runStatus(t); got != "ok" {
		t.Fatalf("run status = %q, want ok", got)
	}
	var summary sql.NullString
	if err := f.db.QueryRow(`SELECT result_summary FROM work_items WHERE id=?`, f.itemID).Scan(&summary); err != nil {
		t.Fatalf("read item summary: %v", err)
	}
	if !summary.Valid || summary.String != "made it" {
		t.Fatalf("result_summary = %v, want %q", summary, "made it")
	}
}

// TestAgentGivesUpAfterBoundedReportRetries pins the other half: the retry is
// bounded, so a board that refuses every report is not answered with a retry
// loop. The run is left non-terminal for the reaper, which is the designed
// outcome of a report that cannot be delivered.
func TestAgentGivesUpAfterBoundedReportRetries(t *testing.T) {
	var reports atomic.Int32
	f := newFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/report") {
				reports.Add(1)
				board.WriteError(w, http.StatusInternalServerError, "report", "still broken")
				return
			}
			next.ServeHTTP(w, r)
		})
	})

	exec := StubExecutor{OnRun: func(item board.ClaimedItem) (Result, error) {
		return Result{Summary: "made it"}, nil
	}}
	a := New(f.config(exec))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errc := startAgent(a, ctx)

	waitFor(t, "the bounded report attempts", func() bool { return reports.Load() >= reportAttempts })
	// A further attempt would mean the retry is not bounded; the next backoff
	// would expire well inside this window.
	time.Sleep(500 * time.Millisecond)
	if got := reports.Load(); got != reportAttempts {
		t.Fatalf("report calls = %d, want %d (bounded)", got, reportAttempts)
	}
	if got := f.itemStatus(t); got != "running" {
		t.Fatalf("item status = %q, want running (left for the reaper)", got)
	}
	cancel()
	waitAgent(t, errc, context.Canceled)
}

// TestCancelledRunIsLeftForTheReaper pins the shutdown story: cancelling
// mid-execution must not report, because a report would either be rejected or
// mark interrupted work failed. The run stays non-terminal, the lease expires,
// and the reaper requeues the item for a second claim.
func TestCancelledRunIsLeftForTheReaper(t *testing.T) {
	f := newFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{})
	var once sync.Once
	exec := StubExecutor{OnRun: func(item board.ClaimedItem) (Result, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return Result{}, ctx.Err()
	}}

	a := New(f.config(exec))
	errc := startAgent(a, ctx)

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("agent never started executing the item")
	}
	cancel()
	waitAgent(t, errc, context.Canceled)

	// No report was attempted: the item and its started run are still live, so
	// the reaper — not the shutting-down agent — settles them.
	if got := f.itemStatus(t); got != "running" {
		t.Fatalf("item status after cancel = %q, want running", got)
	}
	if got := f.runStatus(t); got != "running" {
		t.Fatalf("run status after cancel = %q, want running", got)
	}

	// The node is gone mid-run. Past the lease the reaper marks the run lost
	// and requeues the item, charging the attempt that actually started.
	f.clock.Advance(301 * time.Second)
	res, err := board.Reap(context.Background(), f.db, f.clock)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if res.Lost != 1 {
		t.Fatalf("reap = %+v, want one lost run", res)
	}
	if got := f.itemStatus(t); got != "queued" {
		t.Fatalf("item status after reap = %q, want queued", got)
	}
	if got := f.runStatus(t); got != "lost" {
		t.Fatalf("run status after reap = %q, want lost", got)
	}
	var claimedBy sql.NullString
	var attempt int
	if err := f.db.QueryRow(`SELECT claimed_by, attempt FROM work_items WHERE id=?`, f.itemID).
		Scan(&claimedBy, &attempt); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if claimedBy.Valid || attempt != 1 {
		t.Fatalf("item = (claimed_by %v, attempt %d), want (unclaimed, 1)", claimedBy, attempt)
	}

	// A second claim succeeds: the item is recoverable, not stuck behind the
	// dead run.
	got, err := board.Claim(context.Background(), f.db, f.clock, f.nodeID, 1)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(got) != 1 || got[0].Item.ID != f.itemID {
		t.Fatalf("reclaim = %+v, want the requeued item", got)
	}
	if got[0].Item.Attempt != 2 {
		t.Fatalf("reclaimed attempt = %d, want 2", got[0].Item.Attempt)
	}
	if got[0].Brief != "b" {
		t.Fatalf("reclaimed brief = %q, want %q", got[0].Brief, "b")
	}
}

// TestHeartbeatsContinueWhileExecuting pins the reason the heartbeat has its
// own goroutine: a run longer than the 300s lease must not be reaped while it
// is still executing. board.Heartbeat extends this node's leases (pinned by the
// board's TestReapSkipsAnItemWhoseLeaseWasRenewed), so the agent's side of that
// property is simply "keep beating while the executor blocks" — which a single
// loop that only beat between claims would not do.
func TestHeartbeatsContinueWhileExecuting(t *testing.T) {
	var heartbeats atomic.Int32
	f := newFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/nodes/heartbeat" {
				heartbeats.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})

	release := make(chan struct{})
	exec := StubExecutor{OnRun: func(item board.ClaimedItem) (Result, error) {
		<-release
		return Result{Summary: "released"}, nil
	}}

	cfg := f.config(exec)
	cfg.Heartbeat = 10 * time.Millisecond
	cfg.Poll = time.Hour // only the executor unblocks this test
	a := New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errc := startAgent(a, ctx)

	// The loop is blocked inside the executor; heartbeats must keep flowing.
	waitFor(t, "heartbeats during execution", func() bool { return heartbeats.Load() >= 3 })
	close(release)
	waitFor(t, "item done", func() bool { return f.itemStatus(t) == "done" })
	cancel()
	waitAgent(t, errc, context.Canceled)
}

// TestRefusedClaimDoesNotSpin pins the failure policy: a board that refuses the
// claim is logged and retried only after the poll interval. Logging and
// immediately retrying would be a hot loop against a board that is already
// unhappy.
func TestRefusedClaimDoesNotSpin(t *testing.T) {
	var claims atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/nodes/heartbeat":
			board.WriteJSON(w, http.StatusOK, board.HeartbeatResponse{Status: "online"})
		case "/claim":
			claims.Add(1)
			board.WriteError(w, http.StatusInternalServerError, "claim", "board is refusing")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := New(Config{
		BaseURL:  srv.URL,
		Token:    "token",
		NodeID:   "node",
		Executor: StubExecutor{},
		Protocol: 1,
		// Heartbeat far in the future so only Poll decides the next claim.
		Heartbeat: time.Hour,
		Poll:      time.Minute,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := startAgent(a, ctx)

	waitFor(t, "the first refused claim", func() bool { return claims.Load() >= 1 })
	time.Sleep(100 * time.Millisecond) // far below Poll
	if n := claims.Load(); n != 1 {
		t.Fatalf("claim called %d times in 100ms with Poll=1m; a refused claim must wait, not spin", n)
	}

	cancel()
	waitAgent(t, errc, context.Canceled)
}
