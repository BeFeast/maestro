# Native request budgets

The optional `admission_basis: requests` field on
`worker_native_session_registration` and `supervisor.native_session_registration`
selects version 2 of the existing authenticated authority RPC. The corresponding
role in the installed AI execution manifest must pin the same basis. Omission
retains the original monetary version-1 protocol and receipt shape. Other values
hold before registration. No budget, account, model or transport changes are
inferred from this option.

The architecture remains Maestro → native Claude CLI → CLIProxyAPI → provider.
OAuth credentials belong to the gateway. Maestro uses its existing gateway
principal and separately registers the exact native session with the authority.
Request budgets are neither dollar nor token caps. The authority and gateway
must enforce one durable request unit for every physical attempt, including
retry or account fallback, with shared fleet/project daily and lifetime-run
limits. Request accounting does not fabricate a marginal price or erase earlier
monetary liabilities.

## Typed registration and completion

The controller persists the explicit basis in the registration intent before
RPC, checks the exact echoed binding and records the acknowledgement before
launch. Version 2 is selected from that persisted request, never from a server
response. The authority must bind each run immutably to its original basis.

Version-2 `seal_native` still revokes the exact native registration. Its result
contains `schema_version: 2`, `admission_basis: requests` and
`money_status: unknown`. `request_accounted` permits continuation only when all
nonzero physical attempts have trusted terminal provenance. `no_dispatch` means
zero attempts; unresolved or partial evidence remains `held`. The aggregate
contains request-cap violations and exact physical/terminal/unresolved counts,
never a monetary `settled` result or invented zero cost. The canonical snapshot
and evidence identifier include the complete versioned binding and basis.

Worker, supervisor and reviewer recovery validate this shape against the saved
registration and seal intent. A request-accounted snapshot cannot satisfy a
monetary request, and a changed current configuration cannot relabel an old run.
Both protocols retain durable output checkpoints, OS termination proof,
no-replay fencing and persistence-before-release. Lost authority replies hold;
the exact seal may be reconciled later without repeating inference.

## Explicit retirement of unknown usage

An operator may retire a sealed request generation after verifying that its
native process has terminated and every admitted attempt has a complete,
successful transport observation. This is a separate control RPC, `retire_native`,
enabled only for an explicit authority `--retirement-uid`. The daemon's ordinary
reconciliation does not issue it. Incomplete, cancelled or failed transports and
request-cap violations cannot use this path.

The result `operator_retired_unknown` retains the entire prior held snapshot,
all physical/terminal/unresolved counts, consumed request units and conservative
daily exposure. Money remains unknown. Its `operator_retirement` proof binds the
authenticated operator UID, reason, exact attempt observation digests, native
termination attestation, and original request and snapshot digests. Retirement
does not reinterpret old usage, refund a request, change the run or raise caps.
The authority also prevents replay of an old retired request in a new generation.

Maestro checks the strict version-2 proof when reading both RPC and persisted
outcomes. `ScheduleNativeOperatorRecovery` verifies the saved termination proof
against the operator attestation and queues one successor generation through the
existing operator-restart path. It preserves automatic retry counters and failure
history; a durable scheduling record prevents reuse after the retry is consumed.
Capture and reconcile termination before replacing the original execution profile.
Disable the authority's retirement capability after the scoped recovery while
retaining its new outcome reader and replay protection.

## Verification and activation

Protocol fixtures cover exact Unix framing, version/basis mismatch, corrupt or
ambiguous persisted outcomes, unknown money, cap/partial holds and legacy
monetary receipts. Worker phase and auxiliary recovery tests require the same
saved request basis after lost replies, and do not generate replacement work
until a valid completion is durable. Cross-language and actual gateway fixtures
must additionally prove physical request limits and trusted terminal accounting.

This source option does not activate request admission or choose operational
limits. The reviewed authority policy, matching gateway mode, request ceilings,
installed route/containment proofs and runtime changes belong to the separate
activation package.
