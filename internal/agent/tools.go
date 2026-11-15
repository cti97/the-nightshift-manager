package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cti97/the-nightshift-manager/internal/provider"
)

// This file is the agentic executor's closed tool set (spec §10): read_file,
// write_file, list_dir, shell, http_get and finish.
//
// # The tools are the boundary
//
// Design §10 calls the sandbox a blast-radius reducer, not a security boundary,
// and §19 weakness 17 records the harder fact: in the environment M2 was built
// in, the kernel layer never runs. `sandbox-exec` cannot apply a profile there,
// so a command started through a Sandbox gets the run directory as its working
// directory and a constructed environment, and nothing more. Every Linux node is
// in the same state by design (spec §4: "a plain directory").
//
// What follows is the rule this file is built around: for the file tools, this
// code is the enforcement. A path argument that can be talked into pointing
// outside the run directory is not caught by anything downstream. So:
//
//   - every path goes through the sandbox's own ReadFile / WriteFile / ListDir /
//     MkdirAll, never sb.Dir() plus a join. The join would happen outside
//     os.Root's openat resolution, which is the only confinement there is.
//   - a ".." element is refused, not cleaned. filepath.Clean("a/../../etc/passwd")
//     is a legal-looking path; the point is to refuse the model's intent, not to
//     normalise it. See toolPath.
//   - a file argument ending in a separator is refused rather than cleaned,
//     because cleaning makes write_file("notes/") quietly create a *file* named
//     notes.
//   - http_get is gated on the sandbox's network declaration. On a node without
//     kernel confinement that gate is the only one there is.
//
// # What the tools cannot promise
//
// shell runs a real shell: /bin/sh -c on Unix, cmd.exe on Windows. Inside the
// run directory it can do anything a shell can do, and on a node without kernel
// confinement it can do it outside the run directory too: read the operator's
// files, reach the network whether or not the network capability was declared,
// write anywhere the agent's uid can write. No text filter changes that, and
// this file deliberately contains none — a filter over shell text is defeated by
// quoting, and shipping one would claim a guarantee that does not hold. A
// shell's confinement comes from the kernel layer, which is not applied here;
// KernelConfinement reports that state, and Task 10's report says how to verify
// it on a machine where it applies.
//
// # What a tool returns
//
// Run returns the tool's output and a non-nil error when it failed. On failure
// the output is whatever was produced before the failure (a compiler's stderr,
// for instance) and the caller should show the model both: the diagnostics and
// "exit status 1" are one piece of information, not two.
//
// finish is the exception. It returns the operator-facing summary as its output
// and ErrFinished as its error, which is a control signal rather than a failure:
// the loop stops and reports the string as the run's result. Test it with
// IsFinish before treating a tool error as an error to hand back to the model.
//
// Every Run checks its context before it touches anything. A tool called with a
// cancelled context writes no file, starts no process and opens no socket.

// Tool names. They are the wire names the model calls and the keys the executor
// dispatches on; a constant keeps a typo out of both sides.
const (
	ToolReadFile  = "read_file"
	ToolWriteFile = "write_file"
	ToolListDir   = "list_dir"
	ToolShell     = "shell"
	ToolHTTPGet   = "http_get"
	ToolFinish    = "finish"
)

// Tool is one callable the model may ask for. Schema is the JSON Schema of the
// arguments and carries the tool's description at its top level, which is where
// JSON Schema puts a description. Definition splits the two for the provider
// wire shapes without keeping a second copy of the prose that could drift from
// the schema the model actually sees.
type Tool struct {
	Name   string
	Schema json.RawMessage
	Run    func(ctx context.Context, sb *Sandbox, raw json.RawMessage) (string, error)
}

// ErrFinished is returned by the finish tool. It is a control signal, not a
// failure: the loop stops and reports the string Run returned as the run's
// result. It is deliberately distinguishable — errors.Is(err, ErrFinished) —
// because a loop that treated it as a tool error would hand the model a message
// and keep going, and the run would never end.
var ErrFinished = errors.New("agent: the model ended the run")

// IsFinish reports whether err is the finish tool's control signal.
func IsFinish(err error) bool { return errors.Is(err, ErrFinished) }

