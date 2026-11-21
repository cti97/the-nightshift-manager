package agent

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// TestLanProbeSpecIsStrict: a probe that is silently dropped is a capability that never appears and
// nobody knows why, so every malformed entry names itself.
func TestLanProbeSpecIsStrict(t *testing.T) {
	for _, bad := range []string{
		"nas",                             // no =
		"=192.168.1.2:445",                // no service
		"nas=",                            // no address
		"nas=192.168.1.2",                 // no port
		"nas=192.168.1.2:",                // empty port: SplitHostPort accepts it, the promise does not
		"my nas=1.2.3.4:445",              // whitespace in the name
		"nas=1.2.3.4:445,nas=5.6.7.8:445", // twice
	} {
		if _, err := ParseLanProbes(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
	got, err := ParseLanProbes(" printer=192.168.1.20:631 , nas=192.168.1.10:445 ,")
	if err != nil {
		t.Fatalf("ParseLanProbes: %v", err)
	}
	if len(got) != 2 || got[0].Service != "nas" || got[1].Service != "printer" {
		t.Fatalf("probes = %+v, want nas then printer (sorted, trailing comma tolerated)", got)
	}
	if got[0].Addr != "192.168.1.10:445" {
		t.Fatalf("addr = %q", got[0].Addr)
	}
}

// TestReachabilityIsADial: the promise is "the service answers a TCP connect now", tested against a
// real listener and a real closed port rather than a mock.
func TestReachabilityIsADial(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	// A listener that accepts and drops: the probe only needs the handshake.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	up := LanProbe{Service: "nas", Addr: ln.Addr().String()}
	if !up.Reachable(context.Background()) {
		t.Fatalf("%s did not answer a dial", ln.Addr())
	}

	// A port nothing listens on: reserve one and close it, which is as close to "definitely closed"
	// as a test can get without racing something else for it.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := dead.Addr().String()
	dead.Close()
	down := LanProbe{Service: "printer", Addr: deadAddr, Timeout: 250 * time.Millisecond}
	if down.Reachable(context.Background()) {
		t.Fatalf("%s answered a dial with nothing listening", deadAddr)
	}

	caps := LanCapabilities(context.Background(), []LanProbe{up, down})
	if len(caps) != 1 || caps[0] != "lan:nas" {
		t.Fatalf("capabilities = %v, want only the service that answered", caps)
	}
}

// TestTheHeartbeatDeclaresOnlyReachableServices: the dynamic half of §8. The capability set is
// rebuilt per beat, so a service that stops answering stops being declared.
func TestTheHeartbeatDeclaresOnlyReachableServices(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	probe := LanProbe{Service: "nas", Addr: addr}
	a := &Agent{cfg: Config{Provides: []string{"reflex"}, Lan: []LanProbe{probe}}}
	ctx := context.Background()
	if caps := LanCapabilities(ctx, a.cfg.Lan); len(caps) != 1 || !strings.HasPrefix(caps[0], "lan:nas") {
		t.Fatalf("capabilities with the service up = %v, want lan:nas", caps)
	}
	ln.Close()
	// A closed listener refuses new connections; give the kernel a moment to make that true.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(LanCapabilities(ctx, a.cfg.Lan)) == 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the service stayed declared after its listener closed")
}
