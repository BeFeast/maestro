# Project Setup Runbook for Maestro Auto-Merge

How to configure a new repository so maestro can pick up issues, spawn workers, merge PRs, and deploy automatically.

---

## 1. CI Requirements (GitHub Actions)

Maestro merges PRs only when all required status checks pass. Your repo needs these workflows at minimum:

### Build & Lint

A workflow that runs on every PR and push to `main`:

```yaml
# .github/workflows/ci.yml
name: CI
on:
  pull_request:
  push:
    branches: [main]

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Build
        run: go build ./...    # or cargo build, bun run build, etc.
      - name: Lint
        run: go vet ./...      # or cargo clippy, eslint, etc.
      - name: Test
        run: go test ./...     # or cargo test, bun test, etc.
```

### E2E Tests

If your project has a frontend or API surface, add an E2E job. Maestro workers are instructed to include E2E tests with every PR, so CI must run them:

```yaml
  e2e:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: E2E tests
        run: bun run test:e2e  # or playwright test, cypress run, etc.
```

### Version Bump (on merge to main)

Auto-increment the patch version on every merge to `main` to prevent stale version confusion. Maestro also has built-in versioning support (see section 4), but a CI workflow is a common alternative:

```yaml
# .github/workflows/version-bump.yml
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
      - name: Bump patch version
        run: |
          # project-specific: update version file, tag, push
```

### Deploy Workflow

