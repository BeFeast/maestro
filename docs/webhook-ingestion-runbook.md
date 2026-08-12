# Webhook Ingestion Runbook (#824, epic #811 phase B)

Inbound GitHub webhook ingestion into the unified SQLite store
(`~/.maestro/maestro.db`). The fleet daemon exposes one HTTP endpoint that
validates each delivery's signature, deduplicates on the delivery UUID, and
lands the raw payload plus a parsed envelope durably. Default **OFF** — a daemon
started without `--webhook-secret-file` behaves exactly as before and keeps
learning GitHub state by polling.

Stored deliveries **are** consumed: since #825/#826 each newly-accepted delivery
is projected into the GitHub mirror read model (`mirror_*` tables), and gate
events (`check_run` / `check_suite` / `status`) wake the matching project flow
immediately instead of waiting for the next poll. Phase-E reconciliation repairs
whatever a missed delivery left stale. (Earlier revisions of this runbook claimed
"ingestion only, nothing consumes the stored deliveries" — that has been false
since #825.)

Ingestion is **GitHub-only**. Projects on a `forge: kind: forgejo` row are
poll-only by design and the endpoint actively rejects their deliveries — see
[Forgejo rows are poll-only](#forgejo-rows-are-poll-only).

## Why

All GitHub state (issues, PRs, checks, reviews, labels) reaches maestro by
polling. #798 made unchanged polls cheap (304s), but polling still churns CLI
processes, risks burst-limit exposure during high-change windows, and runs
thousands of exchanges per hour. Webhooks turn that push-shaped: GitHub sends
each change once, signed.

## What lands

For every delivery with a valid `X-Hub-Signature-256`:

- the **raw payload** verbatim (the signed bytes), and
- a denormalised **envelope** — event type (`X-GitHub-Event`), `action`,
  `repository.full_name`, `sender.login`, hook id, and receipt time.

Captured event types (phase B scope): `issues`, `label`, `issue_comment`,
`pull_request`, `pull_request_review`, `pull_request_review_comment`,
`check_run`, `check_suite`, `status`, `projects_v2_item`, plus `ping` (the
delivery GitHub sends when a webhook is first created). A valid-signature
delivery of any other event type is still stored — the supported set gates only
observability labels, never persistence, so no acknowledged event is dropped.

## Enabling it

1. **Mint a secret** and put it in a file the daemon user can read (never commit
   it):

   ```
   openssl rand -hex 32 > ~/.maestro/webhook-secret
   chmod 600 ~/.maestro/webhook-secret
   ```

2. **Start the daemon with the secret file.** Only the *path* is passed on the
   command line; the secret is read from disk at startup, never logged, and
   never written to the config store.

   ```
   maestro daemon \
     --webhook-secret-file ~/.maestro/webhook-secret \
     [--webhook-path /api/v1/webhooks/github] \
     [--webhook-db ~/.maestro/maestro.db]
   ```

   | Flag | Default | Purpose |
   |------|---------|---------|
   | `--webhook-secret-file` | *(empty — ingestion disabled)* | Path to the file holding the GitHub webhook secret. |
   | `--webhook-path` | `/api/v1/webhooks/github` | Endpoint path under the fleet port. |
   | `--webhook-db` | `~/.maestro/maestro.db` | Shared SQLite db deliveries land in. |

   On success the daemon logs
   `webhook ingestion active — POST <path> validates X-Hub-Signature-256 …`.
   A blank / unreadable / empty secret file disables ingestion with a loud
   warning rather than aborting startup.

3. **Point GitHub at the endpoint.** The endpoint listens on the fleet port
   (default `:8786`), which is bound to `127.0.0.1` by default — GitHub cannot
   reach it directly. Expose it with a tunnel or reverse proxy on the LAN host:

   - **Repository / org webhook** (Settings → Webhooks → Add webhook) or the
     **GitHub App** webhook config:
     - Payload URL: `https://<your-tunnel-host>/api/v1/webhooks/github`
     - Content type: `application/json`
     - Secret: the value from step 1
     - Events: issues, labels, issue comments, pull requests, PR reviews, PR
       review comments, check runs, check suites, statuses, and (org/App only)
       Projects v2 items.
   - **Tunnel options:** `cloudflared tunnel`, `ngrok http 8786`, or a reverse
     proxy (Caddy/nginx) that forwards `/api/v1/webhooks/github` to
     `127.0.0.1:8786`. The signature check means the tunnel does not need its
     own auth, but TLS on the public leg is still recommended.

   > The webhook endpoint authenticates by signature, **not** by the fleet
   > bearer token (`server.auth`). GitHub cannot attach an `Authorization`
   > header, so even in the exposed-auth posture the ingestion path is routed
   > *before* the fleet auth middleware. It still fails closed on any bad or
   > missing signature.

## Semantics

- **Idempotent.** Storage is keyed on `X-GitHub-Delivery`. GitHub retries on any
  non-2xx and an operator can replay from the webhook's *Recent Deliveries*; a
  replay of an already-stored delivery is a no-op that returns `200` with
  `{"status":"duplicate"}`. A newly stored delivery returns `202`.
- **Invalid signature → `401`**, counted, payload **not** stored.
- **Gitea/Forgejo-origin delivery → `422`**, counted, **not** stored and **not**
  projected. Checked after the signature so an unauthenticated prober still gets
  `401`. See below.
- **Missing `X-GitHub-Delivery` → `400`.**
- **Durable across restart.** The store uses SQLite WAL, so an acknowledged
  delivery survives a daemon restart — GitHub's at-least-once retry plus the
  idempotency key means nothing is lost or double-counted.

## Forgejo rows are poll-only

Projects with `forge: kind: forgejo` (#1172) **do not** use webhook ingestion.
They learn forge state by polling only, and that is deliberate, not a gap.

**Why.** Forgejo and Gitea deliver GitHub-*aliased* headers — `X-GitHub-Event`,
`X-GitHub-Delivery`, and `X-Hub-Signature-256` with the identical `sha256=`+hex
HMAC scheme. A Forgejo hook pointed at this endpoint with the same secret would
therefore pass signature validation and be stored *and projected* into the
`mirror_*` tables, which are keyed by the bare `owner/name`. For a repo mirrored
between GitHub and Forgejo that is the **same key** the GitHub mirror uses, so
Forgejo entities (different id space, different payload shape) would silently
overwrite GitHub-derived rows. The mirror read model is GitHub-keyed and config
validation documents it as GitHub-webhook-fed; a Gitea-format adapter would have
to reopen that, so M5 enforces poll-only instead.

**What the endpoint does.** A delivery carrying any of these headers is rejected
with `422 Unprocessable Entity` — never stored, never projected:

```
X-Forgejo-Event   X-Forgejo-Event-Type   X-Forgejo-Delivery   X-Forgejo-Signature
X-Gitea-Event     X-Gitea-Event-Type     X-Gitea-Delivery     X-Gitea-Signature
X-Gogs-Event      X-Gogs-Event-Type      X-Gogs-Delivery      X-Gogs-Signature
X-Gitea-Hook-Installation-Target-Type
```

GitHub never sends any of them, so the check cannot mis-fire on real GitHub
traffic. The check runs **after** signature validation: an unsigned probe still
gets `401` and learns nothing about which forges this endpoint distinguishes.

`422`, not a quiet `202`-and-ignore: a stored-but-unprojected Gitea delivery
would inflate `webhooks.total_deliveries` and `by_event_type` and read as healthy
ingestion, hiding the misconfiguration exactly where an operator looks for it.

**Symptom → fix.** `webhooks.forge_rejected > 0` on `GET /api/v1/fleet`, plus
journal lines `[webhook] rejected delivery: gitea/forgejo-origin (…) — forgejo
rows are poll-only`. Delete the webhook from the Forgejo repository. Retrying
cannot help; nothing about the delivery is salvageable here.

**Cleaning up deliveries accepted before the guard existed.** The guard is
ingest-side only: a Forgejo hook that was pointed here *before* #1172 M5 was
accepted (`202`) and projected, so removing the hook stops new contamination but
does not undo the old. Spot it by comparing the mirror rows for the mirrored
`owner/name` against GitHub — Forgejo-derived rows carry Forgejo's id space
(issue/PR numbers and ids that do not exist on the GitHub side):

```
sqlite3 ~/.maestro/maestro.db \
  "SELECT received_at, event_type, repo, sender FROM webhook_deliveries
     WHERE repo = 'owner/name' ORDER BY received_at DESC LIMIT 20;"
```

No manual repair is required for the read model: the GitHub row's phase-E
`runMirrorReconcile` loop snapshots the authoritative GitHub open-issue and
open-PR sets and rewrites whatever diverged, so the mirror converges on its next
pass (`mirror reconcile repaired N row(s)` in the journal). Only the mirror
converges, though — the raw rows in `webhook_deliveries` are kept verbatim and
are never pruned (see the delivery table note below), so a historical
Forgejo-origin payload stays queryable there. It is inert: nothing re-projects a
stored delivery.

**Reading fleet health for a forgejo row.** The top-level `webhooks` block is
fleet-global and says nothing about a poll-only project. Each entry in
`projects[]` carries `forge` (`"github"` | `"forgejo"`) and
`webhooks_applicable`; when `webhooks_applicable` is `false`, do not read global
webhook health as covering that row.

## Diagnostics

- **Journal:** each accepted delivery logs one line
  (`[webhook] stored delivery event=… action=… repo=… delivery=…`); the payload
  is never logged. Rejections log `invalid or missing X-Hub-Signature-256`.
- **Fleet API:** `GET /api/v1/fleet` carries a `webhooks` block:

  ```json
  "webhooks": {
    "enabled": true,
    "path": "/api/v1/webhooks/github",
    "last_delivery_at": "2026-07-06T12:00:00Z",
    "last_event_type": "pull_request",
    "total_deliveries": 128,
    "by_event_type": { "issues": 40, "pull_request": 88 },
    "duplicates": 3,
    "signature_failures": 0,
    "bad_requests": 0,
    "forge_rejected": 0
  }
  ```

  `total_deliveries` and `by_event_type` are seeded from the durable store on
  startup, so they reflect the persisted total across restarts;
  `signature_failures` / `duplicates` / `bad_requests` / `forge_rejected` are the
  current process's tally. `forge_rejected > 0` means a Forgejo/Gitea hook is
  pointed at this endpoint — see
  [Forgejo rows are poll-only](#forgejo-rows-are-poll-only).

- **Missed deliveries self-heal.** A dropped or never-emitted delivery leaves the
  mirror stale until the phase-E reconciliation loop repairs it — `last_delivery_at`
  going quiet alongside a rising `mirror.drift_repairs` is that safety net working.
  See the [mirror reconciliation & health runbook](mirror-reconciliation-runbook.md).

## Inspecting the store

```
sqlite3 ~/.maestro/maestro.db \
  'SELECT received_at, event_type, action, repo FROM webhook_deliveries ORDER BY received_at DESC LIMIT 20;'
```

The `webhook_deliveries` table is disjoint from the approvals / state tables
that share the same `maestro.db`.

It has **no retention or pruning** of any kind: every accepted delivery is kept
with its full payload forever, so the table grows without bound on a busy repo.
Pre-existing and forge-independent; deleting old rows by `received_at` is safe
(nothing re-reads a stored delivery — the mirror is fed at accept time and
repaired by reconciliation), but there is no automation for it yet.
