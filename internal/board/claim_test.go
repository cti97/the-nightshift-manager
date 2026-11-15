// internal/board/claim_test.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestClaimIsExactlyOnceUnderConcurrency(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	nodeA := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	nodeB := SeedNodeForTest(t, db, clock, "b", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")

	if _, err := CreateItem(ctx, db, clock, CreateItemRequest{
		Title: "only once", Brief: "b", Needs: []string{"agentic"},
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	var wg sync.WaitGroup
	results := make([][]ClaimedItem, 2)
	nodes := []string{nodeA, nodeB}
	for i := range nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := Claim(ctx, db, clock, nodes[i], 1)
			if err != nil {
				t.Errorf("Claim(%d): %v", i, err)
				return
			}
			results[i] = got
		}(i)
	}
	wg.Wait()

	total := len(results[0]) + len(results[1])
	if total != 1 {
		t.Fatalf("two claimants got %d items, want exactly 1", total)
	}

	// The returned slices could be right while the store kept two active runs.
	// These counts are the storage-level form of "no item has two runs".
	var runRows, claimedRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runRows); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items WHERE status='claimed'`).Scan(&claimedRows); err != nil {
		t.Fatalf("count claimed items: %v", err)
	}
	if runRows != 1 || claimedRows != 1 {
		t.Fatalf("stored %d runs and %d claimed items, want exactly 1 of each", runRows, claimedRows)
	}
}

func TestClaimSkipsWorkTheNodeCannotDo(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	steward := SeedNodeForTest(t, db, clock, "pi", "steward", "reflex")
	SeedEndpointForTest(t, db, clock, "local", "host", 0)

	if _, err := CreateItem(ctx, db, clock, CreateItemRequest{
		Title: "needs tools", Brief: "b", Needs: []string{"agentic"},
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	got, err := Claim(ctx, db, clock, steward, 1)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("steward claimed %d agentic items, want 0", len(got))
	}
}

func TestClaimRecordsRunAndAttempt(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})

	got, err := Claim(ctx, db, clock, node, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("Claim: got %d items, err %v", len(got), err)
	}
	if got[0].Item.ID != id {
		t.Fatalf("claimed %q, want %q", got[0].Item.ID, id)
	}
	if got[0].PlacementReason == "" {
		t.Fatal("run has no placement reason")
	}

	var attempt int
	var status string
	if err := db.QueryRow(`SELECT attempt, status FROM work_items WHERE id=?`, id).Scan(&attempt, &status); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if attempt != 1 || status != "claimed" {
		t.Fatalf("item attempt=%d status=%s, want 1/claimed", attempt, status)
	}
}

// Claim must not even consider a node that has never heartbeated (or is paused
// or incompatible). An item with no needs is placeable on such a node by every
// placement rule, so the status filter in LoadCandidates is the only guard.
func TestClaimSkipsANodeThatIsNotOnline(t *testing.T) {
	for _, status := range []string{"offline", "paused", "incompatible"} {
		t.Run(status, func(t *testing.T) {
			db, clock := newTestDB(t)
			ctx := context.Background()
			// Registered, never heartbeated: provides_live is empty and the
			// reach classes are the only thing that would make placement work.
			id, _, err := RegisterNode(ctx, db, clock, RegisterRequest{
				Name: "a", Role: "executor", ProvidesDeclared: []string{"agentic"},
				ReachClasses: []string{"host", "lan", "wan"}, Concurrency: 1,
			})
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			if _, err := db.ExecContext(ctx, `UPDATE nodes SET status=? WHERE id=?`, status, id); err != nil {
				t.Fatalf("set status: %v", err)
			}
			SeedEndpointForTest(t, db, clock, "local", "host", 0)
			if _, err := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b"}); err != nil {
				t.Fatalf("CreateItem: %v", err)
			}

			got, err := Claim(ctx, db, clock, id, 1)
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("%s node claimed %d items, want 0", status, len(got))
			}
		})
	}
}

// A claim is only useful to the reaper if the run row says what Task 9 needs:
// status 'claimed' (not running), the same attempt the item now carries, and a
// reason. The audit event is what the dashboard projects.
func TestClaimRecordsAClaimedRunAndEvent(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	const brief = "the operator's brief, which the worker must execute"
	id, err := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: brief, Needs: []string{"agentic"}})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	got, err := Claim(ctx, db, clock, node, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("Claim: got %d items, err %v", len(got), err)
	}
	if got[0].Brief != brief {
		t.Fatalf("claimed brief = %q, want %q", got[0].Brief, brief)
	}

	var (
		runStatus, runNode, reason string
		runAttempt                 int
		startedAt                  int64
		itemAttempt                int
		claimedBy                  string
		leaseUntil                 int64
	)
	if err := db.QueryRow(`SELECT status, node_id, attempt, started_at, placement_reason FROM runs WHERE id=?`,
		got[0].RunID).Scan(&runStatus, &runNode, &runAttempt, &startedAt, &reason); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if err := db.QueryRow(`SELECT attempt, claimed_by, lease_expires_at FROM work_items WHERE id=?`, id).
		Scan(&itemAttempt, &claimedBy, &leaseUntil); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if runStatus != "claimed" {
		t.Fatalf("run status = %q, want claimed", runStatus)
	}
	if runNode != node {
		t.Fatalf("run node = %q, want %q", runNode, node)
	}
	// The two counters mean different things since the reaper landed: a run's
	// attempt is its per-item claim ordinal (append-only and unique), while
	// work_items.attempt counts attempts that actually started. Both halves of
	// that invariant are pinned here; the old "run attempt == item attempt"
	// assertion is deliberately replaced rather than deleted.
	if runAttempt != 1 {
		t.Fatalf("run attempt = %d, want 1 (the first claim is ordinal 1)", runAttempt)
	}
	if want := nonAbandonedRuns(t, db, id); itemAttempt != want {
		t.Fatalf("item attempt = %d, want %d (the number of runs that were not abandoned)", itemAttempt, want)
	}
	if got := runOrdinals(t, db, id); !slices.Equal(got, []int{1}) {
		t.Fatalf("run ordinals = %v, want [1] (contiguous from 1)", got)
	}
	if startedAt != clock.Now() {
		t.Fatalf("run started_at = %d, want the injected clock's %d", startedAt, clock.Now())
	}
	if claimedBy != node {
		t.Fatalf("item claimed_by = %q, want %q", claimedBy, node)
	}
	if leaseUntil != clock.Now()+leaseSeconds {
		t.Fatalf("lease_expires_at = %d, want %d from the injected clock", leaseUntil, clock.Now()+leaseSeconds)
	}
	if reason != got[0].PlacementReason {
		t.Fatalf("stored reason = %q, returned %q", reason, got[0].PlacementReason)
	}

	events, err := ListEvents(ctx, db, "work_item", id, 10)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Type == "claimed" {
			found = true
			if !strings.Contains(e.Payload, node) {
				t.Fatalf("claimed event payload %q does not name node %q", e.Payload, node)
			}
		}
	}
	if !found {
		t.Fatalf("no 'claimed' event for item %s: %+v", id, events)
	}
}

func TestClaimAndCreateEndpointsRequireTheirOwnToken(t *testing.T) {
	srv, _ := newServer(t)
	h := srv.Handler()

	reg := postJSON(t, h, "/nodes/register", "", RegisterRequest{Name: "a", Role: "executor"})
	if reg.Code != http.StatusCreated {
		t.Fatalf("register = %d: %s", reg.Code, reg.Body)
	}
	var issued RegisterResponse
	if err := json.Unmarshal(reg.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode register response: %v", err)
	}

	for _, tc := range []struct{ path, bearer, why string }{
		{"/claim", "", "no token"},
		{"/claim", "admin-secret", "an admin token is not a node token"},
		{"/admin/items", "", "no token"},
		{"/admin/items", issued.Token, "a node token is not an admin token"},
	} {
		rec := postRaw(t, h, tc.path, tc.bearer, `{}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s with %s = %d, want 401: %s", tc.path, tc.why, rec.Code, rec.Body)
		}
		if code := errorCode(t, rec); code != "unauthorized" {
			t.Fatalf("%s error code = %q, want unauthorized", tc.path, code)
		}
	}
}

