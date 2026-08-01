# `/tmp` tmpfs hygiene

Maestro owns a conservative cleanup path for fleet hosts whose `/tmp` is a
tmpfs. It never performs a blanket `/tmp` deletion. The single
`maestro.service` daemon runs one host-level apply sweep every 10 minutes and
publishes the latest result as `tmpfs_hygiene` on `GET /api/v1/fleet`.

The sweeper refuses to run when `/tmp` is not tmpfs. It also reads every
configured project's `local_path` and `worktree_base` from the normal Maestro
config store before each CLI or scheduled run; a candidate that overlaps one
of those paths is protected.

## Dry-run and apply

The config store defaults to the unified Maestro database, so the normal
operator commands are:

```bash
maestro tmpfs-hygiene --dry-run
maestro tmpfs-hygiene --apply
```

Use `--store <path-to-maestro.db>` only when the daemon itself uses a
non-default store. Exactly one of `--dry-run` or `--apply` is required.

Each valid-mode sweep attempt emits exactly one JSON line, including a failed
mount or config-protection check. A daemon tick emits the same shape to the
service journal. Important fields are:

- `reclaimable_bytes`: bytes eligible after every protection and age gate;
- `freed_bytes`: bytes successfully removed by an apply run (zero on dry-run);
- `partial_entries`: candidates where recursive removal reclaimed some content
  before failing; the remaining object is restored when its public name is free;
- `categories`: candidate/protected/deleted/partial/byte counts by policy category;
- `protect_hits`: counts for cwd, fd, cmdline, age, configured-path, git,
  socket/lock, ownership, symlink, scan, and filesystem-boundary guards;
- `use_pct`: tmpfs utilization after the sweep;
- `total_bytes` / `available_bytes`: the post-sweep capacity the pressure
  verdict was made on;
