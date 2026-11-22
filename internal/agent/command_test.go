package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

// commandItem is a claim payload good enough for the executor: the brief is the only prose it reads.
func commandItem(runID, title, brief string) board.ClaimedItem {
	return board.ClaimedItem{
		RunID: runID,
		Brief: brief,
		Item:  board.WorkItem{ID: "item-1", Title: title, Attempt: 1},
	}
}

// TestTheBriefIsInTheHarnessWorkspace: the harness reads a file, not an argument — a brief on a
// command line lands in `ps` where anyone on the machine can read it.
func TestTheBriefIsInTheHarnessWorkspace(t *testing.T) {
	root := t.TempDir()
	e := &CommandExecutor{
		RunCmd: `test -f "$NSM_BRIEF_FILE" && grep -q 'split the migration' "$NSM_BRIEF_FILE" && echo "read the brief"`,
		Root:   root,
	}
	res, err := e.Run(context.Background(), commandItem("run-1", "the title", "split the migration"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Summary, "read the brief") {
		t.Fatalf("summary = %q", res.Summary)
	}
	// The workspace is gone afterwards unless artifacts are kept: a harness leaves caches, clones and
	// half-written files, and a run per hour would fill the disk silently.
	if _, err := os.Stat(filepath.Join(root, "run-run-1")); !os.IsNotExist(err) {
		t.Fatalf("the workspace survived a successful run: %v", err)
	}
}

// TestTheHarnessSeesItsIdentityAndNothingElse: six variables, no token, no endpoint keys. A harness
// with the node token could claim and report other work; the board's protocol is not its business.
func TestTheHarnessSeesItsIdentityAndNothingElse(t *testing.T) {
	// A credential-shaped variable in this process's environment, which must not reach the child.
	t.Setenv("NSM_NODE_TOKEN", "node-secret")
	t.Setenv("OPENAI_API_KEY", "sk-do-not-leak")
	root := t.TempDir()
	e := &CommandExecutor{
		RunCmd: `env | sort | sed -n 's/^NSM_[A-Z_]*=/&/p'; echo "---"; echo "token=${NSM_NODE_TOKEN:-none}"; echo "key=${OPENAI_API_KEY:-none}"`,
		Root:   root,
	}
	res, err := e.Run(context.Background(), commandItem("run-2", "t", "b"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{"NSM_ITEM_ID=item-1", "NSM_RUN_ID=run-2", "NSM_TITLE=t", "NSM_ATTEMPT=1"} {
		if !strings.Contains(res.Summary, want) {
			t.Fatalf("the harness did not see %q:\n%s", want, res.Summary)
		}
	}
	if strings.Contains(res.Summary, "node-secret") || strings.Contains(res.Summary, "sk-do-not-leak") {
		t.Fatalf("a credential reached the harness:\n%s", res.Summary)
	}
	if !strings.Contains(res.Summary, "token=none") || !strings.Contains(res.Summary, "key=none") {
		t.Fatalf("the harness inherited this process's environment:\n%s", res.Summary)
	}
}

// TestANonZeroExitIsAFailedRun: the board must see a failure, not a summary that happens to mention one.
func TestANonZeroExitIsAFailedRun(t *testing.T) {
	e := &CommandExecutor{RunCmd: `echo "it broke" >&2; exit 3`, Root: t.TempDir()}
	res, err := e.Run(context.Background(), commandItem("run-3", "t", "b"))
	if err == nil {
		t.Fatal("a non-zero exit was reported as success")
	}
	if !strings.Contains(err.Error(), "exited 3") {
		t.Fatalf("err = %v, want the exit code", err)
	}
	if !strings.Contains(res.Summary, "it broke") {
		t.Fatalf("summary does not carry the output: %q", res.Summary)
	}
}

// TestExitSeventyFiveAsksForAHuman: the one thing a credential-less harness can do is stop and say
// so, and 75 is the convention that means "this could work later".
func TestExitSeventyFiveAsksForAHuman(t *testing.T) {
	e := &CommandExecutor{
		RunCmd: `echo "NSM-ESCALATE: deploy to staging | the migration needs a maintenance window"; exit 75`,
		Root:   t.TempDir(),
	}
	res, err := e.Run(context.Background(), commandItem("run-4", "t", "b"))
	if err != nil {
		t.Fatalf("an escalation is not an error: %v", err)
	}
	if res.Approval == nil {
		t.Fatal("exit 75 produced no approval request")
	}
	if res.Approval.Action != "deploy to staging" {
		t.Fatalf("action = %q", res.Approval.Action)
	}
	if !strings.Contains(res.Approval.Reason, "maintenance window") {
		t.Fatalf("reason = %q", res.Approval.Reason)
	}
}

// TestAnUnnamedEscalationStillAsks: a harness that exits 75 without the marker still gets a human,
// with the output as the reason. Guessing its intent would be worse than asking someone to read.
func TestAnUnnamedEscalationStillAsks(t *testing.T) {
	e := &CommandExecutor{RunCmd: `echo "I am not sure about the schema"; exit 75`, Root: t.TempDir()}
	res, err := e.Run(context.Background(), commandItem("run-5", "t", "b"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Approval == nil {
		t.Fatal("exit 75 without a marker produced no approval request")
	}
	if !strings.Contains(res.Approval.Reason, "not sure about the schema") {
		t.Fatalf("the human is not shown what the harness said: %q", res.Approval.Reason)
	}
}

// TestTheCommandOutputIsBounded: a harness that prints megabytes must not become a megabyte summary.
func TestTheCommandOutputIsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	e := &CommandExecutor{
		RunCmd: `i=0; while [ $i -lt 2000 ]; do echo "line $i of noise that goes on and on"; i=$((i+1)); done`,
		Root:   t.TempDir(),
	}
	res, err := e.Run(context.Background(), commandItem("run-6", "t", "b"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Summary) > commandTailBytes {
		t.Fatalf("summary is %d bytes, want at most %d", len(res.Summary), commandTailBytes)
	}
	if !strings.Contains(res.Summary, "line 1999") {
		t.Fatal("the summary is not the END of the output, which is where the failure is")
	}
}

// TestAHarnessThatHangsIsStopped: the reaper would take the item eventually, but with a lease-expired
// event that says nothing about the harness. The executor's own timeout says what happened.
func TestAHarnessThatHangsIsStopped(t *testing.T) {
	e := &CommandExecutor{RunCmd: `sleep 30`, Root: t.TempDir(), Timeout: 300 * time.Millisecond}
	start := time.Now()
	_, err := e.Run(context.Background(), commandItem("run-7", "t", "b"))
	if err == nil {
		t.Fatal("a hanging harness reported success")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the timeout took %s", elapsed)
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("err = %v, want it to name the timeout", err)
	}
}

// TestEachRunGetsItsOwnWorkspace: two runs of the same item (an attempt that was retried) must not
// share a directory, or one harness reads the other's leftovers.
func TestEachRunGetsItsOwnWorkspace(t *testing.T) {
	root := t.TempDir()
	e := &CommandExecutor{RunCmd: `echo "$NSM_RUN_ID" > seen.txt; pwd`, Root: root, KeepArtifacts: true}
	if _, err := e.Run(context.Background(), commandItem("run-a", "t", "b")); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := e.Run(context.Background(), commandItem("run-b", "t", "b")); err != nil {
		t.Fatalf("second: %v", err)
	}
	for _, run := range []string{"run-a", "run-b"} {
		p := filepath.Join(root, "run-"+run, "seen.txt")
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s: %v", run, err)
		}
		if strings.TrimSpace(string(body)) != run {
			t.Fatalf("%s saw %q", run, body)
		}
	}
}

// TestTheTailBufferIsBounded: the summary is truncated at the end anyway, so only a test of the
// buffer itself can catch an unbounded one — and the property that matters is memory: a harness that
// prints gigabytes must not become gigabytes resident in the agent that is supposed to be watching it.
func TestTheTailBufferIsBounded(t *testing.T) {
	tb := &tailBuffer{max: 1024}
	chunk := make([]byte, 512)
	for i := 0; i < 64; i++ {
		if _, err := tb.Write(chunk); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if got := len(tb.buf); got > 1024 {
		t.Fatalf("the buffer holds %d bytes with a 1024-byte cap", got)
	}
	if got := len(tb.String()); got != 1024 {
		t.Fatalf("the buffer holds %d bytes after 32 KiB written, want exactly the cap", got)
	}
}

// TestTheBriefIsNotOnTheCommandLine: the brief is a file because a command line is world-readable on
// most systems (`ps`). This asserts the property directly, by making the harness print its own argv.
func TestTheBriefIsNotOnTheCommandLine(t *testing.T) {
	e := &CommandExecutor{
		RunCmd: `ps -p $$ -o args= 2>/dev/null || cat /proc/self/cmdline 2>/dev/null || echo "no ps"`,
		Root:   t.TempDir(),
	}
	res, err := e.Run(context.Background(), commandItem("run-8", "t", "SECRET-BRIEF-MARKER should never appear in argv"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(res.Summary, "SECRET-BRIEF-MARKER") {
		t.Fatalf("the brief reached the command line:\n%s", res.Summary)
	}
	if !strings.Contains(res.Summary, "sh") {
		t.Fatalf("the harness did not manage to print its own command line, so this test proved nothing:\n%s", res.Summary)
	}
}

// TestAHarnessNodeDoesNotAdvertiseASandbox is M8's Rule 1 where it actually bites: the heartbeat used
// to append sandbox:<profile> for every node, so a harness node advertised confinement it cannot
// deliver and the board (correctly) refused the heartbeat as a contradiction.
//
// The assertions are written against the probe's answer, not against "seatbelt": on Linux the probe
// offers nothing, and a test that hard-coded a profile would pass on macOS and fail where the board
// is meant to run.
func TestAHarnessNodeDoesNotAdvertiseASandbox(t *testing.T) {
	ctx := context.Background()
	probe := SandboxCapability()

	harness := &Agent{cfg: Config{
		Provides: []string{"harness:scripted"},
		Executor: &CommandExecutor{RunCmd: "true", Root: t.TempDir()},
	}}
	got := harness.liveCapabilities(ctx)
	if len(got) != 1 || got[0] != "harness:scripted" {
		t.Fatalf("a harness node advertises %v, want exactly its harness capability (probe offers %v)", got, probe)
	}

	// The built-in executor still appends whatever the probe says — on a platform where confinement
	// exists, that is the sandbox capability; where it does not, nothing. Either way the append
	// happens, which is what the harness executor suppresses.
	builtin := &Agent{cfg: Config{Provides: []string{"agentic"}, Executor: &AgenticExecutor{}}}
	if got := builtin.liveCapabilities(ctx); len(got) != 1+len(probe) {
		t.Fatalf("a built-in node advertises %v, want agentic plus %v", got, probe)
	}
}
