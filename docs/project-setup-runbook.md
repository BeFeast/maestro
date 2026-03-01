# Project Setup Runbook

How to set up a new repository so maestro can pick up issues, spawn workers, merge PRs, and deploy — fully automated.

---

## 1. CI Requirements (GitHub Actions)

Maestro waits for all required status checks to pass before merging a PR. Your repo needs at minimum:

### Required workflows

| Workflow | Purpose | Trigger |
|----------|---------|---------|
| **build** | Compile the project, run unit tests | `pull_request` to `main` |
| **lint** | Code formatting and static analysis | `pull_request` to `main` |
| **e2e** | End-to-end / integration tests | `pull_request` to `main` |

These check names must match what you configure as required status checks on the `main` branch (see section 2).

### Version bump workflow (optional but recommended)

Auto-increment the patch version on every merge to `main`. This prevents stale version confusion and gives every build a unique tag.

```yaml
# .github/workflows/version-bump.yml
name: version-bump
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
      # your version bump logic here — update version file, tag, push
```

Alternatively, maestro has built-in version bumping. Configure it in your maestro config:

```yaml
versioning:
  enabled: true
  files:
    - cmd/myapp/main.go   # files containing the version string
  default_bump: "patch"
  tag_prefix: "v"
  create_release: true
```

### Deploy workflow (optional)

If you use GitHub Actions for deployment instead of maestro's `deploy_cmd`, add a workflow triggered on push to `main` or on release creation.

---

## 2. Branch Protection Rules

Go to **Settings → Branches → Add branch protection rule** for `main`:

