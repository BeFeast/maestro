#!/usr/bin/env bash
#
# agent-lint.sh — enforce the fleet's agent anti-pattern rules on a pull request.
#
# Worker-discipline rules live as prose in the worker prompt; rules enforced only
# in prompts are not enforced at all. This script encodes them as CI checks:
#
#   1. closing keywords — fail if the PR title/body uses a GitHub auto-closing
#      keyword (close/fix/resolve …) referencing an issue. Use `Refs #N` or
#      `Implements #N` instead.
#   2. diff hygiene     — fail if the diff adds forbidden artifacts
#      (`.maestro/`, `tmp/`, `_tmp/`, `*.log`, `*.logs`, `*.test`, `*.test.json`).
#   3. secret scan      — fail if added diff lines contain common secret shapes
#      (sk-ant-…, ghp_…, AKIA…, BEGIN PRIVATE KEY, …).
#   4. draft check      — WARN (never fail) if the PR is a draft without an
#      explicit WIP/Partial marker (the deliberate-draft flow from #697).
#
# Escape hatch: a hard failure (checks 1–3) is downgraded to a warning when the
# PR carries the override label (default `agent-lint:override`) or the body
# marker `agent-lint:allow` / `agent-lint:ignore`. The draft check is warn-only.
#
# Modes:
#   --self-test       run built-in fixtures; each forbidden pattern must trip its
#                     check and a clean input must pass. Exit non-zero on any
#                     unexpected result. No network/`gh` needed.
#   (AGENT_LINT_PR set)   fetch the PR title/body/draft/labels/diff via `gh` and
#                         lint it. Used by the composite action in CI.
#   (otherwise)       offline dry-run: lint the values passed directly via
#                     AGENT_LINT_TITLE / _BODY / _DRAFT / _LABELS / _FILES / _DIFF.
#
set -uo pipefail

# ---- inputs / config --------------------------------------------------------
REPO="${AGENT_LINT_REPO:-${GITHUB_REPOSITORY:-}}"
PR="${AGENT_LINT_PR:-}"
OVERRIDE_LABEL="${AGENT_LINT_OVERRIDE_LABEL:-agent-lint:override}"
ALLOWLIST="${AGENT_LINT_ARTIFACT_ALLOWLIST:-}"

# Lint subjects (populated by gh-fetch, offline env, or self-test fixtures).
TITLE="" BODY="" DRAFT="false" LABELS="" FILES="" DIFF=""

# ---- helpers ----------------------------------------------------------------

# mask hides the bulk of a matched token so secrets are never echoed in full.
mask() {
	local s="$1" L=${#1}
	if (( L <= 6 )); then printf '******'; else printf '%s…%s' "${s:0:4}" "${s: -2}"; fi
}

lc() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

# is_artifact reports whether a committed path is a forbidden build/run artifact.
is_artifact() {
	case "$1" in
		.maestro/*|*/.maestro/*) return 0 ;;
		tmp/*|*/tmp/*|_tmp/*|*/_tmp/*) return 0 ;;
		*.log|*.logs|*.test|*.test.json) return 0 ;;
	esac
	return 1
}

# is_allowlisted exempts a path that matches any configured allowlist glob.
is_allowlisted() {
	local f="$1" pat
	[[ -z "${ALLOWLIST//[[:space:]]/}" ]] && return 1
	for pat in $ALLOWLIST; do
		# shellcheck disable=SC2053
		[[ "$f" == $pat ]] && return 0
	done
	return 1
}

# has_partial_marker mirrors orchestrator.prHasDeliberateDraftMarker (#697).
has_partial_marker() {
	local t b
	t="$(lc "$(printf '%s' "$TITLE" | sed 's/^[[:space:]]*//')")"
	b="$(lc "$BODY")"
	case "$t" in
		*'[wip]'*|*'[partial]'*) return 0 ;;
		wip:*|'wip '*|partial:*|draft:*) return 0 ;;
		wip|partial) return 0 ;;
	esac
	case "$b" in
		*maestro:partial*|*maestro:wip*) return 0 ;;
	esac
	return 1
}

# override_active reports the documented false-positive escape hatch.
override_active() {
	local lbl
	while IFS= read -r lbl; do
		[[ "$lbl" == "$OVERRIDE_LABEL" ]] && return 0
	done <<<"$LABELS"
	case "$(lc "$BODY")" in
		*'agent-lint:allow'*|*'agent-lint:ignore'*) return 0 ;;
	esac
	return 1
}

# ---- checks (return 1 on a hard violation, 0 otherwise) ---------------------

