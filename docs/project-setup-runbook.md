# Project Setup Runbook

How to set up a new repository so maestro can pick up issues, spawn workers,
merge PRs, deploy, and notify — all automatically.

---

## 1. CI requirements (GitHub Actions)

Maestro waits for all required status checks to pass before merging a PR.
Your repo needs at minimum:

| Workflow | Purpose |
|----------|---------|
| **build** | Compile/bundle the project — proves nothing is broken |
| **lint** | Static analysis (clippy, eslint, golangci-lint, etc.) |
| **e2e** | End-to-end tests that exercise real behaviour |

Optional but recommended:

| Workflow | Purpose |
|----------|---------|
| **version-bump** | Auto-increment patch version on merge to `main` |
| **deploy** | Trigger deploy on merge (self-hosted runner or webhook) |

### Version bump workflow

Auto-incrementing the version on every merge to `main` prevents stale version
confusion. Maestro also has a built-in `version-bump` command
(`maestro version-bump`) and a `versioning` config section that can handle this
without a separate workflow — choose one approach, not both.

### Key rule

Every check that gates code quality **must** be listed as a required status
check on the `main` branch (see section 2). If a check exists but is not
required, maestro will merge without waiting for it.

---

## 2. Branch protection rules

Go to **Settings → Branches → Branch protection rules** for `main`:

- [x] **Require a pull request before merging**
- [x] **Require status checks to pass before merging** — add every CI job
      name (build, lint, e2e, etc.)
- [x] **Do not allow bypassing the above settings**
- [x] **Restrict direct pushes** — nobody should push straight to `main`

Maestro merges via `gh pr merge`, so it respects these rules like any other
contributor.

---

## 3. Labels setup

Create these labels in **Issues → Labels** (or via `gh label create`):

### Required (maestro picks issues with these)

| Label | Colour suggestion |
|-------|-------------------|
| `bug` | `#d73a4a` |
| `enhancement` | `#a2eeef` |
| `documentation` | `#0075ca` |

### Excluded (maestro skips issues with these)

| Label | Purpose |
|-------|---------|
| `wontfix` | Intentionally not fixing |
| `question` | Discussion, not actionable |
| `blocked` | Waiting on external dependency |
| `duplicate` | Already tracked elsewhere |
| `invalid` | Not a real issue |

These are configured in `issue_labels` and `exclude_labels` in the maestro
config (section 4).

---

## 4. Maestro config (`maestro-<project>.yaml`)

Create a config file for the project. For multi-project setups, name it
`maestro-<project>.yaml` and pass it with `--config`.

```yaml
repo: your-org/your-repo
local_path: /path/to/local/repo
worktree_base: /path/to/worktrees/your-repo

# How many workers run in parallel
max_parallel: 5

# Hard timeout per worker (minutes)
max_runtime_minutes: 120

# Kill worker if tmux output is unchanged for N minutes (0 = disabled)
worker_silent_timeout_minutes: 15

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

# AI backend
model:
  default: claude
  backends:
    claude:
      cmd: claude

# Worker prompt template
worker_prompt: /path/to/worker-prompt-template.md

# Merge strategy
auto_rebase: true
merge_strategy: sequential
merge_interval_seconds: 30

# Deploy command — runs after a PR is merged successfully
deploy_cmd: "/path/to/your-repo/scripts/deploy.sh"

# Telegram notifications
telegram:
  target: "YOUR_CHAT_ID"
  bot_token: "YOUR_BOT_TOKEN"
  openclaw_url: "http://localhost:18789"
```

### Key fields

| Field | Description |
|-------|-------------|
| `repo` | GitHub `owner/repo` — maestro uses `gh` CLI against this |
| `local_path` | Local clone of the repo (must already exist) |
| `worktree_base` | Directory where maestro creates per-worker worktrees |
| `issue_labels` | Only pick issues with at least one of these labels (OR logic) |
| `exclude_labels` | Skip issues with any of these labels |
| `max_parallel` | Cap on concurrent workers |
| `deploy_cmd` | Shell command to run after a successful merge |
| `telegram.target` | Telegram chat ID for notifications |

See `maestro.yaml.example` in the repo root for the full set of options
including routing, versioning, and auto-resolve files.

---

## 5. Deploy script (`scripts/deploy.sh`)

The `deploy_cmd` in your config should point to a script in the target repo.
This script must be **idempotent** — running it twice should produce the same
result.

### Template

