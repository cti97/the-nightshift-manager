package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

const replicaUsage = `usage: nsm replica serve --board URL --dir DIR [--addr 127.0.0.1:8082] [--every 60s]

Pulls a snapshot of the board every interval and serves it read-only. When the manager is down the
last snapshot keeps answering, with a header that says how old it is. It cannot accept a write: the
guard sits in front of every route, because a replica that accepts writes is a second board that
outlives the outage and disagrees with the first one.`

// maxSnapshotBytes bounds a download. The board is a SQLite file; a snapshot larger than this is a
// misconfiguration or a hostile peer, and either way a Pi's disk should not find out slowly.
// A var, not a const, so a test can make the cap small enough to exercise: a 512 MiB fixture is not
// a test, it is a download.
var maxSnapshotBytes int64 = 512 << 20

// replicaCloseGrace is how long the previous snapshot's handle stays open after a swap. In-flight
// requests may still be reading it; closing immediately would turn a periodic pull into a periodic
// 500. Two handles open at most, each closed once — the leak this replaces was one handle per pull.
var replicaCloseGrace = 2 * time.Minute

// replicaState is what the HTTP handler reads: the server for the current snapshot and when it was
// taken. Atomic pointers, because the pull loop replaces them while requests are in flight.
type replicaState struct {
	handler atomic.Pointer[http.Handler]
	db      atomic.Pointer[sql.DB]
	at      atomic.Int64 // unix seconds of the snapshot, 0 for none yet
	closed  atomic.Int32 // handles released so far; diagnostic, and what the leak test counts
}

func replicaCommand(args []string, out, errOut io.Writer) int {
	if len(args) == 0 || args[0] != "serve" {
		fmt.Fprintln(errOut, replicaUsage)
		return 2
	}
	fs := flag.NewFlagSet("replica serve", flag.ContinueOnError)
	fs.SetOutput(errOut)
	baseURL := fs.String("board", defaultBoard, "manager base URL")
	dir := fs.String("dir", "", "directory for the replica's snapshot (required)")
	addr := fs.String("addr", "127.0.0.1:8082", "listen address")
	every := fs.Duration("every", time.Minute, "how often to pull a snapshot")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(errOut, "replica serve: --dir is required")
		return 2
	}
	if *every < 10*time.Second {
		fmt.Fprintln(errOut, "replica serve: --every must be at least 10s; a snapshot is not cheap")
		return 2
	}
	admin, ok := os.LookupEnv("NSM_ADMIN_TOKEN")
	if !ok || admin == "" {
		fmt.Fprintln(errOut, "NSM_ADMIN_TOKEN is required: a snapshot holds every token hash")
		return 2
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		fmt.Fprintf(errOut, "replica serve: %v\n", err)
		return 1
	}

	var state replicaState
	path := filepath.Join(*dir, "replica.db")

	// The first pull is attempted before answering, so a fresh replica does not report "no items"
	// during exactly the outage it exists for. But a replica that CANNOT reach the manager at boot
	// and HAS a snapshot must still come up: the Pi rebooting during an outage is the scenario, not
	// an edge case.
	if err := startReplica(&state, path, *baseURL, admin); err != nil {
		fmt.Fprintf(errOut, "replica serve: %v\n", err)
		return 1
	}
	if state.at.Load() > 0 && state.handler.Load() != nil {
		// startReplica reports which path it took through the returned error, so this line is only
		// about saying so to an operator.
		fmt.Fprintf(errOut, "replica serve: serving %s\n", path)
	}

	pull := func(ctx context.Context) error {
		if err := pullSnapshot(ctx, *baseURL, admin, path); err != nil {
			return err
		}
		return loadSnapshot(&state, path, admin)
	}

	go func() {
		tick := time.NewTicker(*every)
		defer tick.Stop()
		for range tick.C {
			ctx, cancel := context.WithTimeout(context.Background(), 2*(*every))
			err := pull(ctx)
			cancel()
			if err != nil {
				// The last good snapshot keeps answering; the age grows and the header says so.
				fmt.Fprintf(errOut, "replica: pull failed, still serving the last snapshot: %v\n", err)
			}
		}
	}()

	fmt.Fprintf(errOut, "replica listening on %s (snapshot dir %s)\n", *addr, *dir)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := state.handler.Load()
		if p == nil {
			http.Error(w, "the replica has no snapshot yet", http.StatusServiceUnavailable)
			return
		}
		// Every response says what it is: the age is the one number a reader must not have to
		// infer, and a page that looks live during an outage is worse than one that looks broken.
		age := time.Since(time.Unix(state.at.Load(), 0))
		w.Header().Set("X-NSM-Replica", "read-only")
		w.Header().Set("X-NSM-Snapshot-Age", age.Truncate(time.Second).String())
		(*p).ServeHTTP(w, r)
	})
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintf(errOut, "replica serve: %v\n", err)
		return 1
	}
	return 0
}

