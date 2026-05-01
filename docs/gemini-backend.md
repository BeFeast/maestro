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

Or set the `GEMINI_API_KEY` environment variable:

```bash
export GEMINI_API_KEY="your-api-key-here"
```

If running maestro as a systemd service, add the key to your service environment:

```ini
[Service]
Environment=GEMINI_API_KEY=your-api-key-here
Environment=PATH=/usr/local/bin:/usr/bin:/home/user/.npm-global/bin
```

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
- **Init validation** (`validBackends` in `cmd/maestro/init.go`): "gemini" is accepted by `maestro init`

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

Or verify your API key:

```bash
echo $GEMINI_API_KEY
```
