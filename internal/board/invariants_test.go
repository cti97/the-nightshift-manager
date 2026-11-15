// internal/board/invariants_test.go
package board

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The randomized invariants design §17.2 and §17.5 have owed since M1.
//
// Both loops are seeded and take no wall-clock or scheduling input, so a failure
// is reproducible: every failure message prints the seed, the iteration and the
// ids that violated the invariant, and re-running the same test replays the same
// sequence. 200 iterations of 200-odd SQLite statements finish in well under a
// second, which is what keeps the test in the default suite instead of behind a
// flag that nobody runs.
const (
	liveRunSeed    = int64(2026111501)
	noEgressSeed   = int64(2026111502)
	invariantIters = 200
)

// invariantNeedSets covers the placement rules the loops can exercise: plain
// capabilities, reach policy (no_egress, strict_local) and a token floor. A nil
// set is a legitimate item too, and it is the one that places most easily.
var invariantNeedSets = [][]string{
	nil,
	{"agentic"},
	{"reflex"},
	{"vision"},
	{"shell"},
	{"gpu"},
	{"tokens:50000"},
	{"lan:ollama"},
	{"agentic", "no_egress"},
	{"reflex", "strict_local"},
	{"shell", "no_egress"},
	{"agentic", "tokens:50000"},
}

