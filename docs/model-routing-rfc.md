# Task-Aware Model Routing — Analysis & RFC

**Status:** Proposal (analysis + design only). **No production behavior change in
this PR.** A follow-up implementation issue is required before any of Part 2
ships — see [Rollout](#28-rollout) and [Non-goals](#210-non-goals--follow-up).

**Refs:** #781.

This document has two parts. Part 1 is a current-state map of how a backend, a
model, and a reasoning effort are selected for a worker today, with `file:line`
citations. Part 2 is the RFC: a task-signal-driven selection design that keeps
`model.default` and the `model:<label>` override working unchanged.

---

## Part 1 — How selection works today

### 1.1 The configuration inputs

Every lever that influences which backend/model/effort a worker runs on lives in
`internal/config/config.go`:

| Input | Type / fields | Citation |
| --- | --- | --- |
| `model.default` | the backend used when nothing else fires | `internal/config/config.go:279` (`ModelConfig.Default`) |
| `model.backends[*]` | `cmd`, `extra_args`, `prompt_mode`, `enabled`, `provider`, `model`, `variant`, `effort`, `pricing`, `quota`, `non_agentic`, `subagent_hint`, `mcp` | `internal/config/config.go:26-90` (`BackendDef`) |
| `model.fallback_backends` | ordered list tried when the current backend is *blocked* | `internal/config/config.go:281` |
| `model.provider_lanes` | ordered provider defaults plus provider-local fallback chains; used when no explicit `fallback_backends` override is set | `internal/config/provider_routing.go` |
| `routing.mode` | `"manual"` (default) or `"auto"` | `internal/config/config.go:786`, default set at `internal/config/config.go:1429-1430` |
| `routing.router_model` / `routing.router_model_name` | which backend + model id runs the LLM router | `internal/config/config.go:787-788`, defaults at `:1432-1436` |
| `routing.router_prompt` | router prompt template | `internal/config/config.go:789` |
| `routing.task_type_backends` | `task_type → backend`, used **only** when `mode: auto` | `internal/config/config.go:790` |
| `routing.{planner,implementation,validator}_backend` | per-role backend overrides | `internal/config/config.go:794-796` |
| `pipeline.{planner,implementer,validator}.{backend,effort}` | the *other* per-role backend/effort overrides (implement phase + `effort:` added in #841) | `internal/config/config.go` (`PipelineConfig`/`RoleConfig`) |
| `supervisor.review_repair.{backend,model,effort}` | backend used when a green PR is held on blocking review findings | `internal/config/config.go:543-548` |

A subtle but load-bearing fact, with one backend-specific exception: for the
first-class agentic backends (claude/codex/gemini) **`BackendDef.model`,
`.variant`, `.effort`, and `.provider` are attribution metadata, not worker
inputs** — documented as "optional per-backend attribution metadata" used by the
dashboard and the commit trailer (`internal/config/config.go:33-42`). **The Pi
backend is the exception:** its `Provider` and `Model` *are* live worker inputs —
they are copied into `worker.BackendConfig` (`internal/worker/worker.go:115-116`)
and `piBackend.BuildCmd` turns them into `--provider` / `--model` argv
(`internal/worker/backend.go:202-207`). `Variant` is consumed by no worker
builder, and `Effort` is ignored by every worker builder (including Pi); both
stay pure attribution metadata for all backends. See §1.2 for where each worker's
actual model and effort come from.

### 1.2 The fresh-dispatch path (the common case)

When the orchestrator decides to spawn a worker for an issue, it resolves the
backend in the dispatch loop:

1. `Orchestrator.resolveDispatchBackend` calls `resolveBackendDecision`, then
   gates the result on `BackendHealth` so a fresh dispatch never lands on a
   disabled or cooling-down backend — `internal/orchestrator/backend_selector.go:213-240`,
   wired in at `internal/orchestrator/orchestrator.go:5272`.
2. `resolveBackendDecision` delegates to the router —
   `internal/orchestrator/orchestrator.go:4603-4605`.
3. `Router.ResolveBackendDecision` applies a **3-tier priority** —
   `internal/router/resolve.go:77-134`:
   1. `model:<backend>` label on the issue (highest priority) —
      `internal/router/resolve.go:79-88`, label parsing at
      `internal/router/resolve.go:40-49` (`BackendFromLabels`).
   2. Auto-routing via the LLM router, only if `routing.mode == "auto"` —
      `internal/router/resolve.go:91-130` (see §1.3).
   3. `model.default` — `internal/router/resolve.go:133`.
4. The orchestrator stamps the chosen backend + reason onto the session as a
   `BackendSelection` audit record — `internal/orchestrator/orchestrator.go:5333-5338`
   — and spawns the worker via `startWorker` → `worker.BuildWorkerCmd`.

**Where the model and effort actually come from.** `BuildWorkerCmd`
(`internal/worker/backend.go:490-497`) dispatches to a per-CLI builder. The
worker builders for the first-class agentic backends —
`claudeBackend.BuildCmd` (`internal/worker/backend.go:83-114`),
`codexBackend.BuildCmd` (`internal/worker/backend.go:120-146`),
`geminiBackend.BuildCmd` (`internal/worker/backend.go:152-167`) — assemble argv
from `cfg.Cmd` (split into binary + prefix args by `splitCmd`,
`internal/worker/backend.go:17-23`) plus `cfg.ExtraArgs`. **None of these three
(nor the `cline`/generic builders) consult `cfg.Model` or `cfg.Effort`.** So for
them a worker's model and reasoning effort are whatever the operator baked into
the backend's `cmd`/`extra_args` strings, e.g.

```yaml
model:
  backends:
    codex:
      cmd: "codex --model gpt-5.5 -c model_reasoning_effort=medium"
```

**One worker exception: Pi.** `piBackend.BuildCmd`
(`internal/worker/backend.go:194-217`) *does* read `cfg.Provider` and `cfg.Model`
and emit them as `--provider` / `--model` argv (`internal/worker/backend.go:202-207`)
— Pi is a multi-provider shim that selects its model at the CLI rather than
baking it into a fixed `cmd`. So for a Pi-backed worker the per-backend
`model`/`provider` fields are live worker inputs, not just attribution: a future
tier that targets a Pi backend can already steer the model through
`BackendDef.Model`, whereas a codex/claude/gemini tier cannot without editing
`cmd`/`extra_args` (§2.4 step 5). Pi still ignores `cfg.Effort`.

The only place `cfg.Effort` (and `cfg.Model` for the non-Pi backends) is turned
into a `--model`/`--effort` flag is `appendModelOptions`
(`internal/worker/backend.go:286-295`), and it is called **only** by
`BuildSupervisorCmd` (the read-only supervisor/decision path,
`internal/worker/backend.go:504-603`) — never on the worker path. This is the
mechanical root of the issue's symptom: outside the Pi exception there is no
per-issue model/effort lever short of editing the backend `cmd` or applying a
`model:` label that swaps to an entirely different backend whose `cmd` happens to
pin a stronger model.

### 1.3 What `routing.mode: auto` actually does — and why it is unused

When `routing.mode == "auto"`, step 2 of §1.2 invokes the LLM router:

- `Router.RouteDecision` builds a prompt (`internal/router/router.go:76-122`)
  from the template `defaultRouterPrompt` (`internal/router/router.go:14-21`),
  which asks the model to return JSON `{"backend", "task_type", "reason"}`.
- The **task taxonomy** is a fixed 7-type set: `refactor`, `bugfix`, `test`,
  `vision`, `design`, `docs`, `infra` —
  `internal/router/router.go:23-31`, validated by `IsTaskType`
  (`internal/router/router.go:202-209`).
- The router shells out to `routing.router_model` /
  `routing.router_model_name` (`internal/router/router.go:125-154`) and parses
  the JSON, tolerating surrounding prose (`internal/router/router.go:175-192`).
- Back in `ResolveBackendDecision`, a returned `task_type` is mapped through
  `routing.task_type_backends` before the router's free-text backend pick is
  used — `internal/router/resolve.go:107-124`.
- Any failure (model error, parse error, unknown backend) falls back to
  `model.default` but tags the reason `router_error` so operators can tell a
  silent fallback from a deliberate default —
  `internal/router/router.go:80-97`, `internal/router/resolve.go:101-129`. The
  orchestrator even surfaces `router_error` as a notification —
  `internal/orchestrator/orchestrator.go:5298-5301`.

**Why it is effectively unused in practice:**

1. The default mode is `manual` (`internal/config/config.go:1429-1430`), so a
   project gets auto-routing only by explicitly opting in.
2. The taxonomy classifies *task kind* (refactor/bugfix/docs/…), which maps to a
   *backend*, not to *difficulty/risk/effort*. It cannot express "this is a hard
   migration, use a stronger model" — the exact need in the motivating symptom.
3. It costs an extra LLM call (and latency) per issue, and is nondeterministic.
4. The config validator actively warns about the common shape — multiple
   backends configured but `mode: manual` and no role backends — calling out
   that selection is "by `model:<name>` label or `model.default` only, not by
   task content" (`manualRoutingLabelPinWarning`,
   `internal/config/config.go:1800-1837`). That warning is the codebase's own
   acknowledgement that task-based routing is not happening today.

### 1.4 Per-role backends: two parallel mechanisms, one of them dead

There are **two** independent per-role backend features, and they do not share
code:

- `routing.{planner,implementation,validator}_backend` is consumed by
  `Router.ResolveBackendForRole` (`internal/router/resolve.go:136-182`,
  `roleBackend` at `:136-148`). **This function has no non-test callers** — it is
  exercised only by `internal/router/resolve_test.go`. It is dead code on the
  dispatch path.
- `pipeline.{planner,implementer,validator}.backend` is consumed by
  `pipeline.BackendForPhase` (`internal/pipeline/pipeline.go`), which **is** wired
  into the dispatch loop for pipeline phases. Since #841 the implement phase
  carries its own `pipeline.implementer.backend` (empty falls back to
  `model.default`, the unchanged default), and every phase role also takes an
  optional `effort:` threaded into the worker argv via
  `worker.appendTierModelEffort` (claude `--effort`, codex `-c
  model_reasoning_effort`; gemini drops it). This enables the "plan-big /
  execute-small" economics — a strong backend + high effort on plan/validate and a
  cheap backend + low effort on the token-heavy implement phase:

  ```yaml
  pipeline:
    enabled: true
    planner:
      enabled: true
      backend: fable      # strong model plans
      effort: xhigh
    implementer:
      backend: codex      # cheap model executes the mechanical implement phase
      effort: low
    validator:
      enabled: true
      backend: fable      # strong model verifies
      effort: high
  ```

  Operator-pinned flags (a `--model`/`--effort` in the backend `cmd`/`extra_args`)
  still win — the per-phase effort is skipped when the flag is already present.

So the "per-role backend" that actually runs is the `pipeline.*` one, and only
for the phases of the opt-in 3-phase pipeline (`pipeline.enabled` /
`pipeline:full` label). The `routing.*_backend` fields parse and validate but
never affect a worker. Per-phase config is orthogonal to `routing.mode` — it is
phase config, not issue routing.

### 1.5 Failure, fallback, and escalation semantics

**`fallback_backends` swaps the backend only on a backend-level outage**, never
on a work-quality failure. The three trigger classes:

| Trigger | Detection | Action |
| --- | --- | --- |
| Provider rate-limit / quota | `recordProviderLimit` marks the backend cooling-down + `RateLimitHit` — `internal/orchestrator/backend_selector.go:69-95` | `selectProviderLimitFallback` → `selectBackendFallback` — `:134-200` |
| Backend auth / credential failure | `recordBackendFailure` (`:107-132`) | `selectBackendFallback` with `fallback_after_backend_auth_failure` |
| Model unavailable (model pulled/renamed/no access) | `recordBackendFailure` with `BackendBlockModelUnavailable` (copy at `:38-55`; detector `internal/worker/backendfailure.go:37-97`) | `selectBackendFallback` with `fallback_after_backend_model_unavailable` |

`selectBackendFallback` walks the exact route resolved by
`ModelConfig.ResolvedRoute`. A project-level `fallback_backends` chain is the
legacy explicit override. Otherwise `provider_lanes` composes each provider's
default and local fallbacks before moving to the next provider. With neither
configured, only `model.default` is eligible; backend-map iteration order is
never a fallback policy. Disabled, current, already-tried, and cooling-down
candidates are skipped. Fresh dispatch uses the same route, with wraparound only
to recover from an unavailable label/policy pin; a live outage fallback moves
forward and never cycles back to an earlier backend.

Example:

```yaml
model:
  provider_lanes:
    - provider: anthropic
      default: claude
    - provider: openai
      default: sol
      fallback_backends: [gpt55]
  backends:
    claude: {provider: anthropic, model: fable-5, effort: high}
    sol: {provider: openai, model: gpt-5.6-sol, effort: high}
    gpt55: {provider: openai, model: gpt-5.5, effort: high}
```

The effective route is `claude -> sol -> gpt55`. A model-specific cooldown is
keyed by backend, so cooling `sol` does not block `gpt55` even though both are in
the OpenAI lane.

All three trigger classes set `sess.RateLimitHit = true`
(`internal/orchestrator/backend_selector.go:79`, `:120`), which **excludes the
session from the per-issue retry budget**: `FailedAttemptsForIssue` counts only
`PRNumber == 0` dead/failed/retry-exhausted sessions where `!RateLimitHit`
(`internal/state/state.go:3331-3341`; combined with `sess.RetryCount` at
`internal/orchestrator/orchestrator.go:2506`). A backend outage is not the
issue's fault, so it does not burn retries.

**Fallback does NOT trigger on review-gate rejection, CI failure, or ordinary
retries:**

- A failed review gate holds the PR and waits — it does not schedule a worker
  retry or a backend swap (`internal/orchestrator/orchestrator.go:3129-3132`).
- A CI failure schedules an ordinary retry that **reuses the same backend** —
  the retry path respawns with `sess.Backend` and does not re-resolve from
  labels/router (`internal/orchestrator/orchestrator.go:1469-1471`; respawn does
  not call `ResolveBackend`). Consequence: applying a `model:` label *after* a
  retry is scheduled has no effect on that retry.

**Is there any "escalate to a stronger model on retry" today?** Almost none. The
single existing escalation is `spawn_review_repair` (#565): when a session is
*settled `retry_exhausted` on review feedback* and its PR is non-draft,
mergeable, CI-green, and has blocking reviewer findings on the current head, the
supervisor respawns a focused fixer on the **strongest backend** (default
`claude`) — `internal/supervisor/review_repair.go:31-115`,
`SupervisorReviewRepairConfig.EffectiveBackend` at
`internal/config/config.go:574-581`, dispatched at
`internal/orchestrator/orchestrator.go:5248-5260`. This is narrow: it is
backend-only (not an effort bump), it is gated behind full retry exhaustion, and
it is specific to review findings. There is no general "cheap-first, escalate on
failure" ladder, and no escalation at all for CI failures or plain
implementation retries.

### 1.6 Cost / latency / quality profile and observability

Maestro already models per-backend **cost** (`BackendPricing`,
`internal/config/config.go:133-162`) and **subscription capacity**
(`BackendQuota`, `internal/config/config.go:238+`, surfaced and used to steer
dispatch once usage crosses a threshold). It also flags **non-agentic**
text-completion backends that must not run as workers (`BackendDef.NonAgentic`,
`internal/config/config.go:54-64`) and ships a **sub-agent cost hint** to steer
orchestrating CLIs toward cheaper grunt models (`BackendDef.SubagentHint`,
`internal/config/config.go:82-89`). The pieces needed to reason about
cost/quality tradeoffs therefore already exist in config — they are just not fed
into selection.

The configured fleet today (per the issue) — codex `gpt-5.5`/medium, claude
`opus[1m]`, `deepseek-v4-flash-free`, pi-ollama `glm-5.2` — spans roughly two
quality/cost bands (strong-but-expensive vs. cheap/free), but every issue is
dispatched to the single `model.default` regardless of band.

**Observability of the decision** is recorded but shallow:

- `BackendSelection` audit record: `SelectedBackend`, `SelectionReason`,
  `TaskType`, `CandidateScores`, `HardPin`, `PreviousBackend` —
  `internal/state/state.go:104-123`; stamped at spawn
  (`internal/orchestrator/orchestrator.go:5333-5338`) and on the
  `maestro route`/dispatch CLI path (`cmd/maestro/main.go:2076-2078`).
- Canonical reasons: `label`, `role`, `auto`, `default`, `router_error`,
  `unknown_label_backend` (`internal/router/resolve.go:28-35`) plus the
  `fallback_after_*` reasons (`internal/orchestrator/backend_selector.go:14-22`)
  and `phase` / `review_repair`
  (`internal/orchestrator/orchestrator.go:5258`, `:5266`).
- Surfaced in `maestro status` (the `BACKEND` column,
  `cmd/maestro/main.go:1461`), the fleet API / Mission Control drawer
  (`internal/server/fleet.go:1152-1155`, `internal/server/server.go:381,416`),
  and the durable `Maestro-Backend:` commit/PR trailer
  (`internal/state/attribution.go:9-20`).

The gap: `SelectionReason` records **which lever fired**, not **why the task
warranted a strength**. And `CandidateScores.Fit`/`Policy` are constant
placeholders (`backendFitScore`/`backendPolicyScore` return `0.8/0.6` and
`0.9/0.6` based only on "is this the default backend",
`internal/orchestrator/backend_selector.go:344-356`), not signal-derived scores.

### 1.7 Current-state summary

Selection is effectively **static**: `model.default` for every issue, with three
manual/edge levers — a `model:<label>` override, backend fallback on outage, and
the narrow review-repair escalation. No input reflects task difficulty, size,
risk, or prior outcome. That is exactly why a trivial CI-workflow issue and a
heavy DB-migration issue both landed on `gpt-5.5`/medium, and the only fix was a
hand-applied `model:claude` label.

---

## Part 2 — RFC: task-aware selection

### 2.1 Problem statement

Model choice should reflect the *task*, not a single global default. Concretely:
a small, low-risk, leaf issue should run on a cheap backend at low effort; a
large, high-risk, foundational issue (migration, security, infra) should run on
a strong backend at high effort; and an issue that has already failed CI or a
review gate should be retried on something stronger than what just failed —
automatically, without an operator hand-applying a `model:` label.

### 2.2 Signals (and where the data already lives)

| Signal | Source today | Citation |
| --- | --- | --- |
| Explicit labels | `model:<name>`, `long-running`, `pipeline:full`, `epic`, risk labels | `internal/router/resolve.go:40-49`; `orchestrator.go:5304-5310` |
| Risk (migration / security / infra) | issue labels + title/body keywords; the `infra` router task-type | `internal/router/router.go:29-30` |
| Size / complexity | touched-files / LoC-delta hints from the issue body or a pre-worker scan; body length | pre-worker context phases `internal/config/config.go:863-867` (`research`, `test_mapping`) |
| Dependency depth ("foundation" vs "leaf") | the dependency graph the supervisor already builds for unblocking | `internal/supervisor/dependency_unblock.go` |
| Prior outcome (retry) | `sess.RetryCount`, `FailedAttemptsForIssue` | `internal/state/state.go:160`, `:3331-3341` |
| Prior outcome (review rejection) | blocking review findings on head | `internal/supervisor/review_repair.go:91-115` |
| Prior outcome (CI failure) | the CI-failure retry path | `internal/orchestrator/orchestrator.go:3140-3144` |
| Cost / capacity guardrails | `pricing`, `quota` | `internal/config/config.go:133-162`, `:238+` |

The key observation: every signal above is *already computed or trivially
derivable* somewhere in the codebase. The work is feeding them into selection,
not gathering them.

### 2.3 Design options

**Option A — Deterministic heuristic policy.** A small rules engine maps signals
to a "strength tier"; tiers map to backend + effort.
*Pros:* cheap (no extra LLM call), deterministic, debuggable, easy to unit-test,
easy to explain in `maestro status`.
*Cons:* rules are hand-tuned; coarse signals (label/size) may mis-rank a subtle
task.

**Option B — Revive/extend the LLM auto-router.** Extend `routing.mode: auto` so
the router classifies *difficulty + risk* (not just the 7 task types) and maps to
a tier.
*Pros:* reuses existing router plumbing (`internal/router/router.go`); flexible
on fuzzy issues.
*Cons:* per-issue LLM cost + latency + nondeterminism; it is already unused for
these reasons (§1.3); the classification can be confidently wrong with no audit
trail beyond `reason`.

**Option C — Hybrid (recommended).** Deterministic heuristics pick a tier
(Option A); the existing auto-router is an *optional* tie-breaker the operator can
enable; and — crucially — a **cheap-first, escalate-on-failure** ladder climbs a
tier when a task fails CI / review / retry. Per-role tiers and per-wave budget
caps are optional knobs.
*Pros:* deterministic and cheap by default, with an LLM escape hatch and a
self-correcting failure path that generalizes the existing review-repair
escalation (§1.5).
*Cons:* more config surface; escalation interacts with the retry budget and must
not loop.

**Recommendation: Option C.** It directly fixes the symptom, reuses existing
machinery (`fallback_backends`, `BackendHealth`, `BackendSelection`,
`spawn_review_repair`), and degrades to today's behavior when unconfigured.

### 2.4 Recommended design

1. **Strength tiers.** An ordered, named list, each tier = a backend (from
   `model.backends`) + an effort + an optional model override. Example:
   `cheap → standard → strong`. Tiers are the unit the rest of the design speaks
   in, so adding a backend does not require touching rules.
2. **Signal → tier rules.** A small, ordered, first-match rule list (deterministic
   Option A) maps `{labels, risk keywords, size/dependency hints}` to a starting
   tier. No match → `standard` (which defaults to `model.default`).
3. **Precedence (unchanged contract).** `model:<label>` override **>** policy
   tier **>** `model.default`. The label path (`internal/router/resolve.go:79-88`)
   stays the highest-priority, untouched lever, and an absent policy block means
   step 3 (`model.default`) is reached exactly as today.
4. **Cheap-first, escalate-on-failure ladder.** Dispatch at the signal-derived
   tier. On a CI failure, a review-gate rejection, or a plain retry, climb one
   tier on the next attempt — *bump effort first within the same backend, then
   the backend* — capped at the top tier and by the per-issue retry budget. This
   generalizes `spawn_review_repair` (§1.5) from "review findings → claude" to
   "any failure → next tier".
5. **Effort is a first-class field again.** Since worker builders ignore
   `cfg.Effort` today (§1.2), the implementation issue must thread the tier's
   effort into the worker argv for the relevant CLIs (e.g. codex
   `-c model_reasoning_effort=`, claude effort flag), or the tier must express
   effort purely via distinct backend entries. The RFC recommends the former so a
   single backend can serve multiple tiers by effort. A tier's optional *model*
   override is in the same boat: it is already wired for the Pi backend (which
   reads `cfg.Model` → `--model`, §1.2) but for codex/claude/gemini the
   implementation must thread it into argv the same way, or the tier must point at
   a distinct backend entry whose `cmd` pins the model.

### 2.5 Config schema (backward-compatible)

Nest the new fields under the existing `routing` block so they compose with
`mode`, `router_model`, and `task_type_backends`, and add a new
`routing.mode: policy` value. **When the block is absent or `mode != policy`, the
new fields are inert and selection is byte-for-byte today's behavior.**

```yaml
routing:
  mode: policy            # new value; "manual" (default) and "auto" unchanged
  tiers:
    cheap:
      backend: deepseek
    standard:
      backend: codex
      effort: medium      # threaded into the worker argv by the implementation issue
    strong:
      backend: claude
      effort: high
  policy:
    default_tier: standard
    rules:                # first match wins; deterministic
      - when: { labels: ["model:*"] }      # explicit override still wins above this
        tier: passthrough                  # documents that labels bypass the policy
      - when: { labels: ["security", "migration", "infra"] }
        tier: strong
      - when: { risk_keywords: ["migration", "schema", "auth"] }
        tier: strong
      - when: { size: large }              # touched-files / LoC-delta hints
        tier: strong
      - when: { dependency: foundation }   # has dependents in the wave graph
        tier: strong
      - when: { size: small, dependency: leaf }
        tier: cheap
    escalation:
      enabled: true
      on: [ci_failure, review_rejection, retry]
      step: tier+1        # effort bump within backend first, then backend
      max_tier: strong
    budget:
      max_strong_per_wave: 3   # cost cap; excess large tasks queue at standard
```

Compatibility rules the implementation must honor:

- `model.default` and `model:<label>` keep working unchanged
  (`internal/router/resolve.go:79-88`, `:133`). The label override is evaluated
  **before** the policy.
- Config validation/warnings are unaffected for existing configs. The
  `manualRoutingLabelPinWarning` (`internal/config/config.go:1816-1837`) should
  additionally treat `mode: policy` as a configured routing mechanism (so it does
  not warn when a policy block is present). New validation only fires on the new
  fields (e.g. a tier naming an unknown/`non_agentic`/disabled backend).
- Tiers reference existing `model.backends` entries, so `pricing`, `quota`,
  `non_agentic`, and `enabled` continue to apply and gate selection.

### 2.6 Escalation-on-failure behavior

The escalation ladder is the generalization of today's narrow review-repair
escalation (§1.5):

- **Triggers:** CI failure (`orchestrator.go:3140-3144`), review-gate blocking
  findings on head (`review_repair.go:91-115`), and ordinary retry
  (`orchestrator.go:1469-1471`). Each is opt-in via `escalation.on`.
- **Step:** climb one tier — bump effort within the current backend first, then
  switch backend — never exceeding `max_tier`.
- **Re-resolution on retry:** the retry path must re-resolve the tier instead of
  blindly reusing `sess.Backend` (today's behavior, `orchestrator.go:1469-1471`),
  so the escalated tier (and any newly-applied `model:` label) takes effect.
- **Loop safety:** escalation is bounded by `max_tier` and by the existing
  per-issue retry budget (`FailedAttemptsForIssue` + `RetryCount`,
  `state.go:3331-3341`). Backend-outage fallovers stay on the `RateLimitHit`
  path (§1.5) and are *not* counted as escalation triggers.
- **Interaction with `fallback_backends`:** outage fallback (§1.5) is orthogonal
  and unchanged — it answers "this backend is down", while escalation answers
  "this task needs more horsepower". The ladder reuses
  `backendFallbackCandidates`/`BackendHealth` gating
  (`backend_selector.go:313-342`) so an escalated tier still skips a
  cooling-down backend.

### 2.7 Observability

Make the decision self-explaining (closing the §1.6 gap):

- Populate `BackendSelection.SelectionReason` with the **deciding signal + tier**
  (e.g. `policy:strong (labels=migration)`), and make
  `CandidateScores.Fit`/`Policy` real signal-derived scores instead of the
  `0.5/0.8/0.9` placeholders (`backend_selector.go:344-356`). Optionally add a
  `Signals` breakdown field to `BackendSelection`
  (`internal/state/state.go:115-123`).
- Surface it where the reason already renders: the `maestro status` `BACKEND`
  column (`cmd/maestro/main.go:1461`), the fleet API
  (`internal/server/fleet.go:1152-1155`), and the dispatch log line
  (`orchestrator.go:5312`).
  - **Implementation status (#792):** the `maestro status` column, the fleet API
    JSON (`backend_selection` carries `tier`/`effort`/`model`/`shadow_tier` and
    the tier-derived candidate scores), and the dispatch log line are wired. The
    Mission Control **SPA drawer** does not yet render these new fields
    (`internal/server/web/mc/src/` never reads `backend_selection`); rendering
    them in the drawer is a tracked frontend follow-up, not part of #791/#792.
    The data is already exposed on the API for whoever wires the view.
- Keep the durable `Maestro-Backend:` trailer
  (`internal/state/attribution.go:9-20`) as the canonical backend timeline —
  escalation simply appends a new segment with its `EndReason`, which the trailer
  already models.

### 2.8 Rollout

- **Opt-in per project.** `routing.mode` defaults to `manual`
  (`internal/config/config.go:1429-1430`); `policy` mode is never implied.
- **Shadow mode first.** The implementation should log the tier it *would* pick
  (without changing the dispatched backend) so an operator can validate the rules
  against a real wave before enabling.
- **Dogfood on the maestro repo itself first.** maestro already self-deploys its
  own binary after merge (#698, `maestro.yaml.example:125-146`), so the maestro
  project is the natural first adopter: enable `mode: policy` here, watch a wave,
  tune the rules, then document a recommended starter policy in
  `maestro.yaml.example`.

### 2.9 Backward-compatibility confirmation

- **`model.default` keeps working.** With no policy block (or `mode != policy`),
  resolution falls through to `model.default` exactly as today
  (`internal/router/resolve.go:133`).
- **`model:<label>` keeps working.** The label override is evaluated first and is
  untouched by this design (`internal/router/resolve.go:79-88`); it remains the
  highest-priority lever and bypasses the policy.
- **No behavior change when unconfigured.** Every new field is additive and
  inert by default; existing configs parse, validate, and dispatch identically.

### 2.10 Non-goals / follow-up

- **No production behavior change in this PR.** This document is analysis +
  design only; nothing in Part 2 is implemented here.
- A follow-up **implementation issue** must be filed (and linked from #781)
  before any tiers/policy/escalation code lands. That issue owns: the
  `routing.tiers`/`routing.policy` schema + validation, threading effort into the
  worker argv (§2.4 step 5), the escalation ladder + retry re-resolution
  (§2.6), the observability fields (§2.7), and the shadow-mode rollout (§2.8).
- Cleaning up the dead `routing.{planner,implementation,validator}_backend` /
  `ResolveBackendForRole` path (§1.4) — either wire it in or remove it — should
  be scoped into that follow-up so the two role mechanisms do not diverge
  further.
