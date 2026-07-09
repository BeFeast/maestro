# Emergency Stop — the fleet-wide BIG RED BUTTON

**First move during a money-burn incident: press the button.** Do not diagnose
which component is calling which backend first — halt the spend, *then*
investigate while nothing burns. One action stops all LLM calls fleet-wide in
seconds; it is persistent, works from the CLI or the dashboard, and survives a
`maestro.service` restart.

This runbook is safe for shared docs: it uses placeholders for local paths and
never requires printing tokens, environment variables, or raw config.

## What it does

The switch lives as one fleet-wide row in the unified `~/.maestro/maestro.db`
(table `emergency_stop`), so it is *not* a per-project pause — a single write
halts every project. There are two levels:

| Level | Command | Effect |
|---|---|---|
| **LLM stop** | `maestro emergency stop-llm` | Supervisor drops to **deterministic-only** (no supervisor backend calls), the orchestrator **spawns no new workers**, and router LLM calls stop. The daemon keeps running — dashboards, state, GitHub reads and watchdogs stay alive so you can watch while nothing spends. In-flight tmux workers are left running unless you add `--kill-workers`. |
| **Whole-fleet stop** | `maestro emergency stop-all` | Everything LLM stop does, recorded as an emergency state that survives a restart — the daemon comes back up in stopped mode until you explicitly resume. |

Both levels take effect **within one poll interval, without a daemon restart**:
the running daemon re-reads the switch every few seconds and every
supervise/orchestrate cycle checks it before any backend invocation.

## Press the button

```
# Halt all LLM spend fleet-wide (leaves in-flight workers running):
maestro emergency stop-llm --reason "supervise burn on metered backend"

# Same, but also kill in-flight tmux workers:
maestro emergency stop-llm --kill-workers --reason "..."

# Whole-fleet emergency stop (survives restart until resumed):
maestro emergency stop-all --reason "..."
```

The CLI writes the switch **directly to the DB**, so it works even if the web
dashboard or daemon is down:

```
maestro emergency stop-llm --db ~/.maestro/maestro.db --reason "..."
```

A `notify_red`-grade notification is sent on activation (and on resume). The
actor defaults to the current OS user; override with `--actor`.

### From the dashboard

Mission Control shows a prominent **red button** on the fleet page, gated by a
confirmation modal, and a persistent banner — *"EMERGENCY STOP active since …,
by …, reason …"* — on every page while active. Activation is **not**
approval-gated (an emergency stop must never wait in an approval queue). The red
button `POST`s to `/api/v1/fleet/emergency`, which writes the same switch the CLI
does; the running daemon picks it up on its next cycle.

## Confirm it took effect

- `maestro emergency status` prints the current switch (level, since, actor,
  reason).
- The fleet API reports it: `GET /api/v1/fleet` carries
  `"emergency": {"active": true, "level": "llm_stopped", ...}`.
- The daemon journal logs one confirmation line per flow, and the proxy log
  shows zero new supervise/router LLM requests from maestro within one poll
  interval.

## Resume

```
maestro emergency resume        # clears the switch; normal operation next cycle
```

`resume` accepts a `--llm` alias (there is a single fleet-wide switch, so resume
clears whatever level is set). No restart is needed — the orchestrator restores
issue selection and the supervisor restores its LLM path on the next cycle. A
`notify_red`-grade notification confirms the resume.

## Guarantees

- **Persistent:** the flag is in the unified DB, so it survives
  `sudo systemctl restart maestro.service`. A stop set while the daemon is down
  is honored on the next daemon start.
- **Fast:** takes effect within one poll interval — no restart, no config-store
  edit, no per-project surgery.
- **Fleet-wide:** one write halts all projects, unlike `maestro pause` (which is
  per-project, gates only issue selection, and must be invoked once per
  project).

## Supervisor LLM cost profile (baseline spend)

The emergency stop is the brake for a runaway burn; the everyday floor of
supervisor spend is set by how often the supervise loop actually calls its
backend. That floor is deliberately low:

- **Safe, mutation-free cycles do not call the LLM (#837).** When the
  deterministic guardrail already decides `action=none`, a `wait_*` action, or
  `monitor_open_pr` with `risk=safe` and no planned GitHub mutation, the
  supervisor records that decision and skips the backend entirely. It builds no
  state packet and makes no `Complete` call, so an idle project spends **zero**
  supervisor tokens per cycle. The skip is safe because the LLM is only ever
  allowed to *agree* with a `risk=safe` guardrail decision — any disagreement
  resolves back to the deterministic side — so the full-context call could only
  reword the summary, never change the outcome.
- **The LLM still runs where it adds value.** Mutating / approval-gated
  decisions (`spawn_worker`, `spawn_repair_worker`, `merge_pr`,
  `review_retry_exhausted`, and `label_issue_ready` when it plans a real label
  mutation) have `risk != safe` or carry planned mutations, so they take the
  full LLM path exactly as before.
- **Journal signal.** Each skipped cycle logs
  `supervise: safe idle decision, LLM skipped (action=… risk=…)`, so the saving
  is visible in `journalctl` and the proxy log shows no matching
  `session=<backend>:*` supervise request.

Why it matters: before #837 every enabled cycle sent a ~55K-token state packet,
so nine idle fleet projects on a 180s poll interval issued ≈180 LLM calls/hour
that almost all decided "do nothing" — the exact shape of the 2026-07-09 metered
backend incident. The short-circuit removes that baseline; the emergency stop
remains the fast brake for anything the baseline does not cover.

**Escape hatch.** Set `supervisor.always_consult_llm: true` (default `false`) to
restore the pre-#837 behavior and force a full-context LLM call on every enabled
cycle — use it only when you specifically want a second opinion on idle
decisions and accept the token cost.

## Relationship to `pause` / `drain`

`maestro pause` and `maestro drain` remain per-project operator controls that
gate issue selection but do **not** stop the supervisor LLM path. The emergency
stop is the fleet-wide, spend-halting brake — deliberately its own fast path,
not a normal setting. Reach for it first during a burn; use pause/drain for
routine, per-project pausing.