// TestRandomizedClaimExpireReapNeverLeavesTwoLiveRuns is design §17.2: after
// randomized claim / expire / reap traffic, no work item may have two
// non-terminal runs.
//
// The loop runs two claimants at a time on two separate *sql.DB handles on some
// iterations. That is not decoration: Open caps a pool at one connection, so
// against a single handle the claims serialize inside database/sql and the
// claimed_by IS NULL guard could be deleted with a sequential test still green.
// Two handles give two real writers, which is what two nodes asking at once are.
// The claim's own atomicity is then tested where it can fail.
func TestRandomizedClaimExpireReapNeverLeavesTwoLiveRuns(t *testing.T) {
	const seed = liveRunSeed
	t.Logf("randomized claim/expire/reap seed: %d, iterations: %d", seed, invariantIters)

	path := filepath.Join(t.TempDir(), "board.db")
	db, clock := OpenForTest(t, path)

	// A second connection pool on the same file: two real writers, not two
	// goroutines serialized by one pool.
	other, err := Open(path)
	if err != nil {
		t.Fatalf("seed %d: open a second handle on %s: %v", seed, path, err)
	}
	t.Cleanup(func() { other.Close() })

	ctx := context.Background()
	executorProvides := []string{
		"agentic", "reflex", "shell", "network", "vision", "gpu",
		"tokens:100000", "lan:ollama", "no_egress", "strict_local",
	}
	nodeIDs := []string{
		SeedNodeForTest(t, db, clock, "a", "executor", executorProvides...),
		SeedNodeForTest(t, db, clock, "b", "executor", executorProvides...),
		SeedNodeForTest(t, db, clock, "s", "steward", "reflex", "shell"),
	}
	nodeProvides := [][]string{executorProvides, executorProvides, {"reflex", "shell"}}
	SeedEndpointForTest(t, db, clock, "localhost", "host", 0, "tools", "vision")
	SeedEndpointForTest(t, db, clock, "lanbox", "lan", 0.5, "tools")
	// A wan endpoint that is also the cheapest reach: no_egress items must not
	// follow cost onto it, which gives invariant (b) real work in this loop too.
	SeedEndpointForTest(t, db, clock, "cloud", "wan", 0, "tools", "vision")

	createItem := func(needs []string) {
		if _, err := CreateItem(ctx, db, clock, CreateItemRequest{
			Title: "random", Brief: "b", Needs: needs,
		}); err != nil {
			t.Fatalf("seed %d: CreateItem(%v): %v", seed, needs, err)
		}
	}
	for i := 0; i < 8; i++ {
		createItem(invariantNeedSets[i%len(invariantNeedSets)])
	}

	var abandoned, lost, applied, concurrentRounds int

	// act drives a freshly claimed item through the lifecycle the state machine
	// supports: leave it claimed, start it and leave it running, or settle it
	// with a report. Every branch is legitimate traffic, and the reaper has to
	// keep the invariant across all of them.
	//
	// Its decisions come from a per-(iteration, claim) stream rather than one
	// shared stream. Concurrency can change how many items a round claims; if
	// that changed how many draws act consumed, it would shift every later
	// decision and the seed would no longer replay the run.
	act := func(it, idx int, nodeID string, c ClaimedItem) {
		ar := rand.New(rand.NewSource(seed*7_000_019 + int64(it)*11 + int64(idx)))
		if ar.Intn(5) == 0 {
			return // left claimed: the reaper must abandon and refund it
		}
		if err := StartRun(ctx, db, clock, c.RunID, nodeID); err != nil {
			t.Fatalf("seed %d iteration %d: StartRun(%s): %v", seed, it, c.RunID, err)
		}
		if ar.Intn(2) == 0 {
			return // left running: the reaper must mark it lost and spend the attempt
		}
		req := ReportRequest{Status: "ok", Summary: "done"}
		if ar.Intn(4) == 0 {
			req = ReportRequest{Status: "error", Error: "stub failure"}
		}
		res, err := Report(ctx, db, clock, c.RunID, nodeID, req)
		if err != nil {
			t.Fatalf("seed %d iteration %d: Report(%s): %v", seed, it, c.RunID, err)
		}
		if res.Applied {
			applied++
		}
	}

	for it := 0; it < invariantIters; it++ {
		// One stream per iteration, and every draw happens in this fixed order
		// BEFORE any state is read. The item creation, the claim shape and the
		// reap switch are therefore identical on every replay of this seed,
		// whatever the concurrent claimants happened to win.
		r := rand.New(rand.NewSource(seed*1_000_003 + int64(it)))

		// Heartbeats renew the lease of everything this node currently holds, so
		// they have to land before the claims: after them, the advance below
		// expires exactly the work issued this iteration. Without them the
		// reaper would take the nodes offline for silence first.
		for i, id := range nodeIDs {
			if _, err := Heartbeat(ctx, db, clock, id, HeartbeatRequest{
				ProvidesLive: nodeProvides[i],
				ReachClasses: []string{"host", "lan", "wan"},
				Protocol:     ProtocolMax,
			}); err != nil {
				t.Fatalf("seed %d iteration %d: heartbeat %s: %v", seed, it, id, err)
			}
		}

		createItem(invariantNeedSets[r.Intn(len(invariantNeedSets))])
		concurrent := r.Intn(3) == 0
		aIdx, bIdx := r.Intn(2), r.Intn(2)
		slots := [2]int{1 + r.Intn(2), 1 + r.Intn(2)}
		seqIdx := r.Intn(len(nodeIDs))
		seqSlots := 1 + r.Intn(2)
		reapThisRound := r.Intn(4) != 0

		if concurrent {
			// Two claimants at once, on two handles. The free slots were chosen
			// above, so no goroutine touches the RNG.
			ids := [2]string{nodeIDs[aIdx], nodeIDs[bIdx]}
			handles := [2]*sql.DB{db, other}
			var results [2][]ClaimedItem
			var errs [2]error
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					results[i], errs[i] = Claim(ctx, handles[i], clock, ids[i], slots[i])
				}(i)
			}
			close(start)
			wg.Wait()
			for i := 0; i < 2; i++ {
				if errs[i] != nil {
					t.Fatalf("seed %d iteration %d: concurrent Claim for %s: %v", seed, it, ids[i], errs[i])
				}
				for j, c := range results[i] {
					act(it, i*10+j, ids[i], c)
				}
			}
			concurrentRounds++
		} else {
			nodeID := nodeIDs[seqIdx]
			got, err := Claim(ctx, db, clock, nodeID, seqSlots)
			if err != nil {
				t.Fatalf("seed %d iteration %d: Claim for %s: %v", seed, it, nodeID, err)
			}
			for j, c := range got {
				act(it, j, nodeID, c)
			}
		}

		// Expire and reap on most iterations. The step is one second past the
		// lease, so the claims issued above are the ones that expire; the
		// heartbeat at the top of the next iteration keeps the nodes online.
		if reapThisRound {
			clock.Advance(time.Duration(leaseSeconds+1) * time.Second)
			res, err := Reap(ctx, db, clock)
			if err != nil {
				t.Fatalf("seed %d iteration %d: Reap: %v", seed, it, err)
			}
			abandoned += res.Abandoned
			lost += res.Lost
		}

		assertAtMostOneLiveRun(t, seed, it, db)
		assertNoEgressNeverOnWan(t, seed, it, db)
	}

	// A loop that never reaped, never settled and never contended would pass
	// both invariants by doing nothing. These counters are the proof it did the
	// work the test claims to do.
	switch {
	case abandoned == 0:
		t.Fatalf("seed %d: no claim was abandoned by the reaper; the expire half of the loop never ran", seed)
	case lost == 0:
		t.Fatalf("seed %d: no started run was lost to the reaper; the expire half of the loop never ran", seed)
	case applied == 0:
		t.Fatalf("seed %d: no report was applied; the settle path never ran", seed)
	case concurrentRounds == 0:
		t.Fatalf("seed %d: no concurrent claim round ran; the guard was never contended", seed)
	}
	t.Logf("seed %d exercised: abandoned=%d lost=%d reports applied=%d concurrent rounds=%d",
		seed, abandoned, lost, applied, concurrentRounds)
}