# 1. auto-closing keywords referencing an issue, in the PR title or body.
check_closing() {
	local blob hits rc=0
	blob="$(printf '%s\n%s' "$TITLE" "$BODY")"
	local re='\b(close[sd]?|fix|fixes|fixed|resolve[sd]?)\b[[:space:]]*:?[[:space:]]*(#[0-9]+|gh-[0-9]+|https?://github\.com/[^[:space:]]+/issues/[0-9]+)'
	hits="$(printf '%s' "$blob" | grep -inE "$re" || true)"
	if [[ -n "$hits" ]]; then
		echo "::error::agent-lint: PR title/body uses a GitHub auto-closing keyword referencing an issue. Use 'Refs #N' or 'Implements #N' instead."
		while IFS= read -r l; do [[ -n "$l" ]] && echo "  ↳ ${l}"; done <<<"$hits"
		rc=1
	fi
	return $rc
}

# 2. forbidden artifacts committed in the diff.
check_artifacts() {
	local f rc=0
	[[ -z "${FILES//[[:space:]]/}" ]] && return 0
	while IFS= read -r f; do
		[[ -z "$f" ]] && continue
		is_allowlisted "$f" && continue
		if is_artifact "$f"; then
			echo "::error::agent-lint: forbidden artifact committed: ${f}"
			rc=1
		fi
	done <<<"$FILES"
	return $rc
}

# 3. common secret shapes in added diff lines.
check_secrets() {
	local added entry name pat hits h rc=0
	added="$(printf '%s\n' "$DIFF" | grep -E '^\+' | grep -vE '^\+\+\+' || true)"
	[[ -z "$added" ]] && return 0
	for entry in \
		"anthropic-key|sk-ant-[A-Za-z0-9_-]{8,}" \
		"github-token|(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}" \
		"github-pat|github_pat_[A-Za-z0-9_]{20,}" \
		"aws-access-key|A(KIA|SIA)[0-9A-Z]{16}" \
		"slack-token|xox[baprs]-[A-Za-z0-9-]{10,}" \
		"google-api-key|AIza[0-9A-Za-z_-]{35}" \
		"private-key|-----BEGIN [A-Z ]*PRIVATE KEY-----"; do
		name="${entry%%|*}"
		pat="${entry#*|}"
		hits="$(printf '%s\n' "$added" | grep -oE -e "$pat" || true)"
		if [[ -n "$hits" ]]; then
			echo "::error::agent-lint: possible secret detected (${name}) in added diff lines."
			while IFS= read -r h; do [[ -n "$h" ]] && echo "  ↳ $(mask "$h")"; done <<<"$hits"
			rc=1
		fi
	done
	return $rc
}

# 4. draft without a deliberate marker — warning only, never blocks.
check_draft() {
	[[ "$DRAFT" != "true" ]] && return 0
	if has_partial_marker; then
		echo "::notice::agent-lint: draft PR carries a deliberate WIP/Partial marker — OK."
		return 0
	fi
	echo "::warning::agent-lint: PR is a draft without a WIP/Partial marker. If this is deliberate partial work, prefix the title with [Partial] and add '<!-- maestro:partial -->' to the body; otherwise mark it ready for review."
	return 0
}

# ---- orchestration ----------------------------------------------------------

run_lint() {
	local errors=0
	check_closing   || errors=$((errors + 1))
	check_artifacts || errors=$((errors + 1))
	check_secrets   || errors=$((errors + 1))
	check_draft || true   # warn-only

	if (( errors > 0 )); then
		if override_active; then
			echo "::notice::agent-lint: ${errors} hard check(s) failed but the override is active (label '${OVERRIDE_LABEL}' or body marker 'agent-lint:allow') — not blocking."
			return 0
		fi
		echo "::error::agent-lint: ${errors} check(s) failed (see annotations above). To override (use sparingly), add the '${OVERRIDE_LABEL}' label or '<!-- agent-lint:allow -->' to the PR body."
		return 1
	fi
	echo "agent-lint: all checks passed."
	return 0
}

# fetch_pr loads the lint subjects from a real PR via the gh CLI.
fetch_pr() {
	if [[ -z "$REPO" || -z "$PR" ]]; then
		echo "::notice::agent-lint: no pull_request context (AGENT_LINT_PR/REPO unset) — skipping."
		exit 0
	fi
	local meta
	if ! meta="$(gh pr view "$PR" --repo "$REPO" --json title,body,isDraft,labels 2>&1)"; then
		echo "::error::agent-lint: unable to read PR #${PR} in ${REPO}: ${meta}"
		exit 1
	fi
	TITLE="$(printf '%s' "$meta" | jq -r '.title // ""')"
	BODY="$(printf '%s' "$meta" | jq -r '.body // ""')"
	DRAFT="$(printf '%s' "$meta" | jq -r '.isDraft // false')"
	LABELS="$(printf '%s' "$meta" | jq -r '.labels[].name')"
	FILES="$(gh pr diff "$PR" --repo "$REPO" --name-only 2>/dev/null || true)"
	DIFF="$(gh pr diff "$PR" --repo "$REPO" 2>/dev/null || true)"
}

