package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cti97/the-nightshift-manager/internal/provider"
)

// The endpoint/model CRUD tests. Two properties run through all of them:
//
//   - every write is validated BEFORE it touches the store, so a rejection
//     leaves no row, no event and no partial state behind (the board's writers
//     are all-or-nothing);
//   - the provider vocabulary comes from the registry, not from a list in this
//     package, so a fourth adapter is usable the day it is registered.

func testRegistry() *provider.Registry { return provider.NewRegistry() }

// countRows counts the rows of a table. The table name is always a literal in
// these tests; nothing here takes it from a request.
func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func mustCreateEndpoint(t *testing.T, db *sql.DB, clock *FixedClock, reg *provider.Registry, req CreateEndpointRequest) EndpointView {
	t.Helper()
	ep, err := CreateEndpoint(context.Background(), db, clock, reg, req)
	if err != nil {
		t.Fatalf("CreateEndpoint(%q): %v", req.Name, err)
	}
	return ep
}

func mustCreateModel(t *testing.T, db *sql.DB, clock *FixedClock, req CreateModelRequest) ModelView {
	t.Helper()
	m, err := CreateModel(context.Background(), db, clock, req)
	if err != nil {
		t.Fatalf("CreateModel(%q): %v", req.ModelID, err)
	}
	return m
}

// endpointFixture creates a healthy-enough endpoint to hang models on: the
// create path always lands on unknown, and the health flip stands in for the
// probe Task 9 owns.
func endpointFixture(t *testing.T, db *sql.DB, clock *FixedClock) EndpointView {
	t.Helper()
	ep := mustCreateEndpoint(t, db, clock, testRegistry(), CreateEndpointRequest{
		Name: "pi-ollama", Provider: "ollama", BaseURL: "http://192.168.1.9:11434", Reach: "lan",
	})
	if _, err := db.Exec(`UPDATE endpoints SET health='ok' WHERE id=?`, ep.ID); err != nil {
		t.Fatalf("mark endpoint ok: %v", err)
	}
	return ep
}

func TestCreateEndpointValidatesReach(t *testing.T) {
	cases := []struct {
		reach  string
		ok     bool
		stored string
	}{
		{"host", true, "host"},
		{"lan", true, "lan"},
		{"wan", true, "wan"},
		// Trimmed and canonicalised, like every other vocabulary in the board.
		{" lan ", true, "lan"},
		// A value outside the vocabulary stores fine in SQLite (there is a CHECK,
		// but no CHECK for the case the operator actually types) and then never
		// places: the row exists, the work does not run, and nothing says why.
		{"wifi", false, ""},
		{"Host", false, ""},
		{"", false, ""},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("reach=%q", tc.reach), func(t *testing.T) {
			db, clock := newTestDB(t)
			ep, err := CreateEndpoint(context.Background(), db, clock, testRegistry(), CreateEndpointRequest{
				Name: "ep", Provider: "openai", BaseURL: "http://127.0.0.1:8080", Reach: tc.reach,
			})
			if tc.ok {
				if err != nil {
					t.Fatalf("CreateEndpoint(reach=%q): %v", tc.reach, err)
				}
				var stored string
				if err := db.QueryRow(`SELECT reach FROM endpoints WHERE id=?`, ep.ID).Scan(&stored); err != nil {
					t.Fatalf("read reach: %v", err)
				}
				if stored != tc.stored {
					t.Fatalf("stored reach = %q, want %q", stored, tc.stored)
				}
				return
			}
			if err == nil {
				t.Fatalf("CreateEndpoint(reach=%q) = %+v, want an error", tc.reach, ep)
			}
			if !strings.Contains(err.Error(), "reach") {
				t.Fatalf("error = %q, want it to name the reach class", err)
			}
			if n := countRows(t, db, "endpoints"); n != 0 {
				t.Fatalf("%d endpoint rows after a rejected reach, want 0", n)
			}
			if n := countRows(t, db, "events"); n != 0 {
				t.Fatalf("%d event rows after a rejected reach, want 0", n)
			}
		})
	}
}

// TestCreateEndpointAcceptsEveryRegisteredProvider is the anti-drift guard: the
// set of accepted provider names is exactly the set the registry holds. A new
// adapter needs one Register line and no edit to the board.
func TestCreateEndpointAcceptsEveryRegisteredProvider(t *testing.T) {
	reg := testRegistry()
	names := reg.Names()
	if len(names) == 0 {
		t.Fatal("the registry is empty; the test would pass vacuously")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			db, clock := newTestDB(t)
			ep := mustCreateEndpoint(t, db, clock, reg, CreateEndpointRequest{
				Name: "ep-" + name, Provider: name, BaseURL: "http://127.0.0.1:1", Reach: "host",
			})
			if ep.Provider != name {
				t.Fatalf("provider = %q, want %q", ep.Provider, name)
			}
		})
	}
}

func TestCreateEndpointRejectsUnknownProvider(t *testing.T) {
	reg := testRegistry()
	for _, bad := range []string{"opnai", "OpenAI", " openai", "openai ", ""} {
		t.Run(fmt.Sprintf("provider=%q", bad), func(t *testing.T) {
			db, clock := newTestDB(t)
			_, err := CreateEndpoint(context.Background(), db, clock, reg, CreateEndpointRequest{
				Name: "ep", Provider: bad, BaseURL: "http://127.0.0.1:1", Reach: "host",
			})
			if err == nil {
				t.Fatalf("CreateEndpoint(provider=%q) accepted a name the registry does not hold", bad)
			}
			// The message must list what IS registered: a typo whose error only
			// says "unknown provider" makes the operator guess.
			if !strings.Contains(err.Error(), namesOrNone(reg.Names())) {
				t.Fatalf("error = %q, want the registered names %v", err, reg.Names())
			}
			if n := countRows(t, db, "endpoints"); n != 0 {
				t.Fatalf("%d endpoint rows after a rejected provider, want 0", n)
			}
		})
	}
}

