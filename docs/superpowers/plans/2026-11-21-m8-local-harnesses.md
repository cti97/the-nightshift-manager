# M8 — local harnesses

Spec §6 (capabilities), §7 (roles), §8 (dynamic capabilities), §10 (sandbox), §15 (API), §18 (build
order).

A "local harness" is the thing an operator already uses to get work done: Claude Code, Codex CLI, a
DeepSeek Harness session, an editor's agent panel, or a shell script. Today those run outside the
board, so the work they do has no row, no lease, no price and no audit — the exact four things this
project exists to provide.

M8 makes a harness a **node**: it claims items, holds a lease, escalates when it needs a human, and
reports what it did. The manager does not learn any harness's API, and no harness learns the board's.

## The shape: a third executor, not a second loop

`Executor.Run(ctx, item) (Result, error)` is the boundary that already exists. `AgenticExecutor`
runs this repo's tool loop; `ReflexExecutor` makes one model call. A `CommandExecutor` runs the
operator's command with the item's brief in front of it:

```
nsm agent run --node laptop-claude --executor command \
  --run-cmd 'claude -p "$(cat $NSM_BRIEF_FILE)"' --harness claude
```

Everything around the command is already built: heartbeats keep the lease alive while it runs, the
reaper takes the item back if the process dies, escalation parks it, the report settles it. M8 adds
the command, the environment, and the two rules that keep it honest.

## Rule 1 — a harness is not a sandbox

The built-in executor enforces confinement inside this process: an item that needs `sandbox:seatbelt`
gets a seatbelt. A command executor cannot promise that. `claude -p` may sandbox its own tools or may
not; the board cannot tell, and must not claim it can.

So a command node declares `harness:<name>` and **never** `sandbox:<profile>`. Placement already
refuses an item whose `needs` include a capability the node did not declare, which means
sandbox-requiring work can never land on a harness — not by policy in a handler, but by the same
subset check that protects every other placement. The test that matters is the negative one:
an item needing `sandbox:seatbelt` is not placed on a harness node that is otherwise the only
candidate.

If a harness someday proves its confinement (a probe, as `sandbox:<profile>` does today), the way to
add it is a probe, not a flag.

## Rule 2 — unknown tokens are not zero

The board's ledger, its thread reservations and `spend/today` all assume a number. A harness reports
what it reports: often nothing.

Recording 0 would be a lie that makes every budget unbinding: a hundred `claude -p` runs would
show as free.
So the report gains a third state. `ReportRequest.TokenSource` is `reported`, `estimated` (the board's
own `1 + len/4`, as the reserve already uses) or `unknown`. `unknown` still settles the reservation
(it has to: the item is done, the money is spent) but it settles at the estimate and marks the ledger
row, so `spend/today` can say "3 runs unmetered" instead of pretending.

This is the one place where M8 adds a concept to the board rather than reusing one, and it is worth
it: a ledger that silently counts unknown as zero is worse than no ledger.

## Tasks

### T1 — `harness:<name>` in the vocabulary

`internal/caps`: a `harness:` prefix, validated like `lan:` (one word after the colon), declared by a
node and requireable by an item. Placement needs no change — it is a subset check — but the *refusal*
when a node omits it is worth a test.

Acceptance: `TestHarnessIsOneWord`, `TestAnItemNeedingAHarnessIsNotPlacedOnANodeWithoutIt`,
`TestAHarnessNodeCannotDeclareASandboxProfile`.

### T2 — `CommandExecutor`

`internal/agent/command.go`: materialize the brief into a per-run workspace
(`<sandbox-root>/<run-id>/`), export `NSM_ITEM_ID`, `NSM_RUN_ID`, `NSM_ATTEMPT`, `NSM_BRIEF_FILE`,
`NSM_WORKDIR`, `NSM_TITLE`; run the command with `sh -c`; capture stdout+stderr to a bounded log
(64 KiB tail, same as the probe); map exit 0 → done with the log's tail as the summary, non-zero →
failed with the exit code and the tail.

The command inherits the agent's environment except the node token: a harness has no business
reporting its own results, and a leaked token in a subprocess is a leaked credential.

Acceptance: `TestTheBriefIsInTheHarnessWorkspace`, `TestANonZeroExitIsAFailedRun`,
`TestTheNodeTokenIsNotInTheHarnessEnvironment`, `TestTheCommandOutputIsBounded`,
`TestTheWorkspaceIsPerRun`.

### T3 — in-run helpers: `nsm task`

A harness that hits a decision it cannot make must be able to ask, and a wrapper command cannot parse
its way to that. `nsm task escalate --action deploy --reason "…"` reads `NSM_RUN_ID` and
`NSM_NODE_TOKEN` from the environment, calls the board, and exits 0 once the item is parked;
`nsm task thread --say "…"` posts to the item's thread if it has one.

This is the one part of M8 that gives a subprocess a board credential, and it is deliberately the
narrowest possible: escalate and post, no claim, no report, no admin.

Acceptance: `TestTaskEscalateParksItsOwnRun`, `TestTaskEscalateNeedsTheRunEnvironment`,
`TestTaskThreadPostsAsTheItemsNode`.

### T4 — the MCP shim

For a tool-calling harness, the natural integration is not a wrapper command but tools:
`nsm mcp serve` speaks MCP over stdio (JSON-RPC 2.0, stdlib only) and exposes `board_claim`,
`board_start`, `board_report`, `board_escalate`, `board_thread`. A harness with MCP support configures
one server and pulls work as tool calls.

This is the largest task in M8 and the most optional: T2 already covers every harness that can run a
command. It earns its place because a harness that *chooses* when to claim is a better citizen on an
interactive machine than one told to run a command.

Acceptance: `TestMCPHandshakeAndToolList`, `TestAClaimThroughMCPIsTheSameClaim`,
`TestMCPReportsATerminalRefusalAsAToolError`, `TestMCPServesNoAdminTools`.

### T5 — proof

A real harness (Claude Code, or a scripted stand-in if the CLI is unavailable) takes an item through
claim → run → escalate via `nsm task` → operator approves → the same harness finishes it. The proof
records the board's view: one node, two attempts, one grant, and an unmetered run marked as such.

## What M8 does not do

- **No per-harness API keys in the manager.** The harness holds its own credentials; the board never
  sees them, which is also why it cannot count their tokens.
- **No sandbox guarantee for a harness.** Rule 1. A harness that wants sandbox-requiring work must
  become a probe.
- **No interactive approvals inside the terminal.** The decision happens in the operator's UI or CLI;
  the harness waits for the item to come back, which is what escalation already does.
- **No new board routes** unless T3 or T4 genuinely needs one. The protocol is unchanged: a harness
  is a node.

## Why this is a milestone and not a script

A wrapper script that claims and runs a command is an afternoon. What makes it a milestone is the two
rules: without Rule 1 the board's containment story quietly stops being true, and without Rule 2 its
ledger does. Both are about what the board is allowed to *claim*, which is the part worth planning.
