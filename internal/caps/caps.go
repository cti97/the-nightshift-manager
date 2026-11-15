// internal/caps/caps.go
package caps

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The closed vocabulary (spec §6). Unknown names are rejected at write time so
// a typo surfaces immediately instead of producing permanently unplaceable work.
var vocabulary = map[string]struct{}{
	"agentic": {}, "reflex": {}, "shell": {}, "network": {},
	"no_egress": {}, "strict_local": {}, "long_context": {},
	"vision": {}, "gpu": {},
}

const (
	localFSPrefix = "local_fs:"
	tokensPrefix  = "tokens:"
	lanPrefix     = "lan:"
)

// Parse validates and canonicalises a capability list: sorted, de-duplicated,
// and free of unknown names or malformed parameters.
func Parse(raw []string) ([]string, error) {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))

	for _, c := range raw {
		c = strings.TrimSpace(c)
		if c == "" {
			return nil, fmt.Errorf("empty capability")
		}
		if err := validate(c); err != nil {
			return nil, err
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

func validate(c string) error {
	switch {
	case strings.HasPrefix(c, localFSPrefix):
		p := strings.TrimPrefix(c, localFSPrefix)
		if !strings.HasPrefix(p, "/") {
			return fmt.Errorf("capability %q: local_fs path must be absolute", c)
		}
		if len(p) < 2 {
			return fmt.Errorf("capability %q: local_fs path must not be root", c)
		}
		return nil
	case strings.HasPrefix(c, tokensPrefix):
		n, err := strconv.Atoi(strings.TrimPrefix(c, tokensPrefix))
		if err != nil || n <= 0 {
			return fmt.Errorf("capability %q: tokens must be a positive integer", c)
		}
		return nil
	case strings.HasPrefix(c, lanPrefix):
		if strings.TrimSpace(strings.TrimPrefix(c, lanPrefix)) == "" {
			return fmt.Errorf("capability %q: lan needs a service name", c)
		}
		return nil
	default:
		if _, ok := vocabulary[c]; !ok {
			return fmt.Errorf("unknown capability %q", c)
		}
		return nil
	}
}

// Satisfies reports whether anything providing `provides` can serve `needs`.
// Parameterised capabilities do not match by string equality (spec §6).
func Satisfies(needs, provides []string) bool { return len(Missing(needs, provides)) == 0 }

// Missing returns the needs that `provides` does not cover, in need order.
func Missing(needs, provides []string) []string {
	var miss []string
	for _, n := range needs {
		if !satisfiedBy(n, provides) {
			miss = append(miss, n)
		}
	}
	return miss
}

func satisfiedBy(need string, provides []string) bool {
	for _, p := range provides {
		switch {
		case p == need:
			return true
		case strings.HasPrefix(need, localFSPrefix) && strings.HasPrefix(p, localFSPrefix):
			np := strings.TrimPrefix(need, localFSPrefix)
			pp := strings.TrimPrefix(p, localFSPrefix)
			if pp == np || strings.HasPrefix(np, strings.TrimSuffix(pp, "/")+"/") {
				return true
			}
		case strings.HasPrefix(need, tokensPrefix) && strings.HasPrefix(p, tokensPrefix):
			n, e1 := strconv.Atoi(strings.TrimPrefix(need, tokensPrefix))
			m, e2 := strconv.Atoi(strings.TrimPrefix(p, tokensPrefix))
			if e1 == nil && e2 == nil && m >= n {
				return true
			}
		}
	}
	return false
}