// TestCreateEndpointUsesAProviderRegisteredAtRuntime proves the vocabulary is
// read from the registry rather than from a package-level list: a name that does
// not exist in this binary's registry can still be written against a registry
// that holds it, and the same name is refused by the shipped one.
func TestCreateEndpointUsesAProviderRegisteredAtRuntime(t *testing.T) {
	future := &provider.Registry{}
	future.Register("futureproto", func(provider.Config) (provider.Provider, error) {
		return provider.NewStub(nil), nil
	})

	db, clock := newTestDB(t)
	if _, err := CreateEndpoint(context.Background(), db, clock, future, CreateEndpointRequest{
		Name: "future", Provider: "futureproto", BaseURL: "http://127.0.0.1:1", Reach: "host",
	}); err != nil {
		t.Fatalf("CreateEndpoint(futureproto): %v", err)
	}

	if _, err := CreateEndpoint(context.Background(), db, clock, testRegistry(), CreateEndpointRequest{
		Name: "future", Provider: "futureproto", BaseURL: "http://127.0.0.1:1", Reach: "host",
	}); err == nil {
		t.Fatal("the shipped registry accepted futureproto; the test proves nothing about the source of truth")
	}
}

// TestCreateEndpointFailsClosedWithoutARegistry: a nil registry authenticates
// nothing. It must reject rather than panic, exactly like enrollAuth does with
// an empty enrollment hash.
func TestCreateEndpointFailsClosedWithoutARegistry(t *testing.T) {
	db, clock := newTestDB(t)
	_, err := CreateEndpoint(context.Background(), db, clock, nil, CreateEndpointRequest{
		Name: "ep", Provider: "openai", BaseURL: "http://127.0.0.1:1", Reach: "host",
	})
	if err == nil {
		t.Fatal("a nil registry accepted an endpoint")
	}
	if countRows(t, db, "endpoints") != 0 {
		t.Fatal("a nil registry wrote an endpoint row")
	}
}

func TestCreateEndpointRejectsBlankFields(t *testing.T) {
	cases := map[string]CreateEndpointRequest{
		"no name":     {Provider: "openai", BaseURL: "http://127.0.0.1:1", Reach: "host"},
		"blank name":  {Name: "  ", Provider: "openai", BaseURL: "http://127.0.0.1:1", Reach: "host"},
		"no base_url": {Name: "ep", Provider: "openai", Reach: "host"},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			db, clock := newTestDB(t)
			if _, err := CreateEndpoint(context.Background(), db, clock, testRegistry(), req); err == nil {
				t.Fatalf("CreateEndpoint(%+v) accepted an incomplete endpoint", req)
			}
			if n := countRows(t, db, "endpoints"); n != 0 {
				t.Fatalf("%d endpoint rows, want 0", n)
			}
		})
	}
}

// TestCreateEndpointValidatesBaseURL covers the one operator-typed field a
// syntax check can refuse. It is also the field a typo would make PERMANENT:
// there is no endpoint PATCH or DELETE, so a URL stored blind can only be
// corrected with raw SQL. The rule is not restated here — it is the adapters'
// own (provider.CheckBaseURL), shared with normalizeAPIBaseURL.
func TestCreateEndpointValidatesBaseURL(t *testing.T) {
	bad := []string{
		"not a url",
		"127.0.0.1:8080", // no scheme: parses as a path containing a colon
		"pi.local:11434", // scheme "pi.local", which no adapter speaks
		"ftp://example.com",
		"http://", // no host
		"http://example.com/p?x=1",
		"http://example.com/p#frag",
	}
	for _, raw := range bad {
		t.Run("refuse/"+raw, func(t *testing.T) {
			db, clock := newTestDB(t)
			_, err := CreateEndpoint(context.Background(), db, clock, testRegistry(), CreateEndpointRequest{
				Name: "ep", Provider: "openai", BaseURL: raw, Reach: "lan",
			})
			if err == nil {
				t.Fatalf("CreateEndpoint accepted base_url %q, which no adapter can construct", raw)
			}
			if !strings.Contains(err.Error(), "base URL") {
				t.Fatalf("error = %q, want it to name the base URL", err)
			}
			if n := countRows(t, db, "endpoints"); n != 0 {
				t.Fatalf("%d endpoint rows after a rejected base_url, want 0", n)
			}
			if n := countRows(t, db, "events"); n != 0 {
				t.Fatalf("%d events after a rejected base_url, want 0", n)
			}
		})
	}

	good := map[string]string{
		"http://127.0.0.1:8080":         "http://127.0.0.1:8080",
		" https://api.example.com/v1 ":  "https://api.example.com/v1",
		"https://proxy.example/openai/": "https://proxy.example/openai/",
	}
	for raw, want := range good {
		t.Run("accept/"+raw, func(t *testing.T) {
			db, clock := newTestDB(t)
			ep := mustCreateEndpoint(t, db, clock, testRegistry(), CreateEndpointRequest{
				Name: "ep", Provider: "openai", BaseURL: raw, Reach: "lan",
			})
			if ep.BaseURL != want {
				t.Fatalf("stored base_url = %q, want %q (trimmed, not rewritten)", ep.BaseURL, want)
			}
		})
	}
}

func TestCreateEndpointRejectsDuplicateName(t *testing.T) {
	db, clock := newTestDB(t)
	reg := testRegistry()
	mustCreateEndpoint(t, db, clock, reg, CreateEndpointRequest{
		Name: "pi", Provider: "ollama", BaseURL: "http://127.0.0.1:1", Reach: "lan",
	})
	_, err := CreateEndpoint(context.Background(), db, clock, reg, CreateEndpointRequest{
		Name: "pi", Provider: "ollama", BaseURL: "http://127.0.0.1:2", Reach: "lan",
	})
	if err == nil {
		t.Fatal("a duplicate endpoint name was accepted")
	}
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("error = %v, want ErrAlreadyExists so the handler can answer 409", err)
	}
	if n := countRows(t, db, "endpoints"); n != 1 {
		t.Fatalf("%d endpoints, want the original 1", n)
	}
	if n := countRows(t, db, "events"); n != 1 {
		t.Fatalf("%d events, want only the original create's 1", n)
	}
}

