#!/usr/bin/env bash
# self-deploy.sh — opt-in post-merge self-deploy of the maestro binary (#698).
#
# Launched by the orchestrator through a detached transient systemd unit after
# a PR merges, so this script survives restarting the very units that run the
# orchestrator. The launcher scope matches --scope (#742): `systemd-run --user`
# for user scope, `sudo -n systemd-run --uid=<user>` (system manager) for
# system scope. Steps:
#
#   1. build from merged origin/main, version-stamped per #682
#      (-X main.version=<VERSION>+g<shortsha>). The build passes
#      -buildvcs=false (#807): the transient unit builds from a detached git
#      worktree under a different uid, where Go's default -buildvcs=auto runs
#      `git status`, hits git's dubious-ownership guard, and exits 128 —
#      failing the whole build (merges silently never shipped). The version is
#      stamped via -ldflags, so VCS stamping is redundant here anyway,
#   2. install atomically, keeping the previous binary as <bin>.prev,
#   2c. apply any required systemd unit change (#877): for each configured unit
#      whose repo copy differs from the live installed file (FragmentPath), back
#      the installed file up as <path>.prev, install the repo copy, and
#      daemon-reload so the restart adopts it. A fix such as KillMode=mixed lives
#      in the unit file, not the binary, so a binary-only deploy would restart
#      under the OLD unit and never ship it. Rollback restores every unit touched,
#   3. restart the maestro units — the units' normal stop path runs, so
#      existing drain semantics are honored,
#   4. verify post-restart health: installed CLI reports the stamped version,
#      units are active, and (when --health-url is given) the running process
#      reports a version built from the deployed commit SHA within the timeout
#      (SHA match, not full-string match, so a stamp vs Go pseudo-version
#      formatting difference can't cause a false miss — #722),
#   4b. run the behavioral smoke gate `maestro selfcheck` against the freshly
#      installed binary (#842): a held-out, side-effect-free self-test of core
#      invariants (config-store load, backend resolution, prompt assembly, state
#      round-trip). The version check proves the binary booted; this proves it
#      still behaves. It is the evaluator OUTSIDE the self-editing loop — a
#      binary that boots and reports the right version but regressed fails here,
#   5. on failure (version OR gate) roll back to <bin>.prev, restart the units
#      again,
#   6. write a JSON result file the orchestrator surfaces as a supervisor
#      finding on its next cycle (a gate rollback names the failing check).
#
# A non-blocking single-flight lock (flock on <result-file>.lock) guarantees one
# deploy at a time: a re-triggered deploy that lands while one is in flight exits
# cleanly instead of bouncing the units mid-verify (#722).
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
GATE_FAIL_DETAIL=""
# #877: installed unit paths this deploy overwrote (each with a <path>.prev
# backup), so rollback can restore the exact prior unit and daemon-reload. Empty
# unless a required unit change (e.g. KillMode) actually shipped.
APPLIED_UNITS=()

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

# daemon_reload reloads the systemd manager so a freshly installed unit file
# takes effect, scoped per --scope (system manager needs sudo -n).
daemon_reload() {
  if [[ "$SCOPE" == "system" ]]; then
    sudo -n systemctl daemon-reload
  else
    systemctl --user daemon-reload
  fi
}

# installed_unit_path prints the live FragmentPath systemd loaded a unit from
# (the authoritative install destination), or empty when the unit is unknown.
# Read-only, so it needs no privilege even for system units.
installed_unit_path() {
  local unit=$1
  if [[ "$SCOPE" == "system" ]]; then
    systemctl show -p FragmentPath --value "$unit" 2>/dev/null || true
  else
    systemctl --user show -p FragmentPath --value "$unit" 2>/dev/null || true
  fi
}

