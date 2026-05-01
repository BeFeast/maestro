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
- A **deploy hook** in maestro config (`deploy_cmd`) that maestro calls after a successful merge.

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

## 4. Maestro Config (`maestro-<project>.yaml`)

Create the config file at `~/.maestro/maestro-<project>.yaml`. Example:

```yaml
# Repository
repo: YOUR_ORG/YOUR_REPO
local_path: /path/to/local/clone
worktree_base: /path/to/worktrees/repo

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

# Concurrency
max_parallel: 5
max_runtime_minutes: 120

# Worker session naming (workers: proj-1, proj-2, ...)
session_prefix: proj

# Worker prompt template
worker_prompt: /path/to/worker-prompt-template.md

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

# Post-merge deploy hook (runs after each successful merge)
deploy_cmd: "/path/to/repo/scripts/deploy.sh"

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
| `supervisor` | Optional local policy for supervisor queue order, safe actions, and issue-type skips |
| `max_parallel` | Maximum concurrent worker sessions |
| `deploy_cmd` | Shell command maestro runs after merging a PR |
| `session_prefix` | Prefix for tmux session names |
| `worker_prompt` | Path to the worker prompt template file |

Supervisor policy can also live in `.maestro/supervisor.yaml` next to the project config or repository checkout. If an ordered queue is configured, only the first unfinished issue in that queue is eligible for supervisor dispatch until the queue is exhausted. `dynamic_wave` is explicit opt-in and lets the supervisor select the next runnable open issue without listing issue numbers, using priority labels and conservative skip rules.

For Maestro dogfooding, add the `outcome` block to the `BeFeast/maestro` project config first. Point `runtime_target` and `healthcheck_url` at the local Mission Control dashboard, and keep deploy/runtime actions read-only until approval-backed controls exist.

### Optional: versioning config

```yaml
versioning:
  enabled: true
  files:
    - "version.go"
    - "package.json"
  default_bump: patch
  tag_prefix: v
  create_release: true
```

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

### Running as a systemd service

```bash
# Single project
maestro init  # creates ~/.config/systemd/user/maestro.service

# Multiple projects — use the template unit
cp maestro@.service ~/.config/systemd/user/
systemctl --user enable --now maestro@myproject
# This reads ~/.maestro/maestro-myproject.yaml
```

### Fleet dashboard operating model

Use the fleet dashboard when one operator needs a read-only overview across multiple Maestro-managed repos without SSH spelunking. Each repo still has its own project config, state directory, `session_prefix`, and optional per-project dashboard; the fleet dashboard loads those configs, keeps them visible in project tabs, and aggregates active workers through `/api/v1/fleet`.

For day-to-day operations, review gates, queue policy, approvals, and safe recovery steps, see the [Fleet Mission Control operating runbook](fleet-mission-control-runbook.md).

Start read-only first, controls later: keep the fleet dashboard in `--read-only` mode while it is used for observation and dogfooding. Add mutating controls only after the auth, audit, and per-project safety model is explicit.

To add a project:

1. Create or update `~/.maestro/maestro-<project>.yaml` using the config shape above.
2. Use a distinct `state_dir` and `session_prefix` for each project so worker sessions and state files do not overlap.
3. If you run a per-project Mission Control dashboard, set that project's `server.host`, `server.port`, and `server.read_only: true` in the project config.
4. Add the project to `~/.maestro/fleet.yaml` with a human name, config path, and optional `dashboard_url`.
5. Restart the fleet dashboard service and verify `/api/v1/fleet`.

Minimal two-project fleet file:

```yaml
# ~/.maestro/fleet.yaml
projects:
  - name: api
    config: maestro-api.yaml
    dashboard_url: http://127.0.0.1:8788
  - name: web
    config: maestro-web.yaml
    dashboard_url: http://127.0.0.1:8789
```

`config` may be absolute, `~/...`, or relative to the fleet YAML file. `dashboard_url` is only a link target for the fleet UI; omit it when a project does not run its own dashboard. For dogfooding, add Maestro's own project config to the same fleet file so the team can watch Maestro manage Maestro alongside application repos.

Run the fleet dashboard manually:

```bash
maestro serve --fleet ~/.maestro/fleet.yaml --host 127.0.0.1 --port 8787 --read-only
```

Verify the API:

```bash
curl -fsS http://127.0.0.1:8787/api/v1/fleet
```

The fleet response includes `refreshed_at` plus per-project freshness metadata. Project cards show snapshot age and are marked stale when the latest state or log snapshot is older than 15 minutes; one project's load error is shown on that card without hiding the rest of the fleet. Queue snapshots split skipped work into `excluded`, `held/meta`, `blocked-deps`, and non-runnable project status counts so an idle project shows why no worker starts.

`--fleet` versus repeated `--config`:

| Mode | Use it when | Notes |
|---|---|---|
| `maestro serve --fleet ~/.maestro/fleet.yaml --port 8787` | You want a stable operating model for a shared dashboard or systemd service | Supports project names, relative config paths, and `dashboard_url` links |
| `maestro serve --config a.yaml --config b.yaml --port 8787` | You want a quick local aggregate view without writing a fleet file | Project names are derived from `repo`, and there is no place for `dashboard_url` metadata |

For a persistent user service, create a dedicated fleet dashboard unit:

```ini
# ~/.config/systemd/user/maestro-fleet.service
[Unit]
Description=Maestro fleet dashboard
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/maestro serve --fleet %h/.maestro/fleet.yaml --host 127.0.0.1 --port 8787 --read-only
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
```

Enable it:

```bash
systemctl --user daemon-reload
systemctl --user enable --now maestro-fleet.service
systemctl --user status maestro-fleet.service
```

Bind to the LAN only on a trusted network or behind a firewall/reverse proxy:

```bash
maestro serve --fleet ~/.maestro/fleet.yaml --host 0.0.0.0 --port 8787 --read-only
```

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

### Deploy hook eliminates manual deploys

Using `deploy_cmd` in maestro config means every merged PR is automatically deployed. This removes the "forgot to deploy" failure mode and keeps the running service in sync with `main`. The deploy command runs in the context of the local machine, so it can `ssh` to servers, `pct exec` into containers, or build locally.

---

## Checklist

Use this checklist when onboarding a new repo to maestro:

- [ ] CI workflow with build + lint + test jobs
- [ ] Branch protection on `main` requiring PR + status checks
- [ ] Labels created: `bug`, `enhancement`, `documentation`
- [ ] Exclude labels created: `wontfix`, `question`, `blocked`, `duplicate`, `invalid`
- [ ] `maestro-<project>.yaml` config file created
- [ ] `scripts/deploy.sh` written, tested manually, made executable
- [ ] Worker prompt template written with test requirements
- [ ] Post-deploy smoke test in deploy script
- [ ] Version bump configured (CI workflow or maestro `versioning` block)
- [ ] `maestro run --once` succeeds (picks an issue, spawns a worker)
- [ ] Telegram notifications working (if configured)