// ErrPathRefused is returned when a path argument is inside the run directory
// but not usable by the tool that was given it: a trailing separator where a
// file is expected, the run directory itself where a file is expected, or a
// file that is not a regular file.
//
// It is deliberately separate from ErrPathOutside. "The model tried to leave the
// run directory" and "the model named the wrong thing" are different facts, and
// the first is the refusal this layer exists for; a caller that audits escapes
// must not have to parse an error string to tell them apart.
var ErrPathRefused = errors.New("agent: the path argument is not usable by this tool")

// Limits. Each one is a budget guard: the model's context and the run's tokens
// are the scarce resource, so a tool that would spend them on something the
// model cannot read fails in the tool instead.
const (
	// maxWriteBytes bounds one write_file. It mirrors the read cap: content that
	// arrived in a tool call is already token-bounded, so a multi-megabyte
	// argument is a malfunction, not work.
	maxWriteBytes = maxReadBytes

	// maxShellOutput bounds the combined stdout/stderr a shell step returns.
	// Output is discarded past the cap and the answer says so; a command that
	// emits a gigabyte must not take the node's memory with it.
	maxShellOutput = 64 << 10

	// maxListEntries bounds one listing. A directory with a million entries is
	// a listing the model cannot use.
	maxListEntries = 500

	// shellDefaultTimeout, shellMaxTimeout and shellWaitDelay are the shell's
	// whole time policy. The default is what a model gets when it says nothing;
	// the maximum is the ceiling on what it can ask for. shellWaitDelay bounds
	// how long Wait blocks after the direct child is gone — see runShell.
	shellDefaultTimeout = 30 * time.Second
	shellMaxTimeout     = 300 * time.Second
	shellWaitDelay      = 2 * time.Second

	// httpGetMaxBytes bounds one response body. A redirect chain that returns
	// an unbounded body is a denial of service against the run's own budget,
	// which is why the limit is checked while reading, not after.
	httpGetMaxBytes = 1 << 20
	// httpGetTimeout bounds a whole request, including reading the body.
	httpGetTimeout = 20 * time.Second
	// httpGetMaxRedirects bounds request amplification: each hop is a request
	// the run pays for in time, and a loop of redirects is unbounded otherwise.
	httpGetMaxRedirects = 5

	// maxSummaryBytes bounds the operator-facing summary. It is larger than the
	// schema's maxLength because the schema counts characters and the check
	// counts bytes; the check is the one that protects the database.
	maxSummaryBytes = 16 << 10

	// binaryProbeBytes is how much of a file the binary check sniffs. NUL bytes
	// in the first block are file(1)'s own heuristic.
	binaryProbeBytes = 8 << 10
)

