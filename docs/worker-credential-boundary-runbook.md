# Worker credential boundary and migration (#888)

Maestro workers use one service-level private environment file. Generated
`*-run.sh` files contain only variable names and that file's path; no worker gets
a per-slot secret copy. Immediately before starting a worker, Maestro opens the
file without following symlinks, verifies ownership and mode, allow-lists the
provider variables, removes stale values inherited from tmux, and injects the
current values only into the worker process environment. Combined worker output
is redacted from that same in-memory snapshot before JSONL or log persistence.

The production boundary is an operator-owned file named by
`MAESTRO_WORKER_CREDENTIALS_FILE`. If any known provider value is present in the
daemon environment without that reference, worker spawn fails closed. A worker
that uses CLI-native authentication and has no ambient provider values may run
without the file. Maestro never creates a fallback, project-local, or per-slot
credential copy.

## File contract

- The file is a regular file owned by the Maestro service user, mode `0600` or
  stricter, below an owner-only directory owned by that same user (mode `0700`
  or stricter). Symlinks in the path are rejected.
- Use simple `KEY=value` assignments. Single- or double-quoted values are
  accepted. Only `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY`,
  `ANTHROPIC_AUTH_TOKEN`, `CLIPROXY_API_KEY`, `GEMINI_API_KEY`,
  `OPENAI_BASE_URL`, and `OPENAI_API_KEY` are passed to a worker; unrelated
  assignments are ignored.
- A secret manager must write/rotate the file directly. Never place a value in a
  shell argument, issue, PR, note, unit, journal message, or pasteback.

## systemd migration

This is an operator change and requires a controlled daemon restart. Do not
perform it while merely reviewing the code change.

1. Create an owner-only directory and have the authoritative secret manager
   write the private file there. Do not construct it with a command that embeds
   values in argv or shell history.
2. Add a unit drop-in that contains references only (the paths below are an
   owner-home example; use the service user's private location):

   ```ini
   [Service]
   EnvironmentFile=%h/.config/maestro/private/worker-proxy.env
   Environment=MAESTRO_WORKER_CREDENTIALS_FILE=%h/.config/maestro/private/worker-proxy.env
   ```

3. In the same change, remove every provider token/key from literal
   `Environment=` lines and from any other drop-in. Keep non-secret settings such
   as `PATH` unchanged.
4. Let the single daemon's normal SIGTERM path drain active workers, then reload
   and restart `maestro.service` in its actual scope (`systemctl --user` for a
   user unit, `sudo systemctl` for a system unit). Do not kill the service cgroup
   or individual workers as a migration shortcut.
5. Verify only metadata and references: owner/mode/type of the private file,
   the `EnvironmentFile` path, daemon health, and Fleet health. Do **not** use
   `systemctl show -p Environment`, `env`, `set -x`, or any diagnostic that prints
   values.

Startup reconciliation removes legacy `*-run.env` copies and the obsolete
`<state-dir>/credentials/worker-proxy.env` project-local copy, strips old runner
exports/source lines, and masks known legacy values in text state, prompts,
postmortems, audit JSONL, structured output, and logs. Log masking is same-inode
and same-length, so an active worker's open append descriptor remains valid; the
scrub never signals or kills a process. It also repairs recognized worker text
artifacts to owner-only modes and the worker log directory to `0700`, even when
there is no currently known value to mask.

## Rotation, canary, and zero-copy proof

After the code and unit migration are installed, the operator still must:

1. Rotate every affected value through its authoritative private owner.
2. Run one authenticated worker/proxy canary and verify success without echoing
   request credentials or response headers.
3. Run an approved private scan that reads the replacement values from the
   credential file internally (never as command arguments) and reports counts
   only. The replacement value must be absent from the repository, worker
   scripts, prompts, state/audit files, JSONL/logs, Fleet/API responses, process
   arguments, and rendered unit text. The one approved match is the private
   credential file itself.

The synthetic regression suite exercises the same zero-copy invariant without
using production values.

## Rollback

Keep a reference-only backup drop-in and the prior private credential generation
until the authenticated canary passes. On failure, drain, restore the prior
private-file reference, reload, restart, and re-run the canary. Never roll back
to literal `Environment=` secrets or a per-worker env file. Rotation and the
live canary remain operator-owned even when the code rollback succeeds.
