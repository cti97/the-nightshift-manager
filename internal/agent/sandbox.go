package agent

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// This file is the container the agentic executor's tools run in (spec §10).
//
// # What this is
//
// A blast-radius reducer, not a security boundary. The design is explicit
// (§10, §19 weakness 2): sandbox-exec is deprecated and Seatbelt profiles fail
// open in ways that are easy to get wrong. The structural guarantee it does
// make is narrow and qualified: at the tool layer, nothing in the run directory
// can reach the operator's home directory or credentials. A shell started
// through Command is held to that only when the process layer below is actually
// applied; on a machine where it is not, the shell can still read the home
// directory and this type cannot stop it (KernelConfinement says so).
//
// # Two layers, covering different things
//
//  1. The tool layer — ReadFile, WriteFile, ListDir, MkdirAll. It is enforced
//     on every OS by os.Root, which resolves paths with openat relative to the
//     run directory: "..", absolute paths, and symlinks that point outside are
//     refused by the runtime, never cleaned into a legal path. On Linux this is
//     the only boundary there is: spec §4 gives a Linux node "a plain
//     directory".
//
//  2. The process layer — Command. On macOS, when a profile can actually be
//     applied, child processes get a generated Seatbelt profile (read system
//     paths, write only the run directory, network only if the item declared
//     it). When it cannot be applied, the command still gets the scrubbed
//     environment and the run directory as its working directory, but the
//     kernel does not confine it. KernelConfinement reports which case the
//     machine is in; callers must log that and must not claim more.
//
// Neither layer stops a shell on Linux, and neither layer is a jail for a
// determined process on any OS. The model that runs `shell` gets a shell.

// ErrSandboxClosed is returned by operations on a sandbox whose Close has run.
var ErrSandboxClosed = errors.New("agent: sandbox is closed")

// ErrPathOutside is returned when a path argument names something that is not
// inside the run directory. Traversal is refused, not cleaned: a caller that
// asks for "../../etc/passwd" is told no, rather than given "/etc/passwd".
var ErrPathOutside = errors.New("agent: path is outside the run directory")

// maxReadBytes bounds one read_file. A model that asks for a multi-gigabyte
// file must fail in the tool, not take the node's memory with it.
const maxReadBytes = 4 << 20

// SandboxConfig describes one run's disposable working directory.
//
// Root is the configured run root (spec §10's run_root), RunID the board's run
// identifier. Run directories are created as Root/RunID; run ids are unique per
// claim (board.NewToken), so a directory left by a crashed attempt can never be
// mistaken for a live one.
type SandboxConfig struct {
	Root  string
	RunID string
	// Network is the item's declaration (capability "network"), not a guess by
	// this package. It decides whether the generated profile allows outbound
	// connections; on a machine with no kernel confinement it changes nothing.
	Network bool
	// KeepArtifacts leaves the directory behind on Close, for inspection.
	KeepArtifacts bool
}

// Sandbox is one run's directory and process policy.
type Sandbox struct {
	dir     string
	root    *os.Root
	network bool
	keep    bool
	env     []string
	profile string
	kernel  kernelSupport

	// dirInfo identifies the directory this sandbox created, so Close can
	// refuse to delete a different one that appeared at the same path.
	dirInfo os.FileInfo

	mu       sync.RWMutex
	closed   bool
	closeErr error // the first Close outcome, repeated by later calls
}

