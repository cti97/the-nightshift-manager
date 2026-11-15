package board

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
		Protocol:     1,
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

	if _, err := Heartbeat(ctx, db, clock, a, HeartbeatRequest{Protocol: 1}); err != nil {
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
	if live != `["agentic"]` {
		t.Fatalf("provides_live = %s, want it untouched at the declared set", live)
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
		ProvidesLive: []string{"agentic"}, Protocol: 1,
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
		ProvidesLive: []string{"agentic"}, Protocol: 1,
	}); err != nil {
		t.Fatalf("good heartbeat: %v", err)
	}

	_, err = Heartbeat(ctx, db, clock, id, HeartbeatRequest{
		ProvidesLive: []string{"lang:nas"}, Protocol: 1,
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

	rec := postJSON(t, h, "/nodes/register", "", RegisterRequest{
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
