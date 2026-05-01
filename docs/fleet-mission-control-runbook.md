# Fleet Mission Control Operating Runbook

Use Fleet Mission Control as the primary operations surface for Maestro-managed repositories. The fleet dashboard shows one read-only view across project configs, runner state, supervisor decisions, approvals, stuck states, and active workers.

This runbook is intentionally safe for shared docs. It uses placeholders for local paths and never requires printing tokens, environment variables, raw config dumps, or full worker logs.

## Workshop Services

Reserve these services and ports on the workshop host:

| Service | Default bind | Port | Purpose | Notes |
|---|---:|---:|---|---|
| Fleet Mission Control | `127.0.0.1` | `8787` | Primary dashboard and `/api/v1/fleet` aggregate API | Start with `maestro serve --fleet ~/.maestro/fleet.yaml --host 127.0.0.1 --port 8787 --read-only` |
| Project Mission Control | `127.0.0.1` | `8788+` | Optional per-project dashboard and `/api/v1/state` API | Configure each project with a unique `server.port`; link it from `dashboard_url` in the fleet file |
| Project runner | none by default | none | Runs `maestro run --config ...` and owns workers, worktrees, PR handling, and merge/deploy loops | It only serves HTTP when that project config has `server.port` set |
| Supervisor loop | none | none | Runs `maestro supervise --config ...` to record decisions, safe queue label mutations, and approval requests | Can be manual, timer-driven, or a user service |
| Worker sessions | none | none | tmux sessions and log files created under each project's `state_dir` | Inspect through Mission Control, `maestro status`, or `maestro logs` |
| OpenClaw relay | `127.0.0.1` | `18789` | Optional Telegram relay endpoint when `telegram.mode: openclaw` is used | Not required for Mission Control |

Keep Fleet Mission Control read-only until there is an explicit auth and audit model for mutating controls. Bind `0.0.0.0` only behind a trusted firewall or reverse proxy.

## Config Boundaries

Project config and fleet config have different jobs.

| File | Loaded by | Owns | Does not own |
|---|---|---|---|
| `~/.maestro/maestro-<project>.yaml` | `maestro run`, `maestro supervise`, `maestro status`, `maestro logs`, single-project `maestro serve` | Repo, clone path, worktree path, state directory, session prefix, labels, supervisor policy, review gate, merge/deploy policy, optional project dashboard port | Other projects |
| `~/.maestro/fleet.yaml` | `maestro serve --fleet` | Project display names, project config paths, optional dashboard links | Queue policy, labels, state directories, review gates, merge behavior |
| `.maestro/supervisor.yaml`, `.maestro/supervisor.yml`, or `.maestro/supervisor.md` | Loaded beside the project config or inside `local_path/.maestro` | Supervisor policy when the team wants policy beside the repo | Fleet membership |

Fleet config paths may be absolute, `~/...`, or relative to the fleet YAML file. A fleet file should not duplicate project settings. If a project needs a different label, review gate, state directory, runner interval, or dashboard port, change that project's config and restart that project's runner.

Minimal project config shape:

```yaml
repo: OWNER/REPO
local_path: /path/to/repos/<project>
worktree_base: /path/to/worktrees/<project>
state_dir: ~/.maestro/<project>
session_prefix: prj

issue_labels:
  - maestro-ready
exclude_labels:
  - blocked

review_gate: greptile

server:
  host: 127.0.0.1
  port: 8788
  read_only: true

supervisor:
  enabled: true
  mode: cautious
  ready_label: maestro-ready
  blocked_label: blocked
  dynamic_wave:
    enabled: true
    owns_ready_label: true
    runnable_project_statuses:
      - Todo
      - To Do
      - Ready
      - Backlog
      - New
  safe_actions:
    - add_ready_label
    - remove_ready_label
    - remove_blocked_label
    - add_issue_comment
  approval_required:
    - merge_pr
    - close_issue
    - delete_worktree
    - change_global_config
```

Minimal fleet config shape:

```yaml
projects:
  - name: project-a
    config: maestro-project-a.yaml
    dashboard_url: http://127.0.0.1:8788
  - name: project-b
    config: maestro-project-b.yaml
    dashboard_url: http://127.0.0.1:8789
```

## Operating Model

Fleet Mission Control is an observation surface. It loads each project config, reads each project's state/log metadata, and returns one aggregate response from `/api/v1/fleet`. One project load error is shown on that project card without hiding the rest of the fleet.

The project runner remains the execution surface. It starts workers, reconciles dead sessions, opens and monitors PRs, waits for review gates, merges eligible PRs, deploys when configured, and updates local state.

The supervisor is the explainability and policy surface. It records queue analysis, selected candidates, stuck states, safe label mutations, and approval requests. Safe actions are limited to actions explicitly listed in `supervisor.safe_actions`. Risky recommendations are recorded for an operator; approving them with the CLI records the approval but does not execute the risky action by itself.

Use this order during normal operations:

1. Open Fleet Mission Control at `http://127.0.0.1:8787/`.
2. Check fleet summary, stale project cards, attention cards, approvals, queue snapshots, and active workers.
3. Open a project dashboard link only when the fleet card needs project-level detail.
4. Use CLI commands with explicit `--config` paths when you need local state, logs, or a supervised decision.
5. Restart services rather than editing state files by hand.

## Queue Policy

Maestro supports a fixed ordered queue and a dynamic wave policy. Use one ordered queue for tightly sequenced work. Use dynamic wave for continuous operations where Mission Control should explain why the next runnable issue was selected or skipped.

Ordered queue policy:

| Rule | Behavior |
|---|---|
| `supervisor.ordered_queue.issues` | Only the first unfinished issue is eligible |
| Closed issue, merged linked PR, or `done_issues` override | Issue is considered finished and the queue advances |
| First unfinished issue has an open PR | Queue pauses and Mission Control recommends monitoring that PR |
| First unfinished issue is blocked, excluded, or retry exhausted | Queue pauses until an operator fixes the issue/policy or intentionally overrides it |
| Ordered queue exhausted and `dynamic_wave.enabled: true` | Dynamic wave can pick the next issue |

Dynamic wave policy:

| Rule | Behavior |
|---|---|
| Candidate source | Open GitHub issues from the project repo |
| Priority order | `p0`, `p1`, `p2`, `p3`, then unlabeled, with lower issue number as tie breaker |
| Runnable project statuses | Defaults to `Todo`, `To Do`, `Ready`, `Backlog`, and `New`, unless `supervisor.dynamic_wave.runnable_project_statuses` is set |
| Excluded labels | Built-ins include `blocked`, `wontfix`, `question`, `duplicate`, `invalid`, `epic`, and `meta`, plus `exclude_labels`, `supervisor.excluded_labels`, and `supervisor.blocked_label` |
| Other skips | Already running, done, retry exhausted, mission parent, epic-like title, and open blockers detected by `blocker_patterns` |
| Ready label | `supervisor.ready_label` is treated as a queue label and is added to selected work only when `add_ready_label` is allowed |
| Owned ready label | When `owns_ready_label: true`, dynamic wave keeps the ready label on the selected issue and can remove it from other issues if policy allows |
| Blocked label | `supervisor.blocked_label` makes an issue ineligible; it can be removed only when `remove_blocked_label` is an allowed safe action |

Fleet cards surface `open`, `eligible`, `excluded`, `non_runnable_project_status`, selected candidate, and top skipped reason so operators can tell whether the queue is empty, held by policy, or waiting on project status.

## Review And Approval Gates

The default PR review gate is Greptile. A project with `review_gate: greptile` waits for CI and Greptile approval before merge. A project with `review_gate: none` skips the Greptile gate, but this should be an explicit per-project policy decision.

PR states to watch:

