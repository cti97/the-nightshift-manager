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
