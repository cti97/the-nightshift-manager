# M5 threads and budgets — the manual proof

Run on 2026-11-21 against `bin/nsm manager serve` and real HTTP, with one registered node
(heartbeated, protocol 2) and the same fake OpenAI endpoint the M3/M4 proofs used. Script:
`/tmp/m5proof.sh`; board `/tmp/m5proof/m5.db`.

## Part A — a thread that freezes, reopens and closes

Thread `cache policy`: budget 300 tokens, 100-token output ceiling, 4 turns.

| step | observation |
|---|---|
| agent-a posts | 201, reserve 112 charged |
| agent-b posts | 201, reserve 118 charged (the prompt is now both messages) |
| agent-a posts again | **409 `thread_frozen`** — "reserve 118 with 220 used exceeds the 300 budget" |
| events | exactly **one** `frozen`, no event per refused post |
| operator PATCH budget=2000 | 200, status `open`, exactly **one** `open` event |
| the conversation continues | two more posts; after the fourth turn status `closed`, one `closed` event |
| a post to the closed thread | **409 `thread_closed`** |

The boundary is the one §17 asks for: the third message was refused because 220 used + 118
reserved exceeded 300, not because of an off-by-one — the unit test pins both sides of the
exact boundary (64 refuses, 65 accepts).

## Part B — a checkpoint a requeued attempt resumes from

| step | observation |
|---|---|
| item claimed, run started | — |
| `POST /runs/{id}/checkpoint` | 201, `seq` 1 |
| run escalates, operator approves | item `needs_approval` → `queued` carrying the grant |
| the second claim | `prior_checkpoint.declared_progress` = "listed 1184 files, deleted 900" |

This is D7 through the real API: the marker is per run, "newest" is across the item's runs
(not the newest seq — every attempt's seq starts at 1), and the claim carries it.

## Part C — the page on a phone

A second thread (`cache policy on the phone`, 5000 tokens) opened in a real browser at
390×844 with HTTP Basic, after two agent messages:

```
open — 2077 of 5000 tokens used, 2923 left — last word from agent-b
bar: 41%
messages: agent-a (1033 tokens), agent-b (1044 tokens)
```

Then posted through the page's own form:

```
open — 3134 of 5000 tokens used, 1866 left — last word from admin
messages: ..., admin (1057 tokens): "hold off — the index is rebuilding until midnight"
```

The message was recorded as `admin` — the route forces the author, and the browser sent no
author field at all. The post answered 303, so the URL stayed on the thread and a refresh
cannot repost. Screenshot: `/tmp/m5proof/phone-thread.png`.

## What the proof found that the tests did not

1. **The node must heartbeat before it can claim.** The script registered a node and called
   `/claim` — zero items, no error. Placement reads live capabilities, and they arrive with
   the heartbeat; the unit fixture had the same bug first (M5 Task 7). The board is right and
   the callers were wrong, but the silence is worth naming: a claim on an unheartbeated node
   looks exactly like a board with nothing to do.
2. **`curl -d` sends a form content type**, and the decision route parses forms for the M3
   page. An approval posted as `-d '{"decision":"approve"}'` without
   `Content-Type: application/json` is a 400 that mentions no decision. Scripts should say
   what they are sending.
3. **Protocol version is part of the heartbeat.** The first heartbeat omitted
   `protocol_version` and the node became `incompatible` — with empty live capabilities, so
   the claim saw no candidate. The board's answer was correct and its status was visible; the
   script was wrong.
