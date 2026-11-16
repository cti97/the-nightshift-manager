// internal/board/why_test.go
package board

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// TestWhyReportsWhoPausedTheBoard pins A1: the explainer answers "who stopped
// this", not only "was it stopped". Task 13 split pause ownership into two keys
// with independent lifecycles, so "both" is a real third state: clearing the
// operator's key does not resume a board whose ceiling also reaches, and
// raising the ceiling does not clear a pause a person typed.
//
// The four cases are exhaustive (each owner independently on/off), and the
// spend view is asserted to use the same vocabulary: two endpoints disagreeing
// about who holds the board would be the same defect in a different place.
func TestWhyReportsWhoPausedTheBoard(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ceiling  bool
		operator bool
		want     string
	}{
		// The values are literals on purpose: the pauseBy* constants produce
		// them, so comparing against the constants would let a rename pass
		// every test while silently changing the API a consumer reads.
		{name: "ceiling alone", ceiling: true, want: "ceiling"},
		{name: "operator alone", operator: true, want: "operator"},
		{name: "both", ceiling: true, operator: true, want: "ceiling+operator"},
		{name: "neither", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, clock := newServer(t)
			ctx := context.Background()
			node, itemID := seedClaimableBoard(t, srv.db, clock)

			if tc.ceiling {
				// A ceiling that reaches today's spend is a pause the next
				// claim would act on; reconciling records the projection in
				// spend_ceiling_paused, which is the state this test is about.
				mustSetCeiling(t, srv.db, clock, 1)
				dayStart, _ := SpendDay(clock.Now())
				seedRunForTest(t, srv.db, node, itemID, "run-spent", 1, dayStart, 1)
			}
			if tc.operator {
				if _, err := srv.db.ExecContext(ctx,
					`INSERT INTO config (key,value) VALUES (?,?)`, configBoardPaused, "hold for the deploy"); err != nil {
					t.Fatalf("write an operator pause: %v", err)
				}
			}
			// Reconcile: a claim evaluates the gate and records the ceiling's
			// projection, so a "both" case has both keys on disk, not just both
			// conditions true in memory. Only paused cases are claimed, so the
			// unpaused item under explanation is never consumed.
			if tc.want != "" {
				if got := mustClaim(t, srv.db, clock, node, 1); len(got) != 0 {
					t.Fatalf("claimed %d items while paused, want 0", len(got))
				}
			}

			ex, err := Explain(ctx, srv.db, clock, itemID)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if ex.PausedBy != tc.want {
				t.Fatalf("explanation.paused_by = %q, want %q (explanation %+v)", ex.PausedBy, tc.want, ex)
			}
			if ex.Paused != (tc.want != "") {
				t.Fatalf("explanation.paused = %v with paused_by %q", ex.Paused, ex.PausedBy)
			}

			st := mustSpend(t, srv.db, clock)
			if st.PausedBy != tc.want {
				t.Fatalf("spend.paused_by = %q, want %q: the two views disagree about who holds the board", st.PausedBy, tc.want)
			}

			// The distinction has to be machine-readable on the wire, not
			// buried in pause_reason for the operator to parse.
			rec := do(t, srv.Handler(), "GET", "/admin/why/"+itemID, testAdminToken)
			if rec.Code != http.StatusOK {
				t.Fatalf("why = %d, want 200 (body %s)", rec.Code, rec.Body)
			}
			var body Explanation
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode why body: %v", err)
			}
			if body.PausedBy != tc.want {
				t.Fatalf("why JSON paused_by = %q, want %q (body %s)", body.PausedBy, tc.want, rec.Body)
			}
			if tc.want == "" && slices.Contains(keysOf(t, rec.Body.Bytes()), "paused_by") {
				t.Fatalf("why body carries paused_by when nothing is paused: %s", rec.Body)
			}
		})
	}
}

