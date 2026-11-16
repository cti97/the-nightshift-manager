package board

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestApprovalRequestRoundTrips stores each request in work_items.approval_request
// and reads it back: the bytes the board holds must be the bytes it would write
// again, and an empty option set must come back as [] -- a page iterating it
// must not meet null.
func TestApprovalRequestRoundTrips(t *testing.T) {
	db, _ := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	if _, err := db.Exec(`INSERT INTO work_items
		 (id,title,brief,status,created_by,created_at,updated_at)
		 VALUES ('w1','delete the cache','do it','needs_approval','admin',1,1)`); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	cases := []struct {
		name string
		req  ApprovalRequest
	}{
		{
			name: "empty options",
			req: ApprovalRequest{
				Action:      "delete the cache",
				Reason:      "the cache is 40GB and the run needs the disk",
				Options:     nil,
				RunID:       "r1",
				RequestedBy: "node-a",
				RequestedAt: 1_700_000_000,
			},
		},
		{
			name: "explicitly empty options",
			req: ApprovalRequest{
				Action:      "delete the cache",
				Reason:      "the cache is 40GB and the run needs the disk",
				Options:     []string{},
				RunID:       "r1",
				RequestedBy: "node-a",
				RequestedAt: 1_700_000_000,
			},
		},
		{
			name: "two options",
			req: ApprovalRequest{
				Action:      "delete the cache",
				Reason:      "the cache is 40GB and the run needs the disk",
				Options:     []string{"delete it", "keep it"},
				RunID:       "r2",
				RequestedBy: "node-b",
				RequestedAt: 1_700_000_099,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.req)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if _, err := db.Exec(`UPDATE work_items SET approval_request = ? WHERE id = 'w1'`,
				string(encoded)); err != nil {
				t.Fatalf("store: %v", err)
			}

			var stored string
			if err := db.QueryRow(`SELECT approval_request FROM work_items WHERE id = 'w1'`).
				Scan(&stored); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if stored != string(encoded) {
				t.Fatalf("stored %s, want the encoded bytes %s", stored, encoded)
			}

			var got ApprovalRequest
			if err := json.Unmarshal([]byte(stored), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			again, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if string(again) != stored {
				t.Fatalf("re-encoded %s, want the stored bytes %s", again, stored)
			}

			if got.Action != tc.req.Action || got.Reason != tc.req.Reason ||
				got.RunID != tc.req.RunID || got.RequestedBy != tc.req.RequestedBy ||
				got.RequestedAt != tc.req.RequestedAt {
				t.Fatalf("round-tripped %+v, want %+v", got, tc.req)
			}
			if !slices.Equal(got.Options, tc.req.Options) {
				t.Fatalf("options = %v, want %v", got.Options, tc.req.Options)
			}
			if len(tc.req.Options) == 0 {
				if got.Options == nil {
					t.Fatal("empty options read back as nil; a page ranging them gets null")
				}
				if !strings.Contains(stored, `"options":[]`) || strings.Contains(stored, `"options":null`) {
					t.Fatalf("empty options stored as %s, want \"options\":[]", stored)
				}
			}
		})
	}
}
