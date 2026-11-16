package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This file is Task 11's acceptance suite: one table per tool, the refusal
// paths, the traversal rule, and the properties that keep the tool layer honest
// now that the kernel layer does not run in this environment (design §19
// weakness 17).
//
// The tests use the sandbox as the fixture, not as the thing under test: the
// sandbox has its own suite.

// toolByName fails the test when a name is missing from the closed set. Every
// call in this file goes through the same table the executor will use.
func toolByName(t *testing.T, name string) Tool {
	t.Helper()
	tool, ok := Find(name)
	if !ok {
		t.Fatalf("Find(%q) = false; the closed set is missing a tool", name)
	}
	return tool
}

// callTool runs one tool with JSON-marshalled arguments. It is the only way the
// tests reach a tool's Run, so every test exercises the same signature the
// executor will.
func callTool(t *testing.T, ctx context.Context, sb *Sandbox, name string, args any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal %s args: %v", name, err)
	}
	return toolByName(t, name).Run(ctx, sb, raw)
}

// mustWrite is fixture setup, not the tool under test.
func mustWrite(t *testing.T, sb *Sandbox, name string, data []byte) {
	t.Helper()
	if dir := filepath.Dir(name); dir != "." {
		if err := sb.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
	}
	if err := sb.WriteFile(name, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", name, err)
	}
}

// TestToolsAreTheSevenAndHaveDescriptions is the brief's schema acceptance: a
// closed set of seven, stable order, every schema valid JSON, every tool carrying
// a description. A model with no description will misuse the tool; that is the
// whole reason this is an assertion and not a comment. M3 added
// request_approval to the six: an escalation is not a capability, but it is still
// a tool the model has to be told about.
func TestToolsAreTheSevenAndHaveDescriptions(t *testing.T) {
	want := []string{ToolReadFile, ToolWriteFile, ToolListDir, ToolShell, ToolHTTPGet, ToolRequestApproval, ToolFinish}
	tools := Tools()
	if len(tools) != len(want) {
		t.Fatalf("Tools() has %d tools, want %d", len(tools), len(want))
	}
	seen := map[string]bool{}
	for i, tool := range tools {
		if tool.Name != want[i] {
			t.Errorf("Tools()[%d].Name = %q, want %q (the order is part of the contract: it keeps the provider payload stable)", i, tool.Name, want[i])
		}
		if seen[tool.Name] {
			t.Errorf("duplicate tool %q in the closed set", tool.Name)
		}
		seen[tool.Name] = true
		if tool.Run == nil {
			t.Errorf("tool %q has no Run", tool.Name)
		}

		if !json.Valid(tool.Schema) {
			t.Errorf("tool %q schema is not valid JSON: %s", tool.Name, tool.Schema)
			continue
		}
		var schema struct {
			Description string                     `json:"description"`
			Type        string                     `json:"type"`
			Properties  map[string]json.RawMessage `json:"properties"`
			Required    []string                   `json:"required"`
		}
		if err := json.Unmarshal(tool.Schema, &schema); err != nil {
			t.Errorf("tool %q schema is not an object: %v", tool.Name, err)
			continue
		}
		if strings.TrimSpace(schema.Description) == "" {
			t.Errorf("tool %q has no description; a model with no description will misuse it", tool.Name)
		}
		// The shell description is prompt text and the only thing the model
		// reads about the tool. It may describe the environment this tool
		// constructs; it may not claim the machine has no credentials, because
		// it does, and the file header says a shell can read them by absolute
		// path. A reassuring sentence here is a false one.
		if tool.Name == ToolShell && strings.Contains(schema.Description, "no credentials") {
			t.Errorf("shell description claims the machine has no credentials; scope the claim to the scrubbed environment")
		}
		if schema.Type != "object" {
			t.Errorf("tool %q schema type = %q, want object", tool.Name, schema.Type)
		}
		if len(schema.Properties) == 0 {
			t.Errorf("tool %q schema declares no properties", tool.Name)
		}
		for _, req := range schema.Required {
			if _, ok := schema.Properties[req]; !ok {
				t.Errorf("tool %q requires %q, which is not a declared property", tool.Name, req)
			}
		}

		def, err := tool.Definition()
		if err != nil {
			t.Errorf("tool %q Definition(): %v", tool.Name, err)
			continue
		}
		if def.Name != tool.Name || strings.TrimSpace(def.Description) == "" || len(def.Schema) == 0 {
			t.Errorf("tool %q Definition() = %+v; want the name, the description and the schema", tool.Name, def)
		}
	}

	if _, ok := Find("delete_everything"); ok {
		t.Error("Find found a tool that is not in the closed set")
	}
	defs, err := Definitions(tools)
	if err != nil {
		t.Fatalf("Definitions(): %v", err)
	}
	if len(defs) != len(want) {
		t.Fatalf("Definitions() returned %d definitions, want %d", len(defs), len(want))
	}
}

// TestDefinitionRefusesAToolWithoutADescription pins the rule rather than
// describing it: a tool added later with a bare schema is an error, not a
// silently undescribed tool.
func TestDefinitionRefusesAToolWithoutADescription(t *testing.T) {
	cases := []struct {
		name   string
		schema string
	}{
		{"no description", `{"type":"object","properties":{}}`},
		{"blank description", `{"description":"   ","type":"object"}`},
		{"invalid JSON", `{"description":`},
	}
	for _, tc := range cases {
		tool := Tool{Name: "later", Schema: json.RawMessage(tc.schema), Run: runReadFile}
		if _, err := tool.Definition(); err == nil {
			t.Errorf("%s: Definition() = nil error; a tool without a description must be refused", tc.name)
		}
	}
}

