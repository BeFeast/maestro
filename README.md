# maestro

A Go-based agent orchestrator for managing parallel AI coding agents working on GitHub issues.

Replaces the previous `ao` (agent-orchestrator npm package) + shell scripts setup with a proper Go daemon.

## What it does

maestro orchestrates multiple parallel AI coding agents (Claude, Codex, Gemini, Cline), each working on a separate GitHub issue in its own git worktree. It:

- Picks open GitHub issues matching a label (e.g. `enhancement`)
- Creates git worktrees for each agent
- Spawns the configured backend CLI (e.g. `claude`, `codex`, `gemini`, or `cline`) in each worktree with a task prompt
- Monitors agent progress (process alive? PR created? CI green?)
- Auto-merges PRs when CI passes
- Rebases PRs that have merge conflicts
- Notifies you via Telegram (through OpenClaw gateway) on important events
- Cleans up dead/stale sessions

## Mission Control

Mission Control (MC) is the fleet web dashboard maestro serves at `http://127.0.0.1:8786`. It's how an operator watches every project, worker, and approval at a glance.

![Fleet overview](docs/images/mc/overview.png)

*Fleet overview (`/`) — the operator brief: hero next-action, fleet KPIs, the per-project health grid, recent workers, and cost/usage.*

![Approvals](docs/images/mc/approvals.png)

*Approvals (`/approvals`) — the cautious-gate write-path: pending approvals to approve/reject, with the full audit history below.*

![Project view](docs/images/mc/project.png)

*Project view (`/project/<name>`) — single-project drill-down: attention/next-action, live workers, health, project board, and recent supervisor decisions.*

![Workers](docs/images/mc/workers.png)

*Workers (`/workers`) — every worker across the fleet with status, branch/PR, and next step.*

> Screenshots are captured from a live MC instance with [`scripts/capture-mc-screenshots.mjs`](scripts/capture-mc-screenshots.mjs). To refresh them, run `bun scripts/capture-mc-screenshots.mjs` on the host serving MC (override the target with `MC_BASE_URL`).

## Prerequisites

