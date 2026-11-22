package board

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/release"
)

// The release channel. The manager serves the directory build-release.sh produced — SHA256SUMS, its
// signature, and the artifacts the checksum file names — and nothing else. It is behind the ADMIN
// token, not the node token: an artifact list is a map of the fleet's platforms, and a stolen node
// credential should not read it.

// SetDistDir points the server at a release directory. Empty means "this manager serves no
// releases", and the routes answer 404 rather than an empty list: a manager that was never given a
// release must not look like a manager whose release is missing.
func (s *Server) SetDistDir(dir string) { s.distDir = dir }

func (s *Server) handleDistSums(w http.ResponseWriter, r *http.Request) {
	s.serveDistFile(w, r, "SHA256SUMS")
}

func (s *Server) handleDistSig(w http.ResponseWriter, r *http.Request) {
	s.serveDistFile(w, r, "SHA256SUMS.sig")
}

// serveDistFile writes one file from the release directory by name. The name is always a literal
// from this package, never from the request.
func (s *Server) serveDistFile(w http.ResponseWriter, r *http.Request, name string) {
	if s.distDir == "" {
		WriteError(w, http.StatusNotFound, "dist", "this manager serves no releases")
		return
	}
	f, err := os.Open(filepath.Join(s.distDir, name))
	if err != nil {
		WriteError(w, http.StatusNotFound, "dist", "the release does not contain "+name)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, fileModTime(f), f)
}

// handleDistArtifact serves an artifact the signed checksum file names. The allow-list is computed
// from SHA256SUMS, so a request cannot reach a file the release does not vouch for — including
// anything reached by a path the URL decoder let through.
func (s *Server) handleDistArtifact(w http.ResponseWriter, r *http.Request) {
	if s.distDir == "" {
		WriteError(w, http.StatusNotFound, "dist", "this manager serves no releases")
		return
	}
	name := r.PathValue("name")
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		WriteError(w, http.StatusBadRequest, "dist", "an artifact is a file name, not a path")
		return
	}
	sums, err := os.ReadFile(filepath.Join(s.distDir, "SHA256SUMS"))
	if err != nil {
		WriteError(w, http.StatusNotFound, "dist", "this release has no checksum file")
		return
	}
	entries, err := release.ParseSums(sums)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "dist", err.Error())
		return
	}
	allowed := false
	for _, e := range entries {
		if e.File == name {
			allowed = true
			break
		}
	}
	if !allowed {
		WriteError(w, http.StatusNotFound, "dist", "the release does not name that artifact")
		return
	}
	s.serveDistFile(w, r, name)
}

// fileModTime is ServeContent's clock. A missing stat is the zero time, which only costs a
// Last-Modified header on a file that was just opened.
func fileModTime(f *os.File) time.Time {
	st, err := f.Stat()
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}
