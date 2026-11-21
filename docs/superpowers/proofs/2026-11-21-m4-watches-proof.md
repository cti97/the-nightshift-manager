# M4 watches — the manual proof

Run on 2026-11-21 against the real binaries: `bin/nsm manager serve`, a real node
registered over the enrollment credential, `nsm agent run` in its normal polling mode,
and an OpenAI-compatible fake at `127.0.0.1:8098` whose script asks for approval and
only acts once a tool result says the action was granted.

Board: `/tmp/m4proof/m4.db`. Scripts: `/tmp/m4proof-a2.sh`, `/tmp/m4proof-b.sh`.

## Part A — a condition watch through M3's whole path

A condition watch `disk-edge` with `probe: test -f /tmp/m4proof/flag`, 1h dedupe,
`needs: [agentic]`.

| step | observation |
|---|---|
| flag absent, 20s (more than one 15s tick) | **0 items** — a false condition emits nothing |
| flag created | **1 item within 9s**, on the rising edge |
| condition still true, 45s more | still **1 item** — no re-fire while true |
| agent claims | item `needs_approval`, 1 claim |
| `nsm approve --note` | item back to `queued` carrying the grant |
| agent claims again | 2 claims, grant consumed, item `done` |
| final board | `{"done":1}`, every other status 0 |

The wire log shows the three turns the M3 design predicts:

```
{"turn": "ask",  "user_has_grant": false}
{"turn": "ask",  "user_has_grant": true}
{"turn": "act",  "tool_results": ["approved: an operator allowed the action ..."]}
```

Spend: `0.00279` over 2 runs, `unpriced_runs: 0` — the escalation was priced into the
day's ledger, unchanged by being watch-emitted.

Watch state afterwards:

```
watch "disk-edge" is a condition watch: the condition is true (last checked 0s ago)
```

## Part B — a restart across two interval windows

An interval watch `every-minute` (`60s`, the board's floor; dedupe disabled so the
window cannot be the thing that explains the count).

| step | items |
|---|---|
| before | 1 (part A's) |
| first fire | 2 |
| manager stopped 130s — two 60s windows pass | 2 |
| restart | **3** — one catch-up, not one per missed window |
| next window, 70s later | 4 — the schedule resumed |

Three fired events for three emissions; the stopped windows produced nothing until the
restart, and then exactly one item.

## What the proof found that the tests did not

1. **The CLI could not create a condition watch.** `watch add` required `--every` or
   `--cron`; a condition watch has neither. The board supported the feature; the only
   way to reach it from a terminal was a hand-written POST. Fixed: a lone `--probe` is
   a condition watch.
2. **The scheduler logged a line every 15 seconds** for a steady condition, because a
   skip was logged. Ten minutes of a healthy watch was forty lines saying nothing. Now
   only fires and failures are logged; the reason is on the row and in the state
   sentence.
3. Harness setup, not product: a new endpoint is `health: unknown` and claim only
   issues from `ok` (probe it first), and a model serving `agentic` needs the `tools`
   feature. Both were M3 steps that had to be repeated here, and both failures were
   silent until `/why` named the rule.
