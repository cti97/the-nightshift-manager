package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

// CommandExecutor runs somebody else's agent harness (M8): Claude Code, Codex CLI, a shell script.
//
// The board's whole value is the row, the lease, the price and the audit, so a harness that claims
// work becomes a node like any other — the difference is only who runs the loop. Everything around
// the command is this package's: the heartbeat keeps the lease alive, the reaper takes the item back
// if the process dies, the report settles it.
//
// Two rules make the difference honest:
//
//  1. A harness is NOT a sandbox. This executor makes a directory and runs a program in it; it cannot
//     promise confinement, so a node using it must declare harness:<name> and never sandbox:<profile>
//     (caps.ConfinementConflict enforces that at the board).
//  2. The harness gets no credential. It cannot report, claim or file: the node token stays in this
//     process. A harness that needs a human says so through its exit code (see escalateExitCode),
//     which is a signal, not a credential.
type CommandExecutor struct {
	// RunCmd is the command line, run with `/bin/sh -c` inside the run's directory. It is the
	// operator's string and is never composed from item data.
	RunCmd string
	// Root is where each run gets its disposable directory.
	Root string
	// KeepArtifacts leaves the directory behind for inspection; the default removes it.
	KeepArtifacts bool
	// Timeout bounds one harness run. Zero selects defaultCommandTimeout; a harness that hangs must
	// not hold the item forever (the reaper would take it, but slowly and with a scary event).
	Timeout time.Duration
	// now and lookPath are test seams.
	now func() time.Time
}

const (
	// defaultCommandTimeout is generous because a harness is a whole agent session, and a run that
	// needs an hour should finish rather than be killed at fifteen minutes.
	defaultCommandTimeout = 2 * time.Hour
	// commandTailBytes is how much of the harness's output becomes the run's summary. It matches the
	// agent's own summary bound: an item row that a page renders should not be 64 KiB, and a harness
	// that prints more than this is printing a log, not a result.
	commandTailBytes = maxSummaryBytes
	// escalateExitCode is how a harness asks for a human without a credential: exit 75, the BSD
	// EX_TEMPFAIL convention, meaning "this could work later".
	escalateExitCode = 75
)

// commandEnv is what the harness sees. No token, no endpoint keys: a harness brings its own model
// credentials, and this process's are none of its business.
func (e *CommandExecutor) commandEnv(item board.ClaimedItem, workdir, briefPath string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		// HOME is the REAL home, not the workspace. A harness like `claude` keeps its login there; a
		// harness that cannot find its credentials fails at the first call, and the operator's first
		// experience of this feature would be an authentication error they did not cause. The
		// workspace is cwd and is named in NSM_WORKDIR, which is the directory the harness should
		// write to; what it does in its own home is the same thing it does when run by hand.
		"HOME=" + os.Getenv("HOME"),
		"NSM_ITEM_ID=" + item.Item.ID,
		"NSM_RUN_ID=" + item.RunID,
		"NSM_TITLE=" + item.Item.Title,
		"NSM_WORKDIR=" + workdir,
		"NSM_BRIEF_FILE=" + briefPath,
		"NSM_ATTEMPT=" + fmt.Sprint(item.Item.Attempt),
	}
	if lang := os.Getenv("LANG"); lang != "" {
		env = append(env, "LANG="+lang)
	}
	return env
}

// Confines reports false, and the heartbeat honours it: this executor starts somebody else's program
// in a directory and cannot promise the confinement the built-in sandbox provides. A node that
// advertised sandbox:* while running this would accept work that needs confinement and hand it over
// unconfined (M8 Rule 1).
func (e *CommandExecutor) Confines() bool { return false }

