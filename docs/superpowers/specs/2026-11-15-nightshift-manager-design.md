# The Night Shift Manager — design

**Date:** 2026-11-15
**Status:** design approved in conversation; amended for multi-role nodes and remote inference
**Author:** session with the workspace owner

---

## 1. What this is

A coordination system where heterogeneously-equipped machines pick up work from a shared
board and act on it, without the admin assigning anything to anyone.

The board is **the manager**. Everything else is staff on a shift.

The concrete roster:

| Node | Role | Always on? | Serves |
|---|---|---|---|
| VPS | `manager` | yes | The board. SQLite, scheduler, dashboard, approvals. |
| Home laptop (macOS + Ollama) | `executor` | no | Full agentic work, local models, shell |
| Work laptop (Claude) | `executor` | no | Full agentic work, cloud models |
| Raspberry Pi 5 | `steward` | yes | Reflex work, LAN presence, uptime canary, read-only replica |

Inference is *not* necessarily local to a node. A cloud Ollama endpoint is a first-class
participant, and the design has to survive it.

## 2. The problem it solves

Machines with different models cannot cooperate, because there is no place for them to agree
on what is true. A file in a shared folder has no claim protocol, so both machines will
eventually do the same job, or neither will, and neither will know.

The Night Shift Manager is the board — the thing that makes "who is doing what, and why isn't
it done" answerable — and nothing else.

## 3. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | VPS + SQLite is the single writer of truth | Already exists; always on, so the sleeping-laptop problem disappears. |
| D2 | Model all three primitives (`work_items`, `watches`, `checkpoints`) from day one, thin slice of each | They have genuinely different lifecycles; discovering that later means a migration. |
| D3 | Agents get a shared discussion channel with hard budgets | Requested. Chat is the fastest way to burn a budget overnight, so it needs accounting, not just limits. |
| D4 | Execution is sandboxed; anything real becomes a pending approval | The home machine has a trading bot with live Kraken keys. |
| D5 | Placement is declared-capability matching, never a router model | Every placement must be explainable by printing a reason. |
| D6 | Self-contained worker calling model APIs directly | No coupling to DSH internals; also makes the worker testable with a stub. |
| D7 | Pull, never push | A node asks for work. This is what makes "no assignments" real. |
| D8 | Models belong to endpoints, not nodes | Inference can live on another machine entirely. A node only *reaches* an endpoint. |
| D9 | Three node roles: `manager`, `executor`, `steward` | A Pi cannot run a 40-step loop at 6 tok/s, and pretending otherwise produces garbage rather than errors. |
| D10 | The board never carries prompt content | If inference were proxied through the VPS, the manager becomes a content processor for everything and `no_egress` collapses. |
| D11 | `no_egress` splits into two capabilities | One boundary (WAN) is not enough once inference can be on another LAN host. |
| D12 | One binary, `nsm`, with subcommands — not three artifacts | One version, one matrix, one thing to install. The manager carrying dead agent code costs a few MB and removes the "which binary is on which machine" class of confusion. |
| D13 | Six-target Go matrix, `CGO_ENABLED=0`, pure-Go SQLite | One command cross-compiles the whole fleet; `mattn/go-sqlite3` would make CGO mandatory and kill that. |
| D14 | The manager serves the release channel | Every node already trusts the board, so it is the natural place to fetch from, and it removes the "how do I update the Pi while travelling" problem. |
| D15 | Release signatures verify against a key pinned at install time | A board that serves both an artifact and its signature can authorize anything. The trust anchor must not come from the distribution channel. |
| D16 | Providers are protocol families (`anthropic`, `openai`, `ollama`, `stub`), not products | llama.cpp, vLLM, LM Studio, OpenRouter and OpenAI all speak one shape. Adding a model is a row in `endpoints`; adding a *protocol* is one file and a registry line. |
| D17 | A model without native tool calling is `reflex`-only; there is exactly one agent loop | Emulating tools as parsed text is a second, weaker control path that fails in ways that look like model weakness. Restriction is visible in `/why`; a silent fallback is not. |

**On D2 as built:** M0/M1 ships `work_items` and `checkpoints`'s design, but not the
`watches`, `threads`, `messages` or `checkpoints` tables — they arrive with M4/M5, which is what
the versioned migration runner exists for. The decision's *reason* (don't discover three
lifecycles after shipping one) held; its *implementation* is spread across milestones.

## 4. Non-goals

- Multi-user / multi-tenant. One admin, bearer token per node.
- Mobile app. The approvals page must be phone-readable HTML; that is the whole requirement.
- Agent-to-agent direct connections. Coordination goes through the board, always.
- Semantic memory, RAG, vector stores. Work items carry their own brief.
- Learned routing, benchmarking, fine-tuning.
- Containers on laptops. `sandbox-exec` on macOS, a plain directory on Linux.
- CRDTs. One writer. The Pi's replica is read-only, deliberately.
- The Pi running the agentic tool loop. It runs a reflex path only.

## 5. Data model