// TestDecodeToolArgsIsLenientAboutUnknownFields documents the one deliberate
// leniency: a model that repeats a parameter or adds "reasoning" still gets its
// work done, while malformed JSON is refused with a message it can act on.
func TestDecodeToolArgsIsLenientAboutUnknownFields(t *testing.T) {
	sb := newTestSandbox(t, SandboxConfig{})
	mustWrite(t, sb, "a.txt", []byte("ok"))

	out, err := toolByName(t, ToolReadFile).Run(context.Background(), sb,
		json.RawMessage(`{"path":"a.txt","reasoning":"I need to look","path_again":"a.txt"}`))
	if err != nil || out != "ok" {
		t.Fatalf("read_file with unknown fields = (%q, %v); want (\"ok\", nil)", out, err)
	}
	if _, err := toolByName(t, ToolReadFile).Run(context.Background(), sb, json.RawMessage(`{"path":`)); err == nil {
		t.Fatal("read_file with malformed JSON = nil error; want a refusal the model can act on")
	}
	if _, err := toolByName(t, ToolReadFile).Run(context.Background(), sb, json.RawMessage(`null`)); err == nil {
		t.Fatal("read_file with null arguments = nil error; want the required-field refusal")
	}
}

// TestReadFileTool is the per-tool table: success, the refusals that keep it
// inside the run directory, the 4 MiB cap, and the binary refusal (a model that
// reads 4 MiB of PNG into its context burns the run's budget on nothing).
func TestReadFileTool(t *testing.T) {
	sb := newTestSandbox(t, SandboxConfig{})
	mustWrite(t, sb, "notes/hello.txt", []byte("hello world\n"))
	mustWrite(t, sb, "empty.txt", nil)
	mustWrite(t, sb, "png.bin", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"))
	mustWrite(t, sb, "latin1.txt", []byte("caf\xe9 au lait\n"))
	mustWrite(t, sb, "big.txt", make([]byte, maxReadBytes+1))
	mustWrite(t, sb, "utf8.txt", []byte("héllo — 世界\n"))

	cases := []struct {
		name    string
		path    string
		want    string
		wantErr []string // substrings; empty means no error
	}{
		{name: "reads a text file", path: "notes/hello.txt", want: "hello world\n"},
		{name: "reads an empty file", path: "empty.txt", want: ""},
		{name: "reads UTF-8 with a BOM-less accent and CJK", path: "utf8.txt", want: "héllo — 世界\n"},
		{name: "missing file", path: "nope.txt", wantErr: []string{"nope.txt"}},
		{name: "directory", path: "notes", wantErr: []string{"notes"}},
		{name: "directory with a trailing slash", path: "notes/", wantErr: []string{"list_dir"}},
		{name: "the run directory itself", path: ".", wantErr: []string{"list_dir"}},
		{name: "empty path", path: "", wantErr: []string{"empty"}},
		{name: "absolute path", path: "/etc/passwd", wantErr: []string{"absolute"}},
		{name: "binary file", path: "png.bin", wantErr: []string{"NUL", "binary"}},
		{name: "invalid UTF-8", path: "latin1.txt", wantErr: []string{"UTF-8"}},
		{name: "over the 4 MiB cap", path: "big.txt", wantErr: []string{"larger than"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := callTool(t, context.Background(), sb, ToolReadFile, map[string]string{"path": tc.path})
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("read_file(%q) = (%q, nil); want an error containing %q", tc.path, out, tc.wantErr)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("read_file(%q) error = %q; want it to contain %q", tc.path, err, want)
					}
				}
				if out != "" {
					t.Errorf("read_file(%q) returned %q alongside an error; refusals return no content", tc.path, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("read_file(%q): %v", tc.path, err)
			}
			if out != tc.want {
				t.Errorf("read_file(%q) = %q, want %q", tc.path, out, tc.want)
			}
		})
	}
}

