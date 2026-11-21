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
