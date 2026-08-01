#!/usr/bin/env bash
# llm-review.sh — glue runner for the llm-review gate (#1148).
#
# Runs two single-shot LLM reviews of a PR head diff — claude-opus-5 (the
# subscription seat: plain `claude -p`) and gpt-5.6-terra (via CLIProxy:
# ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN + --model) — and publishes, per
# model:
#   1. inline PR review comments (one per finding, with a [P0]-[P3] severity
#      marker and the stream marker so Maestro can attribute them), and
#   2. a commit status `llm-review-opus` / `llm-review-terra` on the head SHA:
#      success unless the model reported any P0/P1 finding.
#
# Maestro's review gate (review_gate: llm-review) reads exactly these two
# surfaces: the status is the stream verdict, the P0/P1 comments are the
# blocking findings (internal/github/github.go llmReviewStreamSpec).
#
# Idempotent per head SHA: a model whose status already exists for the current
# head is skipped, so re-runs (cron, workflow retries, /llm-review comments)
# do not duplicate reviews or comments.
#
# Requirements: gh (authenticated), git, jq, claude CLI. For the terra pass:
# LLM_REVIEW_TERRA_BASE_URL + LLM_REVIEW_TERRA_AUTH_TOKEN (the CLIProxy
# endpoint/key) — without them the terra pass is skipped with a warning.
#
# Usage: llm-review.sh <pr-number> [owner/repo]

set -euo pipefail

PR_NUMBER="${1:-}"
REPO="${2:-${GH_REPO:-}}"

if [[ -z "$PR_NUMBER" ]]; then
    echo "usage: $0 <pr-number> [owner/repo]" >&2
    exit 2
fi
if [[ -z "$REPO" ]]; then
    REPO="$(gh repo view --json nameWithOwner --jq .nameWithOwner)"
fi

OPUS_MODEL="${LLM_REVIEW_OPUS_MODEL:-claude-opus-5}"
TERRA_MODEL="${LLM_REVIEW_TERRA_MODEL:-gpt-5.6-terra}"
MAX_DIFF_BYTES="${LLM_REVIEW_MAX_DIFF_BYTES:-400000}"

pr_json="$(gh pr view "$PR_NUMBER" --repo "$REPO" --json headRefOid,baseRefName,title,number)"
HEAD_SHA="$(jq -r .headRefOid <<<"$pr_json")"
BASE_REF="$(jq -r .baseRefName <<<"$pr_json")"
PR_TITLE="$(jq -r .title <<<"$pr_json")"

if [[ -z "$HEAD_SHA" || "$HEAD_SHA" == "null" ]]; then
    echo "llm-review: could not resolve head SHA for PR #$PR_NUMBER" >&2
    exit 1
fi

echo "llm-review: PR #$PR_NUMBER ($REPO) head=$HEAD_SHA base=$BASE_REF"

# --- diff of the PR head against its merge base -----------------------------
DIFF_FILE="$(mktemp -t llm-review-diff.XXXXXX)"
trap 'rm -f "$DIFF_FILE"' EXIT
gh pr diff "$PR_NUMBER" --repo "$REPO" >"$DIFF_FILE"
if [[ ! -s "$DIFF_FILE" ]]; then
    echo "llm-review: empty diff for PR #$PR_NUMBER — nothing to review" >&2
    exit 0
fi
diff_size=$(wc -c <"$DIFF_FILE")
if (( diff_size > MAX_DIFF_BYTES )); then
    echo "llm-review: diff is $diff_size bytes; truncating to $MAX_DIFF_BYTES" >&2
    truncate -s "$MAX_DIFF_BYTES" "$DIFF_FILE"
fi

# --- helpers ----------------------------------------------------------------

# status_exists <context> — true when a commit status with this context is
# already present on the head SHA (the idempotency key).
status_exists() {
    local context="$1"
    gh api "repos/$REPO/commits/$HEAD_SHA/status" --jq \
        "[.statuses[].context] | index(\"$context\") != null" 2>/dev/null | grep -qx true
}

# post_status <context> <state> <description>
post_status() {
    local context="$1" state="$2" description="$3"
    gh api "repos/$REPO/statuses/$HEAD_SHA" \
        -f state="$state" \
        -f context="$context" \
        -f description="$description" >/dev/null
    echo "llm-review: status $context=$state on $HEAD_SHA"
}

# post_inline_comment <context> <path> <line> <body>
# Falls back to a plain PR comment when the position is not in the diff.
post_inline_comment() {
    local context="$1" path="$2" line="$3" body="$4"
    if [[ -n "$path" && "$line" =~ ^[0-9]+$ && "$line" -gt 0 ]]; then
        if gh api "repos/$REPO/pulls/$PR_NUMBER/comments" \
            -f body="$body" \
            -f commit_id="$HEAD_SHA" \
            -f path="$path" \
            -F line="$line" \
            -f side=RIGHT >/dev/null 2>&1; then
            return 0
        fi
    fi
    # Position rejected (line outside the diff, renamed file, ...) — keep the
    # finding visible as a regular PR comment instead of dropping it.
    gh pr comment "$PR_NUMBER" --repo "$REPO" --body "$body (at \`$path:$line\`)" >/dev/null
}

