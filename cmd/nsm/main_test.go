package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// parkedBoard is a real board with the admin server in front of it and one item
// parked for the given action. It is the fixture the three CLI tests share.
func parkedBoard(t *testing.T, action string) (srv *httptest.Server, itemID string) {
	t.Helper()
	db, clock := board.OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	node, _, err := board.RegisterNode(context.Background(), db, clock, board.RegisterRequest{
		Name: "a", Role: "executor", ProvidesDeclared: []string{"agentic"},
		ReachClasses: []string{"host", "lan", "wan"}, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := board.Heartbeat(context.Background(), db, clock, node, board.HeartbeatRequest{
		ProvidesLive: []string{"agentic"}, ReachClasses: []string{"host", "lan", "wan"}, Protocol: board.ProtocolMax,
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	board.SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")
	itemID, err = board.CreateItem(context.Background(), db, clock, board.CreateItemRequest{
		Title: "cache cleanup", Brief: "b", Needs: []string{"agentic"},
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	claimed, err := board.Claim(context.Background(), db, clock, node, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %d items, err %v", len(claimed), err)
	}
	if err := board.StartRun(context.Background(), db, clock, claimed[0].RunID, node); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := board.Escalate(context.Background(), db, clock, claimed[0].RunID, node,
		board.ApprovalRequest{Action: action, Reason: "the run needs the disk"},
		board.RunTokens{}); err != nil {
		t.Fatalf("escalate: %v", err)
	}

	srv = httptest.NewServer(board.NewServer(db, clock, provider.NewRegistry(),
		board.HashToken("admin-secret"), board.HashToken("enroll-secret")).Handler())
	t.Cleanup(srv.Close)
	return srv, itemID
}

// TestApprovalsCommandPrintsTheQueue: the terminal operator sees the id to copy,
// the action, and how long it has waited. An empty queue prints [], so "nothing is
// waiting" is an answer rather than a blank line a script cannot tell from a
// failure.
func TestApprovalsCommandPrintsTheQueue(t *testing.T) {
	srv, itemID := parkedBoard(t, "delete /var/cache/app")
	t.Setenv("NSM_ADMIN_TOKEN", "admin-secret")

	var out, errOut bytes.Buffer
	if code := run([]string{"approvals", "--board", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %s)", code, errOut.String())
	}
	for _, want := range []string{itemID, "delete /var/cache/app", "the run needs the disk", "1 waiting"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout = %q, want it to contain %q", out.String(), want)
		}
	}

	// A second, empty board answers [].
	empty, _ := board.OpenForTest(t, filepath.Join(t.TempDir(), "empty.db"))
	emptySrv := httptest.NewServer(board.NewServer(empty, board.NewFixedClock(1),
		provider.NewRegistry(), board.HashToken("admin-secret"), board.HashToken("enroll-secret")).Handler())
	t.Cleanup(emptySrv.Close)
	out.Reset()
	if code := run([]string{"approvals", "--board", emptySrv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("empty board: exit = %d, want 0 (stderr %s)", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != "[]" {
		t.Fatalf("empty queue printed %q, want []", got)
	}
}

// TestApproveWithoutANoteIsAcceptedButSaysSo: the decision goes through (the board
// allows it), and the command says on stderr that a note would have made it
// auditable. It does not silently pretend the note was required.
func TestApproveWithoutANoteIsAcceptedButSaysSo(t *testing.T) {
	srv, itemID := parkedBoard(t, "delete /var/cache/app")
	t.Setenv("NSM_ADMIN_TOKEN", "admin-secret")

	var out, errOut bytes.Buffer
	if code := run([]string{"approve", itemID, "--board", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %s)", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "harder to audit") {
		t.Fatalf("stderr = %q, want the no-note warning", errOut.String())
	}
	if !strings.Contains(out.String(), "-> queued") {
		t.Fatalf("stdout = %q, want the item reported as queued", out.String())
	}
}

// TestDenyRequiresTheReason: a denial's note IS the reason recorded on the item
// (D7), so the CLI refuses a denial that would leave "declined by admin" with no
// account of why.
func TestDenyRequiresTheReason(t *testing.T) {
	srv, itemID := parkedBoard(t, "delete /var/cache/app")
	t.Setenv("NSM_ADMIN_TOKEN", "admin-secret")

	var out, errOut bytes.Buffer
	if code := run([]string{"deny", itemID, "--board", srv.URL}, &out, &errOut); code != 2 {
		t.Fatalf("deny without a note: exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "--note is required") {
		t.Fatalf("stderr = %q, want the note requirement", errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"deny", itemID, "--note", "that cache is not regenerable", "--board", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("deny with a note: exit = %d, want 0 (stderr %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "-> cancelled") {
		t.Fatalf("stdout = %q, want the item reported as cancelled", out.String())
	}
}

// TestAdminTokenIsNeverAFlag: the credential comes from the environment. A --token
// flag does not exist, so passing one fails the parse instead of putting a secret
// in ps and the shell history.
func TestAdminTokenIsNeverAFlag(t *testing.T) {
	srv, itemID := parkedBoard(t, "delete /var/cache/app")
	t.Setenv("NSM_ADMIN_TOKEN", "admin-secret")

	var out, errOut bytes.Buffer
	if code := run([]string{"approve", itemID, "--token", "admin-secret", "--board", srv.URL}, &out, &errOut); code != 2 {
		t.Fatalf("--token: exit = %d, want 2 (a token flag must not exist)", code)
	}

	// And with the environment unset, every admin command refuses to run.
	t.Setenv("NSM_ADMIN_TOKEN", "")
	out.Reset()
	errOut.Reset()
	if code := run([]string{"approvals", "--board", srv.URL}, &out, &errOut); code != 2 {
		t.Fatalf("no NSM_ADMIN_TOKEN: exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "NSM_ADMIN_TOKEN is required") {
		t.Fatalf("stderr = %q, want the missing-credential message", errOut.String())
	}
}

// TestApproveReportsTheBoardsRefusal: a 409 from a second decision must be an
// error with the board's own message and a non-zero exit, never a printed
// success — the operator's next move (go look at who decided first) depends on
// knowing the decision did not land.
func TestApproveReportsTheBoardsRefusal(t *testing.T) {
	srv, itemID := parkedBoard(t, "delete /var/cache/app")
	t.Setenv("NSM_ADMIN_TOKEN", "admin-secret")

	var out, errOut bytes.Buffer
	if code := run([]string{"approve", itemID, "--board", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("first approve: exit = %d, want 0 (stderr %s)", code, errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"deny", itemID, "--note", "changed my mind", "--board", srv.URL}, &out, &errOut); code != 1 {
		t.Fatalf("second decision: exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "409") && !strings.Contains(errOut.String(), "not waiting") {
		t.Fatalf("stderr = %q, want the board's refusal", errOut.String())
	}
	if out.String() != "" {
		t.Fatalf("stdout = %q, want nothing: the decision did not land", out.String())
	}
}

// watchBoard is a real board behind an HTTP server, plus the DB so a test can age a
// row the way a stopped manager leaves it. The CLI is exercised through run(), so the
// flag parsing, the request and the exit code are all the ones an operator gets.
func watchBoard(t *testing.T) (*httptest.Server, *sql.DB, *board.FixedClock) {
	t.Helper()
	db, clock := board.OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	srv := httptest.NewServer(board.NewServer(db, clock, provider.NewRegistry(),
		board.HashToken("admin-secret"), board.HashToken("enroll-secret")).Handler())
	t.Cleanup(srv.Close)
	t.Setenv("NSM_ADMIN_TOKEN", "admin-secret")
	return srv, db, clock
}

// TestWatchAddRequiresATriggerAndATemplate: every refusal names what is missing and
// leaves the board empty. The board validates; the CLI does not keep a second copy of
// the rules, so an unknown capability is refused by the same code that would refuse it
// at 03:00.
func TestWatchAddRequiresATriggerAndATemplate(t *testing.T) {
	srv, db, _ := watchBoard(t)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no trigger", []string{"watch", "add", "--name", "x", "--title", "t", "--brief", "b"}, "--every, --cron, or --probe"},
		{"both triggers", []string{"watch", "add", "--name", "x", "--title", "t", "--brief", "b", "--every", "1h", "--cron", "0 * * * *"}, "alternatives"},
		{"no title", []string{"watch", "add", "--name", "x", "--every", "1h", "--brief", "b"}, "--title is required"},
		{"no brief", []string{"watch", "add", "--name", "x", "--every", "1h", "--title", "t"}, "--brief is required"},
		{"unknown capability", []string{"watch", "add", "--name", "x", "--every", "1h", "--title", "t", "--brief", "b", "--needs", "teleport"}, "teleport"},
		{"bad schedule", []string{"watch", "add", "--name", "x", "--every", "soon", "--title", "t", "--brief", "b"}, "schedule"},
	}
	for _, c := range cases {
		var out, errOut bytes.Buffer
		if code := run(append(c.args, "--board", srv.URL), &out, &errOut); code != 2 {
			t.Fatalf("%s: exit = %d, want 2 (stderr %s)", c.name, code, errOut.String())
		}
		if !strings.Contains(errOut.String(), c.want) {
			t.Fatalf("%s: stderr = %q, want it to contain %q", c.name, errOut.String(), c.want)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM watches`).Scan(&count); err != nil {
		t.Fatalf("count watches: %v", err)
	}
	if count != 0 {
		t.Fatalf("watches = %d after five refused adds, want none", count)
	}
}

// TestWatchAddCreatesAWatchAndNamesItsSlot: the operator sees when it will first fire
// without a second command, and the row exists with the fields they typed.
func TestWatchAddCreatesAWatchAndNamesItsSlot(t *testing.T) {
	srv, db, clock := watchBoard(t)

	var out, errOut bytes.Buffer
	code := run([]string{"watch", "add", "--board", srv.URL,
		"--name", "nightly", "--cron", "30 2 * * *",
		"--title", "Sweep the cache", "--brief", "delete what is stale",
		"--needs", "agentic, gpu", "--probe", "test -f /tmp/flag", "--dedupe", "6h",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "first slot") {
		t.Fatalf("stdout = %q, want the first slot named", out.String())
	}

	var w board.Watch
	var needsJSON string
	if err := db.QueryRow(`SELECT name,trigger,schedule,probe,title,needs,dedupe_window FROM watches,
	        (SELECT json_extract(template,'$.title') AS title, json_extract(template,'$.needs') AS needs FROM watches) x
	     WHERE name='nightly'`).Scan(&w.Name, &w.Trigger, &w.Schedule, &w.Probe, &w.Template.Title, &needsJSON, &w.DedupeWindow); err != nil {
		t.Fatalf("read watch: %v", err)
	}
	if w.Name != "nightly" || w.Trigger != board.TriggerCron || w.Schedule != "30 2 * * *" {
		t.Fatalf("watch = %+v, want the cron watch that was typed", w)
	}
	if w.Probe != "test -f /tmp/flag" || w.DedupeWindow != 6*3600 {
		t.Fatalf("probe %q dedupe %d, want them as typed", w.Probe, w.DedupeWindow)
	}
	if !strings.Contains(needsJSON, "agentic") || !strings.Contains(needsJSON, "gpu") {
		t.Fatalf("needs = %s, want both capabilities", needsJSON)
	}
	if clock.Now() == 0 {
		t.Fatal("the fixture clock was not the board's")
	}
}

// TestWatchAddConditionNeedsOnlyAProbe: a condition watch has no schedule, so a probe
// alone is the whole trigger. Without this the CLI could not create the one kind of
// watch that fires on a change.
func TestWatchAddConditionNeedsOnlyAProbe(t *testing.T) {
	srv, db, clock := watchBoard(t)
	var out, errOut bytes.Buffer
	code := run([]string{"watch", "add", "--board", srv.URL, "--name", "disk-edge",
		"--probe", "test -f /tmp/flag", "--title", "t", "--brief", "b"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %s)", code, errOut.String())
	}
	var trigger, schedule string
	var next int64
	if err := db.QueryRow(`SELECT trigger, COALESCE(schedule,''), next_fire_at FROM watches WHERE name='disk-edge'`).
		Scan(&trigger, &schedule, &next); err != nil {
		t.Fatalf("read watch: %v", err)
	}
	if trigger != board.TriggerCondition || schedule != "" {
		t.Fatalf("trigger %q schedule %q, want a condition watch with no schedule", trigger, schedule)
	}
	if next > clock.Now() {
		t.Fatalf("next_fire_at = %d, want the condition evaluated on the next pass (<= %d)", next, clock.Now())
	}
}

// TestWatchListPrintsTheState: the terminal repeats the board's sentence, so a held
// watch reads as held with the expiry and an overdue one reads as overdue. A command
// that composed its own wording would drift from the API the first time the sentence
// changed.
func TestWatchListPrintsTheState(t *testing.T) {
	srv, db, clock := watchBoard(t)

	for _, name := range []string{"held", "overdue"} {
		var out, errOut bytes.Buffer
		if code := run([]string{"watch", "add", "--board", srv.URL, "--name", name,
			"--every", "1h", "--title", "t", "--brief", "b"}, &out, &errOut); code != 0 {
			t.Fatalf("add %s: exit = %d (stderr %s)", name, code, errOut.String())
		}
	}
	// Hold one for six hours; age the other's slot ten minutes into the past, which is
	// what a stopped manager leaves behind.
	var out, errOut bytes.Buffer
	if code := run([]string{"watch", "hold", "held", "--for", "6h", "--board", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("hold: exit = %d (stderr %s)", code, errOut.String())
	}
	if _, err := db.Exec(`UPDATE watches SET next_fire_at=? WHERE name='overdue'`, clock.Now()-600); err != nil {
		t.Fatalf("age the watch: %v", err)
	}

	out.Reset()
	if code := run([]string{"watch", "list", "--board", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("list: exit = %d (stderr %s)", code, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "held by the operator until") {
		t.Fatalf("stdout = %q, want the hold sentence", got)
	}
	if !strings.Contains(got, "overdue") || !strings.Contains(got, "scheduler is not running") {
		t.Fatalf("stdout = %q, want the overdue sentence", got)
	}
}

// TestWatchHoldSetsAndClears: --for is sent as an absolute timestamp (the CLI's clock,
// not the board's), the row carries it, and --clear removes it. Every malformed form
// exits 2 and changes nothing.
func TestWatchHoldSetsAndClears(t *testing.T) {
	srv, db, _ := watchBoard(t)
	var out, errOut bytes.Buffer
	if code := run([]string{"watch", "add", "--board", srv.URL, "--name", "nightly",
		"--every", "1h", "--title", "t", "--brief", "b"}, &out, &errOut); code != 0 {
		t.Fatalf("add: exit = %d (stderr %s)", code, errOut.String())
	}

	var id string
	if err := db.QueryRow(`SELECT id FROM watches WHERE name='nightly'`).Scan(&id); err != nil {
		t.Fatalf("read id: %v", err)
	}
	before := time.Now().Add(6 * time.Hour).Unix()
	out.Reset()
	if code := run([]string{"watch", "hold", "nightly", "--for", "6h", "--board", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("hold: exit = %d (stderr %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "held until") {
		t.Fatalf("stdout = %q, want the expiry named", out.String())
	}
	w, err := board.GetWatch(context.Background(), db, id)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	if w.CooldownUntil == nil || *w.CooldownUntil < before || *w.CooldownUntil > before+5 {
		t.Fatalf("cooldown_until = %v, want about six hours from the CLI's now (%d)", w.CooldownUntil, before)
	}

	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"neither", []string{"watch", "hold", "nightly"}, "--for <duration> or --clear"},
		{"both", []string{"watch", "hold", "nightly", "--for", "1h", "--clear"}, "alternatives"},
		{"negative", []string{"watch", "hold", "nightly", "--for", "-1h"}, "--for <duration> or --clear"},
		{"unknown name", []string{"watch", "hold", "nope", "--for", "1h"}, "no watch named"},
		{"no name", []string{"watch", "hold", "--for", "1h"}, "usage"},
	} {
		var o, e bytes.Buffer
		if code := run(append(c.args, "--board", srv.URL), &o, &e); code != 2 {
			t.Fatalf("%s: exit = %d, want 2 (stderr %s)", c.name, code, e.String())
		}
		if !strings.Contains(e.String(), c.want) {
			t.Fatalf("%s: stderr = %q, want %q", c.name, e.String(), c.want)
		}
	}
	if w, err := board.GetWatch(context.Background(), db, id); err != nil || w.CooldownUntil == nil {
		t.Fatalf("a refused hold changed the row: %+v, err %v", w.CooldownUntil, err)
	}

	out.Reset()
	if code := run([]string{"watch", "hold", "nightly", "--clear", "--board", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("clear: exit = %d (stderr %s)", code, errOut.String())
	}
	if w, err := board.GetWatch(context.Background(), db, id); err != nil || w.CooldownUntil != nil {
		t.Fatalf("cooldown_until = %v after --clear, want nil", w.CooldownUntil)
	}
}

// TestTheAdminTokenIsNeverAFlag: the credential comes from the environment and only
// from there. If --token parsed, someone would put it in their shell history and in
// ps, which is the rule M3 established.
func TestTheAdminTokenIsNeverAFlag(t *testing.T) {
	srv, _, _ := watchBoard(t)
	var out, errOut bytes.Buffer
	code := run([]string{"watch", "list", "--token", "admin-secret", "--board", srv.URL}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr %s)", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "flag provided but not defined: -token") {
		t.Fatalf("stderr = %q, want the flag refused by name", errOut.String())
	}
}

// TestWatchCommandsExitOneWhenTheBoardFails: a 4xx is the operator's mistake and exits
// 2; an unreachable board is not, and exits 1. The two are different things to a
// script, and a wrapper that retries on one and re-prompts on the other needs them
// apart.
func TestWatchCommandsExitOneWhenTheBoardFails(t *testing.T) {
	t.Setenv("NSM_ADMIN_TOKEN", "admin-secret")
	unreachable := "http://127.0.0.1:1"
	for _, args := range [][]string{
		{"watch", "list", "--board", unreachable},
		{"watch", "hold", "nightly", "--for", "1h", "--board", unreachable},
		{"watch", "add", "--name", "x", "--every", "1h", "--title", "t", "--brief", "b", "--board", unreachable},
	} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 1 {
			t.Fatalf("%v: exit = %d, want 1 (stderr %s)", args[:2], code, errOut.String())
		}
	}
}

// TestAgentRunRefusesABadPolicyBeforeClaiming: a node that would run behind a wall it misread is
// worse than a node that refuses to start, and the operator is still watching at start time.
//
// It drives parseAgentConfig rather than run(): run() would, if the refusal regressed, start a
// polling agent against a board that is not there and never return — a regression that hangs a
// test is nearly as bad as one that passes it.
func TestAgentRunRefusesABadPolicyBeforeClaiming(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(bad, []byte(`{"shell":"forbid"}`), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	t.Setenv("NSM_NODE_TOKEN", "token")
	var errOut bytes.Buffer
	cfg, ok := parseAgentConfig([]string{"--node", "n", "--provides", "agentic", "--reach", "host",
		"--sandbox-root", dir, "--policy", bad}, &errOut)
	if ok {
		t.Fatal("a policy with an unknown verdict was accepted")
	}
	if !strings.Contains(errOut.String(), "allow, deny or ask") {
		t.Fatalf("stderr = %q, want the verdict vocabulary", errOut.String())
	}
	_ = cfg

	// A good policy loads, and the node says what wall it is behind.
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"default":"allow","shell":"deny"}`), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	errOut.Reset()
	if _, ok := parseAgentConfig([]string{"--node", "n", "--provides", "agentic", "--reach", "host",
		"--sandbox-root", dir, "--policy", good}, &errOut); !ok {
		t.Fatalf("a valid policy was refused: %s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "shell=deny") {
		t.Fatalf("stderr = %q, want the loaded policy summarised", errOut.String())
	}
}

// TestAgentRunChoosesTheExecutor: the steward runs the reflex path; a typo in --executor must be
// refused at start, not silently turned into the full tool loop on a machine that cannot carry it.
func TestAgentRunChoosesTheExecutor(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NSM_NODE_TOKEN", "token")
	base := []string{"--node", "n", "--provides", "reflex", "--reach", "host", "--sandbox-root", dir}

	// The token is unset by a successful parse (it is read once and removed), so each call sets it
	// again rather than relying on the first.
	t.Setenv("NSM_NODE_TOKEN", "token")
	var errOut bytes.Buffer
	cfg, ok := parseAgentConfig(append(base, "--executor", "reflex"), &errOut)
	if !ok {
		t.Fatalf("--executor reflex refused: %s", errOut.String())
	}
	if _, isReflex := cfg.Executor.(*agent.ReflexExecutor); !isReflex {
		t.Fatalf("executor = %T, want *agent.ReflexExecutor", cfg.Executor)
	}

	errOut.Reset()
	t.Setenv("NSM_NODE_TOKEN", "token")
	if _, ok := parseAgentConfig(append(base, "--executor", "agntic"), &errOut); ok {
		t.Fatal("a misspelled executor was accepted")
	}
	if !strings.Contains(errOut.String(), "neither agentic nor reflex") {
		t.Fatalf("stderr = %q, want the vocabulary", errOut.String())
	}

	// The default is the loop, so existing invocations keep meaning what they meant.
	errOut.Reset()
	t.Setenv("NSM_NODE_TOKEN", "token")
	if cfg, ok := parseAgentConfig(base, &errOut); !ok {
		t.Fatalf("the default was refused: %s", errOut.String())
	} else if _, isAgentic := cfg.Executor.(*agent.AgenticExecutor); !isAgentic {
		t.Fatalf("default executor = %T, want *agent.AgenticExecutor", cfg.Executor)
	}
}