// NewSandbox creates Root/RunID fresh and opens it as the root every path is
// resolved against.
//
// An existing run directory is refused, not adopted and not deleted: run ids
// are minted per claim, so a directory that already exists there belongs to
// some earlier process, and this one did not create it.
func NewSandbox(cfg SandboxConfig) (*Sandbox, error) {
	if err := validateRoot(cfg.Root); err != nil {
		return nil, err
	}
	if err := validateRunID(cfg.RunID); err != nil {
		return nil, err
	}

	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve root %q: %w", cfg.Root, err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("sandbox: create root %q: %w", root, err)
	}
	// Resolve the root so Dir, the profile and Close all name the same
	// directory even when the configured root is a symlink.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve root %q: %w", root, err)
	}

	dir := filepath.Join(resolved, cfg.RunID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("sandbox: run directory %q already exists; a previous attempt left it behind and this sandbox will not adopt or delete it", dir)
		}
		return nil, fmt.Errorf("sandbox: create run directory %q: %w", dir, err)
	}
	// Clean up the half-built sandbox on any later failure: a failed New must
	// not leave a directory the caller has no handle to dispose of.
	fail := func(err error) (*Sandbox, error) {
		os.RemoveAll(dir)
		return nil, err
	}

	info, err := os.Lstat(dir)
	if err != nil {
		return fail(fmt.Errorf("sandbox: stat run directory %q: %w", dir, err))
	}
	rootHandle, err := os.OpenRoot(dir)
	if err != nil {
		return fail(fmt.Errorf("sandbox: open run directory %q: %w", dir, err))
	}
	if err := rootHandle.Mkdir("tmp", 0o700); err != nil {
		rootHandle.Close()
		return fail(fmt.Errorf("sandbox: create tmp directory: %w", err))
	}

	return &Sandbox{
		dir:     dir,
		root:    rootHandle,
		network: cfg.Network,
		keep:    cfg.KeepArtifacts,
		env:     sandboxEnv(dir),
		profile: buildProfile(dir, cfg.Network),
		kernel:  detectKernel(),
		dirInfo: info,
	}, nil
}

// Dir is the absolute path of the run directory. It is a name, not a
// confinement: tools that open files should use this type's methods, which
// resolve through os.Root, not filepath.Join(Dir(), name).
func (s *Sandbox) Dir() string { return s.dir }

// Network reports whether the item declared the "network" capability.
func (s *Sandbox) Network() bool { return s.network }

// Env returns a copy of the environment child processes get. It is built from
// a fixed list rather than filtered from os.Environ(), so an inherited variable
// cannot leak in. The one value read from the parent is SystemRoot on Windows,
// which is a path, not a credential.
func (s *Sandbox) Env() []string { return append([]string(nil), s.env...) }

// ReadFile reads one file inside the run directory. Traversal, absolute paths
// and symlinks that leave the run directory are refused.
func (s *Sandbox) ReadFile(name string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrSandboxClosed
	}
	clean, err := confinedPath(name)
	if err != nil {
		return nil, err
	}
	f, err := s.root.Open(clean)
	if err != nil {
		return nil, fmt.Errorf("sandbox: read %q: %w", name, err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("sandbox: read %q: %w", name, err)
	}
	if int64(len(data)) > maxReadBytes {
		return nil, fmt.Errorf("sandbox: read %q: file is larger than %d bytes", name, maxReadBytes)
	}
	return data, nil
}

// WriteFile writes one file inside the run directory. The parent directory must
// exist; callers create it with MkdirAll. A symlink inside the run directory is
// followed unless it resolves outside, in which case the write is refused.
func (s *Sandbox) WriteFile(name string, data []byte, perm os.FileMode) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrSandboxClosed
	}
	clean, err := confinedPath(name)
	if err != nil {
		return err
	}
	f, err := s.root.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("sandbox: write %q: %w", name, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("sandbox: write %q: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("sandbox: write %q: %w", name, err)
	}
	return nil
}

// MkdirAll creates a directory tree inside the run directory. os.Root has
// MkdirAll only from Go 1.25, and this module targets Go 1.24, so the walk is
// here. Every component goes through Root.Mkdir, so a symlinked component that
// points outside is still refused.
func (s *Sandbox) MkdirAll(name string, perm os.FileMode) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrSandboxClosed
	}
	clean, err := confinedPath(name)
	if err != nil {
		return err
	}
	if clean == "." {
		return nil
	}
	parts := strings.Split(clean, string(filepath.Separator))
	for i := range parts {
		p := filepath.Join(parts[:i+1]...)
		err := s.root.Mkdir(p, perm)
		if err == nil {
			continue
		}
		if errors.Is(err, fs.ErrExist) {
			fi, statErr := s.root.Stat(p)
			if statErr != nil {
				return fmt.Errorf("sandbox: mkdir %q: %w", name, statErr)
			}
			if fi.IsDir() {
				continue
			}
			return fmt.Errorf("sandbox: mkdir %q: %q exists and is not a directory", name, p)
		}
		return fmt.Errorf("sandbox: mkdir %q: %w", name, err)
	}
	return nil
}

