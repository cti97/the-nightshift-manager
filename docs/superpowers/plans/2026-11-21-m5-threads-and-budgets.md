# M5 — threads and budget reserve/settle

Spec §13 (channel and budgets), §12 (checkpoints), §14 item 4 (thread view), §17 item 3
(freeze exactly at the reservation boundary).

A thread is a conversation with a budget, not a chat room. Two agents alternate messages;
each message reserves its cost against the thread's budget *before* it is written, and
settles what it actually used after. The budget is the brake that stops two agents from
looping politely at each other for an hour.

## Decisions

**D1 — the manager estimates, the poster reports.** `reserve = estimated_prompt +
max_output`, and the estimate must be the board's number: a caller that sends its own
reserve can under-reserve and spend a budget it does not own. The manager estimates the
prompt from the thread's messages plus the new body with a documented heuristic
(`1 + len/4` bytes-to-tokens), which is a *reserve* approximation, never a bill. Actuals
come from the poster (`tokens` on the request, input+output), because only the node saw
the model. Settlement charges what was reported — **not capped at the reserve** — since
capping would make an under-reserving caller's overspend free. A node that lies about
its usage is out of scope, like a node that lies about reach (§19.4).

**D2 — `max_output_tokens` is a thread column.** The spec's schema has no place for the
`max_output` half of the reservation. Migration 6 adds it, default 1024, so the
reservation is computable from the row and not from a constant someone forgets.

**D3 — alternation applies to agents, not to the operator.** A message whose author
equals `turn_owner` is refused with 409 `same_author_twice`, except when the author is
`admin`: a human correcting themselves is not a loop. This is the second guard in §13 and
the one that actually stops two agents, because a cap only delays a loop.

**D4 — frozen is a pause, not a death.** A refused reservation freezes the thread and
writes one event. A frozen thread refuses every message (409 `thread_frozen`) until an
operator raises its budget with `PATCH /admin/threads/{id}`, which reopens it when the
numbers fit. Without that route "frozen" would be indistinguishable from "closed", and
the failure matrix promises an admin can act.

**D5 — settlement without a report charges the reservation.** A poster that omits `tokens`
is charged the reserve: the budget is not refunded for silence, or a caller could keep
the reservation forever by never reporting.

**D6 — a turn is a message.** `turns_used` counts messages, and `max_turns` closes the
thread (409 `thread_closed`) rather than freezing it: running out of turns is the
conversation ending, not the budget failing.

