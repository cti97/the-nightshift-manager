package board

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// SnapshotTo writes a consistent copy of the board to path. VACUUM INTO is the whole mechanism: it
// runs inside SQLite, so the copy is a transactionally consistent database rather than a file that
// was being written to while it was read.
func SnapshotTo(ctx context.Context, db *sql.DB, path string) error {
	// VACUUM INTO refuses to overwrite, which is the behaviour we want: a snapshot that silently
	// replaced a file could replace the replica's only readable copy with a half-written one.
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("snapshot: %s already exists", path)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	return nil
}

// snapshotTestHook lets a test hold a snapshot open long enough to race a second one. Nil in
// production; the single-flight guard is the only thing it exists to exercise.
var snapshotTestHook func()

// handleSnapshot serves a snapshot of the whole board, admin only: it contains every item, every
// message and every token hash.
//
// The copy is built in a scratch directory and served from there, because the alternative — a
// predictable path under the manager's working directory — is a file another process could read,
// write or race.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	// One at a time. VACUUM INTO reads the whole database while holding a read lock, and two at once
	// is two full copies and twice the lock time; a replica that polls from two processes (or an
	// operator with curl in a loop) would turn a convenience into an outage.
	if !s.snapshotting.CompareAndSwap(false, true) {
		WriteError(w, http.StatusConflict, "snapshot", "a snapshot is already being taken")
		return
	}
	defer s.snapshotting.Store(false)
	if snapshotTestHook != nil {
		snapshotTestHook()
	}
	dir, err := os.MkdirTemp("", "nsm-snapshot-")
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "snapshot", err.Error())
		return
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "board.db")
	if err := SnapshotTo(r.Context(), s.db, path); err != nil {
		WriteError(w, http.StatusInternalServerError, "snapshot", err.Error())
		return
	}
	f, err := os.Open(path)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "snapshot", err.Error())
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "snapshot", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="board.db"`)
	w.Header().Set("X-NSM-Snapshot-At", fmt.Sprint(s.clock.Now()))
	http.ServeContent(w, r, "board.db", st.ModTime(), f)
}

// HandlerReadOnly wraps the full mux and refuses anything that is not a read.
//
// A replica opens a snapshot, and every write route would write to *that copy* — which is not a
// harmless local edit: it is a board that disagrees with the real one and outlives the outage. The
// guard sits in front of the whole mux rather than around a hand-picked list of safe routes, so a
// route added later cannot accidentally become writable on the replica.
func (s *Server) HandlerReadOnly() http.Handler {
	full := s.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.ToUpper(r.Method) {
		case http.MethodGet, http.MethodHead:
			full.ServeHTTP(w, r)
		default:
			w.Header().Set("Allow", "GET, HEAD")
			WriteError(w, http.StatusMethodNotAllowed, "replica", "this is a read-only replica")
		}
	})
}
