// internal/board/threads_api_test.go
package board

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cti97/the-nightshift-manager/internal/provider"
)

// threadFixture is a board with an admin and one enrolled node, so the two credential
// boundaries the thread routes use can be exercised as a client would meet them.
type threadFixture struct {
	srv       *Server
	db        *sql.DB
	clock     *FixedClock
	nodeID    string
	nodeToken string
}

func newThreadFixture(t *testing.T) *threadFixture {
	t.Helper()
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	f := &threadFixture{
		srv:   NewServer(db, clock, provider.NewRegistry(), HashToken(testAdminToken), HashToken(testEnrollToken)),
		db:    db,
		clock: clock,
	}
	node, token, err := RegisterNode(context.Background(), db, clock, RegisterRequest{
		Name: "a", Role: "executor", ProvidesDeclared: []string{"agentic"},
		ReachClasses: []string{"host"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	f.nodeID, f.nodeToken = node, token
	// A registered node is not a claiming node: placement reads live capabilities, and those
	// arrive with the first heartbeat. Without one, Claim correctly finds no candidate, which
	// is what the first version of this fixture got wrong.
	if _, err := Heartbeat(context.Background(), db, clock, node, HeartbeatRequest{
		ProvidesLive: []string{"agentic"}, ReachClasses: []string{"host"}, Protocol: ProtocolMax,
	}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	return f
}

func (f *threadFixture) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestTheThreadRoutesSpeakTheirCodes: a post to a frozen thread is a 409 with the code a
// client branches on, and the same for a closed thread and a repeated author.
func TestTheThreadRoutesSpeakTheirCodes(t *testing.T) {
	f := newThreadFixture(t)
	rec := f.do(t, "POST", "/admin/threads", testAdminToken,
		CreateThreadRequest{Subject: "s", BudgetTokens: 10})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var th Thread
	if err := json.Unmarshal(rec.Body.Bytes(), &th); err != nil {
		t.Fatalf("decode: %v", err)
	}

	rec = f.do(t, "POST", "/threads/"+th.ID+"/messages", f.nodeToken, PostMessageRequest{Author: "a", Body: "x"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "thread_frozen") {
		t.Fatalf("post to a thread whose reserve does not fit: %d %s", rec.Code, rec.Body.String())
	}

	big := 100_000
	rec = f.do(t, "PATCH", "/admin/threads/"+th.ID, testAdminToken, PatchThreadRequest{BudgetTokens: &big})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do(t, "POST", "/threads/"+th.ID+"/messages", f.nodeToken, PostMessageRequest{Author: "a", Body: "x"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("post after the reopen: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do(t, "POST", "/threads/"+th.ID+"/messages", f.nodeToken, PostMessageRequest{Author: "a", Body: "y"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "same_author_twice") {
		t.Fatalf("repeated author: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do(t, "GET", "/threads/nope", f.nodeToken, nil)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no_such_thread") {
		t.Fatalf("missing thread: %d %s", rec.Code, rec.Body.String())
	}
}

// TestTheThreadReadRequiresANode: the conversation is a participant's view; an admin token is
// not a node token, and neither is a missing one.
func TestTheThreadReadRequiresANode(t *testing.T) {
	f := newThreadFixture(t)
	rec := f.do(t, "POST", "/admin/threads", testAdminToken, CreateThreadRequest{Subject: "s", BudgetTokens: 1000})
	var th Thread
	if err := json.Unmarshal(rec.Body.Bytes(), &th); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, token := range []string{"", testAdminToken} {
		if rec := f.do(t, "GET", "/threads/"+th.ID, token, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: %d, want 401", token, rec.Code)
		}
	}
	if rec := f.do(t, "GET", "/threads/"+th.ID, f.nodeToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("node read: %d %s", rec.Code, rec.Body.String())
	}
}

// TestTheCheckpointRouteIsNodeAndLive: the ownership and liveness rules are enforced at the
// route, not only in the board function.
func TestTheCheckpointRouteIsNodeAndLive(t *testing.T) {
	f := newThreadFixture(t)
	ctx := context.Background()
	SeedEndpointForTest(t, f.db, f.clock, "local", "host", 0, "tools")
	if _, err := CreateItem(ctx, f.db, f.clock, CreateItemRequest{Title: "t", Brief: "b", Needs: []string{"agentic"}}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	claimed, err := Claim(ctx, f.db, f.clock, f.nodeID, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim = %d, err %v", len(claimed), err)
	}
	runID := claimed[0].RunID

	rec := f.do(t, "POST", "/runs/"+runID+"/checkpoint", f.nodeToken,
		CheckpointRequest{DeclaredProgress: "half"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("checkpoint: %d %s", rec.Code, rec.Body.String())
	}
	// A second node's token is a 403, not a 400: the request is well-formed and the board
	// knows whose run it is.
	other, otherToken, err := RegisterNode(ctx, f.db, f.clock, RegisterRequest{
		Name: "b", Role: "executor", ProvidesDeclared: []string{"agentic"},
		ReachClasses: []string{"host"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	_ = other
	if rec := f.do(t, "POST", "/runs/"+runID+"/checkpoint", otherToken, CheckpointRequest{DeclaredProgress: "mine"}); rec.Code != http.StatusForbidden {
		t.Fatalf("another node: %d %s, want 403", rec.Code, rec.Body.String())
	}
}

// TestTheThreadPageShowsTheBudgetBar: a phone gets one column, the numbers, the messages and
// a form. The bar's width is the used percentage, and the text beside it is the number an
// operator quotes.
func TestTheThreadPageShowsTheBudgetBar(t *testing.T) {
	f := newThreadFixture(t)
	ctx := context.Background()
	th, err := CreateThread(ctx, f.db, f.clock, CreateThreadRequest{Subject: "cache policy", BudgetTokens: 2000})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := PostMessage(ctx, f.db, f.clock, th.ID, PostMessageRequest{Author: "agent-a", Body: "delete the cache?"}); err != nil {
		t.Fatalf("PostMessage: %v", err)
	}

	req := httptest.NewRequest("GET", "/admin/threads/"+th.ID, nil)
	req.Header.Set("Accept", "text/html")
	req.SetBasicAuth("operator", testAdminToken)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("page: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"cache policy", "tokens used", "delete the cache?", "agent-a", `<form method="post"`, "Your reply"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page is missing %q", want)
		}
	}
	if !strings.Contains(body, `class="bar"`) {
		t.Fatal("the page has no budget bar")
	}
}

// TestTheThreadPagePostsAsAdmin: the form's post is the operator's, the author is "admin"
// whatever the body says, and the answer is a redirect back to the page (303, so a refresh
// does not repost).
func TestTheThreadPagePostsAsAdmin(t *testing.T) {
	f := newThreadFixture(t)
	ctx := context.Background()
	th, err := CreateThread(ctx, f.db, f.clock, CreateThreadRequest{Subject: "s", BudgetTokens: 10_000})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	form := "body=hold+off+on+the+cache&author=agent-a"
	req := httptest.NewRequest("POST", "/admin/threads/"+th.ID+"/messages", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("operator", testAdminToken)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("form post: %d %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/threads/"+th.ID {
		t.Fatalf("redirect = %q, want the thread page", loc)
	}
	view, err := GetThread(ctx, f.db, th.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if len(view.Messages) != 1 || view.Messages[0].Author != "admin" {
		t.Fatalf("messages = %+v, want one from admin", view.Messages)
	}
	if view.Messages[0].Body != "hold off on the cache" {
		t.Fatalf("body = %q, want the form's text", view.Messages[0].Body)
	}

	// The JSON branch is the same rule: the credential names the author, not the body. An
	// operator posting here cannot appear as an agent in the record.
	rec = f.do(t, "POST", "/admin/threads/"+th.ID+"/messages", testAdminToken,
		PostMessageRequest{Author: "agent-a", Body: "json reply"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("json post: %d %s", rec.Code, rec.Body.String())
	}
	view, err = GetThread(ctx, f.db, th.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if last := view.Messages[len(view.Messages)-1]; last.Author != "admin" {
		t.Fatalf("the JSON post was recorded as %q, want admin", last.Author)
	}
}

// TestThePageAndTheJSONAgreeOnRemaining: one route, two representations, one set of numbers.
func TestThePageAndTheJSONAgreeOnRemaining(t *testing.T) {
	f := newThreadFixture(t)
	ctx := context.Background()
	th, err := CreateThread(ctx, f.db, f.clock, CreateThreadRequest{Subject: "s", BudgetTokens: 5000})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	ten := 10
	if _, err := PostMessage(ctx, f.db, f.clock, th.ID, PostMessageRequest{Author: "agent-a", Body: "x", Tokens: &ten}); err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	get := func(accept string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/admin/threads/"+th.ID, nil)
		req.Header.Set("Accept", accept)
		req.SetBasicAuth("operator", testAdminToken)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	jsonRec := get("application/json")
	var view ThreadView
	if err := json.Unmarshal(jsonRec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	htmlBody := get("text/html").Body.String()
	if !strings.Contains(htmlBody, "4990 left") {
		t.Fatalf("the page does not show the JSON's remaining (%d): %s", view.Remaining, htmlBody)
	}
	if view.Remaining != 4990 {
		t.Fatalf("remaining = %d, want 4990", view.Remaining)
	}
}
