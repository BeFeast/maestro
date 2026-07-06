# Mirror Reconciliation & Health Runbook (#827, epic #811 phase E)

Phases B–D landed inbound webhook ingestion (#824), the normalised read-model
mirror (#825), and the mirror-first read source (#826). Phase E **closes the
loop**: a low-frequency reconciliation loop detects and repairs mirror drift a
missed webhook left behind, and the whole pipeline is observable end-to-end.

Webhooks get missed — endpoint downtime, delivery failures, events GitHub does
not emit. A mirror without reconciliation silently drifts from GitHub truth, and
mirror-first reads would then act on wrong state. The reconciliation loop is the
safety net that keeps the mirror honest **without operator action**.

## What the loop does

For each project, on a low cadence, the daemon takes an authoritative snapshot of
the repo's open issues and open PRs and compares it to the mirror:

- an issue/PR GitHub reports **open** that the mirror is **missing or has recorded
  differently** (stale title/labels/head-branch) is refreshed;
- an issue/PR the mirror still records **open** that GitHub **no longer lists** —
  the missed `closed`/`merged` delivery — is marked closed (a dropped PR's
  authoritative `merged` flag is fetched so a merge is never fabricated or lost);
- rows that already match GitHub are **left untouched**, so an up-to-date mirror
  does no writes and keeps its webhook-sourced `last_seen_at`.

Every row the loop had to correct is counted as a **drift repair**. A nonzero
repair count is the visible sign a webhook gap was healed.

Repaired rows are written with `source = "reconcile"` (vs `webhook` / `api`), so a
reader can tell pushed state from state a snapshot had to repair.

### The snapshot is the complete open set

The snapshot reads follow pagination (`ListAllOpenIssues`, `ListAllOpenPRs` —
gh's `--paginate`), so the loop sees GitHub's **entire** open set, not just the
first 100. This matters because absence from the snapshot is the close signal: if
the loop only read page one, every still-open issue/PR past #100 would be missing
from that page and wrongly stamped closed. Paginating before using absence keeps
a large repo's mirror correct.

### Cheap when nothing changed

Those paginated reads still flow through the gh wrapper's conditional (ETag /
`If-None-Match`) layer (#797). A repo whose open lists fit a partial page and have
not changed since the last pass answers **304 Not Modified** and consumes
**near-zero core quota**; the reconciler then writes nothing. The per-PR
`IsPRMerged` call is made only for a PR that dropped out of the open set (a rare
per-drift call, not a per-cycle one). The loop also skips a pass while the shared
primary rate-limit gate is armed (#812), since the reads would only fail against
an empty budget.

If a dropped PR's `IsPRMerged` lookup keeps failing (e.g. a persistent 404), the
loop does **not** guess a `merged` flag and does **not** silently skip it: the
error is recorded, the pass is counted as a **failure**, and `reconcile[].last_error`
names the stuck PR — so a non-converging row is visible rather than quietly stale.
A transient error simply clears on the next pass.

## Enabling it & cadence

The loop rides the same wiring as the mirror: it runs whenever the daemon has an
open mirror — i.e. webhook ingestion is configured (`--webhook-secret-file`, see
the [webhook ingestion runbook](webhook-ingestion-runbook.md)). It runs **even
with mirror-first reads still off**, keeping the read model correct and warm for
the moment an operator flips `github_mirror.source: mirror-first`.

Cadence is per project, in config (or the config store):

```yaml
github_mirror:
  source: mirror-first     # reconciliation runs regardless; this only gates reads
  stale_seconds: 86400     # read freshness horizon
  reconcile_seconds: 900   # reconciliation cadence; 0 = default 15m
```

The cadence is read **live** from the flow's config holder, so a config-store edit
to `reconcile_seconds` retimes the loop without a restart.

## Checking mirror health

`GET /api/v1/fleet` carries a `mirror` block with the health picture:

```jsonc
"mirror": {
  "enabled": true,
  "stale_horizon": "24h0m0s",
  "counts": { "issues": 12, "pull_requests": 4, ... },
  "stale":  { "issues": 1, ... },
  "total_rows": 66,
  "total_stale": 1,
  "drift_repairs": 3,                       // fleet-wide rows repaired since start
  "reconcile": [
    { "repo": "owner/repo", "last_run_at": "...", "last_success_at": "...",
      "runs": 40, "failures": 0, "repairs": 3, "last_repairs": 0 }
  ],
  "reads": { "mirror_hits": 5400, "api_fallbacks": 240, "hit_rate": 0.957 }
}
```

Read it as:

- **`total_stale > 0`** — that many rows have not been refreshed within the
  horizon; usually deliveries stopped arriving for a repo. The reconcile loop
  should be repairing these — watch `drift_repairs` climb.
- **`reconcile[].last_success_at`** — when the loop last completed a pass for that
  repo. If it stops advancing, the loop is failing (see `last_error`) or the flow
  is down.
- **`reconcile[].repairs`** — cumulative drift healed for that repo; **`last_repairs`**
  is the most recent pass. A steady stream of repairs means webhooks are being
  missed and the loop is compensating; investigate the delivery path.
- **`reconcile[].failures` / `last_error`** — a pass that could not settle a
  dropped PR's merged state (or hit a GitHub read error) increments failures and
  records the reason; a climbing failure count on one repo is a stuck row to chase.
- **`reads.api_fallbacks`** — reads that could not be served locally and fell back
  to the GitHub API (the fallback-to-GitHub count).

The same numbers land in the **journal** (see below), so an operator can check
health straight from `journalctl` without the HTTP endpoint.

### Journal

The gh wrapper's hourly REST-usage line carries the mirror read + reconcile
totals:

```
[github] REST usage last 1h0m: 812 requests, ~120 billed against core quota,
  692 served free by 304, 0 rate-limited; mirror reads: 5400 served locally,
  240 fell back to API (96% hit); mirror reconcile: 40 pass(es), 3 drift
  repair(s), last 2026-07-06T09:14:03Z
```

Each pass that repairs drift also logs a line at the moment it happens:

```
[owner/repo] mirror reconcile repaired 2 row(s) (issues=1 prs=1) — drift healed
  without operator action
```

The morning digest (`maestro digest`) carries a `Mirror reconcile:` header line
and a `reconcile` block in its JSON (`repos`, `runs`, `failures`, `drift_repairs`,
`last_reconcile`).

## Forcing a full reconcile

There is no separate manual-trigger endpoint — reconciliation is a background loop
by design. To force a fresh pass:

1. **Lower the cadence temporarily.** Set `github_mirror.reconcile_seconds` to a
   small value (e.g. `60`) via the config store; the loop picks it up live on its
   next tick and reconciles every minute. Restore the normal value afterwards.
2. **Restart the flow.** `rm` + re-`add` the project in the config store (the
   daemon's diff-loop treats it as a fresh flow) — the reconcile loop starts and
   takes its first pass after one cadence interval.

A pass is idempotent and safe to run as often as you like: it only writes on a
divergence, and unchanged reads cost 304s.

## Verifying convergence after a webhook outage (AC 1)

1. Note `mirror.drift_repairs` and a target issue/PR's mirror state.
2. Stop webhook delivery (disable the endpoint / point GitHub elsewhere) for a few
   minutes and close/relabel the issue on GitHub so the delivery is missed.
3. Restore delivery. Within one reconcile cadence, `drift_repairs` increments and
   the mirror row converges to GitHub truth — with **no operator action** beyond
   restoring the endpoint. The `[repo] mirror reconcile repaired …` journal line
   records the healed row.

## 60-minute runtime soak (epic #811 acceptance)

Phase E's acceptance includes a **60-minute fleet soak on the runtime host**
(webhooks live, mirror-first reads on, all projects) with API usage recorded
before/after and posted to epic #811. That is an **operator task on the runtime
host**, not something the code change performs. Procedure:

1. Baseline: `gh api rate_limit` and the current `[github] REST usage` hourly line;
   note requests/hr.
2. Enable `github_mirror.source: mirror-first` fleet-wide and confirm the mirror is
   warm (`mirror.total_rows` > 0, `total_stale` low).
3. Let the fleet run 60 minutes under normal load.
4. Record the after picture: requests/hr from the hourly line, `reads.hit_rate`,
   `drift_repairs`. Post before/after to epic #811.

A high `reads.hit_rate` with a low `api_fallbacks`, plus a low steady
`drift_repairs`, is the phase-E success signal: the mirror carries the read load,
and the reconciliation loop keeps it correct.

## See also

- [github-mirror-read-model-runbook.md](github-mirror-read-model-runbook.md) —
  what the mirror stores and its staleness model.
- [mirror-first-source-runbook.md](mirror-first-source-runbook.md) — which reads
  are served mirror-first and the escape hatch.
- [webhook-ingestion-runbook.md](webhook-ingestion-runbook.md) — the delivery path
  the mirror rides on.
