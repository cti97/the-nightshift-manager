package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

const itemUsage = `usage: nsm item tree <id> [--board URL]

Prints one item and everything an agent filed under it. A node needs no credential for this: it is an
operator view, so it uses NSM_ADMIN_TOKEN like the rest of the admin surface.`

// itemCommand is the operator's view of agent-filed work (M7 T4). The same data is on the item page;
// this is for a terminal and for scripts, and it prints a tree the way a person reads one.
func itemCommand(args []string, out, errOut io.Writer) int {
	if len(args) == 0 || args[0] != "tree" {
		fmt.Fprintln(errOut, itemUsage)
		return 2
	}
	if len(args) < 2 {
		fmt.Fprintln(errOut, itemUsage)
		return 2
	}
	// The id comes first, then flags: package flag stops at the first positional, so `tree <id>
	// --board URL` would otherwise leave --board unparsed and silently use the default board.
	id := args[1]
	fs := flag.NewFlagSet("item tree", flag.ContinueOnError)
	fs.SetOutput(errOut)
	baseURL := fs.String("board", defaultBoard, "board base URL")
	if err := fs.Parse(args[2:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errOut, itemUsage)
		return 2
	}
	token, ok := os.LookupEnv("NSM_ADMIN_TOKEN")
	if !ok || token == "" {
		fmt.Fprintln(errOut, "NSM_ADMIN_TOKEN is required: the tree is an operator view")
		return 2
	}
	var tree board.ItemNode
	err := adminCall(context.Background(), "GET", *baseURL, "/admin/items/"+id, token, nil, &tree)
	if err != nil {
		fmt.Fprintf(errOut, "item tree: %v\n", err)
		var ae *adminError
		if errors.As(err, &ae) {
			return exitFor(ae)
		}
		return 1
	}
	printNode(out, tree, 0)
	fmt.Fprintf(out, "\n%d item(s) under this one\n", board.CountDescendants(tree))
	return 0
}

// printNode writes one line per item, indented by its depth in the TREE rather than by its stored
// depth: a re-parented or hand-edited row would otherwise render at a misleading indent.
func printNode(out io.Writer, n board.ItemNode, level int) {
	indent := ""
	for i := 0; i < level; i++ {
		indent += "  "
	}
	who := n.CreatedBy
	if n.FiledBy != "" {
		who = fmt.Sprintf("%s via run %.12s", n.CreatedBy, n.FiledBy)
	}
	fmt.Fprintf(out, "%s%s [%s] — %s\n", indent, n.Title, n.Status, who)
	if n.Parked != nil {
		fmt.Fprintf(out, "%s  waiting on a human: %s — %s\n", indent, n.Parked.Action, n.Parked.Reason)
	}
	for _, c := range n.Children {
		printNode(out, c, level+1)
	}
}
