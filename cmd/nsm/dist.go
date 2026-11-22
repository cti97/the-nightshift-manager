package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/release"
)

const distUsage = `usage: nsm dist <keygen|sign|verify> [args]

  keygen --out FILE            make an ed25519 signing key (0600) and print the public half
  sign   --key FILE --dir DIR  sign DIR/SHA256SUMS, writing DIR/SHA256SUMS.sig
  verify --pub KEY --dir DIR   check DIR/SHA256SUMS against a pinned public key

The private key never goes on the manager. A signature the manager could produce is a signature that
protects nobody from a compromised manager; the manager only ever serves bytes.`

func distCommand(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errOut, distUsage)
		return 2
	}
	switch args[0] {
	case "keygen":
		return distKeygen(args[1:], out, errOut)
	case "sign":
		return distSign(args[1:], out, errOut)
	case "verify":
		return distVerify(args[1:], out, errOut)
	default:
		fmt.Fprintln(errOut, distUsage)
		return 2
	}
}

func distKeygen(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("dist keygen", flag.ContinueOnError)
	fs.SetOutput(errOut)
	outPath := fs.String("out", "", "file to write the private key to (created 0600)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *outPath == "" {
		fmt.Fprintln(errOut, "dist keygen: --out is required")
		return 2
	}
	// Refusing to overwrite is the whole safety property: a new key silently replacing the old one
	// makes every agent that pinned the old one refuse every future release, and the operator finds
	// out at 3am.
	if _, err := os.Stat(*outPath); err == nil {
		fmt.Fprintf(errOut, "dist keygen: %s exists; refusing to replace a signing key\n", *outPath)
		return 2
	}
	priv, pub, err := release.GenerateKey()
	if err != nil {
		fmt.Fprintf(errOut, "dist keygen: %v\n", err)
		return 1
	}
	if err := os.WriteFile(*outPath, []byte(priv+"\n"), 0o600); err != nil {
		fmt.Fprintf(errOut, "dist keygen: write %s: %v\n", *outPath, err)
		return 1
	}
	if err := os.Chmod(*outPath, 0o600); err != nil {
		fmt.Fprintf(errOut, "dist keygen: chmod %s: %v\n", *outPath, err)
		return 1
	}
	fmt.Fprintf(out, "wrote %s (0600)\npublic key (pin this on every agent):\n%s\n", *outPath, pub)
	return 0
}

