# Interrupted Delivery Reconciliation Runbook

Use this runbook only when an approval-gated `deploy_project` row remains in
`executing` after the Maestro process or its delivery runner was interrupted.
Maestro never replays such a row automatically: `executing` is a durable lease,
not a retry queue.

## Safety boundary

`maestro supervise reconcile-delivery` is a local, operator-only CLI. Run it
from an authenticated shell on the project's authoritative execution host. It
is not exposed through Mission Control, called by the daemon, or scheduled by a
timer.

Before any reconciliation:

1. Stop and inspect the original runner/process with the project's process
   manager. Do not continue until you can assert it is gone. This is an
   operator assertion; the CLI cannot prove the lifetime of a process that died
   with another host or service manager.
2. Read the authoritative approval row and record its full `merged_sha`.
3. Inspect the target with the project's read-only release-identity/status
   mechanism. Do not run the delivery entrypoint manually.
4. Select exactly one closed outcome below. If the evidence is inconclusive,
   run no reconciliation command and leave the row `executing`.

There is deliberately no `unknown` outcome and no free-text reason flag.

## Outcomes

| Outcome | Evidence required | Durable transition |
|---|---|---|
| `verified` | Original runner is gone; target reports the exact approved full SHA; current delivery config still matches the approved digest; the pinned verifier exits 0 | `executing -> executed` |
| `not-applied` | Original runner is gone; deployment did not reach the target; target is explicitly known safe | `executing -> execution_failed` |
| `remediated-failed` | Original runner is gone; an operator completed rollback/remediation after a partial or failed delivery; target is explicitly known safe | `executing -> execution_failed` |

### Verified target

Pass the exact full SHA reported by the target, not a branch name, tag, or
abbreviation:

```bash
maestro supervise reconcile-delivery <approval-id> \
  --config <project-config.yaml> \
  --approvals-db <maestro.db> \
  --outcome verified \
  --observed-sha <full-approved-and-observed-sha> \
  --assert-runner-gone
```

The command re-reads the authoritative SQLite row, checks `status=executing`,
the repo binding, exact SHA, and current approval digest. It then fetches that
SHA into a new isolated checkout and directly runs only the configured
argument-free `verify_command`. It never runs `delivery.command`. Verifier
output is discarded. Only a zero verifier exit atomically records the closed
`operator_reconcile/verified` result.

A wrong SHA, config drift, unsafe/missing verifier, checkout failure, timeout,
or non-zero verifier leaves the row `executing`. Investigate again; do not
convert uncertainty into `not-applied` unless the target has independently been
proved safe.

For a store-first fleet, select the project directly from the config store
instead of exporting a temporary YAML file:

```bash
maestro supervise reconcile-delivery <approval-id> \
  --config-store <maestro.db> \
  --config-store-project <project-name> \
  --approvals-db <maestro.db> \
  --outcome verified \
  --observed-sha <full-approved-and-observed-sha> \
  --assert-runner-gone
```

`--config` and `--config-store` are mutually exclusive. A store lookup failure
never falls back to YAML, and a multi-project store requires
`--config-store-project`.

### Confirmed not applied

```bash
maestro supervise reconcile-delivery <approval-id> \
  --config <project-config.yaml> \
  --approvals-db <maestro.db> \
  --outcome not-applied \
  --assert-runner-gone \
  --assert-target-safe
```

This path runs no checkout, deploy command, or verifier. The two flags are
explicit operator assertions, not automatic probes.

### Remediated failed delivery

After completing the project's documented rollback/remediation and proving the
target safe:

```bash
maestro supervise reconcile-delivery <approval-id> \
  --config <project-config.yaml> \
  --approvals-db <maestro.db> \
  --outcome remediated-failed \
  --assert-runner-gone \
  --assert-target-safe
```

This also runs no checkout, deploy command, or verifier. It records only the
closed outcome and structured timestamps.

## Concurrency, persistence, and retry

The final write is an atomic `WHERE status='executing'` transition. Concurrent
operators may both perform a read-only verified check, but exactly one can close
the row; every loser receives `approval is not in status=executing`. The record
hash binds the project/repo/state-directory identity, SQL status, strict result,
and canonical audit history.

SQLite is authoritative. `state.json` is updated as a best-effort compatibility
mirror after the commit; a mirror write failure is reported as recoverable and
does not undo the terminal ledger row. No command, local checkout path, target
credential, stdout/stderr, error text, or operator note is persisted.

`not-applied` and `remediated-failed` are terminal failures. To try delivery
again, create and approve a fresh merge/spec generation; never edit the ledger,
reset a terminal row to `approved`, or reuse the interrupted runner.
