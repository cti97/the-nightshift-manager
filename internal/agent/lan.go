// internal/agent/lan.go
package agent

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// LanProbe is one service a node promises is reachable from inside the home network (spec §7.2,
// §8). The promise is dynamic: a laptop that goes to the office is no longer inside the LAN, so the
// capability must be declared only while the check passes, not once at registration.
type LanProbe struct {
	// Service is the capability suffix: `lan:nas` is declared when Addr answers.
	Service string
	// Addr is host:port to dial. A TCP connect is the test because it is what "the service is
	// there" means to a caller: nothing here speaks SMB or CUPS, and pretending to probe deeper
	// would be a lie a placement decision could act on.
	Addr string
	// Timeout bounds one dial. Zero selects defaultLanTimeout.
	Timeout time.Duration
}

// defaultLanTimeout is short on purpose: a heartbeat runs every fifteen seconds and an
// unreachable service must not hold it open. A LAN that cannot answer in a second is a LAN a
// placement decision should not count on.
const defaultLanTimeout = time.Second

// ParseLanProbes reads "nas=192.168.1.10:445,printer=192.168.1.20:631" — the form an operator types
// in a unit file. Malformed entries are refused with the offending entry named: a silently dropped
// probe is a capability that never appears and nobody knows why.
func ParseLanProbes(spec string) ([]LanProbe, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	var out []LanProbe
	seen := make(map[string]struct{})
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, addr, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("--lan %q: want service=host:port", part)
		}
		name, addr = strings.TrimSpace(name), strings.TrimSpace(addr)
		if name == "" {
			return nil, fmt.Errorf("--lan %q: the service name is empty", part)
		}
		if strings.ContainsAny(name, " :,") {
			return nil, fmt.Errorf("--lan %q: a service name is one word (it becomes lan:%s)", part, name)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("--lan %q: %s is probed twice", part, name)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil || host == "" || port == "" {
			return nil, fmt.Errorf("--lan %q: %q is not host:port", part, addr)
		}
		seen[name] = struct{}{}
		out = append(out, LanProbe{Service: name, Addr: addr})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out, nil
}

// Reachable reports whether the service answers a TCP connect within its timeout.
func (p LanProbe) Reachable(ctx context.Context) bool {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultLanTimeout
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", p.Addr)
	if err != nil {
		return false
	}
	// The connection is the evidence; nothing is sent. Close immediately so a probe does not sit in
	// the service's accept queue doing nothing.
	_ = conn.Close()
	return true
}

// LanCapabilities is the `lan:<service>` set for the probes that answer right now. It is computed
// on every heartbeat and never cached: a cached capability is a capability the node stopped having
// when its owner closed the lid.
func LanCapabilities(ctx context.Context, probes []LanProbe) []string {
	var caps []string
	for _, p := range probes {
		if p.Reachable(ctx) {
			caps = append(caps, "lan:"+p.Service)
		}
	}
	return caps
}
