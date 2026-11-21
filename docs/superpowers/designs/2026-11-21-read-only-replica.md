# Read-only board replica — design memo (proposal, not implemented)

> Drafted by a subagent (Hermes on `inkling-2-small:cloud`) from the spec, then reviewed. It is a
> proposal for M6's fourth steward duty, not a plan and not code. The reviewer's caveat is at the
> end.


Scope: the M6 `steward` role gains a fourth duty — serve a read-only copy of the board when
the VPS is unreachable. Read-only, per §7: a Pi that can write to the board is split-brain on
the home LAN. Per §19.1, a VPS outage pauses the shift; the replica only keeps the dashboard
readable. It cannot mint work.

## 1. What to replicate: events, not a materialised view

Replicate `events` plus the reference tables it dereferences (`nodes`, `endpoints`,
`models`, `config`), then rebuild the projection on the Pi. No materialised snapshot.

Why: invariant 2 — `events` is the only source the dashboard reads; state is a projection. An
event log replays deterministically, is idempotent (`AUTOINCREMENT` id), and answers "what
happened while the VPS was down" after recovery. A snapshot is a second, divergent source of
truth with no drift check and no history. Share the projection code with the manager's board
view; do not fork it. The replica lives on USB SSD, never SD (§7), and runs no claim path,
reaper, watches or scheduler — projector and page server, nothing else.

## 2. Mechanism: Litestream. The others lose.

**Pick: Litestream**, replicating the manager's SQLite (WAL) to the Pi over SSH/LAN, ~1s
continuous replication, point-in-time restore.

- `sqlite3 .backup` over SSH: correct online backup, but a nightly cron, not a channel —
  hours-stale, manual board-down restore. Keep it as the mandatory off-box backup (§19.1),
  not the dashboard path.
- HTTP pull from the manager: makes the replica a client of the very thing whose liveness it
  reports. A sick VPS (hung, TCP open, never answers) hangs or 404s the pull, and the replica
  can't tell how stale it is; it also couples the replica to the API surface. Litestream ships
  WAL frames — the Pi needs no manager code path alive and no board-read token.

## 3. Freshness bounds, shown on the page

One number matters: **replica lag**, printed in the page chrome on every view — e.g. "board as of 14:02:11 — 3m12s behind VPS", or "VPS unreachable since 14:02 — showing last replicated state".

- Healthy: lag ≤ ~1–5s on a LAN. >30s amber; >5min red.
- VPS down: lag grows unboundedly — that is the point. The page switches from "lag" to "as-of `<timestamp>`, VPS unreachable" so a dead board never reads as a quiet one.
- Lag comes from `max(events.at)` plus last successful WAL sync time, not the Pi's clock alone (invariant 4: server-side timestamps). If the two disagree by >1min, show both.
- Writes are impossible, so there is no "fresh enough to act on" threshold — only for reading. Approvals, claims and pauses render disabled with "replica is read-only; VPS unreachable".

## 4. Why read-only, and what breaks otherwise

Board correctness rests on a single writer (§5 invariants, §16 concurrent claims). A writable
replica breaks it in specific ways:

- **Split-brain claims.** The Pi claims while the VPS is merely partitioned; both sides assign the same item, and on heal the two SQLite files cannot merge — orphaned events, `runs` loses append-only.
- **Forked event log.** `events.id` is AUTOINCREMENT; two writers collide and diverge. No CRDT exists here and none is wanted.
- **Phantom approvals.** A decision recorded only on the Pi is invisible to the executor that asked; the item parks forever, or the operator re-decides on the VPS — §16 answers that on one board with 409; across two boards it is unanswerable.
- **Spend-ceiling bypass.** The ceiling gates issuance inside the claim transaction; a Pi approving or claiming off-ledger has no view of settled runs and pushes spend past the ceiling with no record.

So: GET routes only, database opened read-only at the driver level, and no credentials that
could write to the manager. The chosen failure mode is "VPS down → dashboard readable, shift
paused". The rejected one is a Pi that "helps" by taking writes and corrupting the ledger quietly.

## 5. How the Pi decides the VPS is down, without flapping

Two independent signals, both required to enter replica mode, either sufficient to leave it:

- Litestream sync state: last successful sync older than T (default 60s), or restore failing.
- HTTP probe: `GET /version`, 2s timeout.

Entering replica mode requires both to agree for 3 consecutive intervals (3 × 10s = 30s). Leaving requires the probe to succeed **and** Litestream within lag budget for 2 consecutive intervals — hysteresis on both edges, so a flapping VPS produces one banner flip, not a strobing page. Transitions are logged as replica-local events (not board events) and stamped on the page. "VPS back" only clears the banner and re-enables links; it never triggers writes, claims or replays from the Pi.

## 6. Task breakdown (repo style)

- **R1 — Replica store and projector.** `internal/replica/{store,project}.go` + `_test.go`;
  share projection code with the manager's board view. Tests: `TestReplicaReplaysEventsIdempotently`,
  `TestReplicaOpensDatabaseReadOnly`, `TestReplicaProjectionMatchesManagerFixture`. Rationale:
  one projection path is the only way the replica can't drift from manager board semantics.
- **R2 — Litestream wiring and lag telemetry.** `internal/replica/litestream.go`,
  `deploy/pi/litestream.yml`, `internal/replica/litestream_test.go`. Tests:
  `TestReplicaLagIsReportedFromWALSyncNotWallClock`, `TestReplicaRefusesWriteStatements`,
  `TestReplicaSurvivesInterruptedRestore`. Rationale: the freshness badge is an operator-safety
  control; it must derive from replication state, not vibes.
- **R3 — Read-only HTTP surface and staleness banner.** `internal/replica/{http,pages}.go`,
  `internal/replica/http_test.go`. Tests: `TestReplicaRegistersNoPostRoutes`,
  `TestStalenessBannerShowsLagUnderBudget`, `TestStalenessBannerSwitchesToAsOfModeWhenVPSUnreachable`,
  `TestReplicaPagesRenderWithoutManagerReachable`. Rationale: "dashboard renders during outage"
  is the whole deliverable — it must be a test, not a hope.
- **R4 — Health decision with hysteresis, steward wiring, runbook.** `internal/replica/health.go`,
  `internal/replica/health_test.go`, `cmd/nsm/main.go`, `docs/replica-runbook.md`; the M6 steward
  starts the replica alongside the reflex loop. Tests: `TestReplicaEntersOnlyAfterThreeFailures`,
  `TestReplicaExitsOnlyAfterTwoSuccesses`, `TestReplicaModeFlapProducesSingleTransition`.
  Rationale: flapping between modes is worse than either — each transition risks an operator
  acting on a half-true page.

Non-goals: no write-through, no approval capture on the Pi, no automatic failover of the
claim path, no SD-card database. The replica is a window, not a second board.

## Reviewer's caveat

The memo's first decision — replicate `events` and rebuild the projection — depends on invariant 2
being true in the implementation, not just in the spec: every state transition the dashboard reads
must be written as an event. The board writes 25 kinds today (`created`, `claimed`, `reported`,
`fired`, `frozen`, `checkpoint`, …), but nobody has yet audited them against the board view's
columns. That audit is the first task of any replica work, and it is cheaper than it sounds: a
query for rows whose current state has no corresponding event, run against a board that has been
used for a week.

If the audit finds a gap, the replica must carry a snapshot of that table as well — and the memo's
"one projection path" becomes "one projection path plus a documented snapshot exception", which is
a different design. Worth knowing before R1 is written, not after.
