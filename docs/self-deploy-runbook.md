# Self-Deploy Runbook (#698)

Opt-in, config-gated self-deploy of the maestro binary after a PR merges.
Default **OFF** — projects without a `self_deploy:` block see no behavior
change. Intended for the dogfood project, where merged maestro fixes should
reach the runtime without an operator manually building, installing, and
restarting units.

## How it works

After the orchestrator merges a PR (and after the optional version bump),
it launches `scripts/self-deploy.sh` through a **detached transient systemd
unit** (`systemd-run --user --collect --unit maestro-self-deploy-pr<N>-<ts>`).
Detachment is the load-bearing part: the script restarts the very units that
run the orchestrator, and must survive that restart.

The script then:

1. **Builds from merged main** — fetches `origin/main`, checks it out into a
   temporary git worktree, reads the `VERSION` file, and builds with
   `-ldflags "-X main.version=<VERSION>+g<shortsha>"` (version stamping per
   #682, extended with the commit SHA as semver build metadata).
2. **Installs atomically** — stages the new binary next to the target
   (`<bin>.next`), preserves the current binary as `<bin>.prev`, and renames
   into place (same-filesystem rename; safe under a running process). When
   `install_via_sudo: true`, these file ops run through `sudo -n` so a
   root-owned `bin_path` (e.g. `/usr/local/bin/maestro`) can be updated by the
   unprivileged deploy user without losing atomic-rename semantics (#711).
3. **Restarts the units** — for each configured unit, scoped by `scope`
   (#716): `systemctl --user restart <unit>` for `user` (default) or
   `sudo -n systemctl restart <unit>` for `system` units (e.g. the Loki fleet,
   where maestro runs as `User=god` system units). Either way the unit's normal
   stop path runs, so existing **drain semantics are honored**: if your unit
   declares `ExecStop=... drain`, in-flight workers finish before the stop
   completes. Budget for this in `timeout_minutes`.
4. **Verifies health** — the installed CLI must report the stamped version
   (`maestro version` → `maestro v<VERSION>+g<shortsha>`), every unit must be
   `active`, and (when a health URL is configured) the **running process**
   must report the stamped version via the `version` field on
   `/api/v1/state` (or `/api/v1/fleet`) before the deadline.
5. **Rolls back on failure** — restores `<bin>.prev`, restarts the units
   again, and records the rollback reason. `.prev` is kept either way for
   manual rollback.
6. **Writes a result file** — `<state_dir>/self-deploy-result.json`. The next
   orchestrator cycle (running the new binary, on success) consumes it,
   records a **supervisor finding** (deployed version / rollback reason)
   visible in Mission Control, and sends a notification. A `failed` /
   `rolled_back` result surfaces as an **error-tone operator state**
   ("Self-deploy failed") in `/api/v1/fleet`, so an undeployed-but-merged host
   is loud rather than buried in journald (#711).

## Enabling it

```yaml
# maestro.yaml of the dogfood project
server:
  port: 8788            # gives the deploy a health endpoint to probe

self_deploy:
  enabled: true
  bin_path: /usr/local/bin/maestro   # default: path of the running binary
  install_via_sudo: true             # #711: stage/rename/rollback bin_path via `sudo -n` (root-owned target)
  scope: user                        # #716: user (default, systemctl --user) | system (sudo -n systemctl, for system units)
  units: ["maestro.service"]         # systemd units to restart
  # health_url: http://127.0.0.1:8788/api/v1/state  # default: derived from server.port
  # health_token_env: MAESTRO_DASH_TOKEN            # default: server.auth.token_env
  # script: ./scripts/self-deploy.sh                # default: <local_path>/scripts/self-deploy.sh
  # timeout_minutes: 30                             # build+restart+verify budget (covers drain)
```

### Scope: user vs system units (#716)

`scope` selects which systemd manager owns the units:

- **`user`** (default, back-compat) — per-user units, restarted with
  `systemctl --user restart` / `systemctl --user is-active`. This was the only
  mode before #716 (the workshop@opti deployment).
- **`system`** — system units (`User=god`, managed by the system manager),
  restarted with `sudo -n systemctl restart` and checked with
  `systemctl is-active` (no `--user`). This is the **Loki fleet** layout, where
  maestro runs as `maestro-<project>{,-supervise,-web}.service` system units.
  System scope needs **passwordless sudo** for `systemctl restart` (the
  read-only `is-active` poll does not); the deploy preflights it and fails fast
  with a recorded finding if it is missing. A minimal sudoers grant:

  ```
  # /etc/sudoers.d/maestro-self-deploy  (validate with: visudo -c -f <file>)
  god ALL=(root) NOPASSWD: /usr/bin/systemctl restart maestro-*
  ```

  On Loki, combine with `install_via_sudo: true` (root-owned
  `/usr/local/bin/maestro`) — the two are independent but both used there.

Prerequisites on the host:

- `systemd-run` / `systemctl --user` available (linger enabled for the user
  if the deploy must run without an active login session:
  `loginctl enable-linger $USER`).
- Go toolchain reachable from the orchestrator's `PATH` (it is handed down
  to the transient unit) or at `/usr/local/go/bin`.
- **Write access to `bin_path`** — the deploy runs as the unprivileged user
  that owns the maestro units. Pick one:
  - **User-writable target** (simplest, no sudo): point `bin_path` at a
    directory the user owns, e.g. `~/.local/bin/maestro`, and make the unit
    `ExecStart` use that path. Leave `install_via_sudo` unset/false.
  - **Root-owned target via sudo** (`install_via_sudo: true`): keep
    `bin_path` at a root-owned path such as `/usr/local/bin/maestro` and grant
    the deploy user **passwordless sudo**. The script then runs the privileged
    file ops (`install`/`cp`/`mv`/`rm` of `<bin>`, `<bin>.next`, `<bin>.prev`)
    through `sudo -n`, preserving the same-filesystem atomic-rename install and
    rollback. The deploy fails fast with a recorded finding if passwordless
    sudo is unavailable. A minimal sudoers grant:

    ```
    # /etc/sudoers.d/maestro-self-deploy  (validate with: visudo -c -f <file>)
    god ALL=(root) NOPASSWD: /usr/bin/install, /bin/cp, /bin/mv, /bin/rm
    ```

> Without one of these, the install step fails (`staging
> /usr/local/bin/maestro.next failed`) and the merge self-deploys nothing —
> the symptom that motivated #711.

## Verifying a rollout

After a merge, within `timeout_minutes`:

```bash
maestro version                                  # maestro v<VERSION>+g<shortsha> of merged main
curl -s http://127.0.0.1:8788/api/v1/state | grep '"version"'
systemctl --user status maestro.service
journalctl --user -u 'maestro-self-deploy-*' -n 200   # deploy log
```

The supervisor finding appears in the state API under
`supervisor_decisions` with a `self-deploy-…` ID and in Mission Control.

## When it rolls back

A deliberately broken build or failed health check leaves:

- the previous binary restored at `bin_path` (and still kept at
  `<bin_path>.prev`),
- units restarted and active on the old version,
- a `rolled_back` finding + notification carrying the reason.

Inspect the deploy log, fix the regression, merge again:

```bash
journalctl --user -u 'maestro-self-deploy-*' --since -2h
```

## Manual rollback

```bash
cp -p /usr/local/bin/maestro.prev /usr/local/bin/maestro
systemctl --user restart maestro.service
maestro version
```

## Failure modes

| Symptom | Meaning | Action |
|---|---|---|
| Finding `self-deploy: failed … staging <bin>.next failed` | The deploy user cannot write `bin_path` (root-owned target, no sudo) | Set `install_via_sudo: true` + grant passwordless sudo, or move `bin_path` to a user-writable path (#711) |
| Finding `self-deploy: failed … passwordless sudo is unavailable` | `install_via_sudo` is set but `sudo -n` does not work for the deploy user | Add a NOPASSWD sudoers grant for the install file ops (see Enabling it) |
| Finding `self-deploy: failed … no .prev` | First deploy failed before any previous binary existed | Build + install manually once, re-merge |
| Finding `rolled_back … health check timed out` | New binary started but never reported the stamped version | Check `journalctl --user -u maestro.service`; the regression is in the merged code |
| No finding, no restart | Trigger failed (script missing, systemd-run unavailable) | Orchestrator log shows `self-deploy trigger failed …`; a ⚠️ notification is sent |
| Units inactive after rollback | Rollback restart also failed | `systemctl --user start maestro.service` by hand; binary on disk is the previous version |

## Interaction with `deploy_cmd`

`self_deploy` is independent of the generic `deploy_cmd` hook: `deploy_cmd`
runs synchronously inside the orchestrator (fine for deploying *other*
projects), while `self_deploy` is detached because maestro cannot wait on a
deploy that restarts maestro. Both may be configured; the self-deploy is
triggered first and proceeds in the background.
