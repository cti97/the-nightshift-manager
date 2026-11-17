package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/agent"
	"github.com/cti97/the-nightshift-manager/internal/board"
	"github.com/cti97/the-nightshift-manager/internal/provider"
	"github.com/cti97/the-nightshift-manager/internal/version"
)

// reaperEvery is how often the manager sweeps expired leases (spec §8). It
// lives beside the process that starts the loop, not in the board package,
// because the cadence is an operational choice.
const reaperEvery = 30 * time.Second

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		usage(errOut)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(out, version.String())
		return 0
	case "manager":
		if len(args) < 2 || args[1] != "serve" {
			usage(errOut)
			return 2
		}
		return managerServe(args[2:], errOut)
	case "agent":
		if len(args) < 2 || args[1] != "run" {
			usage(errOut)
			return 2
		}
		return agentRun(args[2:], errOut)
	case "node":
		if len(args) < 2 || args[1] != "register" {
			usage(errOut)
			return 2
		}
		return nodeRegister(args[2:], out, errOut)
	case "approvals":
		return approvalsList(args[1:], out, errOut)
	case "approve":
		return decideCommand(board.Approve, args[1:], out, errOut)
	case "deny":
		return decideCommand(board.Deny, args[1:], out, errOut)
	default:
		usage(errOut)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: nsm <version|manager|agent|node|approvals|approve|deny> [args]")
}

// managerTokens reads both credentials from the environment, for the same
// reason every other credential here does: a secret on a command line is
// visible in ps and in shell history.
//
// The two must differ. NSM_ENROLL_TOKEN exists so a node never carries the
// admin credential — it can register and nothing else — and setting both to one
// value would quietly hand every enrolling machine the keys to the board.
func managerTokens(errOut io.Writer) (admin, enroll string, ok bool) {
	admin = os.Getenv("NSM_ADMIN_TOKEN")
	if admin == "" {
		fmt.Fprintln(errOut, "NSM_ADMIN_TOKEN is required")
		return "", "", false
	}
	enroll = os.Getenv("NSM_ENROLL_TOKEN")
	if enroll == "" {
		fmt.Fprintln(errOut, "NSM_ENROLL_TOKEN is required (the credential that may register nodes)")
		return "", "", false
	}
	if enroll == admin {
		fmt.Fprintln(errOut, "NSM_ENROLL_TOKEN must differ from NSM_ADMIN_TOKEN: an enrolling node must not hold the admin credential")
		return "", "", false
	}
	return admin, enroll, true
}

// managerServe reads the token hash from the environment so the plaintext is
// never on a command line (spec §6 of AGENTS.md: names, never values).
func managerServe(args []string, errOut io.Writer) int {
	dbPath := flag.NewFlagSet("manager serve", flag.ContinueOnError)
	dbPath.SetOutput(errOut)
	addr := dbPath.String("addr", "127.0.0.1:8081", "listen address")
	path := dbPath.String("db", "nsm.db", "sqlite path")
	if err := dbPath.Parse(args); err != nil {
		return 2
	}
	admin, enroll, ok := managerTokens(errOut)
	if !ok {
		return 2
	}
	db, err := board.Open(*path)
	if err != nil {
		fmt.Fprintf(errOut, "open board: %v\n", err)
		return 1
	}
	defer db.Close()

	srv := board.NewServer(db, board.SystemClock(), provider.NewRegistry(), board.HashToken(admin), board.HashToken(enroll))
	fmt.Fprintf(errOut, "nsm manager listening on %s (db %s)\n", *addr, *path)
	// The reaper is the only thing that expires a lease: without it a node that
	// dies mid-run holds its item forever. It lives for the life of the process;
	// a graceful shutdown path is a later task, so the context is not cancelled.
	go srv.RunReaper(context.Background(), reaperEvery)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		fmt.Fprintf(errOut, "serve: %v\n", err)
		return 1
	}
	return 0
}

