# Mirror-first Source Runbook (#826, epic #811 phase D)

Phase C (#825) built and populated the GitHub read-model mirror in
`~/.maestro/maestro.db`. Phase D puts the supervisor/orchestrator **read paths**
behind a mirror-first source: the high-volume per-cycle reads are served from the
local mirror when it is warm, and fall back to the GitHub API only on a miss, a
stale row, a cold mirror, or the escape hatch. GitHub stays authoritative for
every write and every merge-gating read.

This is the quota win: instead of polling `ListOpenIssues` / `ListOpenPRs` and
per-issue/PR state on every cycle across all projects, a warm mirror answers
those reads with zero REST traffic.

## What is served mirror-first

`internal/mirrorstore.Source` embeds `*github.Client`, so it is a drop-in for the
concrete client. It overrides only these reads:

| Read | Source | Notes |
|---|---|---|
| `ListOpenIssues(labels)` | `mirror_issues` (+ `mirror_labels`) | loss-free — title/body/labels all mirrored |
| `ListOpenPRs()` | `mirror_pull_requests` | needs `head_ref` + `body`, added to the schema in phase D |
| `IsIssueClosed(n)` | `mirror_issues.state` | point read |
| `IsPRMerged(n)` | `mirror_pull_requests.merged` | terminal, monotonic — a lagging mirror only ever re-polls, never fabricates a merge |
| `GetIssue(n)` | mirror hit, else **hydrate** | miss → fetch → store → serve; the next read is local |

Everything else passes straight through to the GitHub API:

- **All writes** — labels, comments, merges, closes, branch updates.
- **All merge-gating reads** — `PRCIStatus`, `PRMergeStatus`, `PRGreptileApproved`,
  review-gate verdicts. These are deliberately **not** mirror-served: the mirror
  cannot prove check completeness, and a stale "green" would be a merge-safety
  regression. Keeping them on the authoritative API is the conservative choice
  (no decision-quality regression).
- Project board reads, rate-limit probes, and any read not in the table above.

## Warmth and the empty-mirror fallback

A point miss (`GetIssue #N`) is a clean "hydrate this one entity" signal. A **list**
read has no per-row miss — an empty result could mean "nothing is open" or "the
mirror never saw the webhooks". So list serving is gated on **warmth**: the newest
mirrored row for that entity must be within the freshness horizon (webhooks are
flowing). A cold or lagging mirror falls back to the API, so an unpopulated mirror
degrades to today's API-direct correctness rather than silently under-reporting.

## Enabling / the escape hatch

Per project, in config (or the config store):

```yaml
github_mirror:
  source: mirror-first   # "" or "api" = API-direct (today's behavior, default)
  stale_seconds: 86400   # freshness horizon; 0 = default 24h
```

- Default (`""` / `api`): every read goes straight to the API — unchanged from
  before this phase. The default stays API-direct until mirror-first has soaked.
- `mirror-first`: serve the reads above from a warm mirror, fall back on miss/stale.
- **Escape hatch:** set `source: api` to restore today's behavior fleet-wide. The
  flag is read on every cycle, so a config-store edit takes effect **without a
  redeploy** — both the supervisor (via the config holder) and the orchestrator
  (via hot-reload of `github_mirror`) flip live.

The source is only built when the daemon has an open mirror — i.e. when webhook
ingestion is configured (`--webhook-secret-file`). Without inbound deliveries the
mirror has nothing to serve, so flows stay API-direct regardless of this flag.

## Observability

The gh wrapper already logs an hourly REST-usage line; phase D appends the
mirror hit/fallback counts to it:

```
[github] REST usage last 1h0m: 812 requests, ~120 billed against core quota,
  692 served free by 304, 0 rate-limited; mirror reads: 5400 served locally,
  240 fell back to API (96% hit)
```

The morning digest (`maestro digest`) carries the same counters on its
`GitHub reads:` header line and in JSON (`github_usage`):

```jsonc
"github_usage": {
  "requests": 812, "billed": 120, "not_modified": 692, "rate_limited": 0,
  "mirror_hits": 5400, "api_fallbacks": 240
}
```

A high `mirror_hits` with a low `api_fallbacks` is the phase-D win, made visible.
`mirror-first reads: not enabled` on the digest line means no read went through
the source (mirror-first off, or mirror empty).

## Verifying the quota drop

1. Confirm the mirror is warm: `GET /api/v1/fleet` → `mirror.total_rows` > 0 and
   `mirror.total_stale` low (see the phase-C runbook).
2. Set `github_mirror.source: mirror-first` for a project.
3. Watch the hourly `[github] REST usage` journal line: `served locally` climbs,
   `billed against core quota` drops.
4. To roll back instantly, set `source: api` — no redeploy.

## Notes / limits

- **List completeness** depends on webhooks. A PR/issue that exists on GitHub but
  never produced a delivery is not in the mirror; warmth catches a *lagging*
  mirror (stale newest row) but not a *missed single delivery*. Mitigations: the
  soak period, the escape hatch, and that maestro itself creates the PRs it gates.
- **Merge-gating stays on the API** by design (see above), so a warm-mirror cycle
  still issues the low-frequency CI/mergeable/review reads for PRs under active
  gating. The dominant per-cycle list + state reads are what move to the mirror.
- `head_ref` and `body` were added to `mirror_pull_requests` in phase D; an
  existing mirror is migrated in place on daemon start (idempotent `ALTER TABLE`).
