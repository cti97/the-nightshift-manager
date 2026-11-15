package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/provider"
)

// The health-probe tests. The properties they carry are the six decisions the
// plan settled before this task, restated as tests:
//
//   - four states are distinguished, and collapsing any of them to `down` fails
//     a test (ok, warming, auth_failed, down);
//   - a 2xx that is not a model list is `down`, not "hold previous health";
//   - a construction failure is a health state, not an error return;
//   - `health_reason` reaches the operator through /why;
//   - a transition to `down` alerts only if the endpoint has been `ok` before;
//   - the probe is on demand only, and it is the only writer of `health`.
//
// Two shapes of fixture recur: an endpoint created through the admin path (so
// the row is exactly what an operator could make) pointed at an httptest server,
// and a raw INSERT for rows the create path refuses — the probe is the second
// net, and a row that predates its validation must not be able to 500 it.

const (
	probeListBody = `{"object":"list","data":[{"id":"gpt-4o"}]}`
	probeTagsBody = `{"models":[{"name":"llama3"}]}`
)

// probeFake is a model-list server whose answer a test can change between
// probes, so one endpoint can be observed healthy and then broken.
type probeFake struct {
	ts *httptest.Server

	mu        sync.Mutex
	status    int
	body      string
	onRequest func()

	hits atomic.Int64
}

func newProbeFake(t *testing.T) *probeFake {
	t.Helper()
	f := &probeFake{status: http.StatusOK, body: probeListBody}
	f.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		status, body, onRequest := f.status, f.body, f.onRequest
		f.mu.Unlock()
		if onRequest != nil {
			onRequest()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.ts.Close)
	return f
}

