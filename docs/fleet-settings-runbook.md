# Fleet Settings — cost-control knobs, fleet-wide or per-project

**Turn the LLM supervisor off on idle projects with one command, without editing
nine YAML rows.** The 2026-07-09 token-burn incident's P0 mitigation ("turn off
the LLM supervisor on idle projects") took a scripted export → edit → re-import
cycle across five project rows. `maestro settings` collapses that into a single
write, applied fleet-wide or to one project, that hot-reloads on the next cycle
and is journaled for attribution.

This runbook is safe for shared docs: it uses placeholders for local paths and
never requires printing tokens, environment variables, or raw config.

## The knobs

These cost/LLM controls used to live only as per-project fields in each config
row. They are now *fleet-settable* — a fleet-level default the projects inherit:

| Key | Type | Meaning |
|---|---|---|
| `supervisor.enabled` | bool | LLM supervisor on/off |
| `supervisor.backend` | string | supervisor LLM backend name |
| `supervisor.model` | string | supervisor LLM model |
| `supervisor.effort` | string | supervisor LLM effort (low/medium/high/xhigh) |
| `supervisor.allow_metered_backend` | bool | opt the supervisor LLM loop into a metered (per-token) backend (#838) |
| `supervisor.always_consult_llm` | bool | always call the LLM even on safe deterministic decisions (pre-#837) |
| `poll_interval_seconds` | int | supervisor/orchestrator poll interval override (0 = use CLI flag) |
| `worker_max_tokens` | int | kill a worker over this token threshold (0 = unlimited) |

`maestro settings list --keys` prints this table from the running binary.

## Precedence

A knob's effective value is resolved with a strict, three-layer precedence:

```
per-project value  >  fleet default  >  built-in (config default)
```

- A **per-project value** is a key set in that project's config row. It always
  wins — a fleet default never overrides a project that spelled the key out.
- A **fleet default** (the `settings` table in the config store) applies to
  every project that does *not* set the key itself.
- The **built-in** is the value `config.parse` uses when neither layer sets it.

The fleet API's `effective_config.cost_controls` reports each knob's resolved
value **and its source** (`builtin` / `fleet` / `project`), so Mission Control
can highlight non-default overrides.

## Flip a knob fleet-wide

```
# Turn the LLM supervisor OFF for every project that hasn't set it itself:
maestro settings set supervisor.enabled=false

# Bound worker token spend fleet-wide:
maestro settings set worker_max_tokens=400000

# Clear a fleet default (projects without an override revert to built-in):
maestro settings set supervisor.enabled= --unset
```

The write lands in the config store (`~/.maestro/config.db` by default; pass
`--db <path>` to target another). A running `maestro daemon --watch-store`
picks it up **on its next poll, without a restart** — a fleet default change
advances every store-backed project's fingerprint, so all flows reload.

## Flip a knob for one project

```
# Override just this project (beats the fleet default going forward):
maestro settings set --project <row> supervisor.enabled=true
```

A per-project override is written into that project's config row, so it survives
export/import and shows as `source: project` in the effective config.

## See what's effective

```
# Fleet-level defaults (unset keys show "(unset)"):
maestro settings list

# One project's effective values with provenance:
maestro settings list --project <row>

# A single key:
maestro settings get supervisor.enabled --project <row>
```

## Attribution (audit trail)

Every change — fleet or per-project — is journaled in the config store with the
key, scope (`fleet` or the project name), old→new values, an actor string, and
an RFC3339 timestamp. The 06:56 backend flip in the 2026-07-09 incident was only
attributable via DB-backup diffing and journalctl archaeology; now it is one
query:

```
maestro settings audit            # most-recent first
maestro settings audit --limit 0  # the whole journal
```

Pass `--actor <who>` on a `set` to record who made the change; it defaults to
`$USER (cli)`.

## Round-trip / backup

`maestro config-store export` writes the fleet-settings layer to a standalone
`_fleet-settings.yaml` alongside the per-project rows, and `config-store
migrate` (import) restores it. The layer is *not* baked into the per-project
files, so the builtin/fleet/project distinction survives a round-trip.

## Mission Control

Mission Control surfaces `effective_config.cost_controls` per project, flagging
each knob's source. Writing settings from the dashboard is a
`change_global_config`-class action and is gated through the cautious approval
flow — the same gate that guards other global-config changes — so a dashboard
flip requires an approval, while the CLI (an operator on the host) writes
directly.
