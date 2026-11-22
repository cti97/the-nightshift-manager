package board

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cti97/the-nightshift-manager/internal/provider"
)

// filingFixture gives a live run: a node that can do `agentic` and `shell`, an item, and the claim
// that started it. The run id is what an agent files against, so every test below needs one.
func filingFixture(t *testing.T) (db *sql.DB, clock *FixedClock, node, itemID, runID string) {
	t.Helper()
	db, clock = newTestDB(t)
	node = SeedNodeForTest(t, db, clock, "worker", "executor", "agentic", "shell")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	itemID, err := CreateItem(context.Background(), db, clock, CreateItemRequest{
		Title: "parent", Brief: "b", Needs: []string{"agentic"},
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	got, err := Claim(context.Background(), db, clock, node, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("Claim: %d items, err %v", len(got), err)
	}
	return db, clock, node, itemID, got[0].RunID
}

func fileReq(title string) FileItemRequest {
	return FileItemRequest{Title: title, Brief: "do the thing", Needs: []string{"agentic"}}
}

// TestAnAgentMayFileAChildOfItsOwnRun is the feature: the child is a real row, placed by capability,
// with the parent's depth and the run that asked for it.
func TestAnAgentMayFileAChildOfItsOwnRun(t *testing.T) {
	db, clock, node, parentID, runID := filingFixture(t)
	res, err := FileItem(context.Background(), db, clock, runID, node, fileReq("child"))
	if err != nil {
		t.Fatalf("FileItem: %v", err)
	}
	if !res.Created || res.ItemID == "" || res.Parked {
		t.Fatalf("result = %+v, want a created item", res)
	}
	var gotParent, gotCreatedBy string
	var depth int
	if err := db.QueryRow(`SELECT COALESCE(parent_id,''), created_by, depth FROM work_items WHERE id=?`,
		res.ItemID).Scan(&gotParent, &gotCreatedBy, &depth); err != nil {
		t.Fatalf("read child: %v", err)
	}
	if gotParent != parentID {
		t.Fatalf("parent = %q, want %q", gotParent, parentID)
	}
	if gotCreatedBy != "agent:"+node {
		t.Fatalf("created_by = %q, want agent:%s", gotCreatedBy, node)
	}
	if depth != 1 {
		t.Fatalf("depth = %d, want 1", depth)
	}
	// The child is queued and ordinary: nothing else about the board changes for it.
	var status string
	if err := db.QueryRow(`SELECT status FROM work_items WHERE id=?`, res.ItemID).Scan(&status); err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != "queued" {
		t.Fatalf("status = %q, want queued", status)
	}
}

// TestAChildCannotWantWhatItsNodeCannotDo: without this an agent with a shell mints work for a more
// privileged box, and placement would happily deliver it.
func TestAChildCannotWantWhatItsNodeCannotDo(t *testing.T) {
	db, clock, node, _, runID := filingFixture(t)
	req := fileReq("wants a sandbox")
	req.Needs = []string{"sandbox:seatbelt"}
	_, err := FileItem(context.Background(), db, clock, runID, node, req)
	if err == nil {
		t.Fatal("a child needing a capability the node lacks was filed")
	}
	if !strings.Contains(err.Error(), "sandbox:seatbelt") {
		t.Fatalf("error does not name the capability: %v", err)
	}
	if n := countRowsQuiet(t, db, `SELECT COUNT(*) FROM work_items WHERE title='wants a sandbox'`); n != 0 {
		t.Fatalf("a refused filing left %d rows", n)
	}
}

// TestAFilingForAnotherNodesRunIsRefused: the run's node is the only identity the board accepts.
func TestAFilingForAnotherNodesRunIsRefused(t *testing.T) {
	db, clock, _, _, runID := filingFixture(t)
	other := SeedNodeForTest(t, db, clock, "intruder", "executor", "agentic")
	if _, err := FileItem(context.Background(), db, clock, runID, other, fileReq("stolen")); err == nil {
		t.Fatal("another node filed against a run it does not own")
	} else if !strings.Contains(err.Error(), "another node") {
		t.Fatalf("error = %v", err)
	}
}

// TestFilingAfterTheRunEndedIsRefused: a finished run is not a licence to keep filing.
func TestFilingAfterTheRunEndedIsRefused(t *testing.T) {
	db, clock, node, itemID, runID := filingFixture(t)
	if _, err := Report(context.Background(), db, clock, runID, node, ReportRequest{
		Status: "ok", Summary: "finished",
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if _, err := FileItem(context.Background(), db, clock, runID, node, fileReq("late")); err == nil {
		t.Fatal("a finished run filed an item")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items WHERE parent_id=?`, itemID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("a refused filing left %d children", n)
	}
}

// TestARetriedFilingReturnsTheSameItem: agents retry on network errors, and a duplicate child is work
// spent twice.
func TestARetriedFilingReturnsTheSameItem(t *testing.T) {
	db, clock, node, _, runID := filingFixture(t)
	req := fileReq("idempotent")
	req.ClientRef = "ref-1"
	first, err := FileItem(context.Background(), db, clock, runID, node, req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := FileItem(context.Background(), db, clock, runID, node, req)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.ItemID != first.ItemID || second.Created {
		t.Fatalf("second = %+v, want the first item and created=false", second)
	}
	if n := countRowsQuiet(t, db, `SELECT COUNT(*) FROM work_items WHERE client_ref='ref-1'`); n != 1 {
		t.Fatalf("%d items carry the ref, want 1", n)
	}
}

// TestAClientRefCannotBeReusedByAnotherRun: otherwise one run's retry silently returns another's item.
func TestAClientRefCannotBeReusedByAnotherRun(t *testing.T) {
	db, clock, node, _, runID := filingFixture(t)
	req := fileReq("first run's child")
	req.ClientRef = "shared-ref"
	if _, err := FileItem(context.Background(), db, clock, runID, node, req); err != nil {
		t.Fatalf("first: %v", err)
	}
	// A second live run on the same node.
	if _, err := CreateItem(context.Background(), db, clock, CreateItemRequest{
		Title: "second parent", Brief: "b", Needs: []string{"agentic"},
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	got, err := Claim(context.Background(), db, clock, node, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("Claim: %d, %v", len(got), err)
	}
	second := fileReq("second run's child")
	second.ClientRef = "shared-ref"
	if _, err := FileItem(context.Background(), db, clock, got[0].RunID, node, second); err == nil {
		t.Fatal("a second run reused another run's client_ref")
	}
}

// TestTheSixthChildParksTheRunAsksForAHuman: the limit is where the human is asked, not where the
// agent is silenced. Five go through; the sixth parks the parent with a request naming the filing.
func TestTheSixthChildParksTheRunAsksForAHuman(t *testing.T) {
	db, clock, node, parentID, runID := filingFixture(t)
	for i := 0; i < maxFilingsPerRun; i++ {
		req := fileReq("child")
		req.ClientRef = "ref-" + string(rune('a'+i))
		if res, err := FileItem(context.Background(), db, clock, runID, node, req); err != nil || !res.Created {
			t.Fatalf("filing %d: %+v, %v", i, res, err)
		}
	}
	res, err := FileItem(context.Background(), db, clock, runID, node, fileReq("sixth"))
	if err != nil {
		t.Fatalf("sixth: %v", err)
	}
	if !res.Parked || res.Created {
		t.Fatalf("sixth = %+v, want parked and not created", res)
	}
	var status, request string
	if err := db.QueryRow(`SELECT status, COALESCE(approval_request,'') FROM work_items WHERE id=?`,
		parentID).Scan(&status, &request); err != nil {
		t.Fatalf("read parent: %v", err)
	}
	if status != "needs_approval" {
		t.Fatalf("parent status = %q, want needs_approval", status)
	}
	if !strings.Contains(request, filingApprovalAction) {
		t.Fatalf("the request does not name the action: %s", request)
	}
	if n := countRowsQuiet(t, db, `SELECT COUNT(*) FROM work_items WHERE title='sixth'`); n != 0 {
		t.Fatalf("a parked filing still wrote %d rows", n)
	}
}

// TestADepthThreeChildParksInsteadOfWriting: depth is stored on the item and read from there, so a
// chain cannot be laundered by a retry.
func TestADepthThreeChildParksInsteadOfWriting(t *testing.T) {
	db, clock, node, _, runID := filingFixture(t)
	if _, err := db.Exec(`UPDATE work_items SET depth=2 WHERE id=(SELECT work_item_id FROM runs WHERE id=?)`, runID); err != nil {
		t.Fatalf("set depth: %v", err)
	}
	res, err := FileItem(context.Background(), db, clock, runID, node, fileReq("too deep"))
	if err != nil {
		t.Fatalf("FileItem: %v", err)
	}
	if !res.Parked {
		t.Fatalf("result = %+v, want parked", res)
	}
	if !strings.Contains(res.Reason, "deep") {
		t.Fatalf("reason = %q", res.Reason)
	}
}

// countRowsQuiet is for assertions after a refusal, where a query error should fail the test rather
// than be swallowed by a helper that had to return a count.
func countRowsQuiet(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// TestAFilingGrantSurvivesTheClaimAndIsSpentByTheFiling: the operator's "one more" must not be spent
// on the act of resuming. A normal grant dies at claim time (M3 D3); a filing grant must not.
func TestAFilingGrantSurvivesTheClaimAndIsSpentByTheFiling(t *testing.T) {
	db, clock, node, parentID, runID := filingFixture(t)
	for i := 0; i < maxFilingsPerRun; i++ {
		req := fileReq("child")
		req.ClientRef = "ref-" + string(rune('a'+i))
		if _, err := FileItem(context.Background(), db, clock, runID, node, req); err != nil {
			t.Fatalf("filing %d: %v", i, err)
		}
	}
	if res, err := FileItem(context.Background(), db, clock, runID, node, fileReq("sixth")); err != nil || !res.Parked {
		t.Fatalf("the sixth filing did not park: %+v, %v", res, err)
	}
	if _, err := Decide(context.Background(), db, clock, parentID, Approve, "one more is fine"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// The parent is queued again; the next claim must NOT eat the grant.
	got, err := Claim(context.Background(), db, clock, node, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("re-claim: %d, %v", len(got), err)
	}
	var grant sql.NullString
	if err := db.QueryRow(`SELECT approval_granted FROM work_items WHERE id=?`, parentID).Scan(&grant); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if !grant.Valid {
		t.Fatal("the claim spent the filing grant")
	}
	// And the filing it was granted for goes through exactly once.
	res, err := FileItem(context.Background(), db, clock, got[0].RunID, node, fileReq("seventh"))
	if err != nil || !res.Created {
		t.Fatalf("the granted filing was refused: %+v, %v", res, err)
	}
	if err := db.QueryRow(`SELECT approval_granted FROM work_items WHERE id=?`, parentID).Scan(&grant); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if grant.Valid {
		t.Fatal("the grant survived the filing it authorised")
	}
	// The next one is over the cap again.
	again, err := FileItem(context.Background(), db, clock, got[0].RunID, node, fileReq("eighth"))
	if err != nil || !again.Parked {
		t.Fatalf("the cap stopped applying after the grant: %+v, %v", again, err)
	}
}

// TestADeniedFilingRequeuesTheRunInsteadOfCancellingIt: the refusal is about one request the work
// made, not about the work.
func TestADeniedFilingRequeuesTheRunInsteadOfCancellingIt(t *testing.T) {
	db, clock, node, parentID, runID := filingFixture(t)
	for i := 0; i < maxFilingsPerRun; i++ {
		req := fileReq("child")
		req.ClientRef = "ref-" + string(rune('a'+i))
		if _, err := FileItem(context.Background(), db, clock, runID, node, req); err != nil {
			t.Fatalf("filing %d: %v", i, err)
		}
	}
	if res, err := FileItem(context.Background(), db, clock, runID, node, fileReq("sixth")); err != nil || !res.Parked {
		t.Fatalf("the sixth filing did not park: %+v, %v", res, err)
	}
	if _, err := Decide(context.Background(), db, clock, parentID, Deny, "five is plenty"); err != nil {
		t.Fatalf("deny: %v", err)
	}
	var status, reason string
	if err := db.QueryRow(`SELECT status, COALESCE(blocked_reason,'') FROM work_items WHERE id=?`,
		parentID).Scan(&status, &reason); err != nil {
		t.Fatalf("read parent: %v", err)
	}
	if status != "queued" {
		t.Fatalf("status = %q, want queued (a denied filing must not cancel the work)", status)
	}
	if !strings.Contains(reason, "five is plenty") {
		t.Fatalf("the operator's note is not recorded: %q", reason)
	}
	// With the note in place, the run is claimable again and still capped.
	got, err := Claim(context.Background(), db, clock, node, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("re-claim: %d, %v", len(got), err)
	}
	if res, err := FileItem(context.Background(), db, clock, got[0].RunID, node, fileReq("ninth")); err != nil || !res.Parked {
		t.Fatalf("the cap stopped applying after a denial: %+v, %v", res, err)
	}
}

// TestAnOperatorsChildIsNotDepthZero: depth is where an item sits, not who filed it. An admin-created
// child at depth 0 would let the agents working on it file two more levels under a limit of two.
func TestAnOperatorsChildIsNotDepthZero(t *testing.T) {
	db, clock := newTestDB(t)
	parent, err := CreateItem(context.Background(), db, clock, CreateItemRequest{Title: "root", Brief: "b"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	child, err := CreateItem(context.Background(), db, clock, CreateItemRequest{
		Title: "operator's child", Brief: "b", ParentID: parent,
	})
	if err != nil {
		t.Fatalf("CreateItem child: %v", err)
	}
	var depth int
	if err := db.QueryRow(`SELECT depth FROM work_items WHERE id=?`, child).Scan(&depth); err != nil {
		t.Fatalf("read depth: %v", err)
	}
	if depth != 1 {
		t.Fatalf("depth = %d, want 1", depth)
	}
	if _, err := CreateItem(context.Background(), db, clock, CreateItemRequest{
		Title: "orphan", Brief: "b", ParentID: "does-not-exist",
	}); err == nil {
		t.Fatal("an item was filed under a parent that does not exist")
	}
}

// TestTwoFilingsCannotBothPassTheCap: the count and the insert are one transaction, so a race cannot
// produce a sixth child. Written as a race because that is the only way the guarantee is visible.
func TestTwoFilingsCannotBothPassTheCap(t *testing.T) {
	db, clock, node, parentID, runID := filingFixture(t)
	for i := 0; i < maxFilingsPerRun-1; i++ {
		req := fileReq("child")
		req.ClientRef = "ref-" + string(rune('a'+i))
		if _, err := FileItem(context.Background(), db, clock, runID, node, req); err != nil {
			t.Fatalf("seed filing %d: %v", i, err)
		}
	}
	// One slot left. Two filings at once: exactly one may create, and the other must be refused or
	// park — never a second insert.
	var wg sync.WaitGroup
	results := make([]FileItemResult, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := fileReq("racing")
			req.ClientRef = "race-" + string(rune('a'+i))
			results[i], errs[i] = FileItem(context.Background(), db, clock, runID, node, req)
		}(i)
	}
	wg.Wait()
	var created int
	for _, r := range results {
		if r.Created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("%d filings created an item, want exactly 1 (%+v, %v)", created, results, errs)
	}
	if n := countRowsQuiet(t, db, `SELECT COUNT(*) FROM work_items WHERE parent_id='`+parentID+`'`); n != maxFilingsPerRun {
		t.Fatalf("%d children, want %d", n, maxFilingsPerRun)
	}
}

// TestTheFilingRouteCodesAreWhatAClientBranchesOn: 201 for a created item, 202 when the run parked
// instead, 409 when the run is not the caller's to file from, 400 when the request is refused. The
// codes are the contract: 201 would tell a client an item exists while the board waits for a human.
func TestTheFilingRouteCodesAreWhatAClientBranchesOn(t *testing.T) {
	db, clock := newTestDB(t)
	srv := NewServer(db, clock, provider.NewRegistry(), HashToken(testAdminToken), HashToken(testEnrollToken))
	ctx := context.Background()
	nodeID, token, err := RegisterNode(ctx, db, clock, RegisterRequest{
		Name: "a", Role: "executor", ProvidesDeclared: []string{"agentic"},
		ReachClasses: []string{"host", "lan", "wan"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if _, err := Heartbeat(ctx, db, clock, nodeID, HeartbeatRequest{
		ProvidesLive: []string{"agentic"}, ReachClasses: []string{"host", "lan", "wan"}, Protocol: ProtocolMax,
	}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	if _, err := CreateItem(ctx, db, clock, CreateItemRequest{
		Title: "parent", Brief: "b", Needs: []string{"agentic"},
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	claimed, err := Claim(ctx, db, clock, nodeID, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim = %d, err %v", len(claimed), err)
	}
	runID := claimed[0].RunID

	post := func(bearer, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/runs/"+runID+"/items", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	if rec := post(testAdminToken, `{"title":"x","brief":"y"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin token = %d, want 401: filing is a run's own act", rec.Code)
	}
	if rec := post(token, `{"title":"","brief":"y"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty title = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	if rec := post(token, `{"title":"kid","brief":"y","needs":["sandbox:seatbelt"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a capability the node lacks = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	for i := 0; i < maxFilingsPerRun; i++ {
		body := fmt.Sprintf(`{"title":"kid %d","brief":"y","client_ref":"r%d"}`, i, i)
		if rec := post(token, body); rec.Code != http.StatusCreated {
			t.Fatalf("filing %d = %d, want 201 (body %s)", i, rec.Code, rec.Body)
		}
	}
	rec := post(token, `{"title":"one too many","brief":"y"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the filing over the cap = %d, want 202 (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"parked":true`) {
		t.Fatalf("the 202 does not say the run parked: %s", rec.Body)
	}
	// The run is parked now, so a further filing is refused, not parked twice.
	if rec := post(token, `{"title":"after the park","brief":"y"}`); rec.Code != http.StatusConflict {
		t.Fatalf("filing from a parked run = %d, want 409 (body %s)", rec.Code, rec.Body)
	}
	// A different node cannot file from this run at all.
	_, otherToken, err := RegisterNode(ctx, db, clock, RegisterRequest{
		Name: "b", Role: "executor", ProvidesDeclared: []string{"agentic"},
		ReachClasses: []string{"host", "lan", "wan"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("RegisterNode b: %v", err)
	}
	if rec := post(otherToken, `{"title":"stolen","brief":"y"}`); rec.Code != http.StatusConflict {
		t.Fatalf("another node = %d, want 409 (body %s)", rec.Code, rec.Body)
	}
}

// TestFilingBoundsItsInput: the ref becomes an index key and the needs list is parsed and stored, so
// an unbounded one is a filing that can slow every later one.
func TestFilingBoundsItsInput(t *testing.T) {
	db, clock, node, _, runID := filingFixture(t)
	tooMany := make([]string, maxNeedsPerFiling+1)
	for i := range tooMany {
		tooMany[i] = "agentic"
	}
	longRef := FileItemRequest{Title: "t", Brief: "b", ClientRef: strings.Repeat("r", maxClientRefBytes+1)}
	if _, err := FileItem(context.Background(), db, clock, runID, node, longRef); err == nil {
		t.Fatal("an overlong client_ref was accepted")
	}
	manyNeeds := FileItemRequest{Title: "t", Brief: "b", Needs: tooMany}
	if _, err := FileItem(context.Background(), db, clock, runID, node, manyNeeds); err == nil {
		t.Fatal("an overlong needs list was accepted")
	}
	if n := countRowsQuiet(t, db, `SELECT COUNT(*) FROM work_items WHERE parent_id IS NOT NULL`); n != 0 {
		t.Fatalf("a refused filing left %d children", n)
	}
}
