# M7 — agents file work

Spec §5 (items), §9 (placement), §11 (escalation), §14 (dashboard), §22 (roadmap).

Today the board issues work in exactly two ways: an operator posts an item, or a watch emits one.
An agent that discovers the job is bigger than its brief has one honest option — escalate — and one
dishonest one: do the extra work inline, invisibly, unmetered, and lose it when the run ends.

M7 gives it a third: **file the follow-up on the board**, where it is placed by capability, priced in
the ledger, and visible to the operator. The item is the unit of work; an agent with a tool to create
items must not be able to turn the board into a fork bomb, an escalation ladder, or a second
identity.

## The four rules that make it safe

**1. Depth, not recursion.** A child carries `parent_id` and `depth`; a root item is depth 0, and an
agent may file at most depth 2. Depth is stored, not computed at read time, so a restart cannot
launder a chain and a moved item cannot lose its history. Grandchildren are allowed; great-
grandchildren are an approval, not a write.

**2. Capability inheritance.** A child's `needs` must be a subset of the parent's *live* capabilities
at filing time, and its `reach` no wider. An agent may not ask for a sandbox profile it did not
have, a model class it was not placed on, or a `lan:*` capability its host cannot reach. Without this
rule, an agent with a shell is an agent that can mint work for a more privileged box.

**3. Fan-out is bounded per run.** One run files at most five children. The cap is enforced in the
same transaction that creates the item (count children of this run, refuse at five), because the
interesting failure is two concurrent filings that each see four.

**4. Over-cap filing is an approval, not an error.** Asking for a sixth child, or a child at max
depth, parks the parent run with `request_approval(action: "file_item", …)` — the M3 machinery, with
a grant that names exactly one filing. An agent that can explain why it needs a sixth child gets one;
an agent that cannot gets a denial whose note is the reason. This is the whole design: **the limit is
where the human is asked, not where the agent is silenced.**

## Identity

`work_items.created_by` records `agent:<node-id>` or `operator`, and the creating event's actor
carries the same string. The dashboard shows the parent → child tree on the item page, so "who asked
for this" is one click, and a chain of agent-filed work reads as a chain rather than as five
unrelated items that appeared overnight.

## Idempotency

Agents retry. `POST /runs/{id}/items` takes a `client_ref` the caller chooses, unique per board; a
repeat returns the existing item with `created: false`. Without it, every network hiccup between the
agent and the board files the same child twice and spends the work twice.

## What building it changed

Three of the plan's own claims did not survive contact:

1. **"Fan-out is counted per run"** is defeated by the reaper. Every re-claim creates a new run id, so
   five children per attempt is fifteen for one item — the exact case the paragraph above said it was
   preventing. The cap now counts the parent's children; `filed_by_run` stays, as the audit record of
   who asked.
2. **The grant cannot be consumed by the claim.** M3's rule is that a claim eats its grant. For
   `file_item` that spent the operator's answer on the act of resuming and then refused the filing it
   had been granted for. The claim now preserves a `file_item` grant and the filing consumes it.
3. **A denied filing must not cancel the work.** D7's "a decline is a cancellation with a reason" is
   about the task; this refusal is about one request the task made. A denial now requeues with the
   note as `blocked_reason`.

Two smaller ones: `CreateItem` ignored `ParentID`, so an operator-filed child was depth 0 and the
agents under it could file two more levels; and a parked filing answered `201`, telling a client an
item existed while the board waited for a human (`202` now).

The caller is a **tool**, not a CLI: `file_item` is bound per run to the board client and added to
that run's tool set only when the executor has a client. The model supplies the work and the id;
the run id comes from the executor, because the run is what the board checks ownership against.

## Tasks

### T1 — the schema (migration 7) — done

`work_items` gains `created_by TEXT`, `depth INTEGER NOT NULL DEFAULT 0`,
`client_ref TEXT UNIQUE`, and `filed_by_run TEXT` (the run that filed it, so fan-out is counted
against a run and not against an item that may be retried).

Acceptance: `TestADepthTwoChildCannotBeMoved`, `TestClientRefIsUniqueAcrossTheBoard`,
`TestAFreshBoardHasDepthZero`.

### T2 — `POST /runs/{id}/items` — done, plus the tool

Node auth, the run's own node, only while the run is `claimed` or `running`. Validates depth,
capability subset and fan-out inside one `BEGIN IMMEDIATE` transaction; returns
`{id, created}`.

Acceptance: `TestAnAgentMayFileAChildOfItsOwnRun`, `TestAChildCannotWantWhatItsParentDidNotHave`,
`TestAFilingForAnotherNodesRunIsRefused`, `TestTheSixthChildIsAnApprovalNotAWrite`,
`TestARetriedFilingReturnsTheSameItem`, `TestFilingAfterTheRunEndedIsRefused`.

### T3 — the `file_item` approval — done

`request_approval` gains the action `file_item`; a grant allows exactly one filing above the cap and
is consumed by it. A denial leaves the run parked with the operator's note.

Acceptance: `TestAGrantLetsExactlyOneExtraChildThrough`, `TestADenialParksTheRunWithItsReason`.

### T4 — CLI and page — done

Built as `GET /admin/items/{id}` — JSON for a caller, a page for a browser (the route's `Accept`
decides, as the approvals page already does) — plus `nsm item tree <id>`, which prints one indented
line per item with its status, who filed it and the run it came from.

`nsm run file` is deliberately NOT built: filing from a run is an agent's act, and an operator who
wants a child can create one with `--parent`. A CLI that could file as any run would be a way to
bypass the capability rule, which is the one thing the route exists to enforce.

Acceptance as built: `TestTheTreeShowsEveryDescendantWithItsProvenance`,
`TestTheTreePageRendersForABrowser`, `TestTheTreeSaysWhenItIsParked`,
`TestItemTreePrintsWhatTheBoardFiles`, `TestItemTreeNeedsAnAdminToken`.

One fix the race detector forced: children were ordered by `created_at, id`, and ids are random
tokens, so siblings filed in the same second came out in arbitrary order. The order is
`created_at, rowid` — insertion order, which is the order the operator watched them appear in.

### T5 — proof — done

`docs/superpowers/proofs/2026-11-21-m7-filing-proof.md`: a real manager, real HTTP, five filings, a
retry returning the first item, a capability refusal, a sixth that parks as 202, an approval, a
re-claim that does NOT spend the grant, one granted child, and a seventh that parks at six.

Three caller-side findings are recorded there: the decision route's form-versus-JSON trap (now fixed
at the route: the body's first byte decides), `client_ref` being scoped to one run, and a node at
concurrency 1 holding a lease being unable to claim.

## What M7 does not do

- **No re-parenting.** An item's ancestors are fixed at creation; moving an item is a new item.
- **No automatic result propagation.** A parent does not wake when a child finishes. The parent was
  told to file and exit; if it wants the child's result it should have used a thread, which exists.
- **No cross-board filing.** One board, one fleet.
- **No agent-to-agent messages beyond threads.** "Agent A asks agent B to do X" is exactly what
  filing an item is; there is no second channel.

## Why not just let the agent spawn subagents inline?

Because the work would then be invisible (no board row), unmetered (no reservation), unplaced (the
parent's node runs everything), and lost on crash (no durable record). The board's whole claim is
that a unit of work is a row with a lease and an event; work that skips the row is the thing this
project exists to stop.