```sql
CREATE TABLE endpoints (
  id                TEXT PRIMARY KEY,
  name              TEXT NOT NULL,
  provider          TEXT NOT NULL,      -- registry name: anthropic | openai | ollama | stub
  base_url          TEXT NOT NULL,
  auth_ref          TEXT,               -- NSM_PROVIDER_* provider-key name; never the secret
  reach             TEXT NOT NULL,      -- host | lan | wan
  health            TEXT NOT NULL,      -- ok | warming | down | auth_failed | unknown
  health_reason     TEXT,               -- e.g. 'model loading', '401 from /v1/models'
  last_ok_at        INTEGER,
  latency_ms        INTEGER,
  created_at        INTEGER NOT NULL
);

CREATE TABLE models (
  id                TEXT PRIMARY KEY,
  endpoint_id       TEXT NOT NULL REFERENCES endpoints(id),
  model_id          TEXT NOT NULL,      -- 'claude-sonnet-4-5', 'qwen2.5:3b'
  provides          TEXT NOT NULL,      -- json array
  features          TEXT NOT NULL,      -- json: ["tools","vision","json_schema","streaming"]
  context_window    INTEGER,
  cost_in_per_mtok  REAL DEFAULT 0,     -- 0 for free/self-hosted
  cost_out_per_mtok REAL DEFAULT 0,
  enabled           INTEGER DEFAULT 1
);

CREATE TABLE nodes (
  id                TEXT PRIMARY KEY,
  name              TEXT UNIQUE NOT NULL,
  token_hash        TEXT NOT NULL,
  role              TEXT NOT NULL,      -- manager | executor | steward
  provides_declared TEXT NOT NULL,      -- config: what it means to offer
  provides_live     TEXT,               -- heartbeat: what is true right now
  reach_classes     TEXT NOT NULL,      -- json: ["host","lan"] — which endpoint reaches it can use
  concurrency       INTEGER DEFAULT 1,
  agent_version     TEXT,               -- reported on register/heartbeat
  protocol_version  INTEGER,
  last_seen_at      INTEGER,
  status            TEXT,               -- online | offline | paused | incompatible
  created_at        INTEGER NOT NULL
);

CREATE TABLE work_items (
  id                TEXT PRIMARY KEY,
  parent_id         TEXT REFERENCES work_items(id),
  title             TEXT NOT NULL,
  brief             TEXT NOT NULL,      -- the only prose a worker sees
  needs             TEXT NOT NULL,      -- json array, matched against provides
  status            TEXT NOT NULL,      -- queued|claimed|running|needs_approval|done|failed|cancelled
  priority          INTEGER DEFAULT 0,
  deadline_at       INTEGER,
  claimed_by        TEXT REFERENCES nodes(id),
  lease_expires_at  INTEGER,
  attempt           INTEGER DEFAULT 0,
  max_attempts      INTEGER DEFAULT 3,
  approval_granted  TEXT,
  result_summary    TEXT,
  artifact_path     TEXT,
  blocked_reason    TEXT,
  created_by        TEXT NOT NULL,      -- 'admin' | node id
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);

CREATE TABLE runs (
  id                TEXT PRIMARY KEY,
  work_item_id      TEXT NOT NULL REFERENCES work_items(id),
  attempt           INTEGER NOT NULL,
  node_id           TEXT NOT NULL,
  endpoint_id       TEXT REFERENCES endpoints(id),
  model_id          TEXT REFERENCES models(id),
  started_at        INTEGER NOT NULL,
  ended_at          INTEGER,
  status            TEXT NOT NULL,      -- claimed | running | ok | error | lost | abandoned | escalated
  input_tokens      INTEGER DEFAULT 0,
  output_tokens     INTEGER DEFAULT 0,  -- provider-reported actuals
  reserved_tokens   INTEGER DEFAULT 0,  -- budget charged for attempts with no usage
  cost_estimate     REAL DEFAULT 0,
  placement_reason  TEXT NOT NULL,
  transcript_path   TEXT,
  error             TEXT,
  UNIQUE (work_item_id, attempt)
);

CREATE TABLE checkpoints (
  id                TEXT PRIMARY KEY,
  run_id            TEXT NOT NULL REFERENCES runs(id),
  seq               INTEGER NOT NULL,
  declared_progress TEXT NOT NULL,
  state             TEXT,
  artifacts         TEXT,
  created_at        INTEGER NOT NULL
);

CREATE TABLE watches (
  id                TEXT PRIMARY KEY,
  name              TEXT NOT NULL,
  trigger           TEXT NOT NULL,      -- cron | interval | condition
  schedule          TEXT,
  probe             TEXT,               -- shell command, exit 0 fires the watch
  template          TEXT NOT NULL,      -- json: {title, brief, needs, priority}
  dedupe_window     INTEGER DEFAULT 3600,
  cooldown_until    INTEGER,
  enabled           INTEGER DEFAULT 1,
  last_fired_at     INTEGER,
  next_fire_at      INTEGER
);

CREATE TABLE threads (
  id                TEXT PRIMARY KEY,
  work_item_id      TEXT REFERENCES work_items(id),
  subject           TEXT NOT NULL,
  budget_tokens     INTEGER NOT NULL,
  tokens_reserved   INTEGER DEFAULT 0,
  tokens_used       INTEGER DEFAULT 0,
  max_turns         INTEGER DEFAULT 20,
  turns_used        INTEGER DEFAULT 0,
  turn_owner        TEXT,
  status            TEXT NOT NULL,      -- open | frozen | closed
  created_at        INTEGER NOT NULL
);

CREATE TABLE messages (
  id                TEXT PRIMARY KEY,
  thread_id         TEXT NOT NULL REFERENCES threads(id),
  author            TEXT NOT NULL,      -- 'admin' | node id | 'system'
  body              TEXT NOT NULL,
  tokens_charged    INTEGER DEFAULT 0,
  created_at        INTEGER NOT NULL
);

CREATE TABLE config (
  key               TEXT PRIMARY KEY,   -- 'daily_spend_ceiling', 'board_paused'
  value             TEXT NOT NULL
);

CREATE TABLE events (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  at                INTEGER NOT NULL,
  actor             TEXT NOT NULL,
  entity            TEXT NOT NULL,      -- work_item | node | endpoint | thread | watch | run
  entity_id         TEXT NOT NULL,
  type              TEXT NOT NULL,
  payload           TEXT
);
```

