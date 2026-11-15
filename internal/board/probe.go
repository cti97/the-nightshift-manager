// internal/board/probe.go
package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/provider"
)

// The health probe: the one writer of endpoints.health.
//
// Task 9's six settled decisions, and where each one lives:
//
//  1. A 2xx that is not a model list is `down`. observe() turns every error the
//     provider's Probe returns into `down` with that error as the reason; no
//     path in this file "holds" a previous health, and a probe that cannot read
//     its own answer has not probed.
//  2. A construction failure is a health state, not an error return. observe()
//     records `down` with the registry's message; the route answers 200.
//  3. health_reason reaches the operator through /why: the candidate loader
//     selects the column (claim.go) and Eligible prints it (placement.go).
//  4. Alerting does not key on `down` alone. ProbeAlert is the rule, and
//     ProbeResult.Alert carries its answer so a scheduler never re-derives it.
//  5. On demand first. This task ships the route (http.go); there is no
//     scheduler, because a schedule without decision 4 would page on startup
//     for every endpoint.
//  6. Created endpoints start unknown and are inert until probed (Task 8's
//     write path). This file is what makes them useful, and POST
//     /admin/endpoints says so in its response body (http.go).
//
// Two more facts this file states rather than leaves to be discovered:
//
//   - Latency is measured around p.Probe, and only around it: construction is
//     local and instant, so including it would report the board's own work as
//     the endpoint's. A probe that never reached the provider (a construction
//     failure, a provider key the manager does not hold) records NULL, which
//     says "there was nothing to measure" rather than an instant answer.
//   - A probe of a `stub` endpoint is `ok`. The stub has no HTTP base URL and no
//     network to fail, so the honest answer is the provider's own: it is the
//     reference implementation doing its job. That is also why a `stub` row does
//     not belong on a production board (Task 8 already records that it is
//     creatable); this probe adds no special case for one adapter, because the
//     registry's promise is that a provider is a provider.
//   - A probe sends the provider key its row names, and only ever a provider
//     key. The danger was never that the manager reads its own environment: it
//     was that a row could name an *arbitrary* secret in it, so a row saying
//     NSM_ADMIN_TOKEN and pointing at any URL would receive the manager's admin
//     token as a bearer header. auth_ref is therefore restricted to the
//     NSM_PROVIDER_* namespace — enforced at endpoint-write time and again here
//     before the lookup (provider.CheckAuthRef) — which makes the manager's own
//     tokens unmatchable by construction rather than by review. The key is
//     resolved locally from this process's environment and never transits the
//     board.
//     A 401 with the row's key is auth_failed ("the endpoint rejected it"); a
//     401 with no key named is also auth_failed, but the reason says the row
//     names none, because "reachable and gated" is not the same fault as "the
//     credential is wrong".
//   - Reach is measured from the manager, not from the executor. A host or lan
//     endpoint on another machine is probed from wherever the manager runs, so a
//     healthy endpoint can be recorded down because the manager cannot see it,
//     and placement then excludes it. That is a limitation of probing from the
//     control plane rather than a fact about the endpoint, and it is deliberately
//     not solved here: delegation (the executor's node probing its own reach, or
//     reporting health back) belongs to Task 12, which owns the run path and the
//     node that can actually answer the question. A sixth state invented in this
//     file would only move the lie.
//
// Two boundaries hold the write. The probe holds no transaction while it is on
// the wire — the pool is one connection, and a slow endpoint must not block the
// board — and the row is re-read inside the write transaction, so the state that
// is written, the transition it represents and the event that describes it are
// all decided against the row as it stands at the moment of the write. A row
// deleted between the read and the write therefore gets nothing written for it,
// and two overlapping probes cannot both alert on the same regression.

// probeTimeout bounds one probe. It is the board's number, not the adapter's:
// the adapters default to 120s because their first request may load a model, but
// a probe asks for a model list and answers the operator's POST. A probe that
// hangs for two minutes is a worse answer than "down", and the failure it
// produces is a context deadline the operator can see.
//
// It is a variable rather than a constant for one reason: the timeout path is
// only testable by shortening it, and a test that sat out the real 20 seconds
// would be the slowest thing in the suite. Nothing in production assigns it —
// probe_test.go saves it, shortens it, and restores it.
var probeTimeout = 20 * time.Second