# apply_units ships any required systemd unit change alongside the binary (#877).
# A fix such as KillMode=mixed lives in the repo's unit file, not the binary, so a
# binary-only deploy would restart under the OLD unit and the fix would never go
# live (P1 #877 review). For each configured unit whose repo copy (built from the
# deployed SHA) differs from the live installed file, back the installed file up
# as <path>.prev, install the repo copy, and remember it in APPLIED_UNITS. A
# single daemon-reload afterward makes the subsequent restart pick up every
# change. rollback() restores exactly these files. Units with no repo copy (an
# operator-managed sibling unit) are left untouched.
apply_units() {
  local unit src dest reloaded=0
  for unit in "${UNIT_LIST[@]}"; do
    src="$BUILD_DIR/$unit"
    [[ -f "$src" ]] || continue
    dest=$(installed_unit_path "$unit")
    if [[ -z "$dest" ]]; then
      log "no installed FragmentPath for $unit — skipping unit apply (restart uses the live unit)"
      continue
    fi
    if [[ -f "$dest" ]] && cmp -s "$src" "$dest"; then
      continue
    fi
    log "unit change detected for $unit — installing repo copy over $dest"
    if [[ -f "$dest" ]]; then
      priv cp -p "$dest" "$dest.prev" || fail "backing up unit $dest failed"
    fi
    priv install -m 0644 "$src" "$dest" || fail "installing unit $unit to $dest failed"
    APPLIED_UNITS+=("$dest")
  done
  if (( ${#APPLIED_UNITS[@]} )); then
    daemon_reload || fail "systemd daemon-reload failed after unit change"
    log "applied ${#APPLIED_UNITS[@]} unit change(s) and reloaded the systemd manager"
  fi
}

# rollback_units restores every unit file apply_units overwrote, from its
# <path>.prev backup, then daemon-reloads so the restored units are live before
# the rollback restart runs. Best-effort: a failed restore is logged, not fatal,
# so binary rollback still proceeds.
rollback_units() {
  (( ${#APPLIED_UNITS[@]} )) || return 0
  local dest restored=0
  for dest in "${APPLIED_UNITS[@]}"; do
    [[ -f "$dest.prev" ]] || continue
    log "rolling back unit $dest to $dest.prev"
    if priv cp -p "$dest.prev" "$dest.rollback.$$" && priv mv -f "$dest.rollback.$$" "$dest"; then
      restored=1
    else
      priv rm -f "$dest.rollback.$$" 2>/dev/null || true
      log "unit rollback of $dest failed"
    fi
  done
  if (( restored )); then
    daemon_reload || log "daemon-reload after unit rollback failed"
  fi
}

health_ok() {
  # Running-process check: the API must answer and report a version built from
  # the deployed commit. Match on the commit SHA (embedded in the stamp as
  # "+g<shortsha>") rather than the full version string: a stamped build reports
  # "1.4.2+gf4550ef" while an unstamped one reports a Go pseudo-version
  # "1.4.3-0.<ts>-f4550ef38a42" — same commit, different string. Matching the
  # SHA is format-agnostic, so an ldflags-stamp vs pseudo-version mismatch can't
  # cause a false verify miss (#722). The short SHA is a prefix of the 12-char
  # pseudo-version SHA, so it matches both.
  local curl_args=(-fsS --max-time 10) body
  if [[ -n "$HEALTH_TOKEN_ENV" && -n "${!HEALTH_TOKEN_ENV:-}" ]]; then
    curl_args+=(-H "Authorization: Bearer ${!HEALTH_TOKEN_ENV}")
  fi
  body=$(curl "${curl_args[@]}" "$HEALTH_URL" 2>/dev/null) || return 1
  # Constrain the SHA match to the reported "version" field (handles both
  # indented `"version": "..."` and compact `"version":"..."` JSON).
  printf '%s' "$body" | grep -oE '"version"[[:space:]]*:[[:space:]]*"[^"]*"' | grep -qF "$SHORT_SHA"
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

# smoke_gate runs the behavioral smoke gate (#842) against the freshly-installed
# binary: `maestro selfcheck` exercises core invariants (config-store load,
# backend resolution, prompt assembly, state round-trip) against a held-out
# fixture, with no external side effects. It is the evaluator that sits OUTSIDE
# the self-editing loop — CI ran pre-merge inside the loop that produced the
# change, so this is the first held-out functional assertion between "new binary
# live" and "deploy finalized". A binary that boots and reports the right
# version but behaves worse fails here and is rolled back exactly like a version
# mismatch. Returns 0 on pass; on failure sets GATE_FAIL_DETAIL to the failing
# check name(s) and returns 1. Deterministic and side-effect-free, so it is safe
# to run against the live binary before finalizing.
smoke_gate() {
  local out rc failed
  out=$("$BIN" selfcheck 2>&1) && rc=0 || rc=$?
  # Log the gate's own report either way so a gate-triggered rollback is
  # diagnosable straight from the transient unit's journal.
  while IFS= read -r line; do
    [[ -n "$line" ]] && log "gate: $line"
  done <<<"$out"
  if (( rc == 0 )); then
    return 0
  fi
  # The subcommand's final line is "[selfcheck] FAILED: <name>[,<name>...]".
  failed=$(printf '%s\n' "$out" | sed -nE 's/^\[selfcheck\] FAILED: (.+)$/\1/p' | tail -n1)
  GATE_FAIL_DETAIL="${failed:-selfcheck exited $rc}"
  return 1
}

rollback() {
  local reason=$1
  # Restore any unit files this deploy shipped BEFORE restarting, so the
  # rollback restart brings the units back on their previous definition too
  # (#877). No-op unless apply_units actually changed a unit.
  rollback_units
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

# --- single-flight: one deploy at a time (#722) -----------------------------
# Overlapping deploys restart the same units (notably the fleet web process the
# verify step polls), so a re-triggered deploy landing on top of an in-flight
# one bounces those units mid-verify and verify never converges. Take a
# non-blocking exclusive lock; if another deploy already holds it, exit cleanly
# without touching the binary or any unit — the in-flight deploy owns this wave.
# (The orchestrator also debounces re-triggers; this is the host-side backstop.)
LOCK_FD=
if command -v flock >/dev/null 2>&1; then
  LOCK_FILE="$RESULT_FILE.lock"
  mkdir -p "$(dirname "$LOCK_FILE")"
  exec {LOCK_FD}>"$LOCK_FILE" || fail "could not open self-deploy lock $LOCK_FILE"
  if ! flock -n "$LOCK_FD"; then
    log "another self-deploy holds $LOCK_FILE — skipping (one deploy at a time)"
    trap - ERR EXIT
    cleanup_build
    exit 0
  fi
  log "acquired self-deploy lock $LOCK_FILE"
else
  log "flock not found — proceeding without single-flight lock"
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
# -buildvcs=false (#807): this build runs inside a detached git worktree owned by
# the deploy uid, where Go's default -buildvcs=auto shells out to `git status`,
# trips git's dubious-ownership guard, and exits 128 — aborting the build so the
# merge silently never ships. The version is already stamped via -ldflags below,
# so VCS stamping adds nothing; disabling it makes the build independent of git
# VCS status in the transient unit. Keep -trimpath and the -X main.version stamp.
(cd "$BUILD_DIR" && go build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=$STAMP" -o "$BUILD_ROOT/maestro" ./cmd/maestro/)

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

# --- 2c. apply any required systemd unit change + daemon-reload (#877) -------
# Must run BEFORE the restart so the restart adopts the new unit definition (a
# fix like KillMode=mixed lives in the unit file, not the binary). fail() ->
# rollback() restores every unit this step overwrote, exactly like the binary.
apply_units

# --- 3. restart units (drain semantics via the units' own stop path) --------
RESTART_BUDGET=$((DEADLINE - $(date +%s)))
(( RESTART_BUDGET > 30 )) || RESTART_BUDGET=30
restart_units "$RESTART_BUDGET" || fail "unit restart failed or exceeded ${RESTART_BUDGET}s"

# --- 4. verify post-restart health ------------------------------------------
verify || fail "post-restart verification failed (expected v$STAMP from sha $SHA)"

# --- 4b. behavioral smoke gate (#842) ---------------------------------------
# The version check above confirms the new binary booted and reports v$STAMP; it
# does NOT confirm the binary still behaves correctly. Run the held-out smoke
# gate against it before finalizing (and before .prev is used for rollback). On
# failure, fail() rolls back to .prev exactly like a version mismatch, and the
# result records which check failed.
smoke_gate || fail "behavioral smoke gate failed: $GATE_FAIL_DETAIL"

# --- 5. success ---------------------------------------------------------------
write_result deployed ""
log "deployed maestro v$STAMP ($SHA)"
cleanup_build
trap - ERR EXIT
exit 0
