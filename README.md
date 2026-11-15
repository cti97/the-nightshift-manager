# The Night Shift Manager

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
| Raspberry Pi 5 | `steward` | yes | Reflex work, LAN presence, endpoint canary, read-only replica |

Inference is not necessarily local to a node. A cloud Ollama, llama.cpp, or any
OpenAI-compatible endpoint is a first-class participant with a declared network reach.

## Status

**Design stage.** No code yet. The design is settled enough to implement and is the contract
that the implementation plans argue from.

## Documents

- [Design spec](docs/superpowers/specs/2026-11-15-nightshift-manager-design.md) — the
  authoritative description of the system: schema, claim protocol, placement, providers,
  budgets, distribution.
- [Implementation plan](docs/superpowers/plans/2026-11-15-m0-m1-foundation.md) — M0 (board
  foundation) and M1 (claim loop), written as executable tasks.

## Reading order

Start with §1–§4 of the spec (what it is, why, decisions, non-goals), then §5–§9 (data model
through placement) — that is the load-bearing half. Everything after §10 is machinery.

## Conventions

- One static Go binary, `nsm`, with subcommands. `CGO_ENABLED=0`; SQLite via
  `modernc.org/sqlite` so the six-target matrix cross-compiles in one command.
- The manager never carries prompt content. It sees titles, briefs and result summaries.
- Release artifacts are built into `dist/` (never committed) and served by the manager at
  `/dist`, verified against a signing key pinned at install time.
