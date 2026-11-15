// internal/board/report_test.go
package board

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestReportIsIdempotent(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	got, _ := Claim(ctx, db, clock, node, 1)
	runID := got[0].RunID

	if err := StartRun(ctx, db, clock, runID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	first, err := Report(ctx, db, clock, runID, node, ReportRequest{Status: "ok", Summary: "done once"})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if !first.Applied {
		t.Fatal("first report was not applied")
	}

	// The item and its run must settle together. A run left running under a
	// done item is invisible to the reaper, which only scans claimed/running
	// items, so it would stay live forever.
	var runStatus string
	var runEnded sql.NullInt64
	if err := db.QueryRow(`SELECT status, ended_at FROM runs WHERE id=?`, runID).
		Scan(&runStatus, &runEnded); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if runStatus != "ok" || !runEnded.Valid {
		t.Fatalf("run = %q (ended=%v) after a report, want ok and closed", runStatus, runEnded.Valid)
	}

	second, err := Report(ctx, db, clock, runID, node, ReportRequest{Status: "error", Summary: "second try"})
	if err != nil {
		t.Fatalf("second Report: %v", err)
	}
	if second.Applied {
		t.Fatal("second report was applied; it must be a no-op")
	}

	var status, summary string
	if err := db.QueryRow(`SELECT status, result_summary FROM work_items WHERE id=?`, id).Scan(&status, &summary); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "done" || summary != "done once" {
		t.Fatalf("item = %q/%q, want done/done once", status, summary)
	}

	// A retried report must not add another audit event: the transaction that
	// settled the run is the only one that happened.
	events, err := ListEvents(ctx, db, "work_item", id, 50)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	reported := 0
	for _, e := range events {
		if e.Type == "reported" {
			reported++
		}
	}
	if reported != 1 {
		t.Fatalf("got %d reported events, want exactly 1 (a retry wrote one)", reported)
	}
}

func TestReportRejectsAWrongNode(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	other := SeedNodeForTest(t, db, clock, "b", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	got, _ := Claim(ctx, db, clock, node, 1)
	runID := got[0].RunID
	if err := StartRun(ctx, db, clock, runID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	if _, err := Report(ctx, db, clock, runID, other, ReportRequest{Status: "ok"}); err == nil {
		t.Fatal("a node reported a run it does not own")
	}

	// A refused report must change nothing. If the ownership check rejected the
	// report after the run had already been settled, the owner's live run would
	// be closed underneath it and the item would be finished by a node that
	// never executed it.
	var runStatus, itemStatus string
	var endedAt sql.NullInt64
	if err := db.QueryRow(`SELECT status, ended_at FROM runs WHERE id=?`, runID).
		Scan(&runStatus, &endedAt); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if runStatus != "running" || endedAt.Valid {
		t.Fatalf("run = %q (ended=%v) after a rejected report, want running and open", runStatus, endedAt.Valid)
	}
	if err := db.QueryRow(`SELECT status FROM work_items WHERE id=?`, id).Scan(&itemStatus); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if itemStatus != "running" {
		t.Fatalf("item status = %q after a rejected report, want running", itemStatus)
	}
}

// A worker that reports and dies before reading the reply may retry while the
// first report is still committing. Exactly one of the racing reports may win,
// and the item must carry the winner's summary.
func TestConcurrentReportsCompleteOnce(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	got, _ := Claim(ctx, db, clock, node, 1)
	runID := got[0].RunID
	if err := StartRun(ctx, db, clock, runID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	const racers = 5
	var wg sync.WaitGroup
	applied := make([]bool, racers)
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := Report(ctx, db, clock, runID, node,
				ReportRequest{Status: "ok", Summary: fmt.Sprintf("report %d", i)})
			applied[i], errs[i] = res.Applied, err
		}(i)
	}
	wg.Wait()

	winners, winner := 0, -1
	for i := range applied {
		if errs[i] != nil {
			t.Fatalf("concurrent report %d: %v", i, errs[i])
		}
		if applied[i] {
			winners++
			winner = i
		}
	}
	if winners != 1 {
		t.Fatalf("%d of %d concurrent reports were applied, want exactly 1", winners, racers)
	}

	var status, summary string
	if err := db.QueryRow(`SELECT status, result_summary FROM work_items WHERE id=?`, id).
		Scan(&status, &summary); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "done" || summary != fmt.Sprintf("report %d", winner) {
		t.Fatalf("item = %q/%q, want done and the winning report's summary", status, summary)
	}
}

func TestExplainSaysWhyNothingRan(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	SeedNodeForTest(t, db, clock, "pi", "steward", "reflex")
	SeedEndpointForTest(t, db, clock, "local", "host", 0)
	id, _ := CreateItem(ctx, db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"gpu"}})

	ex, err := Explain(ctx, db, clock, id)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if ex.Placed {
		t.Fatal("Explain says the item is placeable; nothing provides gpu")
	}
	if ex.BlockedReason == "" {
		t.Fatal("Explain returned no blocked_reason")
	}
	if !strings.Contains(ex.BlockedReason, "gpu") {
		t.Fatalf("blocked_reason = %q, want it to name the unsatisfiable need", ex.BlockedReason)
	}
	// The reason string names only the needs. Which rule failed for which
	// candidate lives in Rejects, and /why is useless to a human without it.
	if len(ex.Rejects) == 0 {
		t.Fatal("Explain returned no per-rule rejects; the reason alone does not say why")
	}
	if !strings.Contains(ex.Rejects[0].Why, "gpu") {
		t.Fatalf("reject = %+v, want it to name the missing capability gpu", ex.Rejects[0])
	}
}

