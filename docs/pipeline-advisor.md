# Optional bounded pipeline Advisor

Maestro can place an independent, review-only Advisor between planning and
implementation:

```text
Planner -> Advisor
             |- PLAN_REVISE -> Planner
             `- PLAN_APPROVED -> Implementer -> Validator
```

The gate is disabled by default. It is intended for plans where an independent
model review is worth the additional latency and token cost before source code
is written.

## Configuration

The Advisor reuses the normal pipeline role controls:

```yaml
pipeline:
  enabled: true
  planner:
    enabled: true
    backend: fable
    effort: xhigh
  advisor:
    enabled: true
    backend: codex
    effort: high
    prompt: ./prompts/advisor.md       # optional; the mandatory packet is appended
    max_runtime_minutes: 20
  advisor_review_rounds: 2            # default 2; accepted range 1..5
  advisor_best_effort: false          # explicit auditable bypass; default false
  implementer:
    backend: codex
    effort: low
  validator:
    enabled: true
    backend: fable
    effort: high
```

Project opt-in requires the phase pipeline and Advisor role to be enabled. An
individual issue can opt in with `pipeline:advised`; that label enables Planner,
Advisor, Implementer, and Validator for that session. `pipeline:full` retains
its existing meaning. A `pipeline:full` issue uses the Advisor only when the
project's `pipeline.advisor.enabled` is already true.

## Review contract

For every pass, Maestro supplies the Advisor with:

- the complete GitHub issue;
- current `MAESTRO_PLAN.md` and `VALIDATION.md` contents;
- plan version and review round; and
- the compact ledger of findings accumulated across earlier rounds.

The Advisor may create only `MAESTRO_PLAN_REVIEW.md`. Its first line must be
exactly `PLAN_APPROVED` or `PLAN_REVISE`. `PLAN_REVISE` must include specific
findings after the first line. Leading prose, a missing artifact, an unknown
marker, or an empty revision verdict is invalid and never counts as approval.

The Planner remains the sole owner of `MAESTRO_PLAN.md` and `VALIDATION.md`.
Before each Advisor pass Maestro snapshots git HEAD, both canonical artifacts,
and worktree status. A commit, canonical-artifact change, or any source/worktree
change outside `MAESTRO_PLAN_REVIEW.md` fails the gate.

## Bounds and failure semantics

The review budget defaults to two Advisor passes and cannot exceed five. A
revision consumes one pass, sends the exact accumulated findings back to the
Planner, increments the plan version after the Planner finishes, and starts the
next Advisor pass. A final `PLAN_REVISE` exhausts the gate and blocks before
implementation.

The default is fail closed. These conditions mark the session failed before an
Implementer starts and surface the exact unresolved findings in Fleet attention:

- review-round exhaustion;
- timeout, silent timeout, or token-budget exhaustion;
- unavailable, disabled, cooling-down, unauthenticated, or model-unavailable
  required Advisor backend;
- missing or malformed verdict;
- Advisor source edits, canonical-plan mutation, commit, or premature PR; and
- inability to build or start the review/revision phase.

`advisor_best_effort: true` is the separate explicit opt-out. When enabled,
Maestro records `PLAN_BYPASSED`, the original failure verdict/reason, findings,
backend/model, and a bypass flag before starting implementation. It is never
enabled implicitly. Review-only boundary violations (source/canonical changes,
commits, pushes, or premature PR creation) remain fail-closed even in
best-effort mode because bypassing them would let the Advisor become an
overlapping writer.

## Observability and restart behavior

Session state and Fleet APIs expose plan version, review round/budget,
Advisor backend/model, latest verdict, unresolved findings, terminal reason,
bypass mode, and the bounded review history. Mission Control shows the current
gate and exact findings in the worker drawer. The phase and workspace snapshot
are durable, so a daemon restart while the Advisor is running reconciles the
same review rather than starting an overlapping Planner or Advisor.

## Cost, latency, and rollout

Each review or revision adds another agent session, so the gate increases wall
clock time and token cost even when the first plan is approved. Start with one
low-stakes project or a `pipeline:advised` issue cohort. Compare against similar
non-advised work using:

- Validator retry count;
- P0/P1 review findings that reach implementation or PR review;
- worker and Advisor terminal failures;
- time to merge and total cycle time; and
- tokens and estimated cost per completed issue.

Keep the role disabled until the pilot demonstrates that earlier findings
offset the extra review latency and cost.
