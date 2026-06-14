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
#   3. restart the maestro units — the units' normal stop path runs, so
#      existing drain semantics are honored,
#   4. verify post-restart health: installed CLI reports the stamped version,
#      units are active, and (when --health-url is given) the running process
#      reports the stamped version within the timeout,
#   5. on failure roll back to <bin>.prev, restart the units again,
#   6. write a JSON result file the orchestrator surfaces as a supervisor
#      finding on its next cycle.
#
# --scope selects the systemd unit manager (#716):
#   * user   (default, back-compat) — `systemctl --user restart/is-active`,
#     for per-user units (the pre-migration workshop@opti deployment).
#   * system — `sudo -n systemctl restart` (non-interactive, mirroring
#     --install-via-sudo) and `systemctl is-active` (no --user), for system
#     units like the Loki fleet, where maestro runs as User=god system units.
# The is-active poll and the rollback restart path are scope-aware too.
#
# When --install-via-sudo is passed (#711), the privileged file operations
# (stage <bin>.next, preserve <bin>.prev, atomic rename into place, rollback)
# run through `sudo -n` so a root-owned bin_path such as /usr/local/bin/maestro
# can be updated by the unprivileged deploy user. Requires passwordless sudo;
# atomic-rename semantics are preserved (sudo mv on the same filesystem).
#
# Usage:
#   self-deploy.sh --repo-dir DIR --bin PATH --units a.service[,b.service] \
#     --result-file PATH --timeout-seconds N --pr N \
#     [--health-url URL] [--health-token-env ENVVAR] [--install-via-sudo] \
#     [--scope user|system]

set -euo pipefail

REPO_DIR=""
BIN=""
UNITS=""
RESULT_FILE=""
TIMEOUT_SECONDS=1800
PR=0
HEALTH_URL=""
HEALTH_TOKEN_ENV=""
INSTALL_VIA_SUDO=0
SCOPE="user"

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
    --install-via-sudo) INSTALL_VIA_SUDO=1; shift ;;
    --scope) SCOPE="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[[ -n "$REPO_DIR" && -n "$BIN" && -n "$UNITS" && -n "$RESULT_FILE" ]] || {
  echo "usage: --repo-dir, --bin, --units, --result-file are required" >&2
  exit 2
}

[[ "$SCOPE" == "user" || "$SCOPE" == "system" ]] || {
  echo "invalid --scope: $SCOPE (want user|system)" >&2
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

# priv runs a privileged file operation against bin_path. With
# --install-via-sudo it prefixes `sudo -n` (non-interactive: fail fast rather
# than block on a password prompt the detached unit has no tty to answer);
# otherwise it runs the command directly, so the no-sudo path is byte-for-byte
# unchanged (#711).
priv() {
  if (( INSTALL_VIA_SUDO )); then
    sudo -n "$@"
  else
    "$@"
  fi
}

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

# restart_units restarts each configured unit, scoped per --scope (#716).
# user-scope uses the per-user manager (`systemctl --user restart`); system-scope
# targets the system manager via `sudo -n systemctl restart` (non-interactive,
# mirroring --install-via-sudo), since restarting a system unit needs privilege.
# Either way the unit's normal stop path runs, so drain semantics are honored.
# The user-scope command is byte-for-byte unchanged from before scope support.
restart_units() {
  local budget=$1 unit
  local -a restart_cmd
  if [[ "$SCOPE" == "system" ]]; then
    restart_cmd=(sudo -n systemctl restart)
  else
    restart_cmd=(systemctl --user restart)
  fi
  for unit in "${UNIT_LIST[@]}"; do
    log "restarting $unit (scope=$SCOPE; honors the unit's stop/drain path; budget ${budget}s)"
    if ! timeout "$budget" "${restart_cmd[@]}" "$unit"; then
      log "restart of $unit failed or timed out"
      return 1
    fi
  done
}

# units_active is scope-aware too (#716). The read-only liveness query needs no
# privilege even for system units, so system-scope simply drops --user (no sudo).
units_active() {
  local unit
  local -a active_cmd
  if [[ "$SCOPE" == "system" ]]; then
    active_cmd=(systemctl is-active --quiet)
  else
    active_cmd=(systemctl --user is-active --quiet)
  fi
  for unit in "${UNIT_LIST[@]}"; do
    "${active_cmd[@]}" "$unit" || return 1
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
  # rename over the live binary, keeping .prev itself for inspection. priv
  # escalates via sudo when bin_path is root-owned (#711).
  if priv cp -p "$BIN.prev" "$BIN.rollback.$$" && priv mv -f "$BIN.rollback.$$" "$BIN"; then
    if restart_units "$ROLLBACK_RESTART_TIMEOUT" && units_active; then
      write_result rolled_back "$reason"
    else
      write_result failed "$reason; rollback restored $BIN.prev but units did not come back active"
    fi
  else
    priv rm -f "$BIN.rollback.$$"
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

# Preflight passwordless sudo when any privileged operation is requested —
# install_via_sudo file ops (#711) or system-scope unit restarts (#716) — so a
# misconfigured host fails loudly here (recorded as a finding) instead of at the
# first privileged operation.
if (( INSTALL_VIA_SUDO )) || [[ "$SCOPE" == "system" ]]; then
  command -v sudo >/dev/null 2>&1 || fail "privileged self-deploy requested but sudo not found in PATH"
  sudo -n true >/dev/null 2>&1 || fail "privileged self-deploy requested but passwordless sudo is unavailable for $(id -un)"
fi
if (( INSTALL_VIA_SUDO )); then
  log "install_via_sudo enabled — privileged file ops on $BIN run via sudo -n"
fi
if [[ "$SCOPE" == "system" ]]; then
  log "scope=system — unit restarts run via sudo -n systemctl"
fi

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
# priv escalates these ops via sudo when bin_path is root-owned (#711).
priv install -m 0755 "$BUILD_ROOT/maestro" "$BIN.next" || fail "staging $BIN.next failed"
if [[ -f "$BIN" ]]; then
  priv cp -p "$BIN" "$BIN.prev" || { priv rm -f "$BIN.next"; fail "preserving $BIN.prev failed"; }
  HAVE_PREV=1
fi
priv mv -f "$BIN.next" "$BIN" || fail "installing $BIN failed"
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
