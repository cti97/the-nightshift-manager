package board

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newServer(t *testing.T) (*Server, *FixedClock) {
	t.Helper()
	db, clock := newTestDB(t)
	srv := NewServer(db, clock, HashToken("admin-secret"))
	return srv, clock
}

func do(t *testing.T, h http.Handler, method, path, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestVersionIsPublic(t *testing.T) {
	srv, _ := newServer(t)
	rec := do(t, srv.Handler(), "GET", "/version", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestBoardRequiresToken(t *testing.T) {
	srv, _ := newServer(t)
	if rec := do(t, srv.Handler(), "GET", "/admin/board", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec := do(t, srv.Handler(), "GET", "/admin/board", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a wrong token", rec.Code)
	}
}

func TestBoardWithAdminTokenReturnsCounts(t *testing.T) {
	srv, _ := newServer(t)
	rec := do(t, srv.Handler(), "GET", "/admin/board", "admin-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["queued"]; !ok {
		t.Fatalf("board counts = %v, want a queued key", body)
	}
}
