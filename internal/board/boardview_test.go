package board

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// boardFixture is a board with one item in each interesting state.
func boardFixture(t *testing.T) (*Server, map[string]string) {
	t.Helper()
	db, clock := newTestDB(t)
	srv := NewServer(db, clock, nil, HashToken(testAdminToken), HashToken(testEnrollToken))
	ctx := context.Background()
	ids := map[string]string{}
	for _, title := range []string{"a queued job", "a finished job"} {
		id, err := CreateItem(ctx, db, clock, CreateItemRequest{Title: title, Brief: "b"})
		if err != nil {
			t.Fatalf("CreateItem: %v", err)
		}
		ids[title] = id
	}
	if _, err := db.Exec(`UPDATE work_items SET status='done' WHERE id=?`, ids["a finished job"]); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := db.Exec(`UPDATE work_items SET status='needs_approval', approval_request=?
		WHERE id=?`, `{"action":"deploy","reason":"because"}`, ids["a queued job"]); err != nil {
		t.Fatalf("park: %v", err)
	}
	return srv, ids
}

func getPage(t *testing.T, srv *Server, path string, html bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	if html {
		req.Header.Set("Accept", "text/html")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestTheBoardPageShowsEveryItemWithAWayIn: the front door lists the work and links into it, with the
// parked item first because that is what the operator came for.
func TestTheBoardPageShowsEveryItemWithAWayIn(t *testing.T) {
	srv, ids := boardFixture(t)
	rec := getPage(t, srv, "/admin", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("page = %d (%s)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<title>The board", "Waiting on a human", "a queued job", "a finished job",
		`href="/admin/items/` + ids["a queued job"] + `"`,
		"1</b> waiting on a human",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the page does not contain %q:\n%s", want, body[:min(len(body), 1200)])
		}
	}
	// Parked before done: a page whose sections sort arbitrarily is a page read twice.
	// The label renders as `Done <span class="meta">`, not `>Done<`: match the heading text, not a tag.
	if strings.Index(body, "Waiting on a human") > strings.Index(body, ">Done <") {
		t.Fatal("the parked section renders after the done section")
	}
}

// TestTheBoardAnswersJSONToACaller: the same route is the machine's view, and it must carry the items
// and the ledger, not only the counts the older /admin/board route reports.
func TestTheBoardAnswersJSONToACaller(t *testing.T) {
	srv, _ := boardFixture(t)
	rec := getPage(t, srv, "/admin", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("json = %d", rec.Code)
	}
	var view BoardView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(view.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(view.Items))
	}
	if view.Counts["needs_approval"] != 1 || view.Counts["done"] != 1 {
		t.Fatalf("counts = %v", view.Counts)
	}
	if view.Items[0].Status != "needs_approval" {
		t.Fatalf("first item is %q, want the parked one", view.Items[0].Status)
	}
	if view.Spend.Day == "" {
		t.Fatal("the view does not carry the ledger")
	}
}

// TestAnEmptyBoardSaysSo: a blank page and a broken page must not look the same.
func TestAnEmptyBoardSaysSo(t *testing.T) {
	srv, _ := newServer(t)
	rec := getPage(t, srv, "/admin", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("page = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "The board is empty") {
		t.Fatal("an empty board does not say it is empty")
	}
}

// TestTheBoardPageNeedsTheAdminToken: the page lists every title on the board.
func TestTheBoardPageNeedsTheAdminToken(t *testing.T) {
	srv := NewServer(nil, nil, nil, HashToken(testAdminToken), HashToken(testEnrollToken))
	_ = srv
	for _, path := range []string{"/admin"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without a token = %d, want 401", path, rec.Code)
		}
	}
}

// TestTheBoardPageTruncatesOutLoud: a board with more items than the page shows must say so rather
// than silently showing less.
func TestTheBoardPageTruncatesOutLoud(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if _, err := CreateItem(ctx, db, clock, CreateItemRequest{Title: "job", Brief: "b"}); err != nil {
			t.Fatalf("CreateItem %d: %v", i, err)
		}
	}
	view, err := ReadBoard(ctx, db, clock, 2)
	if err != nil {
		t.Fatalf("ReadBoard: %v", err)
	}
	if len(view.Items) != 2 || !view.Truncated {
		t.Fatalf("items = %d truncated = %v, want 2 and true", len(view.Items), view.Truncated)
	}
}

// TestTheParkedItemSurvivesTruncation: the ORDER BY decides which items a full page keeps, and the
// one item it must never drop is the one waiting on a human. This is why the order is a contract and
// not a display preference.
func TestTheParkedItemSurvivesTruncation(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	// The parked item is created FIRST, so a page ordered by insertion would keep the two newer ones
	// and drop the only one that needs anybody.
	parked, err := CreateItem(ctx, db, clock, CreateItemRequest{Title: "needs a human", Brief: "b"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if _, err := db.Exec(`UPDATE work_items SET status='needs_approval', approval_request=?
		WHERE id=?`, `{"action":"x","reason":"y"}`, parked); err != nil {
		t.Fatalf("park: %v", err)
	}
	for i := 0; i < 5; i++ {
		// Distinct timestamps, or every item ties on created_at and any order looks like the right one.
		clock.Advance(time.Minute)
		if _, err := CreateItem(ctx, db, clock, CreateItemRequest{Title: "queued job", Brief: "b"}); err != nil {
			t.Fatalf("CreateItem queued %d: %v", i, err)
		}
	}
	view, err := ReadBoard(ctx, db, clock, 2)
	if err != nil {
		t.Fatalf("ReadBoard: %v", err)
	}
	if !view.Truncated {
		t.Fatal("the view is not marked truncated")
	}
	found := false
	for _, it := range view.Items {
		if it.ID == parked {
			found = true
		}
	}
	if !found {
		t.Fatalf("the parked item was truncated away: %+v", view.Items)
	}
}
