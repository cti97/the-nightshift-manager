// Package release is the signature and manifest arithmetic shared by the manager that serves a
// release and the agent that installs one. It is stdlib on purpose: `crypto/ed25519` means the
// verifier ships inside the binary that needs it, with no external tool to install on a Pi and no
// second copy of the checksum format to drift.
//
// The format is the one `scripts/build-release.sh` already writes: a SHA256SUMS file in coreutils
// form (`<hex>  <file>`), one line per artifact, whose filenames carry the target
// (`nsm-<version>-<os>-<arch>[.exe]`). Signing that file signs every artifact at once, and an
// operator who wants to check by hand can still run `sha256sum -c SHA256SUMS`.
package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// Entry is one artifact named by the checksum file.
type Entry struct {
	Hash string // lowercase hex, 64 characters
	File string // base name only: a release never names a path
	OS   string
	Arch string
}

// ParseSums reads coreutils `sha256sum` output. Blank lines and the `\` continuation used by some
// tools are ignored; anything else that is not a 64-hex-digit hash followed by a base name is an
// error, because a checksum file this package half-understands is worse than one it refuses.
func ParseSums(raw []byte) ([]Entry, error) {
	var out []Entry
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == `\` {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: want \"<sha256>  <file>\"", i+1)
		}
		hash, file := strings.ToLower(fields[0]), strings.TrimPrefix(fields[1], "*")
		if len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" {
			return nil, fmt.Errorf("line %d: %q is not a sha256", i+1, fields[0])
		}
		if strings.ContainsAny(file, "/\\") || file == "." || file == ".." {
			return nil, fmt.Errorf("line %d: %q is a path, not a file name", i+1, file)
		}
		e := Entry{Hash: hash, File: file}
		e.OS, e.Arch, _ = targetFromName(file)
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the checksum file names no artifacts")
	}
	return out, nil
}

// targetFromName reads the target out of `nsm-<version>-<os>-<arch>[.exe]`. A name that does not
// carry one yields empty values rather than an error: the file is still a valid checksum entry, it
// just cannot be matched to a running machine.
func targetFromName(name string) (osName, arch string, ok bool) {
	base := strings.TrimSuffix(name, ".exe")
	parts := strings.Split(base, "-")
	if len(parts) < 3 {
		return "", "", false
	}
	osName, arch = parts[len(parts)-2], parts[len(parts)-1]
	if osName == "" || arch == "" {
		return "", "", false
	}
	return osName, arch, true
}

// Find picks the entry for a machine. A release that names a target twice is ambiguous and refused
// rather than resolved by choosing the first: two artifacts claiming one machine is a build fault.
func Find(entries []Entry, osName, arch string) (Entry, error) {
	var found []Entry
	for _, e := range entries {
		if e.OS == osName && e.Arch == arch {
			found = append(found, e)
		}
	}
	switch len(found) {
	case 0:
		return Entry{}, fmt.Errorf("the release has no %s/%s artifact", osName, arch)
	case 1:
		return found[0], nil
	default:
		return Entry{}, fmt.Errorf("the release names %d %s/%s artifacts", len(found), osName, arch)
	}
}

// GenerateKey makes a signing key. The private half is returned base64 so it can be written to a
// 0600 file and never parsed by anything but this function's twin.
func GenerateKey() (private string, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(priv), base64.StdEncoding.EncodeToString(pub), nil
}

// ParsePrivateKey accepts the base64 form GenerateKey writes and refuses any other length: ed25519
// has exactly one private-key size, and a truncated key that "almost" parses is a signing bug.
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("the private key is not base64: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("the private key is %d bytes, want %d", len(raw), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(raw), nil
}

// ParsePublicKey accepts a base64 32-byte key. The agent pins one of these at install time; a
// release whose signature does not verify against it is refused before any file is written.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("the public key is not base64: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("the public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// Sign returns the base64 signature over the exact bytes of the checksum file.
func Sign(priv ed25519.PrivateKey, sums []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sums))
}

// Verify checks a signature over the exact bytes the manager served. Callers must verify the bytes
// they will parse, not a re-serialisation of them: a signature over a reformatted copy is a
// signature over something else.
func Verify(pub ed25519.PublicKey, sums []byte, sig string) error {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sig))
	if err != nil {
		return fmt.Errorf("the signature is not base64: %w", err)
	}
	if len(raw) != ed25519.SignatureSize {
		return fmt.Errorf("the signature is %d bytes, want %d", len(raw), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, sums, raw) {
		return fmt.Errorf("the signature does not match this checksum file")
	}
	return nil
}

// PublicFromPrivate is what `nsm dist sign` prints: the operator needs the pinned key, and asking
// them to run a second command to get it invites pinning the wrong one.
func PublicFromPrivate(priv ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
}

// Check runs the arithmetic an installer does, in the order that makes a failure legible: signature,
// then target, then checksum. The checksum is last because it needs the file, and a bad signature
// should stop the download before it starts.
func Check(pub ed25519.PublicKey, sums []byte, sig, osName, arch, gotHash string) (Entry, error) {
	if err := Verify(pub, sums, sig); err != nil {
		return Entry{}, err
	}
	entries, err := ParseSums(sums)
	if err != nil {
		return Entry{}, err
	}
	e, err := Find(entries, osName, arch)
	if err != nil {
		return Entry{}, err
	}
	if gotHash != "" && !strings.EqualFold(gotHash, e.Hash) {
		return Entry{}, fmt.Errorf("%s: sha256 %s, the signed release says %s", e.File, gotHash, e.Hash)
	}
	return e, nil
}

// HumanBytes is for the CLI's one-line summary; it stays here so the installer and the manager print
// sizes the same way.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