`endpoints.auth_ref` names a provider key; it never holds one. The name must sit in the
reserved `NSM_PROVIDER_*` namespace — `NSM_PROVIDER_` followed by an uppercase name — checked
when the endpoint is created and again by whichever process resolves it, so `NSM_ADMIN_TOKEN`
and `NSM_ENROLL_TOKEN` are unmatchable by construction rather than by review. The key is read
from the environment of the process that needs it (the manager, for a health probe) and never
transits the board: a row carries the name, not the secret.

Watches are evaluated by a scheduler **on the manager**, so `probe` runs on the VPS and not on
a node. A watch that needs to check something that only exists on a laptop must instead emit
a work item with the right `needs` and let placement route it — otherwise the watch has
quietly become a job runner on the wrong machine.

### Invariants

1. `runs` is append-only. `(work_item_id, attempt)` is unique. Every attempt is recorded,
   including ones where the node died.
2. `events` is the only source the dashboard reads. Current state is a projection.
3. `brief` is prose, `needs` is structured. **Placement never reads `brief`.**
4. All timestamps are server-side. Node clocks are never trusted.
5. **No prompt content ever transits the manager.** The manager sees `title`, `brief`, and
   `result_summary`. Transcripts live on the node that produced them and are referenced by
   path; the dashboard fetches them from the node, not from the board.

## 6. Capability vocabulary

Closed set. Unknown strings are rejected at write time, so typos surface immediately instead
of producing permanently unplaceable work.

| Capability | Meaning |
|---|---|
| `agentic` | May run the full multi-step tool loop (executor only) |
| `reflex` | Single call or ≤3 steps (steward or executor) |
| `shell` | May run shell commands inside a sandbox |
| `no_egress` | Inference must not cross the WAN boundary: `reach ∈ {host, lan}` |
| `strict_local` | Inference must be on this same machine: `reach = host` |
| `network` | Item may make outbound HTTP requests from the sandbox |
| `long_context` | Requires ≥128k context |
| `vision` | Requires image input |
| `gpu` | Requires local acceleration |
| `local_fs:<path>` | Requires read access to a path on the node |
| `lan:<service>` | Requires reliable presence inside the home LAN |
| `tokens:<n>` | Estimated token budget; the model must accept it |

Two capabilities are parameterized and do not match by string equality: `local_fs:<path>`
matches when the provider's path is a prefix of the need, and `tokens:<n>` matches when
`provided >= needed`. Everything else is exact.

**`no_egress` and `strict_local` are two different promises** and conflating them is how data
leaks. A Pi serving a laptop over the home LAN satisfies `no_egress`. A rented GPU box in a
cloud VPC you own does **not** — it is still `wan`. Anything derived from the trading bot's
account state is `strict_local`.

## 7. Roles

**`manager`** — the VPS. Owns the board, runs the scheduler and reaper, serves the dashboard.
Claims nothing, executes nothing, and never sees prompt content (invariant 5).

**`executor`** — laptops. Runs the full agentic loop: tools, sandbox, up to ~40 steps. Provides
`agentic`. Only present while awake.

**`steward`** — the Pi. Runs a *reflex* path: claim, one model call or one probe, report. No
tool loop, no sandbox. Its value is not compute, it is **presence**:

1. **It keeps the `no_egress` lane alive 24/7.** When both laptops are shut, private work
   still has a qualified executor. This is the original requirement — 24/7 — finally satisfied
   for the work that cannot go to the cloud.
2. **`lan:<service>` means reliably inside the home network.** A laptop can provide it too, but
   only while it is home — which is why capabilities are dynamic (§8).
3. **Uptime canary** for the cloud endpoint: the Pi probes it on a schedule and flips
   `endpoint.health`, so placement stops selecting a dead model before a run discovers it.
4. **Read-only board replica** so the dashboard still renders when the VPS is down. Read-only,
   because a Pi that can write to the board is split-brain on the home LAN.

The Pi runs its replica on USB SSD, never the SD card — a SQLite WAL on an SD card is a
corruption generator, and this one holds the record of what the shift did.

## 8. Claim protocol

The manager never pushes. A node asks.

