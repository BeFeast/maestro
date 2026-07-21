# Remote worker SSH spike

This runbook covers the opt-in v1 adapter that runs one project's coding agent
and git worktree on a remote host while issue selection, SQLite state, tmux
ownership, logs, and Mission Control remain on the control-plane host.

The adapter is intentionally one-shot: the local worker process is an SSH
connection, and the remote command creates or reuses one deterministic
worktree before starting the configured agent CLI. It is not a lease service,
runner registry, or second orchestrator.

## Boundaries and current limitations

- `remote_runner.enabled` defaults to `false`. Projects without the block run
  exactly as before.
- The control plane keeps a lightweight local shadow worktree so existing
  tmux, process-lease, state, and branch ownership checks remain intact. The
  agent sees only `<remote_runner.worktree_base>/<slot>`.
- The remote bootstrap receives the issue prompt, branch name, runner paths,
  and agent argv. It receives no Maestro database path, DSN, or control-plane
  credential file.
- The SSH client starts with a minimal environment (`HOME`, user identity,
  `PATH`, and `SSH_AUTH_SOCK` only), with forwarding disabled. Ambient
  control-plane provider/GitHub variables therefore cannot cross the boundary.
- The runner reads only its own optional private credential file through
  `maestro _worker-exec`. Do not copy the control-plane environment or secret
  file to the runner.
- The v1 adapter rejects `auto_rebase: true`, `validation_contract: true`, the
  phase pipeline, and lifecycle/tool hooks. These paths still inspect or mutate
  the local shadow worktree. Do not put `pipeline:full` or `pipeline:advised`
  labels on the spike issue.
- Deterministic pre-worker research/plan/test-mapping scans are skipped for a
  remote dispatch so they neither load the control plane nor generate files
  that exist only in the shadow worktree.
- A network break can leave the remote agent alive after the local SSH process
  exits. Use the exact-worktree cleanup procedure below before retrying.

The supported spike target is the issue-to-open-PR path. Merge-time rebases,
remote capacity scheduling, and multi-runner selection are out of scope.

## Provision the runner

Install these tools in the non-interactive SSH user's `PATH`:

- Bash, Git, and GitHub CLI (`gh`)
- the same Maestro build as the control plane
- the configured agent CLI (`codex`, `claude`, `pi`, or another backend)
- the heavy project's build/test toolchain

Create one persistent clone and a separate worktree parent. Use paths owned by
the SSH user, not shared writable directories.

```bash
export RUNNER_REPO=/srv/maestro/repos/PROJECT
export RUNNER_WORKTREES=/srv/maestro/worktrees/PROJECT

install -d -m 700 "$(dirname "$RUNNER_REPO")" "$RUNNER_WORKTREES"
git clone git@github.com:OWNER/REPO.git "$RUNNER_REPO"
git -C "$RUNNER_REPO" fetch origin
```

Configure Git authentication for both `git fetch/push` and `gh pr create`.
Prefer a short-lived GitHub App installation token or equivalent minted token.
If environment credentials are required, write them to an owner-only file on
the runner from the credential broker; never paste values into project config,
the repository, logs, or issue comments.

```bash
export RUNNER_CREDENTIAL_DIR=/run/user/UID/maestro-runner
export RUNNER_CREDENTIAL_FILE="$RUNNER_CREDENTIAL_DIR/worker.env"

install -d -m 700 "$RUNNER_CREDENTIAL_DIR"
umask 077
credential-mint-command >"$RUNNER_CREDENTIAL_FILE"
chmod 600 "$RUNNER_CREDENTIAL_FILE"

maestro _worker-exec --credentials-file "$RUNNER_CREDENTIAL_FILE" -- gh auth status
maestro _worker-exec --credentials-file "$RUNNER_CREDENTIAL_FILE" -- gh auth setup-git
maestro _worker-exec --credentials-file "$RUNNER_CREDENTIAL_FILE" -- git -C "$RUNNER_REPO" fetch origin
maestro _worker-exec --credentials-file "$RUNNER_CREDENTIAL_FILE" -- codex --version
```

The file parser accepts only worker credential names, including `GH_TOKEN`,
`GITHUB_TOKEN`, and the supported provider token variables. Values are read at
process start and output is redacted. If the CLIs use their own secure login
store, omit `credentials_file` entirely.

From the control-plane host, verify non-interactive SSH without forwarding the
control-plane agent or ports:

```bash
ssh -T \
  -o BatchMode=yes \
  -o ClearAllForwardings=yes \
  -o ForwardAgent=no \
  runner@runner.example.internal \
  'command -v bash git gh maestro codex'
```

## Turn remote execution on and off

Add the block to the project's stored configuration. The three runner paths
are runner-side POSIX paths. `ssh_args` is an argv list, not a shell string.

```yaml
auto_rebase: false

remote_runner:
  enabled: true
  target: runner@runner.example.internal
  repo_path: /srv/maestro/repos/PROJECT
  worktree_base: /srv/maestro/worktrees/PROJECT
  base_branch: main
  ssh_command: ssh
  ssh_args:
    - -o
    - ConnectTimeout=15
    - -o
    - ServerAliveInterval=15
    - -o
    - ServerAliveCountMax=3
  maestro_command: /usr/local/bin/maestro
  credentials_file: /run/user/UID/maestro-runner/worker.env
```

