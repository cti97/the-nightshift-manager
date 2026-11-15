// internal/board/endpoints.go
package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/cti97/the-nightshift-manager/internal/caps"
	"github.com/cti97/the-nightshift-manager/internal/provider"
)

// This file is the operator's way into the endpoints and models tables. Until
// now the only writer was a fixture; every smoke test inserted rows with raw
// SQL, which meant the loader's expectations were never exercised by a real
// write path.
//
// Two rules hold on every write here, and they are the same rule:
//
//   - validate BEFORE writing. Every check below runs before the transaction
//     opens, so a rejected request leaves no row, no event and no partial state
//     to clean up. The only checks that need the store (uniqueness, a referenced
//     endpoint) run inside the transaction, and the pool is one connection, so
//     they cannot be raced by another writer.
//   - validate against the real vocabulary. reach and features are closed sets
//     the placement code compares by exact string equality; provider is the
//     registry's own name list. A typo stored here is a row that silently never
//     places — the failure this whole file exists to move to the call that
//     caused it.
//
// There is no DELETE route, for either table, and that is deliberate.
//
// A model is retired with PATCH {"enabled": false}: the loader prefilters it,
// Eligible explains it ("model X is disabled"), and the row keeps its cost,
// features and history so it can be re-enabled. Deleting it instead would have
// to either cascade into runs.model_id (destroying the audit trail of every run
// that used it) or be refused by the foreign key — the schema already says the
// history is not disposable.
//
// An endpoint has no enabled column at all, because health already suppresses
// the whole endpoint: placement requires health 'ok', so a down or unprobed
// endpoint contributes no candidates. Disabling its models is the operator's
// way to retire it, and the models of a merely unhealthy endpoint stay enabled —
// that is the safer behaviour, because health is an observation that reverses
// itself when the box comes back, while disabling is a decision that must be
// made and unmade on purpose. A cascade ("endpoint disabled ⇒ models disabled")
// would silently rewrite that decision on the operator's behalf.

// ErrAlreadyExists marks a write refused by a uniqueness rule, so the HTTP layer
// can answer 409 instead of blaming the request (400) for a well-formed value
// that is merely taken. It is wrapped, never returned bare, so the message can
// name the row.
var ErrAlreadyExists = errors.New("already exists")

// The health a newly created endpoint starts in. Creation is not an observation:
// nobody has asked the endpoint anything yet, and placement requires health
// 'ok', so a new row is inert until Task 9's probe makes a claim about it. The
// alternative — creating rows healthy — would place work on an unverified URL
// with an unverified credential and discover the problem as a failed run.
const (
	healthUnknownOnCreate   = "unknown"
	healthReasonNeverProbed = "never probed"
)

// modelFeatureVocabulary is the closed set of models.features (spec §10:
// "Features are declared, not assumed"). Placement reads "tools" and "vision"
// by exact equality; "json_schema" and "streaming" are declarations an operator
// (and a later dashboard) can rely on. Anything else is refused here, because a
// JSON column has no CHECK constraint to catch it later.
var modelFeatureVocabulary = map[string]struct{}{
	"tools": {}, "vision": {}, "json_schema": {}, "streaming": {},
}

