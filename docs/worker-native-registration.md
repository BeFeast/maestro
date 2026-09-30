# Native Claude worker generation registration

`worker_native_session_registration` opts local Claude workers into native
session attribution. It is disabled by default and does **not** advertise
accounting readiness, financial admission, all-role coverage, or native child
coverage. The downstream gateway remains responsible for every physical send.
No authority, policy, budget, service, credential or provider is provisioned by
this feature.

```yaml
project_id: fixture-project
worker_native_session_registration:
  control_socket: /run/fixture/admission-control.sock
  authority_uid: 1000
  expected_policy_version: 1
  fleet_id: fixture-fleet
  gateway_scope: fixture-gateway
  budget_run_id: fixture-approved-budget
  ttl_seconds: 3600
```

The selected `budget_run_id` must already exist in the authority's approved
policy. Registration uses the shared admission control client, its UDS peer UID
check and exact acknowledgement validation. `run_id` is the approved financial
budget identity; the distinct `role_run_id` identifies this execution generation.
The UUID passed to Claude `--session-id` is a third identity and is the bare
native header UUID, with no `claude:` prefix.

Fresh orchestrator dispatch sets planner/implementer/repair context **before**
`worker.Start` using a per-call config copy. The explicit CLI `spawn` command
supplies implementer context. Phase transitions use the trusted pipeline phase
(planner, advisor, implementer, validator); exact-session repair dispatch
supplies repair context and its prior role-run ancestry. Respawn/fallback and
in-place continuation create a new native UUID and role-run UUID. Native
physical retries within one Claude process keep that process's UUID.

Remote workers, non-Claude carriers, unknown backends and native
session/resume/continue/fork overrides fail closed. Neither a prompt nor worker
output can supply the role, accounting scope or generation.

## Durable sequence and recovery

Owner-only `state_dir/worker-native-sessions/<slot>/generation-<n>.json` receipts
are written with file and directory fsync under an exclusive per-slot flock:

1. `registration_intent` saves the complete immutable request and generation
   identity before contacting the authority.
2. `registered` saves the exact non-revoked, unexpired acknowledgement before
   worktree setup or stopping a preceding generation.
3. `launch_intent` is fsynced immediately before launching the existing
   generation-specific OS process lease.
4. `launched` saves its exact pane PID and OS lease. Session projection carries
   the native UUID, role-run UUID, ancestry and receipt location.

No held registration is retried automatically. The source API
`worker.ReconcileNativeWorkerRegistration(cfg, slot, generation)` repeats only
the exact saved request after checking current scope and backend configuration;
it records `registration_reconciled_not_launched`. It never starts or adopts a
process and does not turn a recovered acknowledgement into launch authority.
There is no new operator CLI command or automatic generation abandonment path.
A held receipt cannot be erased, rebound or replaced by a subsequent generation.

A lost launch reply is reconciled by the existing launcher. If it cannot prove
ownership, the process lease and scratch receipt survive. Re-entering the same
worker entrypoint with the same launch context can only adopt the exact existing
tmux pane, repository worktree, branch and active OS lease. Missing, mismatched,
unreadable or already-terminated runtime holds without replaying a runner. A
registration expiry prevents a new launch but does not erase an existing run.
Normal runtime/CAS projection recovery also validates the native receipt and
preserves its generation.

Only proven exact OS-lease teardown writes the immutable `.terminated` receipt.
Held generations can also reconcile local termination without signalling: the
exact previously launched OS lease must be inactive and the exact tmux pane
absent. An active/unknown lease or uncertain launch intent retains capacity.
This read-only process check preserves the financial hold and raw worktree.
Missing native process-lease state never falls back to legacy PID ancestry kill;
already-terminated receipts allow only verified idempotent cleanup.
Neither terminal-looking session status nor a newer stale state projection can
release unresolved native occupancy. The existing fleet limiter unions native
launch receipts with running sessions by project state directory and slot. It
deduplicates adoption and keeps unresolved launch capacity across daemon restart.
The live-worker floor still counts actual running projections, excluding holds;
an uncertain receipt cannot hide a floor alert.

Native holds remain attached to the canonical session/issue claim. They retain
retry counts, feedback, prior phase, Advisor rounds and generation identity;
they do not trigger provider fallback or generic worker-failure handling. The
fsynced receipt is authoritative when the ordinary state projection write fails.
Supported launch adoption requires explicit context and exact process evidence;
no all-role readiness claim follows from registration alone.

Launch ownership remains separate from provider outcomes. A local process exit
or `.terminated` receipt does **not** prove physical requests settled. Before
minting any next-generation UUID after a launched generation, the worker uses
`seal_native` through the pinned authority peer. The saved request is the exact
old binding plus installed registration version, even after policy rollover or
expiry. An `outcome_intent` is fsynced before the call; the exact digest-validated
result is fsynced before any next-generation authorization.

An outcome intent prevents re-adopting that native generation: a lost reply may
already have sealed it. `worker.ReconcileNativeWorkerOutcome(cfg, slot,
generation)` repeats only that same idempotent seal, never launches or signals,
and can replace a held snapshot after late trusted ledger settlement. Normal
next-generation entrypoints perform the same reconciliation under their
existing per-slot lock. The orchestrator can reconcile a financially held,
proven locally terminal generation, retaining its hold until the saved authority
snapshot explicitly permits recovery. Cleanup without configuration reads only
the persisted proof and never makes a network call.

The authority permanently revokes that native binding before aggregating its
physical attempts. Only a sealed zero-attempt result, or all physical attempts
settled through immutable validated durable-ledger provenance without bound
violations, can permit a next generation. Unknown, missing, partial, conflicting,
or non-durable evidence keeps `previous_outcome_unknown`. Direct gateway
financial acknowledgements, accepted artifacts, exit 0, stderr, quota wording,
registration and process teardown cannot open this seam. Transport failures are
never silently replayed, and an outcome persistence failure cannot authorize
cleanup or a new UUID.

The authority separately fences each physical send inside a native process.
A managed gateway must validate atomic `native_fence` claim capability and
report strict full terminal evidence. Stable native ingress fingerprint checks
prevent same-body SDK replay after partial or lost successful responses. These
transport observations do not settle financial liabilities or permit a new
native generation; that requires the sealed ledger-backed outcome above.

Accounting readiness stays false in these attribution receipts. Supported
all-role readiness, native child coverage, configured authority/tariff evidence,
and operational enablement remain separate prerequisites. There is no operator
boolean that converts unknown outcomes into permission.

## Verification

Fake registrar/process fixtures exercise all four worker entrypoints, role and
ancestry rotation, exact ack validation, untrusted session arguments, unknown
carriers, persistence failures, lost registration/launch replies, crash adoption,
concurrent ownership, terminal release, retry/phase holds, and limiter restart
union/floor separation. Outcome fixtures cover cross-language canonical digests,
claim/registration policy separation, lost seal replies, persistence failures,
held late settlement, phase transitions, exact cleanup proof, and corruption.
Fixtures use temporary repositories and no credentials,
live DB, service control or provider calls. The feature-disabled suite retains
legacy launch/cleanup behavior.
