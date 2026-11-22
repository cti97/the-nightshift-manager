package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

// TestPullSnapshotInstallsABoard: the downloaded file replaces the replica's copy, and a bad
// download does not.
func TestPullSnapshotInstallsABoard(t *testing.T) {
	// Serve a real snapshot: build one from a board, then hand out its bytes.
	db, clock := board.OpenForTest(t, filepath.Join(t.TempDir(), "source.db"))
	if _, err := board.CreateItem(context.Background(), db, clock, board.CreateItemRequest{
		Title: "replicated", Brief: "b",
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	snapshot := filepath.Join(t.TempDir(), "snap.db")
	if err := board.SnapshotTo(context.Background(), db, snapshot); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}
	body, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "replica.db")
	if err := pullSnapshot(context.Background(), srv.URL, "admin-secret", dest); err != nil {
		t.Fatalf("pullSnapshot: %v", err)
	}
	got, err := board.Open(dest)
	if err != nil {
		t.Fatalf("open replica: %v", err)
	}
	defer got.Close()
	var title string
	if err := got.QueryRow(`SELECT title FROM work_items LIMIT 1`).Scan(&title); err != nil {
		t.Fatalf("read: %v", err)
	}
	if title != "replicated" {
		t.Fatalf("title = %q", title)
	}
}

// TestPullSnapshotRefusesJunk: a 200 that is not a board must not replace the file the replica is
// serving — that is how an outage turns into a blank dashboard.
func TestPullSnapshotRefusesJunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("this is not sqlite"))
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "replica.db")
	if err := os.WriteFile(dest, []byte("previous board"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := pullSnapshot(context.Background(), srv.URL, "t", dest); err == nil {
		t.Fatal("junk replaced the replica's snapshot")
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "previous board" {
		t.Fatal("a refused pull changed the snapshot")
	}
}

// TestPullSnapshotNeedsTheAdminToken: a 401 is reported, not written.
func TestPullSnapshotNeedsTheAdminToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "replica.db")
	if err := pullSnapshot(context.Background(), srv.URL, "", dest); err == nil {
		t.Fatal("a 401 was treated as a snapshot")
	}
}

// snapshotFixture writes a board to dir/replica.db the way a previous pull would have left it.
func snapshotFixture(t *testing.T, dir string) {
	t.Helper()
	db, clock := board.OpenForTest(t, filepath.Join(t.TempDir(), "src.db"))
	if _, err := board.CreateItem(context.Background(), db, clock, board.CreateItemRequest{
		Title: "from the last snapshot", Brief: "b",
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if err := board.SnapshotTo(context.Background(), db, filepath.Join(dir, "replica.db")); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}
}

// TestTheReplicaStartsFromItsSnapshotWhenTheManagerIsDown is the scenario the replica exists for: the
// Pi reboots during an outage and must come up serving the last copy, not exit because it cannot
// reach a manager that is down.
func TestTheReplicaStartsFromItsSnapshotWhenTheManagerIsDown(t *testing.T) {
	dir := t.TempDir()
	snapshotFixture(t, dir)
	var state replicaState
	// 127.0.0.1:1 is refused, so the pull cannot succeed.
	if err := startReplica(&state, filepath.Join(dir, "replica.db"), "http://127.0.0.1:1", "admin"); err != nil {
		t.Fatalf("startReplica: %v", err)
	}
	if state.handler.Load() == nil {
		t.Fatal("the replica has no handler after starting from its snapshot")
	}
	if state.at.Load() == 0 {
		t.Fatal("the replica did not record when its snapshot was taken")
	}
}

// TestTheReplicaRefusesToStartWithNothingToServe: no manager and no snapshot is a genuine failure,
// and a replica that came up anyway would answer every request with an error anyway.
func TestTheReplicaRefusesToStartWithNothingToServe(t *testing.T) {
	dir := t.TempDir()
	var state replicaState
	err := startReplica(&state, filepath.Join(dir, "replica.db"), "http://127.0.0.1:1", "admin")
	if err == nil {
		t.Fatal("startReplica succeeded with no manager and no snapshot")
	}
	if !strings.Contains(err.Error(), "no snapshot to fall back on") {
		t.Fatalf("error = %q, want it to say why", err)
	}
	if state.handler.Load() != nil {
		t.Fatal("a failed start installed a handler")
	}
}

// TestTheReplicaClosesTheHandleItReplaced: one handle per pull is a file descriptor leak that only
// shows up after a day of running.
func TestTheReplicaClosesTheHandleItReplaced(t *testing.T) {
	old := replicaCloseGrace
	replicaCloseGrace = 10 * time.Millisecond
	defer func() { replicaCloseGrace = old }()

	dir := t.TempDir()
	snapshotFixture(t, dir)
	var state replicaState
	path := filepath.Join(dir, "replica.db")
	for i := 0; i < 3; i++ {
		if err := loadSnapshot(&state, path, "admin"); err != nil {
			t.Fatalf("loadSnapshot %d: %v", i, err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if state.closed.Load() == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("closed %d of the 2 replaced handles", state.closed.Load())
}

// TestAnOversizedSnapshotIsRefused: a peer that streams forever must not fill the Pi's disk.
func TestAnOversizedSnapshotIsRefused(t *testing.T) {
	old := maxSnapshotBytes
	maxSnapshotBytes = 64
	defer func() { maxSnapshotBytes = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "replica.db")
	if err := pullSnapshot(context.Background(), srv.URL, "t", dest); err == nil {
		t.Fatal("an oversized snapshot was accepted")
	} else if !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("error = %q, want it to name the size", err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("an oversized snapshot was installed")
	}
}
