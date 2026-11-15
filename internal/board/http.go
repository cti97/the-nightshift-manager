package board

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/cti97/the-nightshift-manager/internal/provider"
	"github.com/cti97/the-nightshift-manager/internal/version"
)

type Server struct {
	db    *sql.DB
	clock Clock
	// providers is the same registry the executor will construct endpoints
	// from, so an endpoint row this API accepts is one the runtime can build an
	// adapter for. It is read, never mutated here; nil means nothing is
	// registered and every create is refused (fail closed).
	providers *provider.Registry
	// adminTokenHash is the credential the operator's CLI/UI carries. Handlers
	// never see the plaintext; they compare the hash of the bearer token.
	adminTokenHash string
	// enrollTokenHash is a credential that can do exactly one thing: register a
	// node. It is deliberately not the admin token (spec §15 read register as a
	// node-token endpoint, which cannot bootstrap itself): an enrolling machine
	// must never hold the credential that can create items, read every brief and
	// drive the admin API.
	enrollTokenHash string
}

// NewServer wires the API to the board, the clock and the provider registry the
// admin write path validates endpoint rows against. providers may be nil: a nil
// registry refuses every endpoint creation (nothing is registered) rather than
// accepting a name it cannot check.
func NewServer(db *sql.DB, clock Clock, providers *provider.Registry, adminTokenHash, enrollTokenHash string) *Server {
	return &Server{
		db:              db,
		clock:           clock,
		providers:       providers,
		adminTokenHash:  adminTokenHash,
		enrollTokenHash: enrollTokenHash,
	}
}

// HashToken is the only form in which a credential is stored or compared.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// NewToken returns a 32-byte random credential. It is shown to the caller once.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", s.handleVersion)
	mux.Handle("GET /admin/board", s.adminAuth(http.HandlerFunc(s.handleBoard)))
	mux.Handle("GET /admin/spend/today", s.adminAuth(http.HandlerFunc(s.handleSpendToday)))
	mux.Handle("PUT /admin/spend/ceiling", s.adminAuth(http.HandlerFunc(s.handleSetSpendCeiling)))
	mux.Handle("POST /admin/items", s.adminAuth(http.HandlerFunc(s.handleCreateItem)))
	mux.Handle("POST /admin/endpoints", s.adminAuth(http.HandlerFunc(s.handleCreateEndpoint)))
	mux.Handle("GET /admin/endpoints", s.adminAuth(http.HandlerFunc(s.handleListEndpoints)))
	mux.Handle("POST /admin/endpoints/{id}/probe", s.adminAuth(http.HandlerFunc(s.handleProbeEndpoint)))
	mux.Handle("POST /admin/models", s.adminAuth(http.HandlerFunc(s.handleCreateModel)))
	mux.Handle("PATCH /admin/models/{id}", s.adminAuth(http.HandlerFunc(s.handlePatchModel)))
	mux.Handle("POST /nodes/register", s.enrollAuth(http.HandlerFunc(s.handleRegister)))
	mux.Handle("POST /nodes/heartbeat", s.nodeAuth(http.HandlerFunc(s.handleHeartbeat)))
	mux.Handle("POST /claim", s.nodeAuth(http.HandlerFunc(s.handleClaim)))
	mux.Handle("POST /runs/{id}/start", s.nodeAuth(http.HandlerFunc(s.handleStartRun)))
	mux.Handle("POST /runs/{id}/report", s.nodeAuth(http.HandlerFunc(s.handleReport)))
	mux.Handle("GET /admin/why/{id}", s.adminAuth(http.HandlerFunc(s.handleWhy)))
	return mux
}

func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearer(r)
		if !ok || HashToken(tok) != s.adminTokenHash {
			WriteError(w, http.StatusUnauthorized, "unauthorized", "admin token required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearer(r *http.Request) (string, bool) {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(p) || h[:len(p)] != p {
		return "", false
	}
	return h[len(p):], true
}

