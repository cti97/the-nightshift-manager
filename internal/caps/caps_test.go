// internal/caps/caps_test.go
package caps

import (
	"reflect"
	"testing"
)

func TestParseRejectsUnknownCapability(t *testing.T) {
	if _, err := Parse([]string{"gpuu"}); err == nil {
		t.Fatal("Parse(unknown) returned nil error")
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
