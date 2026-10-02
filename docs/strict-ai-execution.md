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
An explicit `admission_basis: requests` must match the native registration;
omission preserves monetary version 1. See [native request budgets](native-request-accounting.md).
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

The inspector also observes the exact active credential/model selection and
the installed namespace, complete nft ruleset, gateway listener and root-owned
execution profile. The shared launcher contains native workers, supervisors and
reviewers as described in [Native role containment](native-role-containment.md).
Credential selection evidence does not establish account ownership; that still
requires reviewed provisioning evidence. Missing or mismatched installed proof
holds execution. Source tests do not establish operational readiness: the
reviewed R9 installation and complete systemd-to-native-entry acceptance remain
required before activation.

## Managed Claude credential bindings

The `managed-claude-bindings-v1` observation pins the whole managed credential
set. Every configured credential must be a pinned one with its reviewed alias,
kind, binding mode and approved model definitions. A credential that is not
selectable (quota block, expired token, cooldown) may stay as a standby while
every pinned (route, model) key is served by a selectable credential whose
identity proof matches its pin. The same predicate runs at every launch and in
the pre-registration lane probe, which pauses dispatch without persisting a
hold or spending a retry.

| Hold | Meaning |
| --- | --- |
| `binding_credential_unverified` | A selectable credential has no identity proof for its current token. The observer asks the gateway's verifier at most once a minute and observes again. |
| `binding_credential_absent` | A pinned credential has no loaded auth at the gateway: no kind, token, generation or registered model. |
| `binding_credential_mismatch` | A credential contradicts its pin: alias, kind, mode, key digest, identity, generations, or a pin match its fields disagree with. |
| `binding_route_unserved` | A pinned (route, model) key has no selectable, verified credential. |
| `binding_model_mismatch`, `binding_model_set_mismatch` | A credential's model definitions differ from the approved ones. |

Wherever the gateway can compare a credential with the pin it enforces, the
comparison must be reported (`pin_matches`) and agree with the manifest. That
covers every exact key and every OAuth credential that carries an identity
proof, standby or not. The gateway stores a proof only for its configured
identity, so a reported identity is the enforced one. An OAuth standby whose
current token has no proof cannot be compared: the projection exposes the
enforced identity only through a proof. For that standby the launch relies on
provisioning:

- `ValidateClaudeBindingReceipt`, the provisioning contract, accepts a manifest
  only against a receipt in which every pinned credential proves its pin.
- The enforced pins belong to the gateway's immutable startup admission
  snapshot of the manifest's process instance. Its digest is
  `managed_admission_sha256`, which every launch requires unchanged, fully
  applied and without drift.

A pin proved once at provisioning therefore holds for the manifest's lifetime;
a gateway restart or a pin change holds every launch until a new manifest is
provisioned. Provision while every pinned credential is verified: a quota
block keeps the proof, an expired token does not.

Every observer request carries the management key, so the observer proves that
the pinned gateway PID owns the loopback listener immediately before each
request, and once more after the last response.

### Holds persisted by earlier builds

Installing standby support does not re-evaluate holds that earlier builds
already persisted, such as sessions and review attempts held while one pinned
credential was blocked. Earlier builds held at launch, after `launch_intent`,
so the bounded automatic lane resume does not apply: it only resumes a first
generation still at `registered` that a `binding_` hold stopped. These holds
need the existing operator paths:

- Worker first generations with a failed host `launch_intent` or a `registered`
  receipt whose acknowledgement is still valid: the one-shot daemon flag
  `--recover-native-prelaunch project-UUID:slot:native-session-UUID`; see
  [worker native registration](worker-native-registration.md).
- Sealed worker generations with unknown usage: the authority's `retire_native`
  followed by `ScheduleNativeOperatorRecovery`; see
  [native request budgets](native-request-accounting.md).
