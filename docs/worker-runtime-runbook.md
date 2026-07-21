# Isolated worker runtime rollout

`worker_runtime.mode: isolated` moves each worker attempt into its own transient
systemd service and disk-backed scratch directory. The worker-visible `/tmp`,
`TMPDIR`, `TMP`, `TEMP`, `GOTMPDIR`, and `CARGO_TARGET_DIR` are private to that
attempt. Shared dependency caches remain unchanged, while Cargo build output is
never shared between workers.

The feature is opt-in. `legacy` remains the temporary rollback path while a
fleet is canaried.

```yaml
worker_runtime:
  mode: isolated
  scope: system
  scratch_root: /var/tmp/maestro-workers
  memory_max_mb: 4096
```

- `scope: system` matches the shipped system-scoped `maestro.service` and uses
  the existing non-interactive sudo boundary.
- `scope: user` is available for a user-scoped development service.
- `scratch_root` is a base directory. Maestro creates deterministic
  per-project roots below it, then one random private directory per attempt.
  A tmpfs/ramfs root is rejected at runtime. If the base already exists it must
  be a dedicated private (`0700`) directory; Maestro will not chmod a shared
  host directory such as `/var/tmp` itself.
- `memory_max_mb` optionally caps one worker. All isolated workers also enter
  `maestro-workers.slice`, configured at runtime with `MemoryHigh=70%` and
  `MemoryMax=80%` so build pressure cannot evict the control plane.

The system scope requires passwordless access to the exact `systemd-run` and
`systemctl set-property/stop/is-active` operations used by the worker lease.
Validate that boundary before enabling a canary; a failure is spawn-fatal and
does not fall back silently to an unowned worker.

## Canary and rollback

1. Enable `isolated` for one low-risk project and leave the rest on `legacy`.
2. Run normal build/test work, a timeout, a stopped attempt, a phase transition,
   and a respawn. Confirm Mission Control reports no worker-lease attention.
3. Run the forced-kill integration test on the target host:

   ```bash
   MAESTRO_SYSTEMD_INTEGRATION=1 go test ./internal/workerlease -run TestSystemdLeaseForcedKill -v
   ```

   It creates two exact leases, reparents a descendant in one, force-kills that
   lease, and verifies the neighboring worker and scratch remain intact.
4. Expand gradually, then run a 20-worker wave. After completion, the configured
   project scratch roots must contain no per-attempt directories and global
   `/tmp` growth must not track build-output size.

To roll back new launches, restore `mode: legacy`. Existing isolated attempts
retain their persisted lease identity and still stop/clean through that exact
lease. Keep the same `scratch_root` configured until reconciliation reports no
remaining leases or attention; do not replace this with an age/name sweep.

## Build benchmark

Compare the same repository revision, dependency-cache state, and concurrency
before and after enabling isolation. Capture at least three runs of each
representative build:

```bash
/usr/bin/time -v <build-command>
iostat -xz 1
```

Record median wall time, user CPU time, system CPU time, CPU utilization, and
device `%iowait`/latency. Also record global `/tmp` usage and the isolated
scratch filesystem usage before, during, and after the run. Document any
material regression before expanding the canary. Enlarging global tmpfs is not
an accepted mitigation.

Startup and every orchestration cycle enumerate only Maestro's deterministic
per-project scratch root. A valid lease with no live/recoverable session is
stopped and removed idempotently. Invalid or conflicting ownership is surfaced
as path-free Mission Control attention; Maestro does not delete it by age,
prefix, or executable name.