// answer replaces what the server says to every subsequent request.
func (f *probeFake) answer(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

// during installs a hook that runs while the request is in flight — the only
// way to move a FixedClock inside the window the probe is timing.
func (f *probeFake) during(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onRequest = fn
}

// probeEndpoint creates one endpoint the way the admin path does, which means
// it starts unknown and never probed.
func probeEndpoint(t *testing.T, db *sql.DB, clock *FixedClock, name, prov, baseURL, reach string) EndpointView {
	t.Helper()
	return mustCreateEndpoint(t, db, clock, testRegistry(), CreateEndpointRequest{
		Name: name, Provider: prov, BaseURL: baseURL, Reach: reach,
	})
}

// rawEndpoint inserts a row the create path would have refused. It is how a row
// can predate this task's validation, or be edited by hand.
func rawEndpoint(t *testing.T, db *sql.DB, clock *FixedClock, id, name, prov, baseURL, reach, health string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO endpoints (id,name,provider,base_url,reach,health,created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		id, name, prov, baseURL, reach, health, clock.Now()); err != nil {
		t.Fatalf("insert raw endpoint %s: %v", name, err)
	}
}

// endpointEvents returns every event attached to one endpoint row. Counting the
// whole events table would count the `created` event too, which is not what any
// of these tests is asking about.
func endpointEvents(t *testing.T, db *sql.DB, endpointID string) []Event {
	t.Helper()
	all, err := ListEvents(context.Background(), db, "endpoint", endpointID, 100)
	if err != nil {
		t.Fatalf("list events for %s: %v", endpointID, err)
	}
	return all
}

// probeEvents returns the endpoint's probe events in append order.
func probeEvents(t *testing.T, db *sql.DB, endpointID string) []Event {
	t.Helper()
	var out []Event
	for _, e := range endpointEvents(t, db, endpointID) {
		if e.Type == probeEventType {
			out = append(out, e)
		}
	}
	return out
}

// storedHealth reads the four columns a probe owns, so every test asserts the
// row rather than only the value the function returned.
type storedHealth struct {
	health   string
	reason   string
	lastOKAt sql.NullInt64
	latency  sql.NullInt64
}

func readStoredHealth(t *testing.T, db *sql.DB, endpointID string) storedHealth {
	t.Helper()
	var s storedHealth
	if err := db.QueryRow(
		`SELECT health, COALESCE(health_reason,''), last_ok_at, latency_ms
		   FROM endpoints WHERE id=?`, endpointID).
		Scan(&s.health, &s.reason, &s.lastOKAt, &s.latency); err != nil {
		t.Fatalf("read health for %s: %v", endpointID, err)
	}
	return s
}

// TestProbeRecordsEachHealthState is the four-state table. Every row would fail
// if the state were collapsed to `down`: the point of the four states is that an
// operator's next action differs, and a label that says "broken" for "loading"
// removes a working endpoint from placement for the whole load.
func TestProbeRecordsEachHealthState(t *testing.T) {
	cases := []struct {
		name       string
		provider   string
		status     int
		body       string
		want       string
		wantInWhy  string
		wantReason bool
		// exact pins the whole sentence where two states share a name and differ
		// only by cause — auth_failed is "no key declared" or "key rejected",
		// and a substring cannot tell them apart.
		exact string
	}{
		{
			name: "ok", provider: "openai", status: http.StatusOK, body: probeListBody,
			want: "ok",
			// ok clears the reason: an explanation of a failure that has ended
			// is a lie printed under a healthy state.
		},
		{
			name: "warming", provider: "ollama", status: http.StatusServiceUnavailable,
			body: `{"error":"Loading model"}`, want: "warming", wantInWhy: "loading", wantReason: true,
		},
		{
			name: "auth_failed", provider: "openai", status: http.StatusUnauthorized,
			body: `{"error":{"message":"invalid api key"}}`, want: "auth_failed", wantReason: true,
			exact: healthReasonAuthNoKey,
		},
		{
			name: "down from an uninformative 503", provider: "ollama",
			// The same status as warming, and no load in the body: the label
			// must be what the probe observed, not what the status suggests.
			status: http.StatusServiceUnavailable, body: `{"error":"upstream connect error"}`,
			want: "down", wantReason: true,
		},
		{
			// Decision 1: a probe that cannot read its own answer has not probed.
			name: "down from a 2xx that is not a model list", provider: "openai",
			status: http.StatusOK, body: `<html>this is a web page</html>`,
			want: "down", wantInWhy: "not an OpenAI models list", wantReason: true,
		},
		{
			name: "down from a 5xx", provider: "openai", status: http.StatusInternalServerError,
			body: `{"error":{"message":"boom"}}`, want: "down", wantReason: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, clock := newTestDB(t)
			fake := newProbeFake(t)
			fake.answer(tc.status, tc.body)
			ep := probeEndpoint(t, db, clock, "probe-"+tc.want, tc.provider, fake.ts.URL, "lan")

			res, err := ProbeEndpoint(context.Background(), db, clock, testRegistry(), ep.ID)
			if err != nil {
				t.Fatalf("ProbeEndpoint: %v", err)
			}
			if string(res.Health) != tc.want {
				t.Fatalf("health = %q, want %q (reason %q)", res.Health, tc.want, res.HealthReason)
			}
			if tc.wantReason && strings.TrimSpace(res.HealthReason) == "" {
				t.Fatalf("health %q recorded no reason; the operator is told nothing", tc.want)
			}
			if !tc.wantReason && res.HealthReason != "" {
				t.Fatalf("health %q kept reason %q, want it cleared", tc.want, res.HealthReason)
			}
			if tc.wantInWhy != "" && !strings.Contains(strings.ToLower(res.HealthReason), strings.ToLower(tc.wantInWhy)) {
				t.Fatalf("reason %q does not name %q", res.HealthReason, tc.wantInWhy)
			}
			if tc.exact != "" && res.HealthReason != tc.exact {
				t.Fatalf("reason = %q, want exactly %q", res.HealthReason, tc.exact)
			}

			stored := readStoredHealth(t, db, ep.ID)
			if stored.health != tc.want {
				t.Fatalf("stored health = %q, want %q", stored.health, tc.want)
			}
			if stored.reason != res.HealthReason {
				t.Fatalf("stored reason = %q, returned %q", stored.reason, res.HealthReason)
			}
			if tc.want == "ok" {
				if !stored.lastOKAt.Valid || stored.lastOKAt.Int64 != clock.Now() {
					t.Fatalf("last_ok_at = %+v, want %d", stored.lastOKAt, clock.Now())
				}
			} else if stored.lastOKAt.Valid {
				t.Fatalf("last_ok_at = %d after a %q probe, want NULL: it was never ok", stored.lastOKAt.Int64, tc.want)
			}
			// latency is measured, so it exists for every probe that reached the
			// write path, including the failed ones.
			if !stored.latency.Valid {
				t.Fatalf("latency_ms is NULL after a %q probe, want a measurement", tc.want)
			}
			if res.LatencyMS == nil {
				t.Fatalf("returned latency is nil after a %q probe", tc.want)
			}

			evs := probeEvents(t, db, ep.ID)
			if len(evs) != 1 {
				t.Fatalf("%d probe events, want exactly 1: %+v", len(evs), evs)
			}
			var payload struct {
				Health   string `json:"health"`
				Previous string `json:"previous_health"`
				Reason   string `json:"health_reason"`
				Latency  *int64 `json:"latency_ms"`
				Alert    bool   `json:"alert"`
			}
			if err := json.Unmarshal([]byte(evs[0].Payload), &payload); err != nil {
				t.Fatalf("decode probe event payload %q: %v", evs[0].Payload, err)
			}
			if evs[0].Actor != probeActor {
				t.Fatalf("probe event actor = %q, want %q: a scheduled probe must not look human", evs[0].Actor, probeActor)
			}
			if payload.Health != tc.want {
				t.Fatalf("event says health %q, row says %q", payload.Health, tc.want)
			}
			if payload.Previous != healthUnknownOnCreate {
				t.Fatalf("event says previous health %q, want %q", payload.Previous, healthUnknownOnCreate)
			}
			if payload.Latency == nil {
				t.Fatal("event carries no latency")
			}
			if payload.Alert {
				t.Fatal("first probe of a never-ok endpoint raised an alert; that is the cold-load false alarm")
			}
			if got := fake.hits.Load(); got != 1 {
				t.Fatalf("%d requests reached the endpoint, want 1", got)
			}
		})
	}
}

// TestProbeOfAnUnreachableEndpointIsDown pins the transport path: nothing
// answered, so no state was determined by the adapter, and the probe's own
// answer is that the endpoint is down.
func TestProbeOfAnUnreachableEndpointIsDown(t *testing.T) {
	db, clock := newTestDB(t)
	fake := newProbeFake(t)
	ep := probeEndpoint(t, db, clock, "gone", "openai", fake.ts.URL, "lan")
	fake.ts.Close() // the row still names the URL; nothing is listening on it

	res, err := ProbeEndpoint(context.Background(), db, clock, testRegistry(), ep.ID)
	if err != nil {
		t.Fatalf("ProbeEndpoint: %v", err)
	}
	if string(res.Health) != "down" {
		t.Fatalf("health = %q, want down", res.Health)
	}
	if !strings.Contains(res.HealthReason, "transport") {
		t.Fatalf("reason = %q, want it to name the transport failure", res.HealthReason)
	}
	if s := readStoredHealth(t, db, ep.ID); s.health != "down" || s.lastOKAt.Valid {
		t.Fatalf("stored = %+v, want down with no last_ok_at", s)
	}
}

