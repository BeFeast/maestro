# Fleet Mission Control Operating Runbook

Use Fleet Mission Control as the primary operations surface for Maestro-managed repositories. The fleet dashboard aggregates project configs, runner state, supervisor decisions, approvals, stuck states, and active workers in one view. On a trusted LAN the dashboard ships write-enabled by default (#477); the cautious approval gate still guards `merge_pr`, `close_issue`, `delete_worktree`, and `change_global_config`. Add `--read-only=true` (or `server.read_only: true` in YAML) for installs exposed beyond a trusted LAN, and configure the optional HTTP auth layer that #616 wires up.

This runbook is intentionally safe for shared docs. It uses placeholders for local paths and never requires printing tokens, environment variables, raw config dumps, or full worker logs.

## Workshop Services

Reserve these services and ports on the workshop host:

| Service | Default bind | Port | Purpose | Notes |
|---|---:|---:|---|---|
| Fleet Mission Control | `127.0.0.1` | `8787` | The single dashboard for the whole fleet, plus the `/api/v1/fleet` aggregate API and project-scoped routes (`/project/<name>`) | Start with `maestro serve --fleet ~/.maestro/fleet.yaml --host 127.0.0.1 --port 8787` (writes enabled on trusted LAN; add `--read-only=true` for exposed installs) |
| Project runner | none by default | none | Runs `maestro run --config ...` and owns workers, worktrees, PR handling, and merge/deploy loops | It only serves HTTP when that project config has `server.port` set |
| Supervisor loop | none | none | Runs `maestro supervise --config ...` to record decisions, safe queue label mutations, and approval requests | Can be manual, timer-driven, or a user service |
| Worker sessions | none | none | tmux sessions and log files created under each project's `state_dir` | Inspect through Mission Control, `maestro status`, or `maestro logs` |
| OpenClaw relay | `127.0.0.1` | `18789` | Optional Telegram relay endpoint when `telegram.mode: openclaw` is used | Not required for Mission Control |

> Per-project Mission Control ports (`8788+`) were retired in #516. Every project is now reachable through the fleet aggregator at `/project/<name>`. Old `maestro-<project>-web.service` user units can be stopped and disabled; their `server.port` settings in project YAMLs are honored only when a project runs its own single-tenant `maestro serve --config ...` outside the fleet.

### Dashboard auth posture: trusted LAN vs. exposed install

The dashboard auth layer is opt-in. Choose the posture that matches the network the port is reachable from:

- **Trusted LAN (default, no auth configured).** The dashboard binds to `127.0.0.1` or a private network only operators reach. The cautious approval gate still protects `merge_pr`, `close_issue`, `delete_worktree`, and `change_global_config` so flipping `--read-only=false` on a trusted LAN does not expose the four destructive verbs. Per the operator decision recorded against #477 (2026-06-02): the LAN is closed, alien access is not part of the threat model, and `server.auth` may be left empty.
- **Exposed / shared-network install (auth required, #616).** The dashboard is reachable from anywhere outside the trusted LAN — `--host 0.0.0.0`, behind a reverse proxy, on a multi-tenant host, or on a workshop network shared with untrusted devices. Configure `server.auth.token_env` in each project config (the fleet picks up the first project that sets it) and populate the named environment variable from your secret manager (Infisical, 1Password CLI, etc.) — never inline the token in YAML. With auth enabled, every endpoint — **read** `GET /api/v1/...`, **write** `POST /api/v1/...` (`/actions`, `/approvals/.../{approve|reject}`, `/audit/log`, `/fleet/actions`, `/refresh`), and the SPA HTML / static assets — requires a credential and returns `401` otherwise. The cautious approval gate still fires for authenticated callers as defense in depth.

The server advertises both **HTTP Basic** (any username, password equal to the token — so browsers prompt natively and cache the credential for the realm) and **Bearer** (`Authorization: Bearer <token>` — for `curl`, scripts, and secret-manager-driven clients) in the `WWW-Authenticate` challenge. The authenticated identity replaces any `actor` field in the request body so operators sharing a token cannot impersonate one another in the audit log.

## Config Boundaries

Project config and fleet config have different jobs.

| File | Loaded by | Owns | Does not own |
|---|---|---|---|
| `~/.maestro/maestro-<project>.yaml` | `maestro run`, `maestro supervise`, `maestro status`, `maestro logs`, single-project `maestro serve` | Repo, clone path, worktree path, state directory, session prefix, labels, supervisor policy, review gate, merge/deploy policy | Other projects |
| `~/.maestro/fleet.yaml` | `maestro serve --fleet` | Project display names and project config paths | Queue policy, labels, state directories, review gates, merge behavior |
| `.maestro/supervisor.yaml`, `.maestro/supervisor.yml`, or `.maestro/supervisor.md` | Loaded beside the project config or inside `local_path/.maestro` | Supervisor policy when the team wants policy beside the repo | Fleet membership |

Fleet config paths may be absolute, `~/...`, or relative to the fleet YAML file. A fleet file should not duplicate project settings. If a project needs a different label, review gate, state directory, or runner interval, change that project's config and restart that project's runner.

`dashboard_url` was historically used to link from the fleet card to a per-project dashboard on its own port. Those ports are retired (#516); the aggregator routes operators to `/project/<name>` instead. Any `dashboard_url` value still present in a fleet.yaml file is ignored and logged as deprecated on load.

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

outcome:
  desired_outcome: Users can run the product end-to-end.
  runtime_target: https://app.example.com
  deployment_status_command: /path/to/repos/<project>/scripts/status.sh
  healthcheck_url: https://app.example.com/healthz
  source_repo_path: /path/to/repos/<project>
  runtime_host: production host or platform
  non_goals:
    - Rewrite unrelated subsystems

server:
  host: 127.0.0.1
  port: 8788
  # read_only defaults to false (#477 trusted-LAN posture); set true for exposed installs.

blocker_patterns:
  - "blocked by.*?#(\\d+)"
  - "blocked until.*?#(\\d+).*merged"
  - "depends on.*?#(\\d+)"

supervisor:
  enabled: true
  mode: cautious
  ready_label: maestro-ready
  blocked_label: blocked
  dispatch_sla_seconds: 300
  dynamic_wave:
    enabled: true
    owns_ready_label: true
    runnable_project_statuses:
      - Todo
      - To Do
      - Ready
      - Backlog
      - New
    dependency_unblock:
      enabled: true
      max_runnable: 1
      enroll_in_project: true
      announce_with_comment: true
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
  - name: project-b
    config: maestro-project-b.yaml
```

`dashboard_url:` is deprecated and silently ignored on load (#516). Every project is reachable at `/project/<name>` on the aggregator port.

## Operating Model

Fleet Mission Control is an observation surface. It loads each project config, reads each project's state/log metadata, and returns one aggregate response from `/api/v1/fleet`. The header starts with one global operator brief across every configured project; project cards, supervisor details, queues, and worker logs are drilldown/debug data after that brief. One project load error is shown on that project card without hiding the rest of the fleet.

The project runner remains the execution surface. It starts workers, reconciles dead sessions, opens and monitors PRs, waits for review gates, merges eligible PRs, deploys when configured, and updates local state.

When deploying Maestro itself from source on the fleet host, stamp the binary with the release version from `VERSION`; an unstamped source build leaves `maestro version` reporting build metadata instead of the release. Use the same stamped build shape as the release workflow, then verify the installed binary:

```bash
cd /path/to/repos/maestro
git pull --ff-only origin main
VERSION="$(sed -nE 's/^version[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' VERSION)"
test -n "$VERSION"
go build -trimpath -ldflags "-X main.version=${VERSION}" -o maestro ./cmd/maestro/
./maestro version
```

The supervisor is the explainability and policy surface. It records queue analysis, selected candidates, stuck states, outcome context, safe label mutations, and approval requests. Safe actions are limited to actions explicitly listed in `supervisor.safe_actions`. Risky recommendations are recorded for an operator; approving them with the CLI records the approval but does not execute the risky action by itself.

Each project card shows an outcome status: goal, runtime target, health state, and next action. If no `outcome` brief is configured, Mission Control says so explicitly because merged PR throughput is not the same as a working runtime. If PRs keep merging while runtime health is unknown or failing, the supervisor records `no_outcome_progress` and recommends a read-only deploy/runtime check instead of mutating production.

The global brief names one state for the whole fleet: healthy idle, running work, waiting for CI/review, no eligible issues, dispatch failure, stale worker, runtime outcome drift, or the single highest-priority action that needs an operator. When no human action is needed, it says so explicitly.

## Global Next Action Selection

Fleet Mission Control collapses the global brief into one canonical next operator action. `/api/v1/fleet` exposes it as the structured `next_action` field, or `null` when no action is needed. The legacy `verdict.sentence` and `operator_brief` fields stay populated for backward compatibility within this release.

`next_action` shape:

```json
{
  "project": "<project name>",
  "kind": "<operator state kind>",
  "target_url": "<PR or issue or dashboard URL>",
  "reason": "<one-sentence reason>",
  "priority": "P0",
  "picked_at": "<RFC3339 timestamp>"
}
```

Selection algorithm (deterministic, code-readable in `internal/server/fleet.go: buildFleetNextAction`):

1. Build a candidate list from approvals and per-project operator state.
2. Bucket each candidate into a priority tier:

   | Tier | Sources |
   |---|---|
   | P0 | project `error`, `dispatch_failure`, `stale_worker`; selected issue pending dispatch past the configured dispatch SLA; pending approval past the 30m SLA |
   | P1 | project `attention`; pending approval inside SLA |
   | P2 | project `outcome_drift`, `stale`, `no_eligible_issues`, `queue_blocked` |
   | P3 | project `outcome_missing` |

   Non-actionable kinds (`working`, `monitoring_pr`, `pending_dispatch` within SLA, `idle`, ...) are excluded.

3. Sort by priority ascending (P0 first).
4. Within the highest-occupied tier the **oldest by `updated_at`** wins. The "updated_at" comes from the underlying input — the worker session `started_at` for worker-driven kinds, the supervisor decision `created_at` for queue/dispatch/drift kinds, the approval `updated_at` for approvals — never from the snapshot timestamp. That keeps the choice stable across consecutive snapshots while the input is unchanged.
5. The remaining ties are broken by a deterministic key (`approval|project|id` or `project|name|kind`).
6. `picked_at` echoes the winner's `updated_at`, so reloading the dashboard while nothing changes returns the same `picked_at` and the UI text does not flicker.

The frontend (`internal/server/web/static/fleet.js: renderFleetVerdict`) renders one "What needs me now" line plus one CTA link to `target_url`. When `next_action` is `null` the dashboard shows a single `Quiet. Nothing needs you.` message — there are no expanding sections in the brief.

This algorithm is intentionally explicit and explainable. There is no ML priority picker; if the order changes you can read the tier mapping above and the candidate list in the Project Rail to see why.

Use this order during normal operations:

1. Open Fleet Mission Control at `http://127.0.0.1:8787/`.
2. Read the global operator brief first; if it says no action is needed, treat the rest of the page as supporting detail.
3. If the brief names an action, follow that project, issue/PR/session, and reason before scanning lower-priority cards.
4. Open a project dashboard link only when the fleet card needs project-level detail.
5. Use CLI commands with explicit `--config` paths when you need local state, logs, or a supervised decision.
6. Restart services rather than editing state files by hand.

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
| Excluded labels | Built-ins include `blocked`, `wontfix`, `question`, `duplicate`, and `invalid`, plus `exclude_labels`, `supervisor.excluded_labels`, and `supervisor.blocked_label` |
| Held/meta skips | Mission parents, mission issues awaiting decomposition, epic-like titles, and `epic`/`meta` labels are counted separately from exclusions |
| Blocker-dependency skips | Open blockers detected by `blocker_patterns` are counted separately from label-based blocked policy |
| Other skips | Already running, done, and retry exhausted |
| Ready label | `supervisor.ready_label` is treated as a queue label and is added to selected work only when `add_ready_label` is allowed |
| Owned ready label | When `owns_ready_label: true`, dynamic wave keeps the ready label on the selected issue and can remove it from other issues if policy allows |
| Dispatch SLA | `supervisor.dispatch_sla_seconds` controls how long a selected issue may remain pending dispatch before Fleet promotes it from `pending_dispatch` to `dispatch_failure` |
| Blocked label | `supervisor.blocked_label` makes an issue ineligible; it can be removed only when `remove_blocked_label` is an allowed safe action |

Fleet cards surface `open`, `eligible`, `excluded`, `held/meta`, `blocked-deps`, `non_runnable_project_status`, selected candidate, and top skipped reason so operators can tell whether the queue is empty, held by parent/meta policy, blocked by dependencies, or waiting on project status.

### Queue / Next decision plane (#720)

The per-project **Queue / Next** panel in Mission Control visualizes the supervisor's selection decision: the **next** issue (selected candidate, highlighted with its priority label), the **eligible** set in real selection order (priority `P0<P1<P2<P3`, then ascending issue number — the same order as the supervisor's `sortDynamicWaveCandidates`), and every **skipped** candidate with its reason (`retry limit exhausted`, `epic`/`meta`, `blocked`, `project status not runnable`, etc.) and queue counts. Rows link to their GitHub issue.

The panel renders entirely from the supervisor decision already held in fleet state — there are **no GitHub calls on the request path** — and refreshes on the existing 12s `/api/v1/fleet` poll. It stays correct regardless of the issue tracker (GitHub Projects today; Jira / Linear / Asana adapters later), because it reads the persisted decision rather than the tracker.

To carry the full ranked-eligible and skipped sets, the persisted `SupervisorDecision.QueueAnalysis` (`state.SupervisorQueueAnalysis`) gained two bounded fields (capped at 50 entries each):

| Field | JSON | Meaning |
| --- | --- | --- |
| `EligibleRanked` | `eligible_ranked` | Eligible candidates in real selection order; the first entry mirrors `selected_candidate`. |
| `SkippedCandidates` | `skipped_candidates` | Each skipped issue with `number`, `title`, `priority_label`, `category` (`excluded` / `held_meta` / `blocked_by_dependency` / `project_status` / `other`), and `reason`. |

Both are mirrored onto the fleet `queue_snapshot` so the SPA can read them without re-deriving anything client-side.

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
| `backend_auth_failure` (#693) | A backend CLI failed to authenticate (invalid/expired credentials); the worker died early and fell over to a fallback backend without burning the per-issue retry budget. The default backend down is a blocker; a non-default backend warns | Re-authenticate the backend CLI or re-sync its credentials; fallback backends keep the queue moving meanwhile |
| `backend_model_unavailable` (#713) | A backend's configured model is unavailable or not accessible (pulled, renamed, or no access — e.g. Fable pulled from subscriptions early); the worker died early and fell over to a fallback backend without burning the per-issue retry budget. The default backend's model down is a blocker; a non-default backend warns | Swap the model id (`model.default` or the backend's model) to an available one, or restore access; fallback backends keep the queue moving meanwhile |

Supervisor approvals are stale-sensitive. A pending approval becomes stale if the decision payload changes or the target session/PR state changes, and pending `spawn_worker` approvals become superseded when a matching worker has already started. Fleet Mission Control shows pending approvals first; superseded, stale, approved, and rejected approvals are audit history collapsed below the active inbox. Do not approve a stale or superseded approval. Re-run the supervisor, review the new decision, and approve or reject the new ID if appropriate.

## Safe Commands

Use explicit config paths for project commands. These commands are safe for local operation because they do not print token values or dump entire configs. Treat worker logs as potentially sensitive and avoid pasting full logs into PRs or issues.

```bash
# Fleet dashboard and API (writes enabled by default on trusted LAN; add --read-only=true for exposed installs)
maestro serve --fleet ~/.maestro/fleet.yaml --host 127.0.0.1 --port 8787
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

Mission Control indicators: queue `eligible=0`, `no_eligible_issues`, `all_eligible_issues_excluded`, `ordered_queue_exhausted`, nonzero `held/meta` or `blocked-deps`, or a nonzero `non_runnable_project_status` count.

Safe response:

1. Run `maestro supervise --config ~/.maestro/maestro-<project>.yaml --once` and read the queue summary.
2. If there are no open issues, add or wait for work.
3. If issues are missing the ready label, add the configured `supervisor.ready_label` or let the supervisor add it when `add_ready_label` is allowed.
4. If issues are excluded, remove the blocking/excluded label only after confirming the issue is actually runnable.
5. If issues are held as parent/meta work, decompose or retitle/relabel only when the issue should become executable work.
6. If issues are blocked by dependencies, close or resolve the blocker issue before expecting a worker to start.
7. If dynamic wave reports non-runnable project status, move one issue to a configured runnable status or update `supervisor.dynamic_wave.runnable_project_statuses` in the project config.
8. Restart the project runner only if config changed: `systemctl --user restart maestro@<project>.service`.

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

### Stale supervisor sessions

Mission Control indicators: `attention` shows old retry/dead sessions whose worktree has already been cleaned up or whose PR has been closed long ago, and `verdict.sentence` keeps reporting items needing attention even though no operator step is possible.

Safe response:

1. Confirm the project's `stale_session_reconciler` configuration before changing anything:
   - `enabled` defaults to `true`.
   - `idle_after_minutes` defaults to `1440` (24 hours).
   - `require_worktree_missing` defaults to `true`.
   - `merged_pr_dismisses` defaults to `true`.
2. A dead session is filtered from `attention` when **either** condition holds:
   - **Idle/worktree path:** the session is past `idle_after_minutes` AND, when `require_worktree_missing` is true, its recorded worktree path is missing on disk.
   - **Linked-PR-merged path:** the session's branch (head ref recorded as `branch` in the API, e.g. `feat/sup-44-346-…`) maps to a PR that the project state already classifies as merged. This path fires regardless of idle time, so a freshly retry-exhausted session whose PR has just merged stops haunting `attention` immediately.
3. Reconciled sessions are recorded in `audit-log.jsonl` under the project's `state_dir` with action `stale_session_reconciled`. The audit `reason` field carries the trigger (`linked PR merged` for the new path, otherwise the legacy idle-window reason). Sessions remain searchable in `workers` for drilldown.
4. To temporarily disable filtering for a project, set `stale_session_reconciler.enabled: false` and restart the project runner.
5. To keep only the legacy idle/worktree behaviour from PR #400, set `stale_session_reconciler.merged_pr_dismisses: false`.
6. To tighten the idle path during dogfood/recovery, lower `idle_after_minutes` (for example `60` for one hour). Keep `require_worktree_missing: true` so a live worker is never reclaimed.
7. The reconciler reads PR state from the existing project state snapshot (sessions already transitioned to `done`/`code_landed`); it does not issue ad-hoc `gh` calls in the snapshot path.
8. Per-project `stale_session_reconciler` config example:

```yaml
stale_session_reconciler:
  enabled: true
  idle_after_minutes: 1440
  require_worktree_missing: true
  merged_pr_dismisses: true
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

- Fleet dashboard is reachable on `127.0.0.1:8787`; on a trusted LAN it is write-enabled by default (#477), with the cautious approval gate guarding `merge_pr` / `close_issue` / `delete_worktree` / `change_global_config`.
- Every project in `~/.maestro/fleet.yaml` has a distinct `state_dir`, `session_prefix`, and optional project dashboard port.
- Project commands use `--config ~/.maestro/maestro-<project>.yaml`.
- Dynamic wave has known runnable statuses and clear ready/blocked label ownership.
- Greptile gate policy is explicit per project.
- Approvals are fresh before being approved or rejected.
- Incident notes contain summaries and issue/PR numbers, not secrets, raw env output, full config files, or full logs.