// The schemas. The description is the prompt the model reads before it calls the
// tool: it says what the tool does, what it refuses, and — for finish — that the
// summary is the product. A schema without one is refused by Definition.
var (
	readFileSchema = json.RawMessage(`{
  "description": "Read a UTF-8 text file from the run directory and return its contents. The path is relative to the run directory: absolute paths and '..' are refused, never resolved. Files larger than 4 MiB are refused. Binary files are refused, because unreadable bytes only spend the run's context budget.",
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path of the file, relative to the run directory. Example: notes/plan.md"
    }
  },
  "required": ["path"],
  "additionalProperties": false
}`)

	writeFileSchema = json.RawMessage(`{
  "description": "Create or overwrite a text file inside the run directory. Missing parent directories are created. The path is relative to the run directory: absolute paths and '..' are refused. An existing file is replaced completely. Content is written as UTF-8 exactly as given, up to 4 MiB.",
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path of the file, relative to the run directory. Example: out/report.md"
    },
    "content": {
      "type": "string",
      "description": "The complete new content of the file."
    }
  },
  "required": ["path", "content"],
  "additionalProperties": false
}`)

	listDirSchema = json.RawMessage(`{
  "description": "List one directory inside the run directory, sorted by name. Use '.' or omit the path for the run directory itself. Each line is: kind, a tab, size in bytes (or '-' when there is none), a tab, and the name; directories end in '/'. A listing over 500 entries is truncated.",
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Directory path relative to the run directory. Defaults to '.'."
    }
  },
  "additionalProperties": false
}`)

	shellSchema = json.RawMessage(`{
  "description": "Run a shell command with the run directory as its working directory. The command is executed by /bin/sh -c on Unix and cmd.exe on Windows, so pipes, redirects and globs work. It starts with a minimal environment: HOME is the run directory and no credentials are present. The command has a timeout; when it fires the shell is killed and the call fails. stdout and stderr are combined and truncated at 64 KiB.",
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "description": "The shell command line to run."
    },
    "timeout_seconds": {
      "type": "integer",
      "minimum": 1,
      "maximum": 300,
      "description": "How long to let the command run, in seconds. Default 30, maximum 300."
    }
  },
  "required": ["command"],
  "additionalProperties": false
}`)

	httpGetSchema = json.RawMessage(`{
  "description": "Fetch a URL with an HTTP GET and return the status line, the final URL after redirects, the content type and the body. Only available when the item declares the network capability; otherwise the call is refused. Redirects are followed up to 5 hops, and a redirect from https to http is refused. Response bodies over 1 MiB, and binary (non-UTF-8) bodies, are refused.",
  "type": "object",
  "properties": {
    "url": {
      "type": "string",
      "description": "An absolute http or https URL."
    }
  },
  "required": ["url"],
  "additionalProperties": false
}`)

	finishSchema = json.RawMessage(`{
  "description": "End the run and report its result. The summary is the only thing the operator will read about this run: write what you accomplished, where the artifacts are inside the run directory, what you verified, and what is unfinished or uncertain. Do not call this until the work is complete or you are certain it cannot be completed, and do not use it to ask a question.",
  "type": "object",
  "properties": {
    "summary": {
      "type": "string",
      "minLength": 1,
      "maxLength": 16000,
      "description": "The operator-facing result summary, plain text. State what was done, where the artifacts are, and what remains unverified."
    }
  },
  "required": ["summary"],
  "additionalProperties": false
}`)
)

// Tools returns the closed set of six, in a stable order, for one run.
//
// The slice is built per call rather than shared: a caller may sort, filter or
// pass it to something that mutates it, and a package-level slice would make
// that a data race between runs.
func Tools() []Tool {
	return []Tool{
		{Name: ToolReadFile, Schema: readFileSchema, Run: runReadFile},
		{Name: ToolWriteFile, Schema: writeFileSchema, Run: runWriteFile},
		{Name: ToolListDir, Schema: listDirSchema, Run: runListDir},
		{Name: ToolShell, Schema: shellSchema, Run: runShell},
		{Name: ToolHTTPGet, Schema: httpGetSchema, Run: runHTTPGet},
		{Name: ToolFinish, Schema: finishSchema, Run: runFinish},
	}
}

// Find looks a tool up by the name the model used. An unknown name is the
// caller's cue to answer the model with the names that do exist.
func Find(name string) (Tool, bool) {
	for _, tool := range Tools() {
		if tool.Name == name {
			return tool, true
		}
	}
	return Tool{}, false
}

// Definition converts one tool into the provider's wire shape.
func (t Tool) Definition() (provider.ToolDef, error) {
	var head struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(t.Schema, &head); err != nil {
		return provider.ToolDef{}, fmt.Errorf("agent: tool %q has an invalid schema: %w", t.Name, err)
	}
	if strings.TrimSpace(head.Description) == "" {
		return provider.ToolDef{}, fmt.Errorf("agent: tool %q has no description; a model with no description will misuse it", t.Name)
	}
	return provider.ToolDef{Name: t.Name, Description: head.Description, Schema: t.Schema}, nil
}

// Definitions converts a tool set to the provider's wire shape, in order.
func Definitions(tools []Tool) ([]provider.ToolDef, error) {
	out := make([]provider.ToolDef, 0, len(tools))
	for _, tool := range tools {
		def, err := tool.Definition()
		if err != nil {
			return nil, err
		}
		out = append(out, def)
	}
	return out, nil
}