- Review attempts: an operator rearm grant (`AttemptStore.AuthorizeNativeRearm`)
  with proof kind `pre_launch_hold` and the `NativeReviewPreLaunchHoldProof`
  digest for an attempt held before any native launch, or the verified outcome
  digest (`NativeReviewRearmProof`) for a failed attempt.

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
and auxiliary occupancy until every saved native binding has a trusted sealed
outcome for its original admission basis. The authority atomically revokes each exact registration and
returns either sealed zero dispatch, validated ledger settlement without bound
violations, or a hold. The control request intent and exact digest-bound result
are fsynced before releasing any marker or permit. Stderr, local exit, expiry,
and gateway transport observations cannot authorize financial recovery.

The companion typed request-accounting integration selects version 2 only for
an explicit, persisted `admission_basis: requests`. Its completion is
`request_accounted` with `money_status: unknown`, never monetary settlement or
an invented zero cost. The original monetary version-1 contract remains
unchanged. Both bases require OS termination, durable output and trusted exact
binding completion before release; neither permits inference replay on recovery.

Native stdout is capped at 4 MiB and saved in an owner-only checkpoint with
exact role-run, invocation and native IDs, raw bytes, digest, local status,
completeness and truncation metadata. Cancellation, timeout and overflow retain
the bounded partial prefix. A successful exact-role/input replay returns this
validated checkpoint without inference; an incomplete or failed checkpoint can
never become successful output. Corrupt, foreign, symlinked or unsafe checkpoints,
and missing files already referenced by a completed invocation, hold both
recovery and permit release.

A crash before the completed invocation receipt is saved retains the exact
planned invocation. Recovery observes the original pinned containment profile,
native UUID, systemd unit, boot and service incarnation, and kernel cgroup;
a saved boolean or an absent service alone cannot establish termination. The
recovered process proof and output checkpoint are fsynced before sealing the
authority binding. An orphan checkpoint with complete, matching output can be
restored without inference. Missing output becomes a durable
`local_output_unknown` record, never an invented empty successful answer. A
timeout or unresolved containment with a preserved prefix remains a local
failure even after terminal process and financial proof release occupancy.
The full validated OS terminal snapshot is saved with its digest before
financial completion. A completed receipt validates that durable snapshot and
the financial proof without rereading retired profiles, binaries or cgroups;
ordinary profile rotation cannot reclaim or freeze its released occupancy.
An unresolved invocation still requires a fresh observation through its original
pinned profile and remains held if that evidence is unavailable.

`ReconcileNativeConsultation(cfg, identity, prompt)` and re-entry with the same
identity/input reconcile only saved native bindings. They never acquire another
permit, launch a process or walk the fallback chain. Multi-native consultations
require every invocation to be sealed and terminal. A fresh cycle encountering
a prior unresolved role first reconciles that role and returns
`native_prior_outcome_reconciled` without returning old output or starting new
inference. The next cycle can then proceed normally. A different prompt or
reviewer model cannot reuse an old result.

Sealing an exited session can race the authority's own settlement: the usage
tap accounts a physical attempt a few seconds after process exit, so the seal
may still report it as claimed (`outcome_unknown`, `unresolved_attempts > 0`).
Live completion therefore re-seals the same exact binding with backoff for a
bounded window (60 s) before treating the outcome as unresolved; recovery
seals once per cycle. If the window elapses with attempts still unresolved,
the receipt stays held exactly as before. The one-shot reviewer lane is the
single exception: when every binding carries a validated seal, the only hold
reason is settlement latency and the local output is a complete, untruncated
success, the verdict is returned flagged `accounting pending`. The review
producer posts it with that note and records the attempt as
`review_completed_accounting_pending`; the native receipt, launch marker and
auxiliary permit remain held until the regular reconcile observes the settled
seal, and no second attempt is spent on it. Nothing is synthesized locally and
no outcome history is removed.

The controller independently validates the durable completed role receipt and
checkpoint before dropping a retained in-memory permit. Durable pending native
receipts still consume capacity if a launch marker is missing. The strict
contained execution path must record the exact deterministic systemd unit and
verified terminal process state before seal; uncertain containment remains held.
Outcome proof and process termination are separate requirements.

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