### Required
- **`git`** — pre-installed on most systems
- **`gh`** (GitHub CLI) — [cli.github.com](https://cli.github.com)
- **`tmux`** — required for worker session management
- **One of the following AI CLIs:**

| CLI | Provider | Install |
|-----|----------|---------|
| `claude` | Anthropic Claude Code | [claude.ai/code](https://claude.ai/code) |
| `codex` | OpenAI Codex | `bun add -g @openai/codex` |
| `gemini` | Google Gemini | `npm i -g @google/gemini-cli` |
| `cline` | Cline (OpenAI-compatible providers) | `bun add -g cline` |

You only need one — whichever you have access to.

### Optional for Go repositories
- **`gopls`** — enables symbol-aware pre-worker research context for Go modules (`go install golang.org/x/tools/gopls@latest`)

### Verify prerequisites
```bash
git --version        # any recent version
gh --version         # 2.x+
tmux -V              # any recent version
claude --version     # or: codex --version / gemini --version / cline --version
gopls version        # optional; used for Go symbol context when available
```

### Setup
```bash
# Authenticate GitHub CLI
gh auth login

# Verify access to your target repo
gh auth status

# Authenticate/configure your chosen AI CLI (example for Claude):
claude auth   # or codex auth / gemini auth
# For Cline: configure provider + model in ~/.cline/data/globalState.json and secrets.json
```

### Private repositories
Maestro works with private repos — all GitHub operations go through `gh` CLI. As long as `gh auth status` shows access to the repo, maestro works.

## Installation

### Quick install

```bash
curl -fsSL https://raw.githubusercontent.com/BeFeast/maestro/main/install.sh | sh
```

`install.sh` downloads the latest release tarball for your OS/arch (`maestro-<os>-<arch>.tar.gz`), extracts the binary, and installs it to `/usr/local/bin/maestro` (uses `sudo` only if needed).

To install somewhere else:

```bash
INSTALL_DIR="$HOME/.local/bin" curl -fsSL https://raw.githubusercontent.com/BeFeast/maestro/main/install.sh | sh
```

### Build from source

Requires Go 1.22+.

```bash
git clone https://github.com/BeFeast/maestro
cd maestro
VERSION="$(sed -nE 's/^version[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' VERSION)"
test -n "$VERSION"
go build -trimpath -ldflags "-X main.version=${VERSION}" ./cmd/maestro/
sudo mv maestro /usr/local/bin/  # or add to PATH
```

## Quickstart

Get maestro running in under 5 minutes:

```bash
# 1. Install maestro
curl -fsSL https://raw.githubusercontent.com/BeFeast/maestro/main/install.sh | sh
maestro version   # verify installation

# 2. Clone your target repo (if not already)
gh repo clone owner/myrepo ~/src/myrepo

# 3. Run the interactive setup wizard
cd ~/src/myrepo
maestro init

# 4. Add maestro.yaml to .gitignore (it contains local paths)
echo "maestro.yaml" >> .gitignore
```

The `maestro init` wizard will ask you for:
- **GitHub repo** (owner/repo format)
- **Local clone path** (where the repo lives on disk)
- **Worktree base dir** (where worker worktrees are created)
- **Max parallel workers** (how many agents run simultaneously)
- **Default model backend** (claude, codex, gemini, or cline)
- **Issue label filter** (which issues to pick up, e.g. `enhancement`)
- **Telegram notifications** (optional)

It generates a `maestro.yaml` config file and a systemd/launchd service file.

```bash
# 4. Do a test run (picks one issue, runs once, then exits)
maestro run --once

# 5. Check status
maestro status

# 6. Watch the Mission Control dashboard (trusted-LAN default: write-enabled)
#    Add --read-only=true (or set server.read_only: true in YAML) for installs
#    exposed beyond a trusted LAN; #616 covers optional HTTP auth.
maestro serve --port 8787

# 7. When ready, run continuously
maestro run
```

That's it. Maestro will now pick up issues matching your configured label, spawn AI agents in isolated worktrees, and auto-merge PRs when CI passes.

To manually spawn a worker for a specific issue:
```bash
maestro spawn --issue 42
```

To watch Maestro from a browser, use the Mission Control dashboard:
```bash
maestro serve --config ./maestro.yaml --host 127.0.0.1 --port 8787
```

The dashboard now boots **write-enabled by default** (trusted-LAN posture, #477). The cautious approval gate still guards the four mutating verbs — `merge_pr`, `close_issue`, `delete_worktree`, and `change_global_config` — so even a writable HTTP caller cannot bypass operator approval. For installs exposed beyond a trusted LAN, run with `--read-only=true` (or set `server.read_only: true` in YAML) and configure the optional HTTP auth layer (#616, off by default).

For multi-project Fleet Mission Control operations, see [`docs/fleet-mission-control-runbook.md`](docs/fleet-mission-control-runbook.md).

### Dashboard auth posture: trusted LAN vs. exposed

Maestro's dashboard auth is opt-in and disabled by default. The right posture depends on where the port is reachable from:

- **Trusted LAN (default).** When the dashboard is bound to `127.0.0.1` or a network only operators can reach, leave `server.auth` empty. The cautious approval gate still protects `merge_pr`, `close_issue`, `delete_worktree`, and `change_global_config` — flipping `--read-only=false` on a trusted LAN does not expose the four destructive verbs.
- **Exposed / shared network.** When the dashboard is reachable from anywhere outside the trusted LAN — `--host 0.0.0.0`, behind a reverse proxy, on a multi-tenant host — set `server.auth.token_env`. With auth enabled, **every** endpoint (read GETs, write POSTs, and the SPA HTML) rejects unauthenticated requests with `401`; the cautious approval gate still fires for authenticated callers as defense in depth.

The token is loaded at runtime from an environment variable populated by your secret manager (Infisical, 1Password CLI, etc.) — never hardcoded in YAML:

```yaml
server:
  host: 0.0.0.0
  port: 8788
  read_only: false
  auth:
    token_env: MAESTRO_DASHBOARD_TOKEN   # env var name; populate from your secret manager
    actor_name: dashboard-operator       # optional; audit actor recorded for authed requests
```

When `auth.token_env` resolves to a non-empty value, every request — `GET /api/v1/...`, `POST /api/v1/...` (`/actions`, `/approvals/{id}/{approve|reject}`, `/audit/log`, `/refresh`), and the dashboard HTML / static assets — requires a credential and returns `401` otherwise. Two authentication schemes are accepted (the server advertises both in the `WWW-Authenticate` challenge):

- **HTTP Basic** — any username, password equal to the token. Browsers prompt natively, then cache the credential for the realm so subsequent SPA fetches authenticate automatically.
- **Bearer token** — `Authorization: Bearer <token>`. Use this for `curl`, scripts, and clients driven directly by a secret manager.

The authenticated identity replaces any `actor` field in the request body — operators cannot impersonate one another even if they share the token.

To watch workers live in a tmux dashboard:
```bash
maestro watch
```

## Configuration

Create `~/.maestro/config.yaml` or `./maestro.yaml`:

```yaml
repo: OWNER/REPO
local_path: /path/to/local/clone
worktree_base: /path/to/worktrees/repo
max_parallel: 5
max_runtime_minutes: 120           # hard timeout per worker (default: 120)
worker_silent_timeout_minutes: 0   # kill worker if tmux output is unchanged for N minutes (0 = disabled)
worker_max_tokens: 0               # kill worker when cumulative token usage exceeds this (0 = unlimited)
auto_rebase: true                  # auto-rebase conflicting PR branches (default: true)
merge_strategy: sequential         # "sequential" (default) or "parallel"
merge_interval_seconds: 30         # minimum seconds between merges in sequential mode
review_gate: greptile              # "greptile" (default) or "none"
review_retrigger:                  # self-heal greptile webhook misses (#691)
  enabled: true                    # post "@greptile review" when the gate wedges at pending (default: true)
  pending_minutes: 10              # re-trigger after this long pending on one head SHA (default: 10)
  cooldown_minutes: 30             # minimum gap between re-trigger comments (default: 30)
auto_retry_review_feedback: false  # retry PRs with actionable review comments
auto_retry_rebase_conflicts: false # retry PRs whose auto-rebase fails with conflicts
session_prefix: prj                # worker session name prefix (default: first 3 chars of repo name)
state_dir: ~/.maestro/<project>    # state/log directory (default: ~/.maestro/<repo-hash>)
claude_cmd: claude                 # deprecated: use model.backends.claude.cmd
server:
  host: 127.0.0.1                  # bind address for `maestro serve`
  port: 8787                       # 0 = disabled for `maestro run`
  read_only: false                 # default: trusted-LAN, writes enabled; flip true for exposed installs (#477)
issue_labels:                      # preferred label filter (OR semantics)
  - enhancement
exclude_labels:
  - blocked
telegram:
  target: "YOUR_TELEGRAM_CHAT_ID" # Telegram user ID
  openclaw_url: "http://localhost:18789"  # OpenClaw gateway
```

`issue_label` is still supported for backward compatibility, but `issue_labels` is recommended for new configs.

## AI Backends

Maestro supports multiple AI coding agents. Configure via `model:` in `maestro.yaml`:

```yaml
model:
  default: claude        # which backend to use by default
  backends:
    claude:
      cmd: claude        # Anthropic Claude Code CLI
    codex:
      cmd: codex         # OpenAI Codex CLI
    gemini:
      cmd: gemini        # Google Gemini CLI
    cline:
      cmd: cline         # Cline CLI (e.g. SAP AI Core / any OpenAI-compatible provider)
```

### Supported backends

> [!NOTE]
> **Claude** (default) — Anthropic Claude Code CLI
> Install: https://claude.ai/code | `claude --version`

> [!NOTE]
> **OpenAI Codex** — OpenAI Codex CLI
> Install: `npm install -g @openai/codex` or `bun add -g @openai/codex`
> Auth: `codex auth` or set `OPENAI_API_KEY`

> [!NOTE]
> **Gemini** — Google Gemini CLI
> Install: `npm install -g @google/gemini-cli`
> Auth: `gemini auth` or set `GEMINI_API_KEY`

> [!NOTE]
> **Cline** — Cline CLI, supports any OpenAI-compatible provider (including SAP AI Core, Azure OpenAI, etc.)
> Install: `bun add -g cline` | `cline --version`
> Config: `~/.cline/data/globalState.json` + `secrets.json` — configure provider and model before use.
> Headless mode: `cline -y "task"` — auto-approves all actions and exits when done.
> SAP AI Core example: set provider to `sapaicore`, model to `anthropic--claude-4.5-opus`.

### Custom-named backends

Backends do not have to be named after the CLI they run. A custom key (e.g. a model nickname) keeps full CLI-specific behaviour — permission-bypass flags and stdin prompt delivery — as long as Maestro can tell which CLI it wraps. Resolution order:

1. the backend name itself (`claude`, `codex`, `gemini`, `cline`),
2. the `provider` field (`anthropic`/`claude` → Claude, `openai`/`codex` → Codex, `google`/`gemini` → Gemini, `cline` → Cline),
3. the binary basename of `cmd` (`claude`, `codex`, `gemini`, `cline`).

```yaml
model:
  default: fable
  backends:
    fable:
      provider: anthropic    # → claude exec path (--dangerously-skip-permissions, -p, prompt via stdin)
      cmd: claude --model claude-fable-5 --effort xhigh
    fast:
      provider: openai       # → codex exec path (exec, bypass flag, prompt via stdin)
      cmd: codex --profile fast
```

A backend that matches none of the above falls back to the generic exec path: `prompt_mode` applies, no permission-bypass flag is added, and Maestro logs a startup warning naming the backend. Use the generic path only for genuinely custom CLIs.

### Claude / Codex usage capture (tokens + cost)

Plain `claude -p` text mode prints no parseable token total, and `codex exec` text mode only prints a fuzzy single total — so a worker's `tokens_used_total` and USD cost are unreliable or `0`. Opt a `claude` or `codex` backend into structured usage capture with `usage_stream: true`:

```yaml
model:
  default: claude
  backends:
    claude:
      cmd: claude
      usage_stream: true   # run claude in --output-format stream-json; capture tokens + cost
    codex:
      cmd: codex
      usage_stream: true   # run codex exec --json; capture split tokens (cost = virtual)
      pricing:
        input_usd_per_mtok: 1.25
        output_usd_per_mtok: 10
```

When enabled, the worker runs the backend in structured-stream mode (`claude --output-format stream-json --verbose`, or `codex exec --json`) and its NDJSON is piped through `maestro stream-split`, which writes the raw frames to a side-channel `<slot>.jsonl` (parsed for `input`/`output`/cache tokens) while keeping `<slot>.log` human-readable. The session then reports non-zero split tokens in `maestro history --json` and the `/api/v1/fleet` cost panel. Off by default; an operator-pinned `--output-format` (claude) or `--json` (codex) in `extra_args` overrides it. Claude reports its own `total_cost_usd`; codex does not, so its cost is **virtual** — computed from the configured `pricing` block (tokens-only `$0` when no rates are set). (The `pi` backend captures usage natively and needs no opt-in.)

### Optional worker MCP tools

Worker sessions receive no MCP tools by default. Attach project-specific MCP servers per backend with `model.backends.<name>.mcp`:

```yaml
model:
  default: codex
  backends:
    codex:
      cmd: codex
      mcp:
        servers:
          docs:
            command: npx
            args: ["-y", "@example/docs-mcp"]
          symbols:
            url: https://mcp.example.com/mcp
            bearer_token_env_var: SYMBOLS_MCP_TOKEN
    claude:
      cmd: claude
      mcp:
        strict: true
        configs:
          - /path/to/project-mcp.json
```

For Codex workers, Maestro passes configured servers as `-c mcp_servers...` overrides. For Claude workers, Maestro passes `--mcp-config` and `--strict-mcp-config` when requested. Keep tokens in environment variables and reference their names from config.

### Per-issue routing
Label a GitHub issue with `model:codex`, `model:gemini`, or `model:cline` to override the default backend for that specific issue:
```
issue #42 labels: enhancement, model:codex  → runs with Codex
issue #43 labels: enhancement, model:cline  → runs with Cline
issue #44 labels: enhancement               → runs with default (claude)
```

## Commands

### `maestro run`

Runs the orchestration loop. Every interval:
1. Checks running sessions (kill dead, clean stale)
2. Auto-merges PRs where CI is green (sequential by default, configurable via `merge_strategy`). A green draft PR is automatically marked ready for review first (`gh pr ready`), unless its title/body carries an explicit WIP/Partial marker (`[WIP]` / `[Partial]` in the title, or `maestro:partial` / `maestro:wip` in the body) — those drafts are deliberate and stay untouched.
3. Rebases PRs with conflicts
4. Picks new issues to work on (up to `max_parallel - active`)
5. Starts new workers for picked issues

```bash
maestro run                   # runs forever, 10m interval
maestro run --once            # run once and exit (dry run / cron mode)
maestro run --interval 5m     # custom interval
maestro run --prompt /path/to/worker-prompt.md  # custom prompt base
```

### `maestro status`

Shows current state as a formatted table.

```bash
maestro status          # pretty table
maestro status --json   # JSON output
```

Example output:
```
Repo:           OWNER/REPO
Session prefix: prj
State file:     ~/.maestro/<repo-hash>/state.json
Max parallel:   5

SESSION  ISSUE  STATUS   PID    ALIVE  AGE    TITLE
-------  -----  ------   ---    -----  ---    -----
prj-1    #154   running  12345  yes    23m    Add asset inventory endpoint
prj-2    #155   pr_open  12346  no     1h5m   Fix auth refresh
```

### `maestro spawn`

Manually spawn a worker for a specific issue.

```bash
maestro spawn --issue 154
maestro spawn --issue 154 --prompt /path/to/custom-prompt.md
```

### `maestro stop`

Stop a specific session and remove its worktree.

```bash
maestro stop --session pan-1
```

### `maestro pause` / `maestro resume`

First-class pause/resume for a project's execution — no systemd unit or config
file surgery needed.

```bash
maestro pause --config /path/to/project.yaml    # stop selecting/spawning new issues
maestro resume --config /path/to/project.yaml   # restore issue selection next cycle
```

While paused:

- The orchestrator skips issue selection entirely and spawns zero new workers,
  even when `maestro-ready` issues exist.
- An in-flight worker is **not** killed — it runs to completion and lands its
  PR normally (drain semantics are unchanged).
- The flag is persisted in the project state dir and survives a
  `systemctl --user restart` of the maestro service; only `maestro resume`
  clears it.
- The supervisor stays alive and reports the paused state instead of treating
  the idle project as a stall, and `GET /api/v1/fleet` exposes a per-project
  `paused` flag (Mission Control shows a paused badge).

## State

State is stored in `~/.maestro/<repo-hash>/state.json`:

```json
{
  "sessions": {
    "prj-1": {
      "issue_number": 154,
      "issue_title": "Add asset inventory endpoint",
      "worktree": "/path/to/worktrees/repo/prj-1",
      "branch": "feat/prj-1-154-add-asset-inventory-endpoint",
      "pid": 12345,
      "log_file": "~/.maestro/<project>/logs/prj-1.log",
      "started_at": "2026-02-23T00:00:00Z",
      "status": "running"
    }
  },
  "next_slot": 2
}
```

Session statuses:
- `running` — AI agent is working
- `pr_open` — PR created, waiting for CI / review
- `done` — PR merged and worktree cleaned up
- `failed` — Something went wrong
- `conflict_failed` — Rebase failed, needs manual intervention
- `dead` — Process died unexpectedly

State writes are atomic (temp file + rename).

## Logs

Each worker's output goes to `~/.maestro/<repo-hash>/logs/<session>.log`.

## Notifications

maestro sends Telegram notifications via the OpenClaw gateway API at `http://localhost:18789/api/v1/message`:

- 🚀 Worker started for issue
- ✅ PR merged successfully
- ❌ CI failing / merge failed / rebase failed / worker died
- ⏰ Worker running > 2h (might be stuck)
- 🔄 Rebase succeeded

## Worker Prompt

The worker prompt is assembled from:
1. A base prompt (from `worker_prompt` config or `--prompt`)
2. Issue number, title, and body
3. Worktree path and instructions for creating a PR

The exact command depends on the selected backend. Examples:

```bash
# Claude
cd /worktree/path && claude --dangerously-skip-permissions -p < /path/to/prompt.txt

# Codex
cd /worktree/path && codex exec --dangerously-bypass-approvals-and-sandbox -C /worktree/path - < /path/to/prompt.txt

# Cline
cd /worktree/path && cline -y "<assembled prompt>"
```

## Cron Mode

For automatic operation, run on a cron schedule:

```bash
# ~/.config/cron/maestro.cron
*/10 * * * * /usr/local/bin/maestro run --config ~/.maestro/maestro-<project>.yaml --once >> ~/.maestro/maestro-<project>.log 2>&1
```

Or run as a daemon:
```bash
maestro run --interval 10m
```

## Multi-Project Setup

To run maestro for multiple projects simultaneously, use `session_prefix` and `state_dir` to keep workers and state isolated:

```yaml
# ~/.maestro/maestro-panoptikon.yaml
repo: BeFeast/panoptikon
session_prefix: pan           # workers: pan-1, pan-2, ...
state_dir: ~/.maestro/pan
worktree_base: ~/.worktrees/panoptikon
max_parallel: 5
```

```yaml
# ~/.maestro/maestro-myapp.yaml
repo: BeFeast/myapp
session_prefix: app           # workers: app-1, app-2, ...
state_dir: ~/.maestro/app
worktree_base: ~/.worktrees/myapp
max_parallel: 3
```

### Running as a Service

#### Single project (Linux — systemd)

`maestro init` automatically creates a systemd user service at `~/.config/systemd/user/maestro.service`. To enable it:

```bash
systemctl --user daemon-reload
systemctl --user enable --now maestro.service

# Check status
systemctl --user status maestro.service
journalctl --user -u maestro.service -f
```

> **Note:** User services require `loginctl enable-linger $USER` to keep running when you're not logged in.

#### Single project (macOS — launchd)

`maestro init` creates a launchd plist at `~/Library/LaunchAgents/com.maestro.agent.plist`:

```bash
launchctl load ~/Library/LaunchAgents/com.maestro.agent.plist
```

#### Multiple projects (single-service daemon)

For a fleet, run **one** `maestro.service` that drives every project. `maestro
daemon` runs an orchestrator + supervisor loop per project in a single process
plus one aggregating fleet dashboard on `:8786`. This replaces the old
per-project `maestro@.service` template (and the separate supervise/serve units)
— one unit instead of ~15.

Projects live in a SQLite config store, not per-project runtime YAML files. Seed
it once from existing `~/.maestro/maestro.d/*.yaml`; after that, treat those
YAML files as import/export artifacts, not as the operator source of truth:

```bash
maestro config-store migrate --db ~/.maestro/maestro.db --dir ~/.maestro/maestro.d
```

Then install and start the daemon unit (a `maestro.service` for the daemon ships
in the repo root):

```bash
cp maestro.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now maestro.service

# Check status — list-units shows ONE active maestro unit
systemctl --user list-units 'maestro*'
journalctl --user -u maestro.service -f
```

Day-to-day project inspection should target the store row the daemon is running,
not a seed YAML file:

```bash
maestro config-store list --db ~/.maestro/maestro.db
maestro status --config-store ~/.maestro/maestro.db --config-store-project <project-name>
```

The whole cutover (seed store → stop legacy units → start the daemon → verify
`:8786/api/v1/fleet`) is automated by `scripts/migrate-to-daemon.sh`:

```bash
scripts/migrate-to-daemon.sh            # prompts before changing a running host
scripts/migrate-to-daemon.sh --dry-run  # show the actions without applying them
```

The cutover is non-concurrent per project (it stops the legacy units before
starting the daemon, so no project is ever driven by both). **Rollback** is
supported because the legacy unit files stay on disk:

```bash
systemctl --user disable --now maestro.service
# regenerate portable YAML from the store only when you need a backup/export:
maestro config-store export --db ~/.maestro/maestro.db --dir ~/.maestro/maestro.d
systemctl --user enable --now maestro@panoptikon maestro@myapp   # re-enable old units
```

**Adding / removing projects at runtime.** With the daemon you no longer create a
unit per project. Edit the store and the daemon hot-reconciles (no restart):

```bash
maestro config-store add --db ~/.maestro/maestro.db --file ./new-project.yaml
maestro config-store rm  --db ~/.maestro/maestro.db <project>
```

The fleet dashboard exposes the same as authenticated, audited endpoints —
`POST /api/v1/fleet/projects` (body `{"config_yaml": "..."}`) and
`DELETE /api/v1/fleet/projects` (body `{"name": "..."}`) — so you can add a
project from the UI. On `SIGTERM` (`systemctl --user stop`) the daemon drains
gracefully in-process: it stops claiming new issues and waits for in-flight
workers to finish before exiting (bounded by `--drain-timeout`, default 25m,
inside the unit's `TimeoutStopSec=30min`).

## Mission Control bundle

The Mission Control SPA lives in `internal/server/web/mc/` and ships as a pre-built bundle committed under `internal/server/web/static/mc/`. CI rebuilds the bundle on every PR and fails the `frontend-build` job if the committed output drifts from a fresh build.

### Toolchain

The frontend toolchain is pinned via the `packageManager` field in `internal/server/web/mc/package.json` and the matching `bun-version` in `.github/workflows/ci.yml`. Use the same bun version locally — Corepack will respect `packageManager` automatically, or install it explicitly:

```bash
bun --version   # must match the pinned version in package.json
```

When bumping bun, update both `packageManager` in `package.json` and `bun-version` in `.github/workflows/ci.yml` in the same PR so they cannot drift.

### Bundle-rebuild SOP (staleness gate failures)

When CI fails with `Committed MC bundle is stale`, the fix is always to rebuild and commit — never `gh pr merge --admin`. The canonical recipe:

```bash
cd internal/server/web/mc
bun install --frozen-lockfile
bun run build
cd -
git add internal/server/web/static/mc/
git commit -m "chore: rebuild MC bundle"
```

If the diff still appears after a clean rebuild, your local bun/vite versions disagree with CI. Verify `bun --version` matches the pin in `package.json`. Bypassing the gate with `--admin` is the failure mode this guard exists to prevent — the bundle that ships from `static/mc/` is what users load, so a stale commit ships stale UI.

## Troubleshooting

### `gh auth status` fails or maestro can't access the repo

```bash
gh auth login          # re-authenticate
gh auth status         # verify token has repo access
```

For private repos, ensure your token includes the `repo` scope.

### Workers start but immediately die

Check the worker log for errors:

```bash
maestro logs <slot>    # e.g. maestro logs pan-1
```

Common causes:
- AI CLI not authenticated/configured — run `claude auth` (or `codex auth` / `gemini auth`); for Cline, configure provider credentials in `~/.cline/data/globalState.json` + `secrets.json`
- AI CLI not found in PATH — verify with `which claude` / `which codex` / `which gemini` / `which cline`, or use an absolute path in config: `cmd: /usr/local/bin/claude`
- Git worktree creation failed (ensure the local repo clone is clean)

### `maestro run` exits with "load config" error

Maestro looks for config in this order:
1. `--config` flag path
2. `maestro.yaml` in the current directory
3. `~/.maestro/config.yaml`

Run `maestro init` in your repo directory to create a config, or pass an explicit path:

```bash
maestro run --config ~/.maestro/maestro-myapp.yaml
```

### `maestro run` picks no issues

- Verify your `issue_labels` config (or deprecated `issue_label`) matches existing issue labels on GitHub
- Check that issues aren't already assigned or have `exclude_labels`
- Run `gh api 'repos/OWNER/REPO/issues?state=open&labels=enhancement' --jq '.[].number'` to confirm matching issues exist

### tmux errors

maestro requires tmux to manage worker sessions. Install it:

```bash
# Ubuntu/Debian
sudo apt install tmux

# macOS
brew install tmux
```

### Worktree conflicts or stale worktrees

If a worker died and left a stale worktree:

```bash
maestro stop --session <slot>   # cleans up worktree + state
# or force-kill:
maestro kill <slot>

# Manual cleanup if needed:
git -C /path/to/repo worktree remove /path/to/worktree --force
```

### systemd service won't start

```bash
# Check logs
journalctl --user -u maestro.service -f

# Verify the binary is at /usr/local/bin/maestro
which maestro

# Verify the config file exists
ls ~/.maestro/maestro-myapp.yaml

# Reload after editing the unit file
systemctl --user daemon-reload
systemctl --user restart maestro.service

# Ensure linger is enabled (required for services when not logged in)
loginctl enable-linger $USER
```

### Workers stuck for hours

Maestro sends a Telegram notification when a worker runs longer than 2 hours. You can manually kill and retry:

```bash
maestro kill <slot>              # kills the stuck worker
maestro spawn --issue <number>   # retry the issue
```

## Dependencies

- `github.com/befeast/maestro` (this module)
- `gopkg.in/yaml.v3` (config parsing)
- `gh` CLI (GitHub operations)
- `git` (worktree management)
- `claude` / `codex` / `gemini` / `cline` CLI (agent invocation — at least one required)

## Acknowledgments

Inspired by [agent-orchestrator (ao)](https://www.npmjs.com/package/agent-orchestrator) — a great tool for parallelizing AI coding agents across git worktrees. maestro started as a replacement for our ao + shell scripts setup, borrowing the core idea of session-per-issue isolation in worktrees and rewriting it in Go for faster iteration cycles and better process reliability.

## License

[MIT](./LICENSE) — Copyright (c) 2026 Oleg Kossoy

- Free-agentic test pass: opencode produced this commit.
