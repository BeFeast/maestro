# Native role containment (source contract)

Strict managed Claude workers, worker phases, supervisors and reviewers share
one finite launcher. The host runner observes the admitted native registration,
live controller lease/revision, loaded binaries, gateway PID/listeners, runtime
configuration and active credential selection. The existing deterministic
systemd **system** service is then launched into a preprovisioned namespace.
Its unprivileged monitor holds the namespace claim and invokes bubblewrap.

This source does not install a namespace, change nft rules, create a production
unit, provision a token, or resume the fleet. Those operations belong to the
reviewed R9 package. A successful source fixture is not installed readiness.

## Installed inputs

`Manifest.containment` maps each worker slot and auxiliary role to a root-owned
profile `{path, sha256}`. `NativeContainmentProfile` is the authoritative schema.
It binds the project, UID/GID, namespace device/inode, complete nft ruleset
digest, inference listener IPv4 URL, Forgejo IPv4 address/repository, memory
limit, private claim/worktree/scratch directories, root-owned executable hashes,
and finite root-owned read-only runtime/cache mounts. Actual loaded Maestro and
Claude bytes must match the manifest. Read-only bundles/caches must be staged
as immutable reviewed R9 inputs; no arbitrary host home or credential directory
is accepted as a mount target.

The inference listener is observed as belonging to the same gateway PID as the
host loopback management endpoint. The active binding observer separately
compares the exact configured finite credential/model inventory with
`credential_selection_only` evidence. Unmanaged credential inventory count is
informational. Account ownership still comes from reviewed provisioning
evidence, not from a configuration digest or alias alone.

`NativeNftRules(gatewayURL, forgejoIP)` produces the finite provisioning rules:
IPv4 loopback within the exclusive namespace, exact gateway TCP port and exact
Forgejo TCP 443; other output, IPv6, DNS, and forwarding are denied. R9 reads
back `nft --json --stateless list ruleset`; `NativeRulesDigest` binds the entire
result after removing nft's metainfo. The source launcher only observes this
state through pinned `sudo nsenter nft`; it cannot install its own firewall.

The dedicated namespace is atomically claimed through an owner-only `flock`
outside the child filesystem. A persisted prior cgroup claim must be empty
before reuse even if the monitor has disappeared or Maestro has restarted.
The monitor persists boot ID, systemd InvocationID, exact native/unit/cgroup
identity and launch intent before starting the native child. Recovery requires
both a dead cgroup and inactive exact service before recording termination.
Lost local output is `local_output_unknown`; it never authorizes another paid
invocation or fabricates success.

## Filesystem, tools and delivery

The child sees a full HTTPS clone at `/work`, private scratch and HOME, fresh
proc/dev, and explicit read-only runtime/cache inputs. It receives a generated
environment with the managed Anthropic token and child listener URL. Host
provider/SSH/cloud/proxy/Git credentials and configuration are not inherited.
Workers get exactly `Bash,Read,Edit,Write,Glob,Grep`; auxiliary roles have no
tools and one turn. This binds the root native session; it does not claim
universal attribution of arbitrary recursively spawned agents.

Bubblewrap uses mount/PID/user/IPC/UTS/cgroup isolation and drops all caps. An
amd64 seccomp filter, installed after namespace setup, forbids namespace and
mount mutation. `clone3` returns ENOSYS so normal threads use filtered `clone`;
ordinary fork/thread clone flags remain usable. Other architectures hold.
No host sysctl modification is necessary.

Full clones have no linked Git directory or shared alternates. Config, hooks,
commondir and alternates are protected by mounts inside the native child.
Every subsequent host Git call against a registered clone runs in a separate
offline bubblewrap filesystem/network boundary. The registry and pinned
executables/filter live outside the writable clone and survive deletion of
`.git`. Checkpoint/prompt/pipeline artifact file access uses beneath-root
`openat2` with no symlinks. Missing native clones are retained as a recovery
hold rather than reconstructed from an unrelated parent branch.

Host recovery can inspect, switch, checkpoint and commit locally. Host-side
network Git operations on these clones return
`native_git_network_requires_native_tool`; a native repair role performs
fetch/push using its scoped credential. The finite `_native-forgejo-credential`
helper answers only the exact canonical repository HTTPS URL. The
`_native-forgejo-pr` helper accepts `{title, body, head, base}` JSON on stdin,
creates a PR on the configured repository with `base=main` and the assigned
`feat/` branch, follows no redirects and never merges. Secrets are not argv or
files. An uncertain response remains unknown, without automatic replay.

The repository credential comes only from
`MAESTRO_FORGEJO_REPOSITORY_TOKEN` in the authoritative private worker
credential file. Its profile hash is selection evidence, **not an ACL**.
Worker launch additionally requires a root-owned pinned
`forgejo_authorization` attestation containing version 1, repository,
credential_sha256, worker_login, observed_at/expires_at, `admin=false`, and
repository_only/main_push_denied/merge_denied/policy_files_write_denied true,
plus a pinned `server_probe` receipt. This is trusted R9 provisioning evidence
after real server-side negative probes. It is not created by this launcher and
cannot be replaced by hashing the existing broad administrator credential.

## Validation and remaining operational acceptance

Source tests cover envelope boundaries, finite arguments/env, credential
helper destination/length checks, live binding drift, durable terminal
snapshots, service argument assembly and exact existing lease ownership.

Run private kernel fixtures explicitly on Linux amd64 with bubblewrap/nft/ip:

```
MAESTRO_NATIVE_KERNEL_TESTS=1 go test ./internal/aiexecution ./internal/worker ./internal/tmuxsession
```

They exercise actual shared bubblewrap mounts/seccomp and an offline Go test;
Git add/commit/status plus malicious metadata symlinks and aliases; checkpoint
read/write symlink denial; namespace claim concurrency and surviving cgroup;
and anonymous user/net namespaces with a private veth and actual nft policy.
The firewall fixture has live allowed gateway/Forgejo listeners and live
forbidden alternate-port/provider listeners. It touches no production network.

Systemd tests in this slice are synthetic command/ownership tests. Installed
R9 acceptance must still launch the exact preprovisioned service/profile,
prove native entry UID/netns/mounts/caps/seccomp, run role/phase/restart/cancel
probes and authority outcome settlement, and validate repository credentials
and branch/protected-file/merge restrictions. Product dependencies are offline
reviewed inputs. Hedroom prebuild/typecheck is supported; its ordinary Next
bundle still requires a separately reviewed Google-font input or ordinary CI.
No internet exception is added to make that bundle pass.
