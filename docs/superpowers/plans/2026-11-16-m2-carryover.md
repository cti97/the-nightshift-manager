# M2 carry-over — dispositions

Every finding deferred during M0/M1/M2's reviews, with what happens to it. Written because
"all issues" needs a definition that survives the session that produced it.

Rule for inclusion: a finding gets an action if it is **reachable** or **cheap while the file is
open**. Anything that needs a different machine, a different milestone, or a design change is
dispositioned rather than listed as work someone might do later.

---

## Batch A — user-visible behaviour

| # | Finding | Why now |
|---|---|---|
| A1 | `/why` reports `paused` and `pause_reason` but not `paused_by` | Task 13 split pause ownership into two keys with different lifecycles; "who stopped this" is now load-bearing and the explainer cannot answer it |
| A2 | Corrupt rows are discoverable only by grepping a log | One deduplicated log line per distinct corruption per process. After log rotation a long-lived manager has no list of what is broken |
| A3 | `PUT /admin/spend/ceiling` maps storage failure to 400 | A locked or failing database reads as the operator's mistake |
| A4 | `/why` returns 500 for **every** item while any candidate row is corrupt | One bad `provides_live` cell makes the explainer useless board-wide. It should name the row and still explain the item it was asked about |

## Batch B — robustness

| # | Finding | Why now |
|---|---|---|
| B1 | A typed-nil `Provider` would panic inside `Registry.New` rather than being refused | Moves a panic to construction |
| B2 | A `NaN` price makes the ledger `NaN` and the ceiling never trips | `unpriced_runs` is blind to it because `cost_estimate = 0` is false for `NaN` |
| B3 | An empty or whitespace `daily_spend_ceiling` silently means unlimited | Contradicts the function's own doc, which says an unparseable value is an error |
| B4 | `reserve`'s `reservation < 1` clamp also masks an int overflow from an absurd estimate | A theoretical budget bypass in the one function that must not have one |
| B5 | A tool result over 64 KiB is refused, discarding the tool's error | A failing `shell` with large output loses `exit status 1`; the model is told only "over the cap" |
| B6 | `list_dir` materialises every entry before truncating at 500 | Output is bounded, memory is not |

## Batch C — accuracy

| # | Finding | Why |
|---|---|---|
| C1 | `MaxSteps` doc says it caps provider *calls*; retries multiply them by up to three | The loop is still bounded (40 × 3) |
| C2 | `Response.Stop` carries two vocabularies (Anthropic's and OpenAI's) and the interface documents neither | A later task must not branch on it |
| C3 | An empty assistant turn serialises `content: null` for Anthropic | The real API likely refuses it, so the nudge may never reach the model |
| C4 | The corrupt-row dedupe comment claims a repair re-reports; an identical re-corruption stays silent | Comment overstates one direction |
| C5 | `toolPath`'s comment says a backslash is "left alone" on Unix; the code refuses it on every OS | Comment contradicts code |
| C6 | Kernel-confinement parse detection keys on message text although the comment names exit 65 | Exact beats textual |
| C7 | `NSM_REQUIRE_KERNEL_SANDBOX` silently no-ops on non-darwin | Documented skip, not silent, but the doc lives in a test |
| C8 | `main.go` attributes the sandbox's scrubbed child environment to the `Unsetenv` scrub | `sb.Command` builds its own env; the clause misdescribes the layering |
| C9 | `by_model` returns the wire model name and endpoint id, not `models.id` | An operator cannot `PATCH` the price they were just told about |
| C10 | Migration 3 does not backfill `cost_estimate` | A mid-day upgrade counts that day's earlier spend as $0; one line in the upgrade note |
| C11 | The stored pause reason is write-only and can disagree with the reason served | Either surface it or say it is only the ownership marker |
| C12 | The conformance suite never calls `Probe` | Two adapters' `Probe` — the method each task existed for — is certified by nothing |
| C13 | `/why` reports `placed: true` for an item that is already terminal | Placement ignores item status; the explainer should say "already done" instead of implying it could run |

## Won't fix, with reason

| # | Finding | Reason |
|---|---|---|
| W1 | Kernel confinement has never run here (design weakness 17) | Environment: `sandbox_apply` is denied inside the build shell. The command to verify it on an unconfined machine is in the design |
| W2 | A Linux node holds provider keys while a model-authored shell runs (weakness 18) | Needs real kernel confinement or a key-holding process separate from the shell; both larger than M2 |
| W3 | Reserved-token pricing under-counts a failed attempt (weakness 19) | A deliberate trade: pricing an unobserved `max_tokens` at the output rate would let a reservation dominate the day |
| W4 | `conformance.go` imports `testing` from a non-test file | Plan-mandated: the suite must be callable from another package. Nothing imports the package in a binary yet |
| W5 | `Claim` does not check `attempt < max_attempts` | Deliberate: the reaper owns the budget, and a queued row above its limit would otherwise be stranded. Recorded as a contract between the two |
| W6 | Loop 1's seed replays the decision stream, not the interleaving | Property-test posture: any observed failure is still a real bug, and seed plus violating ids are printed |
| W7 | `make release` does not gate on tests | Carried to M6, where the release channel lands |
| W8 | Migrating to iCloud-duplicate detection in `b10coincom`'s `check-strays.sh` | Different repo, its own CI and audit history; flagged to the operator, not changed here |

---

## Order

Batch A, then B, then C — each a single dispatch, reviewed as one unit. Then M3's plan.

Batches are grouped by *what a reviewer must hold in their head*, not by file: A spans board and
HTTP, B is defensive code in four files, C is nearly all prose.
