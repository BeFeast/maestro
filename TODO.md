# TODO — Before maestro can fully replace ao

## Status (2026-02-23)

maestro is configured for panoptikon and the binary builds/runs correctly.
The ao orchestrator is still managing active sessions: pan-14, pan-16, pan-17, pan-18, pan-19.
Maestro state is pre-seeded with `next_slot: 20` to avoid slot collisions with ao.

---

## Remaining Steps

### 1. Wait for ao sessions to finish
The existing ao sessions (#129, #154, #155, #156, #158) need to complete (PR merged or worker died).
Don't run `maestro run --once` while ao workers are still active — both tools would try
to spawn workers for the same open issues, creating duplicates.

### 2. Check: does maestro detect ao-spawned PRs?
Maestro only knows about sessions it started itself (its own state file).
PRs opened by ao workers won't be auto-merged by maestro — they need to be merged manually
or migrated into maestro's state.

**Option A:** Manually merge ao PRs before switching over.
**Option B:** Add a `maestro import` command that reads existing worktrees/PRs and seeds state.

### 3. First dry run (safe test)
Once ao is idle, run:
```bash
cd /home/shtrudel/src/maestro
./maestro run --once 2>&1 | tee /tmp/maestro-dryrun.log
```
Verify: correct issues are picked up, worker prompt is correctly rendered with issue variables.

### 4. Check worker prompt rendering
The template uses `{{ISSUE_NUMBER}}`, `{{ISSUE_TITLE}}`, `{{ISSUE_BODY}}`, `{{BRANCH}}`, `{{WORKTREE}}`, `{{REPO}}`.
After first worker spawned, check the rendered prompt:
```bash
cat ~/.maestro/d581c91d05cc/pan-20-prompt.md
```

### 5. Wire up `pr_open` status transitions
Currently, maestro marks a session as `running` when started.
It needs to detect when a worker opens a PR and transition to `pr_open` status.
Currently this is NOT automatic — maestro only checks PRs that are already `pr_open` in state.

**Fix needed:** In `checkSessions`, detect if a worker process has died AND a PR exists for
its branch → set status to `pr_open` and record the PR number.

### 6. Cron / systemd for continuous operation
Set up `maestro run --interval 10m` as a systemd user unit or cron job:
```bash
# ~/.config/systemd/user/maestro.service
[Service]
ExecStart=/home/shtrudel/src/maestro/maestro run --interval 10m
WorkingDirectory=/home/shtrudel/src/maestro
Restart=on-failure
```

### 7. (Optional) `maestro logs` command
Currently logs are at `~/.maestro/d581c91d05cc/logs/pan-N.log`.
A `maestro logs --session pan-20` shortcut would be nice.

---

## Architecture Note: ao vs maestro coexistence

| Feature              | ao (agent-orchestrator) | maestro |
|----------------------|------------------------|---------|
| State storage        | `~/.agent-orchestrator/` | `~/.maestro/` |
| Session naming       | pan-N (currently at 19) | pan-N (starts at 20) |
| Worktree base        | `~/.worktrees/panoptikon/` | same |
| Auto-merge PRs       | No (manual)            | Yes (when CI green) |
| Prompt template      | `orchestrator-prompt.md` | `worker-prompt-template.md` (with substitution) |
| Rebase on conflict   | No                     | Yes (automatic) |
| Stale detection      | No                     | Yes (>2h warning) |
