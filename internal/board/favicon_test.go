package board

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestTheServedFaviconMatchesTheAsset keeps the copy in Go honest. The const exists because go:embed
// cannot reach outside the package directory; this test is what stops the two from drifting, which
// is the usual price of a copy.
func TestTheServedFaviconMatchesTheAsset(t *testing.T) {
	asset, err := os.ReadFile("../../assets/favicon.svg")
	if err != nil {
		t.Fatalf("read the asset: %v", err)
	}
	normalise := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if normalise(string(asset)) != normalise(faviconSVG) {
		t.Fatal("assets/favicon.svg and the served favicon have drifted apart")
	}
}

// TestTheFaviconIsPublicAndIsAnIcon: a browser fetches it before any credential is involved, and a
// page that links to it must get an image, not a login prompt.
func TestTheFaviconIsPublicAndIsAnIcon(t *testing.T) {
	srv, _ := newServer(t)
	req := httptest.NewRequest(http.MethodGet, "/favicon.svg", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("favicon = %d, want 200 without a token", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Fatalf("content type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "<svg") {
		t.Fatal("the favicon is not an SVG")
	}
}

// mustRender executes one page template into a string, so a test can assert on the head it produces.
func mustRender(t *testing.T, tmpl *template.Template, data any) string {
	t.Helper()
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

// TestEveryPageLinksItsIcon: a favicon nobody links is a file nobody sees. Each page's head carries
// the tag, and the tag points at the route that serves it.
func TestEveryPageLinksItsIcon(t *testing.T) {
	pages := map[string]string{
		"board":     mustRender(t, boardPage, map[string]any{"View": BoardView{Counts: map[string]int{}}, "Groups": nil, "Total": 0}),
		"item":      mustRender(t, itemPage, ItemNode{Title: "t", Status: "queued", Needs: []string{}}),
		"approvals": mustRender(t, approvalsPage, []any{}),
	}
	for name, body := range pages {
		if !strings.Contains(body, `<link rel="icon" type="image/svg+xml" href="/favicon.svg">`) {
			t.Fatalf("the %s page does not link the icon", name)
		}
	}
}
