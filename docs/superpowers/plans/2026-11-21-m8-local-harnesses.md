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

## What building it changed

- **T3's premise was false.** There is no narrow board credential for a subprocess; the exit-code
  signal replaces it (see T3).
- **The heartbeat had to learn what an executor does.** It appended `sandbox:<profile>` for every
  node, so a harness node claimed confinement it cannot deliver and the board (correctly) refused its
  heartbeats. `Confines()` is now part of the contract, and a node advertises only what its executor
  will actually do.
- **Rule 2 needed a column, not just a promise.** `runs.token_source` plus `unmetered_runs` in the
  ledger; without them "no tokens" and "nobody counted" are the same zero.
- **HOME stays the real home.** The first version pointed it at the workspace, which would make every
  real harness fail to find its login — the first thing an operator would hit.

## Tasks

### T1 — `harness:<name>` in the vocabulary — done

`internal/caps`: a `harness:` prefix, validated like `lan:` (one word after the colon), declared by a
node and requireable by an item. Placement needs no change — it is a subset check — but the *refusal*
when a node omits it is worth a test.

Acceptance: `TestHarnessIsOneWord`, `TestAnItemNeedingAHarnessIsNotPlacedOnANodeWithoutIt`,
`TestAHarnessNodeCannotDeclareASandboxProfile`.

### T2 — `CommandExecutor` — done

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

### T3 — the harness signal, not `nsm task` (done, redesigned)

**This task was designed on a false premise and is built differently.** It assumed a narrow credential
— "escalate and post, no claim, no report" — but this codebase has no such thing: a node token is the
whole node, so putting it in a subprocess's environment would let a harness claim and report other
work. The narrow credential is a feature nobody has built (scoped tokens), and M8 should not invent
one to satisfy a plan.

What a credential-less harness *can* do is stop and say so:

- **exit 75** (`EX_TEMPFAIL`) parks the run and asks a human;
- **`NSM-ESCALATE: <action> | <reason>`** on stdout names what it wants, so the operator reads the
  harness's own words rather than a generic "the agent stopped";
- exit 75 without that line still parks, with the output as the reason.

That is the capability T3 wanted without the credential it assumed. If scoped tokens are ever built,
`nsm task thread` becomes possible; until then a harness that needs to say something says it in its
output or in the escalation reason.

Acceptance as built: `TestExitSeventyFiveAsksForAHuman`, `TestAnUnnamedEscalationStillAsks`,
`TestAnEscalatingHarnessIsStillUnmetered`.

### T4 — the MCP shim — NOT STARTED

For a tool-calling harness, the natural integration is not a wrapper command but tools:
`nsm mcp serve` speaks MCP over stdio (JSON-RPC 2.0, stdlib only) and exposes `board_claim`,
`board_start`, `board_report`, `board_escalate`, `board_thread`. A harness with MCP support configures
one server and pulls work as tool calls.

This is the largest task in M8 and the most optional: T2 already covers every harness that can run a
command. It earns its place because a harness that *chooses* when to claim is a better citizen on an
interactive machine than one told to run a command.

Acceptance: `TestMCPHandshakeAndToolList`, `TestAClaimThroughMCPIsTheSameClaim`,
`TestMCPReportsATerminalRefusalAsAToolError`, `TestMCPServesNoAdminTools`.

### T5 — proof — done

`docs/superpowers/proofs/2026-11-22-m8-harnesses-proof.md`: a real manager, a real
`--executor command` agent, and a scripted harness that parks itself by exit 75, is approved, and
finishes on the second attempt. The ledger records both runs as `unknown` rather than free.

The proof found three things the unit tests could not: a harness node was advertising a sandbox it
cannot deliver (the heartbeat appended one for every node), the escalation path was recording
"reported, 0" for a harness that spent tokens, and a proof that sleeps less than the agent's poll
interval proves nothing.

## Hardening after the proof

- **`nsm agent install` writes the harness flags.** The first version passed `--executor` but not
  `--run-cmd`/`--harness`, so `install --executor command` wrote a service that died at its first
  start with "--executor command needs --run-cmd". Found by asking what a reboot would do; the test
  asserts the generated unit contains them.
- **Usage is read on every exit path**, including escalation, failure and timeout. The first version
  only read it on success, so an escalating harness was recorded as "reported, 0".
- **The sandbox assertion is measured against the probe, not against seatbelt.** Linux CI has no
  confinement to offer, and a test that hard-coded a profile passed on macOS and failed where the
  board is meant to run.

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