func distSign(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("dist sign", flag.ContinueOnError)
	fs.SetOutput(errOut)
	keyPath := fs.String("key", "", "file holding the base64 ed25519 private key")
	dir := fs.String("dir", "dist", "release directory containing SHA256SUMS")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *keyPath == "" {
		fmt.Fprintln(errOut, "dist sign: --key is required")
		return 2
	}
	raw, err := os.ReadFile(*keyPath)
	if err != nil {
		fmt.Fprintf(errOut, "dist sign: read key: %v\n", err)
		return 1
	}
	priv, err := release.ParsePrivateKey(string(raw))
	if err != nil {
		fmt.Fprintf(errOut, "dist sign: %v\n", err)
		return 2
	}
	sumsPath := filepath.Join(*dir, "SHA256SUMS")
	sums, err := os.ReadFile(sumsPath)
	if err != nil {
		fmt.Fprintf(errOut, "dist sign: %v (run make release first)\n", err)
		return 1
	}
	entries, err := release.ParseSums(sums)
	if err != nil {
		fmt.Fprintf(errOut, "dist sign: %v\n", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(*dir, "SHA256SUMS.sig"), []byte(release.Sign(priv, sums)), 0o644); err != nil {
		fmt.Fprintf(errOut, "dist sign: write signature: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "signed %d artifacts with key %s\n", len(entries), release.PublicFromPrivate(priv))
	fmt.Fprintf(out, "pin this on every agent: --pubkey %s\n", release.PublicFromPrivate(priv))
	return 0
}

func distVerify(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("dist verify", flag.ContinueOnError)
	fs.SetOutput(errOut)
	pubKey := fs.String("pub", "", "base64 ed25519 public key (or a file containing it)")
	dir := fs.String("dir", "dist", "release directory containing SHA256SUMS")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *pubKey == "" {
		fmt.Fprintln(errOut, "dist verify: --pub is required")
		return 2
	}
	pub, err := readPublicKey(*pubKey)
	if err != nil {
		fmt.Fprintf(errOut, "dist verify: %v\n", err)
		return 2
	}
	sums, err := os.ReadFile(filepath.Join(*dir, "SHA256SUMS"))
	if err != nil {
		fmt.Fprintf(errOut, "dist verify: %v\n", err)
		return 1
	}
	sig, err := os.ReadFile(filepath.Join(*dir, "SHA256SUMS.sig"))
	if err != nil {
		fmt.Fprintf(errOut, "dist verify: %v\n", err)
		return 1
	}
	entries, err := release.Check(pub, sums, string(sig), runtime.GOOS, runtime.GOARCH, "")
	if err != nil {
		fmt.Fprintf(errOut, "dist verify: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "signature ok; this machine's artifact is %s\n", entries.File)
	return 0
}

// readPublicKey accepts the key itself or a file holding it, because an operator who generated one
// has it in a file and one who copied it from a chat has it in their shell history.
func readPublicKey(s string) (ed25519.PublicKey, error) {
	if strings.ContainsAny(s, "/\\.") {
		if raw, err := os.ReadFile(s); err == nil {
			return release.ParsePublicKey(string(raw))
		}
	}
	return release.ParsePublicKey(s)
}

// maxArtifactBytes bounds an upgrade download. The signature only proves WHO published the bytes, not
// how many there are: a compromised or broken manager can stream until the disk is full, and the hash
// check only happens at the end.
var maxArtifactBytes int64 = 256 << 20

// upgradeFromBoard replaces the running binary with the release the board serves, after checking the
// signature over the checksum file and the checksum over the bytes. Nothing is written until both
// pass: a partial download must not be able to become an executable.
// guard is consulted after the signature verifies and before anything is downloaded: it is where the
// caller refuses an older release, and doing it here means a refused upgrade never writes a byte.
type upgradeGuard func(entry release.Entry) error

func upgradeFromBoard(ctx context.Context, baseURL, adminToken, pubKey, exePath string, guard upgradeGuard, out io.Writer) (release.Entry, error) {
	pub, err := readPublicKey(pubKey)
	if err != nil {
		return release.Entry{}, err
	}
	if adminToken == "" {
		return release.Entry{}, errors.New("NSM_ADMIN_TOKEN is required to fetch a release: the artifact list is not public")
	}
	client := &http.Client{Timeout: 60 * time.Second}
	get := func(path string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
		}
		return body, nil
	}

	sums, err := get("/dist/SHA256SUMS")
	if err != nil {
		return release.Entry{}, err
	}
	sig, err := get("/dist/SHA256SUMS.sig")
	if err != nil {
		return release.Entry{}, err
	}
	want, err := release.Check(pub, sums, string(sig), runtime.GOOS, runtime.GOARCH, "")
	if err != nil {
		return release.Entry{}, err
	}
	if guard != nil {
		if err := guard(want); err != nil {
			return release.Entry{}, err
		}
	}

	// Download beside the target, so the rename is on one filesystem and atomic. A temp file in
	// /tmp would make "replace the binary" a copy, which is not atomic and can leave a truncated
	// executable behind.
	tmp, err := os.CreateTemp(filepath.Dir(exePath), ".nsm-upgrade-*")
	if err != nil {
		return release.Entry{}, fmt.Errorf("prepare download: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/dist/files/"+want.File, nil)
	if err != nil {
		return release.Entry{}, err
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := client.Do(req)
	if err != nil {
		return release.Entry{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return release.Entry{}, fmt.Errorf("download %s: %s", want.File, resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxArtifactBytes+1))
	if err != nil {
		return release.Entry{}, fmt.Errorf("download %s: %w", want.File, err)
	}
	if n > maxArtifactBytes {
		return release.Entry{}, fmt.Errorf("%s is larger than %d MiB", want.File, maxArtifactBytes>>20)
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return release.Entry{}, fmt.Errorf("%s arrived truncated: %d of %d bytes", want.File, n, resp.ContentLength)
	}
	if err := tmp.Close(); err != nil {
		return release.Entry{}, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want.Hash {
		return release.Entry{}, fmt.Errorf("%s: sha256 %s, the signed release says %s", want.File, got, want.Hash)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return release.Entry{}, err
	}
	if err := os.Rename(tmpName, exePath); err != nil {
		return release.Entry{}, fmt.Errorf("replace %s: %w", exePath, err)
	}
	fmt.Fprintf(out, "upgraded %s to %s (%s)\n", exePath, want.File, release.HumanBytes(n))
	return want, nil
}

// refuseDowngrade is the anti-rollback rule. A signature proves a release is genuine; it does not
// prove it is current, so a compromised manager can replay an old but validly signed release and put
// a known-bad binary back on every node. When either version has no order (dev, ci, proof) the check
// allows the install and says so: refusing an unversioned build would make local development
// impossible, and the version is printed either way.
func refuseDowngrade(installedFile string, offered release.Entry) error {
	if installedFile == "" {
		return nil
	}
	installed, okOld := release.VersionFromFile(installedFile)
	offeredVersion, okNew := release.VersionFromFile(offered.File)
	if !okOld || !okNew {
		return nil
	}
	cmp, ok := release.CompareVersions(offeredVersion, installed)
	if !ok {
		return nil
	}
	if cmp < 0 {
		return fmt.Errorf("refusing to downgrade: %s is older than the installed %s", offered.File, installedFile)
	}
	return nil
}