// Stat reports the metadata of one path inside the run directory.
//
// It exists for callers that must know what they are about to open. `ReadFile`
// on a named pipe, and `ListDir` on one, block in open(2) until a writer appears
// — and a tool whose read never returns hangs the run it belongs to, with no
// timeout to rescue it. The tool layer checks the mode with this before opening.
// Stat follows a symlink inside the run directory (as Root.Stat does) and
// refuses one that leaves it.
func (s *Sandbox) Stat(name string) (os.FileInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrSandboxClosed
	}
	clean, err := confinedPath(name)
	if err != nil {
		return nil, err
	}
	info, err := s.root.Stat(clean)
	if err != nil {
		return nil, fmt.Errorf("sandbox: stat %q: %w", name, err)
	}
	return info, nil
}

// dirReadBatch is how many directory entries one ListDir call reads at a time.
// The listing is streamed, not read whole: the tool caps its OUTPUT at 500
// entries, and the memory this method holds must be bounded by the same kind of
// number rather than by the directory's size.
const dirReadBatch = 256

// dirEntryMaxHeap holds the `limit` smallest names seen so far. The root is the
// LARGEST kept entry, which is the one a newly seen smaller name replaces.
// A max-heap is what makes "the first 500 by name" a bounded-memory question
// instead of "read everything, sort, slice".
type dirEntryMaxHeap []os.DirEntry

func (h dirEntryMaxHeap) Len() int           { return len(h) }
func (h dirEntryMaxHeap) Less(i, j int) bool { return h[i].Name() > h[j].Name() }
func (h dirEntryMaxHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *dirEntryMaxHeap) Push(x any)        { *h = append(*h, x.(os.DirEntry)) }
func (h *dirEntryMaxHeap) Pop() any {
	old := *h
	n := len(old)
	entry := old[n-1]
	*h = old[:n-1]
	return entry
}

// ListDir lists one directory inside the run directory: at most limit entries,
// the smallest by name, sorted, plus the total number of entries that were
// there. A bound is a parameter because the number belongs to the caller's
// output policy (list_dir's 500-entry cap), not to the filesystem layer; a
// limit of zero or less returns no entries but still counts them.
//
// The directory is read in dirReadBatch-sized batches and only the kept window
// is retained, so memory is O(limit + dirReadBatch) whatever the directory's
// size. os.File.ReadDir(-1) would make it O(entries) — the old behaviour, where
// the output was bounded but the memory was not. The full count is still exact:
// every entry is visited, none is stored unless it is one of the smallest
// `limit` so far.
func (s *Sandbox) ListDir(name string, limit int) ([]os.DirEntry, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, 0, ErrSandboxClosed
	}
	clean, err := confinedPath(name)
	if err != nil {
		return nil, 0, err
	}
	f, err := s.root.Open(clean)
	if err != nil {
		return nil, 0, fmt.Errorf("sandbox: list %q: %w", name, err)
	}
	defer f.Close()

	capacity := limit
	if capacity < 0 {
		capacity = 0
	}
	kept := make(dirEntryMaxHeap, 0, capacity)
	total := 0
	for {
		batch, readErr := f.ReadDir(dirReadBatch)
		for _, entry := range batch {
			total++
			if limit <= 0 {
				continue
			}
			if len(kept) < limit {
				heap.Push(&kept, entry)
				continue
			}
			if entry.Name() < kept[0].Name() {
				heap.Pop(&kept)
				heap.Push(&kept, entry)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, 0, fmt.Errorf("sandbox: list %q: %w", name, readErr)
		}
		if len(batch) == 0 {
			// ReadDir with n > 0 promises an error rather than an empty, nil
			// batch; this guards a future implementation that does not.
			break
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Name() < kept[j].Name() })
	return kept, total, nil
}