// guardCtx is the first statement of every tool: a cancelled context does no
// work. It runs before decoding, so even a malformed call reports the
// cancellation rather than a schema error.
func guardCtx(ctx context.Context, tool string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("agent: %s: %w", tool, err)
	}
	return nil
}

// guardSandbox is a nil check, not a policy: the executor always has a sandbox,
// but a tool that panicked instead of failing would take the whole agent
// process — and every other run in it — down with it.
func guardSandbox(sb *Sandbox, tool string) error {
	if sb == nil {
		return fmt.Errorf("agent: %s was called without a sandbox", tool)
	}
	return nil
}

// decodeToolArgs decodes one call's arguments.
//
// Unknown fields are ignored on purpose. The schemas say
// "additionalProperties": false to steer the model, but providers pass the
// model's JSON through byte-for-byte and models do add a stray "reasoning" or
// repeat a parameter; refusing those costs a step and buys no safety, because
// nothing here reads a field it did not declare. Malformed JSON is refused: that
// is a model error the next step can fix.
func decodeToolArgs(raw json.RawMessage, v any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		trimmed = []byte("{}")
	}
	if err := json.Unmarshal(trimmed, v); err != nil {
		return fmt.Errorf("agent: invalid tool arguments: %w", err)
	}
	return nil
}

// toolPath validates a model-supplied path argument and returns the cleaned
// form the sandbox is given.
//
// It refuses rather than cleans, and that is the whole design:
//
//   - Any ".." element is ErrPathOutside, even when filepath.Clean would fold it
//     into a legal path. "a/../b" cleans to "b" — a legal path — but the model
//     asked to walk up and the answer to walking up is no. The alternative,
//     accept-what-stays-inside, makes the rule depend on how much the model
//     climbed, which is exactly the sort of condition that is wrong one edit
//     later.
//   - An absolute path, a volume-qualified path (Windows) and a NUL byte are
//     refused before anything is opened.
//   - The check runs on the slash-separated form, so a Windows-style
//     "..\\..\\etc" is refused on Windows, while on Unix a backslash is an
//     ordinary filename byte and is left alone.
//
// No Unicode normalisation happens anywhere: not NFC/NFD, not fullwidth
// separators. A lookalike character is an ordinary byte in an ordinary name, so
// it cannot become a separator or a parent reference.
func toolPath(arg string) (string, error) {
	if strings.TrimSpace(arg) == "" {
		return "", fmt.Errorf("%w: the path is empty", ErrPathRefused)
	}
	if strings.ContainsRune(arg, 0) {
		return "", fmt.Errorf("%w: the path contains a NUL byte", ErrPathOutside)
	}
	if filepath.IsAbs(arg) || filepath.VolumeName(arg) != "" {
		return "", fmt.Errorf("%w: %q is absolute; paths are relative to the run directory", ErrPathOutside, arg)
	}
	if strings.ContainsRune(arg, '\\') {
		// Refused on every OS, so the same call behaves the same way everywhere.
		// On Unix a backslash is an ordinary filename byte, and "\" would be a
		// legal — and confusing — inside name; on Windows it is the separator,
		// and a model that writes "..\\..\\etc" means to traverse. Refusing is
		// the answer to both: a path here uses '/', or it is a mistake.
		return "", fmt.Errorf("%w: %q contains a backslash; use '/' as the separator", ErrPathOutside, arg)
	}
	for _, part := range strings.Split(filepath.ToSlash(arg), "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: %q contains a \"..\" element; parent traversal is refused, not resolved", ErrPathOutside, arg)
		}
	}
	return filepath.Clean(arg), nil
}

// fileToolPath is toolPath for tools that name a file. A trailing separator is
// refused here, before the sandbox cleans it away: write_file("notes/") would
// otherwise create a file called notes, and read_file("notes/") would report a
// confusing read error instead of pointing at list_dir.
func fileToolPath(arg string) (string, error) {
	clean, err := toolPath(arg)
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(filepath.ToSlash(arg), "/") {
		return "", fmt.Errorf("%w: %q ends in a separator; this tool takes a file, not a directory — use list_dir for a directory", ErrPathRefused, arg)
	}
	if clean == "." {
		return "", fmt.Errorf("%w: %q names the run directory, not a file — use list_dir", ErrPathRefused, arg)
	}
	return clean, nil
}

