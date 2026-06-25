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
# Scope (#761): --scope selects the systemd manager, mirroring self-deploy.sh
# (#742). Both the legacy units and the new maestro.service live in the same
# manager.
#   * user   (default, back-compat) — `systemctl --user ...`, unit installed
#     into ~/.config/systemd/user, the committed `%h`/default.target template.
#   * system — `sudo -n systemctl ...` for mutations (non-interactive; requires
#     passwordless sudo), unit installed into /etc/systemd/system rendered with
#     User=/WorkingDirectory= and `%h` expanded to the service user's home, so it
#     matches the existing system units (e.g. maestro-supervisor-dogfood.service
#     on the Loki host, which run as User=god). Reads (list-units / is-active)
#     use plain `systemctl` (no sudo).
#
# Steps:
#   1. Seed the SQLite config store from EVERY legacy config source:
#      - the newer `~/.maestro/maestro.d/*.yaml` directory (`config-store migrate`)
#      - the legacy per-instance files the `maestro@%i` template loaded,
#        `~/.maestro/maestro-*.yaml` (`config-store add`, one per file).
#      Then refuse to go any further if the store is still empty — we never stop
#      the legacy units only to start an empty daemon.
#   2. Render maestro.service with the requested --bin/--store/--port (and, for
#      system scope, User=/WorkingDirectory=/absolute home) and install it into
#      the scope's unit dir.
#   3. Stop + disable the legacy units — discovered from BOTH `list-units`
#      (loaded/running) AND `list-unit-files` (enabled-but-inactive on disk), so
#      an enabled instance that is not currently loaded cannot re-appear on the
#      next login/boot next to maestro.service. OLD STOPS BEFORE NEW STARTS.
#   4. enable --now maestro.service.
#   5. Verify :PORT/api/v1/fleet responds and print the running version.
#
# Rollback (the legacy unit files stay on disk; SC = the scope's systemctl):
#   <SC> disable --now maestro.service
#   maestro config-store export --db <store> --dir ~/.maestro/maestro.d   # regen YAML if needed
#   <SC> enable --now maestro@<project> ...                               # re-enable the old units
#
# This script changes the operational topology of a running host and REQUIRES an
# operator to approve it. It does not touch secrets and prints no credentials.
set -euo pipefail

# --- defaults ---------------------------------------------------------------
MAESTRO_DIR="${HOME}/.maestro"
STORE="${MAESTRO_DIR}/maestro.db"
CONFIG_DIR="${MAESTRO_DIR}/maestro.d"
PORT=8786
SCOPE=user
SVCUSER="$(id -un)"
UNIT_DIR=""          # resolved from --scope after parsing unless --unit-dir given
BIN=""
DRY_RUN=0
ASSUME_YES=0
NO_START=0

# Legacy unit name patterns to stop + disable. `maestro-*` (dash form) is the
# broad catch for the per-host dash naming — per-project worker
# (maestro-<project>.service), supervise (maestro-<project>-supervise.service),
# web (maestro-<project>-web.service), plus the dogfood/fleet units — which is
# what the Loki host actually runs; the `@` patterns cover the older
# template-instance naming on other hosts. None of these match the new
# single-service unit (`maestro.service` — no `@`, no `-` after `maestro`); the
# new unit, the self-deploy transient units, and the documented standalone
# maestro-digest unit are excluded in legacy_units (exclude_non_legacy) too.
LEGACY_PATTERNS=(
  'maestro@*'             # per-project run template instances (older naming)
  'maestro-supervise@*'   # per-project supervise template instances (older naming)
  'maestro-*'             # all dash-form units: per-project worker / -supervise /
                          # -web, dogfood, fleet — the live Loki naming
)

usage() {
  cat >&2 <<EOF
usage: migrate-to-daemon.sh [options]

  --scope user|system honor the systemd --user manager (default) or the system
                      manager via sudo -n (default: ${SCOPE})
  --user NAME         service User= for --scope system (default: ${SVCUSER})
  --store PATH        SQLite store path (default: ${STORE})
  --config-dir DIR    YAML configs to seed the store from (default: ${CONFIG_DIR})
  --port N            Fleet web port — written into the unit AND verified (default: ${PORT})
  --unit-dir DIR      systemd unit dir (default: scope-derived —
                      ~/.config/systemd/user or /etc/systemd/system)
  --bin PATH          maestro binary — written into the unit's ExecStart
                      (default: resolved from PATH / /usr/local/bin/maestro)
  --dry-run           print the actions (incl. the rendered unit) without changing anything
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
    --scope) SCOPE="$2"; shift 2 ;;
    --user) SVCUSER="$2"; shift 2 ;;
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

