# Fleet Settings — cost/LLM control knobs

A first-class operator surface for the cost-control knobs that used to be buried
as per-project YAML fields. During the 2026-07-09 token-burn incident the P0
mitigation ("turn off the LLM supervisor on idle projects") required a scripted
export → edit → re-import cycle across five project rows. `maestro settings`
turns that into one command, adds a **fleet-wide default** layer, and journals
every change (#839).

This runbook is safe for shared docs: it uses placeholders for local paths and
never requires printing tokens, environment variables, or raw config.

## The three layers

A knob resolves through three layers, most specific wins:

1. **project** — the value in a project's own config (its config-store row / YAML).
2. **fleet** — a default in the config store's `settings` table (new in #839).
3. **builtin** — the compiled-in default.

So a fleet default applies to **every project that does not set the key itself**,
and any project can still pin its own value. The fleet API's `effective_config`
reports each knob's value and its `source` (`builtin` / `fleet` / `project`), and
Mission Control's Settings panel highlights the non-default (`fleet`/`project`)
overrides.

## Controllable keys

| Key | Kind | Meaning |
|---|---|---|
| `model.provider_lanes` | JSON/YAML array | ordered provider defaults and provider-local fallback backends (#909) |
| `supervisor.enabled` | bool | LLM supervisor on/off |
| `supervisor.backend` | string | supervisor LLM backend |
| `supervisor.model` | string | supervisor model |
| `supervisor.effort` | string | supervisor reasoning effort |
| `supervisor.allow_metered_backend` | bool | allow a per-token supervisor backend (#838) |
| `supervisor.always_consult_llm` | bool | force an LLM call every cycle (#837) |
| `supervisor.unchanged_decision_window_seconds` | int | roll up identical recommendation journal lines (default 3600) |
| `supervisor.recommendation_ttl_seconds` | int | drop unconsumed recommendations with a disposition after this age (default 86400) |
| `poll_interval_seconds` | int | supervise/orchestrate poll cadence |
| `worker_max_tokens` | int | enforce a per-attempt live token ceiling (0 = unlimited) |

## Commands

```
# Flip the LLM supervisor OFF fleet-wide (all projects without an override):
maestro settings set supervisor.enabled=false --actor incident-responder

# Apply provider defaults fleet-wide. Quote the JSON so the shell passes it as
# one key=value argument:
maestro settings set 'model.provider_lanes=[{"provider":"anthropic","default":"claude"},{"provider":"openai","default":"sol","fallback_backends":["gpt55"]}]'

# Pin one project back ON despite the fleet default:
maestro settings set supervisor.enabled=true --project <row>

# What is actually in effect for a project, and from which layer:
maestro settings list --project <row>

# Just the fleet defaults:
maestro settings list

# One key:
maestro settings get worker_max_tokens --project <row>

# Clear a fleet default (projects without an override revert to the builtin):
maestro settings rm supervisor.enabled

# Clear a per-project override (revert to the fleet default, else the builtin):
maestro settings rm supervisor.enabled --project <row>

# Who changed what, when, old -> new:
maestro settings audit
```

Flags: `--db <path>` (default `~/.maestro/maestro.db`), `--actor <name>` (default
`cli:<user>`), and `--limit N` on `audit`. Flags may appear before or after the
`key`/`key=value` argument.

## Live token ceiling

`worker_max_tokens` is enforced in the worker stream, independently of the
normal orchestration poll interval. Claude, Pi, and OpenCode usage is evaluated
after each usage-bearing provider response. Codex's emitted thread id is used to
read its live cumulative `token_count` rollout events (including cached input),
and the same value is also installed as Codex's native `rollout_budget` fallback
inside the agent loop, including sub-agent work.

For Claude and Pi the ceiling measure is **uncached tokens**: input + output +
new cache-write tokens. Cache reads remain visible in cost/usage telemetry but
are excluded from the ceiling because they replay previously produced context;
counting the full cached context on every turn can kill a healthy worker after
only a few new tokens. Repeated Claude stream frames for the same assistant
message are de-duplicated by message id. Token-budget markers and Fleet worker
rows expose `token_budget_measure: uncached_tokens`; total session/cost counters
continue to retain the provider's full cache-aware usage.

The measurement lag and maximum overshoot are therefore **one provider
response**, plus the time needed to flush one JSONL line. There is no additional
minutes-long Maestro poll delay. A single provider response can contain a
variable number of tokens, so the bound is expressed in response units rather
than a fixed token constant; the regression suite verifies later responses are
not consumed after the first event reaches the ceiling.

The ceiling fails closed when Maestro cannot enforce it live:

- Claude and Codex require `usage_stream: true` with Maestro-managed structured
  output; Codex `--ephemeral` mode is rejected because it removes the live
  rollout telemetry file.
- Pi must retain Maestro-managed JSON mode.
- OpenCode requires `usage_stream: true` with Maestro-managed JSON output.
- Kimi's print-mode stream is captured for token/cost accounting but is not
  currently accepted as a reliable live ceiling, so Kimi is rejected while a
  positive `worker_max_tokens` is active.
- Gemini, Cline, generic CLIs, and operator-overridden non-structured output
  cannot start while a positive `worker_max_tokens` is active.

Codex's rollout file supplies the same cumulative total shown in Mission
Control. Its native fallback counts sampled output plus non-cached input at
weight 1.0 and prevents an older or temporarily unreadable rollout path from
degrading all the way back to post-exit-only enforcement.

## Hot reload — no restart

Writes use the same hot-reload path as `config-store` edits. A `maestro daemon
--watch-store` process re-reads the store every tick; a fleet-level change
advances **every** project's fingerprint (it comes from the append-only
`settings_audit` journal, so even *clearing* the last fleet default forces the
affected projects to reload back to the builtin value), so
`maestro settings set supervisor.enabled=false` takes effect on the next cycle
across all projects without a restart.

`model.provider_lanes` uses that same path. The orchestrator updates its live
router and fallback selector when the store watcher delivers the new config;
the Fleet API and supervisor holder do not merely display a newer
`effective_config` while dispatch continues using the old route.

## Provider-lane migration

Provider lanes compose in declaration order. Within a lane the provider default
is tried first, then that provider's local fallbacks, before Maestro advances to
the next provider. For example, `anthropic/claude` followed by
`openai/sol,gpt55` resolves to `claude -> sol -> gpt55`.
Every referenced backend must be declared in each affected project's
`model.backends`; an invalid fleet route is rejected when that project's
effective config is loaded rather than synthesizing an unconfigured backend.

An existing project-level `model.fallback_backends` remains an explicit legacy
route override and takes precedence over a fleet `model.provider_lanes` default.
Remove that project chain only after confirming the provider-lane route shown in
Mission Control is the intended replacement. A project can instead set its own
`model.provider_lanes` value, which is recorded as source `project` and wins over
the fleet value. When neither an explicit chain nor provider lanes exist,
Maestro uses only `model.default`; it never derives fallback order from the
alphabetical order of `model.backends`.

## Audit trail

Every change — fleet or per-project, set or clear — appends a row to
`settings_audit` with the key, scope (`fleet` or `project:<name>`), old → new
value, actor, and an RFC3339 timestamp. `maestro settings audit` reads it back.
The 2026-07-09 incident's 06:56 backend flip was only attributable via DB backup
diffing and journalctl archaeology; this journal makes that a one-liner.

## Mission Control

The Settings panel shows each project's effective knob values with a source
badge and highlights non-default overrides. Writing config from the dashboard is
approval-gated: "Request edit" enqueues a `change_global_config` approval (a
reason is required) rather than mutating config directly — the cautious approval
flow, same as any other high-risk fleet action.

## Export / import round-trip

`maestro config-store export` writes the fleet settings layer alongside the
project YAMLs as `_fleet-settings.yaml`; `config-store migrate` (import) reads it
back into the `settings` table. Unknown or ill-typed keys in a hand-edited export
are rejected, so a bad file cannot poison the layer.
