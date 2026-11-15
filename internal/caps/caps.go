// Package caps defines the closed capability vocabulary, its canonical parsing, and subset matching (spec §6).
package caps

import (
	"fmt"
	"path"
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

// Parse validates and canonicalises a capability list: trimmed, sorted, and
// de-duplicated; local_fs paths are reduced to their canonical form, and
// unknown names or malformed parameters are rejected.
func Parse(raw []string) ([]string, error) {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))

	for _, c := range raw {
		c = strings.TrimSpace(c)
		if c == "" {
			return nil, fmt.Errorf("empty capability")
		}
		canon, err := validate(c)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[canon]; dup {
			continue
		}
		seen[canon] = struct{}{}
		out = append(out, canon)
	}
	sort.Strings(out)
	return out, nil
}

// validate checks c and returns its canonical spelling.
func validate(c string) (string, error) {
	switch {
	case strings.HasPrefix(c, localFSPrefix):
		p := strings.TrimPrefix(c, localFSPrefix)
		if !strings.HasPrefix(p, "/") {
			return "", fmt.Errorf("capability %q: local_fs path must be absolute", c)
		}
		p = path.Clean(p)
		if p == "/" {
			// path.Clean("/") is "/", so this also catches paths that only
			// collapse to root after cleaning: no node claims the whole
			// filesystem.
			return "", fmt.Errorf("capability %q: local_fs path must not be root", c)
		}
		return localFSPrefix + p, nil
	case strings.HasPrefix(c, tokensPrefix):
		n, err := strconv.Atoi(strings.TrimPrefix(c, tokensPrefix))
		if err != nil || n <= 0 {
			return "", fmt.Errorf("capability %q: tokens must be a positive integer", c)
		}
		return c, nil
	case strings.HasPrefix(c, lanPrefix):
		name := strings.TrimSpace(strings.TrimPrefix(c, lanPrefix))
		if name == "" {
			return "", fmt.Errorf("capability %q: lan needs a service name", c)
		}
		if len(strings.Fields(name)) != 1 {
			return "", fmt.Errorf("capability %q: lan service name must not contain whitespace", c)
		}
		return lanPrefix + name, nil
	default:
		if _, ok := vocabulary[c]; !ok {
			return "", fmt.Errorf("unknown capability %q", c)
		}
		return c, nil
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

// parameterised reports whether c carries one of the parameterised prefixes.
// Such capabilities match only through their typed rule, never plain string
// equality, so two identical malformed spellings cannot satisfy each other.
func parameterised(c string) bool {
	return strings.HasPrefix(c, localFSPrefix) ||
		strings.HasPrefix(c, tokensPrefix) ||
		strings.HasPrefix(c, lanPrefix)
}

// lanName extracts the service name of a lan: capability and reports whether
// it is well-formed: exactly one whitespace-free token.
func lanName(c string) (string, bool) {
	fields := strings.Fields(strings.TrimPrefix(c, lanPrefix))
	if len(fields) != 1 {
		return "", false
	}
	return fields[0], true
}

func satisfiedBy(need string, provides []string) bool {
	for _, p := range provides {
		switch {
		case !parameterised(need) && !parameterised(p) && p == need:
			return true
		case strings.HasPrefix(need, localFSPrefix) && strings.HasPrefix(p, localFSPrefix):
			// Defensive normalisation: canonical lists from Parse are already
			// clean, but cleaning here keeps stray trailing slashes and
			// traversal segments from faking containment.
			np := path.Clean(strings.TrimPrefix(need, localFSPrefix))
			pp := path.Clean(strings.TrimPrefix(p, localFSPrefix))
			if np == pp || strings.HasPrefix(np, pp+"/") {
				return true
			}
		case strings.HasPrefix(need, tokensPrefix) && strings.HasPrefix(p, tokensPrefix):
			n, e1 := strconv.Atoi(strings.TrimPrefix(need, tokensPrefix))
			m, e2 := strconv.Atoi(strings.TrimPrefix(p, tokensPrefix))
			if e1 == nil && e2 == nil && n > 0 && m >= n {
				return true
			}
		case strings.HasPrefix(need, lanPrefix) && strings.HasPrefix(p, lanPrefix):
			n, ok1 := lanName(need)
			pn, ok2 := lanName(p)
			if ok1 && ok2 && n == pn {
				return true
			}
		}
	}
	return false
}