// agentRun is `nsm agent run`. The node token comes from the environment for
// the same reason the admin token does: a credential on a command line is
// visible in ps and in shell history.
func agentRun(args []string, errOut io.Writer) int {
	cfg, ok := parseAgentConfig(args, errOut)
	if !ok {
		return 2
	}
	a := agent.New(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(errOut, "nsm agent %s: board %s\n", cfg.NodeID, cfg.BaseURL)
	if err := a.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(errOut, "agent: %v\n", err)
		return 1
	}
	return 0
}

// nodeRegister is `nsm node register`: the node side of enrollment, and the
// only call a node makes before it has a node token.
//
// The enrollment credential comes from the environment for the same reason
// NSM_NODE_TOKEN does: a secret on a command line is visible in ps and in shell
// history. The issued token goes to stdout, once, because that is the only time
// the board will reveal it — the operator stores it as NSM_NODE_TOKEN.
func nodeRegister(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("node register", flag.ContinueOnError)
	fs.SetOutput(errOut)
	baseURL := fs.String("board", "http://127.0.0.1:8081", "board base URL")
	name := fs.String("name", "", "node name, unique on the board")
	role := fs.String("role", "executor", "node role: executor or steward")
	provides := fs.String("provides", "", "comma-separated declared capabilities (e.g. agentic,vision)")
	reach := fs.String("reach", "", "comma-separated reach classes this node can use (host,lan,wan)")
	concurrency := fs.Int("concurrency", 1, "runs this node may hold at once")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" {
		fmt.Fprintln(errOut, "--name is required")
		return 2
	}
	enrollToken := os.Getenv("NSM_ENROLL_TOKEN")
	if enrollToken == "" {
		fmt.Fprintln(errOut, "NSM_ENROLL_TOKEN is required to register a node")
		return 2
	}
	declared, err := listFlag("provides", *provides)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	reaches, err := listFlag("reach", *reach)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}

	resp, err := agent.Enroll(context.Background(), *baseURL, enrollToken, board.RegisterRequest{
		Name:             *name,
		Role:             *role,
		ProvidesDeclared: declared,
		ReachClasses:     reaches,
		Concurrency:      *concurrency,
	})
	if err != nil {
		fmt.Fprintf(errOut, "register: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "node_id: %s\n", resp.NodeID)
	fmt.Fprintf(out, "token: %s\n", resp.Token)
	fmt.Fprintln(errOut, "store the token as NSM_NODE_TOKEN for `nsm agent run`; the board does not show it again")
	return 0
}

// parseAgentConfig turns flags and the environment into what the loop needs.
// It is separate from agentRun so the flags can be tested without starting a
// goroutine that talks to a board forever.
func parseAgentConfig(args []string, errOut io.Writer) (agent.Config, bool) {
	fs := flag.NewFlagSet("agent run", flag.ContinueOnError)
	fs.SetOutput(errOut)
	baseURL := fs.String("board", "http://127.0.0.1:8081", "board base URL")
	nodeID := fs.String("node", "", "node id, as registered with the board")
	provides := fs.String("provides", "", "comma-separated live capabilities (e.g. agentic,vision)")
	reach := fs.String("reach", "", "comma-separated reach classes this node can actually use (host,lan,wan)")
	sandboxRoot := fs.String("sandbox-root", "", "absolute path under which each run gets a disposable directory (required)")
	if err := fs.Parse(args); err != nil {
		return agent.Config{}, false
	}
	if *nodeID == "" {
		fmt.Fprintln(errOut, "--node is required")
		return agent.Config{}, false
	}
	token := os.Getenv("NSM_NODE_TOKEN")
	if token == "" {
		fmt.Fprintln(errOut, "NSM_NODE_TOKEN is required")
		return agent.Config{}, false
	}
	if *sandboxRoot == "" {
		fmt.Fprintln(errOut, "--sandbox-root is required: name the absolute directory under which each run gets its own disposable directory")
		return agent.Config{}, false
	}
	if !filepath.IsAbs(*sandboxRoot) {
		fmt.Fprintf(errOut, "--sandbox-root %q must be an absolute path\n", *sandboxRoot)
		return agent.Config{}, false
	}
	providesLive, err := listFlag("provides", *provides)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return agent.Config{}, false
	}
	reaches, err := listFlag("reach", *reach)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return agent.Config{}, false
	}
	if len(reaches) == 0 {
		// Not defaulted. Claiming a reach class the node does not have lets
		// placement hand it work it cannot actually do; the failure then lands
		// after the claim. Guessing capabilities is what --provides must not do
		// either, and reach is the more dangerous of the two.
		fmt.Fprintln(errOut, "--reach is required: name the reach classes this node can actually use (host,lan,wan)")
		return agent.Config{}, false
	}
	if len(providesLive) == 0 {
		// Not defaulted: a node that provides nothing is a legitimate (if
		// useless) configuration, and guessing a capability would let it claim
		// work it cannot do. Say what it means instead.
		fmt.Fprintln(errOut, "warning: --provides is empty; this node will claim nothing")
	}

	// The token is read once and then removed from this process's environment.
	// What that buys is inheritance, not secrecy: an ordinary child process
	// started after this point does not receive the token in its environment.
	// It is NOT what scrubs the sandbox: Sandbox.Command builds its children's
	// environment from a fixed list (sandboxEnv), so a command the shell tool
	// starts gets a constructed environment rather than this process's, whether
	// or not the variable was unset. The one value sandboxEnv reads from the
	// parent is the Windows SystemRoot path, which is not a credential. The
	// Unsetenv and the sandbox are independent layers. It also does NOT hide the
	// token from the process table: /proc/<pid>/environ reports the exec-time
	// environment block and unsetenv(3) only edits the in-process environ, so on
	// Linux a process the kernel permits to read us can still see it (and ps e
	// where permitted). This is defense in depth, not the sandbox.
	_ = os.Unsetenv("NSM_NODE_TOKEN")

	return agent.Config{
		BaseURL: *baseURL,
		Token:   token,
		NodeID:  *nodeID,
		// The node executes with the real loop. The endpoint and model arrive
		// per claim (placement chose them); the registry constructs the adapter
		// from the claim's config, and the credential is resolved by name from
		// this process's environment. The sandbox root is the flag, not a
		// second configuration: it is the one thing the claim cannot supply.
		Executor: &agent.AgenticExecutor{
			Registry:    provider.NewRegistry(),
			SandboxRoot: *sandboxRoot,
		},
		Protocol: board.ProtocolMax,
		Provides: providesLive,
		Reaches:  reaches,
	}, true
}

