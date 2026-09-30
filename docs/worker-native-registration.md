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

This slice deliberately separates launch ownership from provider outcomes.
A local process exit or `.terminated` receipt does **not** prove that every
physical request committed zero output or settled financially. Before minting
any next-generation UUID after a launched generation, the closed
`previousNativeGenerationOutcome` seam must confirm a trusted physical outcome.
Its production default is `previous_outcome_unknown`: every phase transition,
respawn and fallback holds before a new UUID or registration. The source paths
and their identity/ancestry proof are implemented and exercised with explicit
synthetic outcome evidence; production currently supports initial registration
and exact adoption only until an actual authority outcome bridge is supplied.

Accepted artifacts, CLI exit 0, Claude stderr, quota wording, local process
teardown and successful registration cannot open this seam. OS capacity release
and financial recovery authorization remain separate. R5/native recovery is
not complete, and the actual trusted physical-outcome bridge is the next
mandatory source slice before an operational package can enable supported
next-generation execution. There is no operator configuration boolean that
converts unknown outcomes into permission. Accounting readiness stays false.

## Verification

Fake registrar/process fixtures exercise all four worker entrypoints, role and
ancestry rotation, exact ack validation, untrusted session arguments, unknown
carriers, persistence failures, lost registration/launch replies, crash adoption,
concurrent ownership, terminal release, retry/phase holds, and limiter restart
union/floor separation. Fixtures use temporary repositories and no credentials,
live DB, service control or provider calls. The feature-disabled suite retains
legacy launch/cleanup behavior.