func TestCreateItemThenClaimOverHTTP(t *testing.T) {
	srv, clock := newServer(t)
	h := srv.Handler()

	reg := postJSON(t, h, "/nodes/register", "", RegisterRequest{
		Name: "a", Role: "executor", ProvidesDeclared: []string{"agentic"},
		ReachClasses: []string{"host", "lan", "wan"}, Concurrency: 1,
	})
	if reg.Code != http.StatusCreated {
		t.Fatalf("register = %d: %s", reg.Code, reg.Body)
	}
	var issued RegisterResponse
	if err := json.Unmarshal(reg.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if rec := postJSON(t, h, "/nodes/heartbeat", issued.Token, HeartbeatRequest{
		ProvidesLive: []string{"agentic"}, ReachClasses: []string{"host", "lan", "wan"}, Protocol: 1,
	}); rec.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d: %s", rec.Code, rec.Body)
	}
	SeedEndpointForTest(t, srv.db, clock, "local", "host", 0, "tools")

	const brief = "the operator's brief, which the worker must execute"
	created := postJSON(t, h, "/admin/items", "admin-secret", CreateItemRequest{
		Title: "t", Brief: brief, Needs: []string{"agentic"},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create item = %d: %s", created.Code, created.Body)
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode create response %q: %v", created.Body, err)
	}
	if body.ID == "" {
		t.Fatalf("create response has no id: %s", created.Body)
	}

	rec := postJSON(t, h, "/claim", issued.Token, ClaimRequest{FreeSlots: 1})
	if rec.Code != http.StatusOK {
		t.Fatalf("claim = %d: %s", rec.Code, rec.Body)
	}
	var claimed []ClaimedItem
	if err := json.Unmarshal(rec.Body.Bytes(), &claimed); err != nil {
		t.Fatalf("decode claim response %q: %v", rec.Body, err)
	}
	if len(claimed) != 1 || claimed[0].Item.ID != body.ID {
		t.Fatalf("claim returned %+v, want one item %s", claimed, body.ID)
	}
	if claimed[0].RunID == "" || claimed[0].PlacementReason == "" {
		t.Fatalf("claim response lacks run id or reason: %+v", claimed[0])
	}
	if claimed[0].Brief != brief {
		t.Fatalf("claim body brief = %q, want %q", claimed[0].Brief, brief)
	}

	// Nothing left to claim: the client must see [] rather than null.
	idle := postJSON(t, h, "/claim", issued.Token, ClaimRequest{FreeSlots: 1})
	if idle.Code != http.StatusOK {
		t.Fatalf("idle claim = %d: %s", idle.Code, idle.Body)
	}
	if got := strings.TrimSpace(idle.Body.String()); got != "[]" {
		t.Fatalf("idle claim body = %q, want []", got)
	}
}

func TestCreateItemAndClaimRejectMalformedJSON(t *testing.T) {
	srv, _ := newServer(t)
	h := srv.Handler()

	if rec := postRaw(t, h, "/admin/items", "admin-secret", "{"); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed create = %d, want 400: %s", rec.Code, rec.Body)
	} else if code := errorCode(t, rec); code != "bad_json" {
		t.Fatalf("create error code = %q, want bad_json", code)
	}

	rec := postJSON(t, h, "/admin/items", "admin-secret", CreateItemRequest{Title: "t"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create without a brief = %d, want 400: %s", rec.Code, rec.Body)
	}
	if code := errorCode(t, rec); code != "create_item" {
		t.Fatalf("create error code = %q, want create_item", code)
	}

	rec = postJSON(t, h, "/admin/items", "admin-secret", CreateItemRequest{
		Title: "t", Brief: "b", Needs: []string{"gpuu"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with an unknown capability = %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "gpuu") {
		t.Fatalf("create error does not name the bad capability: %s", rec.Body)
	}

	reg := postJSON(t, h, "/nodes/register", "", RegisterRequest{Name: "a", Role: "executor"})
	var issued RegisterResponse
	if err := json.Unmarshal(reg.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if rec := postRaw(t, h, "/claim", issued.Token, "{"); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed claim = %d, want 400: %s", rec.Code, rec.Body)
	} else if code := errorCode(t, rec); code != "bad_json" {
		t.Fatalf("claim error code = %q, want bad_json", code)
	}
}

// A requeued item must come back at the next attempt with a new run, not a
// reused one. The requeue is driven through the reaper rather than hand-written
// SQL: a hardcoded copy of the requeue is what let this test drift from the
// reaper once already.
func TestClaimReclaimAdvancesTheAttemptAndStartsANewRun(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, err := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	first, err := Claim(ctx, db, clock, node, 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim: got %d items, err %v", len(first), err)
	}
	if first[0].Item.Attempt != 1 {
		t.Fatalf("first claim attempt = %d, want 1", first[0].Item.Attempt)
	}

	// This claim starts and then dies: the attempt is spent, so the reaper
	// requeues the item with the attempt kept.
	if err := StartRun(ctx, db, clock, first[0].RunID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	stepPastLease(clock)
	if res, err := Reap(ctx, db, clock); err != nil {
		t.Fatalf("Reap: %v", err)
	} else if res.Lost != 1 {
		t.Fatalf("Reap = %+v, want one lost run", res)
	}

	second, err := Claim(ctx, db, clock, node, 1)
	if err != nil || len(second) != 1 {
		t.Fatalf("second claim: got %d items, err %v", len(second), err)
	}
	if second[0].Item.ID != id {
		t.Fatalf("second claim got item %q, want %q", second[0].Item.ID, id)
	}
	if second[0].Item.Attempt != 2 {
		t.Fatalf("second claim attempt = %d, want 2 (a started run keeps its attempt)", second[0].Item.Attempt)
	}
	if second[0].RunID == first[0].RunID {
		t.Fatalf("second claim reused run id %q", first[0].RunID)
	}

	// work_items.attempt is the number of attempts actually spent, and the run
	// ordinals are a separate, contiguous, never-reused sequence.
	if got := itemAttempt(t, db, id); got != 2 {
		t.Fatalf("item attempt = %d, want 2", got)
	}
	if got, want := itemAttempt(t, db, id), nonAbandonedRuns(t, db, id); got != want {
		t.Fatalf("item attempt = %d, non-abandoned runs = %d; the attempt counter must equal what was actually spent", got, want)
	}
	if got := runOrdinals(t, db, id); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("run ordinals = %v, want [1 2]", got)
	}
	var secondRunAttempt int
	if err := db.QueryRow(`SELECT attempt FROM runs WHERE id=?`, second[0].RunID).Scan(&secondRunAttempt); err != nil {
		t.Fatalf("read second run: %v", err)
	}
	if secondRunAttempt != 2 {
		t.Fatalf("second run attempt = %d, want 2", secondRunAttempt)
	}
}

// itemAttempt is work_items.attempt: the number of attempts that actually
// started, refunded when a claim is abandoned without starting.
func itemAttempt(t *testing.T, db *sql.DB, itemID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT attempt FROM work_items WHERE id=?`, itemID).Scan(&n); err != nil {
		t.Fatalf("read item attempt: %v", err)
	}
	return n
}

// nonAbandonedRuns is the other half of the invariant itemAttempt must equal:
// the runs that were not abandoned before starting. This replaced the old
// "run attempt == item attempt" parity pin, which a refunded abandoned claim
// makes impossible to keep.
func nonAbandonedRuns(t *testing.T, db *sql.DB, itemID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM runs WHERE work_item_id=? AND status <> 'abandoned'`, itemID).Scan(&n); err != nil {
		t.Fatalf("count non-abandoned runs: %v", err)
	}
	return n
}

// runOrdinals is the item's claim ordinals in order: contiguous from 1 and
// never reused, because runs is append-only with UNIQUE(work_item_id, attempt).
func runOrdinals(t *testing.T, db *sql.DB, itemID string) []int {
	t.Helper()
	rows, err := db.Query(`SELECT attempt FROM runs WHERE work_item_id=? ORDER BY attempt`, itemID)
	if err != nil {
		t.Fatalf("list run ordinals: %v", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan run ordinal: %v", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list run ordinals: %v", err)
	}
	return out
}