// TestCreatedEndpointStartsUnknownAndUnplaceable pins the judgement call that a
// new endpoint has never been probed, so it is inert until a probe says
// otherwise: placement requires health='ok', and creation is not a probe.
func TestCreatedEndpointStartsUnknownAndUnplaceable(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()

	ep := mustCreateEndpoint(t, db, clock, testRegistry(), CreateEndpointRequest{
		Name: "pi-llamacpp", Provider: "openai", BaseURL: "http://192.168.1.9:8080", Reach: "lan",
	})
	if ep.Health != "unknown" {
		t.Fatalf("health = %q, want unknown", ep.Health)
	}
	if ep.HealthReason != "never probed" {
		t.Fatalf("health_reason = %q, want %q", ep.HealthReason, "never probed")
	}

	var lastOK, latency sql.NullInt64
	if err := db.QueryRow(`SELECT last_ok_at, latency_ms FROM endpoints WHERE id=?`, ep.ID).
		Scan(&lastOK, &latency); err != nil {
		t.Fatalf("read endpoint timestamps: %v", err)
	}
	if lastOK.Valid || latency.Valid {
		t.Fatalf("last_ok_at=%v latency_ms=%v, want both NULL on a never-probed endpoint", lastOK, latency)
	}

	mustCreateModel(t, db, clock, CreateModelRequest{
		EndpointID: ep.ID, ModelID: "qwen2.5:3b", Features: []string{"tools"},
	})
	SeedNodeForTest(t, db, clock, "laptop", "executor", "agentic")

	cands, err := LoadCandidates(ctx, db)
	if err != nil {
		t.Fatalf("LoadCandidates: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("LoadCandidates returned %d candidates for a never-probed endpoint, want 0: %+v", len(cands), cands)
	}
	// The row is still there for /why to name, which is the whole reason the
	// wide loader exists.
	all, err := LoadAllCandidates(ctx, db)
	if err != nil {
		t.Fatalf("LoadAllCandidates: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("LoadAllCandidates returned %d rows, want 1", len(all))
	}
	ok, why := Eligible(WorkItem{Needs: []string{"agentic"}}, all[0])
	if ok {
		t.Fatal("Eligible accepted an unprobed endpoint")
	}
	if !strings.Contains(why, "unknown") {
		t.Fatalf("Eligible reason = %q, want it to name the unknown health", why)
	}

	// This is what Task 9's probe will do. After it, the same rows place.
	if _, err := db.Exec(`UPDATE endpoints SET health='ok', health_reason=NULL WHERE id=?`, ep.ID); err != nil {
		t.Fatalf("simulate probe: %v", err)
	}
	cands, err = LoadCandidates(ctx, db)
	if err != nil {
		t.Fatalf("LoadCandidates after probe: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("LoadCandidates returned %d candidates after the probe, want 1", len(cands))
	}
	d := Place(WorkItem{ID: "w1", Needs: []string{"agentic"}}, cands)
	if !d.OK {
		t.Fatalf("Place refused the freshly created rows: %s %+v", d.Reason, d.Rejects)
	}
}

// TestEndpointCreateRollsBackWhenItsEventFails proves the audit row and the
// state change are the SAME write, not two writes that usually both happen:
// with the events table gone the create must fail, and the row it had already
// inserted must be gone with it.
func TestEndpointCreateRollsBackWhenItsEventFails(t *testing.T) {
	t.Run("endpoint", func(t *testing.T) {
		db, clock := newTestDB(t)
		if _, err := db.Exec(`DROP TABLE events`); err != nil {
			t.Fatalf("drop events: %v", err)
		}
		_, err := CreateEndpoint(context.Background(), db, clock, testRegistry(), CreateEndpointRequest{
			Name: "pi", Provider: "ollama", BaseURL: "http://127.0.0.1:1", Reach: "lan",
		})
		if err == nil {
			t.Fatal("the create succeeded with no events table")
		}
		if n := countRows(t, db, "endpoints"); n != 0 {
			t.Fatalf("%d endpoint rows survived a failed event write: insert and event are not one transaction", n)
		}
	})
	t.Run("model", func(t *testing.T) {
		db, clock := newTestDB(t)
		ep := mustCreateEndpoint(t, db, clock, testRegistry(), CreateEndpointRequest{
			Name: "pi", Provider: "ollama", BaseURL: "http://127.0.0.1:1", Reach: "lan",
		})
		if _, err := db.Exec(`DROP TABLE events`); err != nil {
			t.Fatalf("drop events: %v", err)
		}
		if _, err := CreateModel(context.Background(), db, clock, CreateModelRequest{
			EndpointID: ep.ID, ModelID: "m1",
		}); err == nil {
			t.Fatal("the create succeeded with no events table")
		}
		if n := countRows(t, db, "models"); n != 0 {
			t.Fatalf("%d model rows survived a failed event write: insert and event are not one transaction", n)
		}
	})
}

// TestCreateEndpointWritesOneEventInTheSameTransaction: every state change
// carries its audit row, and a rejected write carries none.
func TestCreateEndpointWritesOneEventInTheSameTransaction(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	ep := mustCreateEndpoint(t, db, clock, testRegistry(), CreateEndpointRequest{
		Name: "pi", Provider: "ollama", BaseURL: "http://127.0.0.1:11434", Reach: "lan",
	})

	events, err := ListEvents(ctx, db, "endpoint", ep.ID, 10)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events for the endpoint, want 1", len(events))
	}
	if events[0].Type != "created" {
		t.Fatalf("event type = %q, want created", events[0].Type)
	}
	if events[0].Actor != "admin" {
		t.Fatalf("event actor = %q, want admin", events[0].Actor)
	}
	for _, want := range []string{"pi", "ollama", "lan"} {
		if !strings.Contains(events[0].Payload, want) {
			t.Fatalf("event payload = %s, want it to carry %q", events[0].Payload, want)
		}
	}
}

func TestCreateModelValidatesFeatures(t *testing.T) {
	db, clock := newTestDB(t)
	ep := endpointFixture(t, db, clock)

	all := mustCreateModel(t, db, clock, CreateModelRequest{
		EndpointID: ep.ID, ModelID: "qwen2.5:3b",
		// Deliberately unsorted and duplicated: the stored bytes must depend on
		// the set, not on the order the operator typed.
		Features: []string{"streaming", "tools", "vision", "json_schema", "tools"},
	})
	if got, want := strings.Join(all.Features, ","), "json_schema,streaming,tools,vision"; got != want {
		t.Fatalf("stored features = %q, want %q", got, want)
	}

	for _, bad := range []string{"tool", "Tools", "json-schema", ""} {
		t.Run(fmt.Sprintf("feature=%q", bad), func(t *testing.T) {
			before := countRows(t, db, "models")
			_, err := CreateModel(context.Background(), db, clock, CreateModelRequest{
				EndpointID: ep.ID, ModelID: "m-" + bad, Features: []string{"tools", bad},
			})
			if err == nil {
				t.Fatalf("CreateModel(features=%q) accepted a feature outside the vocabulary", bad)
			}
			if !strings.Contains(err.Error(), "feature") {
				t.Fatalf("error = %q, want it to name the feature", err)
			}
			if after := countRows(t, db, "models"); after != before {
				t.Fatalf("model rows %d -> %d after a rejected feature, want no change", before, after)
			}
		})
	}
}

// TestCreateModelValidatesProvides: models.provides feeds placement exactly like
// nodes.provides, so it follows the same capability vocabulary — a typo would
// otherwise be a model that never serves the thing it was bought for.
func TestCreateModelValidatesProvides(t *testing.T) {
	db, clock := newTestDB(t)
	ep := endpointFixture(t, db, clock)

	m := mustCreateModel(t, db, clock, CreateModelRequest{
		EndpointID: ep.ID, ModelID: "m1", Provides: []string{"vision", "agentic"},
	})
	if got, want := strings.Join(m.Provides, ","), "agentic,vision"; got != want {
		t.Fatalf("stored provides = %q, want %q", got, want)
	}

	if _, err := CreateModel(context.Background(), db, clock, CreateModelRequest{
		EndpointID: ep.ID, ModelID: "m2", Provides: []string{"agentik"},
	}); err == nil {
		t.Fatal("CreateModel accepted a capability outside the vocabulary")
	}
	if n := countRows(t, db, "models"); n != 1 {
		t.Fatalf("%d models, want 1", n)
	}
}

func TestCreateModelRejectsUnknownEndpoint(t *testing.T) {
	db, clock := newTestDB(t)
	_, err := CreateModel(context.Background(), db, clock, CreateModelRequest{
		EndpointID: "no-such-endpoint", ModelID: "m1",
	})
	if err == nil {
		t.Fatal("CreateModel accepted a model on an endpoint that does not exist")
	}
	if !strings.Contains(err.Error(), "endpoint") {
		t.Fatalf("error = %q, want it to name the endpoint", err)
	}
	if n := countRows(t, db, "models"); n != 0 {
		t.Fatalf("%d model rows, want 0", n)
	}
}

func TestCreateModelRejectsNegativeNumbers(t *testing.T) {
	db, clock := newTestDB(t)
	ep := endpointFixture(t, db, clock)

	cases := map[string]CreateModelRequest{
		"negative context": {EndpointID: ep.ID, ModelID: "m1", ContextWindow: -1},
		"negative in":      {EndpointID: ep.ID, ModelID: "m1", CostInPerMtok: -0.01},
		"negative out":     {EndpointID: ep.ID, ModelID: "m1", CostOutPerMtok: -0.01},
		"no model_id":      {EndpointID: ep.ID},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := CreateModel(context.Background(), db, clock, req); err == nil {
				t.Fatalf("CreateModel(%+v) accepted a negative or missing value", req)
			}
		})
	}
	if n := countRows(t, db, "models"); n != 0 {
		t.Fatalf("%d model rows, want 0", n)
	}
}