// keysOf decodes a JSON object's top-level keys. It exists only to assert the
// omitted-when-empty half of paused_by; decoding into a map keeps the check
// independent of the struct's tags.
func keysOf(t *testing.T, raw []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode JSON object: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestWhySaysATerminalItemIsFinished pins C13: placement does not read status,
// so /why used to answer placed:true for an item that was already done, failed
// or cancelled — an answer that reads as "this could be claimed". The placement
// verdict is still kept, because an operator may want to know whether the item
// would have placed; it moves into the reason, where it cannot be mistaken for
// a claimable state.
func TestWhySaysATerminalItemIsFinished(t *testing.T) {
	for _, status := range []string{"done", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			srv, clock := newServer(t)
			db := srv.db
			ctx := context.Background()
			_, itemID := seedClaimableBoard(t, db, clock)
			if _, err := db.ExecContext(ctx, `UPDATE work_items SET status=? WHERE id=?`, status, itemID); err != nil {
				t.Fatalf("mark item %s: %v", status, err)
			}

			ex, err := Explain(ctx, db, clock, itemID)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if ex.Status != status {
				t.Fatalf("explanation status = %q, want %q", ex.Status, status)
			}
			if ex.Placed {
				t.Fatalf("why says placed:true for an item that is already %s; the answer reads as claimable", status)
			}
			if !strings.Contains(ex.Reason, "already "+status) || !strings.Contains(ex.Reason, "cannot be claimed") {
				t.Fatalf("reason = %q, want it to say the item is already %s and cannot be claimed", ex.Reason, status)
			}
			if ex.BlockedReason == "" {
				t.Fatal("why has no blocked_reason for a terminal item")
			}
			// The placement verdict survives: the one candidate is eligible, so
			// the answer still names the node and model it would have used.
			if !strings.Contains(ex.Reason, "on a via local-model") {
				t.Fatalf("reason = %q, want the placement it would have received named", ex.Reason)
			}

			// The wire is what an operator reads, so assert the field that used
			// to say "claimable" is false and the finished status is in the text.
			rec := do(t, srv.Handler(), "GET", "/admin/why/"+itemID, testAdminToken)
			if rec.Code != http.StatusOK {
				t.Fatalf("why = %d, want 200 (body %s)", rec.Code, rec.Body)
			}
			var body Explanation
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode why body: %v", err)
			}
			if body.Placed {
				t.Fatalf("why JSON says placed:true for an already-%s item: %s", status, rec.Body)
			}
			if !strings.Contains(body.Reason, "already "+status) {
				t.Fatalf("why JSON reason = %q, want the finished status named", body.Reason)
			}
		})
	}
}

// TestWhyKeepsThePlacementExplanationForAnUnplaceableTerminalItem is the other
// half: a terminal item that also could not have placed must still explain why
// placement refused, through placed:false, the reason and the per-candidate
// rejects. Otherwise fixing C13 by dropping placement output would have traded
// one unhelpful answer for another.
func TestWhyKeepsThePlacementExplanationForAnUnplaceableTerminalItem(t *testing.T) {
	db, clock := newTestDB(t)
	ctx := context.Background()
	_, itemID := seedClaimableBoard(t, db, clock)
	if _, err := db.ExecContext(ctx, `UPDATE nodes SET status='offline'`); err != nil {
		t.Fatalf("take the node offline: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE work_items SET status='done' WHERE id=?`, itemID); err != nil {
		t.Fatalf("mark item done: %v", err)
	}

	ex, err := Explain(ctx, db, clock, itemID)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if ex.Placed {
		t.Fatal("why says placed:true for a finished item with no online candidate")
	}
	if !strings.Contains(ex.Reason, "even if it were still open it could not be placed") {
		t.Fatalf("reason = %q, want the placement failure kept alongside the finished status", ex.Reason)
	}
	if len(ex.Rejects) == 0 || !strings.Contains(ex.Rejects[0].Why, "offline") {
		t.Fatalf("rejects = %+v, want the offline node named; the placement explanation was lost", ex.Rejects)
	}
}
