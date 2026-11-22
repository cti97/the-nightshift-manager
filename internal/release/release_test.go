package release

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
)

// sumsFor signs a real checksum file for the six targets, which is the shape build-release.sh writes.
func sumsFor(t *testing.T) []byte {
	t.Helper()
	var b strings.Builder
	for _, name := range []string{
		"nsm-1.0.0-darwin-amd64", "nsm-1.0.0-darwin-arm64",
		"nsm-1.0.0-linux-amd64", "nsm-1.0.0-linux-arm64",
		"nsm-1.0.0-windows-amd64.exe", "nsm-1.0.0-windows-arm64.exe",
	} {
		b.WriteString(strings.Repeat("ab", 32) + "  " + name + "\n")
	}
	return []byte(b.String())
}

// TestSignVerifyRoundTripAndTamper is the whole security story in one place: the bytes that were
// signed verify, and a single flipped byte anywhere does not.
func TestSignVerifyRoundTripAndTamper(t *testing.T) {
	privB64, pubB64, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	priv, err := ParsePrivateKey(privB64)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	pub, err := ParsePublicKey(pubB64)
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	if PublicFromPrivate(priv) != pubB64 {
		t.Fatal("PublicFromPrivate disagrees with GenerateKey")
	}
	sums := sumsFor(t)
	sig := Sign(priv, sums)
	if err := Verify(pub, sums, sig); err != nil {
		t.Fatalf("a fresh signature did not verify: %v", err)
	}

	// One changed character in one hash line.
	tampered := []byte(strings.Replace(string(sums), "abababab", "abababac", 1))
	if err := Verify(pub, tampered, sig); err == nil {
		t.Fatal("a tampered checksum file verified")
	}
	// One appended line.
	appended := append(append([]byte{}, sums...), []byte(strings.Repeat("cd", 32)+"  nsm-1.0.0-plan9-mips\n")...)
	if err := Verify(pub, appended, sig); err == nil {
		t.Fatal("a checksum file with an appended artifact verified")
	}
	// Another key's signature.
	_, otherPubB64, _ := GenerateKey()
	otherPub, _ := ParsePublicKey(otherPubB64)
	if err := Verify(otherPub, sums, sig); err == nil {
		t.Fatal("a signature verified against a key that did not make it")
	}
	// A truncated signature.
	if err := Verify(pub, sums, sig[:20]); err == nil {
		t.Fatal("a truncated signature verified")
	}
}

// TestParseSumsReadsTheBuildOutput: the target is parsed out of the filename, and `*` prefixes (the
// binary mode marker sha256sum writes) do not change the file name.
func TestParseSumsReadsTheBuildOutput(t *testing.T) {
	entries, err := ParseSums(sumsFor(t))
	if err != nil {
		t.Fatalf("ParseSums: %v", err)
	}
	if len(entries) != 6 {
		t.Fatalf("entries = %d, want 6", len(entries))
	}
	e, err := Find(entries, "linux", "arm64")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if e.File != "nsm-1.0.0-linux-arm64" || e.Hash != strings.Repeat("ab", 32) {
		t.Fatalf("entry = %+v", e)
	}
	if _, err := Find(entries, "plan9", "mips"); err == nil {
		t.Fatal("a missing target was found")
	}

	star, err := ParseSums([]byte(strings.Repeat("ef", 32) + " *nsm-1.0.0-darwin-arm64\n"))
	if err != nil || star[0].File != "nsm-1.0.0-darwin-arm64" {
		t.Fatalf("binary-mode marker: %+v, %v", star, err)
	}
}

// TestParseSumsRefusesWhatItCannotFullyRead: a line this package half-understands is a checksum
// nobody checked.
func TestParseSumsRefusesWhatItCannotFullyRead(t *testing.T) {
	cases := map[string]string{
		"not a hash":       "zzzz  nsm-1.0.0-linux-amd64\n",
		"short hash":       "abcd  nsm-1.0.0-linux-amd64\n",
		"one field":        strings.Repeat("ab", 32) + "\n",
		"three fields":     strings.Repeat("ab", 32) + "  a b\n",
		"a path":           strings.Repeat("ab", 32) + "  ../../etc/passwd\n",
		"absolute path":    strings.Repeat("ab", 32) + "  /etc/passwd\n",
		"empty":            "",
		"only a backslash": "\\\n",
	}
	for label, body := range cases {
		if _, err := ParseSums([]byte(body)); err == nil {
			t.Fatalf("%s was accepted", label)
		}
	}
}

// TestTwoArtifactsForOneTargetIsRefused: a release that names the same machine twice is a build
// fault, and picking the first would hide it.
func TestTwoArtifactsForOneTargetIsRefused(t *testing.T) {
	body := strings.Repeat("ab", 32) + "  nsm-1.0.0-linux-amd64\n" +
		strings.Repeat("cd", 32) + "  nsm-1.0.0-linux-amd64\n"
	entries, err := ParseSums([]byte(body))
	if err != nil {
		t.Fatalf("ParseSums: %v", err)
	}
	if _, err := Find(entries, "linux", "amd64"); err == nil {
		t.Fatal("an ambiguous release resolved to one artifact")
	}
}

// TestCheckOrderIsSignatureFirst: a bad signature must stop an install before the checksum is even
// consulted, so a caller cannot be tricked into reporting a hash mismatch for an unsigned file.
func TestCheckOrderIsSignatureFirst(t *testing.T) {
	privB64, pubB64, _ := GenerateKey()
	priv, _ := ParsePrivateKey(privB64)
	pub, _ := ParsePublicKey(pubB64)
	sums := sumsFor(t)
	sig := Sign(priv, sums)

	entry, err := Check(pub, sums, sig, "darwin", "arm64", strings.Repeat("ab", 32))
	if err != nil || entry.File != "nsm-1.0.0-darwin-arm64" {
		t.Fatalf("Check: %+v, %v", entry, err)
	}

	if _, err := Check(pub, sums, sig, "darwin", "arm64", strings.Repeat("ff", 32)); err == nil {
		t.Fatal("a mismatched hash passed")
	} else if !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("the hash error does not say so: %v", err)
	}

	_, otherPubB64, _ := GenerateKey()
	otherPub, _ := ParsePublicKey(otherPubB64)
	if _, err := Check(otherPub, sums, sig, "darwin", "arm64", ""); err == nil {
		t.Fatal("a signature from another key passed")
	}
}

// TestKeysHaveExactlyOneLength: a truncated key that "almost" parses is a signing bug, so it is
// refused rather than padded.
func TestKeysHaveExactlyOneLength(t *testing.T) {
	short := []byte("abc")
	if _, err := ParsePrivateKey(hex.EncodeToString(short)); err == nil {
		t.Fatal("a short private key was accepted")
	}
	pub, _, _ := ed25519.GenerateKey(nil)
	if _, err := ParsePublicKey(strings.TrimSpace(string(pub))); err == nil {
		t.Fatal("a raw (non-base64) public key was accepted")
	}
}