// Command builds a command that runs inside the sandbox: the run directory as
// its working directory, this package's environment and nothing else, and — on
// macOS, when it can be applied — the generated Seatbelt profile.
//
// The caller starts it (CombinedOutput, Start, ...) and owns its timeout. The
// context kills the direct child; a shell's own children are the caller's
// problem, and are a known concern for Task 11's shell tool.
func (s *Sandbox) Command(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, ErrSandboxClosed
	}
	env := append([]string(nil), s.env...)
	dir, profile, kernel := s.dir, s.profile, s.kernel
	s.mu.RUnlock()

	program, err := lookPathIn(sandboxPath(), name)
	if err != nil {
		return nil, err
	}

	var cmd *exec.Cmd
	if kernel.path != "" {
		// The profile is passed inline rather than written into the run
		// directory: a file inside the run directory is writable by the very
		// process the profile constrains, and a weakened profile would apply
		// to the next command.
		cmd = exec.CommandContext(ctx, kernel.path, append([]string{"-p", profile, program}, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, program, args...)
	}
	cmd.Dir = dir
	cmd.Env = env
	// nil Stdin means /dev/null: a tool that reads stdin sees EOF, not the
	// agent's input.
	cmd.Stdin = nil
	return cmd, nil
}

// Close disposes of the run directory unless KeepArtifacts was set. It is
// idempotent: a second call returns the same outcome as the first — nil after a
// successful disposal, and the same error after a refusal, so a refused removal
// is not silently lost to a later call.
//
// Close removes only the directory this sandbox created. If that path was
// renamed away and something else now sits there, Close refuses and says so
// rather than deleting another process's directory.
//
// A deferred Close discards its error by construction. A caller that needs to
// know whether the removal actually happened must call Close explicitly at
// least once.
func (s *Sandbox) Close() (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	defer func() { s.closeErr = err }()

	var first error
	if s.root != nil {
		if err := s.root.Close(); err != nil {
			first = err
		}
		s.root = nil
	}
	if s.keep {
		return first
	}

	if s.dirInfo != nil {
		cur, err := os.Lstat(s.dir)
		switch {
		case err == nil && !os.SameFile(s.dirInfo, cur):
			if first == nil {
				first = fmt.Errorf("sandbox: refusing to remove %q: it is not the directory this sandbox created", s.dir)
			}
			return first
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			if first == nil {
				first = fmt.Errorf("sandbox: stat %q before removal: %w", s.dir, err)
			}
			return first
		}
	}
	if err := os.RemoveAll(s.dir); err != nil && first == nil {
		first = fmt.Errorf("sandbox: remove run directory %q: %w", s.dir, err)
	}
	return first
}

// confinedPath validates a caller's path argument before it reaches os.Root.
// os.Root would refuse the same paths, but doing the lexical check here makes
// the refusal a named ErrPathOutside instead of a "path escapes from parent"
// error the tool layer would have to interpret.
func confinedPath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty path", ErrPathOutside)
	}
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: %q contains a NUL byte", ErrPathOutside, name)
	}
	if filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("%w: %q is absolute", ErrPathOutside, name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q escapes the run directory", ErrPathOutside, name)
	}
	return clean, nil
}

func validateRoot(root string) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("sandbox: Root is required (spec §10's run_root)")
	}
	if !filepath.IsAbs(root) {
		return fmt.Errorf("sandbox: Root %q must be an absolute path; a relative root makes the process's working directory the configuration", root)
	}
	for _, r := range root {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("sandbox: Root %q contains a control character", root)
		}
	}
	return nil
}

// validateRunID keeps a run id a single, dull path element. The board mints
// hex tokens, but the value arrives over the wire, so it is not trusted to be
// one: "../.." here would put the run directory outside the configured root.
func validateRunID(id string) error {
	if id == "" {
		return errors.New("sandbox: RunID is required")
	}
	if id == "." || id == ".." {
		return fmt.Errorf("sandbox: RunID %q is not a directory name", id)
	}
	if len(id) > 128 {
		return fmt.Errorf("sandbox: RunID is %d bytes; the limit is 128", len(id))
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("sandbox: RunID %q contains %q; a run id must be one path element of letters, digits, '-', '_' and '.'", id, r)
		}
	}
	return nil
}