If you use GitHub Actions for deployment (instead of maestro's `deploy_cmd`), add a deploy workflow triggered on push to `main` or on release.

**Key point:** all CI job names that maestro should wait for must be registered as required status checks (see section 2).

---

## 2. Branch Protection Rules

Go to **Settings > Branches > Add rule** for `main`:

| Setting | Value |
|---|---|
| Require a pull request before merging | Yes |
| Require approvals | 0 (maestro merges without human approval) |
| Require status checks to pass before merging | Yes |
| Status checks that are required | `build`, `lint`, `test`, `e2e` (match your CI job names) |
| Require branches to be up to date before merging | Recommended (maestro auto-rebases) |
| Allow force pushes | No |
| Allow deletions | No |

**No direct push to `main`.** Maestro creates branches, opens PRs, and merges via the GitHub API after checks pass.

---

## 3. Labels Setup

Maestro uses labels to decide which issues to pick up and which to skip.

### Required labels (create these in the repo)

- `bug` — bug fix issues
- `enhancement` — feature requests
- `documentation` — docs-only changes

These are the labels maestro workers look for by default (configured via `issue_labels` in the config).

### Exclude labels (create these too)

- `wontfix` — decided not to fix
- `question` — not actionable
- `blocked` — waiting on something external
- `duplicate` — duplicate of another issue
- `invalid` — not a real issue

Issues with any exclude label are skipped even if they also have a matching `issue_labels` label.

To create labels via CLI:

```bash
gh label create bug --color d73a4a --repo YOUR_ORG/YOUR_REPO
gh label create enhancement --color a2eeef --repo YOUR_ORG/YOUR_REPO
gh label create documentation --color 0075ca --repo YOUR_ORG/YOUR_REPO
gh label create wontfix --color ffffff --repo YOUR_ORG/YOUR_REPO
gh label create question --color d876e3 --repo YOUR_ORG/YOUR_REPO
gh label create blocked --color e4e669 --repo YOUR_ORG/YOUR_REPO
gh label create duplicate --color cfd3d7 --repo YOUR_ORG/YOUR_REPO
gh label create invalid --color e4e669 --repo YOUR_ORG/YOUR_REPO
```

---

## 4. Maestro Config (`maestro-<project>.yaml`)

Create a config file in the maestro directory. For multi-project setups, name it `maestro-<project>.yaml` (e.g. `maestro-panoptikon.yaml`).

### Minimal config

```yaml
repo: YOUR_ORG/YOUR_REPO
local_path: /path/to/local/clone
worktree_base: /path/to/worktrees/your-repo
max_parallel: 3

issue_labels:
  - bug
  - enhancement
  - documentation

exclude_labels:
  - blocked
  - wontfix
  - question
  - duplicate
  - invalid

worker_prompt: /path/to/worker-prompt-template.md
```

### Full config with deploy and notifications

```yaml
repo: YOUR_ORG/YOUR_REPO
local_path: /path/to/local/clone
worktree_base: /path/to/worktrees/your-repo
max_parallel: 5
max_runtime_minutes: 120
worker_silent_timeout_minutes: 30
worker_max_tokens: 0               # 0 = unlimited

auto_rebase: true                   # auto-rebase conflicting PR branches
merge_strategy: sequential          # "sequential" or "parallel"
merge_interval_seconds: 30

issue_labels:
  - bug
  - enhancement
  - documentation

exclude_labels:
  - blocked
  - wontfix
  - question
  - duplicate
  - invalid

worker_prompt: /path/to/worker-prompt-template.md

# Auto-resolve merge conflicts by keeping both sides in these files:
# auto_resolve_files:
#   - src/api/mod.rs
#   - src/lib/types.ts

# Command to run after a PR is successfully merged
deploy_cmd: "cd /path/to/repo && scripts/deploy.sh"

# Auto version bump on merge
versioning:
  enabled: true
  files:
    - version.txt           # or Cargo.toml, package.json, etc.
  default_bump: patch        # patch | minor | major
  tag_prefix: v
  create_release: false

# Model backends
model:
  default: claude
  backends:
    claude:
      cmd: claude

# Telegram notifications
telegram:
  target: "YOUR_CHAT_ID"
  bot_token: "YOUR_BOT_TOKEN"
  openclaw_url: "http://localhost:18789"
```

### Config field reference

| Field | Required | Default | Description |
|---|---|---|---|
| `repo` | Yes | — | GitHub `owner/repo` |
| `local_path` | Yes | — | Path to the local git clone |
| `worktree_base` | Yes | — | Directory where maestro creates git worktrees |
| `max_parallel` | No | 5 | Max concurrent workers |
| `max_runtime_minutes` | No | 120 | Hard timeout per worker (minutes) |
| `worker_silent_timeout_minutes` | No | 0 (disabled) | Kill worker if tmux output is unchanged for N minutes |
| `worker_max_tokens` | No | 0 (unlimited) | Kill worker when cumulative token usage exceeds this |
| `auto_rebase` | No | true | Auto-rebase conflicting PR branches |
| `merge_strategy` | No | sequential | `sequential` or `parallel` |
| `merge_interval_seconds` | No | 30 | Min seconds between sequential merges |
| `issue_labels` | No | — | Labels to filter issues (OR semantics) |
| `exclude_labels` | No | — | Labels to skip |
| `worker_prompt` | No | — | Path to worker prompt Markdown template |
| `deploy_cmd` | No | — | Shell command to run after successful merge |
| `auto_resolve_files` | No | — | Files to auto-resolve conflicts by keeping both sides |
| `telegram.target` | No | — | Telegram chat/user ID |
| `telegram.bot_token` | No | — | Telegram bot token |
| `telegram.openclaw_url` | No | `http://localhost:18789` | OpenClaw gateway URL |

---

## 5. Deploy Script

If you use `deploy_cmd`, point it at a deploy script in your repo. The script must be **idempotent** — safe to run multiple times with the same result.

### Example: `scripts/deploy.sh`

```bash
#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="/path/to/your/project"
SERVICE_NAME="your-service"

cd "$PROJECT_DIR"

# Pull latest changes
git pull origin main

# Build (see "Frontend embedded in binary" note below)
go build -o ./bin/app ./cmd/app/

# Restart service
sudo systemctl restart "$SERVICE_NAME"

# Verify deploy success
sleep 2
if ! systemctl is-active --quiet "$SERVICE_NAME"; then
    echo "DEPLOY FAILED: $SERVICE_NAME is not running"
    exit 1
fi

echo "Deploy successful: $SERVICE_NAME is active"
```

### Frontend embedded in binary

If your backend embeds a frontend (e.g. Go `embed`, Rust `include_dir!`), **build the frontend BEFORE the backend**:

```bash
# Frontend FIRST
cd "$PROJECT_DIR/web"
bun install
bun run build

# Backend SECOND (embeds frontend build output)
cd "$PROJECT_DIR"
cargo build --release
# or: go build -o ./bin/app ./cmd/app/
```

Getting this order wrong means the binary ships with stale frontend assets.

### LXC container deploys

For deploy scripts that target LXC containers (e.g. `scripts/deploy-lxc.sh`), push the built binary into the container and restart:

```bash
#!/usr/bin/env bash
set -euo pipefail

CONTAINER="your-container"
# ... build steps ...

pct push "$CONTAINER" ./bin/app /opt/app/bin/app
pct exec "$CONTAINER" -- systemctl restart your-service

sleep 2
if ! pct exec "$CONTAINER" -- systemctl is-active --quiet your-service; then
    echo "DEPLOY FAILED"
    exit 1
fi
```

---

## 6. Worker Prompt Requirements

The worker prompt template (`worker_prompt` in config) tells AI workers how to work on issues. Every worker prompt should enforce these rules:

### E2E tests are mandatory

Every feature or bug fix PR must include E2E tests. The prompt should state this clearly:

> Every PR must include E2E tests that cover the actual behavior being added or fixed.

### Tests must cover real behavior

Tests should verify the feature works, not just that a page loads:

- **Bad:** `test('settings page loads', ...)`
- **Good:** `test('changing theme setting persists after reload', ...)`

### Settings pages: save-reload roundtrip

For any settings or configuration UI, tests must verify the full roundtrip:

1. Change a setting
2. Save
3. Reload the page
4. Verify the setting persisted

### Template variables

The worker prompt supports these injected variables:

| Variable | Description |
|---|---|
| `{{ISSUE_NUMBER}}` | GitHub issue number |
| `{{ISSUE_TITLE}}` | Issue title |
| `{{ISSUE_BODY}}` | Full issue body |
| `{{BRANCH}}` | Git branch name |
| `{{WORKTREE}}` | Worktree path on disk |
| `{{REPO}}` | GitHub `owner/repo` |

---

## 7. Smoke Test

After deploy, verify that the service actually works. A silent failure is worse than a loud one.

### Post-deploy smoke test

Add a smoke test to your deploy script or as a separate step:

```bash
# Check key endpoints
ENDPOINTS=(
    "http://localhost:8080/health"
    "http://localhost:8080/api/v1/status"
    "http://localhost:8080/"
)

for url in "${ENDPOINTS[@]}"; do
    status=$(curl -s -o /dev/null -w '%{http_code}' "$url")
    if [ "$status" -ne 200 ]; then
        echo "SMOKE TEST FAILED: $url returned $status"
        exit 1
    fi
done

echo "Smoke test passed"
```

### Failure = alert, not silent

If the smoke test fails, the deploy script should exit with a non-zero code. Maestro will pick up the failure and send a Telegram notification (if configured). Never swallow errors silently.

---

## 8. Lessons Learned from Panoptikon

These are hard-won lessons from running maestro on the [panoptikon](https://github.com/BeFeast/panoptikon) project.

### Frontend embedded in binary: build order matters

Panoptikon embeds a Next.js frontend in a Rust binary using `include_dir!`. The frontend (`bun run build`) **must** complete before the backend (`cargo build`). Getting this wrong ships a binary with stale or missing frontend assets. If your project uses Go's `embed` directive or similar, the same rule applies.

### Auto-version bump prevents stale version confusion

Without automatic version bumping, multiple merges in quick succession can ship the same version string. This makes debugging deployments difficult — you can't tell which version is actually running. Enable versioning in maestro config or add a CI workflow that bumps the version on every merge to `main`.

### Deploy hook in maestro config eliminates manual deploys

Setting `deploy_cmd` in the maestro config means every successful merge triggers a deploy automatically. No human needs to SSH in and restart services. Combined with the smoke test, this creates a fully automated pipeline: issue → PR → merge → deploy → verify.

---

## Quick Checklist

Use this checklist when onboarding a new project:

- [ ] CI workflows exist: build, lint, test, (e2e)
- [ ] Branch protection enabled on `main` with required status checks
- [ ] Labels created: `bug`, `enhancement`, `documentation`
- [ ] Exclude labels created: `wontfix`, `question`, `blocked`, `duplicate`, `invalid`
- [ ] `maestro-<project>.yaml` created with `repo`, `local_path`, `worktree_base`
- [ ] `issue_labels` and `exclude_labels` configured
- [ ] Worker prompt template written and path set in config
- [ ] Deploy script is idempotent and verifies success
- [ ] Frontend builds before backend (if embedded)
- [ ] Smoke test checks key endpoints after deploy
- [ ] Telegram notifications configured (optional but recommended)
- [ ] Versioning configured (auto-bump on merge)
