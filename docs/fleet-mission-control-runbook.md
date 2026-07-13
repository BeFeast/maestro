# Fleet Mission Control Operating Runbook

Use Fleet Mission Control as the primary operations surface for Maestro-managed repositories. The fleet dashboard aggregates project configs, runner state, supervisor decisions, approvals, stuck states, and active workers in one view. On a trusted LAN the dashboard ships write-enabled by default (#477); the cautious approval gate still guards `merge_pr`, `close_issue`, `delete_worktree`, and `change_global_config`. Add `--read-only=true` (or `server.read_only: true` in YAML) for installs exposed beyond a trusted LAN, and configure the optional HTTP auth layer that #616 wires up.

This runbook is intentionally safe for shared docs. It uses placeholders for local paths and never requires printing tokens, environment variables, raw config dumps, or full worker logs.

## Fleet Service

Run one Maestro service on the execution host:

| Service | Default bind | Port | Purpose | Notes |
|---|---:|---:|---|---|
| Maestro fleet daemon + Mission Control | `127.0.0.1` CLI default; `0.0.0.0` in the shipped service | `8786` | Runs every project flow and serves the dashboard, `/api/v1/fleet`, and project-scoped routes (`/project/<name>`) | `maestro daemon --watch-store --store ~/.maestro/maestro.db`; the shipped `maestro.service` uses the same store and port |
| Worker sessions | none | none | tmux sessions and log files created under each project's `state_dir` | Inspect through Mission Control, `maestro status`, or `maestro logs` |
| OpenClaw relay | `127.0.0.1` | `18789` | Optional Telegram relay endpoint when `telegram.mode: openclaw` is used | Not required for Mission Control |

There is no separate fleet dashboard, project runner, or supervisor service. The
daemon owns all three responsibilities. Per-project ports and
`maestro-<project>-web.service`, `maestro@<project>`, `fleet.yaml`, and
`maestro-fleet.service` belong only to the retired topology; use the explicit
legacy migration procedure in the
[project setup runbook](project-setup-runbook.md#single-daemon-mission-control-and-config-store-operating-model)
rather than recreating them.

### Dashboard auth posture: trusted LAN vs. exposed install

The dashboard auth layer is opt-in. Choose the posture that matches the network the port is reachable from:

- **Trusted LAN (default, no auth configured).** The dashboard binds to `127.0.0.1` or a private network only operators reach. The cautious approval gate still protects `merge_pr`, `close_issue`, `delete_worktree`, and `change_global_config` so flipping `--read-only=false` on a trusted LAN does not expose the four destructive verbs. Per the operator decision recorded against #477 (2026-06-02): the LAN is closed, alien access is not part of the threat model, and `server.auth` may be left empty.
- **Exposed / shared-network install (auth required, #616).** The dashboard is reachable from anywhere outside the trusted LAN — `--host 0.0.0.0`, behind a reverse proxy, on a multi-tenant host, or on a workshop network shared with untrusted devices. Configure `server.auth.token_env` in each project config (the fleet picks up the first project that sets it) and populate the named environment variable from your secret manager (Infisical, 1Password CLI, etc.) — never inline the token in YAML. With auth enabled, every endpoint — **read** `GET /api/v1/...`, **write** `POST /api/v1/...` (`/actions`, `/approvals/.../{approve|reject}`, `/audit/log`, `/fleet/actions`, `/refresh`), and the SPA HTML / static assets — requires a credential and returns `401` otherwise. The cautious approval gate still fires for authenticated callers as defense in depth.

The server advertises both **HTTP Basic** (any username, password equal to the token — so browsers prompt natively and cache the credential for the realm) and **Bearer** (`Authorization: Bearer <token>` — for `curl`, scripts, and secret-manager-driven clients) in the `WWW-Authenticate` challenge. The authenticated identity replaces any `actor` field in the request body so operators sharing a token cannot impersonate one another in the audit log.

## Config Boundaries

The config-store row is the runtime source of truth; the portable YAML is an
approved input to genesis, not a second watched config surface.

| Surface | Loaded by | Owns | Does not own |
|---|---|---|---|
| Project row in `~/.maestro/maestro.db` | `maestro daemon --watch-store` | Repo, clone/worktree paths, stable identity, Management Home link, state/session paths, labels, supervisor, review, merge, and deploy policy | Other project rows |
| Private portable project YAML | `maestro project plan` / `project apply` | Desired project row reviewed during genesis | Runtime fleet membership after apply |
| In-repo approved policy/specification | Workers and project policy loaders where explicitly supported | Executable requirements and repo-owned policy | Private Management Home planning context |

Do not create `fleet.yaml` or duplicate membership in a second file. Add/change a
row through the supported config-store/project workflow; `--watch-store` applies
it without restarting the daemon. The built-in dashboard routes operators to
`/project/<name>` on port `8786`.

Minimal project config shape:

```yaml
repo: OWNER/REPO
local_path: /path/to/repos/<project>
worktree_base: /path/to/worktrees/<project>
project_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301
management_home:
  kind: obsidian
  path: /absolute/path/to/Obsidian Vault/Dev/Areas/<project>
  vault: Obsidian Vault
  vault_path: Dev/Areas/<project>
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

Apply the portable YAML with `maestro project plan` followed by its exact
`next[0]` command. A second plan must report `no-op`; the daemon then exposes the
row at `/project/<name>` and in `/api/v1/fleet`.

## Operating Model

Mission Control is the daemon's built-in operations surface. The daemon reads
every project row from `maestro.db`, runs one flow per row, reads each flow's
state/log metadata, and returns one aggregate response from `/api/v1/fleet`. The
header starts with one global operator brief; project cards, supervisor details,
queues, and worker logs are drilldown/debug data. One project load error is shown
on that project card without hiding the rest of the fleet.

The same daemon is the execution surface. Each flow starts workers, reconciles
dead sessions, opens and monitors PRs, waits for review gates, merges eligible
PRs, deploys when configured, and updates local state.

When deploying Maestro itself from source on the fleet host, stamp the binary with the release version from `VERSION`; an unstamped source build leaves `maestro version` reporting build metadata instead of the release. Use the same stamped build shape as the release workflow, then verify the installed binary:

```bash
cd /path/to/repos/maestro
git pull --ff-only origin main
VERSION="$(sed -nE 's/^version[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' VERSION)"
test -n "$VERSION"
go build -trimpath -ldflags "-X main.version=${VERSION}" -o maestro ./cmd/maestro/
./maestro version
```

Each daemon flow includes its supervisor explainability and policy loop. It
records queue analysis, selected candidates, stuck states, outcome context, safe
label mutations, and approval requests. Safe actions are limited to actions
explicitly listed in `supervisor.safe_actions`. Risky recommendations are
recorded for an operator; approving them with the CLI records the approval but
does not execute the risky action by itself.

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

1. Open Fleet Mission Control at `http://127.0.0.1:8786/` (or the trusted host address configured by `maestro.service`).
2. Read the global operator brief first; if it says no action is needed, treat the rest of the page as supporting detail.
3. If the brief names an action, follow that project, issue/PR/session, and reason before scanning lower-priority cards.
4. Open `/project/<name>` only when the fleet card needs project-level detail.
5. Use config-store selectors when a supported CLI diagnostic needs one project.
6. Never edit state files by hand; restart the one daemon service only when the daemon process itself is unhealthy.

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

Use the shared store plus an explicit project row for project-scoped diagnostics.
These commands do not print token values or dump entire configs. Treat worker
logs as potentially sensitive and avoid pasting full logs into PRs or issues.

```bash
# Fleet daemon, dashboard, and API
maestro daemon --watch-store --store ~/.maestro/maestro.db \
  --approvals-store sqlite --state-store sqlite
curl -fsS http://127.0.0.1:8786/api/v1/fleet

# Project status and read-only queue analysis (use the config-store row name)
maestro status --config-store ~/.maestro/maestro.db --config-store-project <project> --json
maestro supervise --config-store ~/.maestro/maestro.db --config-store-project <project> --once --dry-run --json

# Worker status/log excerpts are available from the project's Mission Control
# route; keep full logs on the execution host.

# The only Maestro service (Linux user-service installation)
systemctl --user status maestro.service --no-pager
journalctl --user -u maestro.service --since "30 minutes ago" --no-pager
systemctl --user restart maestro.service   # only when the daemon itself is unhealthy
```

Avoid these during incident handling unless you are deliberately debugging credentials: `env`, raw config dumps, `gh auth token`, shell history dumps, and full worker log pastebacks.

## Webhook Ingestion (#824)

The single-service `maestro daemon` can also ingest inbound GitHub webhooks into
the unified `~/.maestro/maestro.db` — a push-shaped alternative to polling for
issue / PR / check / review / label state. It is default OFF and opt-in via
`--webhook-secret-file`; the ingestion diagnostics surface on `/api/v1/fleet`
under the `webhooks` block (last delivery time, per-event-type counts,
signature-failure counter). See the dedicated
[Webhook Ingestion Runbook](webhook-ingestion-runbook.md) for setup, tunnel
configuration, and semantics.

## Recovery Playbook

### No eligible issues

Mission Control indicators: queue `eligible=0`, `no_eligible_issues`, `all_eligible_issues_excluded`, `ordered_queue_exhausted`, nonzero `held/meta` or `blocked-deps`, or a nonzero `non_runnable_project_status` count.

Safe response:

1. Run `maestro supervise --config-store ~/.maestro/maestro.db --config-store-project <project> --once --dry-run --json` and read the queue summary.
2. If there are no open issues, add or wait for work.
3. If issues are missing the ready label, add the configured `supervisor.ready_label` or let the supervisor add it when `add_ready_label` is allowed.
4. If issues are excluded, remove the blocking/excluded label only after confirming the issue is actually runnable.
5. If issues are held as parent/meta work, decompose or retitle/relabel only when the issue should become executable work.
6. If issues are blocked by dependencies, close or resolve the blocker issue before expecting a worker to start.
7. If dynamic wave reports non-runnable project status, move one issue to a configured runnable status or deliberately update `supervisor.dynamic_wave.runnable_project_statuses` in the project row.
8. Store changes are hot-reloaded; do not restart the daemon merely because one project config changed.

### Running but dead PID

Mission Control indicators: status `running` with `alive=false`, CLI `ALIVE no`, or stuck state `dead_running_pid`.

Safe response:

1. Run `maestro status --config-store ~/.maestro/maestro.db --config-store-project <project> --json` to confirm the session and PID.
2. Inspect the session through the project's Mission Control route; keep full logs on the execution host.
3. Let the daemon's next reconciliation cycle mark the session dead and retry if eligible.
4. Restart `maestro.service` only if the daemon process itself is unhealthy, not to restart one project flow.
5. Do not edit `state.json` manually or launch a competing `maestro run --once` process.

### PR open waiting Greptile

Mission Control indicators: `pr_open`, stuck state `greptile_pending`, or a PR card with passing CI but no review approval yet.

Safe response:

1. Wait for Greptile if the pending state is fresh.
2. Check the GitHub PR page if it remains pending unusually long.
3. If Greptile is not approved, address the feedback or let the configured retry path handle review feedback.
4. Do not spawn another worker for the same issue while the PR is open.
5. Change `review_gate` only as an explicit project policy decision; the daemon hot-reloads the changed store row.

### Retry exhausted

Mission Control indicators: session status `retry_exhausted`, action `review_retry_exhausted`, or stuck state `retry_exhausted`.

Safe response:

1. Inspect the session status and logs with explicit `--config` commands.
2. If a PR is still open, keep it in normal merge flow when checks and review gates pass.
3. If checks failed or no usable PR exists, review failed attempts, split or clarify the issue, and retry intentionally.
4. If retrying is appropriate, update the issue/project row first, then start a new worker with `maestro spawn --config-store ~/.maestro/maestro.db --config-store-project <project> --issue <number>`.
5. Do not increase retry budgets globally just to clear one incident unless that is the intended project policy change.

### Stale approval

Mission Control indicators: approval status `stale` or CLI error `approval is stale` / `approval payload changed`.

Safe response:

1. Do not approve the stale approval.
2. Let the daemon supervisor record a fresh decision, or run a read-only diagnostic with the config-store selectors shown above.
3. Review the new target, risk, reasons, and stuck states.
4. Resolve the new approval through the project's Mission Control approval
   inbox, which is already bound to the daemon flow and shared approval store.

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
4. To temporarily disable filtering for a project, set `stale_session_reconciler.enabled: false` in the project row; the daemon hot-reloads it.
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
5. Project-row changes hot-reload. Restart the single daemon only if the daemon process itself remains unhealthy after auth/config is fixed.
6. Do not paste raw GraphQL output, tokens, or local config contents into issues or PRs.

## Operator Checklist

- The single daemon and Mission Control are reachable on port `8786`; on a trusted LAN it is write-enabled by default (#477), with the cautious approval gate guarding `merge_pr` / `close_issue` / `delete_worktree` / `change_global_config`.
- `maestro.service`, project genesis, and operator commands all use `~/.maestro/maestro.db`.
- Every project row has a distinct stable identity, `state_dir`, and `session_prefix`; there are no per-project dashboard ports or services.
- Project diagnostics use explicit `--config-store` and `--config-store-project` selectors where supported.
- Dynamic wave has known runnable statuses and clear ready/blocked label ownership.
- Greptile gate policy is explicit per project.
- Approvals are fresh before being approved or rejected.
- Incident notes contain summaries and issue/PR numbers, not secrets, raw env output, full config files, or full logs.
