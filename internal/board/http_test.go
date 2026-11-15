package board

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/cti97/the-nightshift-manager/internal/provider"
)

// The two credentials are deliberately different values here, because the
// board's whole enrollment story is that they are different credentials: the
// admin token drives the admin API, the enroll token can only register a node.
const (
	testAdminToken  = "admin-secret"
	testEnrollToken = "enroll-secret"
)

func newServer(t *testing.T) (*Server, *FixedClock) {
	t.Helper()
	db, clock := newTestDB(t)
	srv := NewServer(db, clock, provider.NewRegistry(), HashToken(testAdminToken), HashToken(testEnrollToken))
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

// boardStatuses is every status schema.sql's CHECK constraint permits on
// work_items.status, restated here independently of the production list so a
// status cannot be dropped from the response without this test failing.
var boardStatuses = []string{"queued", "claimed", "running", "needs_approval", "done", "failed", "cancelled"}

func decodeCounts(t *testing.T, rec *httptest.ResponseRecorder) map[string]int {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func TestBoardReturnsEverySchemaStatusZeroOnEmptyDB(t *testing.T) {
	srv, _ := newServer(t)
	body := decodeCounts(t, do(t, srv.Handler(), "GET", "/admin/board", "admin-secret"))
	if len(body) != len(boardStatuses) {
		t.Fatalf("counts = %v, want exactly the %d schema statuses", body, len(boardStatuses))
	}
	for _, status := range boardStatuses {
		n, ok := body[status]
		if !ok {
			t.Fatalf("counts = %v, missing %q", body, status)
		}
		if n != 0 {
			t.Fatalf("counts[%q] = %d, want 0 on an empty board", status, n)
		}
	}
}

func TestBoardCountsStoredStatuses(t *testing.T) {
	srv, clock := newServer(t)
	ctx := context.Background()
	for i, status := range []string{"queued", "queued", "done", "cancelled"} {
		_, err := srv.db.ExecContext(ctx,
			`INSERT INTO work_items (id, title, brief, status, created_by, created_at, updated_at)
			 VALUES (?, ?, 'brief', ?, 'admin', ?, ?)`,
			fmt.Sprintf("w%d", i), "item", status, clock.Now(), clock.Now())
		if err != nil {
			t.Fatalf("insert %s: %v", status, err)
		}
	}

	body := decodeCounts(t, do(t, srv.Handler(), "GET", "/admin/board", "admin-secret"))
	want := map[string]int{"queued": 2, "done": 1, "cancelled": 1}
	for _, status := range boardStatuses {
		expected := want[status]
		if body[status] != expected {
			t.Fatalf("counts[%q] = %d, want %d (body %v)", status, body[status], expected, body)
		}
	}
	if len(body) != len(boardStatuses) {
		t.Fatalf("counts = %v, want exactly the %d schema statuses", body, len(boardStatuses))
	}
}

func TestBoardDatabaseErrorReturnsJSONEnvelope(t *testing.T) {
	srv, _ := newServer(t)
	// A closed pool makes the query fail; the handler must answer 500 rather
	// than panic or return a partial map as truth.
	if err := srv.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	rec := do(t, srv.Handler(), "GET", "/admin/board", "admin-secret")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "db")
}

func TestBoardUnauthorizedReturnsJSONErrorEnvelope(t *testing.T) {
	srv, _ := newServer(t)
	for _, bearer := range []string{"", "wrong"} {
		rec := do(t, srv.Handler(), "GET", "/admin/board", bearer)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("bearer %q: status = %d, want 401", bearer, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("bearer %q: content-type = %q, want application/json", bearer, ct)
		}
		assertErrorCode(t, rec, "unauthorized")
	}
}

func TestVersionBodyCarriesNegotiationFields(t *testing.T) {
	srv, _ := newServer(t)
	rec := do(t, srv.Handler(), "GET", "/version", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"version", "commit", "protocol_min", "protocol_max", "schema_version"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("body = %v, missing %q", body, key)
		}
	}
	if body["protocol_min"] != float64(1) || body["protocol_max"] != float64(1) {
		t.Fatalf("protocol range = %v..%v, want 1..1", body["protocol_min"], body["protocol_max"])
	}
	if body["schema_version"] != float64(SchemaVersion()) {
		t.Fatalf("schema_version = %v, want %d", body["schema_version"], SchemaVersion())
	}
}

// assertErrorCode decodes the error envelope and checks its code.
func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope: %v (body %s)", err, rec.Body.String())
	}
	if body.Error.Code != want {
		t.Fatalf("error code = %q, want %q (body %s)", body.Error.Code, want, rec.Body.String())
	}
	if body.Error.Message == "" {
		t.Fatalf("error message is empty (body %s)", rec.Body.String())
	}
}

// statusesInSchema reads the statuses the embedded schema permits on
// work_items, so workItemStatuses cannot drift from the CHECK constraint.
func statusesInSchema(t *testing.T) []string {
	t.Helper()
	const (
		table  = "CREATE TABLE work_items"
		marker = "CHECK (status IN ("
	)
	ti := strings.Index(schema, table)
	if ti < 0 {
		t.Fatalf("embedded schema has no %q", table)
	}
	ci := strings.Index(schema[ti:], marker)
	if ci < 0 {
		t.Fatalf("embedded schema's %q has no %q", table, marker)
	}
	rest := schema[ti+ci+len(marker):]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatalf("unterminated status CHECK in embedded schema: %q", rest)
	}
	var got []string
	for _, part := range strings.Split(rest[:end], ",") {
		status := strings.Trim(strings.TrimSpace(part), "'")
		if status == "" {
			t.Fatalf("empty status in schema CHECK: %q", rest[:end])
		}
		got = append(got, status)
	}
	return got
}

func TestWorkItemStatusesMatchSchema(t *testing.T) {
	fromSchema := statusesInSchema(t)
	production := append([]string{}, workItemStatuses...)
	sort.Strings(production)
	sort.Strings(fromSchema)
	if !slices.Equal(production, fromSchema) {
		t.Fatalf("workItemStatuses = %v, schema allows %v", workItemStatuses, fromSchema)
	}
}