Use the normal stored-project workflow to validate and apply the portable
project document. Supply the `project_id`, config fingerprint, and baseline
fingerprint returned by `plan` to the matching `apply`:

```bash
maestro project plan --file "$PROJECT_FILE" --db "$MAESTRO_DB" --json

maestro project apply \
  --file "$PROJECT_FILE" \
  --db "$MAESTRO_DB" \
  --confirm "$PROJECT_ID" \
  --fingerprint "$CONFIG_FINGERPRINT" \
  --baseline "$BASELINE_FINGERPRINT" \
  --json
```

To roll back new dispatches, set only:

```yaml
remote_runner:
  enabled: false
```

Changing the flag does not terminate an already-running SSH worker. Let it
finish or stop that exact slot through the normal operator flow, then disable
the flag. A later local dispatch uses the ordinary local runner path.

## Run and record the spike

Choose one heavy-project issue that has the normal ready label and does not opt
into the phase pipeline. Record the issue number, resulting PR number, slot,
remote worktree, start/end time, and the aggregate measurements below. Keep raw
measurement files and worker logs outside the repository and do not paste
credential-bearing output into GitHub.

On the runner, confirm the live process and branch use the remote worktree:

```bash
git -C "$REMOTE_WORKTREE" rev-parse --show-toplevel
git -C "$REMOTE_WORKTREE" symbolic-ref --short HEAD
git -C "$REMOTE_WORKTREE" status --short
```

The worker is successful for this spike when the agent pushes that branch and
opens a PR while Mission Control continues to answer from the control-plane
host.

### Control-plane measurements

Capture a short idle baseline, then run the same sample loop during the remote
worker. Use the actual Fleet URL and any required local authentication without
printing its value.

```bash
CONTROL_PID=$(pgrep -xo maestro)
SAMPLES=60

for sample in $(seq 1 "$SAMPLES"); do
  recorded_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  cpu=$(ps -p "$CONTROL_PID" -o %cpu= | tr -d ' ')
  fleet=$(curl -sS -o /dev/null \
    --connect-timeout 2 \
    --max-time 5 \
    -w '%{http_code} %{time_total}' \
    http://127.0.0.1:8786/api/v1/fleet)
  printf '%s cpu_pct=%s fleet=%s\n' "$recorded_at" "$cpu" "$fleet"
  sleep 5
done
```

Summarize, rather than paste the raw samples:

- idle and during-run CPU average and maximum for the Maestro process
- Fleet API p50/p95/max latency and count of non-2xx/timeouts
- whether the Mission Control project/workers pages remained interactively
  usable during the agent's heaviest build/test phase
- remote worker wall time and whether an SSH reconnect or cleanup was needed

## Failure modes

| Failure | Signal | Recovery |
| --- | --- | --- |
| GitHub or provider auth | `GitHub authentication unavailable`, fetch/push denial, or agent 401/403 | Mint a fresh runner-scoped credential file, re-run the `_worker-exec` preflights, and retry the same slot so the exact remote worktree is reused. |
| Missing CLI or wrong non-interactive `PATH` | `missing required CLI` and exit 127 | Install the named tool in the SSH user's visible `PATH`, or set an absolute `maestro_command`/backend command. Re-run the preflight before redispatch. |
| Network drop | Local SSH/tmux worker exits while a remote process may still be running | Inspect the exact remote worktree's processes. Terminate only confirmed PIDs, then retry; the bootstrap reuses a matching branch/worktree. |
| Leftover worktree | `leftover worktree branch mismatch` or Git reports the branch already checked out | Preserve and inspect the directory. If it belongs to the slot, either reuse the matching branch or perform the exact cleanup below. Never delete an unverified directory. |

## Clean an exact remote zombie

Resolve the worktree and branch from the worker log/state before taking action.
Do not use broad `pkill` patterns or recursive deletion.

```bash
ps -eo pid=,ppid=,args= | rg --fixed-strings "$REMOTE_WORKTREE"

# After verifying each PID belongs to this exact worktree:
kill -TERM "$REMOTE_PID"
sleep 5
if kill -0 "$REMOTE_PID" 2>/dev/null; then
  # Use KILL only if the same verified PID remains after the grace period.
  kill -KILL "$REMOTE_PID"
fi

git -C "$RUNNER_REPO" worktree remove --force "$REMOTE_WORKTREE"
git -C "$RUNNER_REPO" worktree prune
```

Delete the runner-local branch only after confirming all useful work is pushed
and no PR or retry owns it:

```bash
git -C "$RUNNER_REPO" branch -D "$REMOTE_BRANCH"
```

The prompt file is removed by a normal remote-script exit. After an abrupt
host failure, the only expected prompt artifact is the exact slot file under
`<worktree_base>/.maestro-prompts/`; inspect and remove that file explicitly.