// requireRegular refuses a path that exists and is not a regular file.
//
// The failure it prevents is a hang, not an escape: open(2) on a named pipe
// blocks until a writer appears, so a model that runs `mkfifo p` in the shell
// and then calls read_file("p") would freeze the step — and the run, and the
// node — with no timeout to rescue it. A path that does not exist is left to the
// sandbox, which reports the error the model can act on.
//
// The check is a stat followed by an open, which is a TOCTOU window if some
// other process in the run swaps the file in between. Even then the outcome is
// the hang this exists to prevent, not an escape: the path was already confined.
func requireRegular(sb *Sandbox, clean, name string) error {
	info, err := sb.Stat(clean)
	switch {
	case err != nil:
		return nil // let ReadFile/WriteFile produce the error; ENOENT is normal
	case info.Mode().IsRegular():
		return nil
	default:
		return fmt.Errorf("%w: %q is a %s, and this tool handles regular files only", ErrPathRefused, name, modeName(info.Mode()))
	}
}

// modeName is the human name of a file mode's type, for an error a model can
// understand.
func modeName(mode fs.FileMode) string {
	switch {
	case mode.IsRegular():
		return "file"
	case mode.IsDir():
		return "directory"
	case mode&fs.ModeNamedPipe != 0:
		return "named pipe"
	case mode&fs.ModeSocket != 0:
		return "socket"
	case mode&fs.ModeDevice != 0:
		return "device"
	case mode&fs.ModeSymlink != 0:
		return "symbolic link"
	default:
		return "non-regular file"
	}
}

// --- read_file -------------------------------------------------------------