REVIEW_PROMPT_TEMPLATE='You are a strict senior code reviewer. Review ONLY the diff below (PR "%s").
Report genuine defects, not style preferences.

Output format — one line per finding, nothing else:
[P0] path/to/file.ext:LINE — one-sentence description
[P1] path/to/file.ext:LINE — one-sentence description
[P2] path/to/file.ext:LINE — one-sentence description
[P3] path/to/file.ext:LINE — one-sentence description

Severity contract:
- P0: guaranteed breakage, data loss, security hole. Blocks merge.
- P1: real bug or correctness risk on a main path. Blocks merge.
- P2: minor defect or risky pattern. Advisory only.
- P3: nitpick. Advisory only.

LINE must be a new-file line number that appears in the diff. If there are no
findings, output exactly: NO_FINDINGS. Do not output anything else — no
preamble, no summary, no markdown fences.

DIFF:
'

# run_model <stream-name> <mode>
# mode "opus":  subscription seat — ANTHROPIC_* endpoint vars must be unset.
# mode "terra": CLIProxy — ANTHROPIC_BASE_URL/AUTH_TOKEN + --model.
run_model() {
    local stream="$1" mode="$2"
    local context="$stream"

    if status_exists "$context"; then
        echo "llm-review: $context already reported on $HEAD_SHA — skipping (idempotent)"
        return 0
    fi

    local prompt
    # shellcheck disable=SC2059
    prompt="$(printf "$REVIEW_PROMPT_TEMPLATE" "$PR_TITLE")"

    local output
    case "$mode" in
        opus)
            # Unset the CLIProxy redirection so the claude CLI talks to the
            # real Anthropic endpoint: the subscription seat on an operator
            # host, or ANTHROPIC_API_KEY on CI.
            if ! output="$( (env -u ANTHROPIC_BASE_URL -u ANTHROPIC_AUTH_TOKEN \
                    claude -p --model "$OPUS_MODEL" "$prompt$(cat "$DIFF_FILE")") 2>&1)"; then
                echo "llm-review: $context run failed: $output" >&2
                post_status "$context" error "review run failed"
                return 1
            fi
            ;;
        terra)
            if [[ -z "${LLM_REVIEW_TERRA_BASE_URL:-}" || -z "${LLM_REVIEW_TERRA_AUTH_TOKEN:-}" ]]; then
                echo "llm-review: LLM_REVIEW_TERRA_BASE_URL/AUTH_TOKEN not set — skipping $context" >&2
                return 0
            fi
            if ! output="$(ANTHROPIC_BASE_URL="$LLM_REVIEW_TERRA_BASE_URL" \
                    ANTHROPIC_AUTH_TOKEN="$LLM_REVIEW_TERRA_AUTH_TOKEN" \
                    claude -p --model "$TERRA_MODEL" "$prompt$(cat "$DIFF_FILE")" 2>&1)"; then
                echo "llm-review: $context run failed: $output" >&2
                post_status "$context" error "review run failed"
                return 1
            fi
            ;;
        *)
            echo "llm-review: unknown mode $mode" >&2
            return 1
            ;;
    esac

    # Parse findings: only lines matching the contract count.
    local findings
    findings="$(grep -E '^\[P[0-3]\] ' <<<"$output" || true)"

    local blocking=0 total=0
    if [[ -n "$findings" ]]; then
        while IFS= read -r line; do
            total=$((total + 1))
            local sev file_line path lnum msg
            sev="$(sed -E 's/^\[(P[0-3])\].*/\1/' <<<"$line")"
            file_line="$(sed -E 's/^\[P[0-3]\] +([^ ]+:[0-9]+).*/\1/' <<<"$line")"
            path="${file_line%%:*}"
            lnum="${file_line##*:}"
            msg="$(sed -E 's/^\[P[0-3]\] +[^ ]+ +[—-]+ *//' <<<"$line")"
            if [[ "$sev" == "P0" || "$sev" == "P1" ]]; then
                blocking=$((blocking + 1))
            fi
            # The [Px] marker is what Maestro's severity parser matches; the
            # trailing stream marker attributes the comment to this stream.
            post_inline_comment "$context" "$path" "$lnum" \
                "[$sev] $msg"$'\n\n'"<sub>$context @ ${HEAD_SHA:0:12}</sub>" || true
        done <<<"$findings"
    fi

    if (( blocking > 0 )); then
        post_status "$context" failure "$blocking blocking (P0/P1) of $total findings"
    else
        post_status "$context" success "$total advisory findings, none blocking"
    fi
}

rc=0
run_model "llm-review-opus" opus || rc=1
run_model "llm-review-terra" terra || rc=1
exit "$rc"
