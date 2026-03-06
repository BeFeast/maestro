#!/usr/bin/env bash
# droid-worker.sh — standalone adapter for Factory.ai droid exec in maestro headless mode.
#
# Usage:
#   FACTORY_API_KEY=fk-... ./scripts/droid-worker.sh <prompt-file> <worktree-path> [--auto <level>] [--output-format <fmt>]
#
# Environment:
#   FACTORY_API_KEY   Required. Get from https://app.factory.ai/settings/api-keys
#   DROID_CMD         Optional. Path to droid binary (default: droid)
#
# Exit codes:
#   0  success (droid completed the task)
#   1  failure (auth error, permission violation, tool error, unmet objective)
#
# This script is called by maestro when model.default or model.backends.<name>.cmd is set to
# "droid" in maestro.yaml. It wraps the droid exec invocation with sensible defaults for CI/CD.
#
# Maestro config example:
#   model:
#     default: droid
#     backends:
#       droid:
#         cmd: droid
#         extra_args: ["--auto", "high", "--output-format", "json"]

set -euo pipefail

PROMPT_FILE="${1:?Usage: $0 <prompt-file> <worktree-path> [extra-droid-args...]}"
WORKTREE="${2:?Usage: $0 <prompt-file> <worktree-path> [extra-droid-args...]}"
shift 2
EXTRA_ARGS=("$@")

DROID_CMD="${DROID_CMD:-droid}"

# Validate auth early
if [ -z "${FACTORY_API_KEY:-}" ]; then
  echo "[droid-worker] ERROR: FACTORY_API_KEY is not set." >&2
  echo "[droid-worker] Get your API key from: https://app.factory.ai/settings/api-keys" >&2
  echo "[droid-worker] Then run: export FACTORY_API_KEY=fk-..." >&2
  exit 1
fi

# Validate inputs
if [ ! -f "$PROMPT_FILE" ]; then
  echo "[droid-worker] ERROR: prompt file not found: $PROMPT_FILE" >&2
  exit 1
fi

if [ ! -d "$WORKTREE" ]; then
  echo "[droid-worker] ERROR: worktree directory not found: $WORKTREE" >&2
  exit 1
fi

echo "[droid-worker] Starting: droid exec -f $PROMPT_FILE --cwd $WORKTREE --auto high ${EXTRA_ARGS[*]:-}"
echo "[droid-worker] Worktree: $WORKTREE"
echo "[droid-worker] Prompt: $(wc -l < "$PROMPT_FILE") lines"

exec "$DROID_CMD" exec \
  -f "$PROMPT_FILE" \
  --cwd "$WORKTREE" \
  --auto high \
  "${EXTRA_ARGS[@]}"