// TestWriteFileTool pins parent creation (the sandbox does not create parents;
// the tool must), overwrite semantics, and the size cap that stops a step from
// writing a gigabyte.
func TestWriteFileTool(t *testing.T) {
	t.Run("creates missing parents", func(t *testing.T) {
		sb := newTestSandbox(t, SandboxConfig{})
		out, err := callTool(t, context.Background(), sb, ToolWriteFile, map[string]string{"path": "a/b/c.txt", "content": "deep\n"})
		if err != nil {
			t.Fatalf("write_file: %v", err)
		}
		if !strings.Contains(out, "a/b/c.txt") || !strings.Contains(out, "5 bytes") {
			t.Errorf("write_file output = %q; want it to name the path and the byte count", out)
		}
		got, err := sb.ReadFile("a/b/c.txt")
		if err != nil || string(got) != "deep\n" {
			t.Fatalf("the file read back = (%q, %v); want (\"deep\\n\", nil)", got, err)
		}
	})

	t.Run("overwrites in full", func(t *testing.T) {
		sb := newTestSandbox(t, SandboxConfig{})
		mustWrite(t, sb, "x.txt", []byte("a much longer original"))
		if _, err := callTool(t, context.Background(), sb, ToolWriteFile, map[string]string{"path": "x.txt", "content": "short"}); err != nil {
			t.Fatalf("write_file: %v", err)
		}
		got, err := sb.ReadFile("x.txt")
		if err != nil || string(got) != "short" {
			t.Fatalf("x.txt = (%q, %v); want (\"short\", nil) — an overwrite must truncate", got, err)
		}
	})

	cases := []struct {
		name    string
		path    string
		content string
		wantErr []string
	}{
		{name: "empty path", path: "", content: "x", wantErr: []string{"empty"}},
		{name: "absolute path", path: "/tmp/escaped.txt", content: "x", wantErr: []string{"absolute"}},
		{name: "traversal", path: "../../escaped.txt", content: "x", wantErr: []string{".."}},
		{name: "trailing slash", path: "notes/", content: "x", wantErr: []string{"separator"}},
		{name: "the run directory itself", path: ".", content: "x", wantErr: []string{"list_dir"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := newTestSandbox(t, SandboxConfig{})
			_, err := callTool(t, context.Background(), sb, ToolWriteFile, map[string]string{"path": tc.path, "content": tc.content})
			if err == nil {
				t.Fatalf("write_file(%q) = nil error; want a refusal", tc.path)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("write_file(%q) error = %q; want it to contain %q", tc.path, err, want)
				}
			}
			if tc.name == "trailing slash" {
				if _, statErr := os.Stat(filepath.Join(sb.Dir(), "notes")); statErr == nil {
					t.Error("write_file created a file for a path ending in a separator; the trailing slash must be refused, not cleaned away")
				}
			}
		})
	}

	// Every spelling of "a directory", not just the quoted example: filepath.Clean
	// turns "notes/" and "notes/." into the same legal file name "notes", so a
	// rule that refuses only the first is the "looks fixed but isn't" failure.
	t.Run("refuses every directory spelling", func(t *testing.T) {
		for _, path := range []string{"notes/", "notes/.", "notes/./", "notes//"} {
			sb := newTestSandbox(t, SandboxConfig{})
			_, err := callTool(t, context.Background(), sb, ToolWriteFile, map[string]string{"path": path, "content": "x"})
			if err == nil {
				t.Fatalf("write_file(%q) = nil error; want a refusal", path)
			}
			if !errors.Is(err, ErrPathRefused) {
				t.Errorf("write_file(%q) = %v; want ErrPathRefused", path, err)
			}
			if !strings.Contains(err.Error(), "list_dir") {
				t.Errorf("write_file(%q) error = %q; want it to point at list_dir", path, err)
			}
			if _, statErr := os.Stat(filepath.Join(sb.Dir(), "notes")); statErr == nil {
				t.Errorf("write_file(%q) created a file named notes: the directory spelling was cleaned into a file name", path)
			}
		}
	})

	// The same rule is the shared helper's, so read_file gets it too.
	t.Run("read_file refuses a directory spelling", func(t *testing.T) {
		sb := newTestSandbox(t, SandboxConfig{})
		mustWrite(t, sb, "notes/inner.txt", []byte("x"))
		for _, path := range []string{"notes/", "notes/.", "notes/./"} {
			if _, err := callTool(t, context.Background(), sb, ToolReadFile, map[string]string{"path": path}); err == nil {
				t.Errorf("read_file(%q) = nil error; want a refusal", path)
			} else if !strings.Contains(err.Error(), "list_dir") {
				t.Errorf("read_file(%q) error = %q; want it to point at list_dir", path, err)
			}
		}
	})

	t.Run("over the 4 MiB cap", func(t *testing.T) {
		sb := newTestSandbox(t, SandboxConfig{})
		_, err := callTool(t, context.Background(), sb, ToolWriteFile, map[string]string{"path": "huge.bin", "content": strings.Repeat("a", maxWriteBytes+1)})
		if err == nil {
			t.Fatal("write_file accepted more than the cap")
		}
		if !strings.Contains(err.Error(), "limit") {
			t.Errorf("write_file error = %q; want it to name the limit", err)
		}
		if _, statErr := os.Stat(filepath.Join(sb.Dir(), "huge.bin")); statErr == nil {
			t.Error("a refused write created the file anyway")
		}
	})
}

// TestListDirTool pins the listing format a model reads, the sort order, the
// directory/file/link distinction, and the entry cap.
func TestListDirTool(t *testing.T) {
	t.Run("lists the run directory", func(t *testing.T) {
		sb := newTestSandbox(t, SandboxConfig{})
		mustWrite(t, sb, "b.txt", []byte("12345"))
		mustWrite(t, sb, "a.txt", []byte("x"))
		mustWrite(t, sb, "sub/inner.txt", []byte("y"))

		out, err := callTool(t, context.Background(), sb, ToolListDir, map[string]string{"path": "."})
		if err != nil {
			t.Fatalf("list_dir: %v", err)
		}
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		// Header, then tmp (the sandbox's own directory), sub, a.txt, b.txt in
		// sorted order.
		if len(lines) != 5 {
			t.Fatalf("list_dir returned %d lines:\n%s", len(lines), out)
		}
		if !strings.Contains(lines[0], "4 entries") {
			t.Errorf("header = %q; want the entry count", lines[0])
		}
		wantOrder := []string{"a.txt", "b.txt", "sub/", "tmp/"}
		for i, want := range wantOrder {
			if !strings.Contains(lines[i+1], want) {
				t.Errorf("line %d = %q; want it to contain %q (entries must be sorted by name)", i+1, lines[i+1], want)
			}
		}
		if !strings.Contains(lines[2], "b.txt") || !strings.Contains(lines[2], "5") {
			t.Errorf("b.txt line = %q; want its size", lines[2])
		}
		if !strings.HasPrefix(lines[4], "dir") {
			t.Errorf("tmp line = %q; want the kind first", lines[4])
		}
	})

	t.Run("a trailing slash is fine for a directory", func(t *testing.T) {
		sb := newTestSandbox(t, SandboxConfig{})
		mustWrite(t, sb, "sub/inner.txt", []byte("y"))
		out, err := callTool(t, context.Background(), sb, ToolListDir, map[string]string{"path": "sub/"})
		if err != nil {
			t.Fatalf("list_dir(\"sub/\"): %v", err)
		}
		if !strings.Contains(out, "inner.txt") {
			t.Errorf("list_dir(\"sub/\") = %q; want inner.txt", out)
		}
	})

	t.Run("an omitted path means the run directory", func(t *testing.T) {
		sb := newTestSandbox(t, SandboxConfig{})
		mustWrite(t, sb, "a.txt", []byte("x"))
		out, err := callTool(t, context.Background(), sb, ToolListDir, map[string]string{})
		if err != nil {
			t.Fatalf("list_dir with no path: %v", err)
		}
		if !strings.Contains(out, "a.txt") {
			t.Errorf("list_dir with no path = %q; want the run directory listing", out)
		}
	})

	t.Run("truncates a huge directory", func(t *testing.T) {
		sb := newTestSandbox(t, SandboxConfig{})
		for i := 0; i < maxListEntries+10; i++ {
			if err := os.WriteFile(filepath.Join(sb.Dir(), fmt.Sprintf("e%04d", i)), nil, 0o644); err != nil {
				t.Fatalf("fixture: %v", err)
			}
		}
		out, err := callTool(t, context.Background(), sb, ToolListDir, map[string]string{"path": "."})
		if err != nil {
			t.Fatalf("list_dir: %v", err)
		}
		if !strings.Contains(out, "truncated") {
			t.Errorf("list_dir output of %d entries has no truncation note", maxListEntries+11)
		}
		if got := strings.Count(out, "\n"); got > maxListEntries+3 {
			t.Errorf("list_dir returned %d lines for a capped listing of %d", got, maxListEntries)
		}
	})

	cases := []struct {
		name    string
		path    string
		wantErr []string
	}{
		{name: "missing directory", path: "nope", wantErr: []string{"nope"}},
		{name: "a file", path: "file.txt", wantErr: []string{"file.txt"}},
		{name: "absolute path", path: "/etc", wantErr: []string{"absolute"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := newTestSandbox(t, SandboxConfig{})
			mustWrite(t, sb, "file.txt", []byte("x"))
			_, err := callTool(t, context.Background(), sb, ToolListDir, map[string]string{"path": tc.path})
			if err == nil {
				t.Fatalf("list_dir(%q) = nil error; want a refusal", tc.path)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("list_dir(%q) error = %q; want it to contain %q", tc.path, err, want)
				}
			}
		})
	}
}

