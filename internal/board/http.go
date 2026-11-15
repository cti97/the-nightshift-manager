package board

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/cti97/the-nightshift-manager/internal/version"
)

type Server struct {
	db             *sql.DB
	clock          Clock
	adminTokenHash string
}

func NewServer(db *sql.DB, clock Clock, adminTokenHash string) *Server {
	return &Server{db: db, clock: clock, adminTokenHash: adminTokenHash}
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
	mux.Handle("POST /admin/items", s.adminAuth(http.HandlerFunc(s.handleCreateItem)))
	mux.HandleFunc("POST /nodes/register", s.handleRegister)
	mux.Handle("POST /nodes/heartbeat", s.nodeAuth(http.HandlerFunc(s.handleHeartbeat)))
	mux.Handle("POST /claim", s.nodeAuth(http.HandlerFunc(s.handleClaim)))
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