// TestProbeMeasuresLatencyWithTheClock pins that latency_ms is measured and not
// fabricated: the fake advances the board's own clock by a known amount inside
// the request, and the recorded figure must be that amount.
func TestProbeMeasuresLatencyWithTheClock(t *testing.T) {
	const travel = 137 * time.Millisecond
	db, clock := newTestDB(t)
	fake := newProbeFake(t)
	fake.during(func() { clock.Advance(travel) })
	ep := probeEndpoint(t, db, clock, "slow", "openai", fake.ts.URL, "lan")

	res, err := ProbeEndpoint(context.Background(), db, clock, testRegistry(), ep.ID)
	if err != nil {
		t.Fatalf("ProbeEndpoint: %v", err)
	}
	if res.LatencyMS == nil {
		t.Fatal("LatencyMS is nil")
	}
	if got, want := *res.LatencyMS, travel.Milliseconds(); got != want {
		t.Fatalf("latency = %dms, want %dms (measured, not fabricated)", got, want)
	}
	if s := readStoredHealth(t, db, ep.ID); !s.latency.Valid || s.latency.Int64 != travel.Milliseconds() {
		t.Fatalf("stored latency = %+v, want %dms", s.latency, travel.Milliseconds())
	}
}

// TestProbeKeepsLastOKAtAcrossAFailure pins the difference between "when did we
// last see it healthy" and "when did we last look": a failure must not overwrite
// the former, or the alerting rule loses the only fact it needs.
func TestProbeKeepsLastOKAtAcrossAFailure(t *testing.T) {
	db, clock := newTestDB(t)
	fake := newProbeFake(t)
	ep := probeEndpoint(t, db, clock, "flaky", "openai", fake.ts.URL, "lan")
	ctx := context.Background()

	if _, err := ProbeEndpoint(ctx, db, clock, testRegistry(), ep.ID); err != nil {
		t.Fatalf("first probe: %v", err)
	}
	first := readStoredHealth(t, db, ep.ID)
	if !first.lastOKAt.Valid {
		t.Fatal("a successful probe recorded no last_ok_at")
	}

	clock.Advance(time.Minute)
	fake.answer(http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	res, err := ProbeEndpoint(ctx, db, clock, testRegistry(), ep.ID)
	if err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if string(res.Health) != "down" {
		t.Fatalf("health = %q, want down", res.Health)
	}
	second := readStoredHealth(t, db, ep.ID)
	if !second.lastOKAt.Valid || second.lastOKAt.Int64 != first.lastOKAt.Int64 {
		t.Fatalf("last_ok_at = %+v, want the earlier %d preserved", second.lastOKAt, first.lastOKAt.Int64)
	}

	// And recovering clears the stale reason while moving last_ok_at forward.
	fake.answer(http.StatusOK, probeListBody)
	if _, err := ProbeEndpoint(ctx, db, clock, testRegistry(), ep.ID); err != nil {
		t.Fatalf("recovery probe: %v", err)
	}
	recovered := readStoredHealth(t, db, ep.ID)
	if recovered.health != "ok" || recovered.reason != "" {
		t.Fatalf("recovered = %+v, want ok with no reason", recovered)
	}
	if !recovered.lastOKAt.Valid || recovered.lastOKAt.Int64 != clock.Now() {
		t.Fatalf("last_ok_at = %+v, want the recovery's %d", recovered.lastOKAt, clock.Now())
	}
}

// TestProbeTransitionRule is decision 4 stated as a table: alerting does not key
// on `down` alone. A never-ok endpoint going down is a first contact, not a
// regression; the cold load that 503s while it starts must not page anyone.
func TestProbeTransitionRule(t *testing.T) {
	okAt := int64(1_700_000_000)
	cases := []struct {
		name     string
		from     string
		lastOKAt *int64
		to       string
		want     bool
	}{
		{name: "never probed goes down", from: "unknown", to: "down", want: false},
		{name: "down stays down", from: "down", to: "down", want: false},
		// The regression the first version of this rule had: last_ok_at survives
		// a failure, so an endpoint in a sustained outage still carries it. Alert
		// on the transition, or one outage pages on every probe.
		{name: "down stays down after an earlier ok", from: "down", lastOKAt: &okAt, to: "down", want: false},
		{name: "warming without a prior ok goes down", from: "warming", to: "down", want: false},
		{name: "auth failed without a prior ok goes down", from: "auth_failed", to: "down", want: false},
		{name: "ok goes down", from: "ok", to: "down", want: true},
		// Between the ok and the down the endpoint was seen warming; it has
		// still been ok, and last_ok_at is the evidence. Swallowing this alert
		// because the previous sample was warming is a false negative.
		{name: "warming after an earlier ok goes down", from: "warming", lastOKAt: &okAt, to: "down", want: true},
		{name: "ok stays ok", from: "ok", to: "ok", want: false},
		{name: "down recovers", from: "down", to: "ok", want: false},
		{name: "warming recovers", from: "warming", to: "ok", want: false},
		{name: "recovery does not alert", from: "unknown", to: "ok", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProbeAlert(tc.from, tc.lastOKAt, tc.to); got != tc.want {
				t.Fatalf("ProbeAlert(%q, %v, %q) = %v, want %v", tc.from, tc.lastOKAt, tc.to, got, tc.want)
			}
		})
	}
}

