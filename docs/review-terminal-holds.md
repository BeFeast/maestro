# HTTP review terminal holds

The daemon's `ChatLens` review producer consumes structured terminal metadata from
its configured gateway HTTP error response. It does not classify model text,
quoted JSON, harness logs, or usage-queue observations as control evidence. This
change creates no gateway, queue consumer, inference route, or worker recovery
policy.

The decoder accepts the documented `error.terminal` object, or the gateway's
root `terminal` fallback when `error` is not an object. Ambiguous duplicate JSON
keys, case variants of control keys, conflicting locations, missing/null control fields, malformed timestamps,
unknown enum values, inconsistent committed/same-request metadata and oversized
error bodies become holds. Additional unknown members are compatible. Error
bodies are bounded to 64 KiB; their content is never included in public status or
journal messages. HTTP success content is review text, never terminal control.
Redirects are not followed.

Each direct HTTP lens has a durable execution track in the existing project JSON
state, keyed repository, actual fetched PR head and lens. The producer rechecks
head and statuses before HTTP. State's durable flock/update boundary claims execution
and syncs the state file plus its directory before granting a call, and its revision merge preserves concurrent receipts against stale
ordinary state saves. A separately synced owner-only launch marker is created before HTTP and removed
only after the terminal state is durably synced. A failed final rename/sync can
leave visible state but the retained marker still prevents retry. Marker cleanup
failure conservatively keeps the hold. A process restart does not erase intent. Unfinished
claims, missing/corrupt state, failed persistence, unknown outcomes and lost
evidence cannot authorize replay. Existing observed errors or stale pending
statuses without an authoritative execution track also hold; the former
stale-pending heuristic cannot prove that an HTTP call did not execute.

`review_producer.max_attempts` defaults to **1**. Values **2–5** explicitly enable
bounded automatic retries for new exact-head/lens tracks. The initial cap is
retained; raising configuration does not retrospectively enlarge an existing
track's allowance. Reducing the configured cap prevents further claims. Invalid
values fail closed before HTTP.

Only an uncommitted `same_request` terminal outcome can be eligible:

- `quota_cooldown` requires a known future `retry_at`.
- `upstream_transient` waits at least one minute and honors a later supplied
  `retry_at`. An explicitly stale retry time holds.
- Credentials/configuration errors, invalid requests, cancellations, unsupported
  or malformed evidence and unknown outcomes hold for reconciliation.
- `stream_committed`, `new_turn_only` or `none` never automatically replay.

The daemon selects recorded due holds in addition to genuinely unobserved review
streams. It does not retry every observed error. Each retry has a new execution ID;
provider failure stays a non-green review status and does not become a product
finding or a repair-worker request. Other heads, PRs, projects and lenses keep their
own eligibility. Already successful/failed review statuses suppress execution.

Before recovery eligibility is persisted, an owner-only evidence artifact is
written and synced under the state's `review-evidence` directory (0700 directory,
0600 files, maximum 8 KiB per artifact). It contains only allowlisted terminal
metadata, execution identity and a response digest. It contains no raw response,
prompt, API key or header. For a truncated response the digest is explicitly
marked as a prefix digest. A digest proves byte identity, not authoritative usage
or an inference bill. Artifacts are immutable; missing/changed evidence holds
retry. There are at most five artifacts per exact-head/lens track. Historical
tracks are retained rather than evicted in a way that would reset execution
allowance; lifecycle archival needs a separate reviewed policy.

A successful HTTP result also settles the execution track before publication. If
review publication subsequently fails, reconciliation may publish existing trusted
evidence or request an explicit new policy; it must not rerun inference silently.
This change supplies no automatic deletion/reset operation for unknown claims.

The evidence artifact is **not a raw output checkpoint**. The non-streaming error
response does not supply the partial output referenced by a committed terminal
summary; `checkpoint_available` remains false, with a checkpoint-required hold.
Harness-native authoritative terminal mapping, full checkpoint/resume, summary-only
fallback, actual model/account attribution and shared financial admission remain
separate prerequisites. No dollar-cap or full R5 recovery guarantee follows from
this HTTP-only consumer.

Tests use local HTTP fixtures, fake forge clients, subprocess claim races,
persistence failures, restart/due-time scenarios, and stale state saves. No real
provider calls are required.
