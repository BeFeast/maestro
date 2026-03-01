# Project Setup Runbook — How to Set Up a Repo for Maestro Auto-Merge

This runbook covers everything needed to prepare a new repository so maestro can pick up issues, spawn AI workers, auto-merge PRs, and deploy — with no manual intervention.

---

## 1. CI Requirements (GitHub Actions)

Maestro auto-merges PRs when **all required CI checks pass**. Your repo needs at minimum:

### Required workflows

| Workflow | Purpose | Trigger |
|----------|---------|---------|
| **build** | Compile / bundle the project | `push` to PR branches |
| **lint** | Code formatting and static analysis | `push` to PR branches |
| **test** (E2E) | End-to-end / integration tests | `push` to PR branches |

All three must report as GitHub status checks so maestro can query them via `gh pr checks`.

### Example: build + lint + test workflow

```yaml
# .github/workflows/ci.yml
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
      - uses: actions/setup-go@v5  # or setup-node, etc.
        with:
          go-version: '1.22'
      - run: go build ./cmd/myapp/

  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - run: go fmt ./... && git diff --exit-code
      - run: go vet ./...

  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - run: go test ./...
```

### Version bump workflow (optional but recommended)

Auto-increment the patch version on every merge to `main`. This prevents stale version confusion when maestro merges multiple PRs per day.

```yaml
# .github/workflows/version-bump.yml
name: Version Bump
on:
  pull_request:
    types: [closed]
    branches: [main]

jobs:
  bump:
    if: github.event.pull_request.merged == true
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      # Use maestro's built-in version bump, or your own script:
      # maestro version-bump --pr ${{ github.event.pull_request.number }}
```

Alternatively, configure `versioning` in the maestro config (see section 4).

### Deploy workflow

If your project deploys on merge, add a deploy workflow or use maestro's `deploy_cmd` (see section 5).

---

## 2. Branch Protection Rules

Maestro expects PRs to be the only way code reaches `main`. Configure branch protection on `main`:

### Required settings (GitHub → Settings → Branches → Branch protection rules)

| Setting | Value |
|---------|-------|
| **Require a pull request before merging** | Enabled |
| **Require status checks to pass before merging** | Enabled |
| **Status checks that are required** | `build`, `lint`, `test` (match your workflow job names) |
| **Require branches to be up to date before merging** | Optional (maestro handles rebasing) |
| **Do not allow bypassing the above settings** | Recommended |
| **Restrict who can push to matching branches** | No direct pushes |

### Quick setup via `gh` CLI

```bash
# Note: gh branch-protection rules require admin access
gh api repos/OWNER/REPO/branches/main/protection -X PUT \
  --input - <<'EOF'
{
  "required_status_checks": {
    "strict": false,
    "contexts": ["build", "lint", "test"]
  },
  "enforce_admins": true,
  "required_pull_request_reviews": {
    "required_approving_review_count": 0
  },
  "restrictions": null
}
EOF
```

Set `required_approving_review_count` to `0` if you want maestro to auto-merge without human review. Set it to `1` if you want a human to approve before maestro merges.

---

## 3. Labels Setup

Maestro uses labels to decide which issues to pick up and which to skip.

### Required labels (create these on GitHub)

| Label | Purpose |
|-------|---------|
| `bug` | Bug fix issues |
| `enhancement` | Feature / improvement issues |
| `documentation` | Docs-only issues |

### Exclude labels (create these too)

| Label | Purpose |
|-------|---------|
| `wontfix` | Won't be implemented |
| `question` | Discussion, not actionable |
| `blocked` | Waiting on external dependency |
| `duplicate` | Duplicate of another issue |
| `invalid` | Not a valid issue |

### Quick setup via `gh` CLI

```bash
REPO="OWNER/REPO"

# Required labels
gh label create bug          --repo "$REPO" --color d73a4a --description "Something isn't working"
gh label create enhancement  --repo "$REPO" --color a2eeef --description "New feature or request"
gh label create documentation --repo "$REPO" --color 0075ca --description "Improvements or additions to documentation"

# Exclude labels
gh label create wontfix   --repo "$REPO" --color ffffff --description "This will not be worked on"
gh label create question   --repo "$REPO" --color d876e3 --description "Further information is requested"
gh label create blocked    --repo "$REPO" --color e4e669 --description "Waiting on external dependency"
gh label create duplicate  --repo "$REPO" --color cfd3d7 --description "This issue already exists"
gh label create invalid    --repo "$REPO" --color e4e669 --description "This doesn't seem right"
```