// TestRandomizedNoEgressIsNeverClaimedAgainstWan is design §17.5: no item that
// needs no_egress may be claimed against a wan endpoint, over randomized item
// and endpoint sets.
//
// wan is made the cheapest endpoint in every iteration, so an absent guard would
// choose it for no_egress items every time: this is the property test, not a
// restatement of the cost ranking. The invariant is checked against the runs
// table as well as the returned claim, so a value that was right in the response
// and wrong in storage cannot slip through.
func TestRandomizedNoEgressIsNeverClaimedAgainstWan(t *testing.T) {
	const seed = noEgressSeed
	t.Logf("randomized no_egress/wan seed: %d, iterations: %d", seed, invariantIters)

	rng := rand.New(rand.NewSource(seed))
	db, clock := newTestDB(t)
	ctx := context.Background()

	node := SeedNodeForTest(t, db, clock, "worker", "executor",
		"agentic", "reflex", "vision", "shell", "network", "gpu",
		"tokens:100000", "no_egress", "strict_local")
	egressNeeds := [][]string{
		{"no_egress"},
		{"no_egress", "agentic"},
		{"no_egress", "vision"},
		{"no_egress", "reflex"},
	}
	otherNeeds := [][]string{
		{"agentic"},
		{"reflex"},
		{"vision"},
		{"strict_local"},
		{"shell"},
	}

	var noEgressClaims, wanClaims int

	for it := 0; it < invariantIters; it++ {
		suffix := fmt.Sprintf("%03d", it)
		// wan is always the cheapest reach, so placement is genuinely tempted;
		// host and lan stay eligible fallbacks for the no_egress items.
		SeedEndpointForTest(t, db, clock, "host"+suffix, "host", 1+float64(rng.Intn(3)), "tools", "vision")
		SeedEndpointForTest(t, db, clock, "lan"+suffix, "lan", 1+float64(rng.Intn(3)), "tools", "vision")
		SeedEndpointForTest(t, db, clock, "wan"+suffix, "wan", 0, "tools", "vision")

		// Priority makes the no_egress item the first one the claim sees, but
		// both are claimed in the same call, so both halves are exercised.
		if _, err := CreateItem(ctx, db, clock, CreateItemRequest{
			Title: "egress", Brief: "b", Needs: egressNeeds[rng.Intn(len(egressNeeds))], Priority: 10,
		}); err != nil {
			t.Fatalf("seed %d iteration %d: CreateItem(no_egress): %v", seed, it, err)
		}
		if _, err := CreateItem(ctx, db, clock, CreateItemRequest{
			Title: "open", Brief: "b", Needs: otherNeeds[rng.Intn(len(otherNeeds))],
		}); err != nil {
			t.Fatalf("seed %d iteration %d: CreateItem: %v", seed, it, err)
		}

		got, err := Claim(ctx, db, clock, node, 4)
		if err != nil {
			t.Fatalf("seed %d iteration %d: Claim: %v", seed, it, err)
		}
		for _, c := range got {
			noEgress := contains(c.Item.Needs, "no_egress")
			if noEgress && c.Endpoint.Reach == "wan" {
				t.Fatalf("seed %d iteration %d: invariant (b) violated: no_egress item %s claimed against wan endpoint %s (%s)",
					seed, it, c.Item.ID, c.Endpoint.ID, c.Endpoint.Name)
			}
			if noEgress {
				noEgressClaims++
			}
			if c.Endpoint.Reach == "wan" {
				wanClaims++
			}
		}

		assertNoEgressNeverOnWan(t, seed, it, db)

		// Retire this iteration's endpoints so the next claim's candidate
		// cross join stays small: 200 iterations of healthy endpoints would
		// otherwise make the loop quadratic and push it past the one-second
		// budget. Runs keep their endpoint id, so the invariant still reads the
		// reach they were actually placed on.
		for _, id := range []string{"ep-host" + suffix, "ep-lan" + suffix, "ep-wan" + suffix} {
			if _, err := db.ExecContext(ctx, `UPDATE endpoints SET health='down' WHERE id=?`, id); err != nil {
				t.Fatalf("seed %d iteration %d: retire endpoint %s: %v", seed, it, id, err)
			}
		}
	}

	// If the loop never placed a no_egress item, or never placed anything on
	// wan, the invariant held vacuously and the test proves nothing.
	if noEgressClaims == 0 {
		t.Fatalf("seed %d: the loop never claimed a no_egress item, so the invariant was never exercised", seed)
	}
	if wanClaims == 0 {
		t.Fatalf("seed %d: the loop never claimed anything on a wan endpoint, so 'no no_egress on wan' held vacuously", seed)
	}
	t.Logf("seed %d exercised: no_egress claims=%d, wan claims=%d", seed, noEgressClaims, wanClaims)
}

