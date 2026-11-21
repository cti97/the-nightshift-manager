package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

// TestA4xxIsAVerdictAndA5xxIsAGlitch: the client's retry rule reads the status, not the prose. A
// 4xx (except 408/429) will never succeed on a repeat, and a stale lease is the case where
// retrying is actively harmful — the item may already be running somewhere else.
func TestA4xxIsAVerdictAndA5xxIsAGlitch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The client builds /runs/{id}/report; the id names the case.
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/runs/"), "/report")
		switch id {
		case "stale":
			board.WriteError(w, http.StatusConflict, "stale_lease", "the run's lease is gone")
		case "forbidden":
			board.WriteError(w, http.StatusForbidden, "not_your_run", "no")
		case "too-many":
			board.WriteError(w, http.StatusTooManyRequests, "busy", "later")
		case "timeout":
			board.WriteError(w, http.StatusRequestTimeout, "slow", "later")
		case "broken":
			board.WriteError(w, http.StatusInternalServerError, "report", "blip")
		default:
			board.WriteJSON(w, http.StatusOK, board.ReportResult{Applied: true})
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "token")

	cases := []struct {
		path     string
		wantCode string
		terminal bool
	}{
		{"stale", "stale_lease", true},
		{"forbidden", "not_your_run", true},
		{"too-many", "busy", false},
		{"timeout", "slow", false},
		{"broken", "report", false},
	}
	for _, tc := range cases {
		_, err := c.Report(context.Background(), tc.path, board.ReportRequest{Status: "ok"})
		var se *StatusError
		if !errors.As(err, &se) {
			t.Fatalf("%s: %v is not a *StatusError", tc.path, err)
		}
		if se.Code != tc.wantCode {
			t.Fatalf("%s: code = %q, want %q", tc.path, se.Code, tc.wantCode)
		}
		if se.Terminal() != tc.terminal {
			t.Fatalf("%s: terminal = %v, want %v", tc.path, se.Terminal(), tc.terminal)
		}
	}
}
