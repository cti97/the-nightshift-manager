package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cti97/the-nightshift-manager/internal/agent"
	"github.com/cti97/the-nightshift-manager/internal/board"
	"github.com/cti97/the-nightshift-manager/internal/provider"
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
	sandboxRoot := t.TempDir()
	var errOut bytes.Buffer
	cfg, ok := parseAgentConfig([]string{
		"--board", "http://127.0.0.1:9/", "--node", "n",
		"--provides", "agentic, vision", "--reach", "host, lan",
		"--sandbox-root", sandboxRoot,
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
	exec, ok := cfg.Executor.(*agent.AgenticExecutor)
	if !ok {
		t.Fatalf("executor = %T, want *agent.AgenticExecutor: the node must execute, not stub", cfg.Executor)
	}
	if exec.Registry == nil {
		t.Fatal("executor has no provider registry: no endpoint could be constructed")
	}
	if got := strings.Join(cfg.Provides, ","); got != "agentic,vision" {
		t.Fatalf("provides = %q", got)
	}
	if got := strings.Join(cfg.Reaches, ","); got != "host,lan" {
		t.Fatalf("reaches = %q", got)
	}
	if exec.SandboxRoot != sandboxRoot {
		t.Fatalf("executor sandbox root = %q, want %q: the run root is the flag, not a second configuration", exec.SandboxRoot, sandboxRoot)
	}
	if errOut.Len() != 0 {
		t.Fatalf("unexpected warning: %s", errOut.String())
	}
}

func TestParseAgentConfigWarnsOnEmptyProvides(t *testing.T) {
	t.Setenv("NSM_NODE_TOKEN", "secret")
	var errOut bytes.Buffer
	cfg, ok := parseAgentConfig([]string{"--node", "n", "--reach", "host", "--sandbox-root", t.TempDir()}, &errOut)
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
	if _, ok := parseAgentConfig([]string{"--node", "n", "--provides", "agentic", "--sandbox-root", t.TempDir()}, &errOut); ok {
		t.Fatal("parse accepted a missing --reach")
	}
	if !strings.Contains(errOut.String(), "--reach") {
		t.Fatalf("stderr = %q, want the missing --reach named", errOut.String())
	}
}

// TestParseAgentConfigRequiresSandboxRoot pins that the run root is named, not
// guessed. A default would put every node's runs somewhere no one chose.
func TestParseAgentConfigRequiresSandboxRoot(t *testing.T) {
	t.Setenv("NSM_NODE_TOKEN", "secret")
	var errOut bytes.Buffer
	if _, ok := parseAgentConfig([]string{"--node", "n", "--reach", "host"}, &errOut); ok {
		t.Fatal("parse accepted a missing --sandbox-root")
	}
	if !strings.Contains(errOut.String(), "--sandbox-root") {
		t.Fatalf("stderr = %q, want the missing --sandbox-root named", errOut.String())
	}
	if _, ok := parseAgentConfig([]string{"--node", "n", "--reach", "host", "--sandbox-root", "runs"}, &errOut); ok {
		t.Fatal("parse accepted a relative --sandbox-root")
	}
	if !strings.Contains(errOut.String(), "absolute") {
		t.Fatalf("stderr = %q, want the relative-root refusal named", errOut.String())
	}
}

// TestParseAgentConfigForgetsTheNodeTokenInTheEnvironment pins the scrub: the
// token reaches the config and is gone from the in-process environment, so a
// child started later does not inherit it. It does not claim the token is gone
// from /proc/<pid>/environ, which preserves the exec-time block on Linux.
func TestParseAgentConfigForgetsTheNodeTokenInTheEnvironment(t *testing.T) {
	t.Setenv("NSM_NODE_TOKEN", "secret-token-value")
	var errOut bytes.Buffer
	cfg, ok := parseAgentConfig([]string{"--node", "n", "--reach", "host", "--sandbox-root", t.TempDir()}, &errOut)
	if !ok {
		t.Fatalf("parse failed: %s", errOut.String())
	}
	if cfg.Token != "secret-token-value" {
		t.Fatalf("token = %q, want the value from the environment", cfg.Token)
	}
	if got := os.Getenv("NSM_NODE_TOKEN"); got != "" {
		t.Fatalf("NSM_NODE_TOKEN is still in the environment as %q", got)
	}
}

func TestParseAgentConfigRejectsEmptyCapabilityEntry(t *testing.T) {
	t.Setenv("NSM_NODE_TOKEN", "secret")
	var errOut bytes.Buffer
	if _, ok := parseAgentConfig([]string{"--node", "n", "--reach", "host", "--provides", "agentic,,vision", "--sandbox-root", t.TempDir()}, &errOut); ok {
		t.Fatal("parse accepted an empty capability entry")
	}
	if !strings.Contains(errOut.String(), "empty entry") {
		t.Fatalf("stderr = %q, want the empty-entry error", errOut.String())
	}
}

// managerTokens is the one place both credentials enter the process. They must
// be distinct: the enrollment secret exists so an enrolling node never carries
// the admin credential, and setting both to one value would quietly hand every
// node the keys to the board.
func TestManagerTokensRequiresTwoDistinctCredentials(t *testing.T) {
	t.Setenv("NSM_ADMIN_TOKEN", "")
	var errOut bytes.Buffer
	if _, _, ok := managerTokens(&errOut); ok {
		t.Fatal("managerTokens accepted a missing admin token")
	}
	if !strings.Contains(errOut.String(), "NSM_ADMIN_TOKEN") {
		t.Fatalf("stderr = %q, want NSM_ADMIN_TOKEN named", errOut.String())
	}

	t.Setenv("NSM_ADMIN_TOKEN", "admin")
	for _, tc := range []struct{ enroll, want string }{
		{"", "NSM_ENROLL_TOKEN is required"},
		{"admin", "must differ"},
	} {
		t.Setenv("NSM_ENROLL_TOKEN", tc.enroll)
		var buf bytes.Buffer
		if _, _, ok := managerTokens(&buf); ok {
			t.Fatalf("managerTokens accepted enroll=%q alongside admin", tc.enroll)
		}
		if !strings.Contains(buf.String(), tc.want) {
			t.Fatalf("stderr = %q, want it to contain %q", buf.String(), tc.want)
		}
	}

	t.Setenv("NSM_ENROLL_TOKEN", "enroll")
	var noise bytes.Buffer
	admin, enroll, valid := managerTokens(&noise)
	if !valid || admin != "admin" || enroll != "enroll" {
		t.Fatalf("managerTokens = (%q, %q, %v), want admin/enroll/true", admin, enroll, valid)
	}
	if noise.Len() != 0 {
		t.Fatalf("unexpected output: %q", noise.String())
	}
}

// field reads a "key: value" line from the register command's stdout.
func field(t *testing.T, stdout, key string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if rest, found := strings.CutPrefix(line, key+": "); found {
			return rest
		}
	}
	return ""
}