// assertAtMostOneLiveRun is invariant (a) in storage form: an item never has two
// non-terminal runs.
//
// "Non-terminal" is claimed/running only. A legitimately abandoned run is
// terminal — the reaper writes that status in the same transaction that requeues
// the item — so an abandonment can never be mistaken for a double claim.
func assertAtMostOneLiveRun(t *testing.T, seed int64, iteration int, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(
		`SELECT work_item_id, COUNT(*)
		   FROM runs
		  WHERE status IN ('claimed','running')
		  GROUP BY work_item_id
		 HAVING COUNT(*) > 1`)
	if err != nil {
		t.Fatalf("seed %d iteration %d: read live runs: %v", seed, iteration, err)
	}
	defer rows.Close()

	var bad []string
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatalf("seed %d iteration %d: scan live runs: %v", seed, iteration, err)
		}
		bad = append(bad, fmt.Sprintf("item %s has %d", id, n))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("seed %d iteration %d: read live runs: %v", seed, iteration, err)
	}
	if len(bad) > 0 {
		t.Fatalf("seed %d iteration %d: invariant (a) violated: more than one non-terminal run for %s",
			seed, iteration, strings.Join(bad, "; "))
	}
}

// assertNoEgressNeverOnWan is invariant (b) in storage form: join every run to
// the needs it was placed for and the reach it was placed on.
func assertNoEgressNeverOnWan(t *testing.T, seed int64, iteration int, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(
		`SELECT r.id, r.work_item_id, COALESCE(e.reach,''), w.needs
		   FROM runs r
		   JOIN work_items w ON w.id = r.work_item_id
		   LEFT JOIN endpoints e ON e.id = r.endpoint_id`)
	if err != nil {
		t.Fatalf("seed %d iteration %d: read runs for no_egress: %v", seed, iteration, err)
	}
	defer rows.Close()

	var bad []string
	for rows.Next() {
		var runID, itemID, reach, needsJSON string
		if err := rows.Scan(&runID, &itemID, &reach, &needsJSON); err != nil {
			t.Fatalf("seed %d iteration %d: scan run: %v", seed, iteration, err)
		}
		needs, err := decodeList(needsJSON)
		if err != nil {
			t.Fatalf("seed %d iteration %d: work item %s has a corrupt needs cell: %v", seed, iteration, itemID, err)
		}
		if contains(needs, "no_egress") && reach == "wan" {
			bad = append(bad, fmt.Sprintf("run %s of item %s (needs %s) on wan", runID, itemID, needsJSON))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("seed %d iteration %d: read runs for no_egress: %v", seed, iteration, err)
	}
	if len(bad) > 0 {
		t.Fatalf("seed %d iteration %d: invariant (b) violated: no_egress item claimed against wan: %s",
			seed, iteration, strings.Join(bad, "; "))
	}
}