func TestCreateModelRejectsNonFiniteNumbers(t *testing.T) {
	// The reachable half of this class is the INFINITE price. The old "< 0"
	// check caught -Inf by accident, and SQLite refuses a NaN bind into this
	// REAL NOT NULL column outright, so those legs do not exercise the
	// finite-number rule: NaN still fails at the column when only that rule is
	// removed, and -Inf is refused by the negative rule either way. +Inf is the
	// value that was silently stored: it is neither negative nor refused by the
	// driver, and every later ledger comparison treats it as unbounded. The
	// +Inf legs are therefore what give this test teeth against the finite
	// rule; the NaN and -Inf legs stay because the contract refuses them too,
	// and a narrower test would let a later rewrite drop them.
	db, clock := newTestDB(t)
	ep := endpointFixture(t, db, clock)

	cases := map[string]CreateModelRequest{
		"NaN input price":   {EndpointID: ep.ID, ModelID: "m1", CostInPerMtok: math.NaN()},
		"NaN output price":  {EndpointID: ep.ID, ModelID: "m2", CostOutPerMtok: math.NaN()},
		"+Inf input price":  {EndpointID: ep.ID, ModelID: "m3", CostInPerMtok: math.Inf(1)},
		"+Inf output price": {EndpointID: ep.ID, ModelID: "m4", CostOutPerMtok: math.Inf(1)},
		"-Inf output price": {EndpointID: ep.ID, ModelID: "m5", CostOutPerMtok: math.Inf(-1)},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := CreateModel(context.Background(), db, clock, req); err == nil {
				t.Fatalf("CreateModel(%+v) accepted a non-finite price", req)
			}
		})
	}
	if n := countRows(t, db, "models"); n != 0 {
		t.Fatalf("%d model rows, want 0: a refused price must not be stored", n)
	}
}

func TestCreateModelRejectsDuplicateOnTheSameEndpoint(t *testing.T) {
	db, clock := newTestDB(t)
	ep := endpointFixture(t, db, clock)
	other := mustCreateEndpoint(t, db, clock, testRegistry(), CreateEndpointRequest{
		Name: "other", Provider: "openai", BaseURL: "http://127.0.0.1:2", Reach: "host",
	})

	mustCreateModel(t, db, clock, CreateModelRequest{EndpointID: ep.ID, ModelID: "qwen"})
	// The same model id on a different endpoint is a different row: one model
	// served by two boxes is a legitimate configuration.
	mustCreateModel(t, db, clock, CreateModelRequest{EndpointID: other.ID, ModelID: "qwen"})

	_, err := CreateModel(context.Background(), db, clock, CreateModelRequest{EndpointID: ep.ID, ModelID: "qwen"})
	if err == nil {
		t.Fatal("a duplicate (endpoint_id, model_id) was accepted")
	}
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("error = %v, want ErrAlreadyExists", err)
	}
	if n := countRows(t, db, "models"); n != 2 {
		t.Fatalf("%d models, want 2", n)
	}
}