// enrollAuth gates registration with the enrollment credential. An empty enroll
// hash authenticates nothing: a manager started without an enrollment secret
// must not mint node tokens, because every token it mints can claim work and
// read the operator's prose. Failing closed is the only safe default.
func (s *Server) enrollAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearer(r)
		if !ok || s.enrollTokenHash == "" || HashToken(tok) != s.enrollTokenHash {
			WriteError(w, http.StatusUnauthorized, "unauthorized", "enrollment token required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type ctxKey string

const nodeIDKey ctxKey = "node_id"

// nodeAuth resolves a bearer token to a node id. The token itself is never
// stored, so this compares hashes and nothing else.
func (s *Server) nodeAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearer(r)
		if !ok {
			WriteError(w, http.StatusUnauthorized, "unauthorized", "node token required")
			return
		}
		var id string
		err := s.db.QueryRowContext(r.Context(),
			`SELECT id FROM nodes WHERE token_hash = ?`, HashToken(tok)).Scan(&id)
		if err != nil {
			WriteError(w, http.StatusUnauthorized, "unauthorized", "unknown node token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), nodeIDKey, id)))
	})
}

func nodeIDFrom(r *http.Request) string {
	id, _ := r.Context().Value(nodeIDKey).(string)
	return id
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	id, token, err := RegisterNode(r.Context(), s.db, s.clock, req)
	if err != nil {
		WriteError(w, http.StatusConflict, "register", err.Error())
		return
	}
	// The plaintext token is returned exactly once, here.
	WriteJSON(w, http.StatusCreated, RegisterResponse{NodeID: id, Token: token})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	resp, err := Heartbeat(r.Context(), s.db, s.clock, nodeIDFrom(r), req)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "heartbeat", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, resp)
}

// handleClaim is how a node asks for work. The node id comes from the bearer
// token, never from the body: a node cannot claim on another node's behalf.
func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req ClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	items, err := Claim(r.Context(), s.db, s.clock, nodeIDFrom(r), req.FreeSlots)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "claim", err.Error())
		return
	}
	// An idle node gets [] rather than null; both decode, only one is a list.
	if items == nil {
		items = []ClaimedItem{}
	}
	WriteJSON(w, http.StatusOK, items)
}

