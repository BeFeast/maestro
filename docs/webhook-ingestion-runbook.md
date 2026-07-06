# Webhook Ingestion Runbook (#824, epic #811 phase B)

Inbound GitHub webhook ingestion into the unified SQLite store
(`~/.maestro/maestro.db`). The fleet daemon exposes one HTTP endpoint that
validates each delivery's signature, deduplicates on the delivery UUID, and
lands the raw payload plus a parsed envelope durably. Default **OFF** — a daemon
started without `--webhook-secret-file` behaves exactly as before and keeps
learning GitHub state by polling.

This is **ingestion only** (phase B). Nothing consumes the stored deliveries
into the orchestration read path yet; that is phase C/D. The goal of phase B is
to make the deliveries arrive and persist so a later phase can stop polling.

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
- **Missing `X-GitHub-Delivery` → `400`.**
- **Durable across restart.** The store uses SQLite WAL, so an acknowledged
  delivery survives a daemon restart — GitHub's at-least-once retry plus the
  idempotency key means nothing is lost or double-counted.

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
    "bad_requests": 0
  }
  ```

  `total_deliveries` and `by_event_type` are seeded from the durable store on
  startup, so they reflect the persisted total across restarts;
  `signature_failures` / `duplicates` / `bad_requests` are the current process's
  tally.

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