func TestCreateModelStartsEnabledAndWritesOneEvent(t *testing.T) {
	db, clock := newTestDB(t)
	ep := endpointFixture(t, db, clock)
	m := mustCreateModel(t, db, clock, CreateModelRequest{
		EndpointID: ep.ID, ModelID: "qwen2.5:3b", Features: []string{"tools"},
	})
	if !m.Enabled {
		t.Fatal("a newly created model is disabled; enabled must default to true")
	}
	events, err := ListEvents(context.Background(), db, "model", m.ID, 10)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 || events[0].Type != "created" {
		t.Fatalf("model events = %+v, want exactly one created", events)
	}
	off := false
	mustCreateModel(t, db, clock, CreateModelRequest{EndpointID: ep.ID, ModelID: "m-off", Enabled: &off})
	if m := mustGetModel(t, db, "m-off"); m.Enabled {
		t.Fatal("enabled=false on create was ignored")
	}
}

// mustGetModel reads a model row by model_id, for assertions that a PATCH did or
// did not take effect.
func mustGetModel(t *testing.T, db *sql.DB, modelID string) ModelView {
	t.Helper()
	eps, err := ListEndpoints(context.Background(), db)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	for _, ep := range eps {
		for _, m := range ep.Models {
			if m.ModelID == modelID {
				return m
			}
		}
	}
	t.Fatalf("no model %q in the listing", modelID)
	return ModelView{}
}

// TestPatchModelValidatesFeaturesOnTheWritePath: PATCH is a write path, so it
// validates exactly like create. A rejected PATCH must leave every column as it
// was, not just the one it was editing.
func TestPatchModelValidatesFeaturesOnTheWritePath(t *testing.T) {
	db, clock := newTestDB(t)
	ep := endpointFixture(t, db, clock)
	m := mustCreateModel(t, db, clock, CreateModelRequest{
		EndpointID: ep.ID, ModelID: "m1", Features: []string{"tools"}, CostInPerMtok: 3,
	})

	if _, err := PatchModel(context.Background(), db, clock, m.ID, PatchModelRequest{
		Features: &[]string{"tool"}, Enabled: boolPtr(false),
	}); err == nil {
		t.Fatal("PATCH accepted a feature outside the vocabulary")
	}
	after := mustGetModel(t, db, "m1")
	if strings.Join(after.Features, ",") != "tools" {
		t.Fatalf("features = %v after a rejected PATCH, want [tools]", after.Features)
	}
	if !after.Enabled {
		t.Fatal("enabled changed after a rejected PATCH; the whole write must be refused")
	}
	if n := countRows(t, db, "events"); n != 2 { // one endpoint + one model, both "created"
		t.Fatalf("%d events after a rejected PATCH, want 2", n)
	}
}