// TestProbeReportsAnAlertOnlyAfterAnOK drives the same rule end to end, because
// the rule is only useful if the probe populates it: a scheduler reads the
// result, not the table test.
func TestProbeReportsAnAlertOnlyAfterAnOK(t *testing.T) {
	db, clock := newTestDB(t)
	fake := newProbeFake(t)
	fake.answer(http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	// ollama, because one step below distinguishes warming from down and the
	// openai adapter deliberately folds every 5xx into down.
	ep := probeEndpoint(t, db, clock, "cold", "ollama", fake.ts.URL, "lan")
	ctx := context.Background()

	// First contact, failing: no alert.
	res, err := ProbeEndpoint(ctx, db, clock, testRegistry(), ep.ID)
	if err != nil {
		t.Fatalf("first probe: %v", err)
	}
	if res.Alert {
		t.Fatal("the first probe of a never-ok endpoint alerted")
	}

	// Recovers: a recovery is not an alert either.
	fake.answer(http.StatusOK, probeTagsBody)
	if res, err = ProbeEndpoint(ctx, db, clock, testRegistry(), ep.ID); err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if res.Alert {
		t.Fatal("a recovery raised an alert")
	}

	// Loading: not an alert, but the endpoint has been ok.
	fake.answer(http.StatusServiceUnavailable, `{"error":"Loading model"}`)
	if res, err = ProbeEndpoint(ctx, db, clock, testRegistry(), ep.ID); err != nil {
		t.Fatalf("third probe: %v", err)
	}
	if res.Alert {
		t.Fatal("warming raised an alert")
	}

	// Broken: this is the regression, and it must say so.
	fake.answer(http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	if res, err = ProbeEndpoint(ctx, db, clock, testRegistry(), ep.ID); err != nil {
		t.Fatalf("fourth probe: %v", err)
	}
	if !res.Alert {
		t.Fatal("a down transition after an ok did not alert")
	}
	evs := probeEvents(t, db, ep.ID)
	var payload struct {
		Alert bool `json:"alert"`
	}
	if err := json.Unmarshal([]byte(evs[len(evs)-1].Payload), &payload); err != nil {
		t.Fatalf("decode last probe event: %v", err)
	}
	if !payload.Alert {
		t.Fatal("the alert did not reach the event, so a dashboard projecting from events cannot see it")
	}

	// Still broken on the next probe: not a transition, not another alert. One
	// outage is one page.
	if res, err = ProbeEndpoint(ctx, db, clock, testRegistry(), ep.ID); err != nil {
		t.Fatalf("fifth probe: %v", err)
	}
	if res.Alert {
		t.Fatal("a sustained down re-alerted; one outage must page once")
	}
	alerts := 0
	for _, e := range probeEvents(t, db, ep.ID) {
		var p struct {
			Alert bool `json:"alert"`
		}
		if err := json.Unmarshal([]byte(e.Payload), &p); err != nil {
			t.Fatalf("decode probe event %d: %v", e.ID, err)
		}
		if p.Alert {
			alerts++
		}
	}
	if alerts != 1 {
		t.Fatalf("%d alerting probes across five observations, want exactly 1", alerts)
	}
}

// TestProbeRecordsAConstructionFailureAsDown is decision 2: an adapter that
// cannot be built is a health state the operator can read on
// GET /admin/endpoints, not a 500 during a probe.
func TestProbeRecordsAConstructionFailureAsDown(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		baseURL  string
		wantIn   string
	}{
		{name: "unaddressable base url", provider: "openai", baseURL: "", wantIn: "openai"},
		{name: "provider that is not registered", provider: "martian", baseURL: "http://127.0.0.1:9", wantIn: "martian"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, clock := newTestDB(t)
			// Raw insert: the create path refuses both rows, so this is the
			// second net over rows that predate it or were edited by hand.
			rawEndpoint(t, db, clock, "ep-raw", "raw", tc.provider, tc.baseURL, "lan", healthUnknownOnCreate)

			res, err := ProbeEndpoint(context.Background(), db, clock, testRegistry(), "ep-raw")
			if err != nil {
				t.Fatalf("ProbeEndpoint returned %v; a construction failure is a health state", err)
			}
			if string(res.Health) != "down" {
				t.Fatalf("health = %q, want down", res.Health)
			}
			if !strings.Contains(res.HealthReason, tc.wantIn) {
				t.Fatalf("reason = %q, want it to name %q", res.HealthReason, tc.wantIn)
			}
			// No request was made, so there is no latency to report. A zero
			// would be indistinguishable from an instant answer.
			if res.LatencyMS != nil {
				t.Fatalf("latency = %d after a construction failure, want none", *res.LatencyMS)
			}
			s := readStoredHealth(t, db, "ep-raw")
			if s.health != "down" || s.reason == "" || s.latency.Valid {
				t.Fatalf("stored = %+v, want down with a reason and no latency", s)
			}
			if got := len(probeEvents(t, db, "ep-raw")); got != 1 {
				t.Fatalf("%d probe events, want 1", got)
			}
		})
	}
}

// TestProbeSendsTheProviderKeyItsRowNames proves the capability is genuinely
// restored: an endpoint whose auth_ref names a provider key the manager holds
// sends it, so an auth-gated endpoint can reach `ok` and become placeable. The
// fake answers only to the exact bearer, so "sent something" is not enough.
func TestProbeSendsTheProviderKeyItsRowNames(t *testing.T) {
	t.Setenv("NSM_PROVIDER_FAKE", "sk-provider-fake")

	var seen atomic.Value
	seen.Store("")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		seen.Store(auth)
		if auth != "Bearer sk-provider-fake" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(probeListBody))
	}))
	t.Cleanup(ts.Close)

	db, clock := newTestDB(t)
	ep := mustCreateEndpoint(t, db, clock, testRegistry(), CreateEndpointRequest{
		Name: "keyed", Provider: "openai", BaseURL: ts.URL, Reach: "wan", AuthRef: "NSM_PROVIDER_FAKE",
	})

	res, err := ProbeEndpoint(context.Background(), db, clock, testRegistry(), ep.ID)
	if err != nil {
		t.Fatalf("ProbeEndpoint: %v", err)
	}
	if got := seen.Load().(string); got != "Bearer sk-provider-fake" {
		t.Fatalf("the probe sent Authorization %q, want the provider key its row names", got)
	}
	if string(res.Health) != "ok" {
		t.Fatalf("health = %q, want ok (reason %q)", res.Health, res.HealthReason)
	}
	if s := readStoredHealth(t, db, ep.ID); s.health != "ok" || !s.lastOKAt.Valid {
		t.Fatalf("stored = %+v, want ok with a last_ok_at", s)
	}
}

