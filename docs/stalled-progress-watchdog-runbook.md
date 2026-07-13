# Stalled-progress watchdog — durable 20-minute liveness for hands-off projects

The stalled-progress watchdog is maestro's durable, multi-signal liveness
contract for unattended projects (#887). It replaces the legacy
`worker_silent_timeout_minutes`, which watched terminal output only — so it could
kill a worker that was actively editing files without emitting output, and when
disabled (`0`/absent) it could not recover a genuinely hung-but-alive worker.

This runbook is safe for shared docs: it uses placeholders for local paths and
never requires printing tokens, environment variables, or raw config. Every
signal maestro records is a non-reversible digest — no raw path, secret, or
command output is persisted.

## What "material progress" means

The watchdog derives **one durable per-project watermark** from whichever
phase-appropriate signals are present. A single missing or stale signal is never
proof of a stall: the watermark advances whenever *any* signal advances (or the
observed set changes).

| Signal | Evidence |
| --- | --- |
| `issue_session` | issue/session state and lease identity (status, retry counters) |
| `process_tmux` | process id + exact tmux/session identity of live workers |
| `terminal_checkpoint` | terminal output hash or checkpoint advancement |
| `worktree_git` | bounded worktree evidence — branch + PR head (git identity), excluding volatile/generated paths |
| `pr_review` | PR head, CI/check/review state, merge/release identity |
| `delivery` | delivery approval generation, execution lease, terminal receipt |

The combined identity is a stable digest of the present `(kind, fingerprint)`
pairs. Two evaluations with the same present signals share one identity; any
signal advancing changes it and re-arms the deadline.

## The three cadences (reported separately, truthfully)

`poll_interval_seconds` describes the **orchestrator** cycle. Fleet used to show
it beside the supervisor pulse as if it were the whole story. The pulse now
reports the cadences that actually exist, side by side:

- `orchestrator_interval_seconds` — the orchestrator poll cadence.
- `watchdog_eval_interval_seconds` — the watchdog's own evaluation cadence
  (`stalled_progress_watchdog.eval_interval_seconds`, default 60s).
- `silence_budget_seconds` — the configured maximum silence before the watchdog
  acts (`stalled_progress_watchdog.max_silence_minutes`, default 20m).

The Fleet `supervisor_pulse.stalled_progress_watchdog` block also reports the
last material watermark, the derived next deadline, per-signal progress, and the
last recovery decision — each independently, so a quiet-but-active worker is
visibly *making progress* rather than *about to be killed*.

## Configuration

```yaml
stalled_progress_watchdog:
  enabled: true              # default true for new hands-off projects
  max_silence_minutes: 20    # 0 = default (20); negative or enabled:false = disabled
  eval_interval_seconds: 60  # watchdog evaluation cadence
```

Both knobs are fleet-controllable (`maestro settings`):

- `stalled_progress_watchdog.enabled`
- `stalled_progress_watchdog.max_silence_minutes`

A disabled or negative budget collapses to a zero silence budget, which the
evaluator reads as **disabled** — no quiet worker is ever killed by a
misconfiguration.

## Recovery semantics (the replay boundary)

When *no* signal has advanced for the whole silence budget, the watchdog records
a recovery decision. The action depends on the lifecycle phase:

- **pre-delivery** (issue → worker → PR/CI/review → merge): a proven safe stall
  asks the orchestrator to **stop the single stale worker and retry/resume
  exactly once** under the existing retry budget. Recovery never creates two
  live workers for one issue; the retry budget is the single "once" authority.
- **delivery executing / uncertain**: recovery authority ends **before** the
  durable delivery lease. The watchdog **surfaces operator reconciliation and
  never replays** the delivery automatically. Use the
  [delivery-reconciliation runbook](delivery-reconciliation-runbook.md) (#872).

Every recovery decision records the observed signal set (by kind), the last
material watermark, the deadline, the phase, the idempotency/replay boundary,
and the action plus its no-op reason — never a secret, raw private path, or
command output.

## Durability

The watermark lives in the project's `state.json` (`material_progress`) and
survives daemon restart. The deadline is never stored — it is always derived
from `watermark.at + silence_budget`, so a reload re-reads the same watermark and
computes the identical deadline. The concurrent-writer merge keeps the snapshot
with the newer material-progress time, so a concurrent stall record can never
clobber a fresh progress advance and reset the deadline.

## Promotion gate

The fingerprint-bound `multi-signal-progress-v1` contract is emitted by
genesis/lifecycle templates only after runtime canary evidence. Until a project
is live-proven, the capability stays a visible promotion blocker and the
lifecycle planner fails closed — it does not emit the unsafe legacy
`worker_silent_timeout_minutes` for new projects.

Two live canaries close the loop (tracked as runtime verification):

- **no-false-kill**: a quiet worker editing through at least one 20-minute
  deadline reaches a visible commit/PR without a restart.
- **recovery**: a deliberately stalled, harmless pre-delivery worker is detected
  within the budget, stopped once, respawned/resumed once, and reaches a visible
  PR/result without operator prompting.

No canary may expose or rotate real credentials; credential containment is
tracked separately in #888.
