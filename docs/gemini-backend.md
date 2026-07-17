# Gemini CLI Backend

Maestro supports Google's Gemini CLI (`@google/gemini-cli`) as a first-class backend alongside Claude, Codex, and Cline.

## Installation

```bash
npm install -g @google/gemini-cli
```

Verify installation:

```bash
gemini --version
```

## Authentication

Authenticate with Google:

```bash
gemini auth
```

Or have your secret manager populate the `GEMINI_API_KEY` environment variable.
Do not put the value in a shell argument or history. For a direct, interactive
Gemini CLI session (not a Maestro daemon), read it silently and export the
variable only for that session:

```bash
read -rsp 'Gemini API key: ' GEMINI_API_KEY && echo
export GEMINI_API_KEY
```

Maestro fails closed when provider variables are ambient without an explicit
private-file boundary. For Maestro workers, use the service file below instead
of relying on a shell export.

For the systemd service, put the assignment in Maestro's owner-only service
credential file and reference that file from a drop-in. The unit contains paths
and names only — never a literal key:

```ini
[Service]
EnvironmentFile=%h/.config/maestro/private/worker-proxy.env
Environment=MAESTRO_WORKER_CREDENTIALS_FILE=%h/.config/maestro/private/worker-proxy.env
Environment=PATH=/usr/local/bin:/usr/bin:/home/user/.npm-global/bin
```

See the [worker credential boundary runbook](worker-credential-boundary-runbook.md)
for ownership/mode, migration, rotation, and rollback requirements.

## Configuration

### As default backend

```yaml
repo: owner/repo
model:
  default: gemini
  backends:
    gemini:
      cmd: gemini
```

### With extra arguments

```yaml
model:
  default: gemini
  backends:
    gemini:
      cmd: gemini
      extra_args: ["--sandbox", "none"]
```

### Alongside other backends

```yaml
model:
  default: claude
  backends:
    claude:
      cmd: claude
    gemini:
      cmd: gemini
```

Then use `model:gemini` label on specific issues to route them to Gemini.

## How it works

The Gemini backend passes the worker prompt to the CLI via the `-p` flag:

```
gemini -p "<prompt content>" [extra_args...]
```

- Prompt content is read from the prompt file and passed inline as a `-p` argument
- Extra arguments from config are appended after the prompt
- The working directory is set to the issue's git worktree
- Worker runner scripts export `MAESTRO_WORKTREE` and wrap `rg`, `find`, and `grep` so broad-root searches warn and point back to the worktree
- No stdin redirection is used (unlike the Codex backend)

## Per-issue routing

Label a GitHub issue with `model:gemini` to run it with Gemini instead of the default backend:

```
issue #42 labels: enhancement, model:gemini  -> runs with Gemini
issue #43 labels: enhancement                -> runs with default
```

When `routing.mode: auto` is enabled, the LLM router also returns a structured
`task_type` classification: `refactor`, `bugfix`, `test`, `vision`, `design`,
`docs`, or `infra`. You can map task types to preferred backends before the
router's free-text backend pick is used:

```yaml
routing:
  mode: auto
  task_type_backends:
    vision: claude
    design: claude
```

Manual routing is unchanged: with `routing.mode: manual`, `task_type_backends`
is inert and backend selection remains `model:<backend>` labels or
`model.default`.

## Test coverage

The Gemini backend has unit tests covering:

- **Command construction** (`TestBuildWorkerCmd_Gemini`): verifies `-p` flag, prompt content, extra args, and working directory
- **Default command** (`TestBuildWorkerCmd_GeminiDefaultCmd`): verifies "gemini" is used when no `cmd` is configured
- **Argument ordering** (`TestBuildWorkerCmd_GeminiArgOrder`): verifies exact arg structure: `-p <prompt> <extra_args...>`
- **Error handling** (`TestBuildWorkerCmd_GeminiPromptFileError`): verifies proper error when prompt file is missing
- **Known backends** (`TestKnownBackends`): verifies "gemini" is in the known backends list
- **Config parsing** (`TestParse_GeminiDefaultBackend`, `TestParse_ModelConfigExplicit`): verifies YAML config with Gemini as default and with extra args
- **Label routing** (`TestBackendFromLabels_AllKnownBackends`): verifies `model:gemini` label is recognized
- **Backend resolution** (`TestResolveBackend_GeminiAsDefault`, `TestResolveBackend_GeminiLabelOverridesDefault`, `TestResolveBackend_LabelTakesPrecedenceOverAutoRouting`): verifies Gemini works as default and via label override
- **Backend selection**: "gemini" is a first-class `model.default` / `model:gemini` backend in a portable project config applied via `maestro project apply` (the retired `maestro init` wizard no longer gates backends; #871).

## Troubleshooting

### "gemini: command not found"

Ensure the npm global bin directory is in your PATH:

```bash
export PATH="$PATH:$(npm config get prefix)/bin"
```

For systemd services, add the path to the unit file's `Environment=PATH=...` line.

### Authentication errors

Re-authenticate:

```bash
gemini auth
```

Verify presence without printing the value:

```bash
test -n "${GEMINI_API_KEY:-}" && echo 'GEMINI_API_KEY is set' || echo 'GEMINI_API_KEY is unset'
```
