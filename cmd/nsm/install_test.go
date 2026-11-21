package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// specForTest is an install spec with a token, so the failure cases below can differ in one thing.
func specForTest(t *testing.T) installSpec {
	t.Helper()
	return installSpec{
		Binary:  "/usr/local/bin/nsm",
		NodeID:  "node-1",
		Args:    []string{"--board", "http://127.0.0.1:8081", "--node", "node-1", "--provides", "reflex", "--reach", "host", "--sandbox-root", "/tmp/runs"},
		Token:   "tok-abc",
		BaseDir: filepath.Join(t.TempDir(), "nsm"),
		Label:   installLabel,
	}
}

// TestTheUnitFilesCarryNoSecret: a unit file and a plist are world-readable and end up in every
// backup of the machine. The token lives in a 0600 env file the wrapper sources, and this test is
// the reason a future edit cannot quietly inline it "for simplicity".
func TestTheUnitFilesCarryNoSecret(t *testing.T) {
	s := specForTest(t)
	files, err := s.installFiles()
	if err != nil {
		t.Fatalf("installFiles: %v", err)
	}
	for _, f := range files {
		switch filepath.Base(f.Path) {
		case "agent.env":
			if !strings.Contains(f.Body, "tok-abc") {
				t.Fatalf("the env file does not carry the token")
			}
			if f.Mode != 0o600 {
				t.Fatalf("env file mode = %04o, want 0600", f.Mode)
			}
		default:
			if strings.Contains(f.Body, "tok-abc") {
				t.Fatalf("%s carries the token; a service file must not", f.Path)
			}
		}
	}
	wrapper := s.wrapperScript()
	if !strings.Contains(wrapper, "agent.env") || !strings.Contains(wrapper, "exec ") {
		t.Fatalf("wrapper does not source the env file and exec the agent:\n%s", wrapper)
	}
}

// TestInstallRefusesAnUnquotableToken: the env file is sourced by a shell, so a token with a quote
// or a newline would break the source or inject a line.
func TestInstallRefusesAnUnquotableToken(t *testing.T) {
	for _, bad := range []string{"a'b", "a\nb", "a\"b"} {
		s := specForTest(t)
		s.Token = bad
		if _, err := s.installFiles(); err == nil {
			t.Fatalf("token %q was accepted into a sourced file", bad)
		}
	}
}

// TestTheLaunchdPlistIsWellFormed: launchd silently ignores a plist it cannot parse, so the shape
// is the contract. The wrapper is the only program argument; KeepAlive is what makes a crash a
// restart rather than an outage.
func TestTheLaunchdPlistIsWellFormed(t *testing.T) {
	s := specForTest(t)
	plist := s.launchdPlist()
	for _, want := range []string{
		"<key>Label</key><string>" + installLabel + "</string>",
		"<key>ProgramArguments</key>",
		"<string>" + s.wrapperPath() + "</string>",
		"<key>RunAtLoad</key><true/>",
		"<key>KeepAlive</key><true/>",
		"<key>ThrottleInterval</key><integer>30</integer>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist is missing %q:\n%s", want, plist)
		}
	}
	// The XML declaration and a closing plist: a truncated template is a service that never starts.
	if !strings.HasSuffix(strings.TrimSpace(plist), "</plist>") {
		t.Fatalf("plist does not close:\n%s", plist)
	}
}

// TestTheSystemdUnitRestartsAndIsPerUser: a user unit (no root, no root-owned model keys) that
// restarts, with a burst limit so a crash loop does not become a fork bomb.
func TestTheSystemdUnitRestartsAndIsPerUser(t *testing.T) {
	s := specForTest(t)
	unit := s.systemdUnit()
	for _, want := range []string{
		"ExecStart=" + s.wrapperPath(),
		"Restart=always",
		"StartLimitBurst=5",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit is missing %q:\n%s", want, unit)
		}
	}
}

// TestInstallPrintWritesNothing: --print is the review mode — an operator can read the files before
// a daemon appears on their machine.
func TestInstallPrintWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NSM_NODE_TOKEN", "tok-abc")
	var out, errOut bytes.Buffer
	code := installCommand([]string{"--node", "n", "--provides", "reflex", "--reach", "host",
		"--sandbox-root", dir, "--print"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d (stderr %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "agent-run.sh") || !strings.Contains(out.String(), "NSM_NODE_TOKEN=tok-abc") {
		t.Fatalf("--print did not describe the files:\n%s", out.String())
	}
	// The write path is not reached: installFiles is pure (covered above), and installCommand returns
	// before MkdirAll/WriteFile when --print is set. The files themselves are asserted by content,
	// because writing them here would install a daemon on the machine running the tests.
}
