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
| `supervisor.enabled` | bool | LLM supervisor on/off |
| `supervisor.backend` | string | supervisor LLM backend |
| `supervisor.model` | string | supervisor model |
| `supervisor.effort` | string | supervisor reasoning effort |
| `supervisor.allow_metered_backend` | bool | allow a per-token supervisor backend (#838) |
| `supervisor.always_consult_llm` | bool | force an LLM call every cycle (#837) |
| `poll_interval_seconds` | int | supervise/orchestrate poll cadence |
| `worker_max_tokens` | int | kill a worker over this token budget (0 = unlimited) |

## Commands

```
# Flip the LLM supervisor OFF fleet-wide (all projects without an override):
maestro settings set supervisor.enabled=false --actor incident-responder

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

## Hot reload — no restart

Writes use the same hot-reload path as `config-store` edits. A `maestro daemon
--watch-store` process re-reads the store every tick; a fleet-level change
advances **every** project's fingerprint (it comes from the append-only
`settings_audit` journal, so even *clearing* the last fleet default forces the
affected projects to reload back to the builtin value), so
`maestro settings set supervisor.enabled=false` takes effect on the next cycle
across all projects without a restart.

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