```
POST /nodes/heartbeat   {status, provides_live, reach_classes, free_slots}
   → extends leases held by this node
   → updates provides_live (reality, as of now)

POST /claim  { node_id, free_slots }
  BEGIN IMMEDIATE
    rows = SELECT * FROM work_items
            WHERE status = 'queued'
              AND needs ⊆ (node.provides_live ∪ model.provides)
            ORDER BY (cheapest eligible endpoint), priority DESC, created_at ASC
            LIMIT free_slots
    for each row:
      UPDATE work_items
         SET status='claimed', claimed_by=?, lease_expires_at=now()+300, attempt=attempt+1
       WHERE id=? AND claimed_by IS NULL
      if rowcount == 0: continue
      INSERT INTO runs(...)
  COMMIT
```

SQLite's single writer plus the `claimed_by IS NULL` guard plus the rowcount check makes this
atomic with no locking cleverness. A loser gets an empty result and long-polls again.

The `WHERE` clause above is abbreviated — §9 is the normative eligibility rule, including the
reach, health and role checks. Keep the two in sync or the code will implement a subset of the
policy and the `/why` endpoint will confidently explain the wrong thing.

**The claim payload carries the chosen execution target.** A claimed item includes the endpoint
(id, name, provider, base URL, reach) and the model (row id and wire `model_id`), because the
node has no other route to those tables: `GET /admin/endpoints` is admin-only and endpoints have
no node-facing read. The endpoint's `auth_ref` travels as a **name**, never a value; the node
resolves it from its own environment under the same `NSM_PROVIDER_*` rule the manager applies
(§5), so a credential never transits the board. This is protocol 2: a node that speaks 2 needs
the fields, and `ProtocolMax` is 2 so one talking to a protocol-1 board is marked incompatible
and claims nothing rather than claiming work it cannot execute.

**Reserved spend.** `runs.input_tokens`/`output_tokens` are provider-reported actuals.
`runs.reserved_tokens` is the separate budget charge for attempts that failed and reported no
usage (a retried `Complete` can be billed twice); the spend ledger counts both rather than
pretending the failed attempt produced output.

**Capabilities are dynamic.** A laptop that has driven to the office can no longer reach
`lan:home-nas`, and its `provides_live` says so on the next heartbeat (15s). Placement uses
`provides_live`, never `provides_declared`; the declared set is config and is only used to
detect drift (a node whose live set shrinks for an hour is a dashboard warning, not a silent
degradation).

**Lease semantics.** Lease = 300s, extended to `now()+300` on each heartbeat. All lease
arithmetic is server-side.

**Reaper** (every 30s):
- lease expired, run still `claimed` → run `abandoned`, requeue, and **refund** the attempt
- lease expired, run `running` → run `lost`, requeue, attempt stays spent
- `attempt >= max_attempts` → `failed`, event, dashboard card. Never auto-retried.
- `last_seen_at` older than 2× the lease → node `offline` (see §8.1)

`>=`, not `>`: with `>` a `max_attempts` of 1 would permit two executions.

That distinction is the reason `POST /runs/:id/start` exists. A node that claims work and dies
before starting has spent nothing; a node that started and died has spent tokens and, more
importantly, may have had side effects. Without a `claimed → running` transition the board
cannot tell those apart, and either innocent claims burn attempts or dead runs get retried as if
they were free.

### 8.1 Two counters, and why they are not one

`work_items.attempt` counts **started** runs: incremented at claim, refunded when a run is
abandoned before starting. It is what the budget checks.

`runs.attempt` is a **per-item claim ordinal**, derived from `MAX(attempt)+1` over that item's
runs. It exists to satisfy `UNIQUE(work_item_id, attempt)` on an append-only table.

They are deliberately not equal, and they must never be joined to each other — use the run id.
An earlier design had one counter serve both purposes, which made a refund collide with the
abandoned run's ordinal and left the item permanently unclaimable.

### 8.2 Node staleness

A pull-based board cannot know a node is gone until something notices it stopped asking. The
reaper sweeps a node whose `last_seen_at` is older than twice the lease to `offline`. Without
that sweep an offline node stays `online` forever, `/why` cheerfully explains work that nothing
will ever claim, and the answer to "why is this not running" becomes a confident lie.

## 9. Placement

Eligibility, in order:

```
needs ⊆ (node.provides_live ∪ model.provides)
endpoint.reach ∈ node.reach_classes
endpoint.health = 'ok'                  -- warming / auth_failed / down never qualify
model.enabled
no_egress     → endpoint.reach ∈ {host, lan}
strict_local  → endpoint.reach = host
agentic       → node.role = executor AND model.features ∋ tools
vision        → model.features ∋ vision
reflex        → node.role ∈ {executor, steward}
context_window >= tokens:<n>
```

Ranking: cheapest qualifying endpoint → priority DESC → created_at ASC.

The reason is persisted on the run as printable text:

```
queued 40m; pi-steward provides [reflex,no_egress,lan:nas];
qwen2.5:3b @ host (0.0 €/Mtok) cheapest of 3 eligible
```

`blocked_reason` refreshes on every placement failure:

```
needs [agentic,strict_local] · no online node provides both
· pi-steward (online) provides reflex,no_egress,lan:nas
· home-laptop (offline 6h) provides agentic,strict_local
```

### Reach is asserted, so verify it

