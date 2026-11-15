# The Night Shift Manager — M2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the board able to actually run a task against a real model — a provider registry that treats Anthropic, OpenAI-shaped servers and Ollama as peers; endpoints the operator can register and health-check; a sandboxed six-tool agentic loop; and cost accounting with a ceiling that can stop the shift.

**Architecture:** `internal/provider` holds an interface, a registry, and a normalized error taxonomy; each adapter is one file plus one registry line. `internal/board` grows endpoint/model CRUD and health probing over the tables that already exist. `internal/agent` gains `AgenticExecutor`, which runs the tool loop inside a sandbox and reports through the same client as the stub.

**Tech Stack:** Go 1.24+, stdlib only (no provider SDKs — the APIs are three HTTP shapes), `modernc.org/sqlite`, `net/http/httptest` for the conformance suite.

**Spec:** `docs/superpowers/specs/2026-11-15-nightshift-manager-design.md` (§6 capabilities, §9 placement, §10 worker, §19 weaknesses)

**Predecessor:** `docs/superpowers/plans/2026-11-15-m0-m1-foundation.md`, whose "Corrections applied during execution" section is required reading before writing code here.

## A note on this plan's shape

The M0/M1 plan transcribed every function. Eleven defects in that plan were in the transcribed code, not the design — and each one cost a fix round. This plan therefore gives **interfaces, invariants and exact acceptance tests**, and leaves function bodies to the implementer, who now has a codebase full of patterns to follow. Where a shape is load-bearing (the provider interface, the error enum, the tool contract, the sandbox profile) the code is given.

## Global Constraints

- **Module path** `github.com/cti97/the-nightshift-manager`; **Go floor `1.24`**; **`CGO_ENABLED=0`**.
- **No provider SDKs.** All three shapes are HTTP. Adding a dependency here needs a reason stronger than convenience.
- **`time.Now()` appears in exactly one file, `internal/board/clock.go`.** Everything else takes a `Clock`.
- **No prompt content ever transits the board.** It sees titles, briefs, result summaries and paths.
- **Tests are stdlib `testing` only**; fakes are hand-written; no assertion libraries.
- **`make test` runs `check-tree` first** — the repo lives in an iCloud-synced folder that drops duplicate source files into the tree.
- Every state change writes an `AppendEvent` inside the same transaction.
- `no_egress`/`strict_local` are enforced *twice*: by placement, and by the adapter before its first request.

## File map

| File | Responsibility |
|---|---|
| `internal/provider/provider.go` | `Provider` interface, `Request`/`Response`, `Usage`, `ToolCall`, error taxonomy |
| `internal/provider/registry.go` | Name → constructor; `Register`, `New`, `Names` |
| `internal/provider/stub.go` | Scripted provider; the reference implementation |
| `internal/provider/openai.go` | OpenAI-compatible chat completions (covers llama.cpp, vLLM, LM Studio, Groq, OpenRouter) |
| `internal/provider/ollama.go` | The OpenAI shape plus `/api/tags` discovery |
| `internal/provider/anthropic.go` | Messages API plus tool use |
| `internal/provider/conformance.go` | The suite every provider must pass |
| `internal/provider/estimate.go` | Token estimation contract and the default heuristic |
| `internal/board/endpoints.go` | Endpoint/model CRUD, health states, the canary probe |
| `internal/agent/sandbox.go` | Run directory, environment scrubbing, `sandbox-exec` profile |
| `internal/agent/tools.go` | The six tools, their schemas, and dispatch |
| `internal/agent/agentic.go` | `AgenticExecutor`: the loop, caps, cost accounting |
| `internal/board/spend.go` | Cost ledger, daily ceiling, board pause |

## Task index

| # | Deliverable |
|---|---|
| 1 | `provider` interface, registry, and the stub provider |
| 2 | The conformance suite, and the stub must pass it |
| 3 | OpenAI-compatible provider |
| 4 | Ollama provider |
| 5 | Anthropic provider |
| 6 | Error taxonomy mapped to retry policy |
| 7 | Token estimation contract |
| 8 | Endpoint and model CRUD |
| 9 | Health probing with `warming` and `auth_failed` |
| 10 | Sandbox |
| 11 | The six tools |
| 12 | `AgenticExecutor` |
| 13 | Cost accounting and the daily ceiling |
| 14 | Carry-forwards from M1: corrupt-row tolerance, randomized invariants |

---

## Task 1: Provider interface, registry, stub

**Files:** `internal/provider/{provider,registry,stub}.go` + tests.

