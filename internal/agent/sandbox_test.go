package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// newTestSandbox is the common fixture: a sandbox under a temporary root that
// is always disposed of, even when the test fails part-way through.
func newTestSandbox(t *testing.T, cfg SandboxConfig) *Sandbox {
	t.Helper()
	if cfg.Root == "" {
		cfg.Root = t.TempDir()
	}
	if cfg.RunID == "" {
		cfg.RunID = "run-1"
	}
	sb, err := NewSandbox(cfg)
	if err != nil {
		t.Fatalf("NewSandbox(%+v): %v", cfg, err)
	}
	t.Cleanup(func() {
		if err := sb.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return sb
}

// runInSandbox starts one command through the sandbox and returns its combined
// output. A non-zero exit is returned as an error, not fatal: several tests
// assert that a command was refused.
func runInSandbox(t *testing.T, sb *Sandbox, name string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd, err := sb.Command(ctx, name, args...)
	if err != nil {
		t.Fatalf("Command(%q): %v", name, err)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestSandboxCannotReachTheHomeDirectory is the acceptance test from the brief.
//
// It checks two different guarantees, and says which is which:
//
//   - The tool layer (ReadFile and friends) refuses the path on every OS. That
//     half always runs.
//   - The kernel layer stops a *shell* from reading the home directory. That
//     half needs a working sandbox-exec. When the profile cannot be applied,
//     the test skips with the reason rather than passing on a weaker check:
//     a green tick over an unenforced boundary is worse than no test.
func TestSandboxCannotReachTheHomeDirectory(t *testing.T) {
	sb := newTestSandbox(t, SandboxConfig{})

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("os.UserHomeDir failed (%v); there is no home directory to test against", err)
	}
	key := filepath.Join(home, ".ssh", "id_rsa")

	// The tool layer. Absolute paths and traversal are refused before the
	// filesystem is touched, so this is deterministic even when the key does
	// not exist.
	if data, err := sb.ReadFile(key); err == nil {
		t.Fatalf("ReadFile(%q) succeeded (%d bytes); the tool layer must refuse paths outside the run directory", key, len(data))
	}
	if err := sb.WriteFile(key, []byte("x"), 0o600); err == nil {
		t.Fatalf("WriteFile(%q) succeeded; the tool layer must refuse paths outside the run directory", key)
	}
	if _, _, err := sb.ListDir(key, 100); err == nil {
		t.Fatalf("ListDir(%q) succeeded; the tool layer must refuse paths outside the run directory", key)
	}

	// A symlink inside the run directory that points at the home directory is
	// the interesting case: the name is lexically inside, the target is not.
	link := filepath.Join(sb.Dir(), "home-link")
	if err := os.Symlink(home, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, _, err := sb.ListDir("home-link", 100); err == nil {
		t.Fatal("ListDir through a symlink to the home directory succeeded")
	}

	// The kernel layer. Its absence is reported, not papered over.
	if runtime.GOOS != "darwin" {
		t.Skipf("kernel confinement is macOS-only (this is %s); the design gives a %s node a plain directory (spec §4), so the tool layer is the only boundary and the shell is unconfined. NSM_REQUIRE_KERNEL_SANDBOX has no effect here: this platform check runs before the variable is read, and there is no kernel layer to require", runtime.GOOS, runtime.GOOS)
	}
	active, why := KernelConfinement()
	if !active {
		// Plain `go test` does not show skips, so a machine that promises the
		// kernel layer has to be able to turn this into a failure rather than
		// a green tick. The report says which machines are in which state.
		if os.Getenv("NSM_REQUIRE_KERNEL_SANDBOX") != "" {
			t.Fatalf("NSM_REQUIRE_KERNEL_SANDBOX is set, but kernel confinement is not active: %s", why)
		}
		t.Skipf("kernel confinement is not active in this process: %s. The tool layer refused the home path above; whether a shell can read the home directory is NOT proven on this machine. Set NSM_REQUIRE_KERNEL_SANDBOX=1 to make this a failure", why)
	}

	// Positive controls: the ordinary shell works and the home directory is
	// readable outside the sandbox. Without both, a refusal proves nothing.
	if entries, err := os.ReadDir(home); err != nil || len(entries) == 0 {
		t.Skipf("home directory %q is not readable outside the sandbox (%v); cannot prove the sandbox denies it", home, err)
	}
	if err := sb.WriteFile("inside.txt", []byte("inside"), 0o600); err != nil {
		t.Fatalf("WriteFile inside the run directory: %v", err)
	}
	if out, err := runInSandbox(t, sb, "/bin/cat", filepath.Join(sb.Dir(), "inside.txt")); err != nil || strings.TrimSpace(out) != "inside" {
		t.Fatalf("the sandboxed shell cannot read its own run directory: %v: %q", err, out)
	}

	// The structural guarantee from spec §10: nothing in the run directory
	// can reach the operator's home directory.
	if out, err := runInSandbox(t, sb, "/bin/ls", home); err == nil {
		t.Fatalf("the sandboxed shell listed the home directory %q; the profile did not confine it: %q", home, out)
	}

	// And a canary outside the run directory but not under home, so this does
	// not depend on the shape of $HOME.
	outside := t.TempDir()
	canary := filepath.Join(outside, "canary")
	if err := os.WriteFile(canary, []byte("canary"), 0o600); err != nil {
		t.Fatalf("canary: %v", err)
	}
	if out, err := runInSandbox(t, sb, "/bin/cat", canary); err == nil {
		t.Fatalf("the sandboxed shell read %q, outside the run directory: %q", canary, out)
	}
}

// TestSandboxPathCannotEscape is the tool-layer half of the guarantee, and it
// runs on every OS. os.Root is what makes "lexically inside but resolves
// outside" impossible rather than merely unlikely.
func TestSandboxPathCannotEscape(t *testing.T) {
	sb := newTestSandbox(t, SandboxConfig{})

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	// A symlink the sandbox itself could have created (a shell command can).
	if err := os.Symlink(outside, filepath.Join(sb.Dir(), "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// A symlink that stays inside the run directory must still work, or the
	// refusal would be "refuse all symlinks" rather than "refuse escapes".
	if err := os.WriteFile(filepath.Join(sb.Dir(), "real.txt"), []byte("real"), 0o600); err != nil {
		t.Fatalf("write real: %v", err)
	}
	if err := os.Symlink("real.txt", filepath.Join(sb.Dir(), "alias")); err != nil {
		t.Fatalf("symlink alias: %v", err)
	}
	if data, err := sb.ReadFile("alias"); err != nil || string(data) != "real" {
		t.Fatalf("ReadFile through an inside symlink = %q, %v; want %q", data, err, "real")
	}

	for _, name := range []string{
		"/etc/passwd",
		secret,
		"../../etc/passwd",
		"../escape/secret",
		"sub/../../outside",
		"escape",
		"escape/secret",
		".",
	} {
		if name == "." {
			continue // ListDir(".") is the run directory itself; handled below
		}
		if data, err := sb.ReadFile(name); err == nil {
			t.Errorf("ReadFile(%q) succeeded (%q); want a refusal", name, data)
		}
		if err := sb.WriteFile(name, []byte("pwned"), 0o600); err == nil {
			t.Errorf("WriteFile(%q) succeeded; want a refusal", name)
		}
		if _, _, err := sb.ListDir(name, 100); err == nil {
			t.Errorf("ListDir(%q) succeeded; want a refusal", name)
		}
	}
	if _, _, err := sb.ListDir(".", 100); err != nil {
		t.Fatalf("ListDir(%q): %v; the run directory itself must be listable", sb.Dir(), err)
	}
	// Nothing was written through the escape hatch.
	if _, err := os.Stat(filepath.Join(outside, "pwned")); err == nil {
		t.Fatal("a refused write created a file outside the run directory")
	}
}

func TestNewSandboxCreatesAFreshRunDirectory(t *testing.T) {
	root := t.TempDir()
	sb := newTestSandbox(t, SandboxConfig{Root: root, RunID: "abc-123"})

	// NewSandbox resolves the root (sandbox.go's EvalSymlinks) so Dir() names
	// the real directory. $TMPDIR may itself be a symlink — on macOS /var is
	// /private/var, and t.TempDir() hands back whichever spelling the
	// environment used — so resolve the expectation the same way the
	// constructor does. Comparing raw spellings is an environment-dependent
	// failure, not a defect in the sandbox.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve temporary root %q: %v", root, err)
	}
	if got, want := sb.Dir(), filepath.Join(resolvedRoot, "abc-123"); got != want {
		t.Fatalf("Dir() = %q, want %q", got, want)
	}
	fi, err := os.Stat(sb.Dir())
	if err != nil {
		t.Fatalf("stat run directory: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("%q is not a directory", sb.Dir())
	}
	if mode := fi.Mode().Perm(); mode != 0o700 {
		t.Fatalf("run directory mode = %#o, want 0700: another local user must not be able to read a run", mode)
	}
	// The temp directory is inside the run directory, so it dies with it.
	if _, err := os.Stat(filepath.Join(sb.Dir(), "tmp")); err != nil {
		t.Fatalf("tmp dir: %v", err)
	}
}

// TestNewSandboxRefusesAnExistingRunDirectory covers the crash case: a
// directory left by a dead process is never adopted, and never deleted by a
// later sandbox that did not create it.
func TestNewSandboxRefusesAnExistingRunDirectory(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, "run-1")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatalf("mkdir stale: %v", err)
	}
	marker := filepath.Join(stale, "work-in-progress")
	if err := os.WriteFile(marker, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	if sb, err := NewSandbox(SandboxConfig{Root: root, RunID: "run-1"}); err == nil {
		sb.Close()
		t.Fatal("NewSandbox adopted an existing run directory")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep me" {
		t.Fatalf("the refused sandbox touched the existing directory: %q, %v", data, err)
	}
}

func TestNewSandboxRejectsBadRunIDs(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{
		"",
		".",
		"..",
		"a/b",
		"../run",
		"a b",
		"run\n(allow network*)",
		"run-1\x00",
	} {
		if sb, err := NewSandbox(SandboxConfig{Root: root, RunID: id}); err == nil {
			sb.Close()
			t.Errorf("NewSandbox accepted run id %q", id)
		}
	}
	// The root must be absolute: a relative root means the process's working
	// directory decides where runs land, which is not a configuration.
	if sb, err := NewSandbox(SandboxConfig{Root: "relative/runs", RunID: "run-1"}); err == nil {
		sb.Close()
		t.Error("NewSandbox accepted a relative root")
	}
}

func TestSandboxScrubsTheEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the scrub check drives a Unix shell; the environment is built the same way on Windows but this test does not prove it")
	}
	t.Setenv("NSM_SANDBOX_LEAK", "leaked-value")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/ssh-agent.sock")
	t.Setenv("HOME", "/tmp/not-the-real-home")

	sb := newTestSandbox(t, SandboxConfig{})

	out, err := runInSandbox(t, sb, "/bin/sh", "-c",
		`echo "leak=[$NSM_SANDBOX_LEAK]"; echo "sock=[$SSH_AUTH_SOCK]"; env`)
	if err != nil {
		t.Fatalf("shell failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "leaked-value") || strings.Contains(out, "NSM_SANDBOX_LEAK") {
		t.Fatalf("the shell saw a variable from the parent process:\n%s", out)
	}
	if strings.Contains(out, "/tmp/ssh-agent.sock") {
		t.Fatalf("the shell saw SSH_AUTH_SOCK:\n%s", out)
	}
	if !strings.Contains(out, "HOME="+sb.Dir()) {
		t.Fatalf("the shell's HOME is not the run directory:\n%s", out)
	}
}

// TestSandboxEnvIsOnlyWhatWeSet pins the whole environment, not just the one
// secret: an inherited variable that is not a credential today is a credential
// after the next operator exports one.
func TestSandboxEnvIsOnlyWhatWeSet(t *testing.T) {
	t.Setenv("SOME_PARENT_VARIABLE", "must-not-appear")
	sb := newTestSandbox(t, SandboxConfig{})

	got := map[string]string{}
	for _, kv := range sb.Env() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("malformed environment entry %q", kv)
		}
		if _, dup := got[k]; dup {
			t.Fatalf("duplicate environment key %q", k)
		}
		got[k] = v
	}
	want := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "PWD": true, "LANG": true}
	if runtime.GOOS == "windows" {
		for _, k := range []string{"USERPROFILE", "TMP", "TEMP", "PATHEXT", "SystemRoot"} {
			want[k] = true
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("environment carries %q, which the sandbox did not set", k)
		}
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("environment is missing %q", k)
		}
	}
	if got["HOME"] != sb.Dir() || got["PWD"] != sb.Dir() {
		t.Fatalf("HOME/PWD = %q/%q, want %q", got["HOME"], got["PWD"], sb.Dir())
	}
	if !strings.HasPrefix(got["TMPDIR"], sb.Dir()+string(filepath.Separator)) {
		t.Fatalf("TMPDIR = %q, want a path inside %q", got["TMPDIR"], sb.Dir())
	}
}

