package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/agent"
	"github.com/cti97/the-nightshift-manager/internal/board"
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
	default:
		usage(errOut)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: nsm <version|manager|agent> [args]")
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
	admin := os.Getenv("NSM_ADMIN_TOKEN")
	if admin == "" {
		fmt.Fprintln(errOut, "NSM_ADMIN_TOKEN is required")
		return 2
	}
	db, err := board.Open(*path)
	if err != nil {
		fmt.Fprintf(errOut, "open board: %v\n", err)
		return 1
	}
	defer db.Close()

	srv := board.NewServer(db, board.SystemClock(), board.HashToken(admin))
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

// parseAgentConfig turns flags and the environment into what the loop needs.
// It is separate from agentRun so the flags can be tested without starting a
// goroutine that talks to a board forever.
func parseAgentConfig(args []string, errOut io.Writer) (agent.Config, bool) {
	fs := flag.NewFlagSet("agent run", flag.ContinueOnError)
	fs.SetOutput(errOut)
	baseURL := fs.String("board", "http://127.0.0.1:8081", "board base URL")
	nodeID := fs.String("node", "", "node id, as registered with the board")
	provides := fs.String("provides", "", "comma-separated live capabilities (e.g. agentic,vision)")
	reach := fs.String("reach", "host,lan,wan", "comma-separated reach classes")
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
	if len(providesLive) == 0 {
		// Not defaulted: a node that provides nothing is a legitimate (if
		// useless) configuration, and guessing a capability would let it claim
		// work it cannot do. Say what it means instead.
		fmt.Fprintln(errOut, "warning: --provides is empty; this node will claim nothing")
	}

	return agent.Config{
		BaseURL: *baseURL,
		Token:   token,
		NodeID:  *nodeID,
		// M2 replaces the stub with the agentic executor. Until then the loop is
		// real and the work is not.
		Executor: agent.StubExecutor{},
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