---

## 4. Maestro Config (`maestro-<project>.yaml`)

Create a config file for your project. For multi-project setups, name it `maestro-<project>.yaml` and place it in `~/.maestro/`.

### Minimal config

```yaml
repo: OWNER/REPO
local_path: /home/user/src/myproject
worktree_base: /home/user/.worktrees/myproject
max_parallel: 3

issue_labels:
  - enhancement
  - bug
  - documentation

exclude_labels:
  - wontfix
  - question
  - blocked
  - duplicate
  - invalid

worker_prompt: /path/to/worker-prompt-template.md
```

### Full config with deploy, notifications, and versioning

```yaml
repo: OWNER/REPO
local_path: /home/user/src/myproject
worktree_base: /home/user/.worktrees/myproject
max_parallel: 5
max_runtime_minutes: 120
worker_silent_timeout_minutes: 30
auto_rebase: true
merge_strategy: sequential
merge_interval_seconds: 30
session_prefix: myp                # workers: myp-1, myp-2, ...
state_dir: ~/.maestro/myp

issue_labels:
  - enhancement
  - bug
  - documentation

exclude_labels:
  - wontfix
  - question
  - blocked
  - duplicate
  - invalid

model:
  default: claude
  backends:
    claude:
      cmd: claude

worker_prompt: /home/user/src/myproject/worker-prompt-template.md

# Deploy command — runs after each successful merge
deploy_cmd: "/home/user/src/myproject/scripts/deploy.sh"

# Files where conflicts are resolved by keeping both sides
# auto_resolve_files:
#   - server/src/api/mod.rs
#   - web/src/lib/api.ts

# Telegram notifications
telegram:
  target: "YOUR_CHAT_ID"
  bot_token: "YOUR_BOT_TOKEN"
  openclaw_url: "http://localhost:18789"

# Auto version bumping on PR merge
versioning:
  enabled: true
  files: [go.mod, package.json]
  default_bump: patch
  tag_prefix: v
  create_release: true
```

### Key fields explained

| Field | Description |
|-------|-------------|
| `repo` | GitHub `owner/repo` — maestro uses this for all `gh` operations |
| `local_path` | Path to the local git clone (must be a clean checkout of `main`) |
| `worktree_base` | Directory where worker worktrees are created (e.g. `~/.worktrees/myproject`) |
| `max_parallel` | Maximum concurrent workers — start with 3, increase after you trust the setup |
| `issue_labels` | Labels to filter issues (OR semantics — any matching label is picked) |
| `exclude_labels` | Issues with these labels are always skipped |
| `deploy_cmd` | Shell command to run after a PR is merged — typically points to `scripts/deploy.sh` |
| `worker_prompt` | Path to the Markdown prompt template given to AI workers |
| `auto_rebase` | When `true`, maestro rebases PR branches that have conflicts with `main` |
| `telegram.target` | Telegram chat/user ID for notifications |

---

## 5. Deploy Script (`scripts/deploy.sh`)

If your project deploys on merge, create a deploy script in the repo. Maestro runs `deploy_cmd` after every successful PR merge.

### Requirements

1. **Build frontend before backend** — if the frontend is embedded in the binary (e.g. Rust with `include_dir!`, Go with `embed`), the frontend must be built first
2. **Verify deploy success** — don't exit 0 if the service didn't actually start
3. **Idempotent** — running the script twice should produce the same result

### Example: Go project with systemd service

```bash
#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="/home/user/src/myproject"
SERVICE_NAME="myproject"

cd "$PROJECT_DIR"

# Pull latest (main was just updated by maestro merge)
git pull origin main

# Build
go build -o myproject ./cmd/myproject/

# Deploy binary
sudo cp myproject /usr/local/bin/myproject

# Restart service
sudo systemctl restart "$SERVICE_NAME"

# Verify it's running
sleep 2
if ! systemctl is-active --quiet "$SERVICE_NAME"; then
    echo "ERROR: $SERVICE_NAME failed to start"
    exit 1
fi

echo "Deploy successful — $SERVICE_NAME is active"
```

