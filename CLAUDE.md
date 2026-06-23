# CLAUDE.md

Guidance for AI coding agents (and the worker harness that auto-loads this file
from the repo root) working in the maestro repository.

## Commit-message & PR convention

- **Do NOT add `Co-authored-by: Claude …` or `🤖 Generated with Claude Code`
  trailers** to commit messages or PR bodies. maestro attributes work via its
  own durable trailer (below); the Claude/agent byline is noise in this repo's
  history and is intentionally suppressed.

  > Note: the `Co-authored-by` byline is emitted by the Claude Code *harness*,
  > not by maestro code. The deterministic switch that stops it is
  > `includeCoAuthoredBy: false` in the worker's `~/.claude/settings.json`. This
  > document records the convention; an operator still has to set that flag for
  > the trailer to actually disappear.

- **Do preserve the `Maestro-Backend:` trailer.** maestro stamps it on commits
  and PR bodies (`internal/state/attribution.go`,
  `state.AttributionTrailerKey`) to record the backend timeline for a session.
  It is the canonical, queryable attribution for this repo — never strip,
  rewrite, or duplicate it.

- **Do NOT use GitHub auto-closing keywords** (`Closes`, `Fixes`, `Resolves`,
  and their variants) followed by an issue reference in commit messages, PR
  titles, or PR bodies for maestro-managed work — `agent-lint` rejects them, and
  a quoted commit subject can trip the check even when the PR body is clean. Use
  `Refs #N` or `Implements #N` instead.

## Build & test before a PR

- `gofmt` changed Go files.
- `go test ./...`
- `go build ./cmd/maestro/`