case "${SCOPE}" in user|system) ;; *) die "invalid --scope: ${SCOPE} (want user|system)" ;; esac

# Service user's home: the unit's %h is expanded to this for system scope (system
# units do not reliably resolve %h), and a system unit runs as User=${SVCUSER}.
SVCHOME="$(getent passwd "${SVCUSER}" 2>/dev/null | cut -d: -f6)"
[[ -n "${SVCHOME}" ]] || SVCHOME="${HOME}"

# Default unit dir by scope unless the operator pinned one.
if [[ -z "${UNIT_DIR}" ]]; then
  if [[ "${SCOPE}" == "system" ]]; then
    UNIT_DIR="/etc/systemd/system"
  else
    UNIT_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/systemd/user"
  fi
fi

# Where the legacy per-instance files (maestro-*.yaml) live: next to maestro.d.
LEGACY_CFG_DIR="$(dirname "${CONFIG_DIR}")"

# --- scope-aware systemctl --------------------------------------------------
# sc: read-only systemctl (list-units, is-active) — never needs sudo.
sc() {
  if [[ "${SCOPE}" == "system" ]]; then
    systemctl "$@"
  else
    systemctl --user "$@"
  fi
}
# sc_do: MUTATING systemctl (daemon-reload, stop, disable, enable) — routed
# through run() for --dry-run, and via `sudo -n` for system scope.
sc_do() {
  if [[ "${SCOPE}" == "system" ]]; then
    run sudo -n systemctl "$@"
  else
    run systemctl --user "$@"
  fi
}

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

# --- locate the unit template (next to this script's repo root) -------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UNIT_SRC="${SCRIPT_DIR}/../maestro.service"
[[ -f "${UNIT_SRC}" ]] || die "maestro.service not found at ${UNIT_SRC}"

log "scope:      ${SCOPE}$([[ "${SCOPE}" == system ]] && printf ' (User=%s, home %s, sudo -n)' "${SVCUSER}" "${SVCHOME}")"
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

# --- helpers ----------------------------------------------------------------

# Escape a value for sed's replacement (right-hand) side when '|' is the
# delimiter: backslash, the delimiter, and & are special there.
sed_repl() { printf '%s' "$1" | sed -e 's/[\\&|]/\\&/g'; }

# render_unit emits maestro.service with the operator's --bin/--store/--port
# substituted into the template's ExecStart. The committed unit hard-codes the
# defaults (/usr/local/bin/maestro, %h/.maestro/maestro.db, 8786); rendering
# keeps the installed unit consistent with the store this script seeds and the
# endpoint it verifies. The rest of the unit (flag set, drain timeout, restart
# policy) stays the template's.
#
# For --scope system the committed user-style template (%h, default.target, no
# User=) is additionally transformed into a system unit: %h → the service user's
# home, User=/WorkingDirectory= injected, and WantedBy → multi-user.target — so
# it matches the existing system units on the host.
render_unit() {
  local bin store port
  bin="$(sed_repl "${BIN}")"
  store="$(sed_repl "${STORE}")"
  port="$(sed_repl "${PORT}")"
  if [[ "${SCOPE}" == "system" ]]; then
    local home user
    home="$(sed_repl "${SVCHOME}")"
    user="$(sed_repl "${SVCUSER}")"
    sed \
      -e "s|ExecStart=/usr/local/bin/maestro |ExecStart=${bin} |" \
      -e "s|--store %h/.maestro/maestro.db|--store ${store}|" \
      -e "s|--port 8786|--port ${port}|" \
      -e "s|%h|${home}|g" \
      -e "s|^\[Service\]\$|[Service]\nUser=${user}\nWorkingDirectory=${home}|" \
      -e "s|^WantedBy=default.target\$|WantedBy=multi-user.target|" \
      "${UNIT_SRC}"
  else
    sed \
      -e "s|ExecStart=/usr/local/bin/maestro |ExecStart=${bin} |" \
      -e "s|--store %h/.maestro/maestro.db|--store ${store}|" \
      -e "s|--port 8786|--port ${port}|" \
      "${UNIT_SRC}"
  fi
}

