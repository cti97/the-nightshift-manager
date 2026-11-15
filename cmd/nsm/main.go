package main

import (
	"fmt"
	"io"
	"os"

	"github.com/cti97/the-nightshift-manager/internal/version"
)

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
	case "manager", "agent":
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
