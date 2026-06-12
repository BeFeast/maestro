#!/usr/bin/env bash
# self-deploy.sh — opt-in post-merge self-deploy of the maestro binary (#698).
#
# Launched by the orchestrator through a detached transient systemd unit
# (systemd-run --user) after a PR merges, so this script survives restarting
# the very units that run the orchestrator. Steps:
#
#   1. build from merged origin/main, version-stamped per #682
#      (-X main.version=<VERSION>+g<shortsha>),
#   2. install atomically, keeping the previous binary as <bin>.prev,
#   3. restart the maestro user units via systemctl --user restart — the
#      units' normal stop path runs, so existing drain semantics are honored,
#   4. verify post-restart health: installed CLI reports the stamped version,
#      units are active, and (when --health-url is given) the running process
#      reports the stamped version within the timeout,
#   5. on failure roll back to <bin>.prev, restart the units again,
#   6. write a JSON result file the orchestrator surfaces as a supervisor
#      finding on its next cycle.
#
# Usage:
#   self-deploy.sh --repo-dir DIR --bin PATH --units a.service[,b.service] \
#     --result-file PATH --timeout-seconds N --pr N \
#     [--health-url URL] [--health-token-env ENVVAR]

set -euo pipefail

REPO_DIR=""
BIN=""
UNITS=""
RESULT_FILE=""
TIMEOUT_SECONDS=1800
PR=0
HEALTH_URL=""
HEALTH_TOKEN_ENV=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo-dir) REPO_DIR="$2"; shift 2 ;;
    --bin) BIN="$2"; shift 2 ;;
    --units) UNITS="$2"; shift 2 ;;
    --result-file) RESULT_FILE="$2"; shift 2 ;;
    --timeout-seconds) TIMEOUT_SECONDS="$2"; shift 2 ;;
    --pr) PR="$2"; shift 2 ;;
    --health-url) HEALTH_URL="$2"; shift 2 ;;
    --health-token-env) HEALTH_TOKEN_ENV="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[[ -n "$REPO_DIR" && -n "$BIN" && -n "$UNITS" && -n "$RESULT_FILE" ]] || {
  echo "usage: --repo-dir, --bin, --units, --result-file are required" >&2
  exit 2
}

START_TS=$(date +%s)
DEADLINE=$((START_TS + TIMEOUT_SECONDS))
# Rollback restarts get their own fixed budget: the whole point of rollback is
# to recover even after the main deadline is spent.
ROLLBACK_RESTART_TIMEOUT=600

BUILD_ROOT=$(mktemp -d /tmp/maestro-self-deploy.XXXXXX)
BUILD_DIR="$BUILD_ROOT/src"
INSTALLED=0
HAVE_PREV=0
RESULT_WRITTEN=0
SHA=""
STAMP=""
PREV_VERSION=""

IFS=',' read -r -a UNIT_LIST <<<"$UNITS"

log() { echo "[self-deploy] $*" >&2; }