// TestProbeOfARejectedProviderKeySaysSo is the other half of the two auth
// reasons: a key was sent and refused, which is a rotation problem, not a
// "declare a key" problem. The reason must say which.
func TestProbeOfARejectedProviderKeySaysSo(t *testing.T) {
	t.Setenv("NSM_PROVIDER_WRONG", "sk-revoked")

	fake := newProbeFake(t)
	fake.answer(http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`)
	db, clock := newTestDB(t)
	ep := mustCreateEndpoint(t, db, clock, testRegistry(), CreateEndpointRequest{
		Name: "revoked", Provider: "openai", BaseURL: fake.ts.URL, Reach: "wan", AuthRef: "NSM_PROVIDER_WRONG",
	})

	res, err := ProbeEndpoint(context.Background(), db, clock, testRegistry(), ep.ID)
	if err != nil {
		t.Fatalf("ProbeEndpoint: %v", err)
	}
	if string(res.Health) != "auth_failed" {
		t.Fatalf("health = %q, want auth_failed", res.Health)
	}
	if res.HealthReason != healthReasonAuthRejected {
		t.Fatalf("reason = %q, want %q", res.HealthReason, healthReasonAuthRejected)
	}
}

// TestProbeWithAReservedAuthRefSendsNothingAndRecordsDown is the tripwire with
// its polarity flipped. The probe may read its own environment only for a name
// inside the provider-key namespace; a row that names NSM_ADMIN_TOKEN is refused
// before the lookup, so the manager's admin token never reaches a URL the row
// also chose. The fake answers 200 to anything it receives, so a leak would not
// merely send the token, it would make the probe look healthy.
func TestProbeWithAReservedAuthRefSendsNothingAndRecordsDown(t *testing.T) {
	t.Setenv("NSM_ADMIN_TOKEN", "the-managers-own-admin-token")

	var seen atomic.Value
	seen.Store("")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(probeListBody))
	}))
	t.Cleanup(ts.Close)

	db, clock := newTestDB(t)
	rawEndpoint(t, db, clock, "ep-reserved", "reserved", "openai", ts.URL, "wan", healthUnknownOnCreate)
	// Raw SQL, because the write path now refuses this: the probe's check is the
	// one that has to catch a row the write path never saw.
	if _, err := db.Exec(`UPDATE endpoints SET auth_ref='NSM_ADMIN_TOKEN' WHERE id='ep-reserved'`); err != nil {
		t.Fatalf("set the reserved auth_ref: %v", err)
	}

	res, err := ProbeEndpoint(context.Background(), db, clock, testRegistry(), "ep-reserved")
	if err != nil {
		t.Fatalf("ProbeEndpoint: %v", err)
	}
	if got := seen.Load().(string); got != "" {
		t.Fatalf("the probe sent Authorization %q; a name outside NSM_PROVIDER_* must resolve nothing", got)
	}
	if string(res.Health) != "down" {
		t.Fatalf("health = %q, want down (the row cannot be resolved)", res.Health)
	}
	if !strings.Contains(res.HealthReason, "NSM_PROVIDER_") {
		t.Fatalf("reason = %q, want it to name the provider-key namespace", res.HealthReason)
	}
	if res.LatencyMS != nil {
		t.Fatalf("latency = %d, want none: no request was made", *res.LatencyMS)
	}
}

// TestProbeOfAMissingProviderKeyIsDown: the row names a permitted key that this
// process does not hold. That is a configuration failure the operator can fix,
// and it is recorded without spending a round trip to be told 401.
func TestProbeOfAMissingProviderKeyIsDown(t *testing.T) {
	const ref = "NSM_PROVIDER_ABSENT"
	t.Setenv(ref, "") // set-but-empty: a key with no value is no key

	db, clock := newTestDB(t)
	fake := newProbeFake(t)
	ep := mustCreateEndpoint(t, db, clock, testRegistry(), CreateEndpointRequest{
		Name: "keyless", Provider: "openai", BaseURL: fake.ts.URL, Reach: "wan", AuthRef: ref,
	})

	res, err := ProbeEndpoint(context.Background(), db, clock, testRegistry(), ep.ID)
	if err != nil {
		t.Fatalf("ProbeEndpoint: %v", err)
	}
	if string(res.Health) != "down" {
		t.Fatalf("health = %q, want down", res.Health)
	}
	if !strings.Contains(res.HealthReason, ref) {
		t.Fatalf("reason = %q, want it to name %q", res.HealthReason, ref)
	}
	if res.LatencyMS != nil {
		t.Fatalf("latency = %d, want none: no request was made", *res.LatencyMS)
	}
	if got := fake.hits.Load(); got != 0 {
		t.Fatalf("%d requests reached the endpoint; an absent key needs no round trip", got)
	}
}

// TestProbeOfAHangingEndpointIsDown pins the timeout half of the acceptance
// criteria: an endpoint that never answers must produce a state and a recorded
// reason, not an admin request that hangs. The probe timeout is shortened for
// the duration of the test and restored, so the path exercised is the production
// one without sitting out the production 20 seconds.
func TestProbeOfAHangingEndpointIsDown(t *testing.T) {
	restore := probeTimeout
	probeTimeout = 75 * time.Millisecond
	t.Cleanup(func() { probeTimeout = restore })

	db, clock := newTestDB(t)
	// The handler answers only when the probe gives up, so the only thing that
	// can end this request is the timeout under test.
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(hang.Close)
	ep := probeEndpoint(t, db, clock, "hanging", "openai", hang.URL, "lan")

	res, err := ProbeEndpoint(context.Background(), db, clock, testRegistry(), ep.ID)
	if err != nil {
		t.Fatalf("ProbeEndpoint: %v", err)
	}
	if string(res.Health) != "down" {
		t.Fatalf("health = %q, want down", res.Health)
	}
	if !strings.Contains(strings.ToLower(res.HealthReason), "deadline exceeded") {
		t.Fatalf("reason = %q, want the deadline it hit", res.HealthReason)
	}
	if s := readStoredHealth(t, db, ep.ID); s.health != "down" {
		t.Fatalf("stored health = %q, want down", s.health)
	}
	if got := len(probeEvents(t, db, ep.ID)); got != 1 {
		t.Fatalf("%d probe events, want 1", got)
	}
}

// TestProbeWillNotWriteHealthForAnEndpointThatVanished is the self-review
// question made executable. The pool is one connection and the probe holds no
// transaction while it is on the wire, so the row can disappear between the read
// and the write; the write must notice rather than resurrect a state for a row
// nobody can see.
func TestProbeWillNotWriteHealthForAnEndpointThatVanished(t *testing.T) {
	db, clock := newTestDB(t)
	fake := newProbeFake(t)
	ep := probeEndpoint(t, db, clock, "vanishing", "openai", fake.ts.URL, "lan")

	deleted := make(chan error, 1)
	fake.during(func() {
		_, err := db.Exec(`DELETE FROM endpoints WHERE id=?`, ep.ID)
		deleted <- err
	})

	_, err := ProbeEndpoint(context.Background(), db, clock, testRegistry(), ep.ID)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows wrapped", err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("delete the endpoint mid-probe: %v", err)
	}
	if n := countRows(t, db, "endpoints"); n != 0 {
		t.Fatalf("%d endpoint rows, want 0: the probe recreated nothing", n)
	}
	if got := probeEvents(t, db, ep.ID); len(got) != 0 {
		t.Fatalf("%d probe events for a row that no longer exists, want 0", len(got))
	}
}

// TestProbeOfAMissingEndpointIsNotFound pins the same check on the ordinary
// path: an id that never existed is a 404, not a row written against nothing.
func TestProbeOfAMissingEndpointIsNotFound(t *testing.T) {
	db, clock := newTestDB(t)
	_, err := ProbeEndpoint(context.Background(), db, clock, testRegistry(), "no-such-endpoint")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows wrapped", err)
	}
	if n := countRows(t, db, "events"); n != 0 {
		t.Fatalf("%d events written for an unknown endpoint, want 0", n)
	}
}

// TestProbeWritesHealthAndItsEventInOneTransaction breaks the event spine, not
// the endpoint write: the health must roll back with the event or the audit
// trail and the state disagree.
func TestProbeWritesHealthAndItsEventInOneTransaction(t *testing.T) {
	db, clock := newTestDB(t)
	fake := newProbeFake(t)
	ep := probeEndpoint(t, db, clock, "atomic", "openai", fake.ts.URL, "lan")
	if _, err := db.Exec(`ALTER TABLE events RENAME TO events_disabled`); err != nil {
		t.Fatalf("rename events table: %v", err)
	}

	if _, err := ProbeEndpoint(context.Background(), db, clock, testRegistry(), ep.ID); err == nil {
		t.Fatal("ProbeEndpoint succeeded with the event table gone; the event is not in its transaction")
	}
	s := readStoredHealth(t, db, ep.ID)
	if s.health != healthUnknownOnCreate {
		t.Fatalf("health = %q after a failed event write, want %q (the UPDATE was not rolled back)", s.health, healthUnknownOnCreate)
	}
	if s.reason != healthReasonNeverProbed || s.lastOKAt.Valid || s.latency.Valid {
		t.Fatalf("stored = %+v, want the untouched pre-probe row", s)
	}
}

// TestProbeWritesNothingWhenItsCallerHasGone: the probe's context is the admin
// request's. If the operator hangs up, the probe did not observe the endpoint —
// it observed its own cancellation — and writing `down` would blame the
// endpoint for the caller's behaviour.
func TestProbeWritesNothingWhenItsCallerHasGone(t *testing.T) {
	db, clock := newTestDB(t)
	fake := newProbeFake(t)
	ep := probeEndpoint(t, db, clock, "abandoned", "openai", fake.ts.URL, "lan")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ProbeEndpoint(ctx, db, clock, testRegistry(), ep.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	s := readStoredHealth(t, db, ep.ID)
	if s.health != healthUnknownOnCreate || s.lastOKAt.Valid {
		t.Fatalf("stored = %+v, want the untouched pre-probe row", s)
	}
	if got := endpointEvents(t, db, ep.ID); len(got) != 1 {
		// Exactly one: the `created` event Task 8 wrote. Nothing from the probe.
		t.Fatalf("%d events for the endpoint, want only the creation's 1: %+v", len(got), got)
	}
	if got := fake.hits.Load(); got != 0 {
		t.Fatalf("%d requests reached the endpoint for an abandoned probe, want 0", got)
	}
}

// TestProbeRefusesAHealthStateOutsideTheSchema keeps the board's health
// vocabulary closed. A provider returning a fifth state is a programming error;
// writing it would turn the schema's CHECK into a 500 at the next read, and
// inventing a mapping would hide the bug.
func TestProbeRefusesAHealthStateOutsideTheSchema(t *testing.T) {
	for _, state := range []provider.Health{"bogus", ""} {
		t.Run(fmt.Sprintf("state=%q", state), func(t *testing.T) {
			db, clock := newTestDB(t)
			rawEndpoint(t, db, clock, "ep-fake", "fake", "fake", "http://127.0.0.1:9", "lan", healthUnknownOnCreate)
			reg := provider.NewRegistry()
			reg.Register("fake", func(cfg provider.Config) (provider.Provider, error) {
				return fixedHealthProvider{health: state}, nil
			})

			if _, err := ProbeEndpoint(context.Background(), db, clock, reg, "ep-fake"); err == nil {
				t.Fatalf("ProbeEndpoint accepted health %q", state)
			}
			if s := readStoredHealth(t, db, "ep-fake"); s.health != healthUnknownOnCreate {
				t.Fatalf("health = %q, want the row untouched", s.health)
			}
			if n := countRows(t, db, "events"); n != 0 {
				t.Fatalf("%d events written for a refused health state, want 0", n)
			}
		})
	}
}

// fixedHealthProvider is a registered provider that reports one canned state,
// so the board's own validation can be tested without an HTTP server.
type fixedHealthProvider struct{ health provider.Health }

func (p fixedHealthProvider) Name() string { return "fake" }

func (p fixedHealthProvider) Complete(context.Context, provider.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (p fixedHealthProvider) EstimateTokens(provider.Request) int { return 0 }

func (p fixedHealthProvider) Probe(context.Context) (provider.Health, error) { return p.health, nil }

// TestProbeOfAStubEndpointIsOK answers the self-review question about a provider
// with no HTTP base URL. The stub is the reference implementation: it is the
// endpoint, there is no network to fail, and its Probe says ok. A probe cannot
// do better than ask the provider, and inventing a special case for one adapter
// would make the registry's promise ("a provider is a provider") false.
func TestProbeOfAStubEndpointIsOK(t *testing.T) {
	db, clock := newTestDB(t)
	ep := probeEndpoint(t, db, clock, "stubbed", "stub", "http://stub.invalid", "host")
	SeedNodeForTest(t, db, clock, "worker", "executor", "gpu")
	mustCreateModel(t, db, clock, CreateModelRequest{EndpointID: ep.ID, ModelID: "stub-model"})

	res, err := ProbeEndpoint(context.Background(), db, clock, testRegistry(), ep.ID)
	if err != nil {
		t.Fatalf("ProbeEndpoint: %v", err)
	}
	if string(res.Health) != "ok" {
		t.Fatalf("health = %q, want ok (reason %q)", res.Health, res.HealthReason)
	}
	if res.LatencyMS == nil {
		t.Fatal("a stub probe reported no latency; the stub call was still timed")
	}
	// The point of the probe: before it, the endpoint was inert. After it, the
	// row is placeable.
	cands, err := LoadCandidates(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadCandidates: %v", err)
	}
	if len(cands) == 0 {
		t.Fatal("a probed stub endpoint is still not placeable")
	}
}

// TestReadingEndpointsDoesNotTouchHealth is the global constraint as a test: a
// probe writes health, a read does not. Explain and ListEndpoints both walk the
// same rows, and neither may move a state.
func TestReadingEndpointsDoesNotTouchHealth(t *testing.T) {
	db, clock := newTestDB(t)
	const lastOK = int64(1_600_000_000)
	if _, err := db.Exec(
		`INSERT INTO endpoints (id,name,provider,base_url,reach,health,health_reason,last_ok_at,latency_ms,created_at)
		 VALUES ('ep-read','read','stub','http://read','host','ok','probed fine',?,42,?)`,
		lastOK, clock.Now()); err != nil {
		t.Fatalf("insert endpoint: %v", err)
	}
	itemID, err := CreateItem(context.Background(), db, clock, CreateItemRequest{Title: "t", Brief: "b"})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	if _, err := ListEndpoints(context.Background(), db); err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if _, err := Explain(context.Background(), db, clock, itemID); err != nil {
		t.Fatalf("Explain: %v", err)
	}

	s := readStoredHealth(t, db, "ep-read")
	if s.health != "ok" || s.reason != "probed fine" || s.lastOKAt.Int64 != lastOK || s.latency.Int64 != 42 {
		t.Fatalf("stored = %+v, want the read to have changed nothing", s)
	}
	if got := endpointEvents(t, db, "ep-read"); len(got) != 0 {
		t.Fatalf("%d endpoint events written by two reads, want 0", len(got))
	}
}

// TestCreateEndpointTellsTheOperatorToProbe is decision 6: creation is not an
// observation, so the 201 says the endpoint is inert and names the call that
// changes that. A freshly created endpoint that silently never places is the
// failure this hint exists to prevent.
func TestCreateEndpointTellsTheOperatorToProbe(t *testing.T) {
	srv, _ := newServer(t)
	rec := postJSON(t, srv.Handler(), "/admin/endpoints", testAdminToken, CreateEndpointRequest{
		Name: "fresh", Provider: "stub", BaseURL: "http://stub.invalid", Reach: "host",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID           string `json:"id"`
		Health       string `json:"health"`
		HealthReason string `json:"health_reason"`
		Next         string `json:"next"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode create response %s: %v", rec.Body.String(), err)
	}
	if body.Health != "unknown" || body.HealthReason != "never probed" {
		t.Fatalf("created endpoint = %+v, want unknown/never probed", body)
	}
	if want := "POST /admin/endpoints/" + body.ID + "/probe"; body.Next != want {
		t.Fatalf("next = %q, want %q", body.Next, want)
	}

	// The listing is a view of what exists, not a list of instructions to the
	// operator, so it does not repeat the hint.
	list := do(t, srv.Handler(), "GET", "/admin/endpoints", testAdminToken)
	if strings.Contains(list.Body.String(), `"next"`) {
		t.Fatalf("the listing carries a next hint: %s", list.Body.String())
	}
}