| State | Meaning | Operator response |
|---|---|---|
| `pr_open` | A worker opened a PR and Maestro is waiting for checks, review, mergeability, merge interval, or conflict handling | Monitor, do not spawn duplicate work for the same issue |
| `queued` | A follow-up step or merge queue path is still pending | Check project dashboard and latest supervisor decision |
| `review_retry_backoff` | Actionable review feedback scheduled an in-place retry and Maestro is waiting for backoff | Wait for the scheduled retry worker unless the feedback should be handled manually |
| `review_retry_pending` | Backoff elapsed and Maestro is waiting for an available retry worker slot | Wait for the next orchestration cycle or free a worker slot |
| `review_retry_running` | The retry worker is updating the existing PR in place | Wait for the worker to finish and push updates |
| `review_retry_recheck` | The retry updated the PR and Maestro is waiting for CI, Greptile, or the merge gate | Monitor checks/review; Maestro will merge when gates allow it |
| `greptile_pending` stuck state | Greptile has not finished | Wait or check the GitHub PR/check run if it remains pending unusually long |
| `greptile_not_approved` stuck state | Greptile review found actionable feedback or no approval | Address feedback, allow the configured retry path, or make a deliberate project policy change |
| `failing_checks` stuck state | Required checks failed | Inspect the check failure, retry intentionally if budget remains, or fix manually |
| `unmergeable_pr` stuck state | GitHub reports conflicts or unknown mergeability | Wait for GitHub to compute mergeability, or rebase/resolve conflicts |

Supervisor approvals are stale-sensitive. A pending approval becomes stale if the decision payload changes or the target session/PR state changes, and pending `spawn_worker` approvals become superseded when a matching worker has already started. Fleet Mission Control shows pending approvals first; superseded, stale, approved, and rejected approvals are audit history collapsed below the active inbox. Do not approve a stale or superseded approval. Re-run the supervisor, review the new decision, and approve or reject the new ID if appropriate.

## Safe Commands

Use explicit config paths for project commands. These commands are safe for local operation because they do not print token values or dump entire configs. Treat worker logs as potentially sensitive and avoid pasting full logs into PRs or issues.

```bash
# Fleet dashboard and API
maestro serve --fleet ~/.maestro/fleet.yaml --host 127.0.0.1 --port 8787 --read-only
curl -fsS http://127.0.0.1:8787/api/v1/fleet

# Project status and queue analysis
maestro status --config ~/.maestro/maestro-<project>.yaml
maestro status --config ~/.maestro/maestro-<project>.yaml --json
maestro supervise --config ~/.maestro/maestro-<project>.yaml --once
maestro supervise --config ~/.maestro/maestro-<project>.yaml --once --json

# Worker logs through Maestro
maestro logs --config ~/.maestro/maestro-<project>.yaml
maestro logs --config ~/.maestro/maestro-<project>.yaml <session>

# Service status and restart
systemctl --user status maestro@<project>.service --no-pager
journalctl --user -u maestro@<project>.service --since "30 minutes ago" --no-pager
systemctl --user restart maestro@<project>.service
systemctl --user status maestro-fleet.service --no-pager
journalctl --user -u maestro-fleet.service --since "30 minutes ago" --no-pager
systemctl --user restart maestro-fleet.service
```

Avoid these during incident handling unless you are deliberately debugging credentials: `env`, raw config dumps, `gh auth token`, shell history dumps, and full worker log pastebacks.

## Recovery Playbook

### No eligible issues

Mission Control indicators: queue `eligible=0`, `no_eligible_issues`, `all_eligible_issues_excluded`, `ordered_queue_exhausted`, or a nonzero `non_runnable_project_status` count.

Safe response:

1. Run `maestro supervise --config ~/.maestro/maestro-<project>.yaml --once` and read the queue summary.
2. If there are no open issues, add or wait for work.
3. If issues are missing the ready label, add the configured `supervisor.ready_label` or let the supervisor add it when `add_ready_label` is allowed.
4. If issues are excluded, remove the blocking/excluded label only after confirming the issue is actually runnable.
5. If dynamic wave reports non-runnable project status, move one issue to a configured runnable status or update `supervisor.dynamic_wave.runnable_project_statuses` in the project config.
6. Restart the project runner only if config changed: `systemctl --user restart maestro@<project>.service`.