```bash
#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="/path/to/your-repo"
SERVICE_NAME="your-service"

cd "$PROJECT_DIR"
git pull origin main

# --- Build ---
# If your frontend is embedded in the binary (e.g. Rust with include_dir!,
# Go with embed), build the frontend FIRST.
# cd web && bun install && bun run build && cd ..

# Build the backend
# go build -o ./your-binary ./cmd/your-app/
# cargo build --release

# --- Deploy ---
sudo systemctl restart "$SERVICE_NAME"

# --- Verify ---
sleep 3
if ! systemctl is-active --quiet "$SERVICE_NAME"; then
  echo "FATAL: $SERVICE_NAME failed to start"
  exit 1
fi

echo "Deploy successful: $SERVICE_NAME is running"
```

### Rules

1. **Frontend before backend** — if the frontend is embedded in the binary
   (e.g. `include_dir!` in Rust, `//go:embed` in Go), the frontend build
   must complete before the backend build starts.
2. **Verify the deploy** — check `systemctl is-active` (or equivalent) and
   exit non-zero on failure. Maestro reports the exit code in notifications.
3. **Idempotent** — the script must be safe to run repeatedly. No "already
   exists" errors, no duplicate migrations, no leftover state.

---

## 6. Worker prompt requirements

The worker prompt template (`worker_prompt` in config) tells AI workers how to
approach issues. Include these rules in your prompt:

### Testing requirements

- Every feature or bug-fix PR **must** include E2E tests.
- Tests must cover actual behaviour, not just "page loads" or "returns 200".
- For settings/config pages: include a **save → reload → verify** roundtrip
  test that proves the value persists.

### Example prompt snippet

```markdown
## Testing rules
- Every PR must include at least one E2E test for the changed behaviour.
- "Page loads without error" is not a valid test. Test the actual feature.
- Settings pages: write a test that saves a value, reloads the page, and
  asserts the saved value is still there.
```

Adapt the specifics to your project's test framework (Playwright, Cypress,
Go's `testing` package, etc.).

---

## 7. Smoke test

After deploy, verify the service is actually working — not just running.

### What to check

- Key pages return HTTP 200 (or expected status).
- Critical API endpoints respond with valid data.
- Health-check endpoint (if any) reports healthy.

### Implementation

Add a post-deploy smoke test to your deploy script or as a separate step:

```bash
# After deploy, verify key endpoints
SMOKE_URLS=(
  "http://localhost:8080/health"
  "http://localhost:8080/"
  "http://localhost:8080/api/v1/status"
)

for url in "${SMOKE_URLS[@]}"; do
  status=$(curl -s -o /dev/null -w "%{http_code}" "$url")
  if [ "$status" -ne 200 ]; then
    echo "SMOKE FAIL: $url returned $status"
    exit 1
  fi
done

echo "Smoke test passed"
```

### Key rule

Failure must be **loud** — exit non-zero so maestro reports it via Telegram.
Silent failures defeat the purpose.

---

## 8. Lessons learned (from panoptikon)

These are hard-won patterns from running maestro against the panoptikon
project:

### Frontend embedded in binary

If the frontend is bundled into the server binary (e.g. Rust's `include_dir!`,
Go's `//go:embed`), the deploy script **must** build frontend before backend:

```bash
cd web && bun install && bun run build && cd ..
cargo build --release
```

Getting this order wrong produces a binary with stale frontend assets.

### Auto-version bump prevents confusion

Without automatic version bumps, multiple merges produce binaries with the
same version string. This makes debugging impossible. Use either:

- A GitHub Actions workflow that bumps the version on merge, or
- Maestro's built-in `versioning` config to bump version files automatically.

### Deploy hook eliminates manual deploys

Setting `deploy_cmd` in the maestro config means every successful merge
triggers a deploy automatically. No more "I merged but forgot to deploy"
situations. Combined with the smoke test, this gives you continuous
deployment with safety checks.

---

## Quick-start checklist

Use this checklist when onboarding a new repo:

- [ ] CI workflows exist and pass: build, lint, e2e
- [ ] Branch protection enabled on `main` with required checks
- [ ] Labels created: `bug`, `enhancement`, `documentation`
- [ ] Exclude labels created: `wontfix`, `question`, `blocked`, `duplicate`, `invalid`
- [ ] `maestro-<project>.yaml` created with all required fields
- [ ] `scripts/deploy.sh` exists, is executable, and is idempotent
- [ ] Deploy script builds frontend before backend (if applicable)
- [ ] Deploy script verifies service is running after restart
- [ ] Post-deploy smoke test checks key endpoints
- [ ] Worker prompt includes E2E test requirements
- [ ] Telegram notifications configured and tested
- [ ] Run `maestro run --config maestro-<project>.yaml` and verify first issue is picked up