// parseFeatures validates and canonicalises a model's declared feature list:
// trimmed, de-duplicated and sorted, so the stored bytes depend only on the set.
// An empty list is legitimate (a model that offers nothing beyond plain text),
// but a name outside the vocabulary is an error naming the offending value and
// the vocabulary it was measured against.
func parseFeatures(raw []string) ([]string, error) {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, f := range raw {
		f = strings.TrimSpace(f)
		if f == "" {
			return nil, fmt.Errorf("empty feature (want one of %s)", strings.Join(featureNames(), ", "))
		}
		if _, ok := modelFeatureVocabulary[f]; !ok {
			return nil, fmt.Errorf("unknown feature %q (want one of %s)", f, strings.Join(featureNames(), ", "))
		}
		if _, dup := seen[f]; dup {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	sort.Strings(out)
	return out, nil
}

func featureNames() []string {
	out := make([]string, 0, len(modelFeatureVocabulary))
	for f := range modelFeatureVocabulary {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// checkProviderRegistered is the carry-forward this task owns: the provider
// vocabulary is the registry's name list, read at the moment of the write. A
// fourth adapter is one Register line in provider.NewRegistry and nothing in
// this package, and an adapter removed there stops being creatable here with no
// second list to forget.
//
// A nil registry is not a licence to accept anything: with no adapter to
// construct, every name is unknown, and failing closed is the same rule
// enrollAuth follows with an empty enrollment hash.
func checkProviderRegistered(reg *provider.Registry, name string) error {
	var names []string
	if reg != nil {
		names = reg.Names()
	}
	if len(names) == 0 {
		return fmt.Errorf("unknown provider %q (nothing is registered)", name)
	}
	for _, n := range names {
		if n == name {
			return nil
		}
	}
	// The registered names are part of the message: an unknown-provider error
	// that does not say what IS known leaves the operator guessing at spelling.
	return fmt.Errorf("unknown provider %q (registered: %s)", name, strings.Join(names, ", "))
}

// EndpointView is an endpoint row as the admin API shows it: the store's columns
// plus its models. It is deliberately not placement's Endpoint type, which is a
// narrow projection (id, name, provider, reach, health) that must stay free of
// anything placement does not use.
type EndpointView struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	Provider     string      `json:"provider"`
	BaseURL      string      `json:"base_url"`
	AuthRef      string      `json:"auth_ref,omitempty"`
	Reach        string      `json:"reach"`
	Health       string      `json:"health"`
	HealthReason string      `json:"health_reason,omitempty"`
	LastOKAt     *int64      `json:"last_ok_at,omitempty"`
	LatencyMS    *int64      `json:"latency_ms,omitempty"`
	CreatedAt    int64       `json:"created_at"`
	Models       []ModelView `json:"models"`
}

// ModelView is a models row as the admin API shows it: every column, because
// this is the operator's view of what will be placed, not the loader's.
type ModelView struct {
	ID             string   `json:"id"`
	EndpointID     string   `json:"endpoint_id"`
	ModelID        string   `json:"model_id"`
	Provides       []string `json:"provides"`
	Features       []string `json:"features"`
	ContextWindow  int      `json:"context_window"`
	CostInPerMtok  float64  `json:"cost_in_per_mtok"`
	CostOutPerMtok float64  `json:"cost_out_per_mtok"`
	Enabled        bool     `json:"enabled"`
}

type CreateEndpointRequest struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	AuthRef  string `json:"auth_ref"`
	Reach    string `json:"reach"`
}

// CreateEndpoint validates and stores one endpoint, then returns it as the API
// shows it. The returned health is always 'unknown': see healthUnknownOnCreate.
func CreateEndpoint(ctx context.Context, db *sql.DB, clock Clock, reg *provider.Registry, req CreateEndpointRequest) (EndpointView, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return EndpointView{}, errors.New("endpoint name is required")
	}
	// Not defaulted and not derived: an endpoint with no address is a row the
	// provider cannot construct, and every adapter needs one. Adapter-specific
	// URL rules are the adapter's to enforce (Task 9's probe is where an
	// unreachable URL becomes a visible health state).
	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL == "" {
		return EndpointView{}, errors.New("endpoint base_url is required")
	}
	authRef := strings.TrimSpace(req.AuthRef)
	reach, err := parseReach(req.Reach)
	if err != nil {
		return EndpointView{}, err
	}
	if err := checkProviderRegistered(reg, req.Provider); err != nil {
		return EndpointView{}, err
	}

	id, err := NewToken()
	if err != nil {
		return EndpointView{}, err
	}
	now := clock.Now()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return EndpointView{}, err
	}
	defer tx.Rollback()

	// The uniqueness pre-check is inside the write transaction, and the pool is
	// capped at one connection, so no other writer can insert between it and the
	// INSERT. The explicit check exists for the message: a bare constraint error
	// names a column, not what the operator did.
	var exists int
	switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM endpoints WHERE name=?`, name).Scan(&exists); {
	case err == nil:
		return EndpointView{}, fmt.Errorf("endpoint name %q: %w", name, ErrAlreadyExists)
	case !errors.Is(err, sql.ErrNoRows):
		return EndpointView{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO endpoints (id,name,provider,base_url,auth_ref,reach,health,health_reason,created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		id, name, req.Provider, baseURL, nullable(authRef), reach,
		healthUnknownOnCreate, healthReasonNeverProbed, now); err != nil {
		return EndpointView{}, fmt.Errorf("create endpoint %q: %w", name, err)
	}
	if err := AppendEvent(ctx, tx, clock, "admin", "endpoint", id, "created",
		map[string]any{"name": name, "provider": req.Provider, "reach": reach}); err != nil {
		return EndpointView{}, err
	}
	if err := tx.Commit(); err != nil {
		return EndpointView{}, err
	}

	return EndpointView{
		ID:           id,
		Name:         name,
		Provider:     req.Provider,
		BaseURL:      baseURL,
		AuthRef:      authRef,
		Reach:        reach,
		Health:       healthUnknownOnCreate,
		HealthReason: healthReasonNeverProbed,
		CreatedAt:    now,
		Models:       []ModelView{},
	}, nil
}

type CreateModelRequest struct {
	EndpointID     string   `json:"endpoint_id"`
	ModelID        string   `json:"model_id"`
	Provides       []string `json:"provides"`
	Features       []string `json:"features"`
	ContextWindow  int      `json:"context_window"`
	CostInPerMtok  float64  `json:"cost_in_per_mtok"`
	CostOutPerMtok float64  `json:"cost_out_per_mtok"`
	// Enabled defaults to true when absent. It is a pointer so "disabled on
	// creation" is expressible; an ordinary bool could not tell absent from
	// false and would create every model enabled by accident.
	Enabled *bool `json:"enabled"`
}

// CreateModel validates and stores one model under an existing endpoint. What
// the loader will later read is exactly what is validated here: providers are
// the capability vocabulary (caps.Parse), features are the model vocabulary, and
// the numbers are non-negative because placement ranks by cost and fits work by
// context window.
func CreateModel(ctx context.Context, db *sql.DB, clock Clock, req CreateModelRequest) (ModelView, error) {
	modelID := strings.TrimSpace(req.ModelID)
	if modelID == "" {
		return ModelView{}, errors.New("model_id is required")
	}
	if req.EndpointID == "" {
		return ModelView{}, errors.New("endpoint_id is required")
	}
	provides, err := caps.Parse(req.Provides)
	if err != nil {
		return ModelView{}, err
	}
	features, err := parseFeatures(req.Features)
	if err != nil {
		return ModelView{}, err
	}
	if err := checkModelNumbers(req.ContextWindow, req.CostInPerMtok, req.CostOutPerMtok); err != nil {
		return ModelView{}, err
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	id, err := NewToken()
	if err != nil {
		return ModelView{}, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ModelView{}, err
	}
	defer tx.Rollback()

	// The referenced endpoint must exist. Checked here rather than left to the
	// foreign key so the operator is told which endpoint is missing; the FK is
	// still the backstop.
	var one int
	switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM endpoints WHERE id=?`, req.EndpointID).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return ModelView{}, fmt.Errorf("unknown endpoint %q", req.EndpointID)
	case err != nil:
		return ModelView{}, err
	}

	switch err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM models WHERE endpoint_id=? AND model_id=?`, req.EndpointID, modelID).Scan(&one); {
	case err == nil:
		return ModelView{}, fmt.Errorf("model %q on endpoint %q: %w", modelID, req.EndpointID, ErrAlreadyExists)
	case !errors.Is(err, sql.ErrNoRows):
		return ModelView{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO models (id,endpoint_id,model_id,provides,features,context_window,
		                     cost_in_per_mtok,cost_out_per_mtok,enabled)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		id, req.EndpointID, modelID, marshalList(provides), marshalList(features),
		req.ContextWindow, req.CostInPerMtok, req.CostOutPerMtok, enabledInt(enabled)); err != nil {
		return ModelView{}, fmt.Errorf("create model %q: %w", modelID, err)
	}
	if err := AppendEvent(ctx, tx, clock, "admin", "model", id, "created",
		map[string]any{"endpoint_id": req.EndpointID, "model_id": modelID, "features": features}); err != nil {
		return ModelView{}, err
	}
	if err := tx.Commit(); err != nil {
		return ModelView{}, err
	}

	return ModelView{
		ID:             id,
		EndpointID:     req.EndpointID,
		ModelID:        modelID,
		Provides:       nonNilList(provides),
		Features:       nonNilList(features),
		ContextWindow:  req.ContextWindow,
		CostInPerMtok:  req.CostInPerMtok,
		CostOutPerMtok: req.CostOutPerMtok,
		Enabled:        enabled,
	}, nil
}

// PatchModelRequest is a partial update. Every field is a pointer because
// "absent" and "zero" are different instructions: disabling a model, setting a
// cost to 0 and leaving a column alone must be distinguishable, and a plain
// struct cannot tell them apart.
type PatchModelRequest struct {
	ModelID        *string   `json:"model_id"`
	Provides       *[]string `json:"provides"`
	Features       *[]string `json:"features"`
	ContextWindow  *int      `json:"context_window"`
	CostInPerMtok  *float64  `json:"cost_in_per_mtok"`
	CostOutPerMtok *float64  `json:"cost_out_per_mtok"`
	Enabled        *bool     `json:"enabled"`
}

// PatchModel applies a partial update to one model, validating every provided
// field first — the same vocabulary and the same rules as CreateModel, because a
// write path is a write path. enabled=0 (and its JSON spelling false) is the
// disable path; nothing here deletes.
//
// An unknown id wraps sql.ErrNoRows so the HTTP layer can answer 404, the way
// handleWhy already distinguishes "not there" from "cannot read it".
func PatchModel(ctx context.Context, db *sql.DB, clock Clock, id string, req PatchModelRequest) (ModelView, error) {
	if req.ModelID == nil && req.Provides == nil && req.Features == nil &&
		req.ContextWindow == nil && req.CostInPerMtok == nil &&
		req.CostOutPerMtok == nil && req.Enabled == nil {
		return ModelView{}, errors.New("patch model: no fields to change")
	}

	var newModelID string
	if req.ModelID != nil {
		newModelID = strings.TrimSpace(*req.ModelID)
		if newModelID == "" {
			return ModelView{}, errors.New("model_id must not be empty")
		}
	}
	var provides []string
	if req.Provides != nil {
		var err error
		if provides, err = caps.Parse(*req.Provides); err != nil {
			return ModelView{}, err
		}
	}
	var features []string
	if req.Features != nil {
		var err error
		if features, err = parseFeatures(*req.Features); err != nil {
			return ModelView{}, err
		}
	}
	if req.ContextWindow != nil && *req.ContextWindow < 0 {
		return ModelView{}, errors.New("context_window must not be negative")
	}
	if req.CostInPerMtok != nil && *req.CostInPerMtok < 0 {
		return ModelView{}, errors.New("cost_in_per_mtok must not be negative")
	}
	if req.CostOutPerMtok != nil && *req.CostOutPerMtok < 0 {
		return ModelView{}, errors.New("cost_out_per_mtok must not be negative")
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ModelView{}, err
	}
	defer tx.Rollback()

	cur := ModelView{}
	var rawProvides, rawFeatures string
	var enabled int
	if err := tx.QueryRowContext(ctx,
		`SELECT id,endpoint_id,model_id,provides,features,context_window,
		        cost_in_per_mtok,cost_out_per_mtok,enabled
		   FROM models WHERE id=?`, id).
		Scan(&cur.ID, &cur.EndpointID, &cur.ModelID, &rawProvides, &rawFeatures,
			&cur.ContextWindow, &cur.CostInPerMtok, &cur.CostOutPerMtok, &enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ModelView{}, fmt.Errorf("unknown model %q: %w", id, sql.ErrNoRows)
		}
		return ModelView{}, err
	}
	if cur.Provides, err = decodeList(rawProvides); err != nil {
		return ModelView{}, fmt.Errorf("models.provides for %s: %w", id, err)
	}
	if cur.Features, err = decodeList(rawFeatures); err != nil {
		return ModelView{}, fmt.Errorf("models.features for %s: %w", id, err)
	}
	cur.Enabled = enabled != 0

	// Merge: a provided field replaces, an absent one keeps what is stored. A
	// field only counts as changed when the value actually moves, because the
	// event's "changed" list and the no-op refusal below both read this list —
	// appending on presence alone would report an update that never happened.
	var changed []string
	if req.ModelID != nil {
		if newModelID != cur.ModelID {
			changed = append(changed, "model_id")
		}
		cur.ModelID = newModelID
	}
	if req.Provides != nil {
		if !slices.Equal(provides, cur.Provides) {
			changed = append(changed, "provides")
		}
		cur.Provides = provides
	}
	if req.Features != nil {
		if !slices.Equal(features, cur.Features) {
			changed = append(changed, "features")
		}
		cur.Features = features
	}
	if req.ContextWindow != nil {
		if *req.ContextWindow != cur.ContextWindow {
			changed = append(changed, "context_window")
		}
		cur.ContextWindow = *req.ContextWindow
	}
	if req.CostInPerMtok != nil {
		if *req.CostInPerMtok != cur.CostInPerMtok {
			changed = append(changed, "cost_in_per_mtok")
		}
		cur.CostInPerMtok = *req.CostInPerMtok
	}
	if req.CostOutPerMtok != nil {
		if *req.CostOutPerMtok != cur.CostOutPerMtok {
			changed = append(changed, "cost_out_per_mtok")
		}
		cur.CostOutPerMtok = *req.CostOutPerMtok
	}
	if req.Enabled != nil {
		if *req.Enabled != cur.Enabled {
			changed = append(changed, "enabled")
		}
		cur.Enabled = *req.Enabled
	}
	if len(changed) == 0 {
		// Every provided value equals the stored one. Refusing is honest: the
		// caller asked for a change and none happened, and a silent 200 plus an
		// "updated" event would claim otherwise.
		return ModelView{}, errors.New("patch model: the request changes nothing")
	}

	if slices.Contains(changed, "model_id") {
		var one int
		switch err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM models WHERE endpoint_id=? AND model_id=? AND id<>?`,
			cur.EndpointID, cur.ModelID, id).Scan(&one); {
		case err == nil:
			return ModelView{}, fmt.Errorf("model %q on endpoint %q: %w", cur.ModelID, cur.EndpointID, ErrAlreadyExists)
		case !errors.Is(err, sql.ErrNoRows):
			return ModelView{}, err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE models SET model_id=?, provides=?, features=?, context_window=?,
		        cost_in_per_mtok=?, cost_out_per_mtok=?, enabled=? WHERE id=?`,
		cur.ModelID, marshalList(cur.Provides), marshalList(cur.Features), cur.ContextWindow,
		cur.CostInPerMtok, cur.CostOutPerMtok, enabledInt(cur.Enabled), id); err != nil {
		return ModelView{}, fmt.Errorf("patch model %q: %w", id, err)
	}
	if err := AppendEvent(ctx, tx, clock, "admin", "model", id, "updated",
		map[string]any{"endpoint_id": cur.EndpointID, "model_id": cur.ModelID, "changed": changed}); err != nil {
		return ModelView{}, err
	}
	if err := tx.Commit(); err != nil {
		return ModelView{}, err
	}
	cur.Provides = nonNilList(cur.Provides)
	cur.Features = nonNilList(cur.Features)
	return cur, nil
}

// ListEndpoints returns every endpoint with its models and current health.
//
// Disabled models are included on purpose: the listing is what an operator
// reads when work stops, and a model that vanished from the view would be
// indistinguishable from one that was never created. The order is endpoint name
// then model id, so the answer does not depend on insertion order — the store
// has no ORDER BY of its own and the dashboard would otherwise render an
// arbitrary order.
//
// One LEFT JOIN rather than two queries: a model created between an endpoint
// query and a model query would otherwise be invisible or attached to a stale
// endpoint list. An endpoint with no models still appears, with an empty list.
func ListEndpoints(ctx context.Context, db *sql.DB) ([]EndpointView, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT e.id, e.name, e.provider, e.base_url, COALESCE(e.auth_ref,''), e.reach,
		        e.health, COALESCE(e.health_reason,''), e.last_ok_at, e.latency_ms, e.created_at,
		        m.id, m.model_id, m.provides, m.features, m.context_window,
		        m.cost_in_per_mtok, m.cost_out_per_mtok, m.enabled
		   FROM endpoints e
		   LEFT JOIN models m ON m.endpoint_id = e.id
		  ORDER BY e.name, m.model_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []EndpointView{}
	var cur *EndpointView
	for rows.Next() {
		var (
			ep        EndpointView
			lastOK    sql.NullInt64
			latency   sql.NullInt64
			mID       sql.NullString
			mModelID  sql.NullString
			mProvides sql.NullString
			mFeatures sql.NullString
			mContext  sql.NullInt64
			mCostIn   sql.NullFloat64
			mCostOut  sql.NullFloat64
			mEnabled  sql.NullInt64
		)
		if err := rows.Scan(
			&ep.ID, &ep.Name, &ep.Provider, &ep.BaseURL, &ep.AuthRef, &ep.Reach,
			&ep.Health, &ep.HealthReason, &lastOK, &latency, &ep.CreatedAt,
			&mID, &mModelID, &mProvides, &mFeatures, &mContext,
			&mCostIn, &mCostOut, &mEnabled,
		); err != nil {
			return nil, err
		}
		if lastOK.Valid {
			v := lastOK.Int64
			ep.LastOKAt = &v
		}
		if latency.Valid {
			v := latency.Int64
			ep.LatencyMS = &v
		}
		ep.Models = []ModelView{}

		if cur == nil || cur.ID != ep.ID {
			out = append(out, ep)
			cur = &out[len(out)-1]
		}
		if !mID.Valid {
			continue // an endpoint with no models
		}
		m := ModelView{
			ID:             mID.String,
			EndpointID:     cur.ID,
			ModelID:        mModelID.String,
			ContextWindow:  int(mContext.Int64),
			CostInPerMtok:  mCostIn.Float64,
			CostOutPerMtok: mCostOut.Float64,
			Enabled:        mEnabled.Int64 != 0,
		}
		if m.Provides, err = decodeList(mProvides.String); err != nil {
			return nil, fmt.Errorf("models.provides for %s: %w", m.ID, err)
		}
		if m.Features, err = decodeList(mFeatures.String); err != nil {
			return nil, fmt.Errorf("models.features for %s: %w", m.ID, err)
		}
		m.Provides, m.Features = nonNilList(m.Provides), nonNilList(m.Features)
		cur.Models = append(cur.Models, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// checkModelNumbers refuses the values that would make a model win or lose the
// placement auction for a reason nobody intended: a negative cost always sorts
// first, and a negative context window can never satisfy a tokens: need.
func checkModelNumbers(contextWindow int, costIn, costOut float64) error {
	if contextWindow < 0 {
		return errors.New("context_window must not be negative")
	}
	if costIn < 0 {
		return errors.New("cost_in_per_mtok must not be negative")
	}
	if costOut < 0 {
		return errors.New("cost_out_per_mtok must not be negative")
	}
	return nil
}

// enabledInt stores a Go bool in models.enabled, which is INTEGER. Written
// explicitly rather than relying on the driver's bool conversion so the stored
// value is 0 or 1 whatever the driver does.
func enabledInt(on bool) int {
	if on {
		return 1
	}
	return 0
}

// nonNilList keeps a JSON list a list. decodeList returns nil for an empty
// stored value, and a nil slice encodes as null, which is not what a caller
// reading "provides": [] expects.
func nonNilList(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