func runReadFile(ctx context.Context, sb *Sandbox, raw json.RawMessage) (string, error) {
	if err := guardCtx(ctx, ToolReadFile); err != nil {
		return "", err
	}
	if err := guardSandbox(sb, ToolReadFile); err != nil {
		return "", err
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := decodeToolArgs(raw, &args); err != nil {
		return "", err
	}
	clean, err := fileToolPath(args.Path)
	if err != nil {
		return "", err
	}
	if err := requireRegular(sb, clean, args.Path); err != nil {
		return "", err
	}
	data, err := sb.ReadFile(clean)
	if err != nil {
		return "", err
	}
	if reason := binaryReason(data); reason != "" {
		return "", fmt.Errorf("agent: read_file refused %q (%d bytes): it looks binary — %s. This tool returns text only; use shell to inspect or convert it", args.Path, len(data), reason)
	}
	return string(data), nil
}

// --- write_file ------------------------------------------------------------

func runWriteFile(ctx context.Context, sb *Sandbox, raw json.RawMessage) (string, error) {
	if err := guardCtx(ctx, ToolWriteFile); err != nil {
		return "", err
	}
	if err := guardSandbox(sb, ToolWriteFile); err != nil {
		return "", err
	}
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decodeToolArgs(raw, &args); err != nil {
		return "", err
	}
	clean, err := fileToolPath(args.Path)
	if err != nil {
		return "", err
	}
	if len(args.Content) > maxWriteBytes {
		return "", fmt.Errorf("agent: write_file refused %q: %d bytes is over the %d-byte limit; nothing was written", args.Path, len(args.Content), maxWriteBytes)
	}
	// A path that already exists must be a regular file: opening a named pipe
	// for writing blocks until a reader appears, which would hang the run.
	if err := requireRegular(sb, clean, args.Path); err != nil {
		return "", err
	}
	// The sandbox creates no parents (Task 10 was explicit), so the tool does,
	// through the sandbox's own MkdirAll — never a filepath.Join of sb.Dir().
	if err := sb.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
		return "", err
	}
	if err := sb.WriteFile(clean, []byte(args.Content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(args.Content), clean), nil
}

// --- list_dir --------------------------------------------------------------

func runListDir(ctx context.Context, sb *Sandbox, raw json.RawMessage) (string, error) {
	if err := guardCtx(ctx, ToolListDir); err != nil {
		return "", err
	}
	if err := guardSandbox(sb, ToolListDir); err != nil {
		return "", err
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := decodeToolArgs(raw, &args); err != nil {
		return "", err
	}
	name := args.Path
	if strings.TrimSpace(name) == "" {
		name = "."
	}
	clean, err := toolPath(name)
	if err != nil {
		return "", err
	}
	// Checked before open: opening a named pipe blocks in open(2), and this
	// call has no timeout of its own to break out of it.
	if info, statErr := sb.Stat(clean); statErr == nil && !info.IsDir() {
		return "", fmt.Errorf("%w: %q is a %s, not a directory", ErrPathRefused, name, modeName(info.Mode()))
	}
	entries, err := sb.ListDir(clean)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	noun := "entries"
	if len(entries) == 1 {
		noun = "entry"
	}
	fmt.Fprintf(&b, "%d %s in %s\n", len(entries), noun, clean)
	shown := len(entries)
	if shown > maxListEntries {
		shown = maxListEntries
	}
	for _, entry := range entries[:shown] {
		b.WriteString(renderDirEntry(entry))
		b.WriteByte('\n')
	}
	if len(entries) > shown {
		fmt.Fprintf(&b, "... truncated at %d entries; %d more are not shown\n", shown, len(entries)-shown)
	}
	return b.String(), nil
}

// renderDirEntry is one listing line: kind, size, name. The kind comes first so
// a model can scan for directories; the size lets it avoid reading a file it
// would be refused for.
func renderDirEntry(entry os.DirEntry) string {
	kind, size := "other", "-"
	switch {
	case entry.IsDir():
		kind = "dir"
	case entry.Type()&fs.ModeSymlink != 0:
		kind = "link"
	case entry.Type().IsRegular():
		kind = "file"
		if info, err := entry.Info(); err == nil {
			size = strconv.FormatInt(info.Size(), 10)
		}
	}
	name := entry.Name()
	if kind == "dir" {
		name += "/"
	}
	return kind + "\t" + size + "\t" + name
}

// --- shell -----------------------------------------------------------------

// shellInvocation is the command and arguments the tool runs.
//
// The choice is /bin/sh -c (cmd.exe /d /c on Windows), deliberately. The
// alternative — an argv array with no shell — is not safer in the way it looks:
// a shell that is not confined by the kernel can escape any way it likes whether
// or not a shell parsed the command, and a model that writes shell text (which
// is what models do) would turn every pipe and redirect into a failed step, or
// into `sh -c '...'` passed through argv anyway. So the shell is explicit, and
// the confinement story lives where it really lives: the kernel layer, and the
// operator's decision to run an agent at all.
func shellInvocation(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd.exe", []string{"/d", "/c", command}
	}
	return "sh", []string{"-c", command}
}

func runShell(ctx context.Context, sb *Sandbox, raw json.RawMessage) (string, error) {
	if err := guardCtx(ctx, ToolShell); err != nil {
		return "", err
	}
	if err := guardSandbox(sb, ToolShell); err != nil {
		return "", err
	}
	var args struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := decodeToolArgs(raw, &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.Command) == "" {
		return "", errors.New("agent: shell: the command is empty")
	}

	timeout := shellDefaultTimeout
	note := ""
	// Compared in seconds before multiplying: a model-supplied 1<<62 would
	// overflow time.Duration and produce a negative deadline.
	if maxSeconds := int(shellMaxTimeout / time.Second); args.TimeoutSeconds > maxSeconds {
		timeout = shellMaxTimeout
		note = fmt.Sprintf("\n[timeout clamped to %s]", shellMaxTimeout)
	} else if args.TimeoutSeconds > 0 {
		timeout = time.Duration(args.TimeoutSeconds) * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	program, argv := shellInvocation(args.Command)
	cmd, err := sb.Command(runCtx, program, argv...)
	if err != nil {
		return "", err
	}

	out := &cappedWriter{max: maxShellOutput}
	// The same writer for stdout and stderr is safe: os/exec serialises writes
	// when Stdout and Stderr are the same comparable value.
	cmd.Stdout = out
	cmd.Stderr = out
	// A shell that exits while a process it backgrounded still holds the output
	// pipe open would otherwise block Wait — and the step, and the run — until
	// that process dies. WaitDelay bounds the wait; the process is reported as
	// left behind rather than silently orphaned.
	cmd.WaitDelay = shellWaitDelay

	runErr := cmd.Run()

	text := out.String()
	if out.truncated() {
		text += fmt.Sprintf("\n[output truncated at %d bytes]", maxShellOutput)
	}
	text += note

	if err := ctx.Err(); err != nil {
		return text, fmt.Errorf("agent: shell: %w", err)
	}
	switch {
	case runErr == nil:
		return text, nil
	case runCtx.Err() == context.DeadlineExceeded:
		// exec.CommandContext kills the direct child. Its own children are not
		// in that process group, so a backgrounded command outlives the step.
		// Saying so is the honest half of the timeout.
		return text, fmt.Errorf("agent: shell timed out after %s and the shell was killed; a process the command started is not killed with it and may still be running", timeout)
	case errors.Is(runErr, exec.ErrWaitDelay):
		// The command exited 0 but a process it started held the pipe open. The
		// exit status is a success, so it is not a failure — but the output may
		// be incomplete and the model has to know why.
		return text + "\n[a process started by this command is running in the background and held the output open; the output above may be incomplete]", nil
	default:
		return text, fmt.Errorf("agent: shell: %w", runErr)
	}
}

// cappedWriter keeps the first max bytes and counts the rest. Writes always
// report the full length, so the child never blocks on a full buffer and never
// notices it is being truncated.
type cappedWriter struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	room := w.max - w.buf.Len()
	if room <= 0 {
		w.over = true
		return len(p), nil
	}
	if room > len(p) {
		room = len(p)
	}
	w.buf.Write(p[:room])
	if room < len(p) {
		w.over = true
	}
	return len(p), nil
}

func (w *cappedWriter) String() string { return w.buf.String() }

func (w *cappedWriter) truncated() bool { return w.over }

// --- http_get --------------------------------------------------------------

// httpGetClient is shared across calls: it holds no per-call state, its redirect
// policy is static, and reuse keeps the connection pool warm across the steps of
// one run.
var httpGetClient = &http.Client{
	Timeout:       httpGetTimeout,
	CheckRedirect: checkGetRedirect,
}

// checkGetRedirect bounds what a redirect chain can cost the run. Each hop is a
// request the run pays for, an unbounded chain is a denial of service against
// the run's own budget rather than against the node, and a hop from https to
// http would silently drop the TLS guarantee the first request had.
func checkGetRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > httpGetMaxRedirects {
		return fmt.Errorf("stopped after %d redirects", httpGetMaxRedirects)
	}
	switch req.URL.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("refusing a redirect to the %q scheme", req.URL.Scheme)
	}
	if len(via) > 0 {
		if last := via[len(via)-1]; last.URL.Scheme == "https" && req.URL.Scheme == "http" {
			return errors.New("refusing a redirect that downgrades https to http")
		}
	}
	return nil
}

