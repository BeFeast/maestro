# Project Setup Runbook — How to Set Up a Repo for Maestro Auto-Merge

This runbook walks through setting up a new repository so maestro can pick up issues, spawn workers, merge PRs, and deploy — fully automated.

Throughout, we use a fictional project called **acme** as an example.

---

## 1. CI Requirements (GitHub Actions)

Maestro merges PRs only when all required status checks pass. Your repo needs at minimum:

### Required workflows

| Workflow        | Trigger            | Purpose                                |
|-----------------|--------------------|----------------------------------------|
| **build**       | `push` / `pull_request` | Compile the project, catch build errors |
| **lint**        | `push` / `pull_request` | Enforce code style and static analysis  |
| **e2e**         | `pull_request`     | End-to-end tests that cover real behavior |

All three must be **required status checks** on the `main` branch (see Section 2).

### Version bump workflow

Auto-increment the patch version on every merge to `main` so you never ship a stale version string.

Maestro has built-in versioning support. Add this to your project config:

```yaml
versioning:
  enabled: true
  files:
    - version.txt        # or package.json, Cargo.toml, etc.
  default_bump: patch
  tag_prefix: v
  create_release: false  # set true if you want GitHub Releases
```

Alternatively, use a GitHub Actions workflow that bumps and tags on merge to `main`.

### Deploy workflow

Deployment can be handled two ways:

1. **`deploy_cmd` in maestro config** (recommended) — maestro runs this command after a successful merge.
2. **GitHub Actions deploy workflow** — triggers on push to `main` via a self-hosted runner or deploy hook.

If using `deploy_cmd`, see Section 5 for deploy script requirements.

---

## 2. Branch Protection Rules

Go to **Settings → Branches → Branch protection rules** for `main`:

- **Require a pull request before merging** — prevents direct pushes.
- **Require status checks to pass before merging** — select your `build`, `lint`, and `e2e` checks.
- **Do not allow bypassing the above settings** — keeps the rules enforced for everyone, including admins.

Maestro workers never push to `main` directly; they always create PRs. The branch protection rules ensure nothing merges unless CI is green.

---

## 3. Labels Setup

Maestro uses labels to decide which issues to pick up and which to skip.

### Required labels (create these in the repo)

| Label           | Used for                  |
|-----------------|---------------------------|
| `bug`           | Bug fix issues            |
| `enhancement`   | New feature issues        |
| `documentation` | Documentation tasks       |

These are the labels you'll list in `issue_labels` in your config.

### Exclude labels

| Label       | Effect                                        |
|-------------|-----------------------------------------------|
| `wontfix`   | Issue intentionally not addressed              |
| `question`  | Discussion, not actionable work                |
| `blocked`   | Waiting on external dependency                 |
| `duplicate` | Already covered by another issue               |
| `invalid`   | Not a real issue                               |

These go in `exclude_labels` so maestro skips them even if they also have a matching label.

---

## 4. Maestro Config (`maestro-acme.yaml`)

Create a config file at `~/.maestro/maestro-acme.yaml`:

```yaml
# === Project ===
repo: your-org/acme
local_path: /home/you/repos/acme
worktree_base: /home/you/.worktrees/acme

# === Issue filtering ===
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

# === Parallelism & timeouts ===
max_parallel: 5
max_runtime_minutes: 120
worker_silent_timeout_minutes: 30    # kill stuck workers after 30 min of silence

# === PR management ===
auto_rebase: true
merge_strategy: sequential
merge_interval_seconds: 30

# === Model ===
model:
  default: claude
  backends:
    claude:
      cmd: claude

# === Worker prompt ===
worker_prompt: /home/you/repos/acme/worker-prompt.md

# === Session ===
session_prefix: acm
state_dir: ~/.maestro/acm

# === Deployment (optional) ===
deploy_cmd: /home/you/repos/acme/scripts/deploy.sh

# === Version bumping (optional) ===
versioning:
  enabled: true
  files:
    - version.txt
  default_bump: patch
  tag_prefix: v

# === Notifications (optional) ===
telegram:
  target: "YOUR_TELEGRAM_CHAT_ID"
  bot_token: "YOUR_BOT_TOKEN"
  openclaw_url: "http://localhost:18789"
```

### Key config fields

| Field              | Required | Description                                       |
|--------------------|----------|---------------------------------------------------|
| `repo`             | yes      | GitHub org/repo (e.g. `your-org/acme`)            |
| `local_path`       | yes      | Path to the local clone                           |
| `worktree_base`    | yes      | Directory where worker worktrees are created      |
| `issue_labels`     | yes      | Labels that qualify an issue for pickup (OR logic) |
| `exclude_labels`   | no       | Labels that disqualify an issue                   |
| `max_parallel`     | no       | Max concurrent workers (default: 5)               |
| `deploy_cmd`       | no       | Command to run after successful merge             |
| `worker_prompt`    | no       | Path to the worker prompt template                |
| `telegram.target`  | no       | Telegram chat ID for notifications                |

### Running maestro with this config

```bash
# One-shot run
maestro run --once --config ~/.maestro/maestro-acme.yaml

# Continuous mode
maestro run --config ~/.maestro/maestro-acme.yaml

# As a systemd service (multi-project template)
systemctl --user enable --now maestro@acme
```

---

## 5. Deploy Script (`scripts/deploy.sh`)

If your project uses `deploy_cmd`, the deploy script must follow these rules:

### Requirements

1. **Build order matters** — if the frontend is embedded in the backend binary (e.g. a Go or Rust server embedding static assets), build the frontend **before** the backend.
2. **Verify deployment success** — check that the service is actually running after deploy (e.g. `systemctl is-active`).
3. **Idempotent** — running the script twice in a row must produce the same result without errors.

