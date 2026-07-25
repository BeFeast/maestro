# CLAUDE.md

Guidance for AI coding agents (and the worker harness that auto-loads this file
from the repo root) working in the maestro repository.

## Commit-message & PR convention

- **Do NOT add `Co-authored-by: Claude …` or `🤖 Generated with Claude Code`
  trailers** to commit messages or PR bodies. Maestro attributes work in its
  internal durable state; agent bylines and control-plane telemetry are noise
  in product history and are intentionally suppressed.

  > Note: the `Co-authored-by` byline is emitted by the Claude Code *harness*,
  > not by maestro code. The deterministic switch that stops it is
  > `includeCoAuthoredBy: false` in the worker's `~/.claude/settings.json`. This
  > document records the convention; an operator still has to set that flag for
  > the trailer to actually disappear.

- **Keep backend attribution internal.** Maestro records the backend timeline
  in its durable state and Fleet Mission Control. Do not add provider/model,
  effort, retry, host, or session telemetry to product commit messages or PR
  bodies. Historical `Maestro-Backend:` trailers remain readable, but Maestro
  must not amend or force-push target branches solely to add them. PR bodies
  must contain no backend/model attribution, pids, tmux session names, or host-side paths,
  because they land on target repos that may be public (#799).

- **Do NOT use GitHub auto-closing keywords** (`Closes`, `Fixes`, `Resolves`,
  and their variants) followed by an issue reference in commit messages, PR
  titles, or PR bodies for maestro-managed work — `agent-lint` rejects them, and
  a quoted commit subject can trip the check even when the PR body is clean. Use
  `Refs #N` or `Implements #N` instead.

## Build & test before a PR

- `gofmt` changed Go files.
- `go test ./...`
- `go build ./cmd/maestro/`

## Entire Local History (Maestro dogfood only)

Entire is optional and local-only for the public `BeFeast/maestro` dogfood
repo. If `command -v entire` succeeds and `entire status` shows enabled, a
worker may run the read-only `entire search` or `entire why` commands to recover
prior intent.

Do not run `entire login`, create an Entire mirror, push any `entire/*` branch,
or set `strategy_options.push_sessions` to `true`. Entire rewind/resume is not
Maestro recovery; use Maestro's `CHECKPOINT.md` and respawn flow instead.