// TestProbeRouteProbesOneEndpoint covers the route the plan names:
// POST /admin/endpoints/{id}/probe, on demand, behind adminAuth.
func TestProbeRouteProbesOneEndpoint(t *testing.T) {
	srv, clock := newServer(t)
	fake := newProbeFake(t)
	ep := probeEndpoint(t, srv.db, clock, "routed", "openai", fake.ts.URL, "lan")
	h := srv.Handler()
	// The path comes from the same helper the create response's hint uses, so
	// this test fails if the hint and the route drift apart.
	route := probePath(ep.ID)

	if rec := do(t, h, "POST", route, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status without a token = %d, want 401", rec.Code)
	}
	if rec := do(t, h, "POST", route, testAdminToken); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	} else {
		var res ProbeResult
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode probe response %s: %v", rec.Body.String(), err)
		}
		if res.EndpointID != ep.ID || string(res.Health) != "ok" {
			t.Fatalf("probe response = %+v, want ok for %s", res, ep.ID)
		}
	}

	// The route's answer is the row: the listing the operator reads next must
	// agree with what the probe returned.
	list := do(t, h, "GET", "/admin/endpoints", testAdminToken)
	var eps []EndpointView
	if err := json.Unmarshal(list.Body.Bytes(), &eps); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	if len(eps) != 1 || eps[0].Health != "ok" || eps[0].LatencyMS == nil {
		t.Fatalf("listing = %+v, want one ok endpoint with a latency", eps)
	}
}

