# Project Setup Runbook — Maestro Auto-Merge

How to set up a new repository so maestro can pick issues, spawn workers, merge PRs, and deploy automatically.

---

## 1. CI Requirements (GitHub Actions)

Maestro merges PRs only after all required status checks pass. Your repo needs at minimum:

### Required checks

Create `.github/workflows/ci.yml` with jobs that cover:

- **build** — compile the project (e.g. `go build ./cmd/app/`, `cargo build`, `bun run build`)
- **lint** — static analysis (e.g. `go vet ./...`, `clippy`, `eslint`)
- **test** — unit and E2E tests (e.g. `go test ./...`, `cargo test`, `playwright test`)

All three must be **required status checks** on `main` (configured in branch protection — see section 2).

Example for a Go project:

```yaml
name: CI
on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - run: go build ./cmd/app/
      - run: go vet ./...
      - run: go test ./...
```

### Version bump workflow

Auto-increment the patch version on every merge to `main`. This prevents stale-version confusion when multiple PRs merge in quick succession.

```yaml
name: Version Bump
on:
  push:
    branches: [main]

jobs:
  bump:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      # Increment patch version in your version file(s),
      # commit, and optionally tag.
```

Alternatively, maestro's built-in `version-bump` command can handle this via the `versioning` config block (see section 4).

### Deploy workflow

Either:
- A **self-hosted runner** that runs after merge, or
- A **delivery hook** in maestro config (`delivery:`, approval-gated by default) that maestro runs after a successful merge.

The deploy hook approach is simpler — see section 5.

---

## 2. Branch Protection Rules

Go to **Settings → Branches → Add branch protection rule** for `main`:

| Setting | Value |
|---|---|
| Require a pull request before merging | Yes |
| Require status checks to pass before merging | Yes |
| Status checks that are required | `build` (and any other CI job names) |
| Require branches to be up to date before merging | Recommended |
| Do not allow bypassing the above settings | Yes |
| Allow force pushes | No |
| Allow deletions | No |

Maestro creates PRs and waits for checks — it never pushes directly to `main`.

---

## 3. Labels Setup

Maestro filters issues by label. Create these labels in your repo (**Settings → Labels**):

### Required labels (used in `issue_labels`)

| Label | Description |
|---|---|
| `bug` | Something isn't working |
| `enhancement` | New feature or improvement |
| `documentation` | Docs-only change |

### Exclude labels (used in `exclude_labels`)

| Label | Description |
|---|---|
| `wontfix` | Will not be addressed |
| `question` | Needs discussion, not code |
| `blocked` | Waiting on external dependency |
| `duplicate` | Duplicate of another issue |
| `invalid` | Not a valid issue |

Issues with any exclude label are skipped even if they also have a required label.

---

## 4. Portable Maestro Project Config

Create a private portable YAML outside the code repo, then register it with the
`project plan` / `project apply` flow below. The daemon reads the resulting store
row; it does not watch this YAML file. Example:

```yaml
# Repository
repo: YOUR_ORG/YOUR_REPO
local_path: /path/to/local/clone
worktree_base: /path/to/worktrees/repo
project_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301
management_home:
  kind: obsidian
  path: /absolute/path/to/Obsidian Vault/Dev/Areas/your-project
  vault: Obsidian Vault
  vault_path: Dev/Areas/your-project

# Issue filtering
issue_labels:
  - bug
  - enhancement
  - documentation
exclude_labels:
  - wontfix
  - question
  - blocked
  - duplicate
  - invalid
blocker_patterns:
  - "blocked by.*?#(\\d+)"
  - "blocked until.*?#(\\d+).*merged"
  - "depends on.*?#(\\d+)"

# Supervisor policy (optional)
supervisor:
  enabled: true
  mode: cautious
  ready_label: maestro-ready
  blocked_label: blocked
  excluded_labels:
    - epic
    - meta
  ordered_queue:
    enabled: true
    issues:
      - 308
      - 306
  dynamic_wave:
    enabled: true
    owns_ready_label: true
    runnable_project_statuses:
      - Todo
      - To Do
      - Ready
      - Backlog
      - New
    dependency_unblock:
      # Defaults on when dynamic_wave owns ready/blocked labels; set false
      # only when blocker chains are intentionally operator-managed.
      enabled: true
      max_runnable: 1
      enroll_in_project: true
      announce_with_comment: true
  safe_actions:
    - add_ready_label
    - remove_ready_label
    - remove_blocked_label
    - add_issue_comment
  approval_required:
    - merge_pr
    - close_issue
    - delete_worktree
    - change_global_config

# Select a fleet-shared backend. Portable genesis files do not define
# model.backends; configure those once at fleet scope.
model:
  default: claude

# Concurrency
max_parallel: 5
max_runtime_minutes: 120

# Worker session naming (workers: proj-1, proj-2, ...)
session_prefix: proj

# Worker prompt template
worker_prompt: /path/to/worker-prompt-template.md

# Optional in-session worker tool hooks
hooks:
  timeout_ms: 60000
  post_edit:
    command: gofmt -w .
    matcher: Edit|MultiEdit|Write
    block_on_failure: true
  pre_tool:
    command: ./scripts/check-safe-tool.sh
    matcher: Bash
    block_on_failure: true

# Outcome brief (read-only supervisor context)
outcome:
  desired_outcome: Users can run the product end-to-end.
  runtime_target: https://app.example.com
  deployment_status_command: /path/to/repo/scripts/status.sh
  healthcheck_url: https://app.example.com/healthz
  source_repo_path: /path/to/local/clone
  runtime_host: production host or platform
  non_goals:
    - Rewrite unrelated subsystems

# Post-merge delivery (#872). Default-safe: a merged revision creates an
# auditable deploy_project approval pinned to the exact merge commit; only an
# operator approve runs the command + verifier (exactly once, behind a durable
# claim). Set mode: automatic to opt back into unattended deploy-on-merge.
delivery:
  mode: approval_required        # disabled | approval_required | automatic
  command: "/path/to/repo/scripts/deploy.sh"
  timeout_minutes: 15
  target: "prod web"             # operator-safe destination label (never a secret)
  rollback: "scripts/rollback.sh"
  verify_command: "/path/to/repo/scripts/status.sh"

# Legacy: deploy_cmd still works but is deprecated — it maps to
# delivery.mode: automatic (unattended deploy after every merge) and emits a
# load-time deprecation warning. Prefer the delivery: block above.
# deploy_cmd: "/path/to/repo/scripts/deploy.sh"

# Telegram notifications (optional, via OpenClaw gateway)
telegram:
  target: "YOUR_TELEGRAM_CHAT_ID"
  bot_token: "YOUR_BOT_TOKEN"
  openclaw_url: "http://localhost:18789"
```

### Key fields explained

