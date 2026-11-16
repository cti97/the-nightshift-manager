# The Night Shift Manager — M3 Implementation Plan (escalation and approvals)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a run stop and ask a human, and let a human answer — without the worker holding anything open while it waits.

**Architecture:** `request_approval` is the seventh tool. It ends the run as `escalated` and parks the item in `needs_approval`; the worker releases its lease and goes back to claiming. The board stores the request so the approvals page can list it without replaying events. A decision requeues the item carrying `approval_granted`, and the executor honours it rather than escalating the same action twice.

**Tech Stack:** Go 1.24+, stdlib only, `modernc.org/sqlite`, migration 4.

**Spec:** `docs/superpowers/specs/2026-11-15-nightshift-manager-design.md` §11 (escalation), §14 (dashboard), §16 (failure matrix)

**Predecessors:** the M0/M1 and M2 plans, and `2026-11-16-m2-carryover.md`. The M0/M1 plan's "Corrections applied during execution" section is required reading: eleven of its defects were in transcribed code, and three separate fixes replaced one false comment with another.

## Decisions settled before the first task

M2's Task 9 went smoothly because its six inherited decisions were decided in the plan rather than discovered inside the task. These are M3's.

**D1 — The approval request is stored, not only evented.** `needs_approval` items need `action`, `reason`, `options`, `requested_by`, `requested_at` readable by the approvals page. Replaying events to render a page is a query nobody will write twice the same way. Migration 4 adds `work_items.approval_request TEXT` (JSON, nullable).

**D2 — Only the admin decides.** Approval is a board action behind `adminAuth`. A node cannot approve its own escalation, and there is no route that lets one.

**D3 — `approval_granted` names the action, not just "granted".** It stores `{action, note, decided_at}`. The executor compares the pending action against it, so an approval for "delete the temp files" cannot silently authorise "rotate the keys". An approval that does not match the action being requested is a refusal — the run escalates again with the mismatch in its reason.

**D4 — A decision is one-way and first-wins.** A second decision on the same item is a `409`, not a silent overwrite. Approvals are auditable: the event records who decided, when, and what they said.

**D5 — An escalation spends the attempt.** The run used model tokens before it asked, so the attempt stays spent. The item is parked, not failed — `attempt` does not grow on the re-queue either, because the next run is not a retry of the same work.

**D6 — The reaper never touches a parked item.** It scans `claimed`/`running` only, so `needs_approval` is naturally excluded. The reaper's *node sweep* must also leave parked items alone — a parked item holds no lease.

**D7 — A decline is a cancellation with a reason.** `cancelled`, and the reason says it was declined, by whom, and their note. It is not `failed`: nothing went wrong.

**D8 — The approvals page is HTML, and phone-readable.** The design says the dashboard must work on a phone; a JSON endpoint alone does not satisfy "the daily screen". No JavaScript framework — a form and a button.

## Global Constraints

- **Module path** `github.com/cti97/the-nightshift-manager`; **Go `1.24`**; **`CGO_ENABLED=0`**; no new dependencies; stdlib `testing` only.
- **`time.Now()` appears in exactly one file, `internal/board/clock.go`.**
- Every state change writes an `AppendEvent` in the same transaction as the change.
- Handlers never panic; JSON error envelope `{"error":{"code","message"}}`.
- **No worker holds a lease while waiting for a human.** The escalation must close the run, not sleep in it.
- Do not weaken an existing test.
- **A comment must be checked against the code beside it, not edited for tone.** Three times in M2 a fix replaced one false comment with another.

---

## Task index

| # | Deliverable |
|---|---|
| 1 | Migration 4 and the approval-request record |
| 2 | The escalation transition: item `needs_approval`, run `escalated`, lease released |
| 3 | The approvals query and API (`GET`, `POST /admin/approvals/{id}`) |
| 4 | Approve: requeue with `approval_granted`; deny: `cancelled` with a reason |
| 5 | The `request_approval` tool and executor wiring |
| 6 | Honouring an approval: matching action, no second escalation |
| 7 | The approvals page |
| 8 | `/why`, board counts and the failure matrix for parked items |
| 9 | `nsm approve` / `nsm approvals` for the terminal-less operator |