// TestProbeRouteOnAMissingEndpointIs404 keeps "the row is not there" distinct
// from "the probe went wrong": the operator can fix the first by retyping an id.
func TestProbeRouteOnAMissingEndpointIs404(t *testing.T) {
	srv, _ := newServer(t)
	rec := do(t, srv.Handler(), "POST", "/admin/endpoints/no-such/probe", testAdminToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %s: %v", rec.Body.String(), err)
	}
	if body.Error.Code != "not_found" {
		t.Fatalf("error code = %q, want not_found", body.Error.Code)
	}
}

// TestProbeRouteReportsAConstructionFailureAs200 pins decision 2 at the edge: a
// probe that cannot build its adapter is a recorded state, so the operator gets
// a body describing it rather than a 500 that looks like the board broke.
func TestProbeRouteReportsAConstructionFailureAs200(t *testing.T) {
	srv, clock := newServer(t)
	rawEndpoint(t, srv.db, clock, "ep-bad", "bad", "openai", "", "lan", healthUnknownOnCreate)

	rec := do(t, srv.Handler(), "POST", "/admin/endpoints/ep-bad/probe", testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s; a construction failure is a health state", rec.Code, rec.Body.String())
	}
	var res ProbeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(res.Health) != "down" || res.HealthReason == "" {
		t.Fatalf("response = %+v, want down with a reason", res)
	}
}