func TestSandboxIsRemovedOnClose(t *testing.T) {
	sb := newTestSandbox(t, SandboxConfig{})
	dir := sb.Dir()
	if err := sb.WriteFile("artifact.txt", []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := sb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run directory still exists after Close (stat err = %v)", err)
	}
	// Close twice is not an error: the agent's shutdown path may call it and
	// the executor may also defer it.
	if err := sb.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSandboxIsKeptOnRequest(t *testing.T) {
	sb := newTestSandbox(t, SandboxConfig{KeepArtifacts: true})
	dir := sb.Dir()
	if err := sb.WriteFile("artifact.txt", []byte("kept"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := sb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	data, err := os.ReadFile(filepath.Join(dir, "artifact.txt"))
	if err != nil || string(data) != "kept" {
		t.Fatalf("kept artifact = %q, %v; want it to survive Close", data, err)
	}
	if err := sb.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestCloseRefusesToRemoveAReplacedDirectory pins "removes the directory it
// created", not "removes whatever is at the path now".
func TestCloseRefusesToRemoveAReplacedDirectory(t *testing.T) {
	root := t.TempDir()
	sb, err := NewSandbox(SandboxConfig{Root: root, RunID: "run-1"})
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	dir := sb.Dir()
	moved := dir + "-moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir replacement: %v", err)
	}
	other := filepath.Join(dir, "someone-elses-work")
	if err := os.WriteFile(other, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write other: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir); os.RemoveAll(moved) })

	if err := sb.Close(); err == nil {
		t.Fatal("Close removed a directory that was not the one it created")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("Close removed the replacement directory: %v", err)
	}
	// The refusal is the first Close's outcome, not a one-shot event: a later
	// Close must repeat it rather than return a clean nil that hides it.
	if err := sb.Close(); err == nil {
		t.Fatal("a second Close reported success after the first refused")
	}
}

func TestSandboxClosesAfterAFailedCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh; the lifecycle is the same on Windows but this test does not prove it")
	}
	sb := newTestSandbox(t, SandboxConfig{})
	if _, err := runInSandbox(t, sb, "/bin/sh", "-c", "echo partial > partial.txt; exit 7"); err == nil {
		t.Fatal("the command unexpectedly succeeded")
	}
	if _, err := os.Stat(filepath.Join(sb.Dir(), "partial.txt")); err != nil {
		t.Fatalf("the failed command's partial output is missing: %v", err)
	}
	dir := sb.Dir()
	if err := sb.Close(); err != nil {
		t.Fatalf("Close after a failed command: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run directory survived Close after a failed command (stat err = %v)", err)
	}
}

func TestSandboxReadWriteListRoundTrip(t *testing.T) {
	sb := newTestSandbox(t, SandboxConfig{})
	if err := sb.MkdirAll("a/b/c", 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := sb.WriteFile("a/b/c/file.txt", []byte("hello"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, err := sb.ReadFile("a/b/c/file.txt")
	if err != nil || string(data) != "hello" {
		t.Fatalf("ReadFile = %q, %v", data, err)
	}
	entries, _, err := sb.ListDir("a/b", 100)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{"c"}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("ListDir(a/b) = %v, want %v", names, want)
	}
	if err := sb.WriteFile("top.txt", []byte("t"), 0o600); err != nil {
		t.Fatalf("WriteFile top: %v", err)
	}
	entries, _, err = sb.ListDir(".", 100)
	if err != nil {
		t.Fatalf("ListDir(.): %v", err)
	}
	names = names[:0]
	for _, e := range entries {
		names = append(names, e.Name())
	}
	// The sandbox creates "tmp" itself; the test's own entries must be there.
	for _, want := range []string{"a", "top.txt", "tmp"} {
		if !slices.Contains(names, want) {
			t.Fatalf("ListDir(.) = %v, want it to contain %q", names, want)
		}
	}
	if _, _, err := sb.ListDir("does-not-exist", 100); err == nil {
		t.Fatal("ListDir of a missing directory succeeded")
	}
}

// TestListDirBoundsWhatItMaterialises pins B6. list_dir's output was capped at
// 500 entries but the sandbox returned every entry first, so a directory with a
// million files was a million os.DirEntry values in memory before the first
// line was rendered. The sandbox now takes a bound and streams the directory in
// batches, keeping only the smallest `limit` names plus a running total.
func TestListDirBoundsWhatItMaterialises(t *testing.T) {
	sb := newTestSandbox(t, SandboxConfig{})
	const limit = 500
	const extra = 2_000
	for i := 0; i < extra; i++ {
		if err := os.WriteFile(filepath.Join(sb.Dir(), fmt.Sprintf("e%05d", i)), nil, 0o644); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}

	entries, total, err := sb.ListDir(".", limit)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if total != extra+1 { // + the sandbox's own "tmp"
		t.Fatalf("ListDir counted %d entries, want %d", total, extra+1)
	}
	if len(entries) != limit {
		t.Fatalf("ListDir returned %d entries, want exactly %d: the listing must be bounded at the source, not after materialising", len(entries), limit)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Name() > entries[i].Name() {
			t.Fatalf("entries[%d]=%q > entries[%d]=%q; a bounded listing still sorts", i-1, entries[i-1].Name(), i, entries[i].Name())
		}
	}
	if first, last := entries[0].Name(), entries[len(entries)-1].Name(); first != "e00000" || last != "e00499" {
		t.Fatalf("bounded listing runs %q..%q, want the 500 smallest names e00000..e00499", first, last)
	}

	// A directory smaller than the bound is returned whole, with its true count.
	if err := os.RemoveAll(filepath.Join(sb.Dir(), "e00001")); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	entries, total, err = sb.ListDir(".", 10_000)
	if err != nil {
		t.Fatalf("ListDir with a large bound: %v", err)
	}
	if total != extra || len(entries) != total {
		t.Fatalf("ListDir = %d entries of %d, want all %d below the bound", len(entries), total, extra)
	}
}

func TestSandboxProfileIsGenerated(t *testing.T) {
	sb := newTestSandbox(t, SandboxConfig{Network: false})
	profile := sb.profile
	for _, want := range []string{
		"(version 1)",
		"(deny default)",
		"(subpath " + quoteProfilePath(sb.Dir()) + ")",
		"(deny file-read-data",
		// macOS 26+ dyld aborts before main when the root directory is not
		// readable; the run directory rules do not cover "/" itself.
		`(literal "/")`,
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile is missing %q:\n%s", want, profile)
		}
	}
	if strings.Contains(profile, "network-outbound") {
		t.Fatalf("profile allows the network for an item that did not declare network:\n%s", profile)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if !strings.Contains(profile, quoteProfilePath(home)) {
			t.Errorf("profile does not name the home directory %q in its deny rule:\n%s", home, profile)
		}
	}

	networked := newTestSandbox(t, SandboxConfig{RunID: "run-net", Network: true})
	if !strings.Contains(networked.profile, "network-outbound") {
		t.Fatalf("profile for a network item has no network rule:\n%s", networked.profile)
	}
}

// TestSandboxProfileParses is a syntax check, and only a syntax check. On a
// machine where sandbox-exec can apply a profile it also proves the command
// starts. Where it cannot apply one — a nested sandbox, or a future macOS that
// removes the tool — an apply failure is logged and the test does not pretend
// the profile was enforced.
func TestSandboxProfileParses(t *testing.T) {
	path, err := exec.LookPath("sandbox-exec")
	if err != nil {
		t.Skip("sandbox-exec is not installed; the generated profile cannot be parsed on this machine")
	}
	// A root with a quote, a backslash and a space exercises the profile's
	// string escaping. Run ids are validated separately and stay dull; the
	// root is operator-supplied and may contain anything without a control
	// character.
	root := filepath.Join(t.TempDir(), `root "quoted" \ spaced`)
	sb := newTestSandbox(t, SandboxConfig{Root: root, RunID: "run-1"})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-p", sb.profile, "/usr/bin/true")
	out, err := cmd.CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err == nil {
		return
	}
	// A parse failure is a defect in the generated profile, not an
	// environment limitation: sandbox-exec exits 65 (EX_DATAERR) when the
	// profile does not parse, before it ever calls sandbox_apply. A denied
	// apply is a different failure with its own exit code (71 in this process),
	// so the two do not collide. The exit code is the exact signal; matching the
	// sentence is a second-hand guess that a future wording can break.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 65 {
		t.Fatalf("the generated sandbox profile does not parse (sandbox-exec exited 65): %v: %s\nprofile:\n%s", err, msg, sb.profile)
	}
	t.Logf("the generated profile parsed, but sandbox-exec could not apply it here: %v: %s", err, msg)
}

// TestKernelConfinementStatesTheReason keeps "absent" a supported state with an
// explanation instead of a crash or a silent false.
func TestKernelConfinementStatesTheReason(t *testing.T) {
	active, why := KernelConfinement()
	if active && why != "" {
		t.Fatalf("active kernel confinement carries a reason %q", why)
	}
	if !active && why == "" {
		t.Fatal("inactive kernel confinement has no reason; a caller cannot say why it is unenforced")
	}
	if runtime.GOOS != "darwin" && active {
		t.Fatal("kernel confinement is reported active on a non-darwin OS")
	}
}
