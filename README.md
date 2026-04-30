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

### Verify prerequisites
```bash
git --version        # any recent version
gh --version         # 2.x+
tmux -V              # any recent version
claude --version     # or: codex --version / gemini --version / cline --version
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
go build ./cmd/maestro/
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

# 6. Watch the read-only web dashboard
maestro serve --port 8787 --read-only

# 7. When ready, run continuously
maestro run
```

That's it. Maestro will now pick up issues matching your configured label, spawn AI agents in isolated worktrees, and auto-merge PRs when CI passes.

To manually spawn a worker for a specific issue:
```bash
maestro spawn --issue 42
```

To watch Maestro from a browser, use the read-only Mission Control dashboard:
```bash
maestro serve --config ./maestro.yaml --host 127.0.0.1 --port 8787 --read-only
```

Use `--host 0.0.0.0` only on a trusted network if you want to expose the dashboard to the LAN.

To watch workers live in a tmux dashboard:
```bash
maestro watch
```

## Configuration

Create `~/.maestro/config.yaml` or `./maestro.yaml`:

```yaml
repo: BeFeast/panoptikon
local_path: /home/shtrudel/src/panoptikon
worktree_base: /home/shtrudel/.worktrees/panoptikon
max_parallel: 5
max_runtime_minutes: 120           # hard timeout per worker (default: 120)
worker_silent_timeout_minutes: 0   # kill worker if tmux output is unchanged for N minutes (0 = disabled)
worker_max_tokens: 0               # kill worker when cumulative token usage exceeds this (0 = unlimited)
auto_rebase: true                  # auto-rebase conflicting PR branches (default: true)
merge_strategy: sequential         # "sequential" (default) or "parallel"
merge_interval_seconds: 30         # minimum seconds between merges in sequential mode
review_gate: greptile              # "greptile" (default) or "none"
auto_retry_review_feedback: false  # retry PRs with actionable review comments
auto_retry_rebase_conflicts: false # retry PRs whose auto-rebase fails with conflicts
session_prefix: pan                # worker session name prefix (default: first 3 chars of repo name)
state_dir: ~/.maestro/pan          # state/log directory (default: ~/.maestro/<repo-hash>)
claude_cmd: claude                 # deprecated: use model.backends.claude.cmd
server:
  host: 127.0.0.1                  # bind address for `maestro serve`
  port: 8787                       # 0 = disabled for `maestro run`
  read_only: true                  # dashboard mode: block mutating HTTP endpoints
issue_labels:                      # preferred label filter (OR semantics)
  - enhancement
exclude_labels:
  - blocked
telegram:
  target: "79510949"              # Telegram user ID
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
2. Auto-merges PRs where CI is green (sequential by default, configurable via `merge_strategy`)
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
Repo:           BeFeast/panoptikon
Session prefix: pan
State file:     /home/shtrudel/.maestro/d581c91d05cc/state.json
Max parallel:   5

SESSION  ISSUE  STATUS   PID    ALIVE  AGE    TITLE
-------  -----  ------   ---    -----  ---    -----
pan-1    #154   running  12345  yes    23m    Add asset inventory endpoint
pan-2    #155   pr_open  12346  no     1h5m   Fix auth token refresh
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

## State

State is stored in `~/.maestro/<repo-hash>/state.json`:

```json
{
  "sessions": {
    "pan-1": {
      "issue_number": 154,
      "issue_title": "Add asset inventory endpoint",
      "worktree": "/home/shtrudel/.worktrees/panoptikon/pan-1",
      "branch": "feat/pan-1-154-add-asset-inventory-endpoint",
      "pid": 12345,
      "log_file": "/home/shtrudel/.maestro/.../logs/pan-1.log",
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
cd /worktree/path && claude --dangerously-skip-permissions -p "<assembled prompt>"

# Codex
cd /worktree/path && codex exec --dangerously-bypass-approvals-and-sandbox -C /worktree/path - < /path/to/prompt.txt

# Cline
cd /worktree/path && cline -y "<assembled prompt>"
```

## Cron Mode

For automatic operation, run on a cron schedule:

```bash
# ~/.config/cron/maestro.cron
*/10 * * * * cd /home/shtrudel/src/panoptikon && /usr/local/bin/maestro run --once >> ~/.maestro/maestro.log 2>&1
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

#### Multiple projects (systemd template)

A `maestro@.service` template is included for running multiple instances as user services:

```bash
# Install the template
cp maestro@.service ~/.config/systemd/user/
systemctl --user daemon-reload

# Start per-project instances (uses ~/.maestro/maestro-<name>.yaml)
systemctl --user start maestro@panoptikon
systemctl --user start maestro@myapp

# Enable on boot
systemctl --user enable maestro@panoptikon

# Check status
systemctl --user status maestro@panoptikon
journalctl --user -u maestro@panoptikon -f
```

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
- Run `gh issue list --label enhancement` to confirm matching issues exist

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