// TestWhyNamesTheStateAndTheProbesReason is decision 3 through the operator's
// route. "down" alone sends someone looking for a network problem when the
// answer is a rejected credential, and `unknown` alone does not say that nobody
// has ever asked — so the probe's reason has to reach /why, not only
// GET /admin/endpoints.
func TestWhyNamesTheStateAndTheProbesReason(t *testing.T) {
	srv, clock := newServer(t)
	h := srv.Handler()
	SeedNodeForTest(t, srv.db, clock, "pi", "executor", "gpu")

	never := probeEndpoint(t, srv.db, clock, "never-probed", "openai", "http://127.0.0.1:9", "lan")
	mustCreateModel(t, srv.db, clock, CreateModelRequest{EndpointID: never.ID, ModelID: "never-m"})

	rawEndpoint(t, srv.db, clock, "ep-401", "rejecting", "openai", "http://127.0.0.1:8", "lan", "down")
	if _, err := srv.db.Exec(`UPDATE endpoints SET health='down', health_reason='401 from /v1/models' WHERE id='ep-401'`); err != nil {
		t.Fatalf("set the failing endpoint's reason: %v", err)
	}
	mustCreateModel(t, srv.db, clock, CreateModelRequest{EndpointID: "ep-401", ModelID: "rejecting-m"})

	created := postJSON(t, h, "/admin/items", testAdminToken, CreateItemRequest{
		Title: "t", Brief: "b", Needs: []string{"gpu"},
	})
	var item struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &item); err != nil {
		t.Fatalf("decode created item %s: %v", created.Body.String(), err)
	}

	rec := do(t, h, "GET", "/admin/why/"+item.ID, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("why status = %d, body %s", rec.Code, rec.Body.String())
	}
	var ex Explanation
	if err := json.Unmarshal(rec.Body.Bytes(), &ex); err != nil {
		t.Fatalf("decode explanation %s: %v", rec.Body.String(), err)
	}
	if ex.Placed {
		t.Fatal("why says the item is placed; no endpoint is eligible")
	}
	var whys []string
	for _, r := range ex.Rejects {
		whys = append(whys, r.Why)
	}
	joined := strings.Join(whys, "\n")
	if !strings.Contains(joined, "never probed") {
		t.Fatalf("why does not say the endpoint was never probed: %q", joined)
	}
	if !strings.Contains(joined, "401 from /v1/models") {
		t.Fatalf("why does not carry the probe's reason: %q", joined)
	}
}
