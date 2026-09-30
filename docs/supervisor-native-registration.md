# Native Claude supervisor registration

The optional supervisor adapter binds each supported Claude process to a native
session UUID through the existing shared authority's trusted control socket.
This is attribution only. It neither reserves budget nor admits a physical
request. `require_accounting_ready: true` still holds before registration or
inference because physical-request admission remains unverified.

The feature is absent unless `supervisor.native_session_registration` is
configured. Every field is required; there is no production socket, UID, scope,
or policy default. An empty/invalid configured block holds instead of reverting
to unregistered launches. A configuration example with synthetic values is:

```yaml
supervisor:
  native_session_registration:
    control_socket: /private/fixture/control.sock
    authority_uid: 12345
    expected_policy_version: 3
    fleet_id: fixture-fleet
    gateway_scope: fixture-maestro
    budget_run_id: existing-pilot-budget-run
    ttl_seconds: 600
```

`budget_run_id` is an **already provisioned lifetime budget scope** in the shared
policy, bound there to the configured stable project ID. It is distinct from
the local consultation/role-run UUID. Multiple consultations may share a pilot
budget scope, which is potentially stricter than a per-consultation allowance.
This adapter never creates, resets or extends a financial allowance, and has no
policy mutation endpoint. Missing policy scope holds. Dynamic run provisioning
requires a separate trusted policy workflow.

The control server authenticates the caller using Linux `SO_PEERCRED`; this
client also verifies the server UID against explicit `authority_uid` before
sending any request. The server separately restricts configured gateway scopes.
UID authentication is a host-account trust boundary: other processes running as
an authorized UID have that account's authority. It does not distinguish or
isolate agents sharing one OS account. Deployment must protect the socket path
and use the intended account separation. Non-Linux builds compile but reject
registration because this implementation cannot verify the peer there.

## Ordering and evidence

The reusable `internal/admissioncontrol` package uses the ai-bills #87 version-1
protocol: a four-byte big-endian length and UTF-8 JSON on a Unix stream, at most
65536 bytes, one request per connection. It performs no retries. RPC duration is
bounded by seven seconds and the remaining supervisor consultation deadline.
Initial attribution uses `register`, never `reserve`, `claim`, or a policy
mutation. Completed native calls use the separate `seal_native` outcome bridge
described in `strict-ai-execution.md`.

For each supported Claude candidate:

1. Reuse the invocation UUID already owned by the local receipt as the bare
   native session UUID. Record fleet, project, budget run, role, gateway caller
   scope, immutable expiry and expected policy version in the receipt.
2. Sync the receipt and a separate `registration.json` marker before RPC.
3. Require an authenticated, exact binding echo and active registration. Reject
   missing, malformed, duplicate, extra or conflicting response fields, revoked
   and expired registrations, unknown results and all authority holds.
4. Sync the acknowledged binding/version before writing `launch.json`, clearing
   the registration marker and starting the process. A successful registration
   is not a successful process start and does not create an invocation receipt.
5. Preserve launch uncertainty after Start. Save the bounded output checkpoint
   and exact native seal intent before reconciliation. Only a fsynced allowed
   authority outcome plus proven local process termination can clear the marker
   before a fallback registers a new UUID. Every saved native invocation in the
   consultation must be terminal before its auxiliary permit is released.

The supervisor accepts only the Claude harness with an executable basename
`claude`; arbitrary wrappers and other harnesses hold. Configured session,
resume, continue, fork and no-persistence flags are rejected, including equals
forms, short `-r`/`-c` forms/clusters and ambiguous `--`. The adapter injects one
owned `--session-id`. It does not use a prompt or project/role header as authority.

The installed Claude 2.1.285 offline wire receipt proved the bare UUID in both
`x-claude-code-session-id` and JSON `metadata.user_id.session_id`. Two synthetic
503 retry sends and four 529/model-fallback sends retained one UUID. Each actual
send still needs its own gateway-generated physical-attempt identity and durable
admission. Native cost/usage output is not account/model evidence.

Repeating a new `--session-id` in a persisted Claude HOME fails locally; explicit
`--resume` preserves UUID/history. This adapter implements neither automatic
resume nor no-session-persistence. It does not attest arbitrary future Claude
builds, verify the deployed gateway parser, or register native children. The
isolated bare-mode child probes spawned zero agents; child propagation is
unknown. Worker, ChatLens, Cursor, Codex and other paths are separate work.

## Explicit registration-only recovery

An unavailable authority, rejected binding, lost/malformed reply, crash, or
failed acknowledgement sync starts no process and leaves a pending registration.
A fresh `Complete` holds with `unresolved_registration_intent`; it does not mint
another native identity. No automatic retry occurs.

`supervisor.ReconcileNativeSupervisorRegistration(cfg)` is an explicit Go API for
the subsequent operator/reconciliation integration; this slice adds no CLI
activation or background reconciler. It repeats only the exact persisted
registration, with the original UUID, scope, expected policy and expiry. A valid
acknowledgement is synced and the old receipt ends as
`registration_reconciled_not_launched`. It never starts/resumes that process,
extends expiry, changes policy or deletes an uncertain launch marker. Existing
`launch.json` always blocks this API. Policy drift, expiry and revocation can
therefore continue to hold and need separate operator resolution, not an
invented allowance or changed replay payload.

Receipts contain safe identity and binding/version evidence, not socket paths,
raw argv, prompts, credentials, or environment. Readiness remains false. Tests
use local fake Unix authorities and fake executables; no real inference,
production auth, service, usage queue, install or activation is involved.
