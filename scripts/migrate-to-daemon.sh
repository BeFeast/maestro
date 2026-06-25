#!/usr/bin/env bash
# migrate-to-daemon.sh — cut a host over from the legacy multi-unit Maestro
# topology (the maestro@.service template + per-project maestro-supervise@ and
# maestro-serve units, ~15 units) to the single-service daemon: ONE
# maestro.service running `maestro daemon` for the whole fleet (epic #754,
# Phase 6 / #761).
#
# The cutover is deliberately NON-CONCURRENT per project (stop-old-then-start-new)
# so no project ever has both a legacy unit and the daemon driving it — that
# would double-spawn workers.
#
# Steps:
#   1. Seed the SQLite config store from the YAML config dir
#      (`config-store migrate`), unless it is already populated.
#   2. Install maestro.service into the systemd user unit dir.
#   3. Stop + disable the legacy units (maestro@*, maestro-supervise@*,
#      maestro-serve) — OLD STOPS BEFORE NEW STARTS.
#   4. enable --now maestro.service.
#   5. Verify :PORT/api/v1/fleet responds and print the running version.
#
# Rollback (the legacy unit files stay on disk):
#   systemctl --user disable --now maestro.service
#   maestro config-store export --db <store> --dir ~/.maestro/maestro.d   # regen YAML if needed
#   systemctl --user enable --now maestro@<project> ...                   # re-enable the old units
#
# This script changes the operational topology of a running host and REQUIRES an
# operator to approve it. It does not touch secrets and prints no credentials.
set -euo pipefail

# --- defaults ---------------------------------------------------------------
STORE="${HOME}/.maestro/maestro.db"
CONFIG_DIR="${HOME}/.maestro/maestro.d"
PORT=8786
UNIT_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/systemd/user"
BIN=""
DRY_RUN=0
ASSUME_YES=0
NO_START=0

usage() {
  cat >&2 <<EOF
usage: migrate-to-daemon.sh [options]

  --store PATH        SQLite store path (default: ${STORE})
  --config-dir DIR    YAML configs to seed the store from (default: ${CONFIG_DIR})
  --port N            Fleet web port to verify (default: ${PORT})
  --unit-dir DIR      systemd --user unit dir (default: ${UNIT_DIR})
  --bin PATH          maestro binary (default: resolved from PATH / /usr/local/bin/maestro)
  --dry-run           print the actions without changing anything
  --no-start          install + seed only; do not stop old units or start the daemon
  --yes               do not prompt for confirmation
  -h, --help          this help
EOF
  exit "${1:-0}"
}

log() { printf '\033[1;34m[migrate]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[migrate]\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31m[migrate] error:\033[0m %s\n' "$*" >&2; exit 1; }

run() {
  if [[ "${DRY_RUN}" == "1" ]]; then
    printf '  + %s\n' "$*"
  else
    "$@"
  fi
}

# --- parse args -------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --store) STORE="$2"; shift 2 ;;
    --config-dir) CONFIG_DIR="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --unit-dir) UNIT_DIR="$2"; shift 2 ;;
    --bin) BIN="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    --no-start) NO_START=1; shift ;;
    --yes) ASSUME_YES=1; shift ;;
    -h|--help) usage 0 ;;
    *) warn "unknown argument: $1"; usage 1 ;;
  esac
done

# --- resolve the binary -----------------------------------------------------
if [[ -z "${BIN}" ]]; then
  if command -v maestro >/dev/null 2>&1; then
    BIN="$(command -v maestro)"
  elif [[ -x /usr/local/bin/maestro ]]; then
    BIN=/usr/local/bin/maestro
  else
    die "maestro binary not found; install it or pass --bin PATH"
  fi
fi
[[ -x "${BIN}" ]] || die "maestro binary ${BIN} is not executable"

# --- locate the unit file (next to this script's repo root) -----------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UNIT_SRC="${SCRIPT_DIR}/../maestro.service"
[[ -f "${UNIT_SRC}" ]] || die "maestro.service not found at ${UNIT_SRC}"