| Field | Purpose |
|---|---|
| `repo` | GitHub repo in `owner/repo` format |
| `local_path` | Local clone used for `git fetch` and as the base for worktrees |
| `worktree_base` | Directory where maestro creates per-worker worktrees |
| `issue_labels` | Only pick issues with at least one of these labels (OR semantics) |
| `exclude_labels` | Skip issues with any of these labels |
| `outcome` | Project operating brief used by the supervisor to judge runtime progress |
| `supervisor` | Optional local policy for supervisor queue order, safe actions, dispatch SLA, and issue-type skips |
| `model.backends.<name>.mcp` | Optional worker MCP attachment for that backend; omitted means no MCP tools |
| `model.backends.<name>.subagent_hint` | Optional sub-agent model policy injected into the worker prompt for that backend; omitted means the prompt is unchanged |
| `max_parallel` | Maximum concurrent worker sessions |
| `delivery` | Post-merge delivery block (#872): `mode` (disabled/approval_required/automatic), `command`, `timeout_minutes`, `target`, `rollback`, `verify_command`. Default-safe: a merge mints an approval and runs nothing until approved |
| `deploy_cmd` | Deprecated (#872): legacy shell command run automatically after merge; folds into `delivery.mode: automatic` with a deprecation warning |
| `session_prefix` | Prefix for tmux session names |
| `worker_prompt` | Path to the worker prompt template file |
| `hooks.post_edit` | Optional command run inside worker sessions after matching file edit tools |
| `hooks.pre_tool` | Optional command run inside worker sessions before matching tool calls |

Supervisor policy can also live in `.maestro/supervisor.yaml` next to the project config or repository checkout. If an ordered queue is configured, only the first unfinished issue in that queue is eligible for supervisor dispatch until the queue is exhausted. `dynamic_wave` is explicit opt-in and lets the supervisor select the next runnable open issue without listing issue numbers, using priority labels and conservative skip rules. Set `supervisor.dispatch_sla_seconds` to control when Fleet escalates a selected issue that has not started a worker.

For Maestro dogfooding, add the `outcome` block to the `BeFeast/maestro` project config first. Point `runtime_target` and `healthcheck_url` at the local Mission Control dashboard, and keep deploy/runtime actions read-only until approval-backed controls exist.

Worker tool hooks are default-off and opt in per project. `command` runs from the worker worktree and receives the backend hook JSON in `MAESTRO_HOOK_INPUT`, plus `MAESTRO_HOOK_EVENT`, `MAESTRO_HOOK_TOOL_NAME`, and `MAESTRO_HOOK_FILE_PATH` when available. For Claude workers, Maestro writes a local `.claude/settings.local.json` hook file in the worktree and excludes it from git; stdout/stderr is returned to the agent as hook context. Other backends receive the same hook contract in the worker prompt so the configured command remains visible, but automatic per-tool interception depends on backend support. Set `block_on_failure: true` when a non-zero hook result should stop the matching tool/event until the worker corrects it.

### Optional: versioning config

```yaml
versioning:
  enabled: true
  files:
    - "VERSION"
  default_bump: patch
  tag_prefix: v
  create_release: true
```

For a batched release cadence, run `maestro version-bump --since-last-tag` from
a scheduled GitHub Actions workflow with `workflow_dispatch`, full history
checkout (`fetch-depth: 0`), and idempotent `version:major`,
`version:minor`, and `version:patch` label creation. The workflow should use
one repo-local config like `.github/maestro-release.yaml` with `files:
[VERSION]`, `default_bump: patch`, and `tag_prefix: v`.

### Optional: model routing

```yaml
model:
  default: claude
  backends:
    claude:
      cmd: claude
    codex:
      cmd: codex
```

### Optional: sub-agent model policy (`subagent_hint`)

When a worker backend is an orchestrating CLI such as Claude Code, it spawns its own sub-agents and, by default, runs them on the same expensive model as the orchestrator. Bulk grunt subtasks (file sweeps, searches, mechanical edits) then burn the subscription window at orchestrator-model prices, multiplied across parallel sub-agents. Set `model.backends.<name>.subagent_hint` to steer those sub-agents to cheaper models; the worker prompt gains a "Sub-agent Model Policy" section carrying the text verbatim. The field is optional and field-driven — backends without it render an unchanged prompt, so non-orchestrating backends (codex, gemini, …) can leave it unset. A recommended default ships under the claude backend in `maestro.yaml.example`:

```yaml
model:
  default: claude
  backends:
    claude:
      cmd: claude
      subagent_hint: "Use cheaper sub-agent models (e.g. opus/sonnet) for delegated grunt subtasks such as file sweeps, searches, and mechanical edits; reserve the main orchestrator model for planning and final review."
```

### Pricing classes and the metered-backend guard (`pricing_class`, #838)

Maestro's always-on internal loops — the supervisor cycle and the auto-router —
call an LLM on a schedule regardless of whether there is work to do. Pointing one
of those loops at a **per-token metered** model burns money making "do nothing"
decisions: on 2026-07-09 the shared `codex` backend row was re-pointed at a
per-token model and supervise cycles burned ~$4/hour across nine idle projects
before anyone noticed. The `pricing_class` guard caps that blast radius.

Declare how each backend bills with `pricing_class`:

```yaml
model:
  default: claude
  backends:
    claude:
      cmd: claude
      pricing_class: subscription   # fixed monthly plan (Claude Max, etc.)
    fireworks-kimi:
      cmd: fw
      provider: fireworks
      pricing_class: metered        # per-token — gated from always-on loops
```

- `flat` / `subscription` — a fixed-cost plan. Always-on loops run normally.
- `metered` — billed per token. The supervisor and router **refuse** to run
  their LLM path on this backend unless the project opts in explicitly.
- unset — treated as `flat` for backward compatibility. A backend that only sets
  a `pricing:` table for cost observability (#619) is **not** gated; mark it
  `metered` explicitly to activate the guard.

When `supervisor.backend` (or the `model.default` fallback the supervisor uses)
resolves to a `metered` backend and the project has not opted in, the supervisor
drops to **deterministic-only** for that cycle — no supervisor backend call is
made — and surfaces a red *"Metered backend"* attention badge on Mission Control
until the operator re-points the backend or opts in. A config-store edit that
re-points the backend at a per-token model therefore cannot silently start
burning: the next supervise cycle runs deterministic-only and logs the refusal.
The same guard applies to `routing.router_model` when `routing.mode: auto` — the
router skips its per-issue LLM classification and falls back to `model.default`.

Opt in per project when the per-token cost is acceptable:

```yaml
supervisor:
  allow_metered_backend: true   # supervisor LLM may run on a metered backend
routing:
  allow_metered_backend: true   # auto-router LLM may run on a metered backend
```

The supervisor opt-in and the current refusal are visible on the fleet API under
`effective_config.supervisor_gate` (`allow_metered_backend`,
`metered_backend_refused`, `metered_backend`), and each backend's classification
appears under `effective_config.model_policy.backends[].metered`.

**Workers are not gated.** A worker is bounded by `worker_max_tokens` and produces
a concrete PR, so a metered worker backend is a deliberate, bounded spend — this
guard targets only the unbounded always-on loops.

### Optional: parallel review streams

By default, `review_gate: greptile` waits for the single Greptile verdict before merge. Projects that also run the simplicity / over-engineering reviewer can opt in to the aggregate gate:

```yaml
review_gate: greptile
review_gate_streams:
  - greptile
  - simplicity
```

`review_gate: none` disables all review streams. A blocking simplicity finding is treated like a blocking Greptile finding for merge gating and review-repair.

### Optional: visual evidence for UI-affecting PRs (`verify.visual`)

Green tests are not proof a UI renders correctly. Projects with a screenshot
harness can opt in to the visual-evidence step (#705):

```yaml
verify:
  visual:
    enabled: true
    command: ./scripts/capture-screenshots.sh   # launches the app, writes screenshots
    paths:                                      # globs that classify a PR as UI-affecting
      - "web/**"
      - "**/*.jsx"
    # output_dir: .maestro/screenshots          # default; command also sees $MAESTRO_SCREENSHOT_DIR
    # timeout_minutes: 10                       # capture command budget
```

How it behaves:

- Workers get a **Visual Evidence** prompt section: when their diff touches a
  configured glob, they run the capture command and attach the screenshots to
  the PR (a comment with embedded images) before declaring done.
- The orchestrator then checks each UI-affecting PR once. If no image is
  attached it re-runs the capture command in the session worktree as a
  diagnostic, posts a single advisory warning comment on the PR, and the
  supervisor records a `visual_evidence_missing` finding (severity: warning).
- Non-UI PRs are unaffected. The step never blocks or delays merge in v1 —
  it makes the missing evidence loud instead of leaving it to the operator's
  eyes after merge.

The capture command runs from the worktree root with
`MAESTRO_SCREENSHOT_DIR` set to the absolute output directory. Maestro counts
`*.png`, `*.jpg`, `*.jpeg`, `*.gif`, and `*.webp` files (recursive) as
screenshots. `verify.visual.enabled: true` without `command`/`paths` logs a
config warning and stays inert.

### Adding a project (single-daemon genesis)

Maestro runs as **one long-lived daemon** — `maestro daemon --watch-store` —
reading a shared SQLite config store. Each project is a single row in that store;
there is no per-project service. The old `maestro init` wizard (per-project
`maestro.yaml` + a `maestro@<project>` unit) is retired: running it now only prints
a redirect to the flow below.

Register a project with the zero-write `plan` / idempotent `apply` genesis flow.
Write a portable project config with canonical `owner/repository`, absolute
execution-host `local_path` / `worktree_base`, a stable lowercase `project_id`,
and a complete `management_home` block. Generate the UUID once — it is the
durable identity `apply` confirms. Then:

Point `--db` at the same store the daemon unit reads (its `--store` path; the
shipped `maestro.service` uses `%h/.maestro/maestro.db`).

```bash
# 1. Preview the effect on the store — strictly validated, changes no files/rows:
maestro project plan  --file ~/myproject.project.yaml --db ~/.maestro/maestro.db --json

# 2. Apply the exact `next[0]` command returned by the approved plan receipt:
maestro project apply --file ~/myproject.project.yaml --db ~/.maestro/maestro.db \
    --confirm <project-id> --fingerprint <sha256-from-plan> \
    --baseline <baseline-from-plan> --json
```

`plan` is read-only and reports the predicted effect (`create` / `update` /
`no-op` / `conflict`) plus a config fingerprint. `apply` refuses a missing/wrong
`--confirm`, a mismatched desired `--fingerprint`, or a mismatched plan-time store
`--baseline`; config-file and concurrent store changes are refused. A second
identical apply is a reported no-op; an identity conflict is a hard
stop that never overwrites a row by name. Both emit a machine-readable receipt
(store, project id, fingerprint, effect, daemon-reconciliation expectation, exact
next commands) for scripted bootstrap adapters.

The running `maestro daemon --watch-store` observes the new row within one poll
interval and starts exactly one flow — no restart, no `systemctl` step per project.
Removal/rollback stays a separate explicit operator action:

```bash
maestro config-store rm --db ~/.maestro/maestro.db <name>   # drains the flow; the daemon reconciles the removal
```

The daemon itself runs from the single unit described below; if it is not
running with `--watch-store`, a newly applied row is picked up only on its next
restart, and `project apply` says so in its receipt.

### Single-daemon Mission Control and config-store operating model

`~/.maestro/maestro.db` is the authoritative fleet config store. Do not create a
new `maestro.d`, `fleet.yaml`, per-project unit, or separate `maestro serve`
process for a newly registered project. The one `maestro daemon --watch-store`
process runs every project flow and serves the fleet Mission Control/API on
`127.0.0.1:8786` by default.

For day-to-day operations, review gates, queue policy, approvals, dashboard
exposure, and safe recovery steps, see the
[Fleet Mission Control operating runbook](fleet-mission-control-runbook.md).

Start the daemon manually for an interactive test:

```bash
mkdir -p ~/.maestro
maestro daemon --watch-store --store ~/.maestro/maestro.db \
  --approvals-store sqlite --state-store sqlite
```

For a persistent Linux user service, install the repository's single
`maestro.service` and enable that one unit:

```bash
mkdir -p ~/.maestro
mkdir -p ~/.config/systemd/user
cp maestro.service ~/.config/systemd/user/maestro.service
systemctl --user daemon-reload
systemctl --user enable --now maestro.service
systemctl --user status maestro.service
```

The service and every `project plan` / `project apply` command must point to the
same `maestro.db`. Adding or changing a row is hot-reconciled; it does not require
a daemon restart or another service. Verify the fleet API after apply:

```bash
curl -fsS http://127.0.0.1:8786/api/v1/fleet
```

The schema below is implementation context, not a second setup path.

Core project tables:

```sql
CREATE TABLE global (
  key TEXT PRIMARY KEY,
  value_yaml TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE backends (
  name TEXT PRIMARY KEY,
  definition_yaml TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE project (
  name TEXT PRIMARY KEY,
  source_path TEXT NOT NULL,
  config_yaml TEXT NOT NULL,
  backend_ref TEXT NOT NULL DEFAULT 'global',
  updated_at TEXT NOT NULL
);
```

`project.config_yaml` stores the project config without `model.backends`; `backends.definition_yaml` stores each backend definition once and the loader reconstructs the effective config before applying the existing YAML parser defaults and validation. Import rejects divergent definitions for the same backend name so the backend chain cannot silently fork per project.

Existing installations that still have legacy `maestro.d` files or
`maestro@<project>` units should use the explicit, operator-confirmed cutover
script from a repository checkout. This is migration guidance, not the topology
for a new project:

```bash
scripts/migrate-to-daemon.sh --dry-run
scripts/migrate-to-daemon.sh
```

Compatibility guard: when legacy `~/.maestro/config.db` still contains project
rows and `~/.maestro/maestro.db` contains none, default CLI commands remain on
the legacy store and warn. The shipped service refuses to start an empty
canonical fleet until the projects are explicitly exported and migrated; follow
the exact commands in that error or use the cutover script above.

An explicit portable backup can be exported without changing the running
topology:

```sh
maestro config-store export --db ~/.maestro/maestro.db --dir /path/to/maestro-config-backup
```

Exports restore shared backend definitions into each YAML file. Do not point a
second daemon or `maestro serve` process at the export.

To add a project:

1. Create the portable YAML described in the genesis section above.
2. Review `maestro project plan --json` and run its exact `next[0]` command.
3. Re-run plan and require `effect: "no-op"`.
4. Verify exactly one project flow in `http://127.0.0.1:8786/api/v1/fleet`.

The daemon's fleet response includes `refreshed_at` plus per-project freshness
metadata. Project cards show snapshot age and isolate a project's load error
without hiding the rest of the fleet.

Do not create a dedicated fleet dashboard unit. Mission Control is part of the
same daemon process that runs the project flows.

---

## 5. Deploy Script (`scripts/deploy.sh`)

The deploy script is called by maestro's `deploy_cmd` after each successful merge. It must be:

1. **Idempotent** — safe to run multiple times without side effects
2. **Order-aware** — if the frontend is embedded in the binary, build frontend first
3. **Self-verifying** — confirm the service is running after deploy

### Template

```bash
#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
SERVICE_NAME="myapp"

cd "$PROJECT_DIR"
git pull --ff-only origin main

# 1. Build frontend FIRST if embedded in binary
if [ -d "web" ] || [ -d "frontend" ]; then
    echo "Building frontend..."
    cd web  # or frontend/
    bun install && bun run build
    cd "$PROJECT_DIR"
fi

# 2. Build backend
echo "Building backend..."
go build -o "$SERVICE_NAME" ./cmd/app/
# or: cargo build --release

# 3. Restart service
echo "Restarting $SERVICE_NAME..."
systemctl --user restart "$SERVICE_NAME"

# 4. Verify deploy
sleep 2
if systemctl --user is-active --quiet "$SERVICE_NAME"; then
    echo "Deploy successful — $SERVICE_NAME is running"
else
    echo "ERROR: $SERVICE_NAME failed to start" >&2
    systemctl --user status "$SERVICE_NAME" >&2
    exit 1
fi
```

Make it executable:

```bash
chmod +x scripts/deploy.sh
```

### For LXC/remote deploys

If deploying to a container or remote host, the pattern is the same but wrapped in `ssh` or `pct exec`:

```bash
#!/usr/bin/env bash
set -euo pipefail

CONTAINER_ID=100

pct exec "$CONTAINER_ID" -- bash -c '
    cd /opt/myapp
    git pull --ff-only origin main
    # build steps...
    systemctl restart myapp
    sleep 2
    systemctl is-active --quiet myapp || exit 1
'
```

---

## 6. Worker Prompt Requirements

The worker prompt template (`worker-prompt-template.md`) tells each AI agent how to work on issues. Key rules to encode:

### Test requirements

- Every feature or bug-fix PR **must include E2E tests**
- Tests must cover **actual behavior**, not just "page loads"
- Settings pages must include a **save-then-reload roundtrip test** (save settings, reload page, verify settings persisted)

### Example E2E test guidance (for the worker prompt)

```markdown
## Testing rules
- Every PR must include at least one E2E test for the changed behavior
- Test the BEHAVIOR, not just the presence of elements
  - BAD: `expect(page.locator('.settings-form')).toBeVisible()`
  - GOOD: `await page.fill('#name', 'test'); await page.click('#save');
           await page.reload();
           expect(await page.inputValue('#name')).toBe('test');`
- Settings/config pages: always test save → reload → verify roundtrip
```

### Standard worker prompt sections

1. **Step 0: Smoke test** — verify the project compiles before making changes
2. **Git hygiene** — rebase on `origin/main`, never push to `main`
3. **Pre-PR checks** — build, lint, test must all pass
4. **PR format** — title, body, linked issue number

See `worker-prompt-template.md` and `worker-prompt-go.md` in the maestro repo for working examples. The template uses variables (`{{ISSUE_NUMBER}}`, `{{BRANCH}}`, `{{WORKTREE}}`, etc.) that maestro injects automatically.

---

## 7. Smoke Test

A post-deploy smoke test verifies key pages and endpoints are reachable after deployment.

### What to check

- Main page returns HTTP 200
- API health endpoint responds
- Key functional pages load without errors

### Example (add to end of deploy script)

```bash
# Post-deploy smoke test
echo "Running smoke test..."
SMOKE_OK=true

for URL in \
    "http://localhost:8080/" \
    "http://localhost:8080/api/health" \
    "http://localhost:8080/settings"
do
    STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$URL" || echo "000")
    if [ "$STATUS" != "200" ]; then
        echo "SMOKE FAIL: $URL returned $STATUS" >&2
        SMOKE_OK=false
    fi
done

if [ "$SMOKE_OK" = false ]; then
    echo "ERROR: Smoke test failed — alerting" >&2
    # Trigger alert (maestro will send Telegram notification on deploy_cmd failure)
    exit 1
fi

echo "Smoke test passed"
```

**Failure must be loud.** If the smoke test fails, the deploy script should exit non-zero so maestro reports the failure via Telegram notifications. Never fail silently.

---

## 8. Lessons Learned from Panoptikon

Real-world lessons from running maestro on the [panoptikon](https://github.com/BeFeast/panoptikon) project:

### Frontend embedded in binary

If your frontend is bundled into the server binary (e.g. Go's `embed` or Rust's `include_dir!`):

> **`bun build` (or equivalent) MUST run BEFORE `cargo build` / `go build`.**

The backend build embeds the frontend dist files at compile time. If you build backend first, you get stale or missing frontend assets. The deploy script must enforce this order.

### Auto-version bump prevents confusion

Without auto-versioning, multiple PRs merging in sequence all report the same version. This makes debugging deployments difficult. Enable versioning (via CI workflow or maestro's `versioning` config) to auto-increment the patch version on every merge to `main`.

### Delivery hook eliminates manual deploys — with an approval gate by default

Configuring `delivery` in maestro config removes the "forgot to deploy" failure mode and keeps the running service in sync with `main`. The delivery command runs in the context of the local machine, so it can `ssh` to servers, `pct exec` into containers, or build locally.

Delivery is **approval-gated by default** (`delivery.mode: approval_required`): a merged revision creates an auditable `deploy_project` approval pinned to the exact merge commit, carrying the target label, rollback reference, sanitized command preview, and verification plan. No command runs until an operator approves it; approval executes the command once (behind a durable `approved → executing` claim so a daemon restart never replays an in-flight delivery), then runs the configured `verify_command`. A superseding merge supersedes the stale pending approval so an old revision can never be approved into a deploy. Set `delivery.mode: automatic` (or keep a legacy `deploy_cmd`) to opt back into unattended deploy-on-merge.

---

## Checklist

Use this checklist when onboarding a new repo to maestro:

- [ ] CI workflow with build + lint + test jobs
- [ ] Branch protection on `main` requiring PR + status checks
- [ ] Labels created: `bug`, `enhancement`, `documentation`
- [ ] Exclude labels created: `wontfix`, `question`, `blocked`, `duplicate`, `invalid`
- [ ] Portable project YAML includes `repo`, absolute execution-host paths, a stable `project_id`, and `management_home`
- [ ] `scripts/deploy.sh` written, tested manually, made executable
- [ ] Worker prompt template written with test requirements
- [ ] Post-deploy smoke test in deploy script
- [ ] Version bump configured (CI workflow or maestro `versioning` block)
- [ ] `maestro project plan` is reviewed, its exact `next[0]` apply command succeeds, and a second plan reports `no-op`
- [ ] The single `maestro daemon --watch-store` shows exactly one hot-loaded flow in `/api/v1/fleet`
- [ ] Telegram notifications working (if configured)