**Interfaces:**

```go
type Message struct {
	Role    string // system | user | assistant | tool
	Content string
	Name    string
}

type ToolDef struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

type Call struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

type Request struct {
	Model    string
	System   string
	Messages []Message
	Tools    []ToolDef
	MaxTokens int
	// Reach is the endpoint's declared reach class. A provider must refuse to
	// send when the caller requires host/lan and Reach is wan (spec §10).
	RequireLocal bool
	Reach        string
}

type Usage struct{ In, Out int }

type Response struct {
	Content   string
	ToolCalls []Call
	Usage     Usage
	Stop      string
}

type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request) (Response, error)
	EstimateTokens(req Request) int
	Probe(ctx context.Context) (Health, error)
}

type Registry struct{ /* unexported */ }
func (r *Registry) Register(name string, ctor func(Config) (Provider, error))
func (r *Registry) New(cfg Config) (Provider, error)
func (r *Registry) Names() []string
```

`Config` carries `BaseURL`, `APIKey`, `HTTPClient`, `Timeout`, `Reach`.

**Acceptance:**
- `TestRegistryRejectsUnknownProvider` — `New` on an unregistered name errors and names the name.
- `TestRegistryRejectsDuplicateRegistration` — registering `openai` twice panics or errors loudly; choose one and say which in the report.
- `TestStubReplaysAScriptedScript` — the stub returns pre-scripted responses in order and errors when exhausted.
- `TestStubRefusesWhenLocalRequiredButReachIsWAN` — `RequireLocal` + `Reach=="wan"` errors before returning content. This is the `no_egress` guarantee at the provider layer; the test is the one that makes §10 true.
- `TestEstimateTokensIsMonotonic` — a longer request never estimates fewer tokens.

**Notes:**
- The interface is the seam the conformance suite and the executor both depend on. Do not add methods later without a task that adds a conformance check for them.
- `Probe` returns a `Health` value, not a bool: `ok`, `warming`, `down`, `auth_failed`.

---

## Task 2: Conformance suite

**Files:** `internal/provider/conformance.go` + `conformance_test.go`.

The suite is a function other packages call, so a new adapter cannot skip it:

```go
// Conformance runs the checks every provider must pass against h. A provider
// that fails here does not exist (design §10).
func Conformance(t *testing.T, newProvider func() Provider)
```

Six checks, each its own subtest:
1. a plain completion returns content and non-zero `Usage`
2. a tool-call round trip: emits a `Call`, accepts a tool result, finishes
3. `Usage` is stable across two identical runs
4. `EstimateTokens` is within ±40% of actual
5. error mapping: a 401 becomes `ErrAuth`, a 429 `ErrRateLimited`, a context-too-long body `ErrContextOverflow`, a 500 `ErrServer`
6. refuses when `RequireLocal` is set and `Reach` is `wan`

**Acceptance:** `TestStubPassesConformance` runs the suite against the stub through a fake transport. Then a deliberately broken fake provider must fail at least three of the six — prove the suite has teeth, and say which three in the report.

---

## Task 3: OpenAI-compatible provider

**Files:** `internal/provider/openai.go` + `openai_test.go`.

`POST {base}/v1/chat/completions` with `tools`. Must handle `tool_choice`, assistant messages
carrying `tool_calls`, and tool results sent as `role:"tool"` with the matching `tool_call_id`.

Covers OpenAI, `llama-server`, vLLM, LM Studio, Groq and OpenRouter. **Do not** special-case any
of them — they are `openai` rows with different `base_url`s.

**Acceptance:**
- passes `provider.Conformance` against an `httptest` server speaking this shape
- `TestOpenAISendsToolsAsFunctionObjects` — the request body carries a `tools` array of
  `{"type":"function","function":{...}}`, not the provider interface's own shape
- `TestOpenAIDecodesToolCallsVerbatim` — `arguments` survives decode byte-for-byte, because the
  tool layer will hand it back to `json.Unmarshal`
- `TestOpenAIMapsAnErrorBodyToATaxonomySentinel` — a 429 body becomes `ErrRateLimited`

## Task 4: Ollama provider

**Files:** `internal/provider/ollama.go` + `ollama_test.go`.

The OpenAI shape plus `GET /api/tags` for `Probe` and model listing.

**Acceptance:**
- passes `provider.Conformance`
- `TestOllamaReportsWarmingWhileAModelLoads` — a 503 whose body indicates loading yields
  `Health.warming`, **not** `down`. This is the distinction the design's health states exist for:
  a llama.cpp or Ollama server that is loading a model is not broken, and marking it `down`
  removes it from placement for the whole load.
