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
# Idempotent per head SHA: a model whose status already settled
# (success/failure) on the current head is skipped, so re-runs (cron,
# workflow retries, /llm-review comments) do not duplicate reviews or
# comments. A fresh pending status also skips (another run is mid-flight);
# a pending older than REVIEW_PENDING_STALE_MINUTES (default 30) is treated
# as a crashed run and retried.
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
TRUNC_NOTE=""
if (( diff_size > MAX_DIFF_BYTES )); then
    echo "llm-review: diff is $diff_size bytes; truncating to $MAX_DIFF_BYTES" >&2
    truncate -s "$MAX_DIFF_BYTES" "$DIFF_FILE"
    # Surfaced in the final status description: a verdict over a truncated
    # diff is weaker evidence and the operator must be able to see that.
    TRUNC_NOTE="; warning: diff truncated to $MAX_DIFF_BYTES bytes"
fi

# --- helpers ----------------------------------------------------------------

# status_settled <context> — true when this context needs no run on the head
# SHA: a SETTLED state (success/failure — the idempotency key), or a FRESH
# pending (younger than REVIEW_PENDING_STALE_MINUTES: another run is
# presumably mid-flight, e.g. a manual run racing CI). Retryable instead:
# error statuses (run failed, creds missing, unparseable output), pendings
# older than the threshold (the posting run died after phase 1 — the
# crashed-pending self-heal, #1148 round 2), and pendings whose created_at
# cannot be parsed (assume crashed rather than wedge). The combined-status
# endpoint returns one entry per context (the latest), so no dedup is needed.
status_settled() {
    local context="$1"
    local stale_minutes="${REVIEW_PENDING_STALE_MINUTES:-30}"
    local line state created_at
    line="$(gh api "repos/$REPO/commits/$HEAD_SHA/status" --jq \
        "[.statuses[] | select(.context == \"$context\")][0] // empty | [.state, (.created_at // \"\")] | @tsv" \
        2>/dev/null)" || line=""
    [[ -z "$line" ]] && return 1
    state="${line%%$'\t'*}"
    created_at="${line#*$'\t'}"
    case "$state" in
        success|failure)
            return 0
            ;;
        pending)
            local created_epoch now_epoch
            created_epoch="$(date -u -d "$created_at" +%s 2>/dev/null)" || created_epoch=""
            if [[ -z "$created_epoch" ]]; then
                echo "llm-review: $context pending with unparseable created_at (${created_at:-<empty>}) — treating as crashed, retrying" >&2
                return 1
            fi
            now_epoch="$(date -u +%s)"
            if (( now_epoch - created_epoch < stale_minutes * 60 )); then
                echo "llm-review: $context pending since $created_at (< ${stale_minutes}m) — another run appears to be in flight"
                return 0
            fi
            echo "llm-review: $context pending since $created_at (> ${stale_minutes}m) — treating as crashed, retrying" >&2
            return 1
            ;;
        *)
            return 1
            ;;
    esac
}

# post_status <context> <state> <description>
# Returns non-zero when the status API call fails, so callers can distinguish
# "posted" from "gh error" — a swallowed failure here is what used to let a
# transient gh error read as a green no-op run.
post_status() {
    local context="$1" state="$2" description="$3"
    if ! gh api "repos/$REPO/statuses/$HEAD_SHA" \
        -f state="$state" \
        -f context="$context" \
        -f description="$description" >/dev/null; then
        echo "llm-review: FAILED to post status $context=$state on $HEAD_SHA" >&2
        return 1
    fi
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

# prepare_model <stream-name> <mode>
# Phase 1 of the pending-first protocol: decide whether this model runs and
# publish its status BEFORE any model output or comment lands. A pending
# status for every model that will run means Maestro never observes the
# half-state "comments exist but a stream has no status yet" (the gate would
# read that stream as absent), and advisory comments posted mid-run cannot
# race a still-missing verdict.
# Return: 0 = run it, 1 = settled already (skip), 2 = skipped for missing
# creds (error status posted — the pair must not wedge on a silent stream),
# 3 = could not post the pending status (transient gh error) — the caller
# must abort the run with a non-zero exit instead of pretending phase 1
# happened; a green no-op run over a failed pending post would hide the
# protocol violation.
prepare_model() {
    local stream="$1" mode="$2"

    if status_settled "$stream"; then
        echo "llm-review: $stream needs no run on $HEAD_SHA — skipping (idempotent)"
        return 1
    fi

    if [[ "$mode" == terra && ( -z "${LLM_REVIEW_TERRA_BASE_URL:-}" || -z "${LLM_REVIEW_TERRA_AUTH_TOKEN:-}" ) ]]; then
        echo "llm-review: LLM_REVIEW_TERRA_BASE_URL/AUTH_TOKEN not set — skipping $stream" >&2
        # An explicit error status instead of silence: with one stream settled
        # and the other absent, Maestro's aggregate used to sit at
        # Pending+Observed forever with no escape (#1148 review round 1, P1-2).
        post_status "$stream" error "skipped: credentials not configured" || true
        return 2
    fi

    if ! post_status "$stream" pending "review in progress"; then
        return 3
    fi
    return 0
}

# run_model <stream-name> <mode>
# mode "opus":  subscription seat — ANTHROPIC_* endpoint vars must be unset.
# mode "terra": CLIProxy — ANTHROPIC_BASE_URL/AUTH_TOKEN + --model.
run_model() {
    local stream="$1" mode="$2"
    local context="$stream"

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

    # Strip markdown fences (models love wrapping output in ```-blocks) and
    # leading indentation so fenced findings and a fenced NO_FINDINGS still
    # parse, then keep only lines matching the contract.
    local parsed findings
    parsed="$(sed -e '/^[[:space:]]*```/d' -e 's/^[[:space:]]*//' <<<"$output")"
    findings="$(grep -E '^\[P[0-3]\] ' <<<"$parsed" || true)"

    # Fail closed on unparseable output (#1148 review round 1, P1-4): success
    # requires either at least one parsed finding line or the explicit
    # NO_FINDINGS sentinel. A refusal, an apology, or a format drift must
    # never read as "clean review".
    if [[ -z "$findings" ]] && ! grep -qE '^NO_FINDINGS[[:space:]]*$' <<<"$parsed"; then
        echo "llm-review: $context output matched neither findings nor NO_FINDINGS:" >&2
        echo "$output" >&2
        post_status "$context" error "review output unparseable"
        return 1
    fi

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
        post_status "$context" failure "$blocking blocking (P0/P1) of $total findings$TRUNC_NOTE"
    else
        post_status "$context" success "$total advisory findings, none blocking$TRUNC_NOTE"
    fi
}

# Phase 1: settle every model's status (pending / skipped-error) before any
# model runs or any comment is posted, so the pair is never half-observed.
rc=0
RUN_OPUS=0
RUN_TERRA=0
prep_rc=0
prepare_model "llm-review-opus" opus || prep_rc=$?
case "$prep_rc" in
    0) RUN_OPUS=1 ;;
    1) ;;       # settled / in flight — legitimate skip
    *) rc=1 ;;  # 2 = creds skip (error status posted), 3 = pending post failed
esac
prep_rc=0
prepare_model "llm-review-terra" terra || prep_rc=$?
case "$prep_rc" in
    0) RUN_TERRA=1 ;;
    1) ;;
    *) rc=1 ;;
esac

# Phase 2: run the reviews and flip each pending status to its final state.
if (( RUN_OPUS )); then
    run_model "llm-review-opus" opus || rc=1
fi
if (( RUN_TERRA )); then
    run_model "llm-review-terra" terra || rc=1
fi
exit "$rc"