// TestRunRoutesStartReportAndWhy exercises the three new routes through
// Handler(), so the route patterns, the node/admin auth split and the JSON
// shapes are pinned rather than assumed from the handler bodies.
func TestRunRoutesStartReportAndWhy(t *testing.T) {
	srv, clock := newServer(t)
	ctx := context.Background()
	SeedEndpointForTest(t, srv.db, clock, "local", "host", 0, "tools")
	node, token := seedNodeTokenForTest(t, srv.db, clock, "a", "executor", "agentic")
	itemID, _ := CreateItem(ctx, srv.db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	got, err := Claim(ctx, srv.db, clock, node, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("Claim: %v (got %d items)", err, len(got))
	}
	runID := got[0].RunID
	h := srv.Handler()

	rec := doJSON(t, h, "POST", "/runs/"+runID+"/start", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d, body %s", rec.Code, rec.Body.String())
	}
	var started map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode start body: %v", err)
	}
	if started["status"] != "running" {
		t.Fatalf("start body = %v, want status running", started)
	}

	rec = doJSON(t, h, "POST", "/runs/"+runID+"/report", token,
		map[string]any{"status": "ok", "summary": "done via http"})
	if rec.Code != http.StatusOK {
		t.Fatalf("report status = %d, body %s", rec.Code, rec.Body.String())
	}
	var first ReportResult
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode report body: %v", err)
	}
	if !first.Applied || first.ItemStatus != "done" {
		t.Fatalf("first report = %+v, want applied/done", first)
	}

	rec = doJSON(t, h, "POST", "/runs/"+runID+"/report", token,
		map[string]any{"status": "error", "summary": "late retry"})
	if rec.Code != http.StatusOK {
		t.Fatalf("second report status = %d, body %s", rec.Code, rec.Body.String())
	}
	var second ReportResult
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second report body: %v", err)
	}
	if second.Applied {
		t.Fatal("a retried report over HTTP was applied; it must be a no-op")
	}
	if second.ItemStatus != "done" {
		t.Fatalf("second report item status = %q, want the first report's done", second.ItemStatus)
	}

	rec = do(t, h, "GET", "/admin/why/"+itemID, "admin-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("why status = %d, body %s", rec.Code, rec.Body.String())
	}
	var ex Explanation
	if err := json.Unmarshal(rec.Body.Bytes(), &ex); err != nil {
		t.Fatalf("decode why body: %v", err)
	}
	if ex.ItemID != itemID || ex.Status != "done" {
		t.Fatalf("why = %+v, want item %s with status done", ex, itemID)
	}
}

