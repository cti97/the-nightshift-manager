package provider

import (
	"fmt"
	"strings"
)

// AuthRefPrefix is the only namespace an endpoint's auth_ref may name.
//
// The rule exists because auth_ref is the one column that makes a process read
// its own environment on a row's instruction. A free-form name turns that into a
// credential-exfiltration primitive: a row naming NSM_ADMIN_TOKEN, pointed at
// any URL, would have the manager (or the node) send its own admin token as a
// bearer header. A reserved namespace makes the manager's and the node's own
// tokens unmatchable by construction rather than by review — and provider keys
// are exactly what belongs in it.
//
// It lives in this package because there are two consumers of the same rule: the
// board's endpoint write path and health probe, and the node's executor
// resolving the key from its own environment. A second copy in internal/agent
// is how the two would drift, and the weaker copy would be the one that reads an
// environment variable.
const AuthRefPrefix = "NSM_PROVIDER_"

// CheckAuthRef refuses any value that is not a name in the provider-key
// namespace: NSM_PROVIDER_ followed by an uppercase name (A-Z, 0-9, _ starting
// with a letter). It is deliberately the same rule everywhere it is called:
//
//   - the board's endpoint write path calls it so a typo is a 400 at creation;
//   - the board's probe calls it again so a row the write path never saw (a
//     fixture, a migration, raw SQL) cannot reach outside the namespace;
//   - the node's executor calls it before os.Getenv, because the claim payload
//     is another input that could name anything.
//
// An empty auth_ref is legitimate: a local llama.cpp or LM Studio server usually
// has no key, and a row that names nothing must be able to say so. It is not
// trimmed here: the callers trim, so the stored and transmitted value is what
// was checked, and a padded spelling cannot pass one check and fail another.
func CheckAuthRef(ref string) error {
	if ref == "" {
		return nil
	}
	if !strings.HasPrefix(ref, AuthRefPrefix) {
		return fmt.Errorf("auth_ref %q: must name a provider key in the %s* namespace", ref, AuthRefPrefix)
	}
	name := strings.TrimPrefix(ref, AuthRefPrefix)
	if name == "" {
		return fmt.Errorf("auth_ref %q: names must be %s followed by an uppercase name", ref, AuthRefPrefix)
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return fmt.Errorf("auth_ref %q: character %q is not allowed after %s (want A-Z, 0-9 or _)", ref, r, AuthRefPrefix)
		}
		if i == 0 && (r < 'A' || r > 'Z') {
			return fmt.Errorf("auth_ref %q: the name after %s must start with an uppercase letter", ref, AuthRefPrefix)
		}
	}
	return nil
}
