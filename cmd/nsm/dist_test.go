package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cti97/the-nightshift-manager/internal/release"
)

// releaseFixture is a signed release for THIS machine, which is what an upgrade must accept.
func releaseFixture(t *testing.T) (dir, pubB64, privB64, artifact string, body []byte) {
	t.Helper()
	dir = t.TempDir()
	privB64, pubB64, err := release.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	artifact = "nsm-1.0.0-" + runtime.GOOS + "-" + runtime.GOARCH
	body = []byte("new binary\n")
	if err := os.WriteFile(filepath.Join(dir, artifact), body, 0o755); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	sum := sha256.Sum256(body)
	sums := hex.EncodeToString(sum[:]) + "  " + artifact + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums), 0o644); err != nil {
		t.Fatalf("write sums: %v", err)
	}
	priv, _ := release.ParsePrivateKey(privB64)
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS.sig"), []byte(release.Sign(priv, []byte(sums))), 0o644); err != nil {
		t.Fatalf("write sig: %v", err)
	}
	return dir, pubB64, privB64, artifact, body
}

// TestDistSignAndVerifyThroughTheCLI: the two halves of the tool agree, over the bytes on disk.
func TestDistSignAndVerifyThroughTheCLI(t *testing.T) {
	dir, pubB64, privB64, _, _ := releaseFixture(t)
	keyPath := filepath.Join(t.TempDir(), "nsm.key")
	if err := os.WriteFile(keyPath, []byte(privB64+"\n"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	var out, errOut bytes.Buffer
	if code := distCommand([]string{"sign", "--key", keyPath, "--dir", dir}, &out, &errOut); code != 0 {
		t.Fatalf("sign exit = %d (%s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), pubB64) {
		t.Fatal("sign did not print the public key to pin")
	}

	out.Reset()
	if code := distCommand([]string{"verify", "--pub", pubB64, "--dir", dir}, &out, &errOut); code != 0 {
		t.Fatalf("verify exit = %d (%s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "signature ok") {
		t.Fatalf("verify said: %s", out.String())
	}

	// A key file is equally acceptable.
	pubPath := filepath.Join(t.TempDir(), "nsm.pub")
	if err := os.WriteFile(pubPath, []byte(pubB64+"\n"), 0o600); err != nil {
		t.Fatalf("write pub: %v", err)
	}
	out.Reset()
	if code := distCommand([]string{"verify", "--pub", pubPath, "--dir", dir}, &out, &errOut); code != 0 {
		t.Fatalf("verify from a key file: %d (%s)", code, errOut.String())
	}

	// And a tampered signature is refused.
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS.sig"), []byte("AAAA"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	var tamperOut, tamperErr bytes.Buffer
	if code := distCommand([]string{"verify", "--pub", pubB64, "--dir", dir}, &tamperOut, &tamperErr); code == 0 {
		t.Fatal("a tampered signature verified")
	}
}

// TestDistSignRefusesABogusKey: a key file that is not an ed25519 private key must not produce a
// signature file that looks valid but pins nothing.
func TestDistSignRefusesABogusKey(t *testing.T) {
	dir, _, _, _, _ := releaseFixture(t)
	bogus := filepath.Join(t.TempDir(), "bogus.key")
	if err := os.WriteFile(bogus, []byte("not-a-key\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var out, errOut bytes.Buffer
	if code := distCommand([]string{"sign", "--key", bogus, "--dir", dir}, &out, &errOut); code == 0 {
		t.Fatal("signing with a bogus key succeeded")
	}
}

// TestKeygenRefusesToReplaceAKey: a new key silently replacing the old one invalidates every agent
// that pinned it, and the operator finds out at 3am.
func TestKeygenRefusesToReplaceAKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nsm.key")
	var out, errOut bytes.Buffer
	if code := distCommand([]string{"keygen", "--out", path}, &out, &errOut); code != 0 {
		t.Fatalf("keygen exit = %d (%s)", code, errOut.String())
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %04o, want 0600", st.Mode().Perm())
	}
	if !strings.Contains(out.String(), "public key") {
		t.Fatal("keygen did not print the public half")
	}
	var secondOut, secondErr bytes.Buffer
	if code := distCommand([]string{"keygen", "--out", path}, &secondOut, &secondErr); code == 0 {
		t.Fatal("keygen replaced an existing key")
	}
	if !strings.Contains(secondErr.String(), "refusing") {
		t.Fatalf("keygen said: %s", secondErr.String())
	}
}

// distServer serves a release directory the way the manager does, with the admin token.
func distServer(t *testing.T, dir, adminToken string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+adminToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/dist/files/")
		if name == r.URL.Path {
			name = filepath.Base(r.URL.Path) // /dist/SHA256SUMS and its .sig
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
}

// TestUpgradeReplacesTheBinaryAfterBothChecks: signature, then checksum, then one atomic rename.
func TestUpgradeReplacesTheBinaryAfterBothChecks(t *testing.T) {
	dir, pubB64, _, artifact, body := releaseFixture(t)
	srv := distServer(t, dir, "admin-secret")
	defer srv.Close()

	exe := filepath.Join(t.TempDir(), "nsm")
	if err := os.WriteFile(exe, []byte("old binary\n"), 0o755); err != nil {
		t.Fatalf("write old: %v", err)
	}
	var out bytes.Buffer
	if _, err := upgradeFromBoard(context.Background(), srv.URL, "admin-secret", pubB64, exe, nil, &out); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("binary = %q, want the release body", got)
	}
	if !strings.Contains(out.String(), artifact) {
		t.Fatalf("upgrade did not name the artifact: %s", out.String())
	}
	// No temp file left beside it: a leftover .nsm-upgrade-* is a partial download someone could run.
	entries, _ := os.ReadDir(filepath.Dir(exe))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".nsm-upgrade-") {
			t.Fatalf("left behind %s", e.Name())
		}
	}
}

// TestUpgradeRefusesATamperedArtifact: the checksum is checked over the bytes that arrived, and the
// binary is untouched when it fails.
func TestUpgradeRefusesATamperedArtifact(t *testing.T) {
	dir, pubB64, _, artifact, _ := releaseFixture(t)
	// Same name, different bytes: the signed checksum no longer describes what is served.
	if err := os.WriteFile(filepath.Join(dir, artifact), []byte("evil binary\n"), 0o755); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	srv := distServer(t, dir, "admin-secret")
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "nsm")
	if err := os.WriteFile(exe, []byte("old binary\n"), 0o755); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if _, err := upgradeFromBoard(context.Background(), srv.URL, "admin-secret", pubB64, exe, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("a tampered artifact replaced the binary")
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "old binary\n" {
		t.Fatalf("the binary changed after a refused upgrade: %q", got)
	}
}

// TestUpgradeRefusesAnotherKeyAndAMissingToken: two ways an installer must stop before writing.
func TestUpgradeRefusesAnotherKeyAndAMissingToken(t *testing.T) {
	dir, pubB64, _, _, _ := releaseFixture(t)
	_, otherPubB64, _ := release.GenerateKey()
	srv := distServer(t, dir, "admin-secret")
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "nsm")
	if err := os.WriteFile(exe, []byte("old\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := upgradeFromBoard(context.Background(), srv.URL, "admin-secret", otherPubB64, exe, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("a signature from another key was accepted")
	}
	if _, err := upgradeFromBoard(context.Background(), srv.URL, "", pubB64, exe, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("an upgrade without an admin token was accepted")
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "old\n" {
		t.Fatal("a refused upgrade changed the binary")
	}
}

// TestPrintAndUpgradeAreRefusedTogether: --print promises that looking changes nothing, and an
// upgrade replaces the binary.
func TestPrintAndUpgradeAreRefusedTogether(t *testing.T) {
	t.Setenv("NSM_NODE_TOKEN", "tok")
	var out, errOut bytes.Buffer
	code := installCommand([]string{"--node", "n", "--provides", "reflex", "--reach", "host",
		"--sandbox-root", t.TempDir(), "--print", "--upgrade", "--pubkey", "AAAA"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "cannot be combined") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

// TestAnOlderSignedReleaseIsRefused: a signature proves a release is genuine, not that it is
// current. A compromised manager replaying an old one must not reinstall it.
func TestAnOlderSignedReleaseIsRefused(t *testing.T) {
	entry := release.Entry{File: "nsm-1.0.0-darwin-arm64", OS: "darwin", Arch: "arm64"}
	if err := refuseDowngrade("nsm-2.0.0-darwin-arm64", entry); err == nil {
		t.Fatal("1.0.0 over 2.0.0 was allowed")
	}
	if err := refuseDowngrade("nsm-1.0.0-darwin-arm64", entry); err != nil {
		t.Fatalf("reinstalling the same version was refused: %v", err)
	}
	newer := release.Entry{File: "nsm-2.1.0-darwin-arm64", OS: "darwin", Arch: "arm64"}
	if err := refuseDowngrade("nsm-2.0.0-darwin-arm64", newer); err != nil {
		t.Fatalf("an upgrade was refused: %v", err)
	}
	// Unversioned builds have no order: developing against a local `make release` must still work,
	// and the operator sees the version in the output.
	dev := release.Entry{File: "nsm-dev-darwin-arm64"}
	if err := refuseDowngrade("nsm-2.0.0-darwin-arm64", dev); err != nil {
		t.Fatalf("an unversioned build was refused: %v", err)
	}
	if err := refuseDowngrade("", newer); err != nil {
		t.Fatalf("a first install was refused: %v", err)
	}
}

// TestTheGuardRunsBeforeAnyDownload: a refused release must not touch the binary, and the guard is
// called with the entry the signature vouched for.
func TestTheGuardRunsBeforeAnyDownload(t *testing.T) {
	dir, pubB64, _, _, _ := releaseFixture(t)
	var downloads atomic.Int32
	srv := distServerCounting(t, dir, "admin-secret", &downloads)
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "nsm")
	if err := os.WriteFile(exe, []byte("old\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	refused := errors.New("refused by the guard")
	_, err := upgradeFromBoard(context.Background(), srv.URL, "admin-secret", pubB64, exe,
		func(entry release.Entry) error {
			if entry.File == "" || entry.Hash == "" {
				t.Fatalf("the guard saw an empty entry: %+v", entry)
			}
			return refused
		}, &bytes.Buffer{})
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the guard's refusal", err)
	}
	if got := downloads.Load(); got != 0 {
		t.Fatalf("the artifact was downloaded %d times before the guard ran", got)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "old\n" {
		t.Fatal("a refused upgrade changed the binary")
	}
}

// TestAnOversizedArtifactIsRefused: the signature says who published the bytes, not how many.
func TestAnOversizedArtifactIsRefused(t *testing.T) {
	old := maxArtifactBytes
	maxArtifactBytes = 4 // the fixture artifact is 11 bytes
	defer func() { maxArtifactBytes = old }()
	dir, pubB64, _, _, _ := releaseFixture(t)
	srv := distServer(t, dir, "admin-secret")
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "nsm")
	if err := os.WriteFile(exe, []byte("old\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := upgradeFromBoard(context.Background(), srv.URL, "admin-secret", pubB64, exe, nil, &bytes.Buffer{})
	// The error must NAME the size: a hash mismatch would also be an error, and would let this test
	// pass while the cap did nothing.
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v, want it to refuse the size", err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "old\n" {
		t.Fatal("an oversized artifact changed the binary")
	}
}

// distServerCounting counts artifact downloads, which is how "before any download" is measured.
func distServerCounting(t *testing.T, dir, adminToken string, downloads *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+adminToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/dist/files/")
		if name != r.URL.Path {
			downloads.Add(1)
		} else {
			name = filepath.Base(r.URL.Path)
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
}
