package version

import "testing"

func TestStringIncludesVersionAndCommit(t *testing.T) {
	Version, Commit = "v1.2.3", "abc1234"
	defer func() { Version, Commit = "dev", "unknown" }()

	if got, want := String(), "v1.2.3 (abc1234)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestStringOmitsUnknownCommit(t *testing.T) {
	Version, Commit = "v1.2.3", "unknown"
	defer func() { Version, Commit = "dev", "unknown" }()

	if got, want := String(), "v1.2.3"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
