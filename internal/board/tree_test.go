package board

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// treeFixture is a parent with two agent-filed children, one of which has a child of its own.
func treeFixture(t *testing.T) (srv *Server, parentID string) {
	t.Helper()
	db, clock, node, parentID, runID := filingFixture(t)
	srv = NewServer(db, clock, nil, HashToken(testAdminToken), HashToken(testEnrollToken))
	ctx := context.Background()
	first, err := FileItem(ctx, db, clock, runID, node, FileItemRequest{Title: "first child", Brief: "b", Needs: []string{"agentic"}, ClientRef: "c1"})
	if err != nil {
		t.Fatalf("file first: %v", err)
	}
	second, err := FileItem(ctx, db, clock, runID, node, FileItemRequest{Title: "second child", Brief: "b", Needs: []string{"agentic"}, ClientRef: "c2"})
	if err != nil {
		t.Fatalf("file second: %v", err)
	}
	// A grandchild: claim the first child so a run exists to file from it.
	if _, err := db.Exec(`UPDATE work_items SET status='queued' WHERE id=?`, first.ItemID); err != nil {
		t.Fatalf("requeue first: %v", err)
	}
	claimed, err := Claim(ctx, db, clock, node, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim the child: %d, %v", len(claimed), err)
	}
	if _, err := FileItem(ctx, db, clock, claimed[0].RunID, node, FileItemRequest{
		Title: "grandchild", Brief: "b", Needs: []string{"agentic"}, ClientRef: "c3",
	}); err != nil {
		t.Fatalf("file grandchild: %v", err)
	}
	_ = second
	return srv, parentID
}

// TestTheTreeShowsEveryDescendantWithItsProvenance: five rows with a parent_id become a tree the
// operator can read, and every child says who filed it and from which run.
func TestTheTreeShowsEveryDescendantWithItsProvenance(t *testing.T) {
	srv, parentID := treeFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/items/"+parentID, nil)
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tree = %d (%s)", rec.Code, rec.Body)
	}
	tree, err := ItemTree(context.Background(), srv.db, parentID)
	if err != nil {
		t.Fatalf("ItemTree: %v", err)
	}
	if len(tree.Children) != 2 {
		t.Fatalf("children = %d, want 2", len(tree.Children))
	}
	if tree.Children[0].Title != "first child" {
		t.Fatalf("children are not oldest-first: %q", tree.Children[0].Title)
	}
	if tree.Children[0].CreatedBy == "" || !strings.HasPrefix(tree.Children[0].CreatedBy, "agent:") {
		t.Fatalf("the child does not say who filed it: %q", tree.Children[0].CreatedBy)
	}
	if tree.Children[0].Depth != 1 {
		t.Fatalf("child depth = %d, want 1", tree.Children[0].Depth)
	}
	grand := tree.Children[0].Children
	if len(grand) != 1 || grand[0].Depth != 2 {
		t.Fatalf("grandchild = %+v, want one at depth 2", grand)
	}
	if got := CountDescendants(tree); got != 3 {
		t.Fatalf("descendants = %d, want 3", got)
	}
	// A leaf's children are [] rather than null, so a client can tell "none" from "missing".
	if tree.Children[1].Children == nil {
		t.Fatal("a child with no children marshals as null")
	}
}

// TestTheTreePageRendersForABrowser: the same route answers HTML when the caller asks for it, because
// the operator's window on agent-filed work is a page, not a JSON blob.
func TestTheTreePageRendersForABrowser(t *testing.T) {
	srv, parentID := treeFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/items/"+parentID, nil)
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("page = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"<html", "first child", "grandchild", "created by agent:", "depth 2"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the page does not contain %q:\n%s", want, body[:min(len(body), 800)])
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
}

// TestTheTreeSaysWhenItIsParked: the parked question is the reason someone opened the page.
func TestTheTreeSaysWhenItIsParked(t *testing.T) {
	db, clock, node, parentID, runID := filingFixture(t)
	for i := 0; i < maxFilingsPerRun; i++ {
		req := FileItemRequest{Title: "c", Brief: "b", Needs: []string{"agentic"}, ClientRef: "ref-" + string(rune('a'+i))}
		if _, err := FileItem(context.Background(), db, clock, runID, node, req); err != nil {
			t.Fatalf("filing %d: %v", i, err)
		}
	}
	if _, err := FileItem(context.Background(), db, clock, runID, node, FileItemRequest{Title: "sixth", Brief: "b"}); err != nil {
		t.Fatalf("sixth: %v", err)
	}
	tree, err := ItemTree(context.Background(), db, parentID)
	if err != nil {
		t.Fatalf("ItemTree: %v", err)
	}
	if tree.Parked == nil || tree.Parked.Action != filingApprovalAction {
		t.Fatalf("the tree does not carry the parked question: %+v", tree.Parked)
	}
}

// min is used only for truncating a failure message.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
