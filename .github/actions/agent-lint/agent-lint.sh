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
#                     unexpected result. No network and no `gh` needed (`jq` is).
#   (PR resolvable)   fetch the PR title/body/draft/labels/diff from the forge
#                     and lint it. Used by CI. Two arms, see forge_kind:
#                     GitHub via the `gh` CLI, Forgejo via curl + the REST API.
#   (otherwise)       offline dry-run: lint the values passed directly via
#                     AGENT_LINT_TITLE / _BODY / _DRAFT / _LABELS / _FILES / _DIFF.
#
set -uo pipefail

# ---- inputs / config --------------------------------------------------------
REPO="${AGENT_LINT_REPO:-${GITHUB_REPOSITORY:-}}"
PR="${AGENT_LINT_PR:-}"
OVERRIDE_LABEL="${AGENT_LINT_OVERRIDE_LABEL:-agent-lint:override}"
ALLOWLIST="${AGENT_LINT_ARTIFACT_ALLOWLIST:-}"

# API token. The GitHub composite action plumbs its `github-token` input into
# GH_TOKEN (what the gh CLI reads); the Forgejo workflow passes the job token
# the same way, and Forgejo Actions also exports GITHUB_TOKEN. Accept either so
# neither arm needs a bespoke variable.
TOKEN="${GH_TOKEN:-${GITHUB_TOKEN:-}}"

# Lint subjects (populated by gh-fetch, offline env, or self-test fixtures).
TITLE="" BODY="" DRAFT="false" LABELS="" FILES="" DIFF=""

# ---- helpers ----------------------------------------------------------------

# mask hides the bulk of a matched token so secrets are never echoed in full.
mask() {
	local s="$1" L=${#1}
	if (( L <= 6 )); then printf '******'; else printf '%s…%s' "${s:0:4}" "${s: -2}"; fi
}

lc() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

# forge_kind picks which fetch arm loads the PR: "github" or "forgejo".
#
# Explicit signal wins: AGENT_LINT_FORGE=github|forgejo|gitea. CI sets it
# (.forgejo/workflows/agent-lint.yml passes `forgejo`) so a real run never
# depends on sniffing.
#
# The fallback discriminator is GITHUB_API_URL, which both runners export as
# their own API root: GitHub uses https://api.github.com (GitHub Enterprise:
# https://<host>/api/v3) while Forgejo and Gitea use https://<host>/api/v1. The
# `/api/v1` path suffix is therefore the signal — deliberately NOT the hostname,
# because a Forgejo instance lives on an arbitrary domain (git.oklabs.uk here)
# and hostname matching would need a hardcoded allowlist. Unknown or unset ->
# github, which preserves the historical behaviour of this script.
forge_kind() {
	case "$(lc "${AGENT_LINT_FORGE:-}")" in
		github) printf 'github'; return 0 ;;
		forgejo|gitea) printf 'forgejo'; return 0 ;;
		'') ;;
		*) echo "::warning::agent-lint: unrecognised AGENT_LINT_FORGE '${AGENT_LINT_FORGE}' — falling back to auto-detection." >&2 ;;
	esac
	case "${GITHUB_API_URL:-}" in
		*/api/v1|*/api/v1/) printf 'forgejo' ;;
		*) printf 'github' ;;
	esac
}