### Example: Rust + embedded frontend (SPA in binary)

```bash
#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="/home/user/src/myproject"
SERVICE_NAME="myproject"

cd "$PROJECT_DIR"
git pull origin main

# IMPORTANT: build frontend FIRST — it's embedded in the binary
cd web && bun install && bun run build && cd ..

# Then build backend (which embeds the frontend build output)
cargo build --release

# Deploy
sudo cp target/release/myproject /usr/local/bin/myproject
sudo systemctl restart "$SERVICE_NAME"

sleep 2
if ! systemctl is-active --quiet "$SERVICE_NAME"; then
    echo "ERROR: $SERVICE_NAME failed to start"
    exit 1
fi

echo "Deploy successful"
```

### Example: LXC container deploy

```bash
#!/usr/bin/env bash
set -euo pipefail

CONTAINER="myproject-ct"
PROJECT_DIR="/home/user/src/myproject"

cd "$PROJECT_DIR"
git pull origin main

# Build locally
go build -o myproject ./cmd/myproject/

# Push binary into LXC container
pct push "$CONTAINER" myproject /usr/local/bin/myproject

# Restart inside container
pct exec "$CONTAINER" -- systemctl restart myproject

sleep 2
if ! pct exec "$CONTAINER" -- systemctl is-active --quiet myproject; then
    echo "ERROR: myproject failed to start in container $CONTAINER"
    exit 1
fi

echo "Deploy successful in LXC $CONTAINER"
```

Make the script executable:

```bash
chmod +x scripts/deploy.sh
```

---

## 6. Worker Prompt Requirements

The worker prompt (`worker-prompt-template.md`) tells AI agents how to work on issues. It uses template variables that maestro injects at runtime:

| Variable | Value |
|----------|-------|
| `{{ISSUE_NUMBER}}` | GitHub issue number |
| `{{ISSUE_TITLE}}` | Issue title |
| `{{ISSUE_BODY}}` | Full issue body (Markdown) |
| `{{BRANCH}}` | Git branch name for this worker |
| `{{WORKTREE}}` | Absolute path to the worker's git worktree |
| `{{REPO}}` | GitHub `owner/repo` |

### E2E test requirements

Every worker prompt should include these rules for AI agents:

1. **Every feature or bug-fix PR must include E2E tests** — no PR without tests
2. **Tests must cover actual behavior** — not just "page loads" or "no errors", but real assertions on expected outcomes
3. **Settings pages must include a save-reload roundtrip test** — save a setting, reload the page, verify the value persisted

### Example prompt section for test requirements

```markdown
## Testing rules

- Every PR MUST include tests that cover the changed behavior.
- Tests must assert on actual outcomes, not just "no errors".
  - BAD: `expect(page).toBeDefined()` — this tests nothing.
  - GOOD: `expect(await page.textContent('.title')).toBe('My Item')` — this tests behavior.
- For settings/config pages: test the full roundtrip:
  1. Change a setting value
  2. Save
  3. Reload the page
  4. Verify the new value is displayed
- For API endpoints: test request + response body, not just status codes.
```

### Example prompt section for pre-PR checks

```markdown
## Before creating a PR — mandatory sequence

1. `git fetch origin`
2. `git rebase origin/main`
3. Run formatting: `go fmt ./...` (or `cargo fmt`, `bun run lint --fix`, etc.)
4. Run checks: `go vet ./...` (or `cargo check`, `bun run lint`, etc.)
5. Run tests: `go test ./...` (or `cargo test`, `bun run test`, etc.)
6. Build: `go build ./cmd/myapp/` (or `cargo build`, `bun run build`, etc.)

All must pass. If rebase has conflicts, resolve them.
```

---

## 7. Smoke Test

A post-deploy smoke test verifies that the deploy actually worked. This catches cases where the service starts but the app is broken.

### What to smoke-test

- Key pages return HTTP 200 (not 502/503)
- API health endpoint responds
- Critical functionality works (login page loads, main dashboard renders)

### Example: smoke test script