`reach` is a declaration, not a fact. A node that lies about being able to reach a `lan`
endpoint would get egress-constrained work and send it out over the WAN. So the adapter
resolves the endpoint URL and refuses to send when the resolved address is not in the declared
class — loopback/RFC1918 for `host` and `lan`, anything else for `wan`. Cheap check, real
guarantee, and it lives in the adapter so it cannot be bypassed by a code path that skips
placement.

## 10. Worker

```
register(role, provides_declared, reach_classes, concurrency) -> token
loop:
  heartbeat(leases, provides_live)   every 15s
  items = claim(free_slots)          poll every 25s (a ticker, NOT a held request)
  for item in items:
      sandbox = mkdir(run_root/<run_id>)   # executor only
      result  = Executor.run(item, sandbox, model, budget)
      report(result)
```

Two implementations behind one interface, which is also the test seam:

```python
class NodeExecutor(Protocol):
    def run(self, item, model, budget) -> RunResult: ...

AgenticExecutor    # executor: tool loop, sandbox, caps
ReflexExecutor     # steward: one call or one probe, no sandbox
StubExecutor       # replays a scripted transcript; no network, no key
```

The poll is a ticker, not a long-poll. A long-poll would hold the board's single database
connection for its whole duration and block every heartbeat and report behind it — a property
of the single-writer design, not a preference.

### Provider registry

```go
type Provider interface {
    Complete(ctx, req) (resp, error)          // content, tool calls, usage
    EstimateTokens(messages, tools) int
    Probe(ctx) (health, latency, models)      // discovery + canary
}
```

Registered by name; `endpoints.provider` selects one. Four ship in v1:

| `provider` | Shape | Covers |
|---|---|---|
| `anthropic` | Messages API | Claude |
| `openai` | Chat completions + `tool_calls` | OpenAI, llama.cpp `llama-server`, vLLM, LM Studio, OpenRouter, Groq, Together |
| `ollama` | The `openai` shape plus `/api/tags` for discovery | Ollama, local or remote |
| `stub` | Scripted transcript | Tests; the reference implementation |

Adding a *model* is a row in `endpoints`. Adding a *protocol* is one file plus one registry line,
and nothing else in the system changes — placement, leases, budgets, and the dashboard speak
capabilities, never provider names.

**Features are declared, not assumed.** `models.features` holds `tools`, `vision`,
`json_schema`, `streaming`. A model without `tools` can serve `reflex` items only (D17); the
restriction is enforced at placement and explained by `/why`, so it surfaces as a scheduling
fact rather than a run that fails forty steps in.

**The `no_egress` enforcement double-check lives here**: if the item declares `no_egress` or
`strict_local`, the adapter verifies the endpoint's resolved address against
`endpoints.reach` before the first request and refuses the run otherwise.

### Error taxonomy

Every provider error normalizes to one of:
`rate_limited | auth | context_overflow | model_missing | transport | bad_request | server_error`

| Class | Retry within the run? |
|---|---|
| `transport`, `server_error`, `rate_limited` | yes, bounded 3×, with backoff |
| `auth`, `model_missing`, `bad_request` | no — fail once, raise event |
| `context_overflow` | no — this is a **placement bug**, not a runtime condition (§9 computed the token fit) |

Retrying a 401 three times and then marking the item failed is precisely the "burns attempts on
a bad credential" failure in §16.

### Token estimation

Budgets reserve before sending, and most providers only report usage afterwards, so every
provider implements `EstimateTokens`. The default is a heuristic; families with a real tokenizer
can be more precise. The channel reserves at **1.2×** the estimate and re-checks after settling,
because an estimator that runs 30% low turns a budget into a suggestion.

### Provider conformance

Every provider must pass the same suite — an adapter that isn't in the suite does not exist:

1. plain completion returns content and non-zero usage
2. tool-call round-trip: emits a call, accepts the result, finishes
3. usage is stable across two identical runs
4. `EstimateTokens` within ±40% of actual
5. error mapping: fake 401 → `auth`, 429 → `rate_limited`, context-too-long → `context_overflow`
6. refuses to send when the run declares `no_egress` and the reach class contradicts

It runs against a fake HTTP server in CI, and against a real endpoint via
`nsm provider test <endpoint>` when deploying. `stub` is the reference implementation, so the
suite is provable with no provider present.

### Health states

`probe()` lists models, performs a 4-token completion, records latency. Four states, because
they need different responses from the operator: `ok`, `warming` (llama.cpp loads on first
request and 503s until ready), `auth_failed`, `down`.

### Tools

Closed set of six for `agentic`: `read_file`, `write_file` (sandbox only), `list_dir`, `shell`
(sandbox cwd, timeout, network only if `network`), `http_get` (only if `network`), `finish`.

Plus `request_approval(action, reason, options)` — an escalation, not a capability.

Caps: ~40 steps **and** a token budget, whichever trips first.

### Sandbox

Per-run directory, no credentials, `env -i`, `HOME` overridden, no keychain, no ssh agent. On
macOS a generated `sandbox-exec` profile (read system paths, write only the run dir, network
iff declared).

**This is a blast-radius reducer, not a security boundary.** Seatbelt profiles fail open and
`sandbox-exec` is deprecated. The structural guarantee is narrower: nothing in the run
directory can reach the admin's home directory or credentials.

