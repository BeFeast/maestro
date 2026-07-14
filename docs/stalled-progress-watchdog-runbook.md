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

The watchdog derives **one durable watermark per exact lifecycle target**: each
live worker, PR gate, post-merge/live-verification gate, and delivery lease is
evaluated independently. Progress by worker B can never hide a frozen worker A.
Within one target, a single stale signal is never proof of a stall: its
watermark advances whenever *any* phase-appropriate signal advances.

| Signal | Evidence |
| --- | --- |
| `issue_session` | issue/session state and lease identity (status, retry counters) |
| `process_tmux` | process id + exact tmux/session identity of live workers |
| `terminal_checkpoint` | bounded live capture from the exact tmux session and/or checkpoint content advancement |
| `worktree_git` | bounded Git HEAD/index/status/diff/blob identity, excluding volatile/generated paths; mtimes alone never count |
| `pr_review` | durable per-PR head, CI/check/review/finding, late-feedback, and merge identity |
| `outcome_verification` | semantic post-merge deploy/live-verification outcome changes; repeated identical poll timestamps do not count |
| `delivery` | delivery approval generation, execution lease, terminal receipt |

Each target's combined identity is a stable digest of its present
`(kind, fingerprint)` pairs. Two evaluations with the same present signals
share one identity; any signal advancing changes it and re-arms only that
target's deadline. Targets absent from a complete evaluation snapshot are
retired, so an idle project has no synthetic armed deadline.

A configured Git, tmux, or checkpoint probe that fails, times out, or exceeds a
bound is **explicit incomplete evidence**, not an absent/stale signal. The old
watermark and deadline remain unchanged, but the evaluator records
`evidence_unavailable` and suppresses destructive recovery until a complete
observation returns. Restoring observability therefore does not buy a stalled
target another silence budget.

## The three cadences (reported separately, truthfully)

`poll_interval_seconds` describes the **orchestrator** cycle. Fleet used to show
it beside the supervisor pulse as if it were the whole story. The pulse now
reports the cadences that actually exist, side by side:

- `orchestrator_interval_seconds` — the orchestrator poll cadence.
- `supervisor_interval_seconds` — the actual running daemon supervisor cadence
  (runtime-injected; project YAML cannot self-claim it).
- `watchdog_eval_interval_seconds` — the watchdog's own evaluation cadence
  (`stalled_progress_watchdog.eval_interval_seconds`, default 60s).

`silence_budget_seconds` is reported separately from those three clocks. It is
the configured maximum silence before the evaluator recommends an action
(`stalled_progress_watchdog.max_silence_minutes`, default 20m).

The evaluator runs on its own per-project timer, outside `Engine.Decide` and
GitHub/LLM work, and starts before the first standalone `maestro supervise`
network/decision cycle. A failed or blocked first supervisor cycle therefore
cannot pause this cadence. `maestro supervise --once` performs one local
evaluation before its network/decision work; `--dry-run` remains non-persistent.
The Fleet `supervisor_pulse.stalled_progress_watchdog` block also reports each
active target, its watermark/deadline/signals, the latest recommendation, and
the latest **actual** recovery result separately. A recommendation never appears
as a completed recovery.

## Configuration

```yaml
stalled_progress_watchdog:
  enabled: true              # explicit opt-in after accepted runtime evidence
  max_silence_minutes: 20    # 0 = default (20); negative or enabled:false = disabled
  eval_interval_seconds: 60  # watchdog evaluation cadence
```

All three knobs are fleet-controllable (`maestro settings`):

- `stalled_progress_watchdog.enabled`
- `stalled_progress_watchdog.max_silence_minutes`
- `stalled_progress_watchdog.eval_interval_seconds`

Missing watchdog config is inactive. This is an upgrade-safety boundary: a new
binary never arms recovery fleet-wide for legacy projects. New hands-off
projects do not opt in automatically through Maestro's own genesis. External
lifecycle tooling may emit this nested stanza only after its accepted #897
evidence prerequisite is satisfied. A legacy-only
`worker_silent_timeout_minutes: N` is migrated in memory to enabled v1 with the
same `N`-minute budget; explicit `enabled: false` retains the legacy detector as
a compatibility escape hatch. Any other explicit v1 stanza suppresses legacy,
including `enabled: true` with a negative/invalid budget; it fails closed instead
of unexpectedly re-arming the terminal-only killer. The legacy and v1 detectors
never run together.