### Running but dead PID

Mission Control indicators: status `running` with `alive=false`, CLI `ALIVE no`, or stuck state `dead_running_pid`.

Safe response:

1. Run `maestro status --config ~/.maestro/maestro-<project>.yaml` to confirm the session and PID.
2. Inspect the session with `maestro logs --config ~/.maestro/maestro-<project>.yaml <session>`.
3. Restart the project runner with `systemctl --user restart maestro@<project>.service` so the next reconciliation cycle can mark the session dead and retry if eligible.
4. If you intentionally need to reconcile immediately, run `maestro run --config ~/.maestro/maestro-<project>.yaml --once` knowing it can progress orchestration for that project.
5. Do not edit `state.json` manually.

### PR open waiting Greptile

Mission Control indicators: `pr_open`, stuck state `greptile_pending`, or a PR card with passing CI but no review approval yet.

Safe response:

1. Wait for Greptile if the pending state is fresh.
2. Check the GitHub PR page if it remains pending unusually long.
3. If Greptile is not approved, address the feedback or let the configured retry path handle review feedback.
4. Do not spawn another worker for the same issue while the PR is open.
5. Change `review_gate` only as an explicit project policy decision, then restart the project runner.

### Retry exhausted

Mission Control indicators: session status `retry_exhausted`, action `review_retry_exhausted`, or stuck state `retry_exhausted`.

Safe response:

1. Inspect the session status and logs with explicit `--config` commands.
2. If a PR is still open, keep it in normal merge flow when checks and review gates pass.
3. If checks failed or no usable PR exists, review failed attempts, split or clarify the issue, and retry intentionally.
4. If retrying is appropriate, update the issue/config first, then start a new worker with `maestro spawn --config ~/.maestro/maestro-<project>.yaml --issue <number>`.
5. Do not increase retry budgets globally just to clear one incident unless that is the intended project policy change.

### Stale approval

Mission Control indicators: approval status `stale` or CLI error `approval is stale` / `approval payload changed`.

Safe response:

1. Do not approve the stale approval.
2. Run `maestro supervise --config ~/.maestro/maestro-<project>.yaml --once` to record a fresh decision.
3. Review the new target, risk, reasons, and stuck states.
4. Resolve the new approval ID if needed:

```bash
maestro supervise approve --config ~/.maestro/maestro-<project>.yaml --actor <operator> --reason "approved after fresh status check" <approval-or-decision-id>
maestro supervise reject --config ~/.maestro/maestro-<project>.yaml --actor <operator> --reason "state changed" <approval-or-decision-id>
```

### Project API failure

Mission Control indicators: project card error, failed `maestro supervise`, failed `maestro status`, GraphQL/project item errors, or GitHub CLI authentication errors.

Safe response:

1. Check only auth status, not token values: `gh auth status`.
2. Confirm the GitHub user or app has access to the repo and project board.
3. Retry after GitHub rate limits or project API incidents clear.
4. If dynamic wave depends on project status, keep work paused until project item data is reliable or make an explicit temporary policy change in the project config.
5. Restart only the affected project runner after config or auth is fixed.
6. Do not paste raw GraphQL output, tokens, or local config contents into issues or PRs.

## Operator Checklist

- Fleet dashboard is reachable on `127.0.0.1:8787` and is read-only.
- Every project in `~/.maestro/fleet.yaml` has a distinct `state_dir`, `session_prefix`, and optional project dashboard port.
- Project commands use `--config ~/.maestro/maestro-<project>.yaml`.
- Dynamic wave has known runnable statuses and clear ready/blocked label ownership.
- Greptile gate policy is explicit per project.
- Approvals are fresh before being approved or rejected.
- Incident notes contain summaries and issue/PR numbers, not secrets, raw env output, full config files, or full logs.
