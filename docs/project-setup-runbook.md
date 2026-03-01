# Project Setup Runbook

How to set up a new repository so that maestro can auto-merge PRs end-to-end: from picking an issue to deploying the result.

---

## 1. CI Requirements (GitHub Actions)

Maestro merges a PR only when all required status checks pass. Your repo needs at minimum:

| Workflow          | Purpose                                      |
|-------------------|----------------------------------------------|
| **build**         | Compile / typecheck the project              |
| **lint**          | Formatting + static analysis                 |
| **e2e** (or test) | End-to-end / integration tests               |
| **version-bump**  | Auto-increment patch version on merge to main|
| **deploy**        | Build artifacts and deploy (self-hosted runner or deploy hook) |

### Example: version bump workflow

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
      - name: Bump patch version
        run: |
          # language-specific bump logic here
          # e.g. bump Cargo.toml, package.json, or a VERSION file
```

Maestro also has built-in versioning support via the `versioning` config key (see section 4).

---

## 2. Branch Protection Rules

On the **main** branch, enable these settings in GitHub → Settings → Branches → Branch protection rules:

- **Require a pull request before merging** — no direct pushes to main.
- **Require status checks to pass before merging** — select the check names from section 1 (build, lint, e2e, etc.).
- **Require branches to be up to date before merging** — prevents stale merges.

Without these rules maestro will still open PRs, but auto-merge won't engage because GitHub won't enforce the checks.

---

## 3. Labels Setup

Maestro filters issues by label. Create these labels in GitHub → Issues → Labels:

### Required (used as `issue_labels` filter)

- `bug`
- `enhancement`
- `documentation`

### Excluded (used as `exclude_labels` filter)

- `wontfix`
- `question`
- `blocked`
- `duplicate`
- `invalid`

Maestro picks issues whose labels intersect with `issue_labels` (OR semantics) and skips any issue carrying an `exclude_labels` label.

---

## 4. Maestro Config

Create a config file for your project. For multi-project setups the convention is `~/.maestro/maestro-<project>.yaml`. For single-project setups, `maestro.yaml` in the repo root works too.

```yaml
# maestro-myproject.yaml

repo: MyOrg/myproject
local_path: /home/user/repos/myproject
worktree_base: /home/user/.worktrees/myproject

issue_labels:
  - bug
  - enhancement
  - documentation
exclude_labels:
  - blocked
  - wontfix
  - duplicate
  - invalid
  - question

max_parallel: 3
max_runtime_minutes: 120
worker_silent_timeout_minutes: 15
auto_rebase: true
merge_strategy: sequential
merge_interval_seconds: 30

# AI backend
model:
  default: claude
  backends:
    claude:
      cmd: claude

# Worker prompt — path to your project's worker prompt template
worker_prompt: /home/user/repos/myproject/worker-prompt.md

# Deploy command — runs after each successful merge
deploy_cmd: "/home/user/repos/myproject/scripts/deploy.sh"

# Telegram notifications
telegram:
  target: "YOUR_CHAT_ID"
  bot_token: "YOUR_BOT_TOKEN"
  openclaw_url: "http://localhost:18789"
```

### Key fields explained

| Field | Description |
|-------|-------------|
| `repo` | GitHub `owner/repo` |
| `local_path` | Path to the main local clone |
| `worktree_base` | Directory where maestro creates per-worker git worktrees |
| `issue_labels` | Only pick issues with at least one of these labels (OR) |
| `exclude_labels` | Skip issues carrying any of these labels |
| `max_parallel` | How many workers run simultaneously (default: 5) |
| `deploy_cmd` | Shell command maestro runs after a successful merge |
| `worker_prompt` | Path to the Markdown prompt template injected into each worker |
| `telegram.target` | Telegram chat ID for notifications |

### Optional: built-in versioning

```yaml
versioning:
  enabled: true
  files:
    - path: "version.txt"
      pattern: "^(\\d+\\.\\d+\\.)(\\d+)$"
      replace: "${1}%d"
  default_bump: patch
  tag_prefix: "v"
  create_release: true
```

---

## 5. Deploy Script

Create `scripts/deploy.sh` (or `scripts/deploy-lxc.sh` for LXC-based deploys) in your repo. The `deploy_cmd` config points here.

### Requirements

1. **Build order matters** — if the frontend is embedded in the backend binary (e.g. Rust with `include_dir!`, Go with `embed`), build the frontend *before* the backend.
2. **Verify deploy success** — check that the service is actually running after restart.
3. **Idempotent** — running the script twice in a row must not break anything.

### Template

```bash
#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="/home/user/repos/myproject"
SERVICE_NAME="myproject"

