// internal/board/approvals_api_test.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	}, RunTokens{}); err != nil {
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

// page fetches a URL as a browser would, with the admin token in a Basic header
// — the only credential a browser can present (M3 D8).
func (f *approvalsFixture) page(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.SetBasicAuth("operator", testAdminToken)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// submit posts the page's form the way a browser does.
func (f *approvalsFixture) submit(t *testing.T, itemID, decision, note string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"decision": {decision}, "note": {note}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/admin/approvals/"+itemID, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("operator", testAdminToken)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestApprovalsPageRendersEveryPendingItem is the daily screen: one card per
// parked item, the action and the reason visible, a form per card, and no script
// anywhere — a page that needs JavaScript to approve work is a page you cannot
// trust from a locked-down phone.
func TestApprovalsPageRendersEveryPendingItem(t *testing.T) {
	f := newApprovalsFixture(t)
	f.create(t, "cache cleanup", 0)
	f.parkNext(t, "delete /var/cache/app")
	f.create(t, "key rotation", 10)
	f.parkNext(t, "rotate the api key")

	rec := f.page(t, "/admin/approvals")
	if rec.Code != http.StatusOK {
		t.Fatalf("page = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"delete /var/cache/app", "rotate the api key",
		"cache cleanup", "key rotation",
		"the run needs it",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("page does not show %q:\n%s", want, body)
		}
	}
	if got := strings.Count(body, "<form "); got != 2 {
		t.Fatalf("forms = %d, want one per parked item (2)", got)
	}
	if strings.Contains(strings.ToLower(body), "<script") {
		t.Fatal("the page ships JavaScript; it must be usable with scripts off")
	}
}

// TestApprovalsPageIsPhoneReadable asserts the MARKUP that makes a single column:
// a viewport meta tag, one main element, and block-level cards. Styling can be
// edited by anyone; these are the parts that decide whether a phone reflows the
// page at all.
func TestApprovalsPageIsPhoneReadable(t *testing.T) {
	f := newApprovalsFixture(t)
	f.create(t, "cache cleanup", 0)
	f.parkNext(t, "delete /var/cache/app")

	body := f.page(t, "/admin/approvals").Body.String()
	if !strings.Contains(body, `name="viewport"`) || !strings.Contains(body, "width=device-width") {
		t.Fatalf("no viewport meta tag; a phone renders this at desktop width:\n%s", body)
	}
	if got := strings.Count(body, "<main>"); got != 1 {
		t.Fatalf("<main> elements = %d, want 1", got)
	}
	if !strings.Contains(body, `<section class="card"`) {
		t.Fatalf("items are not block-level sections:\n%s", body)
	}
	for _, forbidden := range []string{"<table", "grid-template-columns", "display: flex", "display:flex"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("page uses %q; the layout must be a single column a phone can reflow", forbidden)
		}
	}
}

// TestApprovalsPageSubmitsADecision is the whole loop on the page's own
// transport: a form post decides, the answer is a redirect back to the queue (so a
// refresh cannot re-decide), and the decided item has left the page.
func TestApprovalsPageSubmitsADecision(t *testing.T) {
	f := newApprovalsFixture(t)
	approved := f.create(t, "cache cleanup", 0)
	f.parkNext(t, "delete /var/cache/app")
	f.create(t, "key rotation", 10)
	f.parkNext(t, "rotate the api key")

	rec := f.submit(t, approved, Approve, "go ahead, the cache is regenerable")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("form post = %d, want 303 (body %s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/approvals" {
		t.Fatalf("Location = %q, want /admin/approvals", loc)
	}

	var status string
	if err := f.db.QueryRow(`SELECT status FROM work_items WHERE id=?`, approved).Scan(&status); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "queued" {
		t.Fatalf("item status = %q after the form post, want queued", status)
	}

	body := f.page(t, "/admin/approvals").Body.String()
	if !strings.Contains(body, "rotate the api key") {
		t.Fatalf("the undecided item left the page too:\n%s", body)
	}
	if strings.Contains(body, "delete /var/cache/app") {
		t.Fatalf("the decided item is still on the page:\n%s", body)
	}
	if got := strings.Count(body, "<form "); got != 1 {
		t.Fatalf("forms = %d, want the one still waiting", got)
	}
}

// TestJsonIsStillAvailableForScripts: the same route, the representation the API
// client asked for. A script must not have to parse HTML to read the queue.
func TestJsonIsStillAvailableForScripts(t *testing.T) {
	f := newApprovalsFixture(t)
	f.create(t, "cache cleanup", 0)
	f.parkNext(t, "delete /var/cache/app")

	req := httptest.NewRequest(http.MethodGet, "/admin/approvals", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("json = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var got []Approval
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Action != "delete /var/cache/app" {
		t.Fatalf("approvals = %+v, want the parked item", got)
	}
}

// TestTheApprovalsPageAcceptsBasicAuth is what makes the page usable from a phone:
// the browser cannot set a bearer header, so the token arrives as the Basic
// password. A wrong password is 401 with the prompt, not a JSON body.
func TestTheApprovalsPageAcceptsBasicAuth(t *testing.T) {
	f := newApprovalsFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/admin/approvals", nil)
	req.Header.Set("Accept", "text/html")
	req.SetBasicAuth("operator", testAdminToken)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("basic auth with the admin token = %d, want 200", rec.Code)
	}

	bad := httptest.NewRequest(http.MethodGet, "/admin/approvals", nil)
	bad.Header.Set("Accept", "text/html")
	bad.SetBasicAuth("operator", "wrong")
	badRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d, want 401", badRec.Code)
	}
	if !strings.HasPrefix(badRec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("WWW-Authenticate = %q, want a Basic prompt so the browser can retry", badRec.Header().Get("WWW-Authenticate"))
	}
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