```bash
#!/usr/bin/env bash
set -euo pipefail

BASE_URL="https://myproject.example.com"

check_url() {
    local url="$1"
    local status
    status=$(curl -s -o /dev/null -w "%{http_code}" "$url")
    if [ "$status" -ne 200 ]; then
        echo "FAIL: $url returned $status"
        return 1
    fi
    echo "OK: $url"
}

FAILED=0

check_url "$BASE_URL/"           || FAILED=1
check_url "$BASE_URL/api/health" || FAILED=1
check_url "$BASE_URL/login"      || FAILED=1

if [ "$FAILED" -ne 0 ]; then
    echo "SMOKE TEST FAILED — alerting"
    # Send alert (Telegram, email, webhook, etc.)
    curl -s -X POST "http://localhost:18789/api/v1/message" \
        -H "Content-Type: application/json" \
        -d '{"chat_id": "YOUR_CHAT_ID", "text": "Smoke test FAILED after deploy of myproject"}'
    exit 1
fi

echo "All smoke tests passed"
```

### Integrate with deploy script

Call the smoke test at the end of `scripts/deploy.sh`:

```bash
# ... deploy steps above ...

# Post-deploy smoke test
echo "Running smoke test..."
if ! /home/user/src/myproject/scripts/smoke-test.sh; then
    echo "Smoke test failed — deploy may be broken"
    exit 1
fi
```

### Key principle: failure = alert, not silence

Never silently swallow smoke test failures. If the smoke test fails:
- Exit with a non-zero code (so maestro knows the deploy failed)
- Send a notification (Telegram, email, webhook)
- Maestro will report the deploy failure in its notification channel

---

## 8. Lessons Learned from Panoptikon

These are hard-won insights from running maestro on the [panoptikon](https://github.com/BeFeast/panoptikon) project.

### Frontend embedded in binary — build order matters

**Problem:** Panoptikon embeds a Svelte frontend in the Rust binary using `include_dir!`. If you run `cargo build` without building the frontend first, you get a stale or missing frontend.

**Solution:** The deploy script must always build frontend **before** backend:

```bash
# CORRECT order:
cd web && bun install && bun run build && cd ..
cargo build --release

# WRONG — stale frontend:
cargo build --release
cd web && bun run build  # too late, binary already compiled
```

This applies to any project that embeds static assets at compile time (Go `embed`, Rust `include_dir!`, etc.).

### Auto version bumping prevents stale version confusion

**Problem:** When maestro merges 5 PRs in one day, all running the same version number, it's impossible to tell which deploy corresponds to which merge.

**Solution:** Enable automatic patch version bumping on every merge to `main`. Each merge gets a unique version tag. Configure in maestro:

```yaml
versioning:
  enabled: true
  files: [Cargo.toml, package.json]  # or [go.mod, package.json]
  default_bump: patch
  tag_prefix: v
  create_release: true
```

### Deploy hook in maestro config eliminates manual deploys

**Problem:** Without `deploy_cmd`, maestro merges the PR but someone still has to SSH in and deploy manually. PRs pile up, deploys lag behind.

**Solution:** Set `deploy_cmd` in the maestro config. After every successful merge, maestro runs the deploy script automatically:

```yaml
deploy_cmd: "/home/user/src/myproject/scripts/deploy.sh"
```

This closes the loop: issue → AI worker → PR → CI → merge → deploy — fully automated.

---

## Checklist: New Project Setup

Use this checklist when onboarding a new repo for maestro:

- [ ] **CI workflows** — `build`, `lint`, `test` jobs in `.github/workflows/`
- [ ] **Branch protection** — require PR, require status checks, no direct push to `main`
- [ ] **Labels** — create `bug`, `enhancement`, `documentation` + exclude labels
- [ ] **Local clone** — `gh repo clone owner/repo ~/src/repo`
- [ ] **Worktree directory** — `mkdir -p ~/.worktrees/repo`
- [ ] **Maestro config** — `maestro-<project>.yaml` with repo, paths, labels, deploy_cmd
- [ ] **Worker prompt** — create `worker-prompt-template.md` with project-specific rules
- [ ] **Deploy script** — `scripts/deploy.sh` (idempotent, verifies success)
- [ ] **Smoke test** — `scripts/smoke-test.sh` (alerts on failure)
- [ ] **Systemd service** — `maestro init` or `maestro@.service` template
- [ ] **Test run** — `maestro run --once` to verify the full cycle
- [ ] **Telegram** — configure notifications (optional but recommended)
- [ ] **Version bumping** — enable `versioning` in config (optional but recommended)
