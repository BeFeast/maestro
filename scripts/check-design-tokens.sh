#!/usr/bin/env bash
set -euo pipefail
ROOT="${1:-internal/server/web/mc/src}"
PATTERN='(cyan|sky|teal|slate|indigo|emerald|rose|amber|fuchsia|violet|blue|green|yellow|red|orange|pink|purple|stone|zinc|neutral|gray)-[0-9]'
HITS=$(rg -n --no-heading -g '!mc.css' -g '!*.test.*' -g '!*.spec.*' "$PATTERN" "$ROOT" || true)
if [ -n "$HITS" ]; then
  echo "ERROR: raw framework color literals found. Use project token aliases or literal hex from the design source." >&2
  echo "$HITS" >&2
  exit 1
fi
echo "OK: no raw framework color literals in $ROOT"