// listFlag parses a comma-separated flag value. An empty value is an empty
// list, but an empty entry inside a list ("a,,b") is a typo and fails rather
// than being silently dropped.
func listFlag(name, value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("--%s: empty entry in %q", name, value)
		}
		out = append(out, p)
	}
	return out, nil
}

// adminToken reads the operator's credential from the environment. It is never a
// flag: a secret on a command line is visible in ps and in shell history, which is
// the rule the manager and the node already follow.
func adminToken(errOut io.Writer) (string, bool) {
	tok := os.Getenv("NSM_ADMIN_TOKEN")
	if tok == "" {
		fmt.Fprintln(errOut, "NSM_ADMIN_TOKEN is required (the admin credential; never pass it as a flag)")
		return "", false
	}
	return tok, true
}

// adminCall performs one admin request and decodes the reply. Accept is JSON on
// purpose: these commands are scripts with a person at the keyboard, and the
// approvals page's HTML is for a browser.
func adminCall(ctx context.Context, method, baseURL, path, token string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 35 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// approvalsList is `nsm approvals`: the queue, for a terminal. An empty queue
// prints [] rather than a blank line, so a script and a person both see that the
// answer is "nothing", not "the command did nothing".
func approvalsList(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("approvals", flag.ContinueOnError)
	fs.SetOutput(errOut)
	baseURL := fs.String("board", "http://127.0.0.1:8081", "board base URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	token, ok := adminToken(errOut)
	if !ok {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()

	var list []board.Approval
	if err := adminCall(ctx, http.MethodGet, *baseURL, "/admin/approvals", token, nil, &list); err != nil {
		fmt.Fprintf(errOut, "approvals: %v\n", err)
		return 1
	}
	if len(list) == 0 {
		fmt.Fprintln(out, "[]")
		return 0
	}
	fmt.Fprintf(out, "%d waiting\n", len(list))
	for _, a := range list {
		action := a.Action
		if action == "" {
			action = "(no action recorded)"
		}
		fmt.Fprintf(out, "%s  %s  (asked %s by %s)\n", a.ItemID, action, humanWait(a.RequestedAt), a.RequestedBy)
		if a.Reason != "" {
			fmt.Fprintf(out, "    why: %s\n", a.Reason)
		}
	}
	return 0
}

// decideCommand is `nsm approve` and `nsm deny`. The note is optional for an
// approval (and the command says so, because a decision without one is harder to
// audit later) and required for a denial, where the note IS the reason recorded on
// the cancelled item (D7).
func decideCommand(decision string, args []string, out, errOut io.Writer) int {
	name := decision
	// The plan's syntax is `nsm approve <id> [--note]`, and flag.Parse stops at
	// the first positional argument: without splitting the id out first, every
	// flag after it is read as a second id and the command refuses its own
	// documented form.
	id, flags, err := splitIDAndFlags(args)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errOut)
	baseURL := fs.String("board", "http://127.0.0.1:8081", "board base URL")
	note := fs.String("note", "", "note recorded with the decision (required for deny)")
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	if id == "" {
		fmt.Fprintf(errOut, "usage: nsm %s <item-id> [--note \"...\"]\n", name)
		return 2
	}
	itemID := id
	if decision == board.Deny && strings.TrimSpace(*note) == "" {
		fmt.Fprintln(errOut, "--note is required for deny: it is the reason recorded on the item a week from now")
		return 2
	}
	if decision == board.Approve && strings.TrimSpace(*note) == "" {
		fmt.Fprintln(errOut, "warning: no --note given; a decision without a note is harder to audit later")
	}
	token, ok := adminToken(errOut)
	if !ok {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()

	var res board.DecisionResult
	err = adminCall(ctx, http.MethodPost, *baseURL, "/admin/approvals/"+url.PathEscape(itemID), token,
		board.DecisionRequest{Decision: decision, Note: *note}, &res)
	if err != nil {
		fmt.Fprintf(errOut, "%s %s: %v\n", name, itemID, err)
		return 1
	}
	fmt.Fprintf(out, "%s: %s -> %s\n", name, res.ItemID, res.Status)
	return 0
}

// humanWait renders how long a request has waited, against THIS process's clock.
// The board's own clock decides leases; this one decides what "7m" means to the
// person reading a terminal, and they are allowed to be the same machine.
func humanWait(requestedAt int64) string {
	if requestedAt <= 0 {
		return "at an unknown time"
	}
	d := time.Since(time.Unix(requestedAt, 0))
	switch {
	case d < time.Minute:
		return "less than a minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm ago", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh ago", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// splitIDAndFlags separates the item id from the flags so that the documented
// `nsm approve <id> --note "..."` form works: flag.Parse stops at the first
// positional argument, so the id must come out before the flags are parsed. Both
// flags this command has take a value, which is what makes the pairing below
// unambiguous; a dash-prefixed value would have to be written as --flag=value.
func splitIDAndFlags(args []string) (string, []string, error) {
	var id string
	flags := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		if id != "" {
			return "", nil, fmt.Errorf("unexpected argument %q: one item id is enough", a)
		}
		id = a
	}
	return id, flags, nil
}
