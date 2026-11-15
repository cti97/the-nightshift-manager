package provider

import (
	"strings"
	"testing"
)

// TestCheckAuthRefIsTheOneNamespaceRule pins the rule both the board and the
// executor apply. It is deliberately a table over the spellings an operator can
// type and a row can hold, because the failure mode is a name that merely looks
// canonical: lowercase, a dash, a digit-first key, or the manager's own tokens.
func TestCheckAuthRefIsTheOneNamespaceRule(t *testing.T) {
	if AuthRefPrefix != "NSM_PROVIDER_" {
		t.Fatalf("AuthRefPrefix = %q, want NSM_PROVIDER_", AuthRefPrefix)
	}

	for _, tc := range []struct {
		ref string
		ok  bool
	}{
		{ref: "", ok: true},
		{ref: "NSM_PROVIDER_OPENAI", ok: true},
		{ref: "NSM_PROVIDER_OLLAMA_KEY", ok: true},
		{ref: "NSM_PROVIDER_A9", ok: true},
		{ref: "NSM_PROVIDER_A_B_C", ok: true},

		{ref: "NSM_ADMIN_TOKEN"},
		{ref: "NSM_ENROLL_TOKEN"},
		{ref: "OPENAI_API_KEY"},
		{ref: "nsm_provider_openai"},
		{ref: "NSM_PROVIDER_"},
		{ref: "NSM_PROVIDER_openai"},
		{ref: "NSM_PROVIDER_9KEY"},
		{ref: "NSM_PROVIDER_OPEN-AI"},
		{ref: "NSM_PROVIDER_OPEN AI"},
	} {
		err := CheckAuthRef(tc.ref)
		if tc.ok {
			if err != nil {
				t.Fatalf("CheckAuthRef(%q) = %v, want nil", tc.ref, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("CheckAuthRef(%q) = nil, want a refusal", tc.ref)
		}
		if !strings.Contains(err.Error(), "auth_ref") {
			t.Fatalf("CheckAuthRef(%q) error = %q, want it to name auth_ref", tc.ref, err)
		}
		if strings.Contains(err.Error(), "NSM_ADMIN_TOKEN") && tc.ref != "NSM_ADMIN_TOKEN" {
			t.Fatalf("CheckAuthRef(%q) error names the wrong value: %q", tc.ref, err)
		}
	}
}
