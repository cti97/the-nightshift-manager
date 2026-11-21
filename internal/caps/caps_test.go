package caps

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseRejectsUnknownCapability(t *testing.T) {
	_, err := Parse([]string{"gpuu"})
	if err == nil {
		t.Fatal("Parse(unknown) returned nil error")
	}
	if !strings.Contains(err.Error(), "gpuu") {
		t.Fatalf("Parse(unknown) error %q does not name the capability", err)
	}
}

func TestParseCanonicalises(t *testing.T) {
	got, err := Parse([]string{"shell", "gpu", "shell"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := []string{"gpu", "shell"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse = %v, want %v", got, want)
	}
}

func TestParseRejectsMalformedParameters(t *testing.T) {
	for _, bad := range []string{"local_fs:", "local_fs:relative/path", "tokens:0", "tokens:abc", "lan:"} {
		if _, err := Parse([]string{bad}); err == nil {
			t.Fatalf("Parse(%q) returned nil error", bad)
		}
	}
}

func TestParseCanonicalisesPaths(t *testing.T) {
	cases := []struct{ raw, canon string }{
		{"local_fs:/a/", "local_fs:/a"},
		{"local_fs:/a//b", "local_fs:/a/b"},
		{"local_fs:/a/./b/", "local_fs:/a/b"},
		{"local_fs:/data/../../etc", "local_fs:/etc"},
	}
	for _, tc := range cases {
		got, err := Parse([]string{tc.raw})
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.raw, err)
		}
		if want := []string{tc.canon}; !reflect.DeepEqual(got, want) {
			t.Fatalf("Parse(%q) = %v, want %v", tc.raw, got, want)
		}
	}
	// Equivalent spellings of the same directory dedupe to one canonical name.
	got, err := Parse([]string{"local_fs:/a/", "local_fs:/a", "shell"})
	if err != nil {
		t.Fatalf("Parse(dedupe): %v", err)
	}
	if want := []string{"local_fs:/a", "shell"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse(dedupe) = %v, want %v", got, want)
	}
}

func TestParseRejectsRootAfterClean(t *testing.T) {
	for _, bad := range []string{"local_fs:/", "local_fs:/x/..", "local_fs:/.."} {
		if _, err := Parse([]string{bad}); err == nil {
			t.Fatalf("Parse(%q) returned nil error: a path that cleans to root must be rejected", bad)
		}
	}
}

func TestParseCanonicalisesLanName(t *testing.T) {
	got, err := Parse([]string{"lan: nas"})
	if err != nil {
		t.Fatalf("Parse(lan): %v", err)
	}
	if want := []string{"lan:nas"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse(%q) = %v, want %v", "lan: nas", got, want)
	}
	if _, err := Parse([]string{"lan:na s"}); err == nil {
		t.Fatal("Parse(lan with internal whitespace) returned nil error")
	}
}

