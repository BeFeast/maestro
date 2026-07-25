# Product Owner spec workflow

Source of truth for spec **drafts** is the Obsidian vault at `Dev/Areas/maestro/specs/`. This document describes the GitHub-side execution contract that workers and reviewers rely on.

## Which template for what

`.github/ISSUE_TEMPLATE/` is a small registry of typed templates. Blank issues are
disabled (`config.yml`), so every report lands pre-structured — this is what lets
future automated grooming produce consistent output, and it keeps bugs/ops/docs from
arriving free-form and ranking poorly for supervisor pickup or the digest "Promotable"
list. Pick by the shape of the work:

| Template | File | Use it for | Auto label |
|----------|------|------------|------------|
| **Spec** | `spec.yml` | A feature a worker implements hands-off. The default. | `enhancement` |
| **Bug** | `bug.yml` | A defect: observed vs expected, repro/evidence, affected surface, verification. | `bug` |
| **Runbook / Docs change** | `runbook-docs.yml` | A docs/runbook fix: which doc, what's wrong/missing, done-criteria. | `documentation` |
| **Infra / Ops change** | `infra-ops.yml` | An ops/config change: target unit/row/host, rollback plan, verification commands. | *(none)* |
| **Handoff Epic** | `epic.yml` | A multi-slice epic the supervisor planner dispatches slice-by-slice (see [`handoff-epic-format.md`](./handoff-epic-format.md)). | `epic` |

