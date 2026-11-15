package board

import (
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

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	v, c := version.Info()
	WriteJSON(w, http.StatusOK, map[string]any{
		"version":        v,
		"commit":         c,
		"protocol_min":   1,
		"protocol_max":   1,
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