# files_from_diff derives the changed-file list from a unified diff, standing in
# for `gh pr diff --name-only` on forges without the gh CLI.
#
# It mirrors `git diff --name-only`: both sides of every file header are
# emitted (so a rename reports the old and the new path, and a deletion still
# reports the removed path — matching what the gh arm feeds check_artifacts),
# /dev/null is dropped, the a/ and b/ prefixes are stripped, and duplicates are
# collapsed while preserving first-seen order.
#
# Header lines are only honoured between a `diff --git` line and the `+++` that
# closes that header, so hunk content that happens to look like a header (an
# added line reading "++ b/foo.log" arrives as "+++ b/foo.log") cannot inject a
# phantom path. A file with no ---/+++ pair at all — a pure rename, or a binary
# or mode-only change — falls back to parsing the `diff --git a/X b/Y` line.
files_from_diff() {
	printf '%s\n' "$1" | awk '
		function emit(p) {
			sub(/\t.*$/, "", p)
			if (p == "/dev/null" || p == "") return
			sub(/^[ab]\//, "", p)
			if (p == "" || (p in seen)) return
			seen[p] = 1
			print p
		}
		function flush_hdr(   n) {
			if (hdr == "") return
			n = hdr
			sub(/^diff --git /, "", n)
			if (match(n, / b\//)) {
				emit(substr(n, 1, RSTART - 1))
				emit(substr(n, RSTART + 1))
			}
			hdr = ""
		}
		/^diff --git /      { flush_hdr(); hdr = $0; inhdr = 1; next }
		inhdr && /^--- /    { emit(substr($0, 5)); next }
		inhdr && /^\+\+\+ / { emit(substr($0, 5)); hdr = ""; inhdr = 0; next }
		{ next }
		END { flush_hdr() }
	'
}

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

# resolve_pr fills REPO/PR from the Actions event payload when the workflow did
# not pass them explicitly.
#
# GitHub's pull_request payload and Forgejo's Gitea-shaped one both carry
# `.pull_request.number` (Forgejo's PR object numbers by per-repo index, which
# is exactly the {index} its REST API wants) and `.repository.full_name`, so one
# lookup serves both. Forgejo additionally repeats the index at the top level as
# `.number`; it is tried second as a belt-and-braces fallback. This matters
# because the composite action's `${{ github.event.pull_request.number }}`
# default is the only source on GitHub, and an empty render there would silently
# drop the script into offline dry-run mode and report a false green.
resolve_pr() {
	local ev="${GITHUB_EVENT_PATH:-}"
	[[ -n "$PR" && -n "$REPO" ]] && return 0
	[[ -n "$ev" && -r "$ev" ]] || return 0
	if [[ -z "$PR" ]]; then
		PR="$(jq -r '(.pull_request.number // .number // empty) | tostring' "$ev" 2>/dev/null || true)"
		[[ "$PR" == "null" ]] && PR=""
	fi
	if [[ -z "$REPO" ]]; then
		REPO="$(jq -r '.repository.full_name // empty' "$ev" 2>/dev/null || true)"
	fi
	return 0
}

# fj_get performs one authenticated Forgejo REST GET, printing the response body
# on stdout and returning non-zero on transport failure or a non-2xx status (the
# body is still printed so the caller can quote the API's error JSON).
fj_get() {
	local url="$1" accept="${2:-application/json}" resp code
	local hdrs=(-H "Accept: ${accept}")
	[[ -n "$TOKEN" ]] && hdrs+=(-H "Authorization: token ${TOKEN}")
	if ! resp="$(curl -sS -L --max-time 60 "${hdrs[@]}" -w $'\n%{http_code}' "$url" 2>&1)"; then
		printf '%s' "$resp"
		return 1
	fi
	code="${resp##*$'\n'}"
	printf '%s' "${resp%$'\n'*}"
	case "$code" in 2*) return 0 ;; esac
	return 1
}

# apply_forgejo_meta maps one Forgejo pull-request object onto the lint subjects.
# Split out from fetch_pr_forgejo so the self-test can exercise the field
# mapping without a network. Forgejo names the draft flag `.draft` where gh's
# JSON calls it `isDraft`; both are normalised to the same "true"/"false"
# strings that check_draft expects.
apply_forgejo_meta() {
	local meta="$1"
	TITLE="$(printf '%s' "$meta" | jq -r '.title // ""')"
	BODY="$(printf '%s' "$meta" | jq -r '.body // ""')"
	DRAFT="$(printf '%s' "$meta" | jq -r 'if (.draft // false) then "true" else "false" end')"
	LABELS="$(printf '%s' "$meta" | jq -r '(.labels // [])[].name')"
}

# fetch_pr_forgejo loads the lint subjects from a Forgejo/Gitea instance. The gh
# CLI cannot talk to Forgejo, so this arm uses curl + jq against the REST API:
#
#   GET /repos/{owner}/{repo}/pulls/{index}        -> title, body, draft, labels[].name
#   GET /repos/{owner}/{repo}/pulls/{index}.diff   -> the unified diff
#
# The changed-file list is derived from the diff (see files_from_diff) rather
# than from GET /repos/{owner}/{repo}/pulls/{index}/files: it is one request
# fewer, it has no pagination to get wrong (that endpoint pages, and a silently
# truncated first page would shrink the artifact check to the first N files),
# and it guarantees FILES and DIFF describe the same bytes.
fetch_pr_forgejo() {
	local api meta
	api="${AGENT_LINT_API_URL:-${GITHUB_API_URL:-}}"
	api="${api%/}"
	if [[ -z "$api" ]]; then
		echo "::error::agent-lint: Forgejo mode selected but neither AGENT_LINT_API_URL nor GITHUB_API_URL is set."
		exit 1
	fi
	if ! meta="$(fj_get "${api}/repos/${REPO}/pulls/${PR}")"; then
		echo "::error::agent-lint: unable to read PR #${PR} in ${REPO}: ${meta}"
		exit 1
	fi
	apply_forgejo_meta "$meta"
	# Unlike the gh arm this fails loud: a diff we cannot read would silently
	# disable the artifact and secret checks.
	if ! DIFF="$(fj_get "${api}/repos/${REPO}/pulls/${PR}.diff" 'text/plain')"; then
		echo "::error::agent-lint: unable to read the diff for PR #${PR} in ${REPO}: ${DIFF}"
		exit 1
	fi
	FILES="$(files_from_diff "$DIFF")"
}

# fetch_pr_github loads the lint subjects from a real PR via the gh CLI.
fetch_pr_github() {
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

# fetch_pr dispatches to the arm for the forge this run is on.
fetch_pr() {
	if [[ -z "$REPO" || -z "$PR" ]]; then
		echo "::notice::agent-lint: no pull_request context (AGENT_LINT_PR/REPO unset) — skipping."
		exit 0
	fi
	case "$(forge_kind)" in
		forgejo) fetch_pr_forgejo ;;
		*) fetch_pr_github ;;
	esac
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

	# 5. Forgejo fetch arm — everything that can be checked without a network:
	#    the forge discriminator, the diff -> changed-file derivation, and the
	#    PR-object field mapping. The HTTP calls themselves are not covered.
	_kv() {
		local desc="$1" want="$2" got="$3"
		if [[ "$got" == "$want" ]]; then
			echo "ok   - $desc"
		else
			echo "FAIL - $desc"
			echo "       got:  $(printf '%s' "$got" | tr '\n' '|')"
			echo "       want: $(printf '%s' "$want" | tr '\n' '|')"
			fails=$((fails + 1))
		fi
	}

	# 5a. forge_kind: explicit signal beats auto-detection, both ways.
	_kv "forge: explicit forgejo beats a github api url" "forgejo" \
		"$(AGENT_LINT_FORGE=forgejo; GITHUB_API_URL=https://api.github.com; forge_kind)"
	_kv "forge: explicit github beats a forgejo api url" "github" \
		"$(AGENT_LINT_FORGE=github; GITHUB_API_URL=https://git.example.org/api/v1; forge_kind)"
	_kv "forge: auto-detect /api/v1 → forgejo" "forgejo" \
		"$(AGENT_LINT_FORGE=; GITHUB_API_URL=https://git.example.org/api/v1; forge_kind)"
	_kv "forge: auto-detect trailing-slash /api/v1/ → forgejo" "forgejo" \
		"$(AGENT_LINT_FORGE=; GITHUB_API_URL=https://git.example.org/api/v1/; forge_kind)"
	_kv "forge: auto-detect api.github.com → github" "github" \
		"$(AGENT_LINT_FORGE=; GITHUB_API_URL=https://api.github.com; forge_kind)"
	_kv "forge: auto-detect GHES /api/v3 → github" "github" \
		"$(AGENT_LINT_FORGE=; GITHUB_API_URL=https://ghe.example.com/api/v3; forge_kind)"
	_kv "forge: no api url → github" "github" \
		"$(AGENT_LINT_FORGE=; GITHUB_API_URL=; forge_kind)"

	# 5b. files_from_diff: mirrors `git diff --name-only` off a unified diff.
	_kv "diff→files: modified file" "internal/x.go" \
		"$(files_from_diff "$(printf '%s\n' \
			'diff --git a/internal/x.go b/internal/x.go' \
			'index 1111111..2222222 100644' \
			'--- a/internal/x.go' \
			'+++ b/internal/x.go' \
			'@@ -1 +1 @@' \
			'-old' \
			'+new')")"
	_kv "diff→files: added file drops /dev/null" "tmp/scratch.txt" \
		"$(files_from_diff "$(printf '%s\n' \
			'diff --git a/tmp/scratch.txt b/tmp/scratch.txt' \
			'new file mode 100644' \
			'--- /dev/null' \
			'+++ b/tmp/scratch.txt' \
			'@@ -0,0 +1 @@' \
			'+hi')")"
	_kv "diff→files: deleted file still reported" "debug.log" \
		"$(files_from_diff "$(printf '%s\n' \
			'diff --git a/debug.log b/debug.log' \
			'deleted file mode 100644' \
			'--- a/debug.log' \
			'+++ /dev/null' \
			'@@ -1 +0,0 @@' \
			'-noise')")"
	_kv "diff→files: pure rename reports both paths" $'docs/old.md\ndocs/new.md' \
		"$(files_from_diff "$(printf '%s\n' \
			'diff --git a/docs/old.md b/docs/new.md' \
			'similarity index 100%' \
			'rename from docs/old.md' \
			'rename to docs/new.md')")"
	_kv "diff→files: binary change falls back to the git header" "assets/logo.png" \
		"$(files_from_diff "$(printf '%s\n' \
			'diff --git a/assets/logo.png b/assets/logo.png' \
			'index 3333333..4444444 100644' \
			'Binary files a/assets/logo.png and b/assets/logo.png differ')")"
	_kv "diff→files: hunk content cannot inject a phantom path" "README.md" \
		"$(files_from_diff "$(printf '%s\n' \
			'diff --git a/README.md b/README.md' \
			'--- a/README.md' \
			'+++ b/README.md' \
			'@@ -1,2 +1,3 @@' \
			' context' \
			'+++ b/phantom.log' \
			'--- a/phantom.log')")"
	_kv "diff→files: multiple files keep order and dedup" $'a.go\nb.go' \
		"$(files_from_diff "$(printf '%s\n' \
			'diff --git a/a.go b/a.go' \
			'--- a/a.go' \
			'+++ b/a.go' \
			'@@ -1 +1 @@' \
			'-x' \
			'+y' \
			'diff --git a/b.go b/b.go' \
			'--- a/b.go' \
			'+++ b/b.go' \
			'@@ -1 +1 @@' \
			'-x' \
			'+y')")"
	_kv "diff→files: empty diff yields nothing" "" "$(files_from_diff "")"

	# 5c. the derived FILES must feed the existing checks unchanged.
	_reset
	DIFF="$(printf '%s\n' \
		'diff --git a/.maestro/verify.sh b/.maestro/verify.sh' \
		'new file mode 100755' \
		'--- /dev/null' \
		'+++ b/.maestro/verify.sh' \
		'@@ -0,0 +1 @@' \
		'+echo hi')"
	FILES="$(files_from_diff "$DIFF")"
	_expect "forgejo arm: derived FILES trips the artifact check" 1 check_artifacts

	# 5d. apply_forgejo_meta: Forgejo PR object → lint subjects.
	_reset
	apply_forgejo_meta '{"title":"feat: thing (#5)","body":"Refs #5","draft":true,"labels":[{"name":"size:small"},{"name":"area:ci"}]}'
	_kv "forgejo meta: title" "feat: thing (#5)" "$TITLE"
	_kv "forgejo meta: body" "Refs #5" "$BODY"
	_kv "forgejo meta: .draft true → \"true\"" "true" "$DRAFT"
	_kv "forgejo meta: labels newline-joined" $'size:small\narea:ci' "$LABELS"
	_reset
	apply_forgejo_meta '{"title":"t","body":null,"labels":null}'
	_kv "forgejo meta: null body → empty" "" "$BODY"
	_kv "forgejo meta: missing .draft → \"false\"" "false" "$DRAFT"
	_kv "forgejo meta: null labels → empty" "" "$LABELS"
	_reset
	apply_forgejo_meta '{"title":"t","body":"Fixes #1","draft":false,"labels":[{"name":"'"$OVERRIDE_LABEL"'"}]}'
	_expect "forgejo meta: override label still downgrades failure" 0 run_lint
	_reset
	apply_forgejo_meta '{"title":"t","body":"Fixes #1","draft":false,"labels":[]}'
	_expect "forgejo meta: no override still blocks" 1 run_lint

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

	# Recover REPO/PR from the event payload if the workflow did not pass them,
	# so a blank expression render cannot demote a real CI run to dry-run.
	resolve_pr

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