// TestShellTool is the shell table. It checks the properties the tool layer can
// still guarantee without a kernel: the cwd is the run directory, the
// environment is the scrubbed one, output is bounded, and the exit status
// reaches the model.
func TestShellTool(t *testing.T) {
	t.Run("captures stdout", func(t *testing.T) {
		sb := newTestSandbox(t, SandboxConfig{})
		out, err := callTool(t, context.Background(), sb, ToolShell, map[string]any{"command": "echo hello"})
		if err != nil {
			t.Fatalf("shell: %v", err)
		}
		if strings.TrimSpace(out) != "hello" {
			t.Errorf("shell output = %q, want hello", out)
		}
	})

	t.Run("combines stderr and reports a non-zero exit", func(t *testing.T) {
		sb := newTestSandbox(t, SandboxConfig{})
		out, err := callTool(t, context.Background(), sb, ToolShell, map[string]any{"command": "echo boom >&2; exit 3"})
		if err == nil {
			t.Fatal("shell with exit 3 = nil error")
		}
		if !strings.Contains(out, "boom") {
			t.Errorf("shell output = %q; the error text on stderr must reach the model with the failure", out)
		}
		if !strings.Contains(err.Error(), "exit status 3") {
			t.Errorf("shell error = %q; want the exit status", err)
		}
	})

	t.Run("runs in the run directory", func(t *testing.T) {
		sb := newTestSandbox(t, SandboxConfig{})
		out, err := callTool(t, context.Background(), sb, ToolShell, map[string]any{"command": "pwd"})
		if err != nil {
			t.Fatalf("shell: %v", err)
		}
		if strings.TrimSpace(out) != sb.Dir() {
			t.Errorf("pwd = %q, want the run directory %q", strings.TrimSpace(out), sb.Dir())
		}
	})

	t.Run("inherits the scrubbed environment", func(t *testing.T) {
		t.Setenv("NSM_TOOLS_LEAK", "leaked-secret")
		sb := newTestSandbox(t, SandboxConfig{})
		out, err := callTool(t, context.Background(), sb, ToolShell, map[string]any{"command": "env"})
		if err != nil {
			t.Fatalf("shell env: %v", err)
		}
		if strings.Contains(out, "NSM_TOOLS_LEAK") || strings.Contains(out, "leaked-secret") {
			t.Errorf("the shell saw a parent variable:\n%s", out)
		}
		if !strings.Contains(out, "HOME="+sb.Dir()) {
			t.Errorf("HOME is not the run directory:\n%s", out)
		}
	})

	t.Run("refuses an empty command", func(t *testing.T) {
		sb := newTestSandbox(t, SandboxConfig{})
		if _, err := callTool(t, context.Background(), sb, ToolShell, map[string]any{"command": "  "}); err == nil {
			t.Fatal("shell with a blank command = nil error")
		}
	})

	t.Run("clamps an absurd timeout instead of overflowing", func(t *testing.T) {
		sb := newTestSandbox(t, SandboxConfig{})
		out, err := callTool(t, context.Background(), sb, ToolShell, map[string]any{
			"command":         "echo ok",
			"timeout_seconds": int64(1) << 62,
		})
		if err != nil {
			t.Fatalf("shell: %v", err)
		}
		if !strings.Contains(out, "ok") {
			t.Errorf("shell output = %q; want the command to have run", out)
		}
		if !strings.Contains(out, "clamped") {
			t.Errorf("shell output = %q; want the clamp to be visible to the model", out)
		}
	})

	t.Run("bounds the output", func(t *testing.T) {
		sb := newTestSandbox(t, SandboxConfig{})
		mustWrite(t, sb, "big.txt", []byte(strings.Repeat("a", maxShellOutput*2)))
		out, err := callTool(t, context.Background(), sb, ToolShell, map[string]any{"command": "cat big.txt"})
		if err != nil {
			t.Fatalf("shell cat: %v", err)
		}
		if !strings.Contains(out, "truncated") {
			t.Errorf("output of %d bytes was not marked truncated", maxShellOutput*2)
		}
		if len(out) > maxShellOutput+256 {
			t.Errorf("shell returned %d bytes for a %d-byte cap", len(out), maxShellOutput)
		}
	})
}

// TestShellTimesOutAndSaysGrandchildrenMaySurvive is the brief's timeout
// refusal. The last assertion is the honest half: the context kills the direct
// child, and a process it backgrounded is not killed, so the message says so.
func TestShellTimesOutAndSaysGrandchildrenMaySurvive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses /bin/sh")
	}
	sb := newTestSandbox(t, SandboxConfig{})
	out, err := callTool(t, context.Background(), sb, ToolShell, map[string]any{
		"command":         "sleep 30 & sleep 30",
		"timeout_seconds": 1,
	})
	if err == nil {
		t.Fatal("shell with a 1s timeout and a 30s command = nil error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("shell error = %q; want it to name the timeout", err)
	}
	if !strings.Contains(err.Error(), "may still be running") {
		t.Errorf("shell error = %q; the timeout kills only the direct child and the message must say so", err)
	}
	if strings.Contains(out, "hello") {
		t.Errorf("shell output = %q", out)
	}
}