func TestPatchModelDisablesAndReenables(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	ep := endpointFixture(t, db, clock)
	m := mustCreateModel(t, db, clock, CreateModelRequest{
		EndpointID: ep.ID, ModelID: "qwen2.5:3b", Features: []string{"tools"},
	})
	SeedNodeForTest(t, db, clock, "laptop", "executor", "agentic")

	if _, err := PatchModel(ctx, db, clock, m.ID, PatchModelRequest{Enabled: boolPtr(false)}); err != nil {
		t.Fatalf("PatchModel(disable): %v", err)
	}

	off := mustGetModel(t, db, "qwen2.5:3b")
	if off.Enabled {
		t.Fatal("enabled = true after disabling")
	}
	cands, err := LoadCandidates(ctx, db)
	if err != nil {
		t.Fatalf("LoadCandidates: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("LoadCandidates returned %d rows for a disabled model, want 0", len(cands))
	}
	all, err := LoadAllCandidates(ctx, db)
	if err != nil {
		t.Fatalf("LoadAllCandidates: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("LoadAllCandidates returned %d rows, want the disabled one", len(all))
	}
	if ok, why := Eligible(WorkItem{Needs: []string{"agentic"}}, all[0]); ok || !strings.Contains(why, "disabled") {
		t.Fatalf("Eligible(disabled) = %v %q, want refused and naming the disabled model", ok, why)
	}

	if _, err := PatchModel(ctx, db, clock, m.ID, PatchModelRequest{Enabled: boolPtr(true)}); err != nil {
		t.Fatalf("PatchModel(enable): %v", err)
	}
	cands, err = LoadCandidates(ctx, db)
	if err != nil {
		t.Fatalf("LoadCandidates after enabling: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("LoadCandidates returned %d rows after enabling, want 1", len(cands))
	}

	events, err := ListEvents(ctx, db, "model", m.ID, 10)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 3 { // created, disabled, enabled
		t.Fatalf("got %d model events, want 3: %+v", len(events), events)
	}
	for _, e := range events[1:] {
		if e.Type != "updated" {
			t.Fatalf("event type = %q, want updated", e.Type)
		}
	}
}

func TestPatchModelUpdatesCostAndContextWindow(t *testing.T) {
	db, clock := newTestDB(t)
	ep := endpointFixture(t, db, clock)
	m := mustCreateModel(t, db, clock, CreateModelRequest{
		EndpointID: ep.ID, ModelID: "m1", CostInPerMtok: 1, ContextWindow: 1000,
	})

	patched, err := PatchModel(context.Background(), db, clock, m.ID, PatchModelRequest{
		ModelID: strPtr("m1-renamed"), CostInPerMtok: floatPtr(0), ContextWindow: intPtr(200000),
	})
	if err != nil {
		t.Fatalf("PatchModel: %v", err)
	}
	if patched.ModelID != "m1-renamed" {
		t.Fatalf("model_id = %q, want the rename", patched.ModelID)
	}
	// Zero must be settable: a free model and an unknown window are 0, and a
	// partial update cannot tell "absent" from "zero" without pointers.
	if patched.CostInPerMtok != 0 {
		t.Fatalf("cost_in_per_mtok = %v, want 0", patched.CostInPerMtok)
	}
	if patched.ContextWindow != 200000 {
		t.Fatalf("context_window = %d, want 200000", patched.ContextWindow)
	}
}

func TestPatchModelRejectsUnknownIDAndEmptyBody(t *testing.T) {
	db, clock := newTestDB(t)
	ep := endpointFixture(t, db, clock)
	mustCreateModel(t, db, clock, CreateModelRequest{EndpointID: ep.ID, ModelID: "m1"})

	_, err := PatchModel(context.Background(), db, clock, "nope", PatchModelRequest{Enabled: boolPtr(false)})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("PatchModel(unknown) = %v, want a wrapped sql.ErrNoRows so the handler can answer 404", err)
	}

	if _, err := PatchModel(context.Background(), db, clock, mustGetModel(t, db, "m1").ID, PatchModelRequest{}); err == nil {
		t.Fatal("PatchModel with no fields was accepted")
	}
}

// TestPatchModelRefusesAPatchThatChangesNothing: every field is provided and
// every value equals what is stored. A 200 plus an "updated" event would be a
// claim that something happened, and the audit spine would be lying.
func TestPatchModelRefusesAPatchThatChangesNothing(t *testing.T) {
	db, clock := newTestDB(t)
	ep := endpointFixture(t, db, clock)
	m := mustCreateModel(t, db, clock, CreateModelRequest{
		EndpointID: ep.ID, ModelID: "m1", Features: []string{"tools"}, CostInPerMtok: 3,
	})
	before := countRows(t, db, "events")

	_, err := PatchModel(context.Background(), db, clock, m.ID, PatchModelRequest{
		Features: &[]string{"tools"}, CostInPerMtok: floatPtr(3), Enabled: boolPtr(true),
	})
	if err == nil {
		t.Fatal("a PATCH that changes nothing was accepted and would write an updated event")
	}
	if after := mustGetModel(t, db, "m1"); after.CostInPerMtok != 3 {
		t.Fatalf("cost = %v, want the stored 3", after.CostInPerMtok)
	}
	if n := countRows(t, db, "events"); n != before {
		t.Fatalf("%d events after a no-op PATCH, want the original %d", n, before)
	}
}

func TestPatchModelRejectsNegativeNumbers(t *testing.T) {
	db, clock := newTestDB(t)
	ep := endpointFixture(t, db, clock)
	m := mustCreateModel(t, db, clock, CreateModelRequest{EndpointID: ep.ID, ModelID: "m1", CostInPerMtok: 1})

	if _, err := PatchModel(context.Background(), db, clock, m.ID, PatchModelRequest{CostInPerMtok: floatPtr(-2)}); err == nil {
		t.Fatal("PATCH accepted a negative cost; placement ranks by it, so it would win every auction")
	}
	if after := mustGetModel(t, db, "m1"); after.CostInPerMtok != 1 {
		t.Fatalf("cost = %v after a rejected PATCH, want 1", after.CostInPerMtok)
	}
}

func TestPatchModelRejectsNonFiniteNumbers(t *testing.T) {
	// The PATCH path validates inline rather than through checkModelNumbers, so
	// it needs the same finite rule. As above, +Inf is the value that gives this
	// test teeth against that rule: a NaN bind is refused by the NOT NULL column
	// and -Inf by the negative rule, while +Inf was accepted and stored.
	db, clock := newTestDB(t)
	ep := endpointFixture(t, db, clock)
	m := mustCreateModel(t, db, clock, CreateModelRequest{
		EndpointID: ep.ID, ModelID: "m1", CostInPerMtok: 1, CostOutPerMtok: 2,
	})

	for _, tc := range []struct {
		name string
		req  PatchModelRequest
	}{
		{"NaN input price", PatchModelRequest{CostInPerMtok: floatPtr(math.NaN())}},
		{"NaN output price", PatchModelRequest{CostOutPerMtok: floatPtr(math.NaN())}},
		{"+Inf input price", PatchModelRequest{CostInPerMtok: floatPtr(math.Inf(1))}},
		{"+Inf output price", PatchModelRequest{CostOutPerMtok: floatPtr(math.Inf(1))}},
		{"-Inf output price", PatchModelRequest{CostOutPerMtok: floatPtr(math.Inf(-1))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PatchModel(context.Background(), db, clock, m.ID, tc.req); err == nil {
				t.Fatalf("PATCH(%+v) accepted a non-finite price", tc.req)
			}
		})
	}
	after := mustGetModel(t, db, "m1")
	if after.CostInPerMtok != 1 || after.CostOutPerMtok != 2 {
		t.Fatalf("stored prices = %v/%v after rejected PATCHes, want 1/2", after.CostInPerMtok, after.CostOutPerMtok)
	}
}

func TestListEndpointsReturnsModelsAndHealth(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()

	if eps, err := ListEndpoints(ctx, db); err != nil || len(eps) != 0 {
		t.Fatalf("ListEndpoints(empty) = %v, %v; want an empty slice and no error", eps, err)
	}

	ep := endpointFixture(t, db, clock)
	mustCreateModel(t, db, clock, CreateModelRequest{
		EndpointID: ep.ID, ModelID: "b-model", Provides: []string{"agentic"},
		Features: []string{"tools"}, ContextWindow: 32768, CostInPerMtok: 0.4, CostOutPerMtok: 1.6,
	})
	off := false
	mustCreateModel(t, db, clock, CreateModelRequest{
		EndpointID: ep.ID, ModelID: "a-model", Enabled: &off, CostInPerMtok: 9,
	})

	eps, err := ListEndpoints(ctx, db)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(eps))
	}
	got := eps[0]
	if got.Health != "ok" {
		t.Fatalf("health = %q, want ok", got.Health)
	}
	if got.Provider != "ollama" || got.Reach != "lan" || got.BaseURL == "" {
		t.Fatalf("endpoint = %+v, want provider/reach/base_url from the row", got)
	}
	if len(got.Models) != 2 {
		t.Fatalf("got %d models, want 2 (disabled ones are part of the operator's view)", len(got.Models))
	}
	// Ordered by model_id, so the view does not depend on insertion order.
	if got.Models[0].ModelID != "a-model" || got.Models[1].ModelID != "b-model" {
		t.Fatalf("models = %s, %s; want a-model, b-model", got.Models[0].ModelID, got.Models[1].ModelID)
	}
	if got.Models[0].Enabled {
		t.Fatal("disabled model reported enabled")
	}
	b := got.Models[1]
	if b.ContextWindow != 32768 || b.CostInPerMtok != 0.4 || b.CostOutPerMtok != 1.6 {
		t.Fatalf("model b = %+v, want the stored numbers", b)
	}
	if strings.Join(b.Features, ",") != "tools" || strings.Join(b.Provides, ",") != "agentic" {
		t.Fatalf("model b features/provides = %v/%v", b.Features, b.Provides)
	}
}