const (
	// probeEventType is the event every completed probe appends. It records an
	// observation, not a change: a probe that confirms `down` still bounds the
	// outage, and a dashboard projecting health from events needs both.
	probeEventType = "probed"

	// probeActor is the event actor for a probe. Deliberately not "admin": the
	// admin route is only the first caller, and a scheduled probe appending
	// human-attributed events would make the audit spine lie about who acted.
	probeActor = "probe"

	// The non-ok states carry no diagnostic text on the provider interface:
	// Probe returns (Health, error), and for warming and auth_failed the error
	// is nil, so the state is the whole message. These are the states' meanings
	// as provider.go defines them, not a fabrication of a detail the adapter
	// never supplied. A probe detail string is the interface change that would
	// replace them; until then the alternative is a `down`/`warming` with no
	// reason at all, which the operator cannot act on.
	//
	// auth_failed has two causes the operator fixes differently, so it has two
	// sentences: a row that names a provider key and is refused (rotate it), and
	// a row that names none against a gated endpoint (declare one).
	healthReasonWarming      = "a model is loading"
	healthReasonAuthNoKey    = "reachable, but it requires authentication and this endpoint names no provider key"
	healthReasonAuthRejected = "the endpoint rejected the provider key this endpoint names"
	healthReasonDown         = "the endpoint answered with a failure status"
)

// ProbeResult is one probe: what it observed and what it recorded. It is the
// route's response body as well as the record a scheduler would read, so the
// transition rule's answer is a field rather than something each caller
// recomputes.
type ProbeResult struct {
	EndpointID     string          `json:"endpoint_id"`
	PreviousHealth string          `json:"previous_health"`
	Health         provider.Health `json:"health"`
	HealthReason   string          `json:"health_reason,omitempty"`
	LastOKAt       *int64          `json:"last_ok_at,omitempty"`
	LatencyMS      *int64          `json:"latency_ms,omitempty"`
	// Alert is ProbeAlert's answer for this transition (decision 4).
	Alert bool `json:"alert"`
}

// ProbeAlert reports whether a probe transition deserves an operator alert.
//
// The rule (Task 9 decision 4) is not "down", because an uninformative 503
// during a cold load is down and must not page anyone. A transition to `down`
// alerts only when the endpoint has been `ok` before; a `down` that has never
// been `ok` is a first contact, not a regression.
//
// The `from != "down"` guard is not redundant with "has been ok": last_ok_at is
// preserved across failures, because it is the evidence the rule needs, so a
// sustained outage still carries a set last_ok_at. Without the guard every probe
// of that outage would re-fire and one failure would page once per probe
// interval. Alerting is about a transition, and a transition requires the state
// to have moved.
//
// "Has been ok" is the previous health OR last_ok_at, and the second half
// matters: if the last probe before the failure saw `warming`, the previous
// state is warming while the endpoint is known to have worked, and keying on
// the previous state alone would swallow the alert for the one failure an
// operator most needs. It is the transition that bounds the alert; the history
// only decides whether this transition is a regression.
//
// from is the health stored before the write and to is the health being written.
// Both are the schema's vocabulary, not provider.Health, so a row a hand edit
// left in an unexpected state still compares honestly.
func ProbeAlert(from string, lastOKAt *int64, to string) bool {
	if to != "down" || from == "down" {
		return false
	}
	return from == "ok" || (lastOKAt != nil && *lastOKAt > 0)
}

// probeTarget is the endpoint row the probe needs before it can ask anything:
// the adapter's connection, the declared reach, and the *name* of the provider
// key (authRef), never the key itself.
type probeTarget struct {
	id       string
	name     string
	provider string
	baseURL  string
	authRef  string
	reach    string
}

// ProbeEndpoint probes one endpoint and records what it observed. It returns an
// error only when nothing could be recorded: the row is not there, the caller's
// context ended, a provider broke its contract, or the store refused the write.
// Every health state the probe can determine — including `down`, including an
// adapter that cannot be constructed — is a successful call (decisions 1 and 2).
func ProbeEndpoint(ctx context.Context, db *sql.DB, clock Clock, reg *provider.Registry, id string) (ProbeResult, error) {
	// The caller's context is the admin request's. If it has already ended there
	// is nothing to observe, and the honest answer is the cancellation, not a
	// state for the endpoint.
	if err := ctx.Err(); err != nil {
		return ProbeResult{}, fmt.Errorf("probe: %w", err)
	}
	target, err := loadProbeTarget(ctx, db, id)
	if err != nil {
		return ProbeResult{}, err
	}
	health, reason, latency, err := observe(ctx, clock, reg, target)
	if err != nil {
		return ProbeResult{}, err
	}
	return recordProbe(ctx, db, clock, target, health, reason, latency)
}