// TestShellDoesNotHangWhenABackgroundProcessHoldsTheOutput is the WaitDelay
// property: a shell that exits while a grandchild still holds stdout open must
// not make the tool block until that grandchild dies. Without it, `sleep 1h &`
// would freeze the step and the run's whole budget with it.
func TestShellDoesNotHangWhenABackgroundProcessHoldsTheOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses /bin/sh")
	}
	sb := newTestSandbox(t, SandboxConfig{})

	type result struct {
		out string
		err error
	}
	tool := toolByName(t, ToolShell)
	raw := json.RawMessage(`{"command":"sleep 120 & echo started"}`)
	done := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go func() {
		out, err := tool.Run(ctx, sb, raw)
		done <- result{out, err}
	}()

	select {
	case <-time.After(10 * time.Second):
		t.Fatal("shell did not return while a background process held the output pipe open; exec.Cmd.WaitDelay is what makes this bounded")
	case res := <-done:
		if res.err != nil {
			t.Errorf("shell = %v; a command that exits 0 while a background process holds the pipe is a success with a note, not a failure", res.err)
		}
		if !strings.Contains(res.out, "started") {
			t.Errorf("shell output = %q; want \"started\" before the grandchild was left behind", res.out)
		}
	}
}

// TestHTTPGetTool is the http_get table, starting with the capability gate: no
// network declaration, no request. The hit counter proves the refusal happens
// before the socket, not after.
func TestHTTPGetTool(t *testing.T) {
	t.Run("refused without the network capability", func(t *testing.T) {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			fmt.Fprint(w, "should not be reached")
		}))
		defer srv.Close()

		sb := newTestSandbox(t, SandboxConfig{}) // Network: false
		out, err := callTool(t, context.Background(), sb, ToolHTTPGet, map[string]string{"url": srv.URL})
		if err == nil {
			t.Fatalf("http_get without the network capability = (%q, nil)", out)
		}
		if !strings.Contains(err.Error(), "network") {
			t.Errorf("http_get refusal = %q; want it to name the missing capability", err)
		}
		if got := atomic.LoadInt32(&hits); got != 0 {
			t.Errorf("the server was hit %d times; a refused call must not open a socket", got)
		}
	})

	t.Run("fetches a page", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "the body")
		}))
		defer srv.Close()

		sb := newTestSandbox(t, SandboxConfig{Network: true})
		out, err := callTool(t, context.Background(), sb, ToolHTTPGet, map[string]string{"url": srv.URL})
		if err != nil {
			t.Fatalf("http_get: %v", err)
		}
		for _, want := range []string{"200", "text/plain", srv.URL, "the body"} {
			if !strings.Contains(out, want) {
				t.Errorf("http_get output = %q; want it to contain %q", out, want)
			}
		}
	})

	t.Run("a 404 is an answer, not a transport error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no such page", http.StatusNotFound)
		}))
		defer srv.Close()

		sb := newTestSandbox(t, SandboxConfig{Network: true})
		out, err := callTool(t, context.Background(), sb, ToolHTTPGet, map[string]string{"url": srv.URL})
		if err != nil {
			t.Fatalf("http_get on a 404 returned %v; the model needs the status, not a tool error", err)
		}
		if !strings.Contains(out, "404") || !strings.Contains(out, "no such page") {
			t.Errorf("http_get output = %q; want the status and the body", out)
		}
	})

	t.Run("refuses a body over the cap", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("a", httpGetMaxBytes+1)))
		}))
		defer srv.Close()

		sb := newTestSandbox(t, SandboxConfig{Network: true})
		out, err := callTool(t, context.Background(), sb, ToolHTTPGet, map[string]string{"url": srv.URL})
		if err == nil {
			t.Fatalf("http_get with a %d-byte body = (%d bytes, nil); want a refusal", httpGetMaxBytes+1, len(out))
		}
		if !strings.Contains(err.Error(), "exceeds") {
			t.Errorf("http_get error = %q; want it to name the size limit", err)
		}
	})

	t.Run("refuses a binary body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"))
		}))
		defer srv.Close()

		sb := newTestSandbox(t, SandboxConfig{Network: true})
		_, err := callTool(t, context.Background(), sb, ToolHTTPGet, map[string]string{"url": srv.URL})
		if err == nil {
			t.Fatal("http_get returned a PNG; a binary body burns budget on unreadable bytes")
		}
		if !strings.Contains(err.Error(), "image/png") {
			t.Errorf("http_get error = %q; want it to name the content type", err)
		}
	})

	cases := []struct {
		name    string
		url     string
		wantErr []string
	}{
		{name: "empty", url: "", wantErr: []string{"empty"}},
		{name: "file scheme", url: "file:///etc/passwd", wantErr: []string{"scheme"}},
		{name: "no host", url: "http:///x", wantErr: []string{"host"}},
		{name: "garbage", url: "://", wantErr: []string{"URL"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := newTestSandbox(t, SandboxConfig{Network: true})
			_, err := callTool(t, context.Background(), sb, ToolHTTPGet, map[string]string{"url": tc.url})
			if err == nil {
				t.Fatalf("http_get(%q) = nil error", tc.url)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("http_get(%q) error = %q; want it to contain %q", tc.url, err, want)
				}
			}
		})
	}
}

