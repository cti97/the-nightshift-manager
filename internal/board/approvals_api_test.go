// internal/board/approvals_api_test.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/provider"
)

// approvalsFixture is one board with one enrolled node, one endpoint and the
// admin server in front of them. Items are created and parked through the same
// board functions a node would drive, so the list under test is a list of real
// escalations rather than rows written to satisfy a query.
type approvalsFixture struct {
	srv   *Server
	db    *sql.DB
	clock *FixedClock
	node  string
	token string
}

func newApprovalsFixture(t *testing.T) *approvalsFixture {
	t.Helper()
	db, clock := newTestDB(t)
	f := &approvalsFixture{
		srv:   NewServer(db, clock, provider.NewRegistry(), HashToken(testAdminToken), HashToken(testEnrollToken)),
		db:    db,
		clock: clock,
	}
	ctx := context.Background()
	node, token, err := RegisterNode(ctx, db, clock, RegisterRequest{
		Name: "a", Role: "executor", ProvidesDeclared: []string{"agentic"},
		ReachClasses: []string{"host", "lan", "wan"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	f.node, f.token = node, token
	if _, err := Heartbeat(ctx, db, clock, node, HeartbeatRequest{
		ProvidesLive: []string{"agentic"}, ReachClasses: []string{"host", "lan", "wan"}, Protocol: ProtocolMax,
	}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	return f
}

// create queues one item and returns its id. The clock steps a second between
// calls so created_at is distinct and the ordering assertions mean something.
func (f *approvalsFixture) create(t *testing.T, title string, priority int) string {
	t.Helper()
	f.clock.Advance(time.Second)
	id, err := CreateItem(context.Background(), f.db, f.clock, CreateItemRequest{
		Title: title, Brief: "b", Needs: []string{"agentic"}, Priority: priority,
	})
	if err != nil {
		t.Fatalf("CreateItem %q: %v", title, err)
	}
	return id
}

// parkNext claims whatever the board offers next — priority DESC, then oldest
// first — and escalates it. It returns the parked item's id.
func (f *approvalsFixture) parkNext(t *testing.T, action string) string {
	t.Helper()
	ctx := context.Background()
	claimed, err := Claim(ctx, f.db, f.clock, f.node, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim = %d items, err %v", len(claimed), err)
	}
	runID := claimed[0].RunID
	if err := StartRun(ctx, f.db, f.clock, runID, f.node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := Escalate(ctx, f.db, f.clock, runID, f.node, ApprovalRequest{
		Action: action, Reason: "the run needs it", Options: []string{"yes", "no"},
	}); err != nil {
		t.Fatalf("Escalate: %v", err)
	}
	return claimed[0].Item.ID
}

func (f *approvalsFixture) list(t *testing.T, bearer string) (*httptest.ResponseRecorder, []Approval) {
	t.Helper()
	rec := do(t, f.srv.Handler(), http.MethodGet, "/admin/approvals", bearer)
	var got []Approval
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode approvals: %v (body %s)", err, rec.Body.String())
		}
	}
	return rec, got
}

// TestDecisionRouteAnswersConflicts walks the status codes a caller meets on
// the decision route: 200 for the decision, 409 for the second one (D4), 404
// for an id that is not on the board, and 400 for a body that names no
// decision. The distinction matters to the CLI: 409 means "someone got there
// first", 404 means "wrong id", and 400 means "fix the command".
func TestDecisionRouteAnswersConflicts(t *testing.T) {
	f := newApprovalsFixture(t)
	id := f.create(t, "waiting", 0)
	f.parkNext(t, "delete the cache")

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testAdminToken)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	if rec := post("/admin/approvals/"+id, `{"decision":"later"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown decision = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := post("/admin/approvals/"+id, `{"decision":"approve","note":"yes"}`); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := post("/admin/approvals/"+id, `{"decision":"deny","note":"no"}`); rec.Code != http.StatusConflict {
		t.Fatalf("second decision = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := post("/admin/approvals/no-such-item", `{"decision":"approve"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown item = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestApprovalsListIsOldestFirst pins that the queue is ordered by when the
// item was created, not by when it escalated and not by row order. The three
// created_at values are rewritten so that creation order, insertion order and
// claim order are three different sequences: any implementation that reads the
// table or the claim index in its own order fails here, and only an ORDER BY
// created_at passes.
func TestApprovalsListIsOldestFirst(t *testing.T) {
	f := newApprovalsFixture(t)
	first := f.create(t, "first", 0)
	second := f.create(t, "second", 10)
	third := f.create(t, "third", 20)

	// Insertion order is first, second, third; claim order (priority DESC) is
	// third, second, first; the board's own clock order is second, third, first.
	for _, step := range []struct {
		id    string
		order int64
	}{
		{first, 300}, {second, 100}, {third, 200},
	} {
		if _, err := f.db.Exec(`UPDATE work_items SET created_at=? WHERE id=?`, step.order, step.id); err != nil {
			t.Fatalf("rewrite created_at: %v", err)
		}
	}

	for _, action := range []string{"act-third", "act-second", "act-first"} {
		f.parkNext(t, action)
	}

	rec, got := f.list(t, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var ids []string
	var actions []string
	for _, a := range got {
		ids = append(ids, a.ItemID)
		actions = append(actions, a.Action)
	}
	if want := []string{second, third, first}; !slices.Equal(ids, want) {
		t.Fatalf("order = %v, want %v (oldest created first)", ids, want)
	}
	if want := []string{"act-second", "act-third", "act-first"}; !slices.Equal(actions, want) {
		t.Fatalf("actions = %v, want %v", actions, want)
	}
}

// TestApprovalsListIsEmptyNotNull: an empty queue is [], so a caller can tell
// "nothing is waiting" from "the route did not answer". An HTML page is not
// what a JSON client asked for, so Accept is explicit here.
func TestApprovalsListIsEmptyNotNull(t *testing.T) {
	f := newApprovalsFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/approvals", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Fatalf("body = %q, want []", body)
	}
}

// TestApprovalsRequireTheAdminToken is D2: only the admin decides, and the
// queue is where the decision is made, so a node token — which every run on
// every node carries — must not read it.
func TestApprovalsRequireTheAdminToken(t *testing.T) {
	f := newApprovalsFixture(t)

	if rec, _ := f.list(t, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", rec.Code)
	}
	if rec, _ := f.list(t, f.token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("node token = %d, want 401", rec.Code)
	}
	if rec, _ := f.list(t, testAdminToken); rec.Code != http.StatusOK {
		t.Fatalf("admin token = %d, want 200", rec.Code)
	}
}

// TestApprovalsListExcludesNonParkedItems: the list is the set of decisions
// waiting, not a history. A done item that carries approval_granted — an
// approved item that then completed — is not waiting, and neither is a queued
// one that has never been claimed.
func TestApprovalsListExcludesNonParkedItems(t *testing.T) {
	f := newApprovalsFixture(t)
	ctx := context.Background()

	parked := f.create(t, "waiting", 0)
	f.parkNext(t, "delete the cache")
	_ = f.create(t, "still queued", 0)

	finished := f.create(t, "finished", 10)
	claimed, err := Claim(ctx, f.db, f.clock, f.node, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim = %d items, err %v", len(claimed), err)
	}
	if claimed[0].Item.ID != finished {
		t.Fatalf("claimed %s, want %s", claimed[0].Item.ID, finished)
	}
	if err := StartRun(ctx, f.db, f.clock, claimed[0].RunID, f.node); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := Report(ctx, f.db, f.clock, claimed[0].RunID, f.node, ReportRequest{Status: "ok", Summary: "done"}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if _, err := f.db.Exec(
		`UPDATE work_items SET approval_granted='{"action":"delete the cache","note":"yes","decided_at":1}' WHERE id=?`,
		finished); err != nil {
		t.Fatalf("grant: %v", err)
	}

	_, got := f.list(t, testAdminToken)
	if len(got) != 1 || got[0].ItemID != parked {
		t.Fatalf("queue = %+v, want only the parked item %s", got, parked)
	}
}

// TestAParkedRowWithoutARequestIsStillListed is the migration's NULL edge,
// written down: ALTER TABLE ADD COLUMN does not backfill, so a row parked
// before migration 4 exists with no request. It is still a question waiting for
// a human, so it appears with empty fields rather than vanishing; the page can
// then say the request is missing, which is the truth.
func TestAParkedRowWithoutARequestIsStillListed(t *testing.T) {
	f := newApprovalsFixture(t)
	id := f.create(t, "old parked row", 0)
	f.parkNext(t, "delete the cache")
	if _, err := f.db.Exec(`UPDATE work_items SET approval_request=NULL WHERE id=?`, id); err != nil {
		t.Fatalf("clear request: %v", err)
	}

	rec, got := f.list(t, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a missing request is not a broken route", rec.Code)
	}
	if len(got) != 1 || got[0].ItemID != id {
		t.Fatalf("queue = %+v, want the row with no request", got)
	}
	if got[0].Action != "" || len(got[0].Options) != 0 {
		t.Fatalf("approval = %+v, want empty action and options", got[0])
	}
	// options is [] on the wire, never null: the same rule the stored request
	// follows, so a page ranging it cannot meet null.
	if body := rec.Body.String(); strings.Contains(body, `"options":null`) {
		t.Fatalf("body = %s, want options as []", body)
	}
}