json_escape() {
  # Minimal JSON string escaper: backslash, double quote, newlines/tabs.
  local s=$1
  s=${s//\\/\\\\}
  s=${s//\"/\\\"}
  s=${s//$'\n'/ }
  s=${s//$'\t'/ }
  printf '%s' "$s"
}

write_result() {
  local status=$1 reason=$2
  mkdir -p "$(dirname "$RESULT_FILE")"
  local tmp="$RESULT_FILE.tmp.$$"
  cat >"$tmp" <<EOF
{
  "status": "$(json_escape "$status")",
  "version": "$(json_escape "$STAMP")",
  "prev_version": "$(json_escape "$PREV_VERSION")",
  "expected_sha": "$(json_escape "$SHA")",
  "pr": ${PR:-0},
  "reason": "$(json_escape "$reason")",
  "finished_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF
  mv -f "$tmp" "$RESULT_FILE"
  RESULT_WRITTEN=1
  log "result: $status${reason:+ — $reason}"
}

cleanup_build() {
  git -C "$REPO_DIR" worktree remove --force "$BUILD_DIR" >/dev/null 2>&1 || true
  rm -rf "$BUILD_ROOT"
}

restart_units() {
  local budget=$1 unit
  for unit in "${UNIT_LIST[@]}"; do
    log "restarting $unit (honors the unit's stop/drain path; budget ${budget}s)"
    if ! timeout "$budget" systemctl --user restart "$unit"; then
      log "restart of $unit failed or timed out"
      return 1
    fi
  done
}

units_active() {
  local unit
  for unit in "${UNIT_LIST[@]}"; do
    systemctl --user is-active --quiet "$unit" || return 1
  done
}

health_ok() {
  # Running-process check: the API must answer and report the stamped version.
  local curl_args=(-fsS --max-time 10)
  if [[ -n "$HEALTH_TOKEN_ENV" && -n "${!HEALTH_TOKEN_ENV:-}" ]]; then
    curl_args+=(-H "Authorization: Bearer ${!HEALTH_TOKEN_ENV}")
  fi
  curl "${curl_args[@]}" "$HEALTH_URL" 2>/dev/null | grep -qF "\"version\": \"$STAMP\""
}

verify() {
  # CLI check: the installed binary must report the stamped version.
  local cli
  cli=$("$BIN" version 2>/dev/null || true)
  if [[ "$cli" != *"v$STAMP"* ]]; then
    log "CLI version mismatch: got '$cli', want 'maestro v$STAMP'"
    return 1
  fi
  while :; do
    if units_active; then
      if [[ -z "$HEALTH_URL" ]]; then
        return 0
      fi
      if health_ok; then
        return 0
      fi
    fi
    if (( $(date +%s) >= DEADLINE )); then
      log "verification did not pass before the deadline"
      return 1
    fi
    sleep 5
  done
}

rollback() {
  local reason=$1
  if [[ ! -f "$BIN.prev" ]]; then
    write_result failed "$reason; no $BIN.prev to roll back to"
    return
  fi
  log "rolling back to $BIN.prev"
  # Restore atomically: copy .prev to a temp name on the same filesystem and
  # rename over the live binary, keeping .prev itself for inspection.
  if cp -p "$BIN.prev" "$BIN.rollback.$$" && mv -f "$BIN.rollback.$$" "$BIN"; then
    if restart_units "$ROLLBACK_RESTART_TIMEOUT" && units_active; then
      write_result rolled_back "$reason"
    else
      write_result failed "$reason; rollback restored $BIN.prev but units did not come back active"
    fi
  else
    rm -f "$BIN.rollback.$$"
    write_result failed "$reason; rollback copy of $BIN.prev failed"
  fi
}

fail() {
  local reason=$1
  log "FAILED: $reason"
  if (( INSTALLED )); then
    rollback "$reason"
  else
    write_result failed "$reason"
  fi
  cleanup_build
  exit 1
}

on_exit() {
  local rc=$?
  if (( rc != 0 )) && (( ! RESULT_WRITTEN )); then
    # set -e bailed somewhere unexpected (or systemd's RuntimeMaxSec backstop
    # fired): still attempt rollback + leave a result behind.
    trap - ERR EXIT
    fail "unexpected exit (rc=$rc) at deploy step"
  fi
  cleanup_build
}
trap on_exit EXIT
trap 'fail "command failed: ${BASH_COMMAND}"' ERR

command -v go >/dev/null 2>&1 || export PATH="$PATH:/usr/local/go/bin"
command -v go >/dev/null 2>&1 || fail "go toolchain not found in PATH"

# --- 1. build from merged origin/main, version-stamped (#682) ---------------
log "fetching origin/main in $REPO_DIR"
git -C "$REPO_DIR" fetch --quiet origin main
SHA=$(git -C "$REPO_DIR" rev-parse origin/main)
SHORT_SHA=${SHA:0:7}

git -C "$REPO_DIR" worktree add --force --detach "$BUILD_DIR" "$SHA" >/dev/null

VERSION=$(sed -nE 's/^version[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' "$BUILD_DIR/VERSION")
[[ -n "$VERSION" ]] || fail "could not read version from VERSION at $SHA"
STAMP="${VERSION}+g${SHORT_SHA}"

log "building maestro v$STAMP from $SHA"
(cd "$BUILD_DIR" && go build -trimpath -ldflags "-s -w -X main.version=$STAMP" -o "$BUILD_ROOT/maestro" ./cmd/maestro/)

BUILT_VERSION=$("$BUILD_ROOT/maestro" version)
[[ "$BUILT_VERSION" == *"v$STAMP"* ]] || fail "built binary reports '$BUILT_VERSION', want 'maestro v$STAMP'"

# --- 2. install atomically, keep previous as .prev --------------------------
PREV_VERSION=$("$BIN" version 2>/dev/null | sed 's/^maestro v//' || true)
if [[ -n "$PREV_VERSION" && "$PREV_VERSION" == "$STAMP" ]]; then
  log "already at v$STAMP — nothing to deploy"
  write_result deployed "already at v$STAMP"
  cleanup_build
  exit 0
fi

# Stage next to the target so the final rename is atomic (same filesystem).
install -m 0755 "$BUILD_ROOT/maestro" "$BIN.next" || fail "staging $BIN.next failed"
if [[ -f "$BIN" ]]; then
  cp -p "$BIN" "$BIN.prev" || { rm -f "$BIN.next"; fail "preserving $BIN.prev failed"; }
  HAVE_PREV=1
fi
mv -f "$BIN.next" "$BIN" || fail "installing $BIN failed"
INSTALLED=1
if (( HAVE_PREV )); then
  log "installed $BIN (previous kept at $BIN.prev)"
else
  log "installed $BIN (first deploy — no previous binary to keep)"
fi

# --- 3. restart units (drain semantics via the units' own stop path) --------
RESTART_BUDGET=$((DEADLINE - $(date +%s)))
(( RESTART_BUDGET > 30 )) || RESTART_BUDGET=30
restart_units "$RESTART_BUDGET" || fail "unit restart failed or exceeded ${RESTART_BUDGET}s"

# --- 4. verify post-restart health ------------------------------------------
verify || fail "post-restart verification failed (expected v$STAMP from sha $SHA)"

# --- 5. success ---------------------------------------------------------------
write_result deployed ""
log "deployed maestro v$STAMP ($SHA)"
cleanup_build
trap - ERR EXIT
exit 0
