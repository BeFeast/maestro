#!/usr/bin/env bash
set -euo pipefail
ROOT="${1:-internal/server/web/mc/src}"
# Block ad-hoc card recipe composition outside mc.css (named recipes live in stylesheet).
HITS=$(rg -n --no-heading -g '!mc.css' -g '!*.test.*' \
  'className=.*\b(bg-|border-).*(bg-|border-)' "$ROOT" || true)
if [ -n "$HITS" ]; then
  echo "ERROR: suspected ad-hoc recipe class compositions in JSX. Use named recipe classes from mc.css." >&2
  echo "$HITS" >&2
  exit 1
fi
echo "OK: no ad-hoc recipe compositions in $ROOT"
