# One active scheduler controller per host and OS user

Maestro's executable acquires a nonblocking OS-held lifetime lock before running
an active scheduler command. A second participating controller for the same
effective UID on the same host exits promptly before command setup, store
discovery, writable database/state opens, provider calls or worker dispatch.
Different `--store`, config, port, working directory, project UUID, or ready-label
values do not create another ownership namespace.

This deliberately supports one stable controller alongside candidate source,
builds and offline tests. Two simultaneous candidate/stable controllers are not
supported, even when their declared resources appear disjoint.

## Commands

| Acquires the scheduler lease | Behavior |
| --- | --- |
| `daemon` | Holds it across every in-process flow and shutdown drain. |
| `run`, including `--once` | Holds it for the whole single/multi-project run. |
| `night-start`, including `--dry-run` | Acquires before backend probes or report writes; its nested `runCmd` shares the lease. |
| `spawn` | Holds it during the manual worker-dispatch command. |
| `supervise` loop, `--once`, `--dry-run` | Holds it across standalone scheduling/provider evaluation. |

Exact `-h`/`--help` requests are exempt. Other command arguments use normal
parsing; a string flag whose value is `approve` is not an administrative bypass.

`supervise approve`, `reject`, and `reconcile-delivery` remain administrative
operations, including the existing syntax that places the subcommand after
flags. Configuration/project administration, status/history/logs, `serve`,
emergency/pause/drain/resume/stop/kill and one-shot maintenance commands retain
their existing behavior. They use their existing approval/state contracts.
Worker execution/cleanup/stream helpers do not acquire a scheduler lease.

This is **scheduler controller exclusivity**, not a universal mutation lock.
For example, `serve` is writable by default, and an approved administrative
operation can still execute its authorized effects while the scheduler runs.

## Fixed coordination location

The registry is `<OS-account-home>/.maestro-controller/owner.lock`. The home is
looked up from the OS account database by effective UID; environment `HOME`,
XDG variables, config/store locations and user-supplied flags do not select it.
There is no bypass flag, lock-root option, or fallback path. Failure to resolve
the account or establish a safe registry fails closed.

The home must be an absolute, clean, non-root directory owned by that UID and
not group/other writable. Every path component is opened without following
symlinks. Ancestors must belong to root or that UID and cannot be group/other
writable, except root-owned sticky directories such as `/tmp` that protect the
user's existing entry. The dedicated registry must be private (0700), and the lock a private
regular file (0600) owned by the same UID with no hardlink aliases. Existing
unsafe paths fail; Maestro does not chmod them or overwrite existing contents.

The descriptor uses `flock(LOCK_EX|LOCK_NB)` and close-on-exec. The kernel releases
ownership on normal exit or crash; an executed worker cannot inherit and retain
the controller lock. The registry and lock inode persist after release. **Do
not delete or replace the lock file to take ownership**: that can split the lock
namespace while a controller still owns the old inode. No PID, stale timestamp,
or lock-file contents are used to decide whether ownership is available.

On a conflict, stop the existing scheduler and let its normal drain/handoff
complete before starting another. This change does not terminate workers or
perform any global cleanup.

## Limits

- Only participating binaries under the same effective UID, OS host and shared
  filesystem namespace coordinate. Old binaries, another UID, another host, or
  containers with separate namespaces are not fenced. A malicious process under
  the same UID can manipulate its own files; this is not a security boundary.
- The lease describes the **controller's lifetime**, not every worker's lifetime.
  Workers may survive controller exit or a completed manual `spawn`. Acquiring
  the next controller lease does not prove those workers are gone or that a
  copied state is current. Existing process leases, state, worktrees and
  checkpoint/handoff checks remain required. There is no orphan reaping here.
- No cross-host queue fencing, per-project candidate partitioning, process/cgroup
  or emergency-cleanup isolation, artifact receipt, promotion procedure, or
  rollback compatibility is added. These remain separate R6 gates.
- The existing [candidate preflight](candidate-preflight.md) is still an offline
  declaration comparison and never authorizes an active candidate runtime.

Validation uses subprocess helpers and private fixture registries with an
internal test seam. Tests never acquire the real account registry or start a
daemon/provider/systemd workload.

Refs #1199.