// TestParseNil pins the empty-input contract: no error, and a non-nil empty
// canonical list.
func TestParseNil(t *testing.T) {
	got, err := Parse(nil)
	if err != nil {
		t.Fatalf("Parse(nil): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("Parse(nil) = %#v, want a non-nil empty slice", got)
	}
}

// TestParseDedupesAfterTrimming pins the TrimSpace path: entries are trimmed
// before validation and dedup, so " shell " and "shell" collapse to one name.
func TestParseDedupesAfterTrimming(t *testing.T) {
	got, err := Parse([]string{" shell ", "shell"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := []string{"shell"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse = %v, want %v", got, want)
	}
}

func TestSatisfies(t *testing.T) {
	cases := []struct {
		name     string
		needs    []string
		provides []string
		want     bool
	}{
		{"empty needs", nil, []string{"shell"}, true},
		{"exact", []string{"shell"}, []string{"shell", "gpu"}, true},
		{"absent", []string{"shell"}, []string{"gpu"}, false},
		{"local_fs prefix", []string{"local_fs:/a/b"}, []string{"local_fs:/a"}, true},
		{"local_fs too broad", []string{"local_fs:/a"}, []string{"local_fs:/a/b"}, false},
		{"local_fs must respect path boundary", []string{"local_fs:/ab"}, []string{"local_fs:/a"}, false},
		{"tokens sufficient", []string{"tokens:120000"}, []string{"tokens:200000"}, true},
		{"tokens insufficient", []string{"tokens:200000"}, []string{"tokens:120000"}, false},
		{"lan service", []string{"lan:nas"}, []string{"lan:nas"}, true},
		// Fix-round additions (review findings 3 and 4).
		{"tokens exact boundary", []string{"tokens:120000"}, []string{"tokens:120000"}, true},
		{"tokens malformed identical inputs do not match", []string{"tokens:abc"}, []string{"tokens:abc"}, false},
		{"tokens zero identical inputs do not match", []string{"tokens:0"}, []string{"tokens:0"}, false},
		{"local_fs need with trailing slash", []string{"local_fs:/a/"}, []string{"local_fs:/a"}, true},
		{"local_fs provider with trailing slash", []string{"local_fs:/a"}, []string{"local_fs:/a/"}, true},
		{"local_fs traversal escapes containment", []string{"local_fs:/data/../../etc"}, []string{"local_fs:/data"}, false},
		// Fix-round-2 additions: identical malformed local_fs spellings must
		// not match either (see the comment on parameterised).
		{"local_fs empty path identical inputs do not match", []string{"local_fs:"}, []string{"local_fs:"}, false},
		{"local_fs relative identical inputs do not match", []string{"local_fs:rel"}, []string{"local_fs:rel"}, false},
		{"local_fs traversal identical inputs do not match", []string{"local_fs:/x/.."}, []string{"local_fs:/x/.."}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Satisfies(tc.needs, tc.provides); got != tc.want {
				t.Fatalf("Satisfies(%v, %v) = %v, want %v", tc.needs, tc.provides, got, tc.want)
			}
		})
	}
}

func TestMissingNamesTheGap(t *testing.T) {
	got := Missing([]string{"agentic", "gpu"}, []string{"agentic"})
	if want := []string{"gpu"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Missing = %v, want %v", got, want)
	}
}

func TestMissingAllCovered(t *testing.T) {
	if got := Missing([]string{"agentic", "gpu"}, []string{"gpu", "agentic"}); len(got) != 0 {
		t.Fatalf("Missing = %v, want no gaps", got)
	}
}

// TestSandboxCapabilitiesAreClosedAndNotALadder: the profile names are a closed set, and the only
// ordering is the directory floor. Seatbelt and bwrap are different mechanisms on different
// platforms; treating one as "at least" the other would place work on a boundary the scheduler
// cannot reason about.
func TestSandboxCapabilitiesAreClosedAndNotALadder(t *testing.T) {
	for _, ok := range []string{"sandbox:none", "sandbox:dir", "sandbox:bwrap", "sandbox:systemd_scope", "sandbox:seatbelt"} {
		if _, err := Parse([]string{ok}); err != nil {
			t.Fatalf("Parse(%q) = %v, want it accepted", ok, err)
		}
	}
	// Parse trims the whole capability before validating, as it does for every prefix, so a
	// trailing space is normalised rather than refused.
	for _, bad := range []string{"sandbox:quantum", "sandbox:", "sandbox: bwrap"} {
		if _, err := Parse([]string{bad}); err == nil {
			t.Fatalf("Parse(%q) accepted an unknown profile", bad)
		}
	}

	cases := []struct {
		need, have string
		want       bool
	}{
		{"sandbox:seatbelt", "sandbox:seatbelt", true},
		{"sandbox:dir", "sandbox:seatbelt", true},    // the one true hierarchy: a mechanism is a directory too
		{"sandbox:dir", "sandbox:bwrap", true},       //
		{"sandbox:dir", "sandbox:none", false},       // "none" confines nothing, not even a directory
		{"sandbox:seatbelt", "sandbox:bwrap", false}, // different mechanisms, not rungs
		{"sandbox:bwrap", "sandbox:systemd_scope", false},
		{"sandbox:seatbelt", "", false},
	}
	for _, c := range cases {
		var provides []string
		if c.have != "" {
			provides = []string{c.have}
		}
		if got := Satisfies([]string{c.need}, provides); got != c.want {
			t.Fatalf("Satisfies(%q, %v) = %v, want %v", c.need, provides, got, c.want)
		}
	}
}

// TestSandboxProfileReadsTheDeclaration: a node that declares nothing is "none", not "unknown" —
// the dashboard needs one word for "this machine cannot confine a run".
func TestSandboxProfileReadsTheDeclaration(t *testing.T) {
	if got := SandboxProfile([]string{"agentic"}); got != "none" {
		t.Fatalf("a node declaring no profile = %q, want none", got)
	}
	if got := SandboxProfile([]string{"agentic", "sandbox:seatbelt"}); got != "seatbelt" {
		t.Fatalf("profile = %q, want seatbelt", got)
	}
}