- `attention_code`: `tmpfs_pressure` when post-sweep `available_bytes` falls
  below `pressure_floor_bytes` (default 8GiB). This is an absolute free-byte
  budget, not a share of the mount (#1128): 85% of a RAM-backed tmpfs says
  nothing about how close the host is to running out of memory.

Example inspection without printing unrelated service logs:

```bash
maestro tmpfs-hygiene --dry-run | jq .
curl -fsS http://127.0.0.1:8786/api/v1/fleet |
  jq '.tmpfs_hygiene | {timestamp, use_pct, attention_code, freed_bytes, categories, protect_hits}'
```

## Compiled allowlist and age gates

Patterns match direct children of `/tmp` only. Anything not listed is ignored.
The newest mtime anywhere in a candidate tree controls its age gate.

| Category | Top-level patterns | Minimum age |
|---|---|---:|
| Outcome snapshots | `tmp.*` | 1 hour |
| Browser profiles | `playwright-*`, `playwright_chromiumdev_profile-*`, `chrome-profile-*`, `.org.chromium.Chromium.*` | 2 hours |
| Worker scratch | `maestro-*`, `claude-*`, `codex-*`, `opencode-*` | 6 hours |
| Tooling caches | `go-build*`, `node-compile-cache*`, `npm-*`, `yarn-*`, `pnpm-*`, `uv-*` | 12 hours |

Changing this table is a code-and-test policy change, not an operator glob.

## Protection rules

An allowlisted candidate is kept when any of these rules matches:

- its path appears in any process cwd, open-fd target, or command line under
  `/proc`;
- its top-level name is a socket/lock name or one of the X11, ICE, XIM, dbus,
  ssh-agent, or `systemd-private` infrastructure forms;
- its newest tree mtime has not crossed the category age gate;
- it overlaps a configured `local_path` or `worktree_base`;
- it contains a `.git` entry;
- it belongs to another uid;
- it is a top-level symlink, cannot be inspected completely, or crosses a
  filesystem boundary.

Nested symlinks inside an otherwise eligible abandoned tree are unlinked as
symlinks. Deletion uses no-follow, file-descriptor-relative operations rooted
at the already-open tmpfs directory, so neither a symlink nor a concurrent
symlink swap can redirect removal outside `/tmp`.

`proc_scan_errors` records `/proc` reads that failed during collection and
`proc_unresolved_processes` counts the processes behind them — typically
`systemd --user`, `(sd-pam)` and `ssh-agent`, which run under the daemon's own
uid with `PR_SET_DUMPABLE` cleared and are therefore present on every tick. A
failed read protects only the candidates that one process demonstrably
references (`protect_hits.proc_scan_error`); it is never a verdict on unrelated
candidates. Charging it to the whole sweep made the sweeper a permanent no-op
with `protected_entries == matched_entries` and `reclaimable_bytes: 0` (#1125).
A non-root daemon only removes entries owned by its own uid; a root-run
installation can inspect and sweep all owners.

A sweep that protects every candidate it matched and reclaims nothing reports
`sweep_ineffective: true`, attention code `tmpfs_sweep_ineffective` when nothing
more urgent is pending, and a `SUSPICIOUS` daemon log line. Treat it as a broken
reaper until proven otherwise: in the metric it is indistinguishable from a
clean `/tmp`.

## Scheduling and Fleet pressure

No extra per-project unit, legacy `maestro.d` file, or `fleet.yaml` is used.
The schedule lives in the existing single daemon process. Its default is 10
minutes and can be changed on that daemon invocation with
`--tmpfs-hygiene-interval`, keeping the fleet-host value within 5–10 minutes.

After every apply, Fleet exposes the exact post-sweep state:

```json
{
  "tmpfs_hygiene": {
    "mode": "apply",
    "tmpfs": true,
    "use_pct": 87,
    "available_bytes": 2147483648,
    "pressure_floor_bytes": 8589934592,
    "pressure": true,
    "attention_code": "tmpfs_pressure"
  }
}
```

If `/tmp` is not tmpfs, or the configured protection paths cannot be loaded,
the sweep fails closed, deletes nothing, emits an `error` field, and preserves
that failed result for Fleet inspection.

## Pressure alert and spawn precondition (#1128)

The pressure signal does not depend on a sweep. The daemon samples the tmpfs
root every `--tmpfs-pressure-interval` (default 30s) and publishes the result as
`/api/v1/fleet.tmpfs_pressure`, so the signal exists even when the sweeper is
refused or reclaims nothing:

```json
{
  "tmpfs_pressure": {
    "root": "/tmp",
    "total_bytes": 16686817280,
    "available_bytes": 2147483648,
    "use_pct": 87,
    "pressure_floor_bytes": 8589934592,
    "spawn_floor_bytes": 4294967296,
    "pressure": true,
    "spawn_hold": true,
    "held_spawns": 3
  }
}
```

Two independent absolute floors act on that sample:

- `--tmpfs-pressure-floor-bytes` (default 8GiB) — two consecutive samples below
  it send an ntfy CRITICAL of class `tmpfs_pressure` at priority 5, one page per
  episode, cleared on recovery. Negative disables the alert.
- `--tmpfs-spawn-floor-bytes` (default 4GiB) — below it the orchestrator skips
  its dispatch cycle before listing issues, logging the reason and counting the
  skip in `held_spawns`. Negative disables the pause.

The spawn precondition is a **throughput pause, never a freeze**: it persists no
state, requires no approval, produces no `ActionNone` decision, and spends no
issue retry budget. Every poll re-evaluates it, so dispatch resumes on its own
as soon as space is reclaimed. It also fails open — a missing or failed sample
lets dispatch through, because a measurement gap must not park the fleet.

## 24-hour dogfood verification

Deployment/runtime verification remains an operator step after the code lands:

1. Run one CLI dry-run and confirm the candidates and protection counts are
   plausible for the host.
2. Let the normal daemon schedule run for a full day under representative fleet
   load; do not manually prune during the observation window.
3. Inspect the JSONL sweep records and confirm old `outcome_snapshot`,
   `browser_profile`, `worker_scratch`, and `tooling_cache` candidates are
   deleted after their age gates rather than increasing monotonically.
4. Confirm `/api/v1/fleet.tmpfs_hygiene` advances every interval and that a
   forced or naturally observed post-sweep `available_bytes` below the
   pressure floor reports `attention_code: tmpfs_pressure`. Confirm
   `/api/v1/fleet.tmpfs_pressure` advances on its own 30s cadence even when a
   sweep deleted nothing, and that crossing the spawn floor raises
   `spawn_hold` with a rising `held_spawns` rather than any stuck state.
5. Investigate persistent `scan_error`, `mount_boundary`, or high
   `proc_scan_errors` counts before broadening policy. Do not add a catch-all
   pattern or a blanket `/tmp` removal.
6. Treat any `sweep_ineffective: true` record as a defect report, not as a
   quiet tick.

Changing tmpfs size, worktree GC, distributed runner cleanup, and producer-side
ok-player fixes are intentionally outside this runbook.