// TestHTTPGetBoundsRedirects pins the redirect budget: a chain is a request
// amplification the run pays for, so five hops is the ceiling and the refusal
// names it.
func TestHTTPGetBoundsRedirects(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n, max int
		if _, err := fmt.Sscanf(r.URL.Path, "/r/%d", &n); err != nil {
			http.NotFound(w, r)
			return
		}
		fmt.Sscanf(r.URL.Query().Get("max"), "%d", &max)
		if n < max {
			http.Redirect(w, r, fmt.Sprintf("%s/r/%d?max=%d", srv.URL, n+1, max), http.StatusFound)
			return
		}
		fmt.Fprint(w, "landed")
	}))
	defer srv.Close()

	sb := newTestSandbox(t, SandboxConfig{Network: true})
	for _, tc := range []struct {
		name    string
		hops    int
		wantErr bool
	}{
		{name: "no redirect", hops: 0},
		{name: "five redirects", hops: 5},
		{name: "six redirects", hops: 6, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := fmt.Sprintf("%s/r/0?max=%d", srv.URL, tc.hops)
			out, err := callTool(t, context.Background(), sb, ToolHTTPGet, map[string]string{"url": u})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("http_get over %d redirects = (%q, nil); want a refusal", tc.hops, out)
				}
				if !strings.Contains(err.Error(), "redirect") {
					t.Errorf("http_get error = %q; want it to name the redirect limit", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("http_get after %d redirects: %v", tc.hops, err)
			}
			if !strings.Contains(out, "landed") {
				t.Errorf("http_get output = %q; want the final body", out)
			}
		})
	}
}

// TestHTTPGetRefusesADowngradeRedirect tests the policy directly: an https
// request redirected to cleartext http would silently drop the TLS guarantee
// the first request had.
func TestHTTPGetRefusesADowngradeRedirect(t *testing.T) {
	parse := func(raw string) *url.URL {
		t.Helper()
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return u
	}
	cases := []struct {
		name    string
		via     string
		next    string
		wantErr string
	}{
		{name: "https to http", via: "https://example.test/a", next: "http://example.test/b", wantErr: "downgrade"},
		{name: "http to https", via: "http://example.test/a", next: "https://example.test/b"},
		{name: "http to http", via: "http://example.test/a", next: "http://example.test/b"},
		{name: "https to https", via: "https://example.test/a", next: "https://example.test/b"},
		{name: "ftp", via: "http://example.test/a", next: "ftp://example.test/b", wantErr: "scheme"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			via := []*http.Request{{URL: parse(tc.via)}}
			next := &http.Request{URL: parse(tc.next)}
			err := checkGetRedirect(next, via)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkGetRedirect(%s) = %v; want nil", tc.next, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("checkGetRedirect(%s) = %v; want an error containing %q", tc.next, err, tc.wantErr)
			}
		})
	}
}

// TestFinishTool pins the one tool whose argument is the product. The summary
// comes back as the result and ErrFinished is the signal; an empty or oversized
// summary is refused before it reaches the operator.
func TestFinishTool(t *testing.T) {
	cases := []struct {
		name     string
		args     any
		wantOut  string
		wantErr  []string
		wantDone bool
	}{
		{name: "reports the summary", args: map[string]string{"summary": "built the thing; artifact at out/thing.bin"}, wantOut: "built the thing; artifact at out/thing.bin", wantDone: true},
		{name: "preserves the model's formatting", args: map[string]string{"summary": "  done.\n\n- a\n- b  "}, wantOut: "  done.\n\n- a\n- b  ", wantDone: true},
		{name: "empty summary", args: map[string]string{"summary": ""}, wantErr: []string{"summary"}},
		{name: "blank summary", args: map[string]string{"summary": " \n\t "}, wantErr: []string{"summary"}},
		{name: "missing summary", args: map[string]string{"note": "oops"}, wantErr: []string{"summary"}},
		{name: "oversized summary", args: map[string]string{"summary": strings.Repeat("x", maxSummaryBytes+1)}, wantErr: []string{"limit"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// finish needs no sandbox: it does no I/O, and Task 10 was explicit
			// that it must not be routed through one.
			out, err := callTool(t, context.Background(), nil, ToolFinish, tc.args)
			if tc.wantDone {
				if !IsFinish(err) {
					t.Fatalf("finish error = %v; want ErrFinished", err)
				}
				if out != tc.wantOut {
					t.Errorf("finish summary = %q, want %q", out, tc.wantOut)
				}
				return
			}
			if err == nil {
				t.Fatalf("finish = (%q, nil); want a refusal", out)
			}
			if IsFinish(err) {
				t.Fatalf("finish returned ErrFinished for a refused summary; the loop would end with no product")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("finish error = %q; want it to contain %q", err, want)
				}
			}
		})
	}
}

// TestToolsRefusePathTraversal is the brief's named acceptance test. It is not
// enough that a traversal ends up somewhere legal: the model's intent is
// refused, so `errors.Is(err, ErrPathOutside)` holds and nothing is created —
// no `filepath.Clean`, no join that happens to land inside.
func TestToolsRefusePathTraversal(t *testing.T) {
	traversals := []string{
		"../../etc/passwd",
		"a/../../etc/passwd",
		"..",
		"a/../..",
		"./../secret",
		"a/..",
		`..\..\etc\passwd`,
		"../run-1/inside.txt",
	}
	tools := []string{ToolReadFile, ToolWriteFile, ToolListDir}

	sb := newTestSandbox(t, SandboxConfig{})
	// A file just outside the run directory, so a read that "worked" would
	// return recognisable content rather than merely not erroring.
	outside := filepath.Join(filepath.Dir(sb.Dir()), "secret.txt")
	if err := os.WriteFile(outside, []byte("OUTSIDE-SECRET"), 0o600); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	for _, name := range tools {
		for _, path := range traversals {
			t.Run(name+"/"+path, func(t *testing.T) {
				args := map[string]string{"path": path}
				if name == ToolWriteFile {
					args["content"] = "x"
				}
				out, err := callTool(t, context.Background(), sb, name, args)
				if err == nil {
					t.Fatalf("%s(%q) = (%q, nil); traversal must be refused", name, path, out)
				}
				if !errors.Is(err, ErrPathOutside) {
					t.Errorf("%s(%q) error = %v; want ErrPathOutside (refused, not cleaned into a legal path)", name, path, err)
				}
				if strings.Contains(out, "OUTSIDE-SECRET") {
					t.Errorf("%s(%q) returned content from outside the run directory", name, path)
				}
			})
		}
	}

	// The write half, checked on disk: a "clean into something inside" bug would
	// still be an escape if it created the file above the run directory.
	if _, err := callTool(t, context.Background(), sb, ToolWriteFile, map[string]string{"path": "../../escape-marker", "content": "x"}); err == nil {
		t.Fatal("write_file accepted ../../escape-marker")
	}
	for _, p := range []string{
		filepath.Join(sb.Dir(), "..", "escape-marker"),
		filepath.Join(sb.Dir(), "..", "..", "escape-marker"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s exists: the path was cleaned into a legal one instead of being refused", p)
		}
	}

	// An absolute path naming the run directory itself is still absolute, and
	// absolute is refused: the tool never joins sb.Dir() with model input.
	if _, err := callTool(t, context.Background(), sb, ToolReadFile, map[string]string{"path": sb.Dir() + "/secret.txt"}); !errors.Is(err, ErrPathOutside) {
		t.Errorf("read_file with an absolute run-directory path = %v; want ErrPathOutside", err)
	}
}