# store_project_count prints how many projects the store currently holds, by
# exporting it to a throwaway dir and counting the emitted YAML files (there is
# no `config-store list`). Used to refuse a cutover against an empty store.
store_project_count() {
  local tmp count
  tmp="$(mktemp -d)"
  if "${BIN}" config-store export --db "${STORE}" --dir "${tmp}" >/dev/null 2>&1; then
    shopt -s nullglob
    local files=( "${tmp}"/*.yaml "${tmp}"/*.yml )
    shopt -u nullglob
    count=${#files[@]}
  else
    count=0
  fi
  rm -rf "${tmp}"
  printf '%s' "${count}"
}

# legacy_units prints the legacy service units to act on, unioning currently
# loaded units (`list-units --all`) with installed-but-inactive unit files
# (`list-unit-files`). Bare template files (foo@.service) are dropped — only
# instances/concrete units are stopped/disabled, and the templates stay on disk
# for rollback.
# exclude_non_legacy: drop maestro-prefixed units that the broad `maestro-*`
# glob matches but that are NOT part of the per-project fleet topology this
# cutover replaces, so they are never stopped/disabled:
#   * maestro.service          — the new single-service unit (defense-in-depth;
#                                `maestro-*` already can't match it).
#   * maestro-self-deploy*      — ephemeral self-deploy transient units.
#   * maestro-digest{.service,} — the documented standalone digest unit/timer
#                                (docs/digest-runbook.md); orthogonal to the
#                                fleet, must survive the cutover.
exclude_non_legacy() {
  grep -vxF 'maestro.service' | grep -vE '^maestro-self-deploy' | grep -vE '^maestro-digest'
}

legacy_units() {
  {
    sc list-units --all --plain --no-legend "${LEGACY_PATTERNS[@]}" 2>/dev/null | awk '{print $1}'
    sc list-unit-files --no-legend "${LEGACY_PATTERNS[@]}" 2>/dev/null | awk '{print $1}'
  } | grep -E '\.service$' | grep -vE '@\.service$' | exclude_non_legacy | sort -u || true
}

# --- 1. seed the config store ----------------------------------------------
# Import the legacy per-instance files first (maestro@<i> → maestro-<i>.yaml) so
# a current maestro.d/ wins on conflict; then the maestro.d directory on top.
shopt -s nullglob
LEGACY_CFGS=( "${LEGACY_CFG_DIR}"/maestro-*.yaml "${LEGACY_CFG_DIR}"/maestro-*.yml )
shopt -u nullglob
if [[ ${#LEGACY_CFGS[@]} -gt 0 ]]; then
  log "importing ${#LEGACY_CFGS[@]} legacy maestro-*.yaml config(s) from ${LEGACY_CFG_DIR}"
  for cfg in "${LEGACY_CFGS[@]}"; do
    run "${BIN}" config-store add --db "${STORE}" --file "${cfg}"
  done
fi
if [[ -d "${CONFIG_DIR}" ]]; then
  log "seeding config store from ${CONFIG_DIR}"
  run "${BIN}" config-store migrate --db "${STORE}" --dir "${CONFIG_DIR}"
elif [[ ${#LEGACY_CFGS[@]} -eq 0 ]]; then
  warn "no config sources found (${CONFIG_DIR} missing, no ${LEGACY_CFG_DIR}/maestro-*.yaml)"
  warn "assuming the store at ${STORE} is already populated — verifying below"
fi

# Count what the store holds now (skip under --dry-run: nothing was seeded). The
# empty-store guard that refuses to stop the legacy units lives just before the
# stop step ("fail before stopping anything"); under --no-start we only warn.
STORE_PROJECTS=""
if [[ "${DRY_RUN}" != "1" ]]; then
  STORE_PROJECTS="$(store_project_count)"
  log "config store ${STORE} holds ${STORE_PROJECTS} project(s)"
fi

# --- 2. render + install the unit ------------------------------------------
log "installing maestro.service into ${UNIT_DIR}"
log "  ExecStart binary: ${BIN}"
log "  --store:          ${STORE}"
log "  --port:           ${PORT}"
if [[ "${DRY_RUN}" == "1" ]]; then
  printf '  + install %s/maestro.service (rendered from %s):\n' "${UNIT_DIR}" "${UNIT_SRC}"
  render_unit | sed 's/^/        /'
  if [[ "${SCOPE}" == "system" ]]; then
    printf '  + sudo -n systemctl daemon-reload\n'
  else
    printf '  + systemctl --user daemon-reload\n'
  fi
else
  if [[ "${SCOPE}" == "system" ]]; then
    render_unit | sudo -n tee "${UNIT_DIR}/maestro.service" >/dev/null
  else
    mkdir -p "${UNIT_DIR}"
    render_unit > "${UNIT_DIR}/maestro.service"
  fi
  sc_do daemon-reload
fi

if [[ "${NO_START}" == "1" ]]; then
  if [[ "${STORE_PROJECTS}" == "0" ]]; then
    warn "config store ${STORE} is empty — seed it before cutover (no projects to run)"
  fi
  log "--no-start: store seeded and unit installed; not touching running units"
  exit 0
fi

# Refuse to stop anything if the store ended up empty: stopping the legacy units
# only to start an empty daemon would take the whole fleet down. (Already gated
# on a live store; --dry-run leaves STORE_PROJECTS empty and skips this.)
if [[ "${STORE_PROJECTS}" == "0" ]]; then
  die "config store ${STORE} has no projects after seeding — refusing to stop the legacy units (the daemon would start empty). Seed it first, e.g.:
      ${BIN} config-store migrate --db ${STORE} --dir ${CONFIG_DIR}
    or place the legacy per-project files at ${LEGACY_CFG_DIR}/maestro-<name>.yaml and re-run."
fi

# --- 3. stop + disable the legacy units (OLD STOPS BEFORE NEW STARTS) -------
mapfile -t OLD < <(legacy_units)
if [[ ${#OLD[@]} -gt 0 ]]; then
  log "stopping + disabling ${#OLD[@]} legacy unit(s): ${OLD[*]}"
  sc_do stop "${OLD[@]}" || warn "some legacy units did not stop cleanly"
  sc_do disable "${OLD[@]}" || warn "some legacy units did not disable cleanly"
else
  warn "no legacy maestro units found (loaded or enabled) — proceeding to start the daemon"
fi

# Non-concurrency guard: nothing legacy may still be active when we start the
# daemon, or two drivers would run the same project.
if [[ "${DRY_RUN}" != "1" ]]; then
  still_active="$(sc list-units --plain --no-legend "${LEGACY_PATTERNS[@]}" 2>/dev/null \
    | awk '$3=="active"{print $1}' | grep -E '\.service$' | grep -vE '@\.service$' | exclude_non_legacy || true)"
  if [[ -n "${still_active}" ]]; then
    die "legacy unit(s) still active after stop: ${still_active//$'\n'/ } — refusing to start maestro.service (it would double-drive those projects). Stop them and re-run."
  fi
fi

# --- 4. start the daemon ----------------------------------------------------
log "enabling + starting maestro.service"
sc_do enable --now maestro.service

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
  if [[ "${SCOPE}" == "system" ]]; then
    warn "check: systemctl status maestro.service ; journalctl -u maestro.service -e"
  else
    warn "check: systemctl --user status maestro.service ; journalctl --user -u maestro.service -e"
  fi
  die "verification failed — investigate before assuming the cutover succeeded"
fi

log "active maestro units now:"
sc list-units --no-legend 'maestro*' 2>/dev/null || true
log "running version: $(${BIN} version 2>/dev/null || echo unknown)"
if [[ "${SCOPE}" == "system" ]]; then
  log "cutover complete. Rollback: sudo systemctl disable --now maestro.service && re-enable the legacy units."
else
  log "cutover complete. Rollback: systemctl --user disable --now maestro.service && re-enable the legacy units."
fi
log "(legacy unit files remain on disk, now disabled, so rollback re-enables them.)"
exit 0