---

## Task 1: Migration 4 and the approval record

**Files:** `internal/board/schema4.sql`, `internal/board/db.go` (the migrations list), `internal/board/approvals.go` (new).

```sql
ALTER TABLE work_items ADD COLUMN approval_request TEXT;
CREATE INDEX idx_items_parked ON work_items(status, created_at) WHERE status = 'needs_approval';
```

**The type:**

```go
// ApprovalRequest is why a run stopped and what it wants. Stored, not evented,
// so the approvals page is one query rather than a replay (M3 D1).
type ApprovalRequest struct {
	Action   string   `json:"action"`
	Reason   string   `json:"reason"`
	Options  []string `json:"options"`
	RunID    string   `json:"run_id"`
	RequestedBy string `json:"requested_by"` // node id
	RequestedAt int64  `json:"requested_at"`
}
```

**Acceptance:**
- `TestMigrationFourAddsTheApprovalColumn` — a v3 database gains the column and the index, and the migration is forward-only.
- `TestApprovalRequestRoundTrips` — encode, store, read back byte-identical, including an empty `Options`.
- `TestAVersionFourDatabaseRefusesAnOlderBinary` — already true; re-assert it for the new version.

## Task 2: The escalation transition

**Files:** `internal/board/approvals.go`.

One transaction: item → `needs_approval` with `approval_request` set; its run → `escalated` with `ended_at`; lease cleared (`claimed_by`, `lease_expires_at`); one `escalated` event carrying the action and reason.

**Acceptance:**
- `TestEscalationParksTheItemAndClosesTheRun` — statuses as above, `claimed_by` null, `lease_expires_at` null.
- `TestEscalationChargesTheAttemptButDoesNotGrowIt` — `attempt` unchanged by the escalation, and unchanged by the subsequent re-queue (D5).
- `TestAVanishedRowCannotBeEscalated` — the transaction re-reads the row; escalating a settled item errors and writes nothing.
- `TestTheReaperLeavesAParkedItemAlone` — advance past the lease, reap, assert the parked item is untouched and no `reaped` event exists for it (D6).

## Task 3: The approvals query and API

**Files:** `internal/board/approvals.go`, `internal/board/http.go`.

```
GET  /admin/approvals           -> [{item_id, title, action, reason, options, requested_by, requested_at, attempt}]
POST /admin/approvals/{id}      {decision: "approve"|"deny", note: string}
```

`GET` returns parked items oldest first, and `[]` on an empty board.

**Acceptance:**
- `TestApprovalsListIsOldestFirst` and `TestApprovalsListIsEmptyNotNull`.
- `TestApprovalsRequireTheAdminToken` — node tokens are 401.
- `TestApprovalsListExcludesNonParkedItems` — a `needs_approval` item appears; a `done` one with `approval_granted` set does not.

## Task 4: Deciding

**Files:** `internal/board/approvals.go`.

Approve: `approval_granted = {action, note, decided_at}`, item → `queued`, one `approved` event.
Deny: item → `cancelled`, `blocked_reason` names the decline, one `denied` event.
Both: re-read the row inside the transaction; a decision on a non-parked item is `409` (D4).

**Acceptance:**
- `TestApprovalRequeuesWithTheActionItGranted` — `approval_granted.action` equals the parked action, and the item is `queued`.
- `TestDenialCancelsWithTheDecidersNote` — `cancelled`, and the note appears in `blocked_reason`.
- `TestASecondDecisionIsRefused` — the second returns `409`, the first stands, and exactly one decision event exists (D4).
- `TestAnApprovedItemCanBeClaimedAgain` — the claim path picks it up, and the executor receives the grant.

