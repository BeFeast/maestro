# Supervisor consultation receipts

The CLI supervisor adapter records local execution in the project's state
directory under `supervisor-consultations/`. This is the first R2 slice
(Refs #1193). It does not establish all-role accounting or gateway admission.

`current.json` contains the most recent consultation. Before the next
consultation, its snapshot is archived as `<consultation UUID>.json`. Writes
use a private temporary file, file sync, atomic rename, and directory sync.
A nonblocking per-directory lock serializes writers across clients/processes.
There is no automatic receipt pruning in this slice.

Each consultation identifies the stable project UUID (empty for legacy rows),
the deterministic decision/cycle, role `supervisor`, and optional parent
role-run UUID. Its UUID links from `SupervisorDecision.consultation_id`.
Reusing an archived consultation UUID is refused. Legacy `LLMClient` adapters
remain source-compatible; they do not acquire receipt coverage by implementing
the old text-only interface.

Before process launch, the receipt contains a `launch_intent` candidate and a
`planned_invocation` route snapshot; a separate durable `launch.json` marker
is written. Only a successful process start creates an entry in `invocations`.
Each invocation has its own UUID and sequential local number. Build/start
failures, disabled/cooling candidates, failure-memory skips, denied metered
routes, and elapsed deadlines remain candidate observations. They are not
physical attempts or usage events.

The completed invocation is saved before another candidate can run. Only a
successful sync permits removal of the launch marker. A write failure stops
the consultation immediately. A crash or failed outcome write leaves durable
uncertainty: a fresh client returns `unresolved_launch_intent` instead of
repeating the call. Do not automatically delete that marker. Reconciliation
must preserve the receipt and establish what happened to the prior call;
automatic reconciliation/settlement belongs to the subsequent accounting
integration. Deterministic supervisor decisions continue when synthesis is
held; this hold does not authorize or deny other roles' work.

## Model and policy evidence

The receipt separates:

- `requested_model`: the `supervisor.model` setting, possibly empty.
- `configured_model`: selected backend metadata, which may not reach argv.
- `effective_cli_model`: a single unambiguous explicit model argument, with
  evidence `single_explicit_cli_model_argument`. This describes CLI input,
  not the model that actually answered upstream. Generic commands, defaults,
  repeated model arguments, and settings/config overrides remain unknown.
- `upstream_actual_model` and `account_alias`: null until authoritative
  gateway evidence exists. `catalog_revision` is also null.

`model_arguments` contains only validated model values. Receipts omit the
prompt/output, raw argv, environment, executable/auth paths, and raw errors.
`policy_digest` hashes the local routing configuration snapshot; it is not
a catalog or config-store revision. `policy_version` names the existing
configured backend-chain semantics. Fallback records its reason and whether
the configured fallback route was selected; exact upstream model preservation
remains unverified. Metered candidates require the existing explicit
`supervisor.allow_metered_backend` policy, including fallback candidates.

## Strict mode and remaining dependencies

`supervisor.require_accounting_ready: true` returns typed
`accounting_route_unsupported` before any model call, including custom clients.
The current adapter's `AccountingCapability` has neither verified attribution
nor verified physical-request admission. It cannot be enabled by a configured
claim. Safe deterministic short circuits make no consultation or usage record.

The optional `ConsultationClient` result/identity boundary is the integration
point for future transport evidence. A future adapter must bind this ancestry
to gateway requests and prove the shared authority admits every physical call,
including provider retries; a grant at process-spawn time is insufficient.
One local invocation may contain many gateway logical requests, physical
attempts, or model-generated summaries. Native usage remains supplemental.

The existing gateway session headers are not injected by this slice. Each
harness needs verified forwarding and identity precedence first. No new key,
router, budget ledger, or usage queue consumer is added. Worker, pipeline,
reviewer and summary paths still require their own integration. Passing tests
and merging this source do not promote a gateway, install a binary, or resume
a fleet.
