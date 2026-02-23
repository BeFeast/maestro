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

## Installation

```bash
git clone https://github.com/BeFeast/maestro
cd maestro
go build ./cmd/maestro/
sudo mv maestro /usr/local/bin/  # or add to PATH
```

Requires Go 1.22+.

## Configuration

Create `~/.maestro/config.yaml` or `./maestro.yaml`:

```yaml
repo: BeFeast/panoptikon
local_path: /home/shtrudel/src/panoptikon
worktree_base: /home/shtrudel/.worktrees/panoptikon
max_parallel: 5
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

### Per-issue routing
Label a GitHub issue with `model:codex` or `model:gemini` to override the default backend for that specific issue:
```
issue #42 labels: enhancement, model:codex  → runs with Codex
issue #43 labels: enhancement               → runs with default (claude)
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
Repo:         BeFeast/panoptikon
State file:   /home/shtrudel/.maestro/d581c91d05cc/state.json
Max parallel: 5

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
