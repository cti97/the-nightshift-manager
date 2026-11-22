# M9 proof — retention that deletes, and a front door

Run on 2026-11-22 against a real manager. Script: `/tmp/m9proof/proof.sh`. The board was given a
year of history before the manager's first retention tick, because the interesting case is a board
that has been working, not a fresh one.

## The fixture

```
== a board with a year of history: let the manager create the schema, then seed it ==
  events: 402
```

402 audit rows: one a day for 400 days, one from an hour ago, and one from two hours ago. The
two-hour one is a witness — see the floor below.

## The front door

Three items — one parked, one queued, one done — and `GET /admin` as a browser sees it:

```
<title>The board — the night shift manager</title>
Waiting on a human
needs your decision
Queued
queued work
Done <
already finished
```

Parked first, then queued, then done: the order is the SQL's `CASE status`, and it is a contract
because it decides which items survive truncation. `TestTheParkedItemSurvivesTruncation` is the test
that keeps it honest — the parked item is the *oldest*, so any order that sorts by insertion drops
exactly the row the operator came for.

## Pruning to 90 days

```
== retention: prune to 90 days, on demand ==
{"cutoff":1787582408,"deleted":311,"keep_hours":2160}
  events left: 96
  work items: 3
  runs: 0
```

311 of 402 rows removed; 96 remain — roughly the 90 days inside the window plus the rows the manager
wrote while starting. The work items are untouched, which is the claim that matters: retention prunes
the audit spine, not the board.

## The floor

```
== the floor: asking for 1 hour still keeps a day ==
{"cutoff":1795272008,"deleted":89,"keep_hours":1}
  the two-hour-old witness survived: True
  events left after asking for 1h: 7
```

The request said one hour; 89 events went, because everything daily is more than 24 hours old. The
witness — two hours old — survived. That is the rule: `--retention` below 24h is raised to 24h rather
than obeyed, because a typo in a unit file must not be able to delete today's spine at the next tick.

## What this proof does not show

- **A year of history in a *running* board.** The fixture was seeded between two starts. The pruning
  query is the same either way, but nobody has watched a live board's file stop growing.
- **Vacuum.** Deleting rows frees pages for reuse; the file does not shrink. A board that pruned
  400 days of history still occupies most of what it did until SQLite reuses those pages or an
  operator runs `VACUUM` — which this project does not do, by design: `VACUUM` rewrites the whole file
  and would hold the write lock for as long as it takes.
- **The tick.** Retention runs hourly in the daemon; this proof drove `POST /admin/retention/prune`
  because waiting an hour is not a proof. The scheduler is covered by
  `TestRetentionPrunesInBatches` and the same `PruneEvents` path.