// The enrollment path end to end: the CLI sends the enrollment credential, the
// real board accepts it, and the token it prints is one the node can actually
// authenticate with. A node that cannot register cannot exist, so this is the
// smoke path for C2.
func TestNodeRegisterEnrollsAgainstARealBoard(t *testing.T) {
	db, clock := board.OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	srv := httptest.NewServer(board.NewServer(db, clock, provider.NewRegistry(),
		board.HashToken("admin-token"), board.HashToken("enroll-token")).Handler())
	defer srv.Close()

	t.Setenv("NSM_ENROLL_TOKEN", "enroll-token")
	var out, errOut bytes.Buffer
	code := run([]string{"node", "register",
		"--board", srv.URL, "--name", "laptop",
		"--provides", "agentic", "--reach", "host,lan", "--concurrency", "2",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut.String())
	}

	nodeID := field(t, out.String(), "node_id")
	nodeToken := field(t, out.String(), "token")
	if nodeID == "" || nodeToken == "" {
		t.Fatalf("stdout = %q, want a node_id and a token", out.String())
	}
	if !strings.Contains(errOut.String(), "NSM_NODE_TOKEN") {
		t.Fatalf("stderr = %q, want it to tell the operator where the token goes", errOut.String())
	}

	var status, reach string
	if err := db.QueryRow(`SELECT status, reach_classes FROM nodes WHERE id=?`, nodeID).Scan(&status, &reach); err != nil {
		t.Fatalf("read the registered node: %v", err)
	}
	if status != "offline" {
		t.Fatalf("node status = %q, want offline until the first heartbeat", status)
	}
	if reach != `["host","lan"]` {
		t.Fatalf("reach_classes = %s, want the declared set", reach)
	}

	// The printed token is the node credential: it heartbeats.
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/nodes/heartbeat",
		strings.NewReader(fmt.Sprintf(`{"protocol_version":%d,"provides_live":["agentic"],"reach_classes":["host","lan"]}`, board.ProtocolMax)))
	if err != nil {
		t.Fatalf("build heartbeat: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+nodeToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat with the issued token = %d, want 200", resp.StatusCode)
	}
	var beat board.HeartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&beat); err != nil {
		t.Fatalf("decode heartbeat: %v", err)
	}
	if beat.Status != "online" {
		t.Fatalf("heartbeat status = %q, want online", beat.Status)
	}
}

// A wrong enrollment credential is a board refusal, not a crash: the command
// exits 1 and says what the board said.
func TestNodeRegisterReportsAnEnrollmentRefusal(t *testing.T) {
	db, clock := board.OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	srv := httptest.NewServer(board.NewServer(db, clock, provider.NewRegistry(),
		board.HashToken("admin-token"), board.HashToken("enroll-token")).Handler())
	defer srv.Close()

	t.Setenv("NSM_ENROLL_TOKEN", "wrong")
	var out, errOut bytes.Buffer
	if code := run([]string{"node", "register", "--board", srv.URL, "--name", "laptop"}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "401") {
		t.Fatalf("stderr = %q, want the board's refusal", errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing on a refusal", out.String())
	}

	var nodes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if nodes != 0 {
		t.Fatalf("nodes = %d after a refused enrollment, want 0", nodes)
	}
}

func TestNodeRegisterRequiresEnrollToken(t *testing.T) {
	t.Setenv("NSM_ENROLL_TOKEN", "")
	var out, errOut bytes.Buffer
	if code := run([]string{"node", "register", "--name", "laptop"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "NSM_ENROLL_TOKEN") {
		t.Fatalf("stderr = %q, want NSM_ENROLL_TOKEN named", errOut.String())
	}
}
