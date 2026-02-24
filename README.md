# maestro

A Go-based agent orchestrator for managing parallel AI coding agents working on GitHub issues.

Replaces the previous `ao` (agent-orchestrator npm package) + shell scripts setup with a proper Go daemon.

## What it does

maestro orchestrates multiple parallel AI coding agents (Claude, Codex, Gemini), each working on a separate GitHub issue in its own git worktree. It:

- Picks open GitHub issues matching a label (e.g. `enhancement`)
- Creates git worktrees for each agent
- Spawns `claude --dangerously-skip-permissions` in each worktree with a task prompt
- Monitors agent progress (process alive? PR created? CI green?)
- Auto-merges PRs when CI passes
- Rebases PRs that have merge conflicts
- Notifies you via Telegram (through OpenClaw gateway) on important events
- Cleans up dead/stale sessions

## Prerequisites

### Required
- `gh` (GitHub CLI) — [cli.github.com](https://cli.github.com)
- `git` — pre-installed on most systems
- **One of the following AI CLIs:**

| CLI | Provider | Install |
|-----|----------|---------|
| `claude` | Anthropic Claude Code | [claude.ai/code](https://claude.ai/code) |
| `codex` | OpenAI Codex | `bun add -g @openai/codex` |
| `gemini` | Google Gemini | `npm i -g @google/gemini-cli` |

You only need one — whichever you have access to.

### Setup
```bash
# Authenticate GitHub CLI
gh auth login

# Authenticate your chosen AI CLI (example for Claude):
claude auth   # or codex auth / gemini auth
```

### Private repositories
Maestro works with private repos — all GitHub operations go through `gh` CLI. As long as `gh auth status` shows access to the repo, maestro works.

## Installation

### Quick install

```bash
curl -fsSL https://raw.githubusercontent.com/BeFeast/maestro/main/install.sh | sh
```

Or download a binary manually from the [latest release](https://github.com/BeFeast/maestro/releases/latest).

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

# 2. Clone your repo and cd into it
git clone https://github.com/yourorg/yourrepo
cd yourrepo

# 3. Run the interactive setup wizard
maestro init
```

The wizard will ask for your repo name, local paths, max parallel workers, and preferred AI backend. It generates a `maestro.yaml` in the current directory.

```bash
# 4. Do a test run (picks one issue, runs once, then exits)
maestro run --once

# 5. Check status
maestro status

# 6. When ready, run continuously
maestro run
```

That's it. Maestro will now pick up issues matching your configured label, spawn AI agents in isolated worktrees, and auto-merge PRs when CI passes.

## Configuration

Create `~/.maestro/config.yaml` or `./maestro.yaml`:

```yaml
repo: BeFeast/panoptikon
local_path: /home/shtrudel/src/panoptikon
worktree_base: /home/shtrudel/.worktrees/panoptikon
max_parallel: 5
session_prefix: pan         # worker session name prefix (default: first 3 chars of repo name)
state_dir: ~/.maestro/pan   # state/log directory (default: ~/.maestro/<repo-hash>)
claude_cmd: claude          # the claude CLI binary
issue_label: enhancement    # label to filter issues
exclude_labels:
  - blocked
telegram:
  target: "79510949"        # Telegram user ID
  openclaw_url: "http://localhost:18789"  # OpenClaw gateway
```

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
2. Auto-merges PRs where CI is green
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
- `running` — Claude agent is working
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
1. A base prompt (from `orchestrator-prompt.md` or `--prompt`)
2. Issue number, title, and body
3. Worktree path and instructions for creating a PR

The agent runs as:
```bash
cd /worktree/path && claude --dangerously-skip-permissions -p "<assembled prompt>"
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
- AI CLI not authenticated — run `claude auth` (or `codex auth` / `gemini auth`)
- AI CLI not found in PATH — verify with `which claude` (or `which codex` / `which gemini`)
- The repo has no open issues matching the configured `issue_label`

### `maestro run` exits with "load config" error

Maestro looks for config in this order:
1. `--config` flag path
2. `maestro.yaml` in the current directory
3. `~/.maestro/config.yaml`

Run `maestro init` in your repo directory to create a config, or pass an explicit path:

```bash
maestro run --config ~/.maestro/maestro-myapp.yaml
```

### Worktree conflicts or stale worktrees

If a worker died and left a stale worktree:

```bash
maestro stop --session <slot>   # cleans up worktree + state
# or manually:
git worktree remove /path/to/worktree --force
```

### systemd service won't start

```bash
# Check logs
journalctl --user -u maestro@myapp -f

# Verify the binary is at /usr/local/bin/maestro
which maestro

# Verify the config file exists
ls ~/.maestro/maestro-myapp.yaml

# Reload after editing the unit file
systemctl --user daemon-reload
systemctl --user restart maestro@myapp
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
- `claude` / `codex` / `gemini` CLI (agent invocation — at least one required)

## Acknowledgments

Inspired by [agent-orchestrator (ao)](https://www.npmjs.com/package/agent-orchestrator) — a great tool for parallelizing AI coding agents across git worktrees. maestro started as a replacement for our ao + shell scripts setup, borrowing the core idea of session-per-issue isolation in worktrees and rewriting it in Go for faster iteration cycles and better process reliability.

## License

[MIT](./LICENSE) — Copyright (c) 2026 Oleg Kossoy