// sandboxEnv is the entire child environment. It is constructed from a fixed
// list rather than filtered from os.Environ(): a variable this package does not
// name cannot leak in, which is the only version of "env -i" that stays true as
// the operator's shell grows. On Windows the list includes SystemRoot, read
// from the parent because the loader needs it; it is a path, not a credential.
func sandboxEnv(dir string) []string {
	tmp := filepath.Join(dir, "tmp")
	env := []string{
		"LANG=C",
		"PATH=" + sandboxPath(),
		"HOME=" + dir,
		"PWD=" + dir,
		"TMPDIR=" + tmp,
	}
	if runtime.GOOS == "windows" {
		env = append(env,
			"USERPROFILE="+dir,
			"TMP="+tmp,
			"TEMP="+tmp,
			"PATHEXT=.COM;.EXE;.BAT;.CMD",
			"SystemRoot="+windowsRoot(),
		)
	}
	return env
}

// sandboxPath is the PATH children see, not the parent's. It names only system
// binaries; a tool installed somewhere else is not on it, deliberately.
func sandboxPath() string {
	if runtime.GOOS == "windows" {
		root := windowsRoot()
		return filepath.Join(root, "System32") + ";" + root
	}
	return "/usr/bin:/bin:/usr/sbin:/sbin"
}

func windowsRoot() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		return root
	}
	return `C:\Windows`
}

