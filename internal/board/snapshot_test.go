package board

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestASnapshotIsAReadableBoard: the point of VACUUM INTO is that the copy is a database, not a file
// that happened to be there. The test opens it and reads an item out of it.
func TestASnapshotIsAReadableBoard(t *testing.T) {
	srv, clock := newServer(t)
	if _, err := CreateItem(context.Background(), srv.db, clock, CreateItemRequest{
		Title: "the item the replica must show", Brief: "brief",
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	rec := distGet(t, srv, "/admin/snapshot", testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot = %d, want 200", rec.Code)
	}
	path := filepath.Join(t.TempDir(), "replica.db")
	if err := os.WriteFile(path, rec.Body.Bytes(), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	copyDB, err := Open(path)
	if err != nil {
		t.Fatalf("the snapshot is not a readable board: %v", err)
	}
	defer copyDB.Close()
	var title string
	if err := copyDB.QueryRowContext(context.Background(),
		`SELECT title FROM work_items LIMIT 1`).Scan(&title); err != nil {
		t.Fatalf("read item from snapshot: %v", err)
	}
	if title != "the item the replica must show" {
		t.Fatalf("title = %q", title)
	}
	if rec.Header().Get("X-NSM-Snapshot-At") == "" {
		t.Fatal("the snapshot does not say when it was taken")
	}
}

// TestSnapshotNeedsTheAdminToken: the file holds every token hash and every message.
func TestSnapshotNeedsTheAdminToken(t *testing.T) {
	srv, _ := newServer(t)
	if rec := distGet(t, srv, "/admin/snapshot", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", rec.Code)
	}
	if rec := distGet(t, srv, "/admin/snapshot", testEnrollToken); rec.Code != http.StatusUnauthorized {
		t.Fatalf("enroll token = %d, want 401", rec.Code)
	}
}

// TestSnapshotRefusesToOverwrite: a snapshot that replaced an existing file could replace a
// replica's only readable copy with a half-written one.
func TestSnapshotRefusesToOverwrite(t *testing.T) {
	srv, _ := newServer(t)
	path := filepath.Join(t.TempDir(), "exists.db")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := SnapshotTo(context.Background(), srv.db, path); err == nil {
		t.Fatal("SnapshotTo overwrote an existing file")
	}
}

// TestAReplicaRefusesEveryWrite: the guard is in front of the whole mux, so a route added later
// cannot become writable on the replica by omission.
func TestAReplicaRefusesEveryWrite(t *testing.T) {
	srv, _ := newServer(t)
	ro := srv.HandlerReadOnly()
	req := httptest.NewRequest(http.MethodPost, "/admin/items", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	rec := httptest.NewRecorder()
	ro.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST on a replica = %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
	}
	var n int
	if err := srv.db.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("a refused write created %d items", n)
	}

	// A read still works, which is the half that must not break.
	get := httptest.NewRequest(http.MethodGet, "/admin/board", nil)
	get.Header.Set("Authorization", "Bearer "+testAdminToken)
	rec = httptest.NewRecorder()
	ro.ServeHTTP(rec, get)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET board on a replica = %d, want 200", rec.Code)
	}
}

// TestOnlyOneSnapshotAtATime: two concurrent snapshots are two full reads of the database holding
// the read lock twice, for the same answer.
func TestOnlyOneSnapshotAtATime(t *testing.T) {
	srv, _ := newServer(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	snapshotTestHook = func() {
		close(entered)
		<-release
	}
	defer func() { snapshotTestHook = nil }()

	first := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/admin/snapshot", nil)
		req.Header.Set("Authorization", "Bearer "+testAdminToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		first <- rec.Code
	}()
	<-entered

	req := httptest.NewRequest(http.MethodGet, "/admin/snapshot", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second snapshot = %d, want 409", rec.Code)
	}
	close(release)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first snapshot = %d, want 200", code)
	}
}

// TestAReplicaOpensItsSnapshotReadOnly: reading a snapshot must not be a write — Open migrates, which
// would rewrite schema and drop a -wal beside an artifact that is meant to be frozen.
func TestAReplicaOpensItsSnapshotReadOnly(t *testing.T) {
	srv, clock := newServer(t)
	if _, err := CreateItem(context.Background(), srv.db, clock, CreateItemRequest{
		Title: "frozen", Brief: "b",
	}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	path := filepath.Join(t.TempDir(), "replica.db")
	if err := SnapshotTo(context.Background(), srv.db, path); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	var title string
	if err := ro.QueryRow(`SELECT title FROM work_items LIMIT 1`).Scan(&title); err != nil {
		t.Fatalf("read: %v", err)
	}
	if title != "frozen" {
		t.Fatalf("title = %q", title)
	}
	// CREATE TABLE rather than an INSERT: an INSERT could fail on the schema's own constraints and
	// the test would pass without ever proving the handle is read-only.
	if _, err := ro.Exec(`CREATE TABLE probe (x INTEGER)`); err == nil {
		t.Fatal("the replica's handle accepted a write")
	}
	// No journal beside a frozen artifact.
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			t.Fatalf("opening for read created %s", path+suffix)
		}
	}
}