# ---- self-test --------------------------------------------------------------

self_test() {
	local fails=0
	_reset() { TITLE="" BODY="" DRAFT="false" LABELS="" FILES="" DIFF=""; ALLOWLIST=""; }
	_expect() {
		local desc="$1" want="$2" fn="$3" rc
		"$fn" >/dev/null 2>&1
		rc=$?
		if [[ "$rc" == "$want" ]]; then
			echo "ok   - $desc"
		else
			echo "FAIL - $desc (rc=$rc, want $want)"
			fails=$((fails + 1))
		fi
	}

	# 1. closing keywords
	_reset; BODY="Some change. Fixes #123"; _expect "closing: 'Fixes #123' trips" 1 check_closing
	_reset; BODY="Closes https://github.com/o/r/issues/9"; _expect "closing: issue URL trips" 1 check_closing
	_reset; TITLE="feat: thing (#5)"; BODY="Refs #5"; _expect "closing: 'Refs #5' passes" 0 check_closing
	_reset; BODY="Implements #5; this resolves the layout but closes nothing concrete"; _expect "closing: keyword without issue ref passes" 0 check_closing

	# 2. diff hygiene
	_reset; FILES=".maestro/verify.sh"; _expect "artifact: .maestro/ trips" 1 check_artifacts
	_reset; FILES=$'internal/x.go\ndebug.log'; _expect "artifact: *.log trips" 1 check_artifacts
	_reset; FILES="tmp/scratch.txt"; _expect "artifact: tmp/ trips" 1 check_artifacts
	_reset; FILES=$'pkg/foo_test.go\ninternal/server.go'; _expect "artifact: clean files pass" 0 check_artifacts
	_reset; FILES="docs/sample.test"; ALLOWLIST="docs/*.test"; _expect "artifact: allowlisted path passes" 0 check_artifacts; ALLOWLIST=""

	# 3. secret scan — fixtures built by concatenation so the source file itself
	#    carries no contiguous secret literal (and so cannot self-trip).
	local sk="sk-""ant-abcdef12345678" akia="AKIA""IOSFODNN7EXAMPLE" pk="-----BEGIN ""PRIVATE KEY-----"
	local ghp="ghp_$(printf 'A%.0s' {1..30})"
	_reset; DIFF=$'+++ b/config.yaml\n+token: '"$sk"; _expect "secret: sk-ant- trips" 1 check_secrets
	_reset; DIFF=$'+++ b/main.go\n+const t = "'"$ghp"'"'; _expect "secret: ghp_ trips" 1 check_secrets
	_reset; DIFF=$'+++ b/aws.txt\n+id = '"$akia"; _expect "secret: AKIA trips" 1 check_secrets
	_reset; DIFF=$'+++ b/id_rsa\n+'"$pk"; _expect "secret: private key trips" 1 check_secrets
	_reset; DIFF=$'+++ b/main.go\n+const region = "us-east-1"\n-old removed line'; _expect "secret: clean diff passes" 0 check_secrets

	# 4. draft check (warn-only → always rc 0)
	_reset; DRAFT="true"; _expect "draft: no marker warns (rc 0)" 0 check_draft
	_reset; DRAFT="true"; TITLE="[Partial] feat: x (#1)"; _expect "draft: with marker (rc 0)" 0 check_draft

	# escape hatch
	_reset; BODY="Fixes #1"; LABELS="$OVERRIDE_LABEL"; _expect "override: label downgrades failure" 0 run_lint
	_reset; BODY=$'Fixes #1\n<!-- agent-lint:allow -->'; _expect "override: body marker downgrades failure" 0 run_lint
	_reset; BODY="Fixes #1"; _expect "no override: failure blocks" 1 run_lint

	echo "----"
	if (( fails > 0 )); then
		echo "agent-lint self-test: ${fails} failure(s)"
		return 1
	fi
	echo "agent-lint self-test: all fixtures behaved as expected"
	return 0
}

# ---- entrypoint -------------------------------------------------------------

main() {
	if [[ "${1:-}" == "--self-test" ]]; then
		self_test
		exit $?
	fi

	if [[ -n "$PR" ]]; then
		fetch_pr
	else
		# offline dry-run: take subjects directly from the environment.
		TITLE="${AGENT_LINT_TITLE:-}"
		BODY="${AGENT_LINT_BODY:-}"
		DRAFT="${AGENT_LINT_DRAFT:-false}"
		LABELS="${AGENT_LINT_LABELS:-}"
		FILES="${AGENT_LINT_FILES:-}"
		DIFF="${AGENT_LINT_DIFF:-}"
	fi

	run_lint
	exit $?
}

main "$@"
