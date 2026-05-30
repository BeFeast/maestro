#!/usr/bin/env bash
# Block nesting an outer-tier recipe inside itself.
# Outer recipes define one border + surface; nesting two outers produces a
# double border that does not exist in the design source. The accepted
# pattern is outer (`.<recipe>`) + second-tier (`.<recipe>-2`) for any
# inner card. See docs/design-export-to-ux-issues-runbook.md.
set -euo pipefail

ROOT="${1:-internal/server/web/mc/src}"
RECIPES='card|panel|tape|appv|section|hb|sb|stat'

if [ ! -d "$ROOT" ]; then
  echo "ERROR: design recipe nesting guard: root '$ROOT' is not a directory" >&2
  exit 1
fi

mapfile -d '' FILES < <(find "$ROOT" -type f \( -name '*.jsx' -o -name '*.tsx' \) \
  ! -name '*.test.*' ! -name '*.spec.*' -print0)

if [ "${#FILES[@]}" -eq 0 ]; then
  echo "OK: no JSX/TSX sources under $ROOT"
  exit 0
fi

VIOLATIONS=$(awk -v recipes="$RECIPES" '
  FNR == 1 {
    delete stack_ind
    delete stack_rec
    delete stack_line
    n = 0
  }
  {
    indent = 0
    for (i = 1; i <= length($0); i++) {
      c = substr($0, i, 1)
      if (c == " ") indent++
      else if (c == "\t") indent += 2
      else break
    }
    while (n > 0 && stack_ind[n] >= indent) {
      delete stack_ind[n]
      delete stack_rec[n]
      delete stack_line[n]
      n--
    }
    rest = $0
    while (match(rest, /className="[^"]*"/)) {
      val = substr(rest, RSTART, RLENGTH)
      rest = substr(rest, RSTART + RLENGTH)
      sub(/^className="/, "", val)
      sub(/"$/, "", val)
      m = split(val, classes, " ")
      for (j = 1; j <= m; j++) {
        cls = classes[j]
        if (cls ~ ("^(" recipes ")$")) {
          for (k = 1; k <= n; k++) {
            if (stack_rec[k] == cls) {
              print FILENAME ":" FNR ": nested recipe \"" cls "\" inside ancestor at line " stack_line[k]
            }
          }
          n++
          stack_ind[n] = indent
          stack_rec[n] = cls
          stack_line[n] = FNR
        }
      }
    }
  }
' "${FILES[@]}")

if [ -n "$VIOLATIONS" ]; then
  echo "ERROR: nested same-recipe usage detected. Use the second-tier '-2' recipe variant when nesting outer recipes (e.g. .card-2 inside .card)." >&2
  echo "$VIOLATIONS" >&2
  exit 1
fi
echo "OK: no nested same-recipe usage in $ROOT"