### Example deploy script

```bash
#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="/home/you/repos/acme"
SERVICE_NAME="acme"

cd "$PROJECT_DIR"

# Pull latest code
git pull origin main

# Build frontend first (if embedded in backend)
echo "Building frontend..."
cd web && bun install && bun run build && cd ..

# Build backend
echo "Building backend..."
go build -o ./acme ./cmd/acme/

# Restart service
echo "Restarting $SERVICE_NAME..."
sudo systemctl restart "$SERVICE_NAME"

# Verify service is running
sleep 2
if systemctl is-active --quiet "$SERVICE_NAME"; then
    echo "Deploy successful: $SERVICE_NAME is running"
else
    echo "ERROR: $SERVICE_NAME failed to start" >&2
    exit 1
fi
```

For LXC container deployments, the script would use `pct exec` or `lxc exec` to run the build and restart inside the container.

---

## 6. Worker Prompt Requirements

The worker prompt template (`worker-prompt.md`) tells each coding agent how to do its job. Key requirements to include:

### E2E test coverage

Every feature or fix PR **must** include end-to-end tests that cover the actual behavior, not just "the page loads."

Bad:
```
test("settings page loads", () => {
  cy.visit("/settings");
  cy.get("h1").should("exist");
});
```

Good:
```
test("settings page saves and persists values", () => {
  cy.visit("/settings");
  cy.get("#api-key").clear().type("new-key");
  cy.get("#save").click();
  cy.reload();
  cy.get("#api-key").should("have.value", "new-key");
});
```

### Settings pages — save/reload roundtrip

For any settings or configuration UI, the test must:
1. Change a value
2. Save
3. Reload the page
4. Verify the value persisted

This catches bugs where the save endpoint works but the load endpoint doesn't return the new value.

### Prompt structure

A worker prompt should include:
- **Assignment** section with issue details (use `{{ISSUE_NUMBER}}`, `{{ISSUE_TITLE}}`, `{{ISSUE_BODY}}`, `{{BRANCH}}`, `{{WORKTREE}}`, `{{REPO}}` placeholders)
- **Build verification** — require the worker to verify the project compiles before starting
- **Pre-PR checklist** — format, lint, test, build must all pass before creating a PR
- **Git rules** — never push to main, always rebase on `origin/main`
- **Project structure** — overview of the codebase so the worker knows where things are
- **Stop condition** — after creating the PR, the worker must stop

See the existing `worker-prompt-template.md` and `worker-prompt-go.md` in the maestro repo for reference.

---

## 7. Smoke Test

After deployment, verify that the application is actually working.

### What to test

- Key pages return HTTP 200 (e.g. `/`, `/settings`, `/login`)
- Critical API endpoints respond correctly
- Default values load properly
- Redirects work as expected

### How to integrate

Option A: Add a smoke test step at the end of `scripts/deploy.sh`:
```bash
# Smoke test
echo "Running smoke test..."
HTTP_STATUS=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/health)
if [ "$HTTP_STATUS" != "200" ]; then
    echo "ERROR: Smoke test failed — /health returned $HTTP_STATUS" >&2
    # Alert via Telegram, PagerDuty, etc.
    exit 1
fi
echo "Smoke test passed"
```

Option B: Separate smoke test script invoked after `deploy_cmd` completes.

### Failure handling

A smoke test failure should **alert** (Telegram notification, log entry, non-zero exit code) — never fail silently. If `deploy_cmd` exits non-zero, maestro will report the failure in its logs and via Telegram if configured.

---

## 8. Lessons Learned from Panoptikon

These are hard-won lessons from running maestro on the [panoptikon](https://github.com/BeFeast/panoptikon) project:

### Frontend embedded in binary

If your backend embeds the frontend (e.g. Go's `embed` package, Rust's `include_dir!`), the frontend **must** be built before the backend. Otherwise the binary ships with stale or missing assets.

**Deploy order:** `bun build` (frontend) → `go build` / `cargo build` (backend) → restart service.

Getting this wrong means deploying a binary that serves yesterday's frontend.

### Auto-version bump prevents confusion

Without automatic version bumping, multiple PRs merge but the version string stays the same. This makes it impossible to tell which version is deployed.

Enable `versioning.enabled: true` in your maestro config. Every merge to `main` gets a unique version tag.

### Deploy hook eliminates manual deploys

Before `deploy_cmd`, merged PRs sat on `main` until someone manually deployed. With `deploy_cmd` in the maestro config, deployment happens automatically after each successful merge — no human in the loop.

---

## Checklist

Use this checklist when onboarding a new repo:

- [ ] GitHub Actions: `build`, `lint`, and `e2e` workflows exist and run on PRs
- [ ] Branch protection: PRs required, status checks required, no bypass
- [ ] Labels: `bug`, `enhancement`, `documentation` created
- [ ] Exclude labels: `wontfix`, `question`, `blocked`, `duplicate`, `invalid` created
- [ ] Maestro config: `maestro-<project>.yaml` created with repo, paths, labels
- [ ] Worker prompt: `worker-prompt.md` written with project-specific instructions
- [ ] Deploy script: `scripts/deploy.sh` builds in correct order, verifies success
- [ ] Smoke test: post-deploy verification in place, alerts on failure
- [ ] Version bumping: `versioning.enabled: true` or equivalent CI workflow
- [ ] Telegram notifications: configured for merge/deploy/failure events
- [ ] Test run: `maestro run --once --config ~/.maestro/maestro-<project>.yaml` completes successfully