- `TestOllamaProbeListsModelsFromTags`

## Task 5: Anthropic provider

**Files:** `internal/provider/anthropic.go` + `anthropic_test.go`.

`POST {base}/v1/messages`. `system` is a **top-level field**, never a message. Tools carry
`input_schema`. Tool use arrives as `tool_use` content blocks and tool results go back as
`tool_result` blocks inside a **user** message. Requires `anthropic-version` and `max_tokens`.

**Acceptance:**
- passes `provider.Conformance`
- `TestAnthropicKeepsSystemOutOfTheMessages` — the request has a top-level `system` and no
  `role:"system"` entry
- `TestAnthropicSendsToolResultsAsUserBlocks` — a tool result is a `tool_result` block in a user
  message, not an OpenAI-shaped `role:"tool"` message
- `TestAnthropicDecodesToolUseBlocks` — a `tool_use` block becomes a `Call` with its `id` and
  `input` preserved

## Task 6: Error taxonomy → retry policy

**Files:** `internal/provider/errors.go` + `internal/agent/agentic.go` (the retry decision).

```go
var (
	ErrAuth            = errors.New("auth")
	ErrRateLimited     = errors.New("rate limited")
	ErrContextOverflow = errors.New("context overflow")
	ErrModelMissing    = errors.New("model missing")
	ErrTransport       = errors.New("transport")
	ErrBadRequest      = errors.New("bad request")
	ErrServer          = errors.New("server")
)
```

**Acceptance:**
- `TestRetryableClassifiesEveryError` — a table over all seven plus `nil`; exactly `transport`, `server`, `rate_limited` retry.
- `TestContextOverflowIsNotRetriedAndIsTaggedAsAPlacementBug` — the error surfaces with the placement reason attached, because a context overflow means placement computed the fit wrongly (design §9).

---

## Task 7: Token estimation

**Files:** `internal/provider/estimate.go`.

Default `EstimateTokens` is a heuristic over message and tool text; families may override. The channel budget (M5) will reserve at 1.2× this estimate, so the contract is documented here and consumed later.

**Acceptance:** monotonicity, a floor of 1 for a non-empty request, and a table asserting the heuristic stays within a factor of two of a hand-counted sample.

---

## Task 8: Endpoint and model CRUD

**Files:** `internal/board/endpoints.go` + admin routes.

Routes: `POST /admin/endpoints`, `GET /admin/endpoints`, `POST /admin/models`, `PATCH /admin/models/{id}`.

**Acceptance:**
- Creating an endpoint validates `reach ∈ {host,lan,wan}` and `provider` against the registry's names — a typo is rejected at write time, the same rule capabilities follow.
- A model's `features` are validated against `tools`, `vision`, `json_schema`, `streaming`.
- `GET` returns endpoints with their models and current health.
- Deleting nothing: `enabled=0` is the disable path.

---

## Task 9: Health probing

**Acceptance:**
- `Probe` runs on a schedule and on demand (`POST /admin/endpoints/{id}/probe`).
- A 503 while a model loads sets `warming` with a reason; a 401 sets `auth_failed`; a timeout sets `down`. Each is pinned by a test that would fail if the state were collapsed to `down`.
- Placement refuses `warming` and `auth_failed` (already true — the eligibility rule requires `ok`), and `/why` names the state, because `Eligible` now reads it.

---

## Task 10: Sandbox

**Files:** `internal/agent/sandbox.go`.

**Contract** (from design §10, which is explicit that this is a blast-radius reducer and not a boundary):
- one directory per run under a configured root, created fresh per attempt
- `env -i` with a minimal environment; `HOME` overridden into the run dir
- no keychain, no ssh agent, no credentials
- on macOS a generated `sandbox-exec` profile: read system paths, write only the run dir, network only when the item declares `network`
- a `Close` that removes the directory unless `KeepArtifacts` is set

**Acceptance:**
- `TestSandboxCannotReachTheHomeDirectory` — a `read_file` of `$HOME/.ssh/id_rsa` (or any path outside the run dir) fails. Skip with a stated reason if `sandbox-exec` is unavailable, and say so in the report rather than silently passing.
- `TestSandboxScrubsTheEnvironment` — a shell tool cannot see a variable that was set in the parent process.
- `TestSandboxIsRemovedOnClose` and `TestSandboxIsKeptOnRequest`.

---

## Task 11: The six tools

