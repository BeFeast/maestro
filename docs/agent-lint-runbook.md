# agent-lint Runbook (#707)

`agent-lint` is a reusable CI check that encodes the fleet's **agent
anti-pattern rules** so they are enforced, not just described in the worker
prompt. Rules enforced only in prompts are not enforced at all — models
occasionally violate them and today those violations are caught late, by review
or by the operator.

It ships as a **composite action** at `.github/actions/agent-lint`, backed by a
single self-contained script (`agent-lint.sh`). The action is referenced by this
repo's `agent-lint` workflow and is designed for one-file copy/reuse across the
other fleet repos.

## What it checks

| # | Check | Result | Rule |
|---|-------|--------|------|
| 1 | **Closing keywords** | ❌ fail | PR title/body must not use a GitHub auto-closing keyword (`close`/`closes`/`closed`, `fix`/`fixes`/`fixed`, `resolve`/`resolves`/`resolved`) that references an issue (`#N`, `GH-N`, or an issue URL). Use `Refs #N` or `Implements #N` instead. |
| 2 | **Diff hygiene** | ❌ fail | The diff must not add build/run artifacts: `.maestro/`, `tmp/`, `_tmp/`, `*.log`, `*.logs`, `*.test`, `*.test.json`. Exempt extra paths with `artifact-allowlist`. |
| 3 | **Secret scan** | ❌ fail | Added diff lines must not contain common secret shapes: `sk-ant-…`, `ghp_…`/`gho_…`/`ghs_…`, `github_pat_…`, `AKIA…`/`ASIA…`, `xox…` (Slack), `AIza…` (Google), or a `BEGIN … PRIVATE KEY` block. Matches are reported masked, never echoed in full. |
| 4 | **Draft check** | ⚠️ warn | A draft PR without an explicit WIP/Partial marker only **warns** (it never blocks). The recognised markers mirror the orchestrator's deliberate-draft detection (#697): `[WIP]`/`[Partial]` in the title, a `WIP:`/`Partial:`/`Draft:` prefix, or `maestro:partial`/`maestro:wip` in the body (the worker Partial flow embeds `<!-- maestro:partial -->`). |

## Escape hatch (false positives)

When a check fires incorrectly, downgrade the **hard** checks (1–3) to warnings
so the job passes — pick either:

- **Label:** add the `agent-lint:override` label to the PR, or
- **Body marker:** add `<!-- agent-lint:allow -->` to the PR description.

Use sparingly — the override is logged as a CI notice so it is visible in
review. The draft check is warn-only and needs no override.

> Operators: create the override label once per repo, e.g.
> `gh label create "agent-lint:override" --color "FBCA04" --description "Bypass agent-lint hard checks"`.

## One-file adoption for other fleet repos

The lint logic travels with the composite action at its pinned ref, so a
consuming repo only adds **one** workflow file — no scripts to copy:

```yaml
# .github/workflows/agent-lint.yml
name: agent-lint
on:
  pull_request:
    types: [opened, edited, reopened, synchronize, ready_for_review, converted_to_draft, labeled, unlabeled]
permissions:
  contents: read
  pull-requests: read
jobs:
  agent-lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: BeFeast/maestro/.github/actions/agent-lint@main
        # with:
        #   artifact-allowlist: |
        #     docs/**/*.log
        #   override-label: agent-lint:override
```

Pin `@main` to a tag or commit SHA if you want the rules to change only on
deliberate updates.

## Local dry-run

The same script runs locally — no `gh`, no network — by passing the PR fields
directly as environment variables:

```bash
AGENT_LINT_TITLE="feat: thing (#42)" \
AGENT_LINT_BODY="Fixes #42" \
AGENT_LINT_FILES=$(git diff --name-only main) \
AGENT_LINT_DIFF="$(git diff main)" \
bash .github/actions/agent-lint/agent-lint.sh
# → fails on the 'Fixes #42' closing keyword
```

To lint a real PR locally (requires an authenticated `gh`):

```bash
AGENT_LINT_PR=123 AGENT_LINT_REPO=BeFeast/maestro \
  bash .github/actions/agent-lint/agent-lint.sh
```

## Fixtures / deliberate violations

`agent-lint.sh --self-test` runs built-in fixtures that deliberately violate
each rule and asserts that every check trips (and that clean input passes). The
repo's `agent-lint` workflow runs this on every PR, so the checks cannot
silently rot. To trip each check by hand:

| Check | How to trip it |
|-------|----------------|
| Closing keyword | Put `Fixes #1` in the PR body or title. |
| Diff hygiene | Commit a `debug.log` or any `.maestro/` file. |
| Secret scan | Add a line containing a token shaped like `sk-ant-…` or `AKIA…`. |
| Draft warning | Open a draft PR with no `[Partial]`/`[WIP]` marker. |

```bash
bash .github/actions/agent-lint/agent-lint.sh --self-test
```
