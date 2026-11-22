package board

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cti97/the-nightshift-manager/internal/release"
)

// distFixture writes a miniature release: two artifacts and a checksum file that names them.
func distFixture(t *testing.T) (dir string, artifact string, body []byte) {
	t.Helper()
	dir = t.TempDir()
	body = []byte("#!/bin/sh\necho nsm\n")
	artifact = "nsm-1.0.0-darwin-arm64"
	if err := os.WriteFile(filepath.Join(dir, artifact), body, 0o755); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	sum := sha256.Sum256(body)
	sums := hex.EncodeToString(sum[:]) + "  " + artifact + "\n" +
		strings.Repeat("cd", 32) + "  nsm-1.0.0-linux-amd64\n"
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums), 0o644); err != nil {
		t.Fatalf("write sums: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS.sig"), []byte("c2ln"), 0o644); err != nil {
		t.Fatalf("write sig: %v", err)
	}
	// A file that is NOT in the release: the allow-list is the checksum file, not the directory.
	if err := os.WriteFile(filepath.Join(dir, "secrets.txt"), []byte("not for you"), 0o600); err != nil {
		t.Fatalf("write stray: %v", err)
	}
	return dir, artifact, body
}

func distGet(t *testing.T, srv *Server, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestDistNeedsTheAdminToken: the artifact list maps the fleet's platforms, so a node credential —
// or no credential — must not read it.
func TestDistNeedsTheAdminToken(t *testing.T) {
	dir, _, _ := distFixture(t)
	srv, _ := newServer(t)
	srv.SetDistDir(dir)
	for _, path := range []string{"/dist/SHA256SUMS", "/dist/SHA256SUMS.sig", "/dist/files/nsm-1.0.0-darwin-arm64"} {
		if rec := distGet(t, srv, path, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without a token = %d, want 401", path, rec.Code)
		}
		if rec := distGet(t, srv, path, testEnrollToken); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s with the enroll token = %d, want 401", path, rec.Code)
		}
	}
}

// TestDistServesWhatTheChecksumFileNames: the artifact byte-for-byte, and a 404 for a file in the
// directory the release does not name.
func TestDistServesWhatTheChecksumFileNames(t *testing.T) {
	dir, artifact, body := distFixture(t)
	srv, _ := newServer(t)
	srv.SetDistDir(dir)

	rec := distGet(t, srv, "/dist/files/"+artifact, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("artifact = %d, want 200", rec.Code)
	}
	got, _ := io.ReadAll(rec.Body)
	if string(got) != string(body) {
		t.Fatalf("artifact body = %q, want %q", got, body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content type = %q", ct)
	}

	if rec := distGet(t, srv, "/dist/files/secrets.txt", testAdminToken); rec.Code != http.StatusNotFound {
		t.Fatalf("an unnamed file = %d, want 404", rec.Code)
	}
	if rec := distGet(t, srv, "/dist/files/..%2fSHA256SUMS", testAdminToken); rec.Code == http.StatusOK {
		t.Fatal("a traversal reached a file")
	}
}

// TestDistSaysSoWhenNoReleaseIsConfigured: an empty directory is "no release", not "an empty
// release" — the difference between a manager that was never given one and one whose release broke.
func TestDistSaysSoWhenNoReleaseIsConfigured(t *testing.T) {
	srv, _ := newServer(t)
	if rec := distGet(t, srv, "/dist/SHA256SUMS", testAdminToken); rec.Code != http.StatusNotFound {
		t.Fatalf("no release = %d, want 404", rec.Code)
	}
	if rec := distGet(t, srv, "/dist/files/nsm-1.0.0-darwin-arm64", testAdminToken); rec.Code != http.StatusNotFound {
		t.Fatalf("artifact with no release = %d, want 404", rec.Code)
	}
}

// TestTheServedReleaseVerifies: what a manager serves is what an agent checks — signature over the
// exact bytes, then checksum of the exact bytes. A test that used a fixture signature could pass
// while the two halves disagreed about newlines.
func TestTheServedReleaseVerifies(t *testing.T) {
	dir, _, _ := distFixture(t)
	privB64, pubB64, err := release.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	priv, _ := release.ParsePrivateKey(privB64)
	pub, _ := release.ParsePublicKey(pubB64)
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("read sums: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS.sig"), []byte(release.Sign(priv, sums)), 0o644); err != nil {
		t.Fatalf("write sig: %v", err)
	}

	srv, _ := newServer(t)
	srv.SetDistDir(dir)
	sumsRec := distGet(t, srv, "/dist/SHA256SUMS", testAdminToken)
	sigRec := distGet(t, srv, "/dist/SHA256SUMS.sig", testAdminToken)
	served := sumsRec.Body.Bytes()
	if err := release.Verify(pub, served, sigRec.Body.String()); err != nil {
		t.Fatalf("the served release does not verify: %v", err)
	}
	entry, err := release.ParseSums(served)
	if err != nil || len(entry) != 2 {
		t.Fatalf("served sums: %v, %v", entry, err)
	}
	artRec := distGet(t, srv, "/dist/files/"+entry[0].File, testAdminToken)
	sum := sha256.Sum256(artRec.Body.Bytes())
	if hex.EncodeToString(sum[:]) != entry[0].Hash {
		t.Fatal("the served artifact does not match the signed checksum")
	}
}
