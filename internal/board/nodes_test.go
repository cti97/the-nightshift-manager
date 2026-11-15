package board

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cti97/the-nightshift-manager/internal/provider"
)

func TestRegisterIssuesAWorkingTokenAndStoresOnlyItsHash(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()

	id, token, err := RegisterNode(ctx, db, clock, RegisterRequest{
		Name: "home-laptop", Role: "executor",
		ProvidesDeclared: []string{"agentic", "shell"},
		ReachClasses:     []string{"host", "lan", "wan"},
		Concurrency:      1,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if token == "" || id == "" {
		t.Fatal("register returned an empty id or token")
	}

	var stored string
	if err := db.QueryRow(`SELECT token_hash FROM nodes WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatalf("read token_hash: %v", err)
	}
	if stored == token {
		t.Fatal("the plaintext token was stored; only its hash may be")
	}
	if stored != HashToken(token) {
		t.Fatal("stored hash does not match HashToken(token)")
	}
}

func TestRegisterRejectsDuplicateName(t *testing.T) {
	db, clock := newTestDB(t)
	req := RegisterRequest{Name: "pi", Role: "steward", ProvidesDeclared: []string{"reflex"}, ReachClasses: []string{"host"}}
	if _, _, err := RegisterNode(context.Background(), db, clock, req); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if _, _, err := RegisterNode(context.Background(), db, clock, req); err == nil {
		t.Fatal("duplicate name accepted, want an error")
	}
}

func TestHeartbeatUpdatesLiveCapabilitiesAndLease(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	id, _, err := RegisterNode(ctx, db, clock, RegisterRequest{
		Name: "home-laptop", Role: "executor",
		ProvidesDeclared: []string{"agentic"}, ReachClasses: []string{"host"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// An item this node holds, with a lease that expires soon.
	clock.Set(1_700_000_000)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO work_items (id,title,brief,needs,status,claimed_by,lease_expires_at,created_by,created_at,updated_at)
		VALUES ('w1','t','b','["agentic"]','claimed',?,?, 'admin',?,?)`,
		id, clock.Now()+10, clock.Now(), clock.Now()); err != nil {
		t.Fatalf("insert item: %v", err)
	}

	resp, err := Heartbeat(ctx, db, clock, id, HeartbeatRequest{
		ProvidesLive: []string{"agentic", "lan:nas"},
		ReachClasses: []string{"host", "lan"},
		FreeSlots:    1,
		AgentVersion: "v0.1.0",
		Protocol:     ProtocolMax,
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if resp.Status != "online" {
		t.Fatalf("status = %q, want online", resp.Status)
	}

	var lease int64
	if err := db.QueryRow(`SELECT lease_expires_at FROM work_items WHERE id='w1'`).Scan(&lease); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if lease != clock.Now()+leaseSeconds {
		t.Fatalf("lease = %d, want %d", lease, clock.Now()+leaseSeconds)
	}

	var live string
	if err := db.QueryRow(`SELECT provides_live FROM nodes WHERE id=?`, id).Scan(&live); err != nil {
		t.Fatalf("read provides_live: %v", err)
	}
	if live != `["agentic","lan:nas"]` {
		t.Fatalf("provides_live = %s", live)
	}
}

func TestHeartbeatMarksIncompatibleProtocol(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	id, _, _ := RegisterNode(ctx, db, clock, RegisterRequest{
		Name: "old-node", Role: "executor", ProvidesDeclared: []string{}, ReachClasses: []string{},
	})

	resp, err := Heartbeat(ctx, db, clock, id, HeartbeatRequest{Protocol: 99})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if resp.Status != "incompatible" {
		t.Fatalf("status = %q, want incompatible", resp.Status)
	}
}

func TestHeartbeatOnlyExtendsThisNodesClaimedAndRunningLeases(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	a, _, err := RegisterNode(ctx, db, clock, RegisterRequest{Name: "a", Role: "executor"})
	if err != nil {
		t.Fatalf("register a: %v", err)
	}
	b, _, err := RegisterNode(ctx, db, clock, RegisterRequest{Name: "b", Role: "executor"})
	if err != nil {
		t.Fatalf("register b: %v", err)
	}
	clock.Set(1_700_000_000)

	// claimed/running by a are extended; b's lease, a's finished item and an
	// unowned item must all keep their old expiry.
	items := []struct {
		id     string
		status string
		owner  any
		lease  int64
	}{
		{"w1", "claimed", a, 100},
		{"w2", "running", b, 200},
		{"w3", "done", a, 300},
		{"w4", "claimed", nil, 400},
	}
	for _, it := range items {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO work_items (id,title,brief,needs,status,claimed_by,lease_expires_at,created_by,created_at,updated_at)
			VALUES (?,?,?,'[]',?,?,?,'admin',?,?)`,
			it.id, "t", "b", it.status, it.owner, it.lease, clock.Now(), clock.Now()); err != nil {
			t.Fatalf("insert %s: %v", it.id, err)
		}
	}

	if _, err := Heartbeat(ctx, db, clock, a, HeartbeatRequest{Protocol: ProtocolMax}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	for _, it := range items {
		want := it.lease
		if it.id == "w1" {
			want = clock.Now() + leaseSeconds
		}
		var got int64
		if err := db.QueryRow(`SELECT lease_expires_at FROM work_items WHERE id=?`, it.id).Scan(&got); err != nil {
			t.Fatalf("read lease %s: %v", it.id, err)
		}
		if got != want {
			t.Fatalf("%s lease = %d, want %d", it.id, got, want)
		}
	}
}

func TestIncompatibleHeartbeatPersistsStatusAndLeavesLeases(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	id, _, err := RegisterNode(ctx, db, clock, RegisterRequest{
		Name: "future-node", Role: "executor", ProvidesDeclared: []string{"agentic"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	clock.Set(1_700_000_000)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO work_items (id,title,brief,needs,status,claimed_by,lease_expires_at,created_by,created_at,updated_at)
		VALUES ('w1','t','b','[]','claimed',?,10,'admin',?,?)`,
		id, clock.Now(), clock.Now()); err != nil {
		t.Fatalf("insert item: %v", err)
	}

	if _, err := Heartbeat(ctx, db, clock, id, HeartbeatRequest{
		ProvidesLive: []string{"agentic"}, Protocol: 99,
	}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	var status, live string
	var lastSeen int64
	if err := db.QueryRow(`SELECT status, provides_live, last_seen_at FROM nodes WHERE id=?`, id).
		Scan(&status, &live, &lastSeen); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if status != "incompatible" {
		t.Fatalf("node status = %q, want incompatible", status)
	}
	if live != "[]" {
		t.Fatalf("provides_live = %s, want it cleared by the incompatible heartbeat", live)
	}
	if lastSeen != clock.Now() {
		t.Fatalf("last_seen_at = %d, want %d", lastSeen, clock.Now())
	}
	var lease int64
	if err := db.QueryRow(`SELECT lease_expires_at FROM work_items WHERE id='w1'`).Scan(&lease); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if lease != 10 {
		t.Fatalf("lease = %d, want it untouched at 10", lease)
	}

	// A node that later speaks a supported protocol comes back online.
	resp, err := Heartbeat(ctx, db, clock, id, HeartbeatRequest{
		ProvidesLive: []string{"agentic"}, Protocol: ProtocolMax,
	})
	if err != nil {
		t.Fatalf("recovery heartbeat: %v", err)
	}
	if resp.Status != "online" {
		t.Fatalf("recovery status = %q, want online", resp.Status)
	}
}

func TestHeartbeatRejectsUnknownCapabilityAndKeepsLastGoodState(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	id, _, err := RegisterNode(ctx, db, clock, RegisterRequest{
		Name: "home-laptop", Role: "executor", ProvidesDeclared: []string{"agentic"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := Heartbeat(ctx, db, clock, id, HeartbeatRequest{
		ProvidesLive: []string{"agentic"}, Protocol: ProtocolMax,
	}); err != nil {
		t.Fatalf("good heartbeat: %v", err)
	}

	_, err = Heartbeat(ctx, db, clock, id, HeartbeatRequest{
		ProvidesLive: []string{"lang:nas"}, Protocol: ProtocolMax,
	})
	if err == nil {
		t.Fatal("unknown capability accepted on heartbeat, want an error")
	}
	if !strings.Contains(err.Error(), `"lang:nas"`) {
		t.Fatalf("error %q does not name the offending capability", err)
	}

	var status, live string
	if err := db.QueryRow(`SELECT status, provides_live FROM nodes WHERE id=?`, id).Scan(&status, &live); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if status != "online" || live != `["agentic"]` {
		t.Fatalf("status = %q, provides_live = %s; want the last good heartbeat kept", status, live)
	}
}

// The tests below pin the brief asks for in self-review: a
// rejected registration must leave no half-written node, and an unknown
// capability name must surface by name rather than become unplaceable work.

func TestRegisterRejectsUnknownCapabilityAndWritesNothing(t *testing.T) {
	db, clock := newTestDB(t)
	_, _, err := RegisterNode(context.Background(), db, clock, RegisterRequest{
		Name: "typo", Role: "executor", ProvidesDeclared: []string{"agentic", "agnet"},
	})
	if err == nil {
		t.Fatal("unknown capability accepted, want an error")
	}
	if !strings.Contains(err.Error(), `"agnet"`) {
		t.Fatalf("error %q does not name the offending capability", err)
	}
	var nodes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if nodes != 0 {
		t.Fatalf("nodes = %d after a rejected registration, want 0", nodes)
	}
}

func TestDuplicateRegisterLeavesOneNodeAndOneEvent(t *testing.T) {
	db, clock := newTestDB(t)
	req := RegisterRequest{Name: "pi", Role: "steward", ProvidesDeclared: []string{"reflex"}, ReachClasses: []string{"host"}}
	if _, _, err := RegisterNode(context.Background(), db, clock, req); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if _, _, err := RegisterNode(context.Background(), db, clock, req); err == nil {
		t.Fatal("duplicate name accepted, want an error")
	}
	var nodes, events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE entity='node' AND type='registered'`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if nodes != 1 || events != 1 {
		t.Fatalf("after a duplicate registration: nodes = %d, registered events = %d; want 1 and 1", nodes, events)
	}
}

func postJSON(t *testing.T, h http.Handler, path, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestNodeAuthAcceptsIssuedTokenAndRejectsRotatedOrUnknown(t *testing.T) {
	srv, _ := newServer(t)
	h := srv.Handler()

	rec := postJSON(t, h, "/nodes/register", testEnrollToken, RegisterRequest{
		Name: "home-laptop", Role: "executor",
		ProvidesDeclared: []string{"agentic"}, ReachClasses: []string{"host"}, Concurrency: 1,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("register status = %d, want 201: %s", rec.Code, rec.Body)
	}
	var reg RegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &reg); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if reg.Token == "" || reg.NodeID == "" {
		t.Fatal("register response is missing the id or the token")
	}

	beat := HeartbeatRequest{ProvidesLive: []string{"agentic"}, ReachClasses: []string{"host"}, Protocol: 1}

	rec = postJSON(t, h, "/nodes/heartbeat", reg.Token, beat)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat with an issued token = %d, want 200: %s", rec.Code, rec.Body)
	}

	for _, tok := range []string{"", "not-a-node-token"} {
		rec = postJSON(t, h, "/nodes/heartbeat", tok, beat)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("heartbeat with token %q = %d, want 401", tok, rec.Code)
		}
	}

	// Rotation: the old credential must stop working the moment the stored
	// hash changes, and the new one must work.
	if _, err := srv.db.Exec(`UPDATE nodes SET token_hash=? WHERE id=?`, HashToken("rotated-secret"), reg.NodeID); err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	rec = postJSON(t, h, "/nodes/heartbeat", reg.Token, beat)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("heartbeat with the rotated-away token = %d, want 401", rec.Code)
	}
	rec = postJSON(t, h, "/nodes/heartbeat", "rotated-secret", beat)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat with the rotated-in token = %d, want 200: %s", rec.Code, rec.Body)
	}
}

// Registration writes configuration, never reality: a node that has not
// heartbeated yet provides nothing, and an empty list is stored as [].
func TestRegisterWritesDeclaredButNoLiveCapabilities(t *testing.T) {
	db, clock := newTestDB(t)
	id, _, err := RegisterNode(context.Background(), db, clock, RegisterRequest{
		Name: "home-laptop", Role: "executor",
		ProvidesDeclared: []string{"agentic", "shell"},
		ReachClasses:     nil,
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	var declared, live, reach string
	if err := db.QueryRow(`SELECT provides_declared, provides_live, reach_classes FROM nodes WHERE id=?`, id).
		Scan(&declared, &live, &reach); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if declared != `["agentic","shell"]` {
		t.Fatalf("provides_declared = %s", declared)
	}
	if live != "[]" {
		t.Fatalf("provides_live = %s, want [] until the first heartbeat", live)
	}
	if reach != "[]" {
		t.Fatalf("reach_classes = %s, want [] (empty slice, not null)", reach)
	}

	var actor, typ string
	if err := db.QueryRow(`SELECT actor, type FROM events WHERE entity='node' AND entity_id=?`, id).
		Scan(&actor, &typ); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if actor != "node" || typ != "registered" {
		t.Fatalf("event = %s/%s, want node/registered", actor, typ)
	}
}

func TestIncompatibleHeartbeatClearsLiveCapabilities(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	id, _, err := RegisterNode(ctx, db, clock, RegisterRequest{
		Name: "old-node", Role: "executor", ProvidesDeclared: []string{"agentic", "lan:nas"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := Heartbeat(ctx, db, clock, id, HeartbeatRequest{
		ProvidesLive: []string{"agentic", "lan:nas"}, Protocol: ProtocolMax,
	}); err != nil {
		t.Fatalf("good heartbeat: %v", err)
	}
	if _, err := Heartbeat(ctx, db, clock, id, HeartbeatRequest{Protocol: 99}); err != nil {
		t.Fatalf("incompatible heartbeat: %v", err)
	}

	var live string
	if err := db.QueryRow(`SELECT provides_live FROM nodes WHERE id=?`, id).Scan(&live); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if live != "[]" {
		t.Fatalf("provides_live = %s after an incompatible heartbeat, want []", live)
	}
}

func TestHeartbeatPreservesOperatorPause(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	id, _, err := RegisterNode(ctx, db, clock, RegisterRequest{Name: "pi", Role: "steward"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE nodes SET status='paused' WHERE id=?`, id); err != nil {
		t.Fatalf("pause node: %v", err)
	}

	if _, err := Heartbeat(ctx, db, clock, id, HeartbeatRequest{Protocol: ProtocolMax}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM nodes WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if status != "paused" {
		t.Fatalf("status = %q, want the operator pause preserved", status)
	}
}

func TestHeartbeatReportsQueuedAndPendingApprovals(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	id, _, err := RegisterNode(ctx, db, clock, RegisterRequest{Name: "home-laptop", Role: "executor"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, it := range []struct{ id, status string }{
		{"w1", "queued"}, {"w2", "queued"}, {"w3", "needs_approval"}, {"w4", "needs_approval"}, {"w5", "done"},
	} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO work_items (id,title,brief,needs,status,created_by,created_at,updated_at)
			VALUES (?,?,?,'[]',?,'admin',?,?)`,
			it.id, "t", "b", it.status, clock.Now(), clock.Now()); err != nil {
			t.Fatalf("insert %s: %v", it.id, err)
		}
	}

	resp, err := Heartbeat(ctx, db, clock, id, HeartbeatRequest{Protocol: ProtocolMax})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if resp.Queued != 2 {
		t.Fatalf("queued = %d, want 2", resp.Queued)
	}
	if resp.Approvals != 2 {
		t.Fatalf("pending_approvals = %d, want 2", resp.Approvals)
	}
}

func TestHeartbeatRejectsUnknownNodeID(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	if _, _, err := RegisterNode(ctx, db, clock, RegisterRequest{Name: "pi", Role: "steward"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	for _, protocol := range []int{ProtocolMax, 99} {
		if _, err := Heartbeat(ctx, db, clock, "no-such-node", HeartbeatRequest{Protocol: protocol}); err == nil {
			t.Fatalf("heartbeat for an unknown node (protocol %d) succeeded, want an error", protocol)
		}
	}
}

func postRaw(t *testing.T, h http.Handler, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope %q: %v", rec.Body, err)
	}
	return envelope.Error.Code
}

func TestRegisterHandlerReturnsConflictForDuplicateName(t *testing.T) {
	srv, _ := newServer(t)
	h := srv.Handler()
	body := RegisterRequest{Name: "pi", Role: "steward", ProvidesDeclared: []string{"reflex"}}

	if rec := postJSON(t, h, "/nodes/register", testEnrollToken, body); rec.Code != http.StatusCreated {
		t.Fatalf("first register = %d, want 201: %s", rec.Code, rec.Body)
	}
	rec := postJSON(t, h, "/nodes/register", testEnrollToken, body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate register = %d, want 409: %s", rec.Code, rec.Body)
	}
	if code := errorCode(t, rec); code != "register" {
		t.Fatalf("error code = %q, want register", code)
	}
	if strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("a rejected registration leaked a token: %s", rec.Body)
	}
}

func TestHandlersRejectMalformedJSON(t *testing.T) {
	srv, _ := newServer(t)
	h := srv.Handler()

	rec := postRaw(t, h, "/nodes/register", testEnrollToken, "{")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed register = %d, want 400: %s", rec.Code, rec.Body)
	}
	if code := errorCode(t, rec); code != "bad_json" {
		t.Fatalf("register error code = %q, want bad_json", code)
	}

	reg := postJSON(t, h, "/nodes/register", testEnrollToken, RegisterRequest{Name: "pi", Role: "steward"})
	if reg.Code != http.StatusCreated {
		t.Fatalf("register = %d, want 201: %s", reg.Code, reg.Body)
	}
	var issued RegisterResponse
	if err := json.Unmarshal(reg.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode register response: %v", err)
	}

	rec = postRaw(t, h, "/nodes/heartbeat", issued.Token, "{")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed heartbeat = %d, want 400: %s", rec.Code, rec.Body)
	}
	if code := errorCode(t, rec); code != "bad_json" {
		t.Fatalf("heartbeat error code = %q, want bad_json", code)
	}
}

// A pause has to survive both heartbeat paths. The incompatible path used to
// overwrite it, so the next good beat saw 'incompatible' and went 'online'.
func TestPauseSurvivesIncompatibleThenGoodHeartbeat(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	id, _, err := RegisterNode(ctx, db, clock, RegisterRequest{Name: "pi", Role: "steward"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE nodes SET status='paused' WHERE id=?`, id); err != nil {
		t.Fatalf("pause node: %v", err)
	}

	if _, err := Heartbeat(ctx, db, clock, id, HeartbeatRequest{Protocol: 99}); err != nil {
		t.Fatalf("incompatible heartbeat: %v", err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM nodes WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if status != "paused" {
		t.Fatalf("status = %q after an incompatible heartbeat, want the pause preserved", status)
	}

	if _, err := Heartbeat(ctx, db, clock, id, HeartbeatRequest{Protocol: ProtocolMax}); err != nil {
		t.Fatalf("good heartbeat: %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM nodes WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if status != "paused" {
		t.Fatalf("status = %q after a good heartbeat, want the pause preserved", status)
	}
}

// Registration mints a credential that can claim work — and a claim carries the
// operator's brief — so it must require a credential of its own, and that
// credential must not be the admin token (spec §15 read register as a
// node-token endpoint, which cannot bootstrap itself; the enrollment secret can
// register and nothing else).
func TestRegisterRequiresTheEnrollTokenAndTheAdminTokenCannotEnroll(t *testing.T) {
	srv, _ := newServer(t)
	h := srv.Handler()
	body := RegisterRequest{Name: "home-laptop", Role: "executor", ReachClasses: []string{"host"}}

	for _, tc := range []struct{ bearer, why string }{
		{"", "no token"},
		{testAdminToken, "the admin token is not an enrollment credential"},
		{testEnrollToken + "x", "a wrong enrollment token"},
	} {
		rec := postJSON(t, h, "/nodes/register", tc.bearer, body)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("register with %s = %d, want 401 (body %s)", tc.why, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), `"token":`) {
			t.Fatalf("a refused registration leaked a node token: %s", rec.Body)
		}
	}

	rec := postJSON(t, h, "/nodes/register", testEnrollToken, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register with the enrollment token = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	var issued RegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if issued.NodeID == "" || issued.Token == "" {
		t.Fatalf("register response = %+v, want an id and a token", issued)
	}

	// The two credentials are not interchangeable in either direction: the
	// enrollment secret can do exactly one thing.
	if rec := do(t, h, "GET", "/admin/board", testEnrollToken); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/admin/board with the enrollment token = %d, want 401", rec.Code)
	}
	if rec := do(t, h, "GET", "/admin/board", testAdminToken); rec.Code != http.StatusOK {
		t.Fatalf("/admin/board with the admin token = %d, want 200", rec.Code)
	}
}

// A server configured without an enrollment secret must refuse to mint tokens.
// Failing open would undo the credential entirely.
func TestRegisterFailsClosedWithoutAnEnrollSecret(t *testing.T) {
	db, clock := newTestDB(t)
	h := NewServer(db, clock, provider.NewRegistry(), HashToken(testAdminToken), "").Handler()

	for _, bearer := range []string{"", testAdminToken, testEnrollToken} {
		rec := postJSON(t, h, "/nodes/register", bearer, RegisterRequest{Name: "a", Role: "executor"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("register with %q on an enroll-less server = %d, want 401", bearer, rec.Code)
		}
	}
}

// reach_classes is compared by exact string equality in placement, so a node
// that stores "Host" heartbeats happily and claims nothing forever. Both
// writers reject the value by name, and the heartbeat writes nothing at all.
func TestReachClassesAreValidatedOnRegisterAndHeartbeat(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()

	_, _, err := RegisterNode(ctx, db, clock, RegisterRequest{
		Name: "typo", Role: "executor", ReachClasses: []string{"host", "Host"},
	})
	if err == nil {
		t.Fatalf("registration accepted reach class %q", "Host")
	}
	if !strings.Contains(err.Error(), `"Host"`) {
		t.Fatalf("error %q does not name the offending reach class", err)
	}
	var nodes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if nodes != 0 {
		t.Fatalf("nodes = %d after a rejected registration, want 0", nodes)
	}

	// Accepted values are canonicalised: trimmed, de-duplicated, sorted.
	id, _, err := RegisterNode(ctx, db, clock, RegisterRequest{
		Name: "home-laptop", Role: "executor", ReachClasses: []string{" lan ", "host", "host"},
	})
	if err != nil {
		t.Fatalf("register with valid reach classes: %v", err)
	}
	var reach string
	if err := db.QueryRow(`SELECT reach_classes FROM nodes WHERE id=?`, id).Scan(&reach); err != nil {
		t.Fatalf("read reach: %v", err)
	}
	if reach != `["host","lan"]` {
		t.Fatalf("reach_classes = %s, want the canonical [\"host\",\"lan\"]", reach)
	}

	if _, err := Heartbeat(ctx, db, clock, id, HeartbeatRequest{
		ProvidesLive: []string{"agentic"}, ReachClasses: []string{"host"}, Protocol: ProtocolMax,
	}); err != nil {
		t.Fatalf("good heartbeat: %v", err)
	}

	_, err = Heartbeat(ctx, db, clock, id, HeartbeatRequest{
		ProvidesLive: []string{"agentic"}, ReachClasses: []string{"wan", "wfi"}, Protocol: ProtocolMax,
	})
	if err == nil {
		t.Fatalf("heartbeat accepted reach class %q", "wfi")
	}
	if !strings.Contains(err.Error(), `"wfi"`) {
		t.Fatalf("error %q does not name the offending reach class", err)
	}

	// Nothing was written by the refused heartbeat: the node keeps its last
	// good status, live set and reach set.
	var status, live string
	if err := db.QueryRow(`SELECT status, provides_live, reach_classes FROM nodes WHERE id=?`, id).
		Scan(&status, &live, &reach); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if status != "online" || live != `["agentic"]` || reach != `["host"]` {
		t.Fatalf("node = (status %q, live %s, reach %s), want the last good heartbeat kept", status, live, reach)
	}
}

// TestProtocolMismatchIsIncompatibleInBothDirections pins the symmetric cutover.
// Protocol 2 is the claim payload that carries the endpoint's construction
// config; a node that cannot build a provider must not claim work, in either
// direction. The old-node-new-board leg uses the real Heartbeat; the
// new-node-old-board leg uses heartbeatInRange with a protocol-1 board, which is
// the same code path with the range a previous release spoke.
func TestProtocolMismatchIsIncompatibleInBothDirections(t *testing.T) {
	setup := func(t *testing.T) (*sql.DB, *FixedClock, string) {
		t.Helper()
		db, clock := newTestDB(t)
		node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
		SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
		if _, err := CreateItem(context.Background(), db, clock, CreateItemRequest{
			Title: "t", Brief: "b", Needs: []string{"agentic"},
		}); err != nil {
			t.Fatalf("CreateItem: %v", err)
		}
		return db, clock, node
	}

	t.Run("an old node on a new board", func(t *testing.T) {
		db, clock, node := setup(t)
		resp, err := Heartbeat(context.Background(), db, clock, node, HeartbeatRequest{
			ProvidesLive: []string{"agentic"}, ReachClasses: []string{"host"}, Protocol: 1,
		})
		if err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		if resp.Status != "incompatible" {
			t.Fatalf("status = %q, want incompatible: a protocol-1 node cannot build a provider", resp.Status)
		}
		if resp.ProtocolMin != ProtocolMin || resp.ProtocolMax != ProtocolMax {
			t.Fatalf("response range = %d..%d, want the board's %d..%d", resp.ProtocolMin, resp.ProtocolMax, ProtocolMin, ProtocolMax)
		}
		assertIncompatibleClaimsNothing(t, db, clock, node)
	})

	t.Run("a new node on an old board", func(t *testing.T) {
		db, clock, node := setup(t)
		// The range a protocol-1 board speaks. The node is not at fault for
		// speaking a newer protocol; the board refuses it rather than handing it
		// work it cannot execute.
		resp, err := heartbeatInRange(context.Background(), db, clock, node, HeartbeatRequest{
			ProvidesLive: []string{"agentic"}, ReachClasses: []string{"host"}, Protocol: ProtocolMax,
		}, 1, 1)
		if err != nil {
			t.Fatalf("heartbeatInRange: %v", err)
		}
		if resp.Status != "incompatible" {
			t.Fatalf("status = %q, want incompatible", resp.Status)
		}
		if resp.ProtocolMin != 1 || resp.ProtocolMax != 1 {
			t.Fatalf("response range = %d..%d, want the old board's 1..1", resp.ProtocolMin, resp.ProtocolMax)
		}
		assertIncompatibleClaimsNothing(t, db, clock, node)
	})
}

func assertIncompatibleClaimsNothing(t *testing.T, db *sql.DB, clock *FixedClock, node string) {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM nodes WHERE id=?`, node).Scan(&status); err != nil {
		t.Fatalf("read node status: %v", err)
	}
	if status != "incompatible" {
		t.Fatalf("node status = %q, want incompatible", status)
	}
	got, err := Claim(context.Background(), db, clock, node, 1)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an incompatible node claimed %d items, want 0", len(got))
	}
}