// TestPatchModelRenameCollisionIsRefused: model_id is the value a provider is
// called with, so it is editable — but (endpoint_id, model_id) is UNIQUE, and a
// rename onto a taken name must be refused without moving the row.
func TestPatchModelRenameCollisionIsRefused(t *testing.T) {
	db, clock := newTestDB(t)
	ep := endpointFixture(t, db, clock)
	mustCreateModel(t, db, clock, CreateModelRequest{EndpointID: ep.ID, ModelID: "a"})
	b := mustCreateModel(t, db, clock, CreateModelRequest{EndpointID: ep.ID, ModelID: "b"})
	before := countRows(t, db, "events")

	_, err := PatchModel(context.Background(), db, clock, b.ID, PatchModelRequest{ModelID: strPtr("a")})
	if err == nil {
		t.Fatal("PATCH renamed a model onto a name already taken on the same endpoint")
	}
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("error = %v, want ErrAlreadyExists so the handler can answer 409", err)
	}
	var got string
	if err := db.QueryRow(`SELECT model_id FROM models WHERE id=?`, b.ID).Scan(&got); err != nil {
		t.Fatalf("read model_id: %v", err)
	}
	if got != "b" {
		t.Fatalf("model_id = %q after a refused rename, want b", got)
	}
	if n := countRows(t, db, "events"); n != before {
		t.Fatalf("%d events after a refused rename, want the original %d", n, before)
	}
}

// TestPatchModelRollsBackWhenItsEventFails is PatchModel's half of the
// same-transaction guarantee the create test proves: the UPDATE and the event are
// one write, so a failed event leaves every column at its old value rather than
// changing the row without an audit trail.
func TestPatchModelRollsBackWhenItsEventFails(t *testing.T) {
	db, clock := newTestDB(t)
	ep := endpointFixture(t, db, clock)
	m := mustCreateModel(t, db, clock, CreateModelRequest{
		EndpointID: ep.ID, ModelID: "m1", CostInPerMtok: 1,
	})

	if _, err := db.Exec(`DROP TABLE events`); err != nil {
		t.Fatalf("drop events: %v", err)
	}
	if _, err := PatchModel(context.Background(), db, clock, m.ID, PatchModelRequest{
		CostInPerMtok: floatPtr(5), Enabled: boolPtr(false),
	}); err == nil {
		t.Fatal("the PATCH succeeded with no events table")
	}

	var cost float64
	var enabled int
	if err := db.QueryRow(`SELECT cost_in_per_mtok, enabled FROM models WHERE id=?`, m.ID).
		Scan(&cost, &enabled); err != nil {
		t.Fatalf("read model: %v", err)
	}
	if cost != 1 || enabled != 1 {
		t.Fatalf("cost=%v enabled=%d after a failed event write, want 1 and 1: the UPDATE is not in the event's transaction", cost, enabled)
	}
}

// --- HTTP surface -----------------------------------------------------------

// doJSON (the body-carrying request helper) already exists in report_test.go;
// the routes below use it rather than a second copy.

func TestAdminEndpointRoutesRequireTheAdminToken(t *testing.T) {
	srv, _ := newServer(t)
	h := srv.Handler()
	cases := []struct{ method, path string }{
		{"POST", "/admin/endpoints"},
		{"GET", "/admin/endpoints"},
		{"POST", "/admin/models"},
		{"PATCH", "/admin/models/x"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			for _, bearer := range []string{"", "wrong", testEnrollToken} {
				rec := doJSON(t, h, c.method, c.path, bearer, map[string]any{})
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("bearer %q: status = %d, want 401", bearer, rec.Code)
				}
				assertErrorCode(t, rec, "unauthorized")
			}
		})
	}
}