## Task 5: The `request_approval` tool

**Files:** `internal/agent/tools.go`, `internal/agent/agentic.go`.

A seventh tool whose schema carries `action`, `reason` and `options`. Calling it ends the run with `ErrNeedsApproval` (a sentinel, like `ErrFinished`) and a `Result` carrying the request. The executor reports the run as escalated rather than ok or failed.

**Acceptance:**
- `TestRequestApprovalEndsTheRunWithoutFurtherSteps` — no provider call after it, and the step count stops there.
- `TestTheApprovalRequestReachesTheBoard` — end-to-end: an item the stub script escalates appears in `GET /admin/approvals` with the same action and options.
- `TestEscalationIsNotAProviderError` — the run's status is `escalated`, and no `error` text is written.

## Task 6: Honouring a grant

**Files:** `internal/agent/agentic.go`.

Before dispatching `request_approval`, the executor compares the pending action against `item.ApprovalGranted`. A match proceeds **without** re-escalating. A mismatch escalates again with the mismatch named in the reason (D3).

**Acceptance:**
- `TestAGrantedActionDoesNotEscalateTwice` — the second run completes; no second approval appears.
- `TestAGrantForADifferentActionEscalatesAgain` — the reason names both actions.
- `TestAGrantIsConsumedOnce` — a third run does not silently reuse it; say what the intended behaviour is and pin it.

## Task 7: The approvals page

**Files:** `internal/board/pages.go` (new), `internal/board/http.go`.

HTML at `GET /admin/approvals` when the client accepts HTML, JSON when it accepts JSON — one route, two representations, decided by `Accept`. A form per item with Approve/Deny and a note field.

**Acceptance:**
- `TestApprovalsPageRendersEveryPendingItem` — one card per parked item, action and reason visible, no JavaScript.
- `TestApprovalsPageIsPhoneReadable` — the viewport meta tag is present and the layout is single-column; assert the markup that makes that true rather than the styling.
- `TestApprovalsPageSubmitsADecision` — the form posts to the decision route and the item leaves the page.
- `TestJsonIsStillAvailableForScripts` — `Accept: application/json` returns the same data.

## Task 8: `/why`, counts, and the matrix

**Files:** `internal/board/why.go`, `internal/board/http.go`, design §16.

`/why` for a parked item must say it is waiting on a human, naming the action and how long it has waited. Board counts already include `needs_approval`. The design's failure matrix gains the escalation rows.

**Acceptance:**
- `TestWhySaysAParkedItemIsAwaitingAHuman` — the action and the wait are in `blocked_reason`.
- `TestWhyDoesNotSayAParkedItemCouldBeClaimed` — `placed:false`, because a parked item is not claimable.
- `TestBoardCountsParkedItems`.

## Task 9: `nsm approve` / `nsm approvals`

**Files:** `cmd/nsm/main.go`.

Two subcommands so a decision does not require a browser. `nsm approvals` lists; `nsm approve <id> [--note]` and `nsm deny <id> --note`.

**Acceptance:**
- `TestApproveWithoutANoteIsAcceptedButSaysSo` — the CLI warns that a decision with no note is harder to audit later.
- `TestApprovalsCommandPrintsTheQueue` — and `[]` when empty, not a blank line.
- The admin token comes from the environment, never a flag.

---

## Verification for the milestone

```bash
make test && make race && go vet ./...
NSM_ADMIN_TOKEN=a NSM_ENROLL_TOKEN=b ./bin/nsm manager serve --db /tmp/m3.db &
# an endpoint, an item the stub escalates, then approve it and watch it complete
```

And one manual proof that M3 changed something real: an item that escalates, appears on the approvals page, is approved with a note, and then **completes on the next claim without asking again**.

## What M3 deliberately does not do

- No multi-approver consensus. One admin, one decision.
- No approval expiry. A parked item waits indefinitely; if it should time out, that is a watch (M4).
- No watches, threads, or steward work (M4–M6).