// loadProbeTarget reads the row the probe is about. sql.ErrNoRows stays wrapped:
// the route tells "this endpoint does not exist" (404) from "the board cannot
// read it" (500) by errors.Is.
func loadProbeTarget(ctx context.Context, db *sql.DB, id string) (probeTarget, error) {
	var t probeTarget
	err := db.QueryRowContext(ctx,
		`SELECT id, name, provider, base_url, COALESCE(auth_ref,''), reach
		   FROM endpoints WHERE id=?`, id).
		Scan(&t.id, &t.name, &t.provider, &t.baseURL, &t.authRef, &t.reach)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return probeTarget{}, fmt.Errorf("endpoint %s: %w", id, sql.ErrNoRows)
		}
		return probeTarget{}, err
	}
	return t, nil
}

// observe asks the endpoint for its health, and returns the state to record plus
// the reason and the measured latency. The error return is reserved for "the
// probe determined nothing about this endpoint": the caller went away, or a
// provider returned a state the schema does not have. Everything the endpoint
// itself did becomes a state (decisions 1 and 2).
//
// The credential is resolved here and nowhere else: a name the write path already
// accepted into the provider-key namespace, checked again before the lookup so a
// row inserted by hand cannot reach outside it. A key that is absent, or a name
// outside the namespace, is a configuration failure — recorded `down`, no request
// sent. A 401/403 after a key was sent is auth_failed with "rejected"; a 401/403
// with no key named is auth_failed with "names no provider key", because the two
// are different problems to fix.
func observe(ctx context.Context, clock Clock, reg *provider.Registry, t probeTarget) (provider.Health, string, *int64, error) {
	apiKey, err := probeCredential(t)
	if err != nil {
		return provider.HealthDown, err.Error(), nil, nil
	}
	p, err := newProber(reg, t, apiKey)
	if err != nil {
		return provider.HealthDown, err.Error(), nil, nil
	}

	start := clock.NowMillis()
	health, probeErr := p.Probe(ctx)
	elapsed := clock.NowMillis() - start
	latency := &elapsed

	if probeErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// The probe's failure is the caller's cancellation, not the
			// endpoint's condition: blaming the endpoint would be a lie the
			// next operator has to disprove.
			return "", "", nil, fmt.Errorf("probe of endpoint %s did not complete: %w", t.name, ctxErr)
		}
		// Decision 1: whatever the adapter could not read, the probe did not
		// observe. `down` is the state that says so.
		return provider.HealthDown, probeErr.Error(), latency, nil
	}
	reason, ok := reasonForHealth(health, apiKey != "")
	if !ok {
		// A fifth state is a provider bug. Writing it would fail the schema's
		// CHECK on the next read and hide where it came from; inventing a
		// mapping would paper over it. The board refuses instead.
		return "", "", nil, fmt.Errorf(
			"provider %q returned health %q, which is not one of ok, warming, down or auth_failed",
			t.provider, health)
	}
	return health, reason, latency, nil
}

// probeCredential resolves the provider key endpoints.auth_ref names. The name is
// checked against the namespace again here — not because the write path is
// untrusted, but because rows exist that the write path never saw (fixtures, raw
// SQL, a migration). A refused name sends no request and reads no environment.
//
// The returned error is the reason string for a `down`: a row that names a key
// the manager does not hold cannot authenticate, and saying so is both cheaper
// and more actionable than the 401 it would earn.
func probeCredential(t probeTarget) (string, error) {
	if t.authRef == "" {
		return "", nil
	}
	if err := provider.CheckAuthRef(t.authRef); err != nil {
		return "", err
	}
	key := os.Getenv(t.authRef)
	if key == "" {
		return "", fmt.Errorf("auth_ref %q names a provider key that is not set in the manager's environment", t.authRef)
	}
	return key, nil
}