## 11. Escalation

`request_approval(action, reason, options)` → item `needs_approval`; run closes as `escalated`;
the item appears on the approvals page. Approve → requeue with `approval_granted`. Deny →
`cancelled`. No sleeping worker holds a lease open waiting for a human.

## 12. Checkpoints

**Progress markers, not state migration.** A half-finished conversation cannot be handed to
another machine with its tool state intact.

A checkpoint records `{seq, declared_progress, state, artifacts}`. "Resume" means a new attempt
whose brief includes the prior checkpoint. Weaker than it sounds, far stronger than pretending.

## 13. Channel and budgets

Reserve-then-settle, like a card authorization:

1. Before send: `reserve = estimated_prompt + max_output`
2. If `tokens_used + tokens_reserved + reserve > budget_tokens` → refuse, thread `frozen`, event.
3. After: settle actuals, release the difference.

Second guard: `turn_owner`. An agent may not post twice consecutively — alternation is a more
reliable brake than any cap, because two agents that can always reply to each other will loop.

Third: `max_turns`. Fourth: a **global daily spend ceiling**. When it trips, the manager stops
issuing work, freezes all threads, and marks itself `paused` until a human clears it.

## 14. Dashboard

1. **Board** — items by status; every `queued` item shows `blocked_reason`.
2. **Approvals** — phone-readable. Action, reason, options. The daily screen.
3. **Staff** — nodes with role, last seen, live capabilities, current item; endpoints with
   health, latency, spend today. Degraded states are visible: "no_egress lane dark",
   "ollama-cloud down 20m".
4. **Thread** — messages with the budget bar.

Plus:

```
GET /why/:item_id
```

which explains why an item ran, is queued, or failed. A system that can explain why work
*isn't* running is worth more than one that runs more work. Without it, `queued` becomes a
synonym for `lost` and the board stops being believed.

## 15. API

```
POST /nodes/register           {name, role, provides_declared, reach_classes, concurrency}
                               requires NSM_ENROLL_TOKEN — a secret that can ONLY enroll,
                               never the admin token, so a node never carries admin rights
POST /nodes/heartbeat          {status, provides_live, reach_classes, free_slots}
POST /claim                    {node_id, free_slots} -> [items]
POST /runs/:id/start           {} — the node has begun executing; claimed → running
POST /runs/:id/report          {status, result_summary, tokens, artifacts, error}
POST /runs/:id/checkpoint      {declared_progress, state, artifacts}
POST /runs/:id/approval        {action, reason, options}

GET  /board
GET  /why/:item_id
GET  /approvals
POST /approvals/:item_id       {decision, note}

POST /items
POST /watches
GET  /staff                    nodes + endpoints + health
GET  /spend/today

POST /endpoints                {name, provider, base_url, auth_ref, reach}
POST /endpoints/:id/probe      trigger a health check (canary)
GET  /transcripts/:run_id      proxied from the owning node, never from the board

GET  /dist/manifest            {version, targets, sha256, minisig}
GET  /dist/:os/:arch/nsm       the artifact itself; bearer-authenticated, not public
GET  /version                  board version + supported protocol range

POST /threads/:id/messages     {author, body} — reserve, send, settle
GET  /threads/:id
```

`POST /runs/:id/report` is **idempotent**: the first report wins, later ones are accepted and
ignored. A worker that reports and dies before the reply must not double-complete an item —
the one silent-failure vector in the write path.

All endpoints require `Authorization: Bearer <node_token>` except the admin surface, which sits
behind the reverse proxy's auth.

## 16. Failure matrix

| Situation | Behaviour |
|---|---|
| Lid closes mid-run | Lease expires → run `lost` → requeue `attempt+1` → another node picks it up |
| Node claims, dies before starting | Reaped with `attempt` unchanged — no tokens spent |
| Transient model error | Retried inside the run, bounded 3× — never re-queued |
| Worker reports, then dies before reply | Idempotent report; re-report is a no-op |
| `needs` unsatisfiable | Stays `queued` with `blocked_reason`; visible, not rotting |
| `attempt > max_attempts` | `failed` + event + card; never auto-retried |
| Two nodes claim at once | Single writer + guard + rowcount; loser retries |
| Cloud endpoint down | `endpoint.health=down` via canary; placement skips it; item falls to the next eligible endpoint or shows `blocked_reason` |
| Endpoint returns 401 | Health `auth_failed`, event raised, run errors **once** — a bad credential must not burn attempts |
| llama.cpp endpoint 503s while loading | Health `warming`; placement skips it; item waits or falls to another endpoint |
| `context_overflow` at runtime | Placement computed the fit wrongly; item fails with a visible reason and the placement log is attached — this is a bug report, not a retry |
| Laptop leaves the LAN | `provides_live` loses `lan:*` on next heartbeat; in-flight `lan` run fails cleanly and requeues |
| Pi offline | No `no_egress` lane; dashboard shows degraded rather than items silently queueing |
| VPS disk full | Board refuses writes, claims fail loudly, no silent state loss |
| Thread budget exhausted | Send refused, thread frozen, admin notified |

## 17. Testing

Cheap because the model is stubbed. The six that matter:

1. **Kill the worker mid-run** → exactly one requeue, original run marked `lost`.
2. **No item ever has two active runs** — invariant check after randomized claim/expire/reap.
3. **A thread freezes exactly at its reservation boundary**, not before or after.
4. **A stub transcript drives dispatch, caps, accounting and escalation** end-to-end, no network.
5. **`no_egress` is never placed on a `wan` endpoint** — property test over randomized items
   and endpoint sets. Fails loudly if someone later "optimizes" placement.
6. **The adapter refuses an endpoint whose resolved address contradicts its declared reach** —
   unit test with a stubbed resolver, since this is the check that makes the guarantee real.
7. **Every registered provider passes the conformance suite** (§10) against a fake HTTP server.
   A new provider that isn't in the suite does not exist, and `stub` is the reference.

## 18. Build order

- **M0** — manager skeleton: schema, migrations, `events`, `/why`, board read view.
- **M1** — executor register/heartbeat/claim/lease, reaper, `StubExecutor`, one deterministic
  end-to-end test, and `make release` producing the full six-target matrix. Provably correct,
  does nothing useful, and installable.
- **M2** — provider registry with `anthropic`, `openai`, `ollama`, `stub`; sandbox; six tools; cost
  accounting; conformance suite. Endpoints with `reach` land here — a cloud Ollama is just a `wan`
  row, and llama.cpp is an `openai` row, with no special case either way.
- **M3** — escalation and approvals.
- **M4** — watches (cron, interval, condition) with dedupe and cooldown.
- **M5** — threads and budget reserve/settle.
- **M6** — steward: Pi, `ReflexExecutor`, dynamic `lan:*` capabilities, endpoint canary,
  read-only replica, plus `nsm agent install` (launchd / systemd / Task Scheduler), release
  signing, and the manager's `/dist` channel.

Watches come after the claim path because a watch whose emitted items cannot be claimed is a
spam generator. The Pi comes last because everything above it must work before a steward is
worth adding.

## 19. Known weaknesses

1. **The VPS is a single point of failure.** Off-box backup is mandatory, not optional:
   Litestream or a nightly `sqlite3 .backup`. The Pi's replica keeps the dashboard readable but
   cannot mint work, so a VPS outage pauses the shift.
2. **The sandbox is best-effort** (§10).
3. **`blocked_reason` can go stale** between refresh passes.
4. **Reach is asserted by nodes**, partially mitigated by the adapter's address check (§9). A
   node that both lies about reach and controls its own DNS is out of scope.
5. **Per-hour GPU endpoints do not fit per-token cost fields.** Their cost is amortized into an
   estimate, so cost-based ranking is approximate — it picks a plausible endpoint, not an
   optimal one.
6. **A `reflex` model asked to do `agentic` work returns confident garbage, not an error.** The
   capability split depends on items being labelled correctly by whoever writes them.
7. **Alternation is a heuristic.** Two agents can still loop slowly across threads; the spend
   ceiling is the actual backstop.
8. **The work laptop may not permit a launch agent**, in which case that node exists only while
   logged in. Outside the design, and the largest unknown.
9. **The pinned signing key is also a pin on operational pain.** Rotating it means touching
   every node by hand, which is exactly the case where some node is asleep on the other side of
   the country.
10. **Auto-update does not exist on Windows in v1** (§20), so that node is updated manually or
    not at all.
11. **No `linux/arm` (v7).** Pi Zero and older boards are excluded; adding it later is one
    matrix line, but it is one more target that can silently fail its `file(1)` assertion.
12. **The board is now a code distribution point.** The pinned key makes a compromised board a
    courier rather than an author, but it does not stop a *rollback* to a signed older build
    unless the agent records the highest version it has ever seen. That check is worth having
    and is not yet designed.
13. **A capable local model with no tool calling is wasted capacity.** D17 is the right default
    and it does mean a strong model that only speaks plain completions can never do `agentic`
    work. Revisit only if a specific model makes the case.
14. **`EstimateTokens` is the weakest number in the system.** The 1.2× reserve absorbs a 20% miss;
    a provider whose real tokenizer diverges further will silently overshoot a thread budget.
    The post-settle re-check catches it, which is why that check is not optional.
15. **The `ollama` provider wraps the `openai` shape**, so an upstream change to either can put
    them in disagreement while both individually pass conformance.

16. **A permitted provider key can be sent to a row-chosen `base_url`.** The probe
    resolves only names in the reserved `NSM_PROVIDER_*` namespace, which makes the manager's own
    admin and enrollment tokens unmatchable — but a *provider* key is by definition sent to a
    provider URL, and the probe cannot tell a legitimate `base_url` from a malicious one. Creating
    an endpoint requires admin auth, so the residual needs admin access or a direct database write.
    Any future task that adds endpoint import, templating, or a non-admin write path must revisit
    this: the namespace defends the manager's tokens, not the provider's.

17. **Kernel confinement has never run in the environment M2 was built in.** The development
    shell is itself Seatbelt-sandboxed, so `sandbox_apply` is denied and
    `sandbox-exec -p '(version 1)(allow default)' /usr/bin/true` fails with "Operation not
    permitted". The acceptance test skips with that reason and `NSM_REQUIRE_KERNEL_SANDBOX=1`
    turns the skip into a failure. What M2 actually delivers is **tool-layer confinement** —
    `os.Root`, constructed environment, cwd, per-run directory. The generated profile is
    syntax-checked but never applied here. Verify on an unconfined machine with
    `NSM_REQUIRE_KERNEL_SANDBOX=1 go test ./internal/agent -run TestSandboxCannotReachTheHomeDirectory -v`
    before relying on the kernel layer for anything.