// startReplica gets the replica to its first answer: pull a fresh snapshot, or fall back to the one
// on disk when the manager cannot be reached. It is separate from replicaCommand because that
// function blocks on ListenAndServe, and the boot rule is the part worth testing.
func startReplica(state *replicaState, path, baseURL, admin string) error {
	if err := pullSnapshot(context.Background(), baseURL, admin, path); err == nil {
		return loadSnapshot(state, path, admin)
	} else if _, statErr := os.Stat(path); statErr != nil {
		return fmt.Errorf("first pull: %w (and no snapshot to fall back on)", err)
	} else if loadErr := loadSnapshot(state, path, admin); loadErr != nil {
		return fmt.Errorf("the last snapshot is unreadable: %w", loadErr)
	}
	return nil
}

// loadSnapshot opens a snapshot read-only and installs it as the one being served. The previous
// handle is closed after a grace period rather than immediately, because requests may still be
// reading it; one pull per minute with a two-minute grace leaves at most three open.
func loadSnapshot(state *replicaState, path, admin string) error {
	db, err := board.OpenReadOnly(path)
	if err != nil {
		return err
	}
	// The replica authenticates readers with the SAME admin token (hashed here, never stored): its
	// pages show every item and message, so "read-only" must not mean "public". There is no enroll
	// hash: nothing enrolls against a replica.
	srv := board.NewServer(db, board.SystemClock(), nil, board.HashToken(admin), "")
	var h http.Handler = srv.HandlerReadOnly()
	state.handler.Store(&h)
	if old := state.db.Swap(db); old != nil {
		time.AfterFunc(replicaCloseGrace, func() {
			_ = (*old).Close()
			state.closed.Add(1)
		})
	}
	state.at.Store(time.Now().Unix())
	return nil
}

// pullSnapshot downloads a snapshot and installs it atomically. The download lands beside the
// destination so the rename is one filesystem: a copy would be a window in which the replica has a
// truncated board.
func pullSnapshot(ctx context.Context, baseURL, adminToken, dest string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/admin/snapshot", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("/admin/snapshot: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".replica-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	// One byte past the cap, so "too large" is a fact rather than a truncated file that happens to
	// open. A bare LimitReader would hand a silently short database to the swap below.
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxSnapshotBytes+1))
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if n > maxSnapshotBytes {
		_ = tmp.Close()
		return fmt.Errorf("the snapshot is larger than %d MiB", maxSnapshotBytes>>20)
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		_ = tmp.Close()
		return fmt.Errorf("the snapshot arrived truncated: %d of %d bytes", n, resp.ContentLength)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// A snapshot that is not a board must never replace one that is.
	if err := verifyIsBoard(tmpName); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

// verifyIsBoard opens the file the way the replica will, so a truncated or wrong-content download
// fails here rather than at the first page request after the swap. Read-only, so verification does
// not itself create a journal beside the file it is checking.
func verifyIsBoard(path string) error {
	db, err := board.OpenReadOnly(path)
	if err != nil {
		return fmt.Errorf("the download is not a board: %w", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&n); err != nil {
		return fmt.Errorf("the download is not a board: %w", err)
	}
	return nil
}