cd "$PROJECT_DIR"
git pull origin main

# 1. Build frontend (if embedded in backend binary)
# cd web && bun install && bun run build && cd ..

# 2. Build backend
go build -o ./myproject ./cmd/myproject/
# or: cargo build --release

# 3. Restart service
sudo systemctl restart "$SERVICE_NAME"

# 4. Verify
sleep 2
if systemctl is-active --quiet "$SERVICE_NAME"; then
  echo "Deploy OK: $SERVICE_NAME is running"
else
  echo "Deploy FAILED: $SERVICE_NAME is not active" >&2
  exit 1
fi
```

Make it executable: `chmod +x scripts/deploy.sh`.

---

## 6. Worker Prompt Requirements

Each project needs a worker prompt template (`worker-prompt.md` or similar). Maestro injects these variables:

- `{{ISSUE_NUMBER}}`, `{{ISSUE_TITLE}}`, `{{ISSUE_BODY}}`
- `{{BRANCH}}`, `{{WORKTREE}}`, `{{REPO}}`

### What the prompt must enforce

1. **E2E tests for every feature/fix PR** — the worker should write tests, not just code.
2. **Tests must cover actual behavior** — not just "page loads" or "endpoint returns 200". Test the real logic.
3. **Settings pages must include a save-reload roundtrip test** — POST new values, GET them back, assert they match.
4. **Smoke test before opening PR** — the worker should verify its changes actually work (curl, run the binary, etc.) and document the result.
5. **Scope discipline** — implement only what the issue asks, don't refactor unrelated code.
6. **Mandatory pre-PR sequence** — fetch, rebase, format, lint, test, build, push, create PR.

See `worker-prompt-template.md` in this repo for a full example targeting a Rust/TypeScript stack, or `worker-prompt-go.md` for a Go project.

---

## 7. Smoke Test

After deploy, a smoke test should verify that key pages and endpoints are reachable.

### Guidelines

- Hit the main page and at least one API endpoint.
- Assert HTTP 200 (or the expected status code).
- On failure: **alert** (Telegram, etc.), do not fail silently.
- The smoke test can live in CI, in the deploy script, or as a separate post-deploy step.

### Example (in the deploy script)

```bash
# Post-deploy smoke test
SMOKE_URL="http://localhost:8080/api/health"
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$SMOKE_URL")
if [ "$STATUS" != "200" ]; then
  echo "SMOKE TEST FAILED: $SMOKE_URL returned $STATUS" >&2
  # send alert via telegram / webhook
  exit 1
fi
echo "Smoke test passed"
```

---

## 8. Lessons Learned (from Panoptikon)

Real-world gotchas discovered while running maestro on the [panoptikon](https://github.com/BeFeast/panoptikon) project:

### Frontend embedded in binary

If your frontend is bundled into the backend binary (e.g. `include_dir!` in Rust, `//go:embed` in Go):

> **`bun build` (or equivalent) MUST run before `cargo build` / `go build`.**

If you build the backend first, it embeds stale (or missing) frontend assets. The deploy looks successful but serves an old UI.

### Auto version bump prevents confusion

Without automatic version bumping, multiple merges in quick succession all report the same version. When debugging, you can't tell which build is running. Add a version-bump workflow or use maestro's built-in `versioning` config.

### Deploy hook eliminates manual deploys

Putting `deploy_cmd` in the maestro config means every merged PR is deployed automatically. No more "I merged it but forgot to deploy" situations. The deploy script runs in the maestro process, so failures show up in maestro logs and Telegram alerts.

---

## Checklist

Use this checklist when onboarding a new project:

- [ ] GitHub Actions: build, lint, and test workflows exist and pass
- [ ] Branch protection: PRs required, status checks enforced on main
- [ ] Labels: `bug`, `enhancement`, `documentation` created; exclude labels created
- [ ] Maestro config: `maestro-<project>.yaml` written with correct paths and labels
- [ ] Worker prompt: template created with pre-PR sequence, test requirements, and project structure
- [ ] Deploy script: builds in correct order, verifies service health, is idempotent
- [ ] Smoke test: key endpoints checked post-deploy, failures alert (not silent)
- [ ] First dry run: `maestro run --once --config maestro-<project>.yaml` picks an issue and opens a PR
