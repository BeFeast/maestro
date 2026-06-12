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
   into place (same-filesystem rename; safe under a running process).
3. **Restarts the units** — `systemctl --user restart <unit>` for each
   configured unit. The unit's normal stop path runs, so existing **drain
   semantics are honored**: if your unit declares `ExecStop=... drain`,
   in-flight workers finish before the stop completes. Budget for this in
   `timeout_minutes`.
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
   visible in Mission Control, and sends a notification.

## Enabling it

```yaml
# maestro.yaml of the dogfood project
server:
  port: 8788            # gives the deploy a health endpoint to probe

self_deploy:
  enabled: true
  bin_path: /usr/local/bin/maestro   # default: path of the running binary
  units: ["maestro.service"]         # systemd user units to restart
  # health_url: http://127.0.0.1:8788/api/v1/state  # default: derived from server.port
  # health_token_env: MAESTRO_DASH_TOKEN            # default: server.auth.token_env
  # script: ./scripts/self-deploy.sh                # default: <local_path>/scripts/self-deploy.sh
  # timeout_minutes: 30                             # build+restart+verify budget (covers drain)
```

Prerequisites on the host:

- `systemd-run` / `systemctl --user` available (linger enabled for the user
  if the deploy must run without an active login session:
  `loginctl enable-linger $USER`).
- Go toolchain reachable from the orchestrator's `PATH` (it is handed down
  to the transient unit) or at `/usr/local/go/bin`.
- The user can write `bin_path` and its directory (no sudo: install the
  binary under the user's control, e.g. `~/.local/bin/maestro`, or make
  `/usr/local/bin/maestro` user-writable).

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
