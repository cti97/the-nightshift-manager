package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

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
		fmt.Fprintf(errOut, "nsm %s: not implemented yet\n", args[0])
		return 2
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