## 20. Distribution and release

The project ships one static binary, six artifacts, and a self-hosted channel.

### One binary

`nsm` with subcommands: `manager serve`, `agent run`, `agent install`, `agent rollback`,
`submit`, `why`, `approve`, `version`.

`CGO_ENABLED=0`. SQLite via `modernc.org/sqlite`, because `mattn/go-sqlite3` requires CGO and
one-command cross-compilation is the entire reason this matrix is viable.

### Matrix

| Target | Node |
|---|---|
| `darwin/arm64` | Home laptop |
| `darwin/amd64` | Any Intel Mac |
| `linux/amd64` | VPS (manager) |
| `linux/arm64` | Pi 5 (steward) |
| `windows/amd64` | Work laptop |
| `windows/arm64` | Not owned; free to include |

### Release script conventions

The same shape as `b10coincom/scripts/build-release.sh`, deliberately — it already encodes
failures worth not repeating:

- `dist/` starts empty and is swapped in only after every target succeeds
- artifact count must equal target count
- `file(1)` assertion demanding both a format and an arch token, in any order, because Apple's
  `file` prints `Mach-O 64-bit executable x86_64` and libmagic prints `Mach-O 64-bit x86_64
  executable`
- `vcs.revision` and `vcs.modified` stamped in, so `nsm version` names the commit it came from
- `-trimpath`, plus `dist/BUILD-INFO` recording the toolchain that actually ran
- `SHA256SUMS`, with a `.minisig` when the operator supplies a key from the environment; the
  key is never in the repo

Two implementations of this script (b10coin, nsm) is acceptable parallelism. If a third project
needs it, extract it then — not before.

### Where builds live

`dist/` is the build output and stays out of git. The manager serves it:

```
GET /dist/manifest           {version, targets, sha256, minisig}
GET /dist/:os/:arch/nsm      bearer-authenticated
```

`nsm agent install` fetches, verifies, and swaps atomically; `nsm agent rollback` restores
`.prev`.

**The verification key is pinned at install time and never re-fetched from the board.** The
board serving both an artifact and its signature would make a compromised board into code
execution on every node. The signature is a check against the pinned key, and the board is
merely a courier.

### Install and supervision

| OS / arch | Mechanism | Admin | Notes |
|---|---|---|---|
| macOS arm64 | LaunchAgent `~/Library/LaunchAgents/io.nsm.agent.plist`, `KeepAlive`, `RunAtLoad`, `ProcessType=Background` | no | Runs only while logged in; the lease, not the daemon, is what guarantees correctness |
| Linux arm64 (Pi) | systemd system unit, `Restart=always` | yes | journald for logs; replica on USB SSD, never the SD card |
| Windows amd64 | Per-user Task Scheduler entry at logon | no | Service mode requires admin that a corporate laptop will not grant |

Install via `curl` from the board, not a browser download: a browser download carries the
quarantine bit and macOS will refuse to execute the result.

### Version handshake and updates

The agent reports `version` and `protocol_version` on every heartbeat. Unsupported protocol →
node `incompatible` with a visible reason, **not** a crash loop against a schema it cannot
parse.

Auto-update is opt-in per node and Unix-only in v1. Windows cannot replace a running `.exe`,
and the workaround costs more than a manual update every few weeks — stated rather than hidden.

### Install order in the build plan

The M1 script emits `SHA256SUMS` unsigned. Signing arrives with the `/dist` channel at M6,
which is when a pinned verification key becomes meaningful — an unsigned checksum is a typo
detector, not a trust boundary.

`make release` lands at M1 because it is a script and proves the matrix early. `agent install`,
signing, and the release channel land at M6 with the steward, because that is the first machine
that has to run unattended.

## 21. Open questions

- Cloud Ollama auth: one endpoint-level token, or per-node tokens with revocation?
- Do transcripts get uploaded on completion, or fetched on demand from the node (which requires
  the node to be awake)? Invariant 5 implies fetch-on-demand; that makes old transcripts
  unavailable while a laptop sleeps.
- Should a `needs_approval` item block dependent follow-ups, or allow partial progress?
- Does the Pi need a UPS? A steward that browns out mid-report is a requeue, not corruption —
  but it is also the box that keeps the private lane open.
- Does the work laptop's MDM block an unsigned, unnotarized binary outright? If it does, the
  Windows and macOS install stories both collapse on that machine and it becomes dashboard-only.
- Key rotation: re-pin by hand on each node, or ship a second trusted key in the installer so
  rotation is a signature rather than a visit?
- Streaming: the loop doesn't need it, but the dashboard showing a live run would. Deferred
  until someone is actually watching a long run.
- Do providers get model discovery (`/v1/models`, `/api/tags`) or is every model declared by
  hand? Discovery is convenient and conflicts with declaring `features` per model — the likely
  compromise is discovery creates rows with provider defaults and a human overrides `features`.
