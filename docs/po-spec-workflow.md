# Product Owner spec workflow

Source of truth for spec **drafts** is the Obsidian vault at `Dev/Areas/maestro/specs/`. This document describes the GitHub-side execution contract that workers and reviewers rely on.

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

## Operator commands

Inspect the queue:

```bash
ssh workshop 'cd /mnt/storage/src/maestro && gh issue list --repo BeFeast/maestro --state open --label maestro-ready --limit 50'
```

Watch the live brief:

```bash
ssh workshop 'curl -fsS http://127.0.0.1:8786/api/v1/fleet | /usr/bin/jq .verdict.sentence'
```

Pause a single spec without losing it:

```bash
ssh workshop 'gh issue edit <NUM> --repo BeFeast/maestro --add-label blocked --remove-label maestro-ready'
```