Label notes that matter for the [pickup contract](#pickup-contract-do-not-break-by-accident):

- None of these templates apply `maestro-ready` — a reviewer applies that once the
  issue is complete (or moves it into the Project, and the supervisor labels it).
- `bug.yml` / `runbook-docs.yml` / `infra-ops.yml` apply no pickup-excluded label, so
  they enter the runnable queue normally once marked ready.
- `epic.yml` applies `epic` on purpose — that label is pickup-excluded, so a worker
  never picks up the epic itself; the handoff planner reads it and schedules the child
  slices.

## Loop

1. **Draft in Obsidian** — create a note in `Dev/Areas/maestro/specs/<kebab-case>.md` with frontmatter
   `type: spec`, `status: draft`, and the same sections this template will eventually expect
   (Summary / Why / Scope / Acceptance / Test Plan / Verification / Non-goals). Iterate until the spec
   is testable without re-asking the PO.
2. **Promote to GitHub** — open a new issue in `BeFeast/maestro` using the **Spec** template
   (`.github/ISSUE_TEMPLATE/spec.yml`). Paste/adapt the body from the Obsidian draft. Set the priority
   dropdown. Update the Obsidian note: `status: ready`, `gh_issue: <number>`.
3. **Self-review on the issue** — walk the PO checklist at the bottom of the form. If any box does not
   check, leave the issue unlabeled until it does.
4. **Mark ready** — apply the `maestro-ready` label, or move the issue into the Maestro GitHub Project
   (#5) in column `Todo` / `Ready` / `Backlog` / `New`. The supervisor will add the label automatically
   (`owns_ready_label: true`). Update the Obsidian note: `status: in-flight`.
5. **Walk away** — `maestro-supervisor-dogfood.service` polls every 2 minutes, picks the issue,
   spawns a worker (Claude opus, xhigh effort) in `/mnt/storage/worktrees/maestro/<session>`,
   and produces a PR. Update the Obsidian note: `gh_pr: <number>`.
6. **Review** — check the PR on GitHub. Wait for CI and Greptile.
   Address any P1/P2 Greptile findings before merge (per the hard rules in the handover).
7. **Merge** — operator-only, the supervisor cannot merge by itself
   (`approval_required: [merge_pr, ...]`). Update the Obsidian note: `status: merged`.
8. **Deploy** — only if the change touches code paths that ship with `/usr/local/bin/maestro`.
   See `maestro-handover-2026-05-03.md` for the deploy snippet.


## Pickup contract (do not break by accident)

The supervisor selects issues that match **all** of:

- have label `maestro-ready`
- do **not** have any of `blocked`, `wontfix`, `question`, `duplicate`, `invalid`, `epic`, `meta`
- are **open**

Source of truth: `~/.maestro/maestro.d/maestro-supervisor-dogfood.yaml`.

For hard or architectural dogfood issues, add `pipeline:full` to that GitHub
issue. The dogfood config should keep `pipeline.enabled` unset or `false` so
routine `maestro-ready` issues continue to use the cheaper single implementer
session by default.

Use `pipeline:advised` when the issue also needs the bounded independent plan
review described in [`pipeline-advisor.md`](./pipeline-advisor.md). The Advisor
adds latency and token cost, fails closed by default, and should be piloted on a
small cohort before broader rollout.

If a spec needs to wait, add `blocked` and explain in a comment. Do not delete the issue — supervisor needs the
audit trail.

## What makes a good spec

- **Acceptance criteria are testable without re-asking the PO.** If a worker has to guess, the spec is not done.
- **Verification runs against live workshop services**, not just unit tests. Maestro is judged by what the
  Fleet dashboard says after deploy, not by green CI.
- **Scope and non-goals are explicit.** The worker prompt forbids "broad refactors"; specs that imply them get
  partial implementations.
- **No multi-repo coordination in one spec.** Split into per-repo specs.

## What makes a bad spec

- "Improve the dashboard" — not testable.
- "Refactor X" with no observable behavior change — workers cannot prove done.
- Specs that require running things off-workshop (laptop, NFS) — violates the hard operating rules.
- Specs that re-introduce dense, nested, inspector-style UI — see the UI direction in the handover.

## Spec-lint + grooming agent (#851)

The supervisor can enforce the "good spec" rules above automatically. It is
**off by default**; enabling it is a config-store row change, no code or restart:

```bash
maestro settings set supervisor.spec_groom.enabled=true            # fleet default
maestro settings set supervisor.spec_groom.enabled=true --project befeast-maestro
```

Once enabled, per supervisor cycle (polling only — no webhook):

- **Spec-lint** — each open, not-yet-`maestro-ready` issue is scored against the
  rules above in a single cheap LLM pass (same backend selection as the
  supervisor, respecting `supervisor.allow_metered_backend`). A failing issue
  gets **exactly one** checklist comment naming what is missing; a well-formed
  issue gets none. Lint runs at most once per issue-body change (a body hash is
  tracked in state), so there is zero comment churn on unchanged issues.
- **Grooming** — comment `@maestro groom` on any issue to get a proposed rewrite
  in the Spec template structure (Summary / Why / Scope / Acceptance / Test Plan
  / Non-goals; nothing dropped, gaps marked `TBD`) posted as a comment. The
  proposal changes nothing on its own: applying it is the approval-gated
  `edit_issue_body` verb — **approve** to replace the issue body, **reject** to
  leave it untouched. Both outcomes are visible in the approvals UI and audit.

Optional strict gate:

```bash
maestro settings set supervisor.spec_groom.require_lint_pass=true --project befeast-maestro
```

With `require_lint_pass` set, the supervisor withholds the `maestro-ready` label
from an issue until its current body has **passed** spec-lint (default is
warn-only: a failing issue still gets its lint comment but keeps its normal
labeling flow).

## Operator commands

Inspect the queue:

```bash
ssh workshop 'cd /mnt/storage/src/maestro && gh api "repos/BeFeast/maestro/issues?state=open&labels=maestro-ready&per_page=50" --jq ".[].number"'
```

Watch the live brief:

```bash
ssh workshop 'curl -fsS http://127.0.0.1:8786/api/v1/fleet | /usr/bin/jq .verdict.sentence'
```

Pause a single spec without losing it:

```bash
ssh workshop 'gh issue edit <NUM> --repo BeFeast/maestro --add-label blocked --remove-label maestro-ready'
```
