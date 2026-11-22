# M8 proof — a local harness is a node

Run on 2026-11-22 against a real manager (`bin/nsm manager serve`) and a real
`nsm agent run --executor command`. The harness is a shell script standing in for `claude -p`: the
point is the board around it, not the model. Script: `/tmp/m8proof/proof.sh`.

## What the harness got

The node was started with:

```
nsm agent run --board http://127.0.0.1:8100 --node harness-node \
  --provides harness:scripted --reach host --sandbox-root /tmp/m8proof/run/work \
  --executor command --run-cmd "sh /tmp/m8proof/run/harness.sh" --harness scripted
```

and the harness's own view of the run was six variables — item, run, title, attempt, workdir, brief
file — plus PATH, HOME and LANG. No node token, no endpoint keys: `TestTheHarnessSeesItsIdentityAndNothingElse`
asserts the first, and the proof's harness could not have reported anything even if it tried.

## Attempt 1: the harness asks for a human by exit code

A harness has no board credential, so it cannot call the approvals API. It stops with exit 75
(`EX_TEMPFAIL`) and says what it wants on a line the executor reads:

```
NSM-ESCALATE: run the migration against the live table | it has 2M rows and I cannot rehearse it
```

```
== the board's view after the first attempt ==
  status: needs_approval
  why: item 2efff783… is waiting on a human: the action "run the migration against the live table" has to be app…
  parked: 2efff78320ab | run the migration against the live table | it has 2M rows and I cannot rehearse it
```

The operator sees the harness's own words, not a generic "the agent stopped". A harness that exits 75
without naming anything still gets a human, with its output as the reason — guessing what it wanted
would be worse than asking someone to read (`TestAnUnnamedEscalationStillAsks`).

## The decision, and attempt 2

```
== approve — a human reads the reason the harness gave ==
  decision -> 200

== after the second attempt ==
  status: done
  item: ('done', '## plan for: # migrate the ledger table\nworkdir=/tmp/m8proof/run/work/run-d5256316293ff809')
  runs: 2 | escalated: 1
  events: created, created, probed, created, registered, claimed, started, escalated, approved,
          claimed, started, reported
```

The second attempt is a new run on the same item, with the approval carried as a grant that the claim
delivers. The harness found the human's go-ahead, finished, and the item settled `done` with the
harness's output as its summary.

## What the ledger says

```
== the ledger knows what it did not measure ==
  runs: 2 | spent: 0 | unmetered: 2 | input: 0 | output: 0
```

Both runs are `token_source: unknown`. That is M8's Rule 2 holding: a harness that reports nothing is
*unmeasured*, not free. Before this, both runs would have shown as `reported` with zero tokens — a
day of harness work would have looked like a day of nothing. A harness that does account for itself
writes `NSM_USAGE.json` and the run becomes `reported`
(`TestAHarnessCanReportItsOwnUsage`).

## Three things the proof found that the tests did not

1. **A harness node advertised a sandbox.** The heartbeat appended `sandbox:<profile>` for every
   node, so `harness:scripted` + `sandbox:seatbelt` reached the board together and the board refused
   the heartbeat as a contradiction. `liveCapabilities` now asks the executor (`Confines()`), because
   the sandbox probe is a fact about the *machine* while `provides_live` is what the node will *do*
   with the work.
2. **The escalation path lost the token source.** The first version of `Run` returned a `Result`
   without a source when the harness exited 75, so the board recorded an escalating harness as
   "reported, 0". Usage is now read on every exit path, including failure and timeout.
3. **The agent polls on its own clock.** Six seconds was not enough to see the second attempt; the
   proof waits 35s. Not a defect, but a proof that sleeps for less than the poll interval proves
   nothing and looks like a failure.

## What this proof does not show

- No real `claude`/`codex` run. The harness is a script; what a real one needs (its own login in
  `$HOME`, its own model credentials) is exactly why `HOME` is passed through rather than pointed at
  the workspace.
- No MCP shim: `nsm mcp serve` is not built. A tool-calling harness must be wrapped in a command for
  now.
- Nothing ran on a Pi, and no item was placed on a harness with a *sandbox requirement* — that
  combination is refused by `caps.ConfinementConflict` and by placement, both proven by tests rather
  than by this script.