**D7 — checkpoints are per run, the newest belongs to the item.** `POST
/runs/{id}/checkpoint` is node-authenticated and only the run's own node may write; `seq`
is per run and monotonic. Claim carries `prior_checkpoint` (the latest checkpoint across
the item's runs) so a requeued attempt can resume from what the last one declared — the
spec's "resume means a new attempt whose brief includes the prior checkpoint", made real
without pretending tool state migrates.

**D8 — messages are append-only, and the thread is append-only except for its budget.**
No edit, no delete, no reordering: a conversation that can be rewritten is not an audit
trail, and the messages are part of why an item did what it did.

## Tasks

### Task 1: migration 6 — the three tables

**Files:** `internal/board/schema6.sql`, `internal/board/db.go`, `internal/board/db_test.go`.

`threads`, `messages`, `checkpoints` with the spec's columns plus `max_output_tokens`;
status vocabulary `open|frozen|closed` enforced; `seq` unique per run; the message index
the thread view reads.

Acceptance: `TestMigrationSixAddsThreadsAndCheckpoints` (columns, the status check, the
unique `(run_id, seq)`, indexes), `TestAVersionFiveDatabaseRefusesAnOlderBinary` still
passes with the new migration list, and a v6 downgrade refuses with the running version
named.

### Task 2: create and read a thread

**Files:** `internal/board/threads.go`, `internal/board/threads_test.go`, `internal/board/http.go`.

`Thread`, `Message`, `CreateThread`, `GetThread` (thread + messages + budget numbers),
`listMessages`. Validation: subject, positive budget, max_turns, max_output. The read
returns `remaining = budget - used - reserved`, and `[]` for messages, never null.

Acceptance: `TestCreateThreadStoresTheBudget`, `TestThreadBudgetIsValidated` (zero,
negative, absurd max_turns), `TestGetThreadReturnsMessagesInOrder`.

### Task 3: post a message — reserve, refuse, send, settle

**Files:** `internal/board/threads.go`, `internal/board/threads_test.go`.

One transaction (`BEGIN IMMEDIATE`): re-read the thread, check status, alternation,
max_turns, compute `reserve`, refuse-or-freeze, insert the message, settle, update the
thread's counters. The refusal writes the freeze; the success writes the message and the
counters in the same transaction.

Acceptance: `TestAMessageReservesAndSettles` (used/reserved after send), `TestSettlementReleasesTheDifference`
(report 10 of a 1000 reserve), `TestTheThreadFreezesAtTheReservationBoundary` (§17 item 3:
one token short does not freeze, exactly at the boundary does), `TestASilentPosterIsChargedTheReserve`,
`TestAnAgentCannotPostTwice` (409), `TestAdminMayPostTwice`, `TestTheLastTurnClosesTheThread`.

### Task 4: freeze, reopen, and the events

**Files:** `internal/board/threads.go`, `internal/board/http.go`, `internal/board/threads_test.go`.

`PatchThread` (budget only), `frozen` on refusal with one event, `reopen` when the new
budget fits, `409` for a patch that does not.

Acceptance: `TestAFrozenThreadRefusesMessages`, `TestRaisingTheBudgetReopensTheThread`,
`TestFreezingWritesExactlyOneEvent`, `TestALowerBudgetCannotRefreezeAnOpenThread` (the
patch either fits or is refused — it never puts the thread back into a state its numbers
contradict).

### Task 5: checkpoints

**Files:** `internal/board/checkpoints.go`, `internal/board/checkpoints_test.go`, `internal/board/http.go`.

`POST /runs/{id}/checkpoint` behind node auth; the run must be live and belong to the
node; `seq` increments; `declared_progress` required.

Acceptance: `TestACheckpointIsStoredWithTheNextSeq`, `TestOnlyTheRunsNodeMayCheckpoint`
(403 for another node), `TestACheckpointOnAFinishedRunIsRefused` (409), `TestCheckpointsAreOrdered`.

### Task 6: resume — the claim carries the prior checkpoint

**Files:** `internal/board/claim.go`, `internal/board/checkpoints.go`, `internal/board/claim_test.go`.

`ClaimedItem` gains `prior_checkpoint` (latest across the item's runs, nil for a first
attempt). A requeued item's second claim carries the first run's checkpoint; a first
claim carries none.

Acceptance: `TestTheSecondClaimCarriesTheFirstCheckpoint`, `TestAFirstClaimCarriesNoCheckpoint`,
`TestTheCheckpointBelongsToTheItemNotTheRun`.

### Task 7: the API surface

**Files:** `internal/board/http.go`, `internal/board/threads_api_test.go`.

`POST /admin/threads`, `GET /threads/{id}` (JSON for agents), `POST /threads/{id}/messages`,
`PATCH /admin/threads/{id}`; admin routes behind `adminAuth`, the thread read behind node
auth (it is a participant's view), errors as `{error:{code,message}}` with the codes
`thread_frozen`, `thread_closed`, `same_author_twice`, `budget_refused`.

Acceptance: `TestAPostToAFrozenThreadIs409WithTheCode`, `TestTheThreadReadRequiresANode`,
`TestAShortBodyStillChargesTheEstimate`.

### Task 8: the thread page

**Files:** `internal/board/pages.go`, `internal/board/pages_test.go`.

A single-column page like approvals: subject, the budget bar (used/reserved/remaining),
messages oldest-first with author and charge, and a form to post as `admin`. One route
negotiating HTML and JSON (M3's D8 pattern).

Acceptance: `TestTheThreadPageShowsTheBudgetBar`, `TestTheThreadPagePostsAsAdmin`,
`TestThePageAndTheJSONAgreeOnRemaining`.

### Task 9: the manual proof

Two agents alternating in a thread with a small budget: they talk, the thread freezes at
the boundary (one message refused, exactly one freeze event), the operator raises the
budget, they continue, max_turns closes it, and a checkpoint written mid-run appears in
the next attempt's claim. Recorded under `docs/superpowers/proofs/`.

**Note after implementation:** the proof found three things the tasks did not name, all on the
caller's side rather than the board's: a node must heartbeat (with `protocol_version`) before
`/claim` finds it, and `curl -d` sends a form content type that the decision route parses as a
form. The board answered correctly each time; the failures were silent enough to be worth
writing down. See `docs/superpowers/proofs/2026-11-21-m5-threads-proof.md`.

## Verification

```bash
make test && make race && go vet ./... && gofmt -l internal cmd
```

## What M5 deliberately does not do

- No streaming, no notifications, no unread markers: a thread is a log with a budget.
- No per-message model choice, no tool calls in messages: messages are text.
- No token verification — the board trusts the poster's count (D1) and says so.
- No rewind, edit or delete (D8).
- No automatic unfreeze at midnight: budgets are per thread, not per day; the daily
  ceiling is the other brake (§13) and it is already implemented.
