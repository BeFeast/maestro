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