log "binary:     ${BIN} ($(${BIN} version 2>/dev/null || echo 'version unknown'))"
log "store:      ${STORE}"
log "config dir: ${CONFIG_DIR}"
log "unit dir:   ${UNIT_DIR}"
log "port:       ${PORT}"
[[ "${DRY_RUN}" == "1" ]] && warn "DRY RUN — no changes will be made"

if [[ "${ASSUME_YES}" != "1" && "${DRY_RUN}" != "1" ]]; then
  printf 'This stops the legacy maestro units and starts the single daemon. Continue? [y/N] '
  read -r reply
  case "${reply}" in y|Y|yes|YES) ;; *) die "aborted by operator" ;; esac
fi

# --- 1. seed the config store ----------------------------------------------
if [[ -d "${CONFIG_DIR}" ]]; then
  log "seeding config store from ${CONFIG_DIR}"
  run "${BIN}" config-store migrate --db "${STORE}" --dir "${CONFIG_DIR}"
else
  warn "config dir ${CONFIG_DIR} not found — skipping seed (store assumed already populated)"
fi

# --- 2. install the unit ----------------------------------------------------
log "installing maestro.service into ${UNIT_DIR}"
run mkdir -p "${UNIT_DIR}"
run cp "${UNIT_SRC}" "${UNIT_DIR}/maestro.service"
run systemctl --user daemon-reload

if [[ "${NO_START}" == "1" ]]; then
  log "--no-start: store seeded and unit installed; not touching running units"
  exit 0
fi

# --- 3. stop + disable the legacy units (OLD STOPS BEFORE NEW STARTS) -------
# Collect the legacy units actually present so we only act on what exists and the
# cutover stays non-concurrent. Patterns: per-project run (maestro@*), per-project
# supervise (maestro-supervise@*), and the single fleet serve unit (maestro-serve).
legacy_units() {
  systemctl --user list-units --all --plain --no-legend \
    'maestro@*' 'maestro-supervise@*' 'maestro-serve.service' 2>/dev/null \
    | awk '{print $1}' | grep -E '\.service$' || true
}
mapfile -t OLD < <(legacy_units)
if [[ ${#OLD[@]} -gt 0 ]]; then
  log "stopping ${#OLD[@]} legacy unit(s): ${OLD[*]}"
  run systemctl --user stop "${OLD[@]}"
  run systemctl --user disable "${OLD[@]}"
else
  warn "no legacy maestro units found running — proceeding to start the daemon"
fi

# --- 4. start the daemon ----------------------------------------------------
log "enabling + starting maestro.service"
run systemctl --user enable --now maestro.service

# --- 5. verify --------------------------------------------------------------
if [[ "${DRY_RUN}" == "1" ]]; then
  log "dry run complete"
  exit 0
fi

log "verifying fleet on :${PORT}"
ok=0
for _ in $(seq 1 15); do
  if curl -fsS "http://127.0.0.1:${PORT}/api/v1/fleet" >/dev/null 2>&1; then
    ok=1; break
  fi
  sleep 2
done
if [[ "${ok}" != "1" ]]; then
  warn "fleet endpoint :${PORT}/api/v1/fleet did not respond yet"
  warn "check: systemctl --user status maestro.service ; journalctl --user -u maestro.service -e"
  die "verification failed — investigate before assuming the cutover succeeded"
fi

remaining="$(systemctl --user list-units --plain --no-legend 'maestro-*' 2>/dev/null | awk '{print $1}' || true)"
log "active maestro units now:"
systemctl --user list-units --no-legend 'maestro*' 2>/dev/null || true
log "running version: $(${BIN} version 2>/dev/null || echo unknown)"
log "cutover complete. Rollback: systemctl --user disable --now maestro.service && re-enable the legacy units."
[[ -n "${remaining}" ]] && warn "note: legacy maestro-* units still present (left on disk for rollback): ${remaining}"
exit 0