// lookPathIn resolves a program against the sandbox PATH rather than the
// parent's, so the same name cannot mean different binaries in the agent and in
// the sandbox. An absolute path is accepted as given; the profile (when one is
// applied) decides whether it may be executed.
func lookPathIn(pathList, name string) (string, error) {
	if name == "" {
		return "", errors.New("sandbox: empty program name")
	}
	if filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return executable(name)
	}
	if strings.ContainsRune(name, filepath.Separator) {
		return "", fmt.Errorf("sandbox: program %q is relative but contains a separator; pass a bare name or an absolute path", name)
	}
	for _, dir := range filepath.SplitList(pathList) {
		if dir == "" {
			continue
		}
		for _, candidate := range programCandidates(name) {
			if p, err := executable(filepath.Join(dir, candidate)); err == nil {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("sandbox: program %q not found in the sandbox PATH %q", name, pathList)
}

func programCandidates(name string) []string {
	if runtime.GOOS != "windows" || filepath.Ext(name) != "" {
		return []string{name}
	}
	return []string{name, name + ".exe", name + ".cmd", name + ".bat"}
}

func executable(p string) (string, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("sandbox: program %q: %w", p, err)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("sandbox: program %q is a directory", p)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("sandbox: program %q is not executable", p)
	}
	return p, nil
}

// kernelSupport is the detected state of the OS process layer.
type kernelSupport struct {
	path   string // sandbox-exec, when it can actually apply a profile
	reason string // why not, when path is empty
}

var (
	kernelOnce sync.Once
	kernelInfo kernelSupport
)

// KernelConfinement reports whether child processes started through a Sandbox
// are confined by the OS on this machine, and if not, why.
//
// false is a supported state, not a failure: Linux has no layer here at all
// (spec §4: "a plain directory"), and on macOS the tool can be absent, or
// present but unable to apply a profile — for example when the agent itself
// runs inside a sandbox, because a nested sandbox_apply is denied. Callers
// should log the reason once and must not describe the run as kernel-confined.
//
// The acceptance test that proves the kernel half
// (TestSandboxCannotReachTheHomeDirectory) skips when confinement is inactive,
// because a green tick over an unenforced boundary is worse than no test. On a
// machine that is supposed to have the layer, NSM_REQUIRE_KERNEL_SANDBOX=1 turns
// that skip into a failure. The variable is read by that test only, never by
// this function or by the running process, and on a non-darwin OS it has no
// effect: the test skips on the platform before it reads the variable, and there
// is no kernel layer here for it to require.
func KernelConfinement() (bool, string) {
	k := detectKernel()
	if k.path != "" {
		return true, ""
	}
	return false, k.reason
}

// detectKernel finds out whether sandbox-exec can actually apply a profile here
// by applying a trivial one to /usr/bin/true. Presence is not enough: the
// binary exists on this machine but sandbox_apply is refused in a nested
// sandbox, and a caller that only checked exec.LookPath would claim
// confinement it does not have.
func detectKernel() kernelSupport {
	kernelOnce.Do(func() {
		if runtime.GOOS != "darwin" {
			kernelInfo.reason = fmt.Sprintf("sandbox-exec is macOS-only; on %s the run directory is a plain directory and the tool layer is the only boundary (spec §4)", runtime.GOOS)
			return
		}
		path, err := exec.LookPath("sandbox-exec")
		if err != nil {
			kernelInfo.reason = "sandbox-exec is not installed"
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		probe := exec.CommandContext(ctx, path, "-p", "(version 1)(allow default)", "/usr/bin/true")
		probe.Env = []string{"PATH=/usr/bin:/bin"}
		out, err := probe.CombinedOutput()
		if err != nil {
			detail := strings.TrimSpace(string(out))
			if detail == "" {
				detail = err.Error()
			} else {
				detail = fmt.Sprintf("%v: %s", err, detail)
			}
			kernelInfo.reason = "sandbox-exec is present but cannot apply a profile in this process (" + detail + "); the quoted failure is the authoritative reason"
			return
		}
		kernelInfo.path = path
	})
	return kernelInfo
}

// buildProfile generates the Seatbelt profile for one run. It is generated
// even when the profile cannot be applied, so it can be inspected and parsed.
//
// The ordering is load-bearing: Seatbelt resolves rules in order and the last
// matching rule wins. The broad system allow therefore comes first, the home
// and data-volume denies next, and the run-directory allow after both, so a run
// root under the home directory is still usable while the rest of the home is
// not.
func buildProfile(dir string, network bool) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n\n")

	b.WriteString("; Process and IPC basics for a shell. No process-info*: a shell does\n")
	b.WriteString("; not need to inspect other processes, and that is one way credentials\n")
	b.WriteString("; leak. mach-lookup is limited to two services a command-line tool\n")
	b.WriteString("; plausibly needs; the keychain services are deliberately not among them.\n")
	b.WriteString("(allow process-exec*)\n")
	b.WriteString("(allow process-fork)\n")
	b.WriteString("(allow signal (target self))\n")
	b.WriteString("(allow ipc-posix-shm)\n")
	b.WriteString("(allow sysctl-read)\n")
	b.WriteString("(deny sysctl-read (sysctl-name \"kern.procargs2\"))\n")
	b.WriteString("(allow mach-lookup\n")
	b.WriteString("    (global-name \"com.apple.system.logger\")\n")
	b.WriteString("    (global-name \"com.apple.system.notification_center\"))\n\n")

	b.WriteString("; Read-only system paths. /usr/local and /opt/homebrew are not here: a\n")
	b.WriteString("; tool that is not part of the OS is not available to the shell.\n")
	b.WriteString("(allow file-read*\n")
	b.WriteString("    (subpath \"/System\")\n")
	b.WriteString("    (subpath \"/usr\")\n")
	b.WriteString("    (subpath \"/bin\")\n")
	b.WriteString("    (subpath \"/sbin\")\n")
	b.WriteString("    (subpath \"/Library/Frameworks\")\n")
	b.WriteString("    (subpath \"/Library/Apple\")\n")
	b.WriteString("    (subpath \"/private/etc\")\n")
	b.WriteString("    (subpath \"/private/var/db/dyld\")\n")
	b.WriteString("    (literal \"/dev/null\")\n")
	b.WriteString("    (literal \"/dev/zero\")\n")
	b.WriteString("    (literal \"/dev/random\")\n")
	b.WriteString("    (literal \"/dev/urandom\")\n")
	b.WriteString("    (literal \"/dev/stdin\")\n")
	b.WriteString("    (literal \"/dev/stdout\")\n")
	b.WriteString("    (literal \"/dev/stderr\")\n")
	b.WriteString("    (subpath \"/dev/fd\"))\n\n")

	b.WriteString("; Path traversal needs metadata; file contents do not follow from it.\n")
	b.WriteString("(allow file-read-metadata)\n\n")

	// macOS firmlinks make the data volume visible under /System/Volumes/Data.
	// Without this deny, the (subpath "/System") allow above would cover every
	// user file through that spelling, including the home directory. Only data
	// is denied, not metadata: traversal through a firmlinked ancestor is what
	// lets a run directory under the home stay usable.
	b.WriteString("; The data volume is the same inode as /Users, /Library, /private and\n")
	b.WriteString("; /usr/local under another name. Denying its data closes the alias that\n")
	b.WriteString("; the /System allow would otherwise open. Metadata stays readable so\n")
	b.WriteString("; path traversal into the run directory still works.\n")
	b.WriteString("(deny file-read-data (subpath \"/System/Volumes/Data\"))\n")
	b.WriteString("(deny file-write* (subpath \"/System/Volumes/Data\"))\n\n")

	if home := realHome(); home != "" {
		// A rule that names one spelling of a path does not cover the other.
		// Both the logical home and its data-volume spelling are named.
		paths := []string{home}
		if strings.HasPrefix(home, string(filepath.Separator)) && !strings.HasPrefix(home, "/System/Volumes/Data") {
			paths = append(paths, filepath.Join("/System/Volumes/Data", home))
		}
		b.WriteString("; The operator's home directory: contents are denied even though no\n")
		b.WriteString("; allow above covers it. This is defense in depth against a later edit\n")
		b.WriteString("; that widens a system allow, and it names the data-volume spelling\n")
		b.WriteString("; because that is the same inode under a different name.\n")
		b.WriteString("(deny file-read-data")
		for _, p := range paths {
			b.WriteString(" (subpath " + quoteProfilePath(p) + ")")
		}
		b.WriteString(")\n")
		b.WriteString("(deny file-write*")
		for _, p := range paths {
			b.WriteString(" (subpath " + quoteProfilePath(p) + ")")
		}
		b.WriteString(")\n\n")
	}

	// The run directory allow comes after the home deny, so a run root under
	// the home stays writable while the rest of the home does not. Both
	// spellings are allowed for the same reason the denies name both: the
	// kernel may resolve one path through the firmlink.
	runPaths := []string{dir}
	if strings.HasPrefix(dir, string(filepath.Separator)) && !strings.HasPrefix(dir, "/System/Volumes/Data") {
		runPaths = append(runPaths, filepath.Join("/System/Volumes/Data", dir))
	}
	b.WriteString("; The run directory: the only writable place.\n")
	b.WriteString("(allow file-read*")
	for _, p := range runPaths {
		b.WriteString(" (subpath " + quoteProfilePath(p) + ")")
	}
	b.WriteString(")\n")
	b.WriteString("(allow file-write*")
	for _, p := range runPaths {
		b.WriteString(" (subpath " + quoteProfilePath(p) + ")")
	}
	b.WriteString(")\n")
	b.WriteString("(allow file-write* (literal \"/dev/null\"))\n\n")

	b.WriteString("; Keychain files are part of the credential store the profile exists to\n")
	b.WriteString("; keep away from a run. They are outside the allows above anyway; naming\n")
	b.WriteString("; them keeps that true if a system path is ever widened.\n")
	b.WriteString("(deny file-read-data (subpath \"/System/Library/Keychains\") (subpath \"/Library/Keychains\"))\n\n")

	if network {
		b.WriteString("; The item declared the network capability.\n")
		b.WriteString("(allow network-outbound)\n")
	}
	return b.String()
}

// realHome is the operator's home as this process sees it. It is read once per
// profile, from the parent environment, because the child's HOME is the run
// directory.
func realHome() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	home = filepath.Clean(home)
	for _, r := range home {
		if r < 0x20 || r == 0x7f {
			// A profile is a program. A path with a newline in it cannot be
			// quoted into one safely, so the explicit rule is omitted; the
			// allows above still do not name the home.
			return ""
		}
	}
	return home
}

// quoteProfilePath quotes a path for the profile string syntax. Parentheses and
// spaces are fine inside a quoted string; only the quote and the backslash need
// escaping.
func quoteProfilePath(p string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p) + `"`
}
