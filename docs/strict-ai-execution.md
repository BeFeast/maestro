# Strict AI execution and auxiliary occupancy

`ai_execution.require_verified_route` applies to the actual model leaves. Its
proof package is selected by an absolute `manifest_path` and exact lowercase
`manifest_sha256`. A source fixture, configured URL, registered native UUID,
successful process exit, or evidence-file existence cannot establish installed
readiness. Missing proof is a typed hold.

The manifest is version 1, `evidence_kind: installed`, expiring, bound to the
project/fleet and the normalized project configuration digest. `routes` is a map
by exact role (`planner`, `advisor`, `implementer`, `validator`, `repair`,
`supervisor`, `reviewer`). Each route keeps its explicit model, gateway scope,
budget-run binding, managed-principal digest, and domain-separated caller scope.
No model is substituted across roles. One package covers one installed gateway
instance; different installed gateways need separate packages.

The inspector compares exact Maestro loaded executable bytes, native harness,
gateway binary/process UID, boot/start ticks, command digest, proof-file hashes,
explicit model/session argv, and actual credential environment. Its authenticated
gateway observation uses a fresh nonce, one bounded direct loopback request, no
proxy or redirect, and an exact PID-owned listener/IP/network-namespace binding.
The supported `managed-runtime-v1` response is explicitly `configuration_only`:
startup/current config hashes, process instance, build, caller and policy must
match, and configuration application must be complete. Expected hashes come
from the gateway's own read-only `admission-config-digests` helper.

**Installed readiness remains held in this source slice.** Matching config
hashes do not observe the active credential-to-account mapping, dynamic model
registry, kernel egress rules or credentials reachable by tool subprocesses.
The final containment hold must be replaced by those actual observers, not a
config flag or a copied source-test receipt. Account and containment mechanisms
are separate required source work before any operational activation.

## Leaf behavior

Supervisor calls and explicit `review_producer.native_opus: true` share the
existing native Claude registration/receipt runner. The reviewer preserves
`opus_model` and `llm-review-opus`, uses one route, `--bare`, no tools, one turn,
and a bounded private cwd. It never inherits the supervisor fallback chain.
Arbitrary extra reviewer CLI arguments are held. Existing HTTP ChatLens,
CursorLens, router-model execution, spec grooming/custom completers, remote
workers and opaque hooks hold in strict mode.

Every local worker generation writes an owner-only execution descriptor tied to
its native acknowledgement and the exact project configuration. Parent preflight
checks the same proof as `_worker-exec`; the latter validates the actual argv and
the single credential-file snapshot immediately before `cmd.Start`. A descriptor
hash travels in the generated runner. The gate covers initial start, respawn,
in-place respawn and each serialized pipeline phase.

Controller reload invalidates captured policy snapshots. Detached workers check
both a durable revision file and an owner-only sealed memfd held by the live
controller incarnation. Reload/stop closes the old memfd before writing the
durable revision. Thus a failed disk invalidation cannot revive a delayed
worker, and an fd number reused for a new generation cannot match its random
digest. Durable failure is reported and a reload is not published as applied.
This is a check before launch; it does not claim atomic revocation of a process
that already started.

## Outcomes and recovery artifacts

All registered native invocations, including exit 0, retain their launch marker
and auxiliary occupancy until a trusted authority outcome establishes terminal
financial status. No stderr or local return code supplies that status. The
existing worker previous-generation outcome seam also stays closed. The actual
authority seal/settlement bridge and its consumer/reconciliation API are the
next required source slice.

Native stdout is capped at 4 MiB. Before returning an unresolved-outcome hold,
the runner fsyncs an owner-only output checkpoint containing exact role-run,
invocation and native-session IDs, raw bytes, digest, local status, completeness
and truncation metadata. Cancellation, timeout and overflow preserve the bounded
partial prefix. Metadata receipts reference that checkpoint; they do not copy
raw output into telemetry. Later trusted reconciliation can recover the same
result without requesting inference again. This slice does not yet consume such
a recovered result or claim settlement.

## One capacity owner

`fleet.max_auxiliary_runs` defaults to 2; zero holds auxiliary starts. The
existing fleet limiter owns this ceiling separately from the live worker floor.
In-memory reservations and durable launch markers are unioned by receipt-root
and role-run ID. Unknown/persistence-failed runs keep capacity. A durable SQLite
receipt-root index survives project removal and daemon restart, so removing a
project cannot erase its outstanding occupancy. Missing, unreadable, unsafe or
corrupt indexed roots hold the ceiling. A removed project cannot acquire a new
permit. No standalone managed reviewer can silently create another controller.

`scripts/llm-review.sh` is retired and always holds before any command/API call.
The old GitHub workflow is now only a manual explanatory notice with no inference
credentials, checkout, tools or automatic triggers. Product workflows are not
changed by this slice. This is source retirement, not proof that an already
deployed remote workflow has been replaced.