// Run materializes the brief, runs the command, and maps the outcome onto the board's vocabulary.
func (e *CommandExecutor) Run(ctx context.Context, item board.ClaimedItem) (Result, error) {
	if strings.TrimSpace(e.RunCmd) == "" {
		return Result{}, errors.New("agent: CommandExecutor has no RunCmd")
	}
	if e.Root == "" {
		return Result{}, errors.New("agent: CommandExecutor has no Root")
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}

	workdir := filepath.Join(e.Root, "run-"+item.RunID)
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		return Result{}, fmt.Errorf("agent: create the run's workspace: %w", err)
	}
	keep := e.KeepArtifacts
	defer func() {
		if !keep {
			_ = os.RemoveAll(workdir)
		}
	}()

	briefPath := filepath.Join(workdir, "BRIEF.md")
	// The brief is a file, not an argument: a brief may be megabytes (maxBriefBytes is 64 KiB) and
	// anything on a command line lands in the process table where `ps` can read it.
	brief := "# " + item.Item.Title + "\n\n" + item.Brief + "\n"
	if err := os.WriteFile(briefPath, []byte(brief), 0o600); err != nil {
		return Result{}, fmt.Errorf("agent: write the brief: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "/bin/sh", "-c", e.RunCmd)
	cmd.Dir = workdir
	cmd.Env = e.commandEnv(item, workdir, briefPath)
	// A harness that leaves a child holding the pipe would otherwise hang Close forever; the same
	// two-line lesson the watch probe learned.
	cmd.WaitDelay = 2 * time.Second
	tail := &tailBuffer{max: commandTailBytes}
	var mu sync.Mutex
	writer := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return tail.Write(p)
	})
	cmd.Stdout = writer
	cmd.Stderr = writer

	err := cmd.Run()
	out := tail.String()

	if runCtx.Err() == context.DeadlineExceeded {
		// The item stays out of "done": a harness killed at the limit did not finish the work.
		return Result{Summary: truncateSummary("the harness ran longer than " + timeout.String() + "\n\n" + out)},
			fmt.Errorf("agent: the harness exceeded %s and was stopped", timeout)
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		if code == escalateExitCode {
			return Result{
				Summary:  truncateSummary(out),
				Approval: escalationFromOutput(out, e.RunCmd),
			}, nil
		}
		return Result{Summary: truncateSummary(fmt.Sprintf("the harness exited %d\n\n%s", code, out))},
			fmt.Errorf("agent: the harness exited %d", code)
	}
	if err != nil {
		return Result{}, fmt.Errorf("agent: start the harness: %w", err)
	}
	in, outTokens, source := readUsage(workdir)
	return Result{Summary: truncateSummary(out), TokensIn: in, TokensOut: outTokens, TokenSource: source}, nil
}

// escalationFromOutput builds the approval request. A harness may name its own action on a line
// starting with "NSM-ESCALATE:"; otherwise the action is deliberately vague and the reason is the
// output, because guessing what the harness wanted is worse than asking the human to read.
func escalationFromOutput(out, runCmd string) *board.ApprovalRequest {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "NSM-ESCALATE:") {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(line, "NSM-ESCALATE:"))
		action, reason, _ := strings.Cut(body, "|")
		action, reason = strings.TrimSpace(action), strings.TrimSpace(reason)
		if action != "" {
			if reason == "" {
				reason = "the harness asked for a human"
			}
			return &board.ApprovalRequest{Action: action, Reason: reason,
				Options: []string{"allow it for this run", "deny"}}
		}
	}
	return &board.ApprovalRequest{
		Action:  "run the harness again with a human present",
		Reason:  "the harness stopped and asked for a human; it did not say why in a form the board can read:\n" + tailOf(out, 2000),
		Options: []string{"allow it for this run", "deny"},
	}
}

// usageFile is where a harness can say what it spent. It is a file rather than an argument or a
// protocol: the harness already writes into this directory, and nothing about the board needs to be
// understood to write two numbers.
const usageFile = "NSM_USAGE.json"

// readUsage reads the harness's own accounting. No file, or an unreadable one, means "unknown" — not
// zero. A day of harness runs that reported nothing must not look like a day of free work (M8 Rule 2).
func readUsage(workdir string) (in, out int, source string) {
	raw, err := os.ReadFile(filepath.Join(workdir, usageFile))
	if err != nil {
		return 0, 0, "unknown"
	}
	var u struct {
		TokensIn  int `json:"tokens_in"`
		TokensOut int `json:"tokens_out"`
	}
	if err := json.Unmarshal(raw, &u); err != nil || u.TokensIn < 0 || u.TokensOut < 0 {
		// A malformed file is not a zero: the harness tried to tell us and we could not read it.
		return 0, 0, "unknown"
	}
	return u.TokensIn, u.TokensOut, "reported"
}

// tailBuffer keeps the last max bytes written to it. A harness can print megabytes; the summary is
// the end, where the failure is.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// truncateSummary keeps a summary within what the board accepts.
func truncateSummary(s string) string {
	s = strings.TrimRight(s, "\n")
	if len(s) > commandTailBytes {
		return s[len(s)-commandTailBytes:]
	}
	if s == "" {
		return "(the harness printed nothing)"
	}
	return s
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