// handleStartRun is the node's "I have begun executing" call. The run id comes
// from the path and the node id from the bearer token, so ownership is part of
// the request rather than something the body could claim.
func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	if err := StartRun(r.Context(), s.db, s.clock, r.PathValue("id"), nodeIDFrom(r)); err != nil {
		WriteError(w, http.StatusConflict, "start", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

// handleReport is the node's final word on a run. A retry after the first
// report is a 200 with applied=false, not an error: the node cannot tell which
// of its calls settled the item, and only the first one did.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	var req ReportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	res, err := Report(r.Context(), s.db, s.clock, r.PathValue("id"), nodeIDFrom(r), req)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "report", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

// handleWhy is the operator's explanation endpoint. It returns the whole
// placement decision, rejects included, because the summary reason alone does
// not say which rule failed for which node.
//
// An unknown item is a 404. Anything else — a corrupt needs column, a broken
// board — is a 500 that names what could not be read: the item exists, and
// answering "not found" would send the operator looking for a row that is right
// there, at the moment they most need the explainer.
func (s *Server) handleWhy(w http.ResponseWriter, r *http.Request) {
	ex, err := Explain(r.Context(), s.db, s.clock, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			WriteError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		WriteError(w, http.StatusInternalServerError, "why", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, ex)
}

func (s *Server) handleCreateItem(w http.ResponseWriter, r *http.Request) {
	var req CreateItemRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	id, err := CreateItem(r.Context(), s.db, s.clock, req)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "create_item", err.Error())
		return
	}
	WriteJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// createEndpointResponse is EndpointView plus the one instruction creation owes
// the operator (Task 9 decision 6): a new endpoint is inert until a probe makes
// a claim about it, and a 201 that does not say so leaves the operator with a
// row that never places and no next step. The hint is create-only — the listing
// is a view of what exists, not a list of things to do — which is why it lives
// in a wrapper here rather than on EndpointView. The embedding keeps the body's
// original shape, so a client that decodes the endpoint still does.
type createEndpointResponse struct {
	EndpointView
	Next string `json:"next"`
}

// probePath builds the hint's URL. The route pattern above spells {id} because
// ServeMux requires it; the test that probes through the hinted path is what
// keeps the two spellings in agreement.
func probePath(endpointID string) string {
	return "/admin/endpoints/" + endpointID + "/probe"
}

// handleCreateEndpoint is the operator's way into the endpoints table. The
// provider name is checked against the registry the server was built with, so a
// typo is a 400 here rather than a row that never places.
func (s *Server) handleCreateEndpoint(w http.ResponseWriter, r *http.Request) {
	var req CreateEndpointRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	ep, err := CreateEndpoint(r.Context(), s.db, s.clock, s.providers, req)
	if err != nil {
		writeAdminWriteError(w, "create_endpoint", err)
		return
	}
	WriteJSON(w, http.StatusCreated, createEndpointResponse{
		EndpointView: ep,
		Next:         "POST " + probePath(ep.ID),
	})
}

// handleProbeEndpoint is the on-demand health check (Task 9 decision 5: on
// demand first, scheduled second). It is the only route that writes health, and
// a 200 means "the probe ran and recorded a state" — including `down`, and
// including an adapter that could not be built (decision 2), because both are
// conclusions about the endpoint. A 500 here means the board could not record
// anything at all, which is the only case the operator cannot act on by reading
// the endpoint.
func (s *Server) handleProbeEndpoint(w http.ResponseWriter, r *http.Request) {
	res, err := ProbeEndpoint(r.Context(), s.db, s.clock, s.providers, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			WriteError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		WriteError(w, http.StatusInternalServerError, "probe", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

// handleListEndpoints is the read side: every endpoint with its models and its
// current health. It is a GET, so it takes no body and writes nothing.
func (s *Server) handleListEndpoints(w http.ResponseWriter, r *http.Request) {
	eps, err := ListEndpoints(r.Context(), s.db)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "endpoints", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, eps)
}

func (s *Server) handleCreateModel(w http.ResponseWriter, r *http.Request) {
	var req CreateModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	m, err := CreateModel(r.Context(), s.db, s.clock, req)
	if err != nil {
		writeAdminWriteError(w, "create_model", err)
		return
	}
	WriteJSON(w, http.StatusCreated, m)
}

// handlePatchModel is the update path, and the only disable path: enabled=0.
// There is no DELETE route — see the endpoint-deletion note in endpoints.go's
// file comment and the Task 8 report.
func (s *Server) handlePatchModel(w http.ResponseWriter, r *http.Request) {
	var req PatchModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	m, err := PatchModel(r.Context(), s.db, s.clock, r.PathValue("id"), req)
	if err != nil {
		writeAdminWriteError(w, "patch_model", err)
		return
	}
	WriteJSON(w, http.StatusOK, m)
}

// writeAdminWriteError maps the store's three outcomes onto the three answers an
// operator can act on: the row is not there (404), the value is well-formed but
// taken (409), or the request itself is wrong (400). The code names the
// operation, matching every other handler's envelope.
func writeAdminWriteError(w http.ResponseWriter, code string, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		WriteError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ErrAlreadyExists):
		WriteError(w, http.StatusConflict, "conflict", err.Error())
	default:
		WriteError(w, http.StatusBadRequest, code, err.Error())
	}
}

// handleSpendToday is the operator's spend ledger: what today's runs cost, the
// ceiling it is measured against, and whether the board is refusing work. It is
// a read of `runs` on every call — there is no running total to go stale, so the
// number here is the same number the claim gate acts on.
func (s *Server) handleSpendToday(w http.ResponseWriter, r *http.Request) {
	st, err := TodaySpend(r.Context(), s.db, s.clock)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "spend", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, st)
}

// setCeilingRequest requires the field: 0 is a meaningful value (unlimited), so
// an absent ceiling must be distinguishable from an explicit zero. A plain
// float64 would let a body that forgot the field quietly disable the ceiling.
type setCeilingRequest struct {
	Ceiling *float64 `json:"ceiling"`
}

// handleSetSpendCeiling writes the daily limit and answers with the resulting
// ledger, so the operator sees immediately whether raising the ceiling above
// today's spend resumed the board or whether it is still reached.
func (s *Server) handleSetSpendCeiling(w http.ResponseWriter, r *http.Request) {
	var req setCeilingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if req.Ceiling == nil {
		WriteError(w, http.StatusBadRequest, "ceiling", "ceiling is required (0 means unlimited)")
		return
	}
	st, err := SetDailySpendCeiling(r.Context(), s.db, s.clock, *req.Ceiling)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "set_ceiling", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, st)
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	v, c := version.Info()
	WriteJSON(w, http.StatusOK, map[string]any{
		"version":        v,
		"commit":         c,
		"protocol_min":   ProtocolMin,
		"protocol_max":   ProtocolMax,
		"schema_version": SchemaVersion(),
	})
}

// workItemStatuses is every value schema.sql's CHECK constraint permits on
// work_items.status, in the order the dashboard renders them. The board
// handler zero-fills each one so the response shape does not depend on which
// statuses happen to have rows. This is the single place a new schema status
// must be added; TestWorkItemStatusesMatchSchema fails if it drifts.
var workItemStatuses = []string{
	"queued", "claimed", "running", "needs_approval", "done", "failed", "cancelled",
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT status, COUNT(*) FROM work_items GROUP BY status`)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			WriteError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		counts[status] = n
	}
	// Next returning false means "done" or "the driver failed midway"; without
	// this the handler cannot tell, and would report a partial map as truth.
	if err := rows.Err(); err != nil {
		WriteError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	for _, status := range workItemStatuses {
		if _, ok := counts[status]; !ok {
			counts[status] = 0
		}
	}
	WriteJSON(w, http.StatusOK, counts)
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func WriteError(w http.ResponseWriter, status int, code, msg string) {
	WriteJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// Errorf keeps handler error construction uniform.
func Errorf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

// logf is the background loops' only way to speak; handlers answer in JSON.
func (s *Server) logf(format string, a ...any) { log.Printf(format, a...) }
