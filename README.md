# The Night Shift Manager

[![ci](https://github.com/cti97/the-nightshift-manager/actions/workflows/ci.yml/badge.svg)](https://github.com/cti97/the-nightshift-manager/actions/workflows/ci.yml)

A coordination board for heterogeneous machines. A VPS runs the manager; laptops and a
Raspberry Pi run agents that pick up work whenever they are awake and qualified. Nothing is
assigned to anyone — nodes ask, the board decides placement from declared capabilities, and
every decision is explainable.

The name is the point: it is a night shift for machines that sleep.

## Roster

| Node | Role | Always on | Serves |
|---|---|---|---|
| VPS | `manager` | yes | The board: SQLite, scheduler, dashboard, approvals |
| Home laptop (macOS + Ollama) | `executor` | no | Agentic work, local models, shell |
| Work laptop (Claude) | `executor` | no | Agentic work, cloud models |
| Raspberry Pi 5 | `steward` | yes | Reflex work, LAN presence, endpoint canary, read-only replica (all M6 — not built yet) |

Inference is not necessarily local to a node. A cloud Ollama, llama.cpp, or any
OpenAI-compatible endpoint is a first-class participant with a declared network reach.

## Status

**M0–M5 are implemented.** The manager, the claim loop, the provider layer, escalation and
approvals, watches, and threads with budgeted conversations all work end to end. M4 and M5 were
proved against the real binaries and the evidence is in `docs/superpowers/proofs/`; the earlier
milestones are covered by their tests.

M6 — the steward (Pi, `ReflexExecutor`, dynamic `lan:*` capabilities, endpoint canary,
read-only replica), `nsm agent install`, release signing and the manager's `/dist` channel — is
not written. `make release` builds the six-target matrix and checksums it, but nothing serves
those artifacts yet.

Every push and pull request runs `make test`, `make race`, `gofmt`, `go vet` and a `go mod tidy`
diff on **both** `ubuntu-latest` and `macos-latest`, then builds and checksums the six-target
matrix. Both platforms are load-bearing: the sandbox tests skip on Linux with a stated reason,
so one runner alone would leave either Seatbelt or the Linux build unproven. The badge above is
that run.

## Quick start

```sh
make build                                   # bin/nsm

# The manager. Both credentials come from the environment and must differ: the
# enrollment token can only register nodes.
NSM_ADMIN_TOKEN=…  NSM_ENROLL_TOKEN=…  ./bin/nsm manager serve --db nsm.db --addr 127.0.0.1:8081

# A node enrolls once; the token is shown once and is the node's only credential.
NSM_ENROLL_TOKEN=… ./bin/nsm node register --name laptop --provides agentic,vision \
    --reach host,lan --concurrency 2        # prints node_id and token

# The agent heartbeats, claims, runs and reports. It needs a disposable directory per run.
NSM_NODE_TOKEN=… ./bin/nsm agent run --node <node_id> --provides agentic,vision \
    --reach host,lan --sandbox-root /tmp/nsm-runs
```

Before a node can claim, the board needs somewhere to place work: an endpoint
(`POST /admin/endpoints`), a model under it (`POST /admin/models`), and `POST
/admin/endpoints/{id}/probe` to move its health to `ok`. Claim only issues from healthy
endpoints — an unprobed endpoint is not an error, just an empty claim.

## Command line

```
nsm version
nsm manager serve   [--addr] [--db]
nsm node register   --name --provides --reach [--role] [--concurrency] [--board]
nsm agent run       --node --provides --reach --sandbox-root [--policy FILE] [--executor agentic|reflex]
                    [--lan service=host:port,...] [--canary 60s]
nsm agent install   --node --provides --reach --sandbox-root [--executor …] [--print]
                    [--upgrade --pubkey KEY]
nsm dist keygen     --out nsm.key            # ed25519 signing key, 0600, refuses to overwrite
nsm dist sign       --key nsm.key --dir dist # writes dist/SHA256SUMS.sig
nsm dist verify     --pub KEY --dir dist
nsm approvals       [--board]                       # the queue, for a terminal
nsm approve <item>  [--note "…"] [--board]
nsm deny <item>     --note "…"   [--board]          # the note is the recorded reason
nsm watch add       --name (--every D | --cron "…" | --probe "cmd") --title --brief
                    [--needs a,b] [--dedupe 1h] [--board]
nsm watch list      [--board]                       # prints each watch's state sentence
nsm watch hold <name> (--for 6h | --clear) [--board]
```

Credentials are never flags: `NSM_ADMIN_TOKEN` for the admin commands, `NSM_ENROLL_TOKEN` for
`node register`, `NSM_NODE_TOKEN` for `agent run`. A `--token` argument does not parse.

`--canary` is the steward duty: every interval it probes each endpoint from this box's network path
and reports what it cannot reach. Three consecutive failures mark an endpoint `down` (one WAN
blip does not); a canary may demote but never promote, because a 401 from a reachable service is not
health and only the manager's authenticated probe is allowed to say `ok`.

`--lan` declares a service capability (`lan:nas`) only while a TCP dial to it answers, re-checked
on every heartbeat: a laptop that leaves the house stops advertising the home NAS within 15s.

`nsm agent install` writes a launchd agent (macOS) or a systemd **user** unit (Linux), plus a
wrapper that sources a `0600` environment file: the node token never appears in a unit file or a
plist, because those are world-readable and end up in every backup. `--print` shows all three files
without writing anything.

`nsm manager serve --dist dist` serves a signed release on `/dist` (admin token only — an artifact
list is a map of the fleet's platforms). The signature is ed25519 over the exact bytes of
`SHA256SUMS`, verified with `crypto/ed25519` inside the binary that needs it: no external tool to
install on a Pi, and no second copy of the checksum format to drift. The private key never goes on
the manager — a signature the manager could forge protects nobody from a compromised manager.
`nsm agent install --upgrade --pubkey …` fetches the artifact for its own target, checks the
signature, checks the bytes, and replaces itself with one atomic rename; a refused upgrade leaves
the old binary untouched. It records the artifact it installed and refuses a **lower** version: a
signature proves a release is genuine, not that it is current, so a compromised manager could
otherwise replay an old but validly signed binary.

`--policy` is a node's local wall: a JSON file of per-tool verdicts (`allow`, `deny`, `ask`),
consulted before a tool runs. `deny` refuses the tool and tells the model why; `ask` parks the run
for a human like `request_approval` does. Without a file every tool is allowed, which is how the
node behaved before the policy existed.

A watch is a standing trigger — cron, interval, or a condition probe — that emits an ordinary
item. It never holds a lease and never runs a model: **a watch emits work, it does not run it.**

## HTTP surface

Admin routes take the admin token as a bearer credential, or as the password of HTTP Basic so a
browser can open the pages. Node routes take the node's token; the node's identity is the token,
never a field in the body.

```
POST /admin/items              GET  /admin/board          GET  /admin/why/{id}
POST /admin/endpoints          GET  /admin/endpoints      POST /admin/endpoints/{id}/probe
POST /admin/models             PATCH /admin/models/{id}   GET  /admin/spend/today
PUT  /admin/spend/ceiling      GET  /admin/corrupt-rows
GET  /admin/approvals          POST /admin/approvals/{id}   (HTML or JSON)
POST /admin/watches            GET  /admin/watches          GET/PATCH /admin/watches/{id}
POST /admin/threads            GET/PATCH /admin/threads/{id}          POST /admin/threads/{id}/messages
GET  /threads/{id}             POST /threads/{id}/messages
POST /nodes/register           POST /nodes/heartbeat
POST /claim                    POST /runs/{id}/start        POST /runs/{id}/report
POST /runs/{id}/approval       POST /runs/{id}/checkpoint
GET  /version
```

A thread is a conversation with a budget: each post reserves `estimated_prompt + max_output`
against it, is refused (and freezes the thread) when that does not fit, and settles the
poster's reported tokens. Two agents alternate; the operator posts as `admin` and may raise a
frozen thread's budget.

## Documents

- [Design spec](docs/superpowers/specs/2026-11-15-nightshift-manager-design.md) — the
  authoritative description: schema, claim protocol, placement, providers, budgets,
  distribution, failure matrix.
- Implementation plans, one per milestone, each written as executable tasks:
  [M0/M1](docs/superpowers/plans/2026-11-15-m0-m1-foundation.md),
  [M2](docs/superpowers/plans/2026-11-15-m2-providers-and-execution.md) and its
  [carryover](docs/superpowers/plans/2026-11-16-m2-carryover.md),
  [M3](docs/superpowers/plans/2026-11-16-m3-escalation-and-approvals.md),
  [M4](docs/superpowers/plans/2026-11-17-m4-watches.md),
  [M5](docs/superpowers/plans/2026-11-21-m5-threads-and-budgets.md),
  [M6](docs/superpowers/plans/2026-11-21-m6-steward.md) (done),
  [M7](docs/superpowers/plans/2026-11-21-m7-agents-file-work.md) (planned).
- Plans — the milestone-by-milestone build, each with its acceptance tests.
- Design memos — proposals that are not scheduled:
  [read-only replica](docs/superpowers/designs/2026-11-21-read-only-replica.md).
- Proofs — what was run against the real binaries, and what it found:
  [M4 watches](docs/superpowers/proofs/2026-11-21-m4-watches-proof.md),
  [M5 threads](docs/superpowers/proofs/2026-11-21-m5-threads-proof.md).

## Development

```sh
make test      # go test ./..., after a check that iCloud has not dropped "name 2.go" copies
make race      # the same under -race
make vet
make release   # six targets into dist/, checksummed
```

## Reading order

Start with §1–§4 of the spec (what it is, why, decisions, non-goals), then §5–§9 (data model
through placement) — that is the load-bearing half. Everything after §10 is machinery.

## Conventions

- One static Go binary, `nsm`, with subcommands. `CGO_ENABLED=0`; SQLite via
  `modernc.org/sqlite` so the six-target matrix (darwin/linux/windows × amd64/arm64)
  cross-compiles in one command.
- The manager never carries prompt content. It sees titles, briefs, result summaries and, for
  threads, the messages the operator and agents choose to post.
- Every state change is one transaction per mutation; read-then-write takes `BEGIN IMMEDIATE` on
  a dedicated connection, because a deferred transaction loses its upgrade under contention.
- The kernel sandbox is best effort: macOS Seatbelt when `sandbox-exec` can apply a profile,
  otherwise the tool layer is the boundary and the agent says so in its log.
- Tests are the contract, and every load-bearing assertion has been mutation-checked: a test that
  cannot fail on the defect it names is a test that has not been written yet.

## License

MIT — see [LICENSE](LICENSE).