func TestCreateEndpointAndModelOverHTTP(t *testing.T) {
	srv, _ := newServer(t)
	h := srv.Handler()

	rec := doJSON(t, h, "POST", "/admin/endpoints", testAdminToken, map[string]any{
		"name": "pi-llamacpp", "provider": "openai", "base_url": "http://192.168.1.9:8080", "reach": "lan",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var ep EndpointView
	if err := json.Unmarshal(rec.Body.Bytes(), &ep); err != nil {
		t.Fatalf("decode endpoint: %v", err)
	}
	if ep.ID == "" || ep.Health != "unknown" {
		t.Fatalf("created endpoint = %+v, want an id and unknown health", ep)
	}

	rec = doJSON(t, h, "POST", "/admin/models", testAdminToken, map[string]any{
		"endpoint_id": ep.ID, "model_id": "qwen2.5:3b",
		"features": []string{"tools", "streaming"}, "context_window": 32768,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var m ModelView
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode model: %v", err)
	}
	if !m.Enabled || m.EndpointID != ep.ID {
		t.Fatalf("created model = %+v", m)
	}

	// The listing is the operator's view: the endpoint carries its models and
	// its current health, in the documented JSON spelling.
	rec = do(t, h, "GET", "/admin/endpoints", testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d (body %s)", rec.Code, rec.Body.String())
	}
	var listed []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d endpoints, want 1", len(listed))
	}
	for _, key := range []string{"id", "name", "provider", "base_url", "reach", "health", "models"} {
		if _, ok := listed[0][key]; !ok {
			t.Fatalf("endpoint JSON = %v, missing %q", listed[0], key)
		}
	}
	models, ok := listed[0]["models"].([]any)
	if !ok || len(models) != 1 {
		t.Fatalf("models = %v, want one model", listed[0]["models"])
	}

	// PATCH is the disable path; there is no delete route.
	rec = doJSON(t, h, "PATCH", "/admin/models/"+m.ID, testAdminToken, map[string]any{"enabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var patched ModelView
	if err := json.Unmarshal(rec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("decode patched: %v", err)
	}
	if patched.Enabled {
		t.Fatal("PATCH enabled=false left the model enabled")
	}
}

func TestCreateEndpointHTTPRejectsATypoWithTheErrorEnvelope(t *testing.T) {
	srv, _ := newServer(t)
	rec := doJSON(t, srv.Handler(), "POST", "/admin/endpoints", testAdminToken, map[string]any{
		"name": "ep", "provider": "opnai", "base_url": "http://127.0.0.1:1", "reach": "host",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "create_endpoint")
	if !strings.Contains(rec.Body.String(), "openai") {
		t.Fatalf("body = %s, want the registered names in the message", rec.Body.String())
	}
}

// TestDuplicateWritesOverHTTPAre409 pins the status the store's ErrAlreadyExists
// maps to. A duplicate is a well-formed value that is merely taken, and answering
// 400 would send the operator looking for a typo that is not there.
func TestDuplicateWritesOverHTTPAre409(t *testing.T) {
	srv, _ := newServer(t)
	h := srv.Handler()

	endpointBody := map[string]any{
		"name": "pi", "provider": "ollama", "base_url": "http://127.0.0.1:11434", "reach": "lan",
	}
	rec := doJSON(t, h, "POST", "/admin/endpoints", testAdminToken, endpointBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var ep EndpointView
	if err := json.Unmarshal(rec.Body.Bytes(), &ep); err != nil {
		t.Fatalf("decode endpoint: %v", err)
	}

	rec = doJSON(t, h, "POST", "/admin/endpoints", testAdminToken, endpointBody)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate endpoint = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "conflict")

	modelBody := map[string]any{"endpoint_id": ep.ID, "model_id": "qwen2.5:3b"}
	if rec := doJSON(t, h, "POST", "/admin/models", testAdminToken, modelBody); rec.Code != http.StatusCreated {
		t.Fatalf("first model create = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, "POST", "/admin/models", testAdminToken, modelBody)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate model = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "conflict")
}

func TestPatchUnknownModelOverHTTPIs404(t *testing.T) {
	srv, _ := newServer(t)
	rec := doJSON(t, srv.Handler(), "PATCH", "/admin/models/nope", testAdminToken, map[string]any{"enabled": false})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "not_found")
}

func TestAdminCRUDRejectsBadJSONWithoutPanicking(t *testing.T) {
	srv, _ := newServer(t)
	h := srv.Handler()
	for _, c := range []struct{ method, path string }{
		{"POST", "/admin/endpoints"},
		{"POST", "/admin/models"},
		{"PATCH", "/admin/models/x"},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader("{not json"))
		req.Header.Set("Authorization", "Bearer "+testAdminToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req) // a panic fails the test by crashing the binary
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s %s: status = %d, want 400", c.method, c.path, rec.Code)
		}
		assertErrorCode(t, rec, "bad_json")
	}
}

func TestListEndpointsOnAnEmptyBoardIsAnEmptyJSONArray(t *testing.T) {
	srv, _ := newServer(t)
	rec := do(t, srv.Handler(), "GET", "/admin/endpoints", testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("body = %q, want [] (null is not a list)", got)
	}
}

func namesOrNone(names []string) string {
	if len(names) == 0 {
		return "nothing is registered"
	}
	return names[0]
}

func strPtr(s string) *string     { return &s }
func boolPtr(b bool) *bool        { return &b }
func intPtr(n int) *int           { return &n }
func floatPtr(f float64) *float64 { return &f }

// TestCreateEndpointRefusesAnAuthRefOutsideTheProviderNamespace pins the write
// half of the credential rule. auth_ref is the one column that makes the manager
// read its own environment on a row's instruction, so a free-form name would let
// a row name NSM_ADMIN_TOKEN and have the manager send its own admin token to a
// URL the row chose. The refusal happens here, with the row still uncreated, so
// the operator who typed the name is told; the probe re-checks for rows this
// path never saw.
func TestCreateEndpointRefusesAnAuthRefOutsideTheProviderNamespace(t *testing.T) {
	cases := []struct {
		name    string
		authRef string
		ok      bool
		stored  string
	}{
		{name: "no key at all", authRef: "", ok: true, stored: ""},
		{name: "a provider key", authRef: "NSM_PROVIDER_OPENAI", ok: true, stored: "NSM_PROVIDER_OPENAI"},
		{name: "a second provider key", authRef: "NSM_PROVIDER_OLLAMA_KEY", ok: true, stored: "NSM_PROVIDER_OLLAMA_KEY"},
		{name: "digits after the prefix", authRef: "NSM_PROVIDER_A9", ok: true, stored: "NSM_PROVIDER_A9"},
		// Padded values are trimmed like every other string this path stores.
		{name: "padded", authRef: " NSM_PROVIDER_PADDED ", ok: true, stored: "NSM_PROVIDER_PADDED"},
		// The two names that made the rule necessary.
		{name: "the manager admin token", authRef: "NSM_ADMIN_TOKEN", ok: false},
		{name: "the enrollment token", authRef: "NSM_ENROLL_TOKEN", ok: false},
		// Everything else outside the namespace.
		{name: "a bare provider env name", authRef: "OPENAI_API_KEY", ok: false},
		{name: "lowercase namespace", authRef: "nsm_provider_openai", ok: false},
		{name: "prefix only", authRef: "NSM_PROVIDER_", ok: false},
		{name: "lowercase name", authRef: "NSM_PROVIDER_openai", ok: false},
		{name: "digit first", authRef: "NSM_PROVIDER_9KEY", ok: false},
		{name: "a dash", authRef: "NSM_PROVIDER_OPEN-AI", ok: false},
		{name: "a space inside", authRef: "NSM_PROVIDER_OPEN AI", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, clock := newTestDB(t)
			ep, err := CreateEndpoint(context.Background(), db, clock, testRegistry(), CreateEndpointRequest{
				Name: "ep", Provider: "openai", BaseURL: "http://127.0.0.1:8080", Reach: "lan", AuthRef: tc.authRef,
			})
			if tc.ok {
				if err != nil {
					t.Fatalf("CreateEndpoint(auth_ref=%q): %v", tc.authRef, err)
				}
				var stored string
				if err := db.QueryRow(`SELECT COALESCE(auth_ref,'') FROM endpoints WHERE id=?`, ep.ID).Scan(&stored); err != nil {
					t.Fatalf("read auth_ref: %v", err)
				}
				if stored != tc.stored {
					t.Fatalf("stored auth_ref = %q, want %q", stored, tc.stored)
				}
				return
			}
			if err == nil {
				t.Fatalf("CreateEndpoint(auth_ref=%q) = %+v, want an error", tc.authRef, ep)
			}
			if !strings.Contains(err.Error(), "auth_ref") {
				t.Fatalf("error = %q, want it to name auth_ref", err)
			}
			if n := countRows(t, db, "endpoints"); n != 0 {
				t.Fatalf("%d endpoint rows after a refused auth_ref, want 0", n)
			}
			if n := countRows(t, db, "events"); n != 0 {
				t.Fatalf("%d event rows after a refused auth_ref, want 0", n)
			}
		})
	}
}
