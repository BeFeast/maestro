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
- `attention_code`: `tmpfs_pressure` when post-sweep `use_pct >= 85`.

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

`proc_scan_errors` records process entries that disappeared or could not be
read during collection. A non-root daemon only removes entries owned by its own
uid; a root-run installation can inspect and sweep all owners.

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
    "pressure": true,
    "attention_code": "tmpfs_pressure"
  }
}
```

If `/tmp` is not tmpfs, or the configured protection paths cannot be loaded,
the sweep fails closed, deletes nothing, emits an `error` field, and preserves
that failed result for Fleet inspection.

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
   forced or naturally observed post-sweep utilization of at least 85% reports
   `attention_code: tmpfs_pressure`.
5. Investigate persistent `scan_error`, `mount_boundary`, or high
   `proc_scan_errors` counts before broadening policy. Do not add a catch-all
   pattern or a blanket `/tmp` removal.

Changing tmpfs size, worktree GC, distributed runner cleanup, and producer-side
ok-player fixes are intentionally outside this runbook.