- **Branch name pattern:** `main`
- **Require a pull request before merging** — enabled
  - Require approvals: 0 (maestro PRs won't have human reviewers)
- **Require status checks to pass before merging** — enabled
  - Add each required check: `build`, `lint`, `e2e` (must match your workflow job names)
- **Do not allow bypassing the above settings** — enabled
- **Block direct pushes** — no one pushes to main directly

Maestro uses `gh pr merge --auto --squash` so the PR merges as soon as checks pass. Without required checks, `--auto` has nothing to wait for and the PR merges immediately — which defeats the purpose.

---

## 3. Labels Setup

Maestro uses labels to decide which issues to pick up and which to skip.

### Required labels (create these in your repo)

| Label | Purpose |
|-------|---------|
| `bug` | Bug fix issues |
| `enhancement` | New features / improvements |
| `documentation` | Docs-only changes |

These should match `issue_labels` in your maestro config. Maestro picks up issues that have **any** of the listed labels (OR semantics).

### Exclude labels

| Label | Purpose |
|-------|---------|
| `wontfix` | Intentionally not addressing |
| `question` | Discussion, not actionable |
| `blocked` | Waiting on external dependency |
| `duplicate` | Already tracked elsewhere |
| `invalid` | Not a real issue |

Issues with any of these labels are skipped. Configure them in `exclude_labels`.

Create all labels via the GitHub UI (**Issues → Labels**) or with the CLI:

```bash
gh label create bug --repo YOUR_ORG/YOUR_REPO --color d73a4a
gh label create enhancement --repo YOUR_ORG/YOUR_REPO --color a2eeef
gh label create documentation --repo YOUR_ORG/YOUR_REPO --color 0075ca
gh label create wontfix --repo YOUR_ORG/YOUR_REPO --color ffffff
gh label create question --repo YOUR_ORG/YOUR_REPO --color d876e3
gh label create blocked --repo YOUR_ORG/YOUR_REPO --color fbca04
gh label create duplicate --repo YOUR_ORG/YOUR_REPO --color cfd3d7
gh label create invalid --repo YOUR_ORG/YOUR_REPO --color e4e669
```

---

## 4. Maestro Config

Create a config file for your project. Convention: `maestro-<project>.yaml` in `~/.maestro/` or alongside the maestro binary.

### Minimal config

```yaml
repo: YOUR_ORG/YOUR_REPO
local_path: ~/src/your-repo
worktree_base: ~/worktrees/your-repo

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

max_parallel: 3
worker_prompt: /path/to/worker-prompt.md
```

### Full config with deploy and notifications

```yaml
repo: YOUR_ORG/YOUR_REPO
local_path: ~/src/your-repo
worktree_base: ~/worktrees/your-repo

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

max_parallel: 5
max_runtime_minutes: 120
worker_silent_timeout_minutes: 30
auto_rebase: true
merge_strategy: sequential
merge_interval_seconds: 30

session_prefix: myapp
state_dir: ~/.maestro/myapp

worker_prompt: /path/to/worker-prompt.md

deploy_cmd: "cd ~/src/your-repo && scripts/deploy.sh"

telegram:
  target: "YOUR_TELEGRAM_USER_ID"
  bot_token: "YOUR_TELEGRAM_BOT_TOKEN"

versioning:
  enabled: true
  files:
    - cmd/myapp/main.go
  default_bump: "patch"
  tag_prefix: "v"
  create_release: true
```

### Key fields explained

| Field | Description |
|-------|-------------|
| `repo` | GitHub `owner/repo` — used for issue queries and PR operations |
| `local_path` | Local clone of the repo (maestro runs `git fetch` here) |
| `worktree_base` | Directory where worker worktrees are created (one per issue) |
| `issue_labels` | Only pick up issues with at least one of these labels |
| `exclude_labels` | Skip issues with any of these labels |
| `max_parallel` | Max concurrent workers — tune based on machine resources |
| `deploy_cmd` | Shell command to run after a PR merges (runs in `local_path`) |
| `worker_prompt` | Path to the markdown file with worker instructions |
| `auto_rebase` | Auto-rebase PR branches that have conflicts with main |
| `merge_strategy` | `sequential` (one at a time, safer) or `parallel` |

### Running with this config

```bash
# One-shot (process current issues and exit)
maestro run --config ~/.maestro/maestro-myapp.yaml --once

# Continuous loop
maestro run --config ~/.maestro/maestro-myapp.yaml

# As a systemd service (see maestro@.service template)
systemctl --user start maestro@myapp
```

---

## 5. Deploy Script

If you set `deploy_cmd` in your config, maestro runs it after each successful PR merge. The command runs via `bash -c` with a 5-minute timeout, in the `local_path` directory.

### Requirements

1. **Build order matters** — if your frontend is embedded in the binary (e.g., Go embed, Rust include_bytes), build the frontend **before** the backend.
2. **Verify deploy success** — check that the service is actually running after restart.
3. **Idempotent** — running the script twice in a row must not break anything.

### Example: `scripts/deploy.sh`

```bash
#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_DIR"

# Pull latest changes
git pull origin main

# Build frontend first (if embedded in binary)
# echo "Building frontend..."
# cd web && bun install && bun run build && cd ..

# Build backend
echo "Building backend..."
go build -o myapp ./cmd/myapp/

# Deploy binary
echo "Deploying..."
sudo cp myapp /usr/local/bin/myapp
sudo systemctl restart myapp

# Verify
sleep 2
if systemctl is-active --quiet myapp; then
    echo "Deploy successful — myapp is running"
else
    echo "ERROR: myapp failed to start after deploy" >&2
    exit 1
fi
```

### Example: LXC container deploy (`scripts/deploy-lxc.sh`)

```bash
#!/usr/bin/env bash
set -euo pipefail

CONTAINER="myapp-prod"
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_DIR"

git pull origin main

# Build inside container or copy binary
go build -o myapp ./cmd/myapp/
lxc file push myapp "$CONTAINER/usr/local/bin/myapp"
lxc exec "$CONTAINER" -- systemctl restart myapp

sleep 2
if lxc exec "$CONTAINER" -- systemctl is-active --quiet myapp; then
    echo "Deploy successful"
else
    echo "ERROR: deploy failed" >&2
    exit 1
fi
```

### What happens on failure

If `deploy_cmd` exits non-zero, maestro sends a Telegram notification:

> ⚠️ maestro: deploy failed after PR #42 merge

The merge is **not** rolled back — fix the deploy issue manually and re-run.

---

## 6. Worker Prompt Requirements

The worker prompt (`worker_prompt` in config) tells each spawned agent how to work on the repo. It must cover project-specific conventions. Key requirements for quality PRs:

### Every PR must include tests

- Feature PRs: include E2E or integration tests that cover the actual behavior.
- Bug fix PRs: include a regression test that would have caught the bug.
- Tests must verify real behavior, not just "page loads" or "no errors."

### Settings pages: save-reload roundtrip

If the PR touches a settings or configuration page, the test must:

1. Save a new value.
2. Reload / re-fetch the page.
3. Verify the saved value persists.

### Template variables

Maestro injects these variables into your worker prompt before passing it to the agent:

| Variable | Value |
|----------|-------|
| `{{REPO}}` | GitHub `owner/repo` |
| `{{ISSUE_NUMBER}}` | Issue number being worked on |
| `{{ISSUE_TITLE}}` | Issue title |
| `{{ISSUE_BODY}}` | Full issue description |
| `{{BRANCH}}` | Branch name for this worker |
| `{{WORKTREE}}` | Path to the worker's git worktree |

### Prompt structure recommendations

Your worker prompt should include (see `worker-prompt-template.md` for a full example):

1. **Build verification** — worker must verify the project compiles before making changes.
2. **Git hygiene** — branch conventions, no force-push to main.
3. **Pre-PR checklist** — rebase, format, lint, test, build — all must pass.
4. **Project structure** — list key directories and files so the agent knows where things are.
5. **Code conventions** — formatting, naming, patterns to follow.
6. **Scope discipline** — implement only what the issue asks for, no extra refactoring.
7. **Smoke test** — verify the change works before opening the PR.
8. **Stop condition** — after PR creation, stop. Don't keep iterating.

---

## 7. Smoke Test

Workers should verify their changes actually work before opening a PR. This is defined in the worker prompt, not in maestro core.

### What to include in the worker prompt

```markdown
## Before opening PR — smoke test your changes

After implementing, verify the feature actually works:
- If you added an API endpoint: curl it and verify the response
- If you changed a settings page: POST a new value, GET settings, verify it persisted
- If you changed a UI component: verify the build succeeds and the component renders
- If you changed a redirect: verify the target URL is correct

Document your smoke test result in the PR body under "## Smoke Test".
```

### Post-deploy smoke test

For production verification after deploy, add checks to your deploy script:

```bash
# After service restart, verify key endpoints
sleep 3
HTTP_STATUS=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/health)
if [ "$HTTP_STATUS" != "200" ]; then
    echo "ERROR: health check failed (HTTP $HTTP_STATUS)" >&2
    # Send alert (maestro handles this if deploy_cmd fails)
    exit 1
fi
echo "Smoke test passed — health endpoint returned 200"
```

Failure must result in a non-zero exit code so maestro sends an alert — never fail silently.

---

## 8. Lessons Learned (from panoptikon)

Real-world issues encountered while running maestro on the panoptikon project:

### Frontend embedded in binary

If your frontend (e.g., Next.js, Vite, SvelteKit) is embedded in the backend binary via `go:embed`, Rust `include_bytes!`, or similar:

- `bun build` (or equivalent) **must** run **before** `cargo build` / `go build`.
- If the frontend build output doesn't exist at compile time, the binary either fails to compile or ships with stale assets.
- Put the build order in both your deploy script and your CI workflow.

### Auto-version bump prevents confusion

Without auto-versioning, multiple PRs merge to main but the version string stays the same. This makes it impossible to tell which build is running in production.

- Enable `versioning` in your maestro config, or add a GitHub Actions workflow that bumps the version on every push to `main`.
- Every merge = new version = no ambiguity.

### Deploy hook eliminates manual deploys

Before `deploy_cmd`, every merge required someone to SSH in and deploy manually. With `deploy_cmd` in the maestro config:

- PR merges → maestro runs deploy → service restarts → notification sent.
- No human in the loop for routine deploys.
- Failures are caught and reported via Telegram.

---

## Checklist

Use this checklist when setting up a new project for maestro:

- [ ] Repository has CI workflows for build, lint, and E2E tests
- [ ] Branch protection on `main`: require PR, require status checks
- [ ] Labels created: `bug`, `enhancement`, `documentation`
- [ ] Exclude labels created: `wontfix`, `question`, `blocked`, `duplicate`, `invalid`
- [ ] Maestro config file created (`maestro-<project>.yaml`)
- [ ] `repo`, `local_path`, `worktree_base` set correctly
- [ ] `issue_labels` and `exclude_labels` configured
- [ ] Worker prompt written with project-specific conventions
- [ ] Deploy script created, tested, and is idempotent
- [ ] Deploy script verifies service health after restart
- [ ] Version bumping configured (maestro config or GitHub Actions)
- [ ] Telegram notifications configured
- [ ] Systemd service installed and enabled (`maestro@<project>`)
- [ ] Run `maestro run --config <config> --once` to verify setup
