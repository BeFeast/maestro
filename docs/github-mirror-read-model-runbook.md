# GitHub Mirror Read Model Runbook (#825, epic #811 phase C)

A normalised, queryable mirror of GitHub state in the unified SQLite store
(`~/.maestro/maestro.db`). Phase B (#824) lands **raw** webhook deliveries; this
phase projects them into typed tables — issues, pull requests, labels,
comments, reviews, check/CI status, and Projects v2 items — so a later phase can
read GitHub state locally instead of polling the API on every cycle.

This phase **builds and populates** the mirror only. **No read path switches over
to it yet** — that is phase D. Today's polling behaviour is unchanged; the mirror
runs alongside it and is observable through diagnostics.

## What lands

For each accepted webhook delivery the ingestor projects the modelled events into
`mirror_*` tables (`internal/mirrorstore`):

| Table | Keyed on | Filled from |
|---|---|---|
| `mirror_issues` | repo, number | `issues` |
| `mirror_pull_requests` | repo, number | `pull_request` |
| `mirror_labels` | repo, subject, name | issue / PR label sets |
| `mirror_comments` | repo, comment id | `issue_comment`, `pull_request_review_comment` |
| `mirror_reviews` | repo, review id | `pull_request_review` |
| `mirror_checks` | repo, head sha, name | `check_run`, `status` |
| `mirror_project_items` | repo, item id | `projects_v2_item` |

Every row carries `last_seen_at` (the source resource's own timestamp) and
`source` (`webhook` or `api`).

## Three guarantees

- **Ordering-safe.** An upsert applies only when the incoming `last_seen_at` is
  `>=` the stored one. An out-of-order or duplicate redelivery never regresses a
  row; GitHub retries and operator replays are idempotent.
- **Explicit staleness.** A reader classifies any row as **fresh / stale /
  missing** against a configurable horizon (`mirrorstore.Classify`,
  default `mirrorstore.DefaultStaleHorizon` = 24h). Diagnostics expose per-entity
  stale counts.
- **Hydration on miss.** A reader can fetch a missing entity via the existing
  `internal/github` client, record it (`source = "api"`), and return — so the
  mirror converges to coverage without a bulk backfill. The next read is local.

## Enabling it

The mirror rides the webhook ingestion path: start the daemon with
`--webhook-secret-file` (see the [webhook ingestion
runbook](webhook-ingestion-runbook.md)). When ingestion comes up, the daemon also
opens the mirror in the same `maestro.db` and projects each accepted delivery
into it. No separate flag or migration step — the schema is added idempotently on
start, so an existing fleet upgrades with no manual action.

If the mirror store cannot open, ingestion still lands raw deliveries (phase B
behaviour); only the read model is skipped, with a warning in the journal.

## Observing it

The fleet snapshot (`GET /api/v1/fleet`) carries a `mirror` block when the mirror
is configured:

```jsonc
"mirror": {
  "enabled": true,
  "stale_horizon": "24h0m0s",
  "counts": { "issues": 12, "pull_requests": 4, "labels": 20, "comments": 8,
              "reviews": 3, "checks": 15, "project_items": 4 },
  "stale":  { "issues": 1, ... },
  "total_rows": 66,
  "total_stale": 1
}
```

`total_stale > 0` means that many mirrored rows have not been refreshed within
the horizon — usually a sign deliveries stopped arriving for a repo. The block is
omitted entirely when the mirror is not configured.

Phase E (#827) adds `drift_repairs`, a per-repo `reconcile` array (last
run/success time and drift-repair counts), and a `reads` sub-block (mirror
hits / API fallbacks) to this block — see the [mirror reconciliation &
health runbook](mirror-reconciliation-runbook.md) for how to read them, force a
full reconcile, and run the soak.

## Phase D — reads switch over

Phase D (#826) puts the supervisor/orchestrator read paths behind a mirror-first
source (`mirrorstore.Source`): the open-issue/open-PR lists and issue/PR state
reads are served from a warm mirror and fall back to the API on a miss/stale.
It is opt-in per project via `github_mirror.source: mirror-first`, with `source:
api` as the fleet-wide escape hatch. See
[mirror-first-source-runbook.md](mirror-first-source-runbook.md).

## Notes / limits (phase C)

- Hydration is implemented for **issues** (`Hydrator.Issue`); other entity types
  are populated by projection. PR/other hydration follows the identical
  miss → fetch → store shape and is added as readers need it in phase D.
- `projects_v2_item` status is best-effort: the webhook carries the new value
  only on an `edited` delivery (`changes.field_value.to.name`); otherwise the row
  records item presence and `updated_at` with an empty status.
- `check_suite` roll-ups and the repo-level `label` **definition** event are not
  projected (the raw deliveries still land in `webhook_deliveries`, so coverage
  can widen later without re-ingesting).