func runHTTPGet(ctx context.Context, sb *Sandbox, raw json.RawMessage) (string, error) {
	if err := guardCtx(ctx, ToolHTTPGet); err != nil {
		return "", err
	}
	if err := guardSandbox(sb, ToolHTTPGet); err != nil {
		return "", err
	}
	// The capability gate is the only network enforcement on a node without
	// kernel confinement, so it comes before the arguments are even parsed.
	if !sb.Network() {
		return "", fmt.Errorf("agent: http_get is refused: the item did not declare the %q capability, so this run may not make outbound requests", "network")
	}
	var args struct {
		URL string `json:"url"`
	}
	if err := decodeToolArgs(raw, &args); err != nil {
		return "", err
	}
	target, err := parseGetURL(args.URL)
	if err != nil {
		return "", err
	}

	reqCtx, cancel := context.WithTimeout(ctx, httpGetTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		return "", fmt.Errorf("agent: http_get: %w", err)
	}
	req.Header.Set("User-Agent", "nsm-agent http_get")

	resp, err := httpGetClient.Do(req)
	if err != nil {
		return "", getTransportError(ctx, reqCtx, err, target.Redacted())
	}
	defer resp.Body.Close()

	// The limit is applied while reading, not after: a body of unknown length is
	// exactly the case that would otherwise be read into memory in full.
	body, err := io.ReadAll(io.LimitReader(resp.Body, httpGetMaxBytes+1))
	if err != nil {
		return "", getTransportError(ctx, reqCtx, fmt.Errorf("reading the response body: %w", err), target.Redacted())
	}
	if len(body) > httpGetMaxBytes {
		return "", fmt.Errorf("agent: http_get %s: the response body exceeds the %d-byte limit; nothing was returned", target.Redacted(), httpGetMaxBytes)
	}

	contentType := resp.Header.Get("Content-Type")
	if reason := binaryReason(body); reason != "" {
		if contentType == "" {
			contentType = "no content type"
		}
		return "", fmt.Errorf("agent: http_get %s: the response is %d bytes of %s and looks binary — %s. This tool returns text only", target.Redacted(), len(body), contentType, reason)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %s\n", resp.Status)
	fmt.Fprintf(&b, "url: %s\n", resp.Request.URL.Redacted())
	if contentType != "" {
		fmt.Fprintf(&b, "content-type: %s\n", contentType)
	}
	fmt.Fprintf(&b, "bytes: %d\n\n", len(body))
	b.Write(body)
	return b.String(), nil
}

// getTransportError names the three failures a caller can tell apart: the run
// was cancelled, the run's own deadline expired, or this tool's deadline did.
// Everything else is what the transport said.
func getTransportError(ctx, reqCtx context.Context, err error, target string) error {
	if parentErr := ctx.Err(); parentErr != nil {
		return fmt.Errorf("agent: http_get %s: %w", target, parentErr)
	}
	if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("agent: http_get %s timed out after %s", target, httpGetTimeout)
	}
	return fmt.Errorf("agent: http_get %s: %w", target, err)
}