A disabled or negative budget collapses to a zero silence budget, which the
evaluator reads as **disabled** — no quiet worker is ever killed by a
misconfiguration.

## Recommendation semantics (the replay boundary)

When *no* signal for one exact target has advanced for the whole silence budget,
the evaluator records an idempotent recovery recommendation. Recommendation and
actuation are separate durable records. **The current v1 runtime is
recommendation-only: no recovery actuator is wired yet.** Fleet reports
`last_recommendation` for evaluator output and leaves `last_recovery` empty
unless an actual actuator attempt is explicitly persisted. The boundary depends
on the lifecycle phase:

- **implementation worker**: a proven safe stall recommends stopping the exact
  stale worker and retrying/resuming exactly once under the existing retry
  budget. It does not claim that stop/retry happened.
- **PR gate** (`pr_open` or `queued`): recommends an idempotent gate repair. It
  carries no process identity and does not claim a delivery replay boundary.
- **post-merge/live verification** (`code_landed`): recommends outcome repair,
  not worker/merge replay. It is not an uncertain executing-delivery result.
- **delivery approval pending**: surfaces the control-plane wait; execution has
  not begun, so `replay_boundary=false`.
- **delivery executing / uncertain**: recovery authority ends **before** the
  durable delivery lease. The watchdog **surfaces operator reconciliation and
  sets `replay_boundary=true`; it never replays the delivery automatically. Use the
  [delivery-reconciliation runbook](delivery-reconciliation-runbook.md) (#872).

Every recommendation decision records the observed signal set (by kind), the last
material watermark, the deadline, the phase, the idempotency/replay boundary,
and the action plus its no-op reason — never a secret, raw private path, or
command output.

## Durability

The per-target watermarks live in the project's `state.json`
(`material_progress.targets`) and survive daemon restart. Deadlines are never
stored — each is derived from `target.watermark.at + current_budget`, so reload
does not reset it. Concurrent writes merge per exact target; actual recovery
attempt/results are unioned independently from evaluator verdicts.

PR-gate evidence is first persisted separately in `pr_gate_snapshots`, keyed by
the exact project, issue, PR, current head SHA, and semantic generation. The
orchestrator advances that generation only for an actual head change,
CI/per-check rollup or effective-verdict transition, review decision or
actionable-finding generation, or immutable merge identity. The supervisor then
hashes the newest exact snapshot into `pr_review`. Notification dedup fields and
the Greptile pending/retrigger clock are intentionally excluded: changing either
without a forge transition does not refresh the watchdog watermark. Check/review
details are persisted only as bounded opaque digests; raw output, review text,
paths, URLs, and credentials never enter the snapshot.

Per-signal ages track when each signal last changed (not the evaluation time):
an unchanged signal carries its prior observation time forward, so operators can
see which specific signal stopped advancing even while the combined watermark is
still moving on another. A watchdog disabled in config reports a zero budget and
no deadline even when durable state still carries a previously-enabled budget, so
Fleet never raises a false overdue alert before the next evaluation.

## Promotion gate

The fingerprint-bound `multi-signal-progress-v1` contract may be published only
after runtime canary evidence. Maestro's own genesis does not currently add the
watchdog stanza or publish the contract automatically. Evidence-gated external
lifecycle tooling may emit the nested v1 config after the #897 prerequisite is
accepted; while pending it omits actuation and never emits the unsafe legacy
`worker_silent_timeout_minutes` key.

Fleet reports evaluator `mode` separately, while `contract` remains empty and
`contract_pending=true` until a durable canary-proof source exists (#896/#897).
Config, observations, recommendations, or a manually recorded recovery record
cannot self-assert the live capability. The missing bounded actuator and live
canaries are tracked in #896/#897; until they land, documentation and UI must
describe this implementation as recording/recommendation-only.

Two live canaries close the loop (tracked as runtime verification):

- **no-false-kill**: a quiet worker editing through at least one 20-minute
  deadline reaches a visible commit/PR without a restart.
- **recovery**: a deliberately stalled, harmless pre-delivery worker is detected
  within the budget, stopped once, respawned/resumed once, and reaches a visible
  PR/result without operator prompting.

No canary may expose or rotate real credentials; credential containment is
tracked separately in #888.