```go
type Tool struct {
	Name   string
	Schema json.RawMessage
	Run    func(ctx context.Context, sb *Sandbox, raw json.RawMessage) (string, error)
}
```

`read_file`, `write_file`, `list_dir`, `shell`, `http_get`, `finish`.
`write_file` and `list_dir` are confined to the sandbox; `shell` runs with the sandbox as cwd and a timeout; `http_get` is refused unless the item declares `network`; `finish` ends the loop.

**Acceptance:**
- a table test per tool, including the refusal paths: writing outside the sandbox, `shell` past its timeout, `http_get` without the capability
- `TestToolsRefusePathTraversal` — `../../etc/passwd` in any path argument is refused, not cleaned into a legal path
- the schemas are valid JSON and every tool has a description (a model with no description will misuse it)

---

## Task 12: `AgenticExecutor`

**Files:** `internal/agent/agentic.go`.

Implements `agent.Executor` (already defined in M1, taking `board.ClaimedItem`). Loop: build messages from the brief, call the provider, dispatch tool calls, stop on `finish` or a cap.

**Caps:** `maxSteps` (default 40) **and** a token budget; whichever trips first ends the run with a result that says which cap tripped.

**Acceptance:**
- `TestExecutorStopsAtTheStepCap` and `TestExecutorStopsAtTheTokenBudget` — each names the cap that fired.
- `TestExecutorReportsACapTripAsAnErrorNotASuccess` — an item that ran out of steps is `failed` with a reason, so the operator sees it rather than a truncated "done".
- `TestExecutorRoutesThroughTheProvidersErrorTaxonomy` — a `rate_limited` retries; an `auth` failure fails the run once with the provider named.
- The M1 end-to-end test is re-run with `AgenticExecutor` and the stub provider instead of `StubExecutor`, and still goes `queued → done`.

**Carry-forward from M1's final review:** the report-retry window is ~29 s. With a real executor a dropped report now means *executed work discarded*, so the executor must not swallow a report failure: log it, and let the retry do its job.

---

## Task 13: Cost accounting and the ceiling

**Files:** `internal/board/spend.go`.

Every run records `cost_estimate` from `Usage` × the model's per-Mtok prices. `GET /admin/spend/today` sums them. A config key `daily_spend_ceiling` (0 = unlimited) is checked before a claim is issued: at or above it, `Claim` returns nothing and the board sets itself `paused` with a reason.

**Acceptance:**
- `TestSpendSumsOnlyTodaysRuns` — a run from yesterday does not count.
- `TestCeilingStopsClaimsAndPausesTheBoard` — with a ceiling of 1 unit and 1 unit spent, `Claim` returns zero items and the board's status reads `paused` with a reason naming the ceiling.
- `TestCeilingOfZeroMeansUnlimited`.
- The spend is computed from `runs`, not from a mutable counter — a counter can drift from the ledger and the ledger is the truth.

---

## Task 14: M1 carry-forwards

1. **Corrupt-row tolerance in the claim path.** M1's final review found that one malformed `needs` cell 500s every node's `/claim`. Decode failures on a *row* must skip-and-log that row, not abort the call; `/why` already returns 500 naming the column, and the claim path should name the row.
   *Acceptance:* `TestClaimSkipsACorruptRowAndStillClaimsTheNext` — two queued items, the first with a corrupt `needs`, the second valid; the caller gets the valid one and the corrupt one is logged and left `queued`.
2. **Randomized invariants** (design §17.2 and §17.5, owed since M1): a seeded loop of ≥200 iterations of claim / expire / reap asserting (a) an item never has two non-terminal runs, and (b) no `no_egress` item is ever claimed against a `wan` endpoint.
   *Acceptance:* both loops run in under a second and fail loudly with the seed and the violating ids printed.

---

## Verification for the milestone

```bash
make test          # includes check-tree; all packages green
make race
go vet ./...
NSM_ADMIN_TOKEN=a NSM_ENROLL_TOKEN=b ./bin/nsm manager serve --db /tmp/m2.db &
curl -s -X POST localhost:8081/admin/endpoints -H "Authorization: Bearer a" ... # an endpoint
```

And one manual proof that M2 changed something real: an item claiming against a `stub` provider with a scripted tool call reaches `done` with a `transcript_path` and a non-zero `cost_estimate`.

## What M2 deliberately does not do

- No `request_approval` tool (M3), no watches (M4), no threads (M5), no steward or install path (M6).
- No streaming. The loop does not need it and nothing watches a live run yet.
- No model discovery beyond `Probe`'s list — models are declared, because their `features` have to be.