// TestToolsRefuseSymlinkEscape covers the escape a lexical check cannot see: a
// symlink inside the run directory that points outside it. os.Root resolves
// with openat and refuses; the tools must surface that as an error, never as
// content.
func TestToolsRefuseSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs a privilege this test cannot assume on Windows")
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("OUTSIDE-SECRET"), 0o600); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	sb := newTestSandbox(t, SandboxConfig{})
	if err := os.Symlink(outside, filepath.Join(sb.Dir(), "escape")); err != nil {
		t.Fatalf("fixture symlink: %v", err)
	}

	if out, err := callTool(t, context.Background(), sb, ToolReadFile, map[string]string{"path": "escape/secret.txt"}); err == nil {
		t.Fatalf("read_file read through a symlink out of the run directory: %q", out)
	}
	if _, err := callTool(t, context.Background(), sb, ToolWriteFile, map[string]string{"path": "escape/planted.txt", "content": "x"}); err == nil {
		t.Fatal("write_file wrote through a symlink out of the run directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "planted.txt")); err == nil {
		t.Error("the symlink escape created a file outside the run directory")
	}
	if _, err := callTool(t, context.Background(), sb, ToolListDir, map[string]string{"path": "escape"}); err == nil {
		t.Fatal("list_dir listed through a symlink out of the run directory")
	}

	// A symlink that stays inside must keep working: refusing every symlink
	// would be a different, worse tool. The target is relative because os.Root
	// refuses an absolute symlink target — it cannot tell, without following it,
	// whether the target is inside the root, and refusing is the safe answer.
	if err := os.Symlink("real.txt", filepath.Join(sb.Dir(), "alias.txt")); err != nil {
		t.Fatalf("fixture symlink: %v", err)
	}
	mustWrite(t, sb, "real.txt", []byte("inside"))
	out, err := callTool(t, context.Background(), sb, ToolReadFile, map[string]string{"path": "alias.txt"})
	if err != nil || out != "inside" {
		t.Fatalf("read_file(alias.txt) = (%q, %v); an inside symlink must still read", out, err)
	}
}

// TestToolsDoNoWorkWithACancelledContext is the cancellation contract: a tool
// called with a context that is already cancelled must return before it touches
// anything. For write_file and shell there is a side effect to check; for
// http_get there is a counter.
func TestToolsDoNoWorkWithACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	sb := newTestSandbox(t, SandboxConfig{Network: true})

	cases := []struct {
		name string
		args any
	}{
		{ToolReadFile, map[string]string{"path": "a.txt"}},
		{ToolWriteFile, map[string]string{"path": "cancelled.txt", "content": "x"}},
		{ToolListDir, map[string]string{"path": "."}},
		{ToolShell, map[string]any{"command": "touch shell-ran"}},
		{ToolHTTPGet, map[string]string{"url": srv.URL}},
		{ToolFinish, map[string]string{"summary": "should not finish"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := callTool(t, ctx, sb, tc.name, tc.args)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s with a cancelled context = %v; want context.Canceled", tc.name, err)
			}
			if IsFinish(err) {
				t.Fatal("finish ended the run through a cancelled context")
			}
		})
	}

	if _, err := os.Stat(filepath.Join(sb.Dir(), "cancelled.txt")); err == nil {
		t.Error("write_file wrote a file through a cancelled context")
	}
	if _, err := os.Stat(filepath.Join(sb.Dir(), "shell-ran")); err == nil {
		t.Error("shell ran a command through a cancelled context")
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("http_get opened %d sockets through a cancelled context", got)
	}
}

