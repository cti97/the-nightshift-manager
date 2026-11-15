package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

func TestRunVersionExitsZero(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "dev") {
		t.Fatalf("stdout = %q, want it to contain the version", out.String())
	}
}

func TestRunUnknownCommandExitsTwo(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"nope"}, &out, &errOut); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "usage") {
		t.Fatalf("stderr = %q, want usage text", errOut.String())
	}
}

func TestRunAgentWithoutRunExitsTwo(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"agent"}, &out, &errOut); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "usage") {
		t.Fatalf("stderr = %q, want usage text", errOut.String())
	}
}

func TestRunAgentRunRequiresNodeID(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"agent", "run"}, &out, &errOut); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "--node") {
		t.Fatalf("stderr = %q, want the missing --node named", errOut.String())
	}
}

func TestRunAgentRunRequiresNodeToken(t *testing.T) {
	t.Setenv("NSM_NODE_TOKEN", "")
	var out, errOut bytes.Buffer
	if code := run([]string{"agent", "run", "--node", "n"}, &out, &errOut); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "NSM_NODE_TOKEN") {
		t.Fatalf("stderr = %q, want NSM_NODE_TOKEN named", errOut.String())
	}
}

// TestParseAgentConfigCarriesLiveCapabilities pins the wiring that matters: the
// live capability set is what makes the node eligible, so it must reach the
// config exactly as given rather than being defaulted or dropped.
func TestParseAgentConfigCarriesLiveCapabilities(t *testing.T) {
	t.Setenv("NSM_NODE_TOKEN", "secret")
	var errOut bytes.Buffer
	cfg, ok := parseAgentConfig([]string{
		"--board", "http://127.0.0.1:9/", "--node", "n",
		"--provides", "agentic, vision", "--reach", "host, lan",
	}, &errOut)
	if !ok {
		t.Fatalf("parse failed: %s", errOut.String())
	}
	if cfg.BaseURL != "http://127.0.0.1:9/" || cfg.Token != "secret" || cfg.NodeID != "n" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.Protocol != board.ProtocolMax {
		t.Fatalf("protocol = %d, want %d", cfg.Protocol, board.ProtocolMax)
	}
	if cfg.Executor == nil {
		t.Fatal("executor = nil, want the stub executor")
	}
	if got := strings.Join(cfg.Provides, ","); got != "agentic,vision" {
		t.Fatalf("provides = %q", got)
	}
	if got := strings.Join(cfg.Reaches, ","); got != "host,lan" {
		t.Fatalf("reaches = %q", got)
	}
	if errOut.Len() != 0 {
		t.Fatalf("unexpected warning: %s", errOut.String())
	}
}

func TestParseAgentConfigWarnsOnEmptyProvides(t *testing.T) {
	t.Setenv("NSM_NODE_TOKEN", "secret")
	var errOut bytes.Buffer
	cfg, ok := parseAgentConfig([]string{"--node", "n", "--reach", "host"}, &errOut)
	if !ok {
		t.Fatalf("parse failed: %s", errOut.String())
	}
	if len(cfg.Provides) != 0 {
		t.Fatalf("provides = %v, want empty: nothing may be invented", cfg.Provides)
	}
	if !strings.Contains(errOut.String(), "claim nothing") {
		t.Fatalf("stderr = %q, want the empty-provides warning", errOut.String())
	}
}

// TestParseAgentConfigRequiresReach pins that reach is not guessed. A default
// of host,lan,wan claims every reach class including the ones the node may not
// have, which lets placement hand it work it cannot actually reach.
//
// It goes through parseAgentConfig, not run: with the guard removed, run would
// start the daemon and hang instead of failing, and a test that hangs is not a
// regression test.
func TestParseAgentConfigRequiresReach(t *testing.T) {
	t.Setenv("NSM_NODE_TOKEN", "secret")
	var errOut bytes.Buffer
	if _, ok := parseAgentConfig([]string{"--node", "n", "--provides", "agentic"}, &errOut); ok {
		t.Fatal("parse accepted a missing --reach")
	}
	if !strings.Contains(errOut.String(), "--reach") {
		t.Fatalf("stderr = %q, want the missing --reach named", errOut.String())
	}
}

func TestParseAgentConfigRejectsEmptyCapabilityEntry(t *testing.T) {
	t.Setenv("NSM_NODE_TOKEN", "secret")
	var errOut bytes.Buffer
	if _, ok := parseAgentConfig([]string{"--node", "n", "--reach", "host", "--provides", "agentic,,vision"}, &errOut); ok {
		t.Fatal("parse accepted an empty capability entry")
	}
	if !strings.Contains(errOut.String(), "empty entry") {
		t.Fatalf("stderr = %q, want the empty-entry error", errOut.String())
	}
}