func TestReportRouteRejectsAWrongNodeAndChangesNothing(t *testing.T) {
	srv, clock := newServer(t)
	ctx := context.Background()
	SeedEndpointForTest(t, srv.db, clock, "local", "host", 0, "tools")
	node, _ := seedNodeTokenForTest(t, srv.db, clock, "a", "executor", "agentic")
	_, otherToken := seedNodeTokenForTest(t, srv.db, clock, "b", "executor", "agentic")
	id, _ := CreateItem(ctx, srv.db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}})
	got, err := Claim(ctx, srv.db, clock, node, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("Claim: %v (got %d items)", err, len(got))
	}
	runID := got[0].RunID
	if err := StartRun(ctx, srv.db, clock, runID, node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	rec := doJSON(t, srv.Handler(), "POST", "/runs/"+runID+"/report", otherToken,
		map[string]any{"status": "ok"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong-node report status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "report")

	var runStatus, itemStatus string
	if err := srv.db.QueryRow(`SELECT status FROM runs WHERE id=?`, runID).Scan(&runStatus); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if err := srv.db.QueryRow(`SELECT status FROM work_items WHERE id=?`, id).Scan(&itemStatus); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if runStatus != "running" || itemStatus != "running" {
		t.Fatalf("run/item = %q/%q after a rejected report, want running/running", runStatus, itemStatus)
	}
}

func TestWhyRoutePrintsPerRuleRejects(t *testing.T) {
	srv, clock := newServer(t)
	ctx := context.Background()
	SeedNodeForTest(t, srv.db, clock, "pi", "steward", "reflex")
	SeedEndpointForTest(t, srv.db, clock, "local", "host", 0)
	itemID, _ := CreateItem(ctx, srv.db, clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"gpu"}})

	// /why is an operator view and must not answer without the admin token.
	if rec := do(t, srv.Handler(), "GET", "/admin/why/"+itemID, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated why status = %d, want 401", rec.Code)
	}

	rec := do(t, srv.Handler(), "GET", "/admin/why/"+itemID, "admin-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("why status = %d, body %s", rec.Code, rec.Body.String())
	}
	var ex Explanation
	if err := json.Unmarshal(rec.Body.Bytes(), &ex); err != nil {
		t.Fatalf("decode why body: %v", err)
	}
	if ex.Placed {
		t.Fatal("why says the item is placeable; nothing provides gpu")
	}
	if len(ex.Rejects) == 0 {
		t.Fatalf("why returned no rejects (body %s); the per-rule text never reached the operator", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no provider for gpu") {
		t.Fatalf("why body = %s, want the per-rule text", rec.Body.String())
	}
}

// doJSON posts an optional JSON body with an optional bearer token.
func doJSON(t *testing.T, h http.Handler, method, path, bearer string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		req = httptest.NewRequest(method, path, bytes.NewReader(raw))
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// seedNodeTokenForTest is SeedNodeForTest plus the plaintext token: the HTTP
// tests authenticate as a node, and the token is returned exactly once at
// registration, so it cannot be recovered from the store afterwards.
func seedNodeTokenForTest(t *testing.T, db *sql.DB, clock *FixedClock, name, role string, provides ...string) (id, token string) {
	t.Helper()
	ctx := context.Background()
	id, token, err := RegisterNode(ctx, db, clock, RegisterRequest{
		Name: name, Role: role, ProvidesDeclared: provides,
		ReachClasses: []string{"host", "lan", "wan"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	if _, err := Heartbeat(ctx, db, clock, id, HeartbeatRequest{
		ProvidesLive: provides, ReachClasses: []string{"host", "lan", "wan"}, Protocol: 1,
	}); err != nil {
		t.Fatalf("heartbeat %s: %v", name, err)
	}
	return id, token
}