// TestToolsRefuseSpecialFilesThatWouldBlock covers the hazard a path check
// cannot see: open(2) on a named pipe blocks until a writer appears. A model
// that runs `mkfifo p` in the shell and then calls read_file on it would freeze
// the step, the run, and the node, with no timeout anywhere to rescue it. The
// tools check the mode first, so the call fails instead of hanging.
func TestToolsRefuseSpecialFilesThatWouldBlock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mkfifo is a Unix notion")
	}
	// Deliberately not newTestSandbox: its cleanup calls Close, which takes the
	// sandbox's write lock, while a regressed tool blocks inside open(2) holding
	// the read lock. The cleanup would then wait forever and turn the ten-second
	// watchdog below into a ten-minute package timeout — a regression test whose
	// failure mode is "hangs" is a regression test that gets deleted as flaky.
	// A failure here must be a failure. t.TempDir's own cleanup removes the
	// directory without taking that lock; the blocked goroutine and its open
	// descriptor die with the test process, which is the right trade.
	root := t.TempDir()
	sb, err := NewSandbox(SandboxConfig{Root: root, RunID: "run-1"})
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	pipe := filepath.Join(sb.Dir(), "pipe")
	if out, err := exec.Command("mkfifo", pipe).CombinedOutput(); err != nil {
		t.Skipf("mkfifo is unavailable (%v: %s); the guard is still in the code path", err, out)
	}

	cases := []struct {
		name string
		args any
	}{
		{ToolReadFile, map[string]string{"path": "pipe"}},
		{ToolWriteFile, map[string]string{"path": "pipe", "content": "x"}},
		{ToolListDir, map[string]string{"path": "pipe"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatalf("marshal %s args: %v", tc.name, err)
			}
			tool := toolByName(t, tc.name)
			type result struct {
				out string
				err error
			}
			done := make(chan result, 1)
			go func() {
				out, err := tool.Run(context.Background(), sb, raw)
				done <- result{out, err}
			}()
			select {
			case <-time.After(10 * time.Second):
				t.Fatal("the tool blocked on open(2); a named pipe must be refused, not opened")
			case got := <-done:
				if got.err == nil {
					t.Fatalf("%s on a named pipe = (%q, nil); want a refusal", tc.name, got.out)
				}
				if !errors.Is(got.err, ErrPathRefused) {
					t.Errorf("%s refusal = %v; want ErrPathRefused", tc.name, got.err)
				}
			}
		})
	}

	// The pipe is still a pipe: nothing was opened through it.
	if info, err := os.Lstat(pipe); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("the fixture pipe was replaced (mode %v, err %v)", info, err)
	}

	// Close only when every subtest returned. On the regression path a subtest's
	// goroutine is still blocked in open(2) holding the read lock, and Close
	// would wait for it — the hang this structure exists to avoid. t.Failed()
	// is true when any subtest failed, so the lock holder is left to the process
	// exit instead.
	if !t.Failed() {
		if err := sb.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}
}

// TestToolsTreatUnicodePathsAsNamesInsideTheRunDirectory covers the last
// self-review question. No normalisation is performed — not NFC/NFD, not
// fullwidth separators — so a lookalike character is an ordinary byte in an
// ordinary name and cannot become a separator or a parent reference.
func TestToolsTreatUnicodePathsAsNamesInsideTheRunDirectory(t *testing.T) {
	sb := newTestSandbox(t, SandboxConfig{})

	const fullwidth = "\uFF0Fetc\uFF0Fpasswd" // U+FF0F, not '/'
	out, err := callTool(t, context.Background(), sb, ToolWriteFile, map[string]string{"path": fullwidth, "content": "inside"})
	if err != nil {
		t.Fatalf("write_file(%q): %v", fullwidth, err)
	}
	if !strings.Contains(out, fullwidth) {
		t.Errorf("write_file output = %q; want the literal name", out)
	}
	if _, err := os.Stat(filepath.Join(sb.Dir(), fullwidth)); err != nil {
		t.Errorf("the fullwidth separator was interpreted as path syntax: %v", err)
	}

	const oneDotLeaders = "\u2024\u2024/escaped.txt" // U+2024 twice is not ".."
	if _, err := callTool(t, context.Background(), sb, ToolWriteFile, map[string]string{"path": oneDotLeaders, "content": "inside"}); err != nil {
		t.Fatalf("write_file(%q): %v", oneDotLeaders, err)
	}
	if _, err := os.Stat(filepath.Join(sb.Dir(), "\u2024\u2024", "escaped.txt")); err != nil {
		t.Errorf("the one-dot-leader directory was not created inside the run directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(sb.Dir()), "escaped.txt")); err == nil {
		t.Error("a Unicode lookalike escaped the run directory")
	}

	// NFC and NFD spellings are ordinary distinct byte strings to this layer;
	// the filesystem may fold them, and either way the access stays inside.
	nfc := "caf\u00e9.txt"
	nfd := "cafe\u0301.txt"
	if _, err := callTool(t, context.Background(), sb, ToolWriteFile, map[string]string{"path": nfc, "content": "nfc"}); err != nil {
		t.Fatalf("write_file(%q): %v", nfc, err)
	}
	if _, err := callTool(t, context.Background(), sb, ToolReadFile, map[string]string{"path": nfd}); err != nil {
		t.Logf("read_file(%q) after writing %q: %v (the filesystem does not fold them; still inside)", nfd, nfc, err)
	}
	for _, name := range []string{nfc, nfd} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(sb.Dir()), name)); err == nil {
			t.Errorf("%q appeared outside the run directory", name)
		}
	}
}

// TestRequestApprovalValidatesTheQuestion is the tool-layer half of the board's
// rule that an approval must name its action. A model that emits an empty action
// or an empty choice gets a tool error and continues the run; it must not end the
// run with a question nobody can answer.
func TestRequestApprovalValidatesTheQuestion(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no action", map[string]any{"reason": "because"}, "needs both an action and a reason"},
		{"blank action", map[string]any{"action": "   ", "reason": "because"}, "needs both an action and a reason"},
		{"no reason", map[string]any{"action": "delete the cache"}, "needs both an action and a reason"},
		{"blank option", map[string]any{"action": "a", "reason": "r", "options": []string{"ok", "  "}}, "option 2 is empty"},
		{"too many options", map[string]any{"action": "a", "reason": "r",
			"options": []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}}, "limit is 10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := callTool(t, context.Background(), nil, ToolRequestApproval, tc.args)
			if err == nil {
				t.Fatalf("request_approval accepted %v", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
			var need *ApprovalNeeded
			if errors.As(err, &need) {
				t.Fatal("an invalid request ended the run; the model must get a tool error and continue")
			}
		})
	}

	// The valid case: values are trimmed and the run ends with the request.
	_, err := callTool(t, context.Background(), nil, ToolRequestApproval, map[string]any{
		"action": "  delete /var/cache/app ", "reason": " needs the disk ", "options": []string{" delete it "},
	})
	var need *ApprovalNeeded
	if !errors.As(err, &need) {
		t.Fatalf("valid request returned %v, want an ApprovalNeeded signal", err)
	}
	if need.Request.Action != "delete /var/cache/app" || need.Request.Reason != "needs the disk" {
		t.Fatalf("request = %+v, want trimmed action and reason", need.Request)
	}
	if len(need.Request.Options) != 1 || need.Request.Options[0] != "delete it" {
		t.Fatalf("options = %v, want the trimmed option", need.Request.Options)
	}
}
