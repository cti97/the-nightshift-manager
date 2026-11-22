package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

// TestItemTreePrintsWhatTheBoardFiles: the CLI is the terminal view of the same tree the page renders.
func TestItemTreePrintsWhatTheBoardFiles(t *testing.T) {
	tree := board.ItemNode{
		ID: "root", Title: "the big job", Status: "needs_approval", CreatedBy: "admin",
		Parked: &board.ApprovalRequest{Action: "file_item", Reason: "this item has already filed 5 children"},
		Children: []board.ItemNode{{
			ID: "c1", Title: "first child", Status: "queued", CreatedBy: "agent:pi",
			FiledBy: "run-abcdefghijklmnop", Depth: 1,
			Children: []board.ItemNode{{
				ID: "g1", Title: "grandchild", Status: "done", CreatedBy: "agent:pi", Depth: 2,
			}},
		}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/items/root" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(tree)
	}))
	defer srv.Close()

	t.Setenv("NSM_ADMIN_TOKEN", "tok")
	var out, errOut bytes.Buffer
	if code := itemCommand([]string{"tree", "root", "--board", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d (%s)", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{
		"the big job [needs_approval]",
		"  first child [queued] — agent:pi via run run-abcdefgh",
		"    grandchild [done]",
		"waiting on a human: file_item",
		"2 item(s) under this one",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the output is missing %q:\n%s", want, got)
		}
	}
}

// TestItemTreeNeedsAnAdminToken: the tree shows every brief and every child, so it is not a node view.
func TestItemTreeNeedsAnAdminToken(t *testing.T) {
	os.Unsetenv("NSM_ADMIN_TOKEN")
	var out, errOut bytes.Buffer
	if code := itemCommand([]string{"tree", "root"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "NSM_ADMIN_TOKEN") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}