// newProber builds the adapter for one endpoint row. Construction is where a
// bad base URL, an unknown provider or a provider that rejects its own
// configuration becomes visible, and it is deliberately not an error return: an
// adapter that cannot be built cannot be probed, which is a thing the operator
// needs to see on GET /admin/endpoints (decision 2).
func newProber(reg *provider.Registry, t probeTarget, apiKey string) (provider.Provider, error) {
	if reg == nil {
		// A server wired without a registry can build nothing. Failing closed
		// mirrors checkProviderRegistered, which accepts no name without one.
		return nil, errors.New("no provider registry is configured, so no adapter can be built")
	}
	// The key is the one probeCredential resolved from the provider-key
	// namespace, or empty. Every adapter sends no Authorization header when it is
	// empty (openai.go), so "names no provider key" really means no header.
	p, err := reg.New(provider.Config{
		Provider: t.provider,
		BaseURL:  t.baseURL,
		APIKey:   apiKey,
		Reach:    t.reach,
		Timeout:  probeTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot build the %s adapter: %w", t.provider, err)
	}
	return p, nil
}

// reasonForHealth turns a state into the sentence the operator reads. ok clears
// the reason: a stale explanation printed under a healthy state is a lie, and
// "ok: 401 from /v1/models" is worse than no reason at all.
func reasonForHealth(h provider.Health, sentKey bool) (string, bool) {
	switch h {
	case provider.HealthOK:
		return "", true
	case provider.HealthWarming:
		return healthReasonWarming, true
	case provider.HealthAuthFailed:
		if sentKey {
			return healthReasonAuthRejected, true
		}
		return healthReasonAuthNoKey, true
	case provider.HealthDown:
		return healthReasonDown, true
	}
	return "", false
}

// recordProbe writes the observation and its event in one transaction. The
// event's actor is "probe", not "admin": the route is only the first caller, and
// a scheduled probe must not append events that claim a human acted.
//
// The previous state is read inside the transaction rather than taken from the
// read that built the adapter: the transition, and therefore the alert, must be
// decided against the row at the moment of the write, or two overlapping probes
// could each report the same regression as fresh. Reading it here is also what
// makes the write conditional on the row still existing — a probe of an endpoint
// that was deleted while it was on the wire writes nothing and says so.
func recordProbe(ctx context.Context, db *sql.DB, clock Clock, t probeTarget, health provider.Health, reason string, latency *int64) (ProbeResult, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ProbeResult{}, err
	}
	defer tx.Rollback()

	var previous string
	var lastOK sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT health, last_ok_at FROM endpoints WHERE id=?`, t.id).
		Scan(&previous, &lastOK); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProbeResult{}, fmt.Errorf("endpoint %s no longer exists: %w", t.id, sql.ErrNoRows)
		}
		return ProbeResult{}, err
	}

	var hadOK *int64
	if lastOK.Valid {
		v := lastOK.Int64
		hadOK = &v
	}
	// last_ok_at is "when we last saw it healthy", so only an ok observation
	// moves it. A failure keeps the earlier proof, which is what ProbeAlert
	// needs on the next transition.
	lastOKAt := hadOK
	if health == provider.HealthOK {
		now := clock.Now()
		lastOKAt = &now
	}
	alert := ProbeAlert(previous, hadOK, string(health))

	res, err := tx.ExecContext(ctx,
		`UPDATE endpoints SET health=?, health_reason=?, last_ok_at=?, latency_ms=? WHERE id=?`,
		string(health), nullable(reason), nullInt(lastOKAt), nullInt(latency), t.id)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("record probe of endpoint %s: %w", t.id, err)
	}
	// Unreachable while the SELECT above is in this transaction (the pool is one
	// connection, so no other writer can remove the row between them), and kept
	// because that is the invariant the write depends on: if a later edit makes
	// the SELECT tolerant of a missing row, this catches the silent no-op.
	if n, err := res.RowsAffected(); err != nil {
		return ProbeResult{}, err
	} else if n != 1 {
		return ProbeResult{}, fmt.Errorf("record probe of endpoint %s: the update matched no row", t.id)
	}

	if err := AppendEvent(ctx, tx, clock, probeActor, "endpoint", t.id, probeEventType, map[string]any{
		"previous_health": previous,
		"health":          string(health),
		"health_reason":   reason,
		"latency_ms":      latency,
		"alert":           alert,
	}); err != nil {
		return ProbeResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProbeResult{}, err
	}

	return ProbeResult{
		EndpointID:     t.id,
		PreviousHealth: previous,
		Health:         health,
		HealthReason:   reason,
		LastOKAt:       lastOKAt,
		LatencyMS:      latency,
		Alert:          alert,
	}, nil
}

// nullInt stores a measured value, or NULL when there is none. Spelled out
// rather than passing a nil *int64 through the driver: the driver's nil-pointer
// handling is an implementation detail, and NULL is a statement here — "there
// was nothing to measure", which a construction failure genuinely has.
func nullInt(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}