// parseGetURL accepts only an absolute http or https URL. Everything else —
// file://, a bare path, a scheme-less string — is refused here, before a
// transport sees it.
func parseGetURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("agent: http_get: the url is empty")
	}
	target, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("agent: http_get: %q is not a URL: %w", raw, err)
	}
	switch target.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("agent: http_get: the %q scheme is not supported; only http and https are", target.Scheme)
	}
	if target.Host == "" {
		return nil, fmt.Errorf("agent: http_get: %q has no host", raw)
	}
	return target, nil
}

// --- finish ----------------------------------------------------------------

func runFinish(ctx context.Context, _ *Sandbox, raw json.RawMessage) (string, error) {
	if err := guardCtx(ctx, ToolFinish); err != nil {
		return "", err
	}
	var args struct {
		Summary string `json:"summary"`
	}
	if err := decodeToolArgs(raw, &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.Summary) == "" {
		return "", fmt.Errorf("agent: finish needs a non-empty summary: it is the only thing the operator will read about this run")
	}
	if len(args.Summary) > maxSummaryBytes {
		return "", fmt.Errorf("agent: finish: the summary is %d bytes and the limit is %d. Shorten it to what was done, where the artifacts are, and what is unverified; nothing was reported", len(args.Summary), maxSummaryBytes)
	}
	// The string is the product; ErrFinished is the control signal. The
	// unmodified text is returned so the model's formatting survives to the
	// board.
	return args.Summary, ErrFinished
}

// --- shared text check -----------------------------------------------------

// binaryReason reports why data cannot be shown as text, or "" when it can.
//
// The test is deliberately simple and deterministic — a NUL byte in the first
// block, or content that is not valid UTF-8 — because this is a budget guard,
// not a format detector. A model that reads a 4 MiB PNG or a 4 MiB archive gets
// nothing it can use and spends the run's context on it; a Latin-1 page is
// refused for the same reason and the message says which test failed, so the
// model can convert it with shell if it really wants the bytes.
func binaryReason(data []byte) string {
	head := data
	if len(head) > binaryProbeBytes {
		head = head[:binaryProbeBytes]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return "it contains a NUL byte in the first block, which UTF-8 text does not"
	}
	if !utf8.Valid(data) {
		return "it is not valid UTF-8"
	}
	return ""
}
