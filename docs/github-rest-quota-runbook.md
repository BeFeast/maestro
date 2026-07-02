# GitHub REST quota runbook

The fleet daemon polls GitHub on a single shared PAT: every flow reads issues,
open PRs, per-PR state, check-runs, and combined status each cycle. Two
incidents shaped the protections in `internal/github`:

- **2026-06-28 (#794)** — synchronized bursts tripped GitHub's *secondary*
  rate limit. Fix: per-flow start/tick jitter, bounded retry with
  backoff + Retry-After in the gh wrapper, per-cycle `ListOpenPRs` dedup.
- **2026-07-02 (#797)** — steady-state consumption approached the full
  *primary* 5000/hr window twice in one evening, and a rate-limited window
  during a drain stretched daemon stop past 20 minutes. Fix: conditional
  (ETag) polling, a usage counter with an hourly journal digest, and a
  shutdown fail-fast mode.

## What the wrapper does (#797)

- **Conditional requests.** Every polling GET through the wrapper remembers
  the endpoint's last `ETag` + body and replays with `If-None-Match`. GitHub
  answers an unchanged resource with `304 Not Modified`, which does **not**
  consume the hourly REST quota; the wrapper serves the cached body. This has
  no effect on merge-gating correctness — a 304 is GitHub's own guarantee
  that a 200 would have returned identical content, so every gate still sees
  exactly what GitHub would have sent.
- **Usage counter.** Each gh exchange is counted (total, 304s served free,
  rate-limited responses) and a one-line digest is written to the journal
  every hour:

      [github] REST usage last 1h0m0s: 412 requests, ~118 billed against core quota, 294 served free by 304, 0 rate-limited

- **Shutdown fail-fast.** `github.BeginShutdown()` — called by the daemon at
  the start of drain and teardown — disables rate-limit retries and cuts
  short any backoff already sleeping, so a rate-limited window degrades to
  failed cycles instead of blocking flow stop.

## Measuring calls/hour (before/after)

1. Pull the hourly digests for the window of interest:

       journalctl -u maestro.service --since "2026-07-02 20:00" | grep "REST usage last"

   `requests − served free by 304` is what was billed against the shared
   core bucket that hour. The digest counts one per gh invocation, so a
   multi-page `--paginate` call is a lower bound on raw quota use.

2. Cross-check against GitHub's own accounting (the `core.used` field):

       gh api rate_limit --jq '.resources.core'

   `used` resets exactly at the hourly window boundary (`reset`, epoch
   seconds); the target is steady-state `used` under 50% of `limit` so
   operator/CI activity on the same token cannot tip the fleet over.

3. Rate-limited lines, when they do occur, are logged with the real GitHub
   error text: grep for `rate-limited` in the same journal.

## If the fleet is rate-limited during shutdown

Nothing to do: the drain path already fails gh reads fast. If a stop still
hangs, a second SIGTERM aborts the drain wait immediately (`maestro daemon`
two-phase shutdown, #761).
