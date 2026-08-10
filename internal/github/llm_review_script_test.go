package github

// Regression tests for scripts/llm-review.sh — the glue runner behind the
// llm-review gate (#1148). The script is run for real with stubbed `gh` and
// `claude` binaries on PATH; the gh stub evaluates the script's --jq
// expressions with the actual jq, so the idempotency query is exercised
// against real combined-status JSON rather than a canned answer.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func llmReviewScriptPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "scripts", "llm-review.sh"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("scripts/llm-review.sh not found at %s: %v", p, err)
	}
	return p
}

const llmScriptHeadSHA = "headsha4567890abcdef"

// ghStub answers the script's gh invocations. Every call is appended to
// $STUB_DIR/calls so tests can assert ordering (pending statuses must precede
// comments). The combined-status endpoint pipes $STUB_DIR/combined-status.json
// through the REAL jq with the script's own --jq expression.
const ghStub = `#!/usr/bin/env bash
set -u
printf '%s\n' "$*" >> "$STUB_DIR/calls"
cmd="${1:-}"; sub="${2:-}"
if [[ "$cmd" == "pr" && "$sub" == "view" ]]; then
    printf '{"headRefOid":"headsha4567890abcdef","baseRefName":"main","title":"test PR","number":7}\n'
elif [[ "$cmd" == "pr" && "$sub" == "diff" ]]; then
    cat "$STUB_DIR/diff"
elif [[ "$cmd" == "pr" && "$sub" == "comment" ]]; then
    exit 0
elif [[ "$cmd" == "api" ]]; then
    endpoint="$sub"
    case "$endpoint" in
        */commits/*/status)
            expr=""
            prev=""
            for a in "$@"; do
                if [[ "$prev" == "--jq" ]]; then expr="$a"; fi
                prev="$a"
            done
            jq -r "$expr" < "$STUB_DIR/combined-status.json"
            ;;
        */statuses/*)
            if [[ -n "${STUB_FAIL_STATUS_POST:-}" ]]; then exit 1; fi
            exit 0
            ;;
        *)
            exit 0
            ;;
    esac
fi
exit 0
`

const claudeStub = `#!/usr/bin/env bash
printf 'claude %s\n' "$*" >> "$STUB_DIR/calls"
cat "$STUB_DIR/claude-output"
exit 0
`

// cursorAgentStub mirrors the real cursor-agent (live-verified to read the
// prompt from stdin): it consumes stdin fully (so the piping printf never hits
// SIGPIPE under the script's pipefail) AND asserts the prompt actually arrived
// — an empty stdin means the script stopped piping the prompt+diff, which would
// otherwise ship a lens that reviews nothing yet posts success. On empty stdin
// it exits non-zero so the cursor branch errors and the test fails loudly.
const cursorAgentStub = `#!/usr/bin/env bash
piped="$(cat)"
if [[ -z "$piped" ]]; then
    echo "cursor-agent stub: empty stdin — prompt was not piped" >&2
    exit 3
fi
printf 'cursor-agent %s\n' "$*" >> "$STUB_DIR/calls"
cat "$STUB_DIR/claude-output"
exit 0
`

type llmScriptResult struct {
	exitCode int
	calls    []string
	output   string
}

// runLLMReviewScript executes the script with stub binaries. claudeOutput is
// what both model passes "print"; combinedStatus is the pre-existing commit
// status JSON on the head; terraCreds toggles the CLIProxy env pair.
func runLLMReviewScript(t *testing.T, claudeOutput, combinedStatus string, terraCreds bool, extraEnv ...string) llmScriptResult {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not in PATH")
	}

	stubDir := t.TempDir()
	writeStubFile(t, filepath.Join(stubDir, "gh"), ghStub, 0o755)
	writeStubFile(t, filepath.Join(stubDir, "claude"), claudeStub, 0o755)
	writeStubFile(t, filepath.Join(stubDir, "cursor-agent"), cursorAgentStub, 0o755)
	writeStubFile(t, filepath.Join(stubDir, "claude-output"), claudeOutput, 0o644)
	writeStubFile(t, filepath.Join(stubDir, "combined-status.json"), combinedStatus, 0o644)
	writeStubFile(t, filepath.Join(stubDir, "diff"), "diff --git a/a.go b/a.go\n+real change\n", 0o644)
	writeStubFile(t, filepath.Join(stubDir, "calls"), "", 0o644)

	cmd := exec.Command("bash", llmReviewScriptPath(t), "7", "owner/repo")
	cmd.Env = append([]string{
		"PATH=" + stubDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + stubDir,
		"TMPDIR=" + stubDir,
		"STUB_DIR=" + stubDir,
	}, extraEnv...)
	if terraCreds {
		cmd.Env = append(cmd.Env,
			"LLM_REVIEW_TERRA_BASE_URL=http://proxy.test",
			"LLM_REVIEW_TERRA_AUTH_TOKEN=stub-token")
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run script: %v\n%s", err, out)
		}
		code = exitErr.ExitCode()
	}
	callsRaw, readErr := os.ReadFile(filepath.Join(stubDir, "calls"))
	if readErr != nil {
		t.Fatalf("read calls log: %v", readErr)
	}
	var calls []string
	for _, line := range strings.Split(string(callsRaw), "\n") {
		if strings.TrimSpace(line) != "" {
			calls = append(calls, line)
		}
	}
	return llmScriptResult{exitCode: code, calls: calls, output: string(out)}
}

func writeStubFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const emptyStatus = `{"state":"pending","statuses":[]}`

func callsMatching(calls []string, needles ...string) []string {
	var out []string
	for _, call := range calls {
		ok := true
		for _, needle := range needles {
			if !strings.Contains(call, needle) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, call)
		}
	}
	return out
}

// P1-4 regression: model output with neither a finding line nor the
// NO_FINDINGS sentinel must fail CLOSED — an error status, never success.
func TestLLMReviewScript_UnparseableOutputFailsClosed(t *testing.T) {
	res := runLLMReviewScript(t, "I'm sorry, I cannot review this diff.\n", emptyStatus, true)
	if res.exitCode == 0 {
		t.Fatalf("exit = 0, want non-zero for unparseable output\n%s", res.output)
	}
	for _, stream := range []string{"llm-review-opus", "llm-review-terra"} {
		if len(callsMatching(res.calls, "statuses/"+llmScriptHeadSHA, "state=error", "context="+stream, "review output unparseable")) == 0 {
			t.Errorf("no error status with 'review output unparseable' for %s\ncalls:\n%s", stream, strings.Join(res.calls, "\n"))
		}
		if len(callsMatching(res.calls, "state=success", "context="+stream)) != 0 {
			t.Errorf("%s posted success on unparseable output (fail-open)\ncalls:\n%s", stream, strings.Join(res.calls, "\n"))
		}
	}
}

// P1-4: a fenced NO_FINDINGS sentinel still counts as a clean review.
func TestLLMReviewScript_FencedNoFindingsIsSuccess(t *testing.T) {
	res := runLLMReviewScript(t, "```\nNO_FINDINGS\n```\n", emptyStatus, true)
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\n%s", res.exitCode, res.output)
	}
	for _, stream := range []string{"llm-review-opus", "llm-review-terra"} {
		if len(callsMatching(res.calls, "state=success", "context="+stream)) == 0 {
			t.Errorf("no success status for %s on a fenced NO_FINDINGS\ncalls:\n%s", stream, strings.Join(res.calls, "\n"))
		}
	}
}

// P1-4: findings wrapped in markdown fences must still parse and block.
func TestLLMReviewScript_FencedFindingsParse(t *testing.T) {
	res := runLLMReviewScript(t, "```\n[P0] internal/foo.go:12 — nil deref on empty verdict\n```\n", emptyStatus, true)
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0 (a blocking finding is a successful run)\n%s", res.exitCode, res.output)
	}
	if len(callsMatching(res.calls, "state=failure", "context=llm-review-opus", "1 blocking")) == 0 {
		t.Fatalf("no failure status with the blocking count for opus\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
	if len(callsMatching(res.calls, "pulls/7/comments", "[P0]")) == 0 {
		t.Fatalf("no inline comment carrying the [P0] marker\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
}

// P1-2 (glue side) regression: missing terra creds must post an explicit
// error status — a silent skip left the pair half-observed and wedged the
// Maestro gate forever.
func TestLLMReviewScript_MissingTerraCredsPostsErrorStatus(t *testing.T) {
	res := runLLMReviewScript(t, "NO_FINDINGS\n", emptyStatus, false)
	if res.exitCode == 0 {
		t.Fatalf("exit = 0, want non-zero when a configured model cannot run\n%s", res.output)
	}
	if len(callsMatching(res.calls, "state=error", "context=llm-review-terra", "skipped: credentials not configured")) == 0 {
		t.Fatalf("no explicit skipped-creds error status for terra\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
	// The opus pass must still complete normally.
	if len(callsMatching(res.calls, "state=success", "context=llm-review-opus")) == 0 {
		t.Fatalf("opus did not finish despite terra being skipped\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
}

// The cursor lens is opt-in: with no LLM_REVIEW_STREAMS the script runs exactly
// the opus+terra pair and never invokes cursor-agent or posts a cursor status.
// This is the behaviour-preserving guarantee for existing rows.
func TestLLMReviewScript_CursorLensOffByDefault(t *testing.T) {
	res := runLLMReviewScript(t, "NO_FINDINGS\n", emptyStatus, true, "CURSOR_API_KEY=stub-cursor-key")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\n%s", res.exitCode, res.output)
	}
	if len(callsMatching(res.calls, "cursor-agent ")) != 0 {
		t.Errorf("cursor-agent ran without LLM_REVIEW_STREAMS opting it in\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
	if len(callsMatching(res.calls, "context=llm-review-cursor")) != 0 {
		t.Errorf("a cursor status was posted while the lens is off by default\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
}

// When LLM_REVIEW_STREAMS opts the cursor lens in and CURSOR_API_KEY is present,
// cursor-agent runs and posts an llm-review-cursor status, reusing the shared
// parser (a fenced NO_FINDINGS reads as a clean success).
func TestLLMReviewScript_CursorLensRunsWhenEnabled(t *testing.T) {
	res := runLLMReviewScript(t, "NO_FINDINGS\n", emptyStatus, true,
		"CURSOR_API_KEY=stub-cursor-key",
		"LLM_REVIEW_STREAMS=llm-review-opus,llm-review-terra,llm-review-cursor")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\n%s", res.exitCode, res.output)
	}
	if len(callsMatching(res.calls, "cursor-agent ")) == 0 {
		t.Errorf("cursor-agent was not invoked despite being opted in\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
	if len(callsMatching(res.calls, "state=success", "context=llm-review-cursor")) == 0 {
		t.Errorf("no success status for the cursor lens\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
}

// The single selector makes the terra-free opus+cursor pairing (the whole
// point of the change) actually achievable: naming only opus+cursor must run
// exactly those two and never attempt terra.
func TestLLMReviewScript_OpusCursorPairingSkipsTerra(t *testing.T) {
	res := runLLMReviewScript(t, "NO_FINDINGS\n", emptyStatus, false, // terra creds absent on purpose
		"CURSOR_API_KEY=stub-cursor-key",
		"LLM_REVIEW_STREAMS=llm-review-opus, llm-review-cursor") // note the space: whitespace tolerated
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0 — terra must not run (and thus not error) when unselected\n%s", res.exitCode, res.output)
	}
	if len(callsMatching(res.calls, "context=llm-review-terra")) != 0 {
		t.Errorf("terra was attempted despite not being selected\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
	if len(callsMatching(res.calls, "state=success", "context=llm-review-opus")) == 0 ||
		len(callsMatching(res.calls, "state=success", "context=llm-review-cursor")) == 0 {
		t.Errorf("expected opus and cursor to both post success\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
}

// P1-2 parity: an opted-in cursor lens with no CURSOR_API_KEY must post an
// explicit error status (never a silent skip), exactly like the terra guard,
// so Maestro's gate settles instead of wedging on an unobserved stream.
func TestLLMReviewScript_CursorEnabledMissingKeyPostsError(t *testing.T) {
	res := runLLMReviewScript(t, "NO_FINDINGS\n", emptyStatus, true,
		"LLM_REVIEW_STREAMS=llm-review-opus,llm-review-cursor")
	if res.exitCode == 0 {
		t.Fatalf("exit = 0, want non-zero when the opted-in cursor lens cannot run\n%s", res.output)
	}
	if len(callsMatching(res.calls, "state=error", "context=llm-review-cursor", "skipped: credentials not configured")) == 0 {
		t.Fatalf("no skipped-creds error status for cursor\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
	if len(callsMatching(res.calls, "cursor-agent ")) != 0 {
		t.Errorf("cursor-agent ran despite a missing key\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
}

// P2 regression: an error status is retryable, not settled — a re-run must
// replace it. Only success/failure are the idempotency key.
func TestLLMReviewScript_ErrorStatusIsNotSettled(t *testing.T) {
	preexisting := `{"state":"error","statuses":[
		{"context":"llm-review-opus","state":"error"},
		{"context":"llm-review-terra","state":"pending"}]}`
	res := runLLMReviewScript(t, "NO_FINDINGS\n", preexisting, true)
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\n%s", res.exitCode, res.output)
	}
	for _, stream := range []string{"llm-review-opus", "llm-review-terra"} {
		if len(callsMatching(res.calls, "state=success", "context="+stream)) == 0 {
			t.Errorf("%s was not re-run over its error/stale-pending status\ncalls:\n%s", stream, strings.Join(res.calls, "\n"))
		}
	}
}

// Crashed-pending self-heal (#1148 round 2): a pending status older than
// REVIEW_PENDING_STALE_MINUTES means the run that posted it died mid-flight
// — a re-run must treat it as retryable and replace it.
func TestLLMReviewScript_StalePendingIsRetried(t *testing.T) {
	stale := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	preexisting := fmt.Sprintf(`{"state":"pending","statuses":[
		{"context":"llm-review-opus","state":"success","created_at":%q},
		{"context":"llm-review-terra","state":"pending","created_at":%q}]}`, stale, stale)
	res := runLLMReviewScript(t, "NO_FINDINGS\n", preexisting, true)
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\n%s", res.exitCode, res.output)
	}
	if len(callsMatching(res.calls, "state=success", "context=llm-review-terra")) == 0 {
		t.Fatalf("terra was not re-run over its stale pending status\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
	if len(callsMatching(res.calls, "state=pending", "context=llm-review-opus")) != 0 {
		t.Fatalf("settled opus must not be re-run\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
}

// The other side of the self-heal boundary: a FRESH pending (younger than the
// stale threshold) means another run is mid-flight — skip instead of racing
// it with duplicate reviews and comments.
func TestLLMReviewScript_FreshPendingSkips(t *testing.T) {
	fresh := time.Now().UTC().Add(-1 * time.Minute).Format(time.RFC3339)
	preexisting := fmt.Sprintf(`{"state":"pending","statuses":[
		{"context":"llm-review-opus","state":"success","created_at":%q},
		{"context":"llm-review-terra","state":"pending","created_at":%q}]}`, fresh, fresh)
	res := runLLMReviewScript(t, "NO_FINDINGS\n", preexisting, true)
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0 (a legitimate skip is not a failure)\n%s", res.exitCode, res.output)
	}
	if len(callsMatching(res.calls, "claude ")) != 0 {
		t.Fatalf("a fresh pending must not trigger a duplicate model run\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
}

// The threshold is env-overridable: with REVIEW_PENDING_STALE_MINUTES=1 even
// a few-minutes-old pending is already stale and retried.
func TestLLMReviewScript_PendingStaleMinutesOverride(t *testing.T) {
	recent := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	preexisting := fmt.Sprintf(`{"state":"pending","statuses":[
		{"context":"llm-review-opus","state":"success","created_at":%q},
		{"context":"llm-review-terra","state":"pending","created_at":%q}]}`, recent, recent)
	res := runLLMReviewScript(t, "NO_FINDINGS\n", preexisting, true,
		"REVIEW_PENDING_STALE_MINUTES=1")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\n%s", res.exitCode, res.output)
	}
	if len(callsMatching(res.calls, "state=success", "context=llm-review-terra")) == 0 {
		t.Fatalf("terra was not re-run with the lowered stale threshold\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
}

// prepare_model conflation fix (#1148 round 2): a transient gh failure while
// posting the pending status must abort the run with a non-zero exit — it is
// NOT the settled-skip case, and a green no-op run here would hide a review
// that never happened.
func TestLLMReviewScript_PendingPostFailureAbortsNonZero(t *testing.T) {
	res := runLLMReviewScript(t, "NO_FINDINGS\n", emptyStatus, true,
		"STUB_FAIL_STATUS_POST=1")
	if res.exitCode == 0 {
		t.Fatalf("exit = 0, want non-zero when the pending status cannot be posted\n%s", res.output)
	}
	if len(callsMatching(res.calls, "claude ")) != 0 {
		t.Fatalf("no model may run when phase 1 (pending-first) failed\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
}

// A genuinely settled status (success or failure) still short-circuits.
func TestLLMReviewScript_SettledStatusSkips(t *testing.T) {
	preexisting := `{"state":"failure","statuses":[
		{"context":"llm-review-opus","state":"success"},
		{"context":"llm-review-terra","state":"failure"}]}`
	res := runLLMReviewScript(t, "NO_FINDINGS\n", preexisting, true)
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\n%s", res.exitCode, res.output)
	}
	if len(callsMatching(res.calls, "claude ")) != 0 {
		t.Fatalf("a settled pair must not re-run any model\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
}

// P2 regression (pending-first protocol): both models' pending statuses must
// be on the head BEFORE any inline comment is posted, so Maestro can never
// observe comments from a stream that has no status yet.
func TestLLMReviewScript_PendingStatusesPrecedeComments(t *testing.T) {
	res := runLLMReviewScript(t, "[P1] internal/foo.go:12 — leaked handle\n", emptyStatus, true)
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\n%s", res.exitCode, res.output)
	}
	firstComment := -1
	pendingIdx := map[string]int{}
	for i, call := range res.calls {
		if strings.Contains(call, "pulls/7/comments") && firstComment == -1 {
			firstComment = i
		}
		for _, stream := range []string{"llm-review-opus", "llm-review-terra"} {
			if strings.Contains(call, "state=pending") && strings.Contains(call, "context="+stream) {
				if _, seen := pendingIdx[stream]; !seen {
					pendingIdx[stream] = i
				}
			}
		}
	}
	if firstComment == -1 {
		t.Fatalf("no inline comment was posted\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
	for _, stream := range []string{"llm-review-opus", "llm-review-terra"} {
		idx, ok := pendingIdx[stream]
		if !ok {
			t.Fatalf("no pending status for %s\ncalls:\n%s", stream, strings.Join(res.calls, "\n"))
		}
		if idx > firstComment {
			t.Errorf("pending status for %s (call %d) landed after the first comment (call %d)", stream, idx, firstComment)
		}
	}
}

// P2 regression: a truncated diff must be visible in the final status
// description — a verdict over partial input is weaker evidence.
func TestLLMReviewScript_TruncationWarningInStatus(t *testing.T) {
	res := runLLMReviewScript(t, "NO_FINDINGS\n", emptyStatus, true,
		"LLM_REVIEW_MAX_DIFF_BYTES=10")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\n%s", res.exitCode, res.output)
	}
	if len(callsMatching(res.calls, "state=success", "context=llm-review-opus", "diff truncated")) == 0 {
		t.Fatalf("final status description carries no truncation warning\ncalls:\n%s", strings.Join(res.calls, "\n"))
	}
}

// P2 regression: the /llm-review comment trigger must be restricted to
// member/owner/collaborator authors — anyone must not be able to spend
// review tokens by commenting the magic string.
func TestLLMReviewWorkflowCommentTriggerGatedOnAuthor(t *testing.T) {
	p, err := filepath.Abs(filepath.Join("..", "..", ".github", "workflows", "llm-review.yml"))
	if err != nil {
		t.Fatalf("resolve workflow path: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	wf := string(data)
	if !strings.Contains(wf, "github.event.comment.author_association") {
		t.Fatal("workflow does not gate the comment trigger on author_association")
	}
	for _, role := range []string{"MEMBER", "OWNER", "COLLABORATOR"} {
		if !strings.Contains(wf, fmt.Sprintf("%q", role)) {
			t.Errorf("workflow author gate is missing the %s role", role)
		}
	}
}

// Crashed-pending self-heal, workflow side (#1148 round 2): one run per PR at
// a time, superseded runs cancelled — a duplicate trigger must not race a
// live run into posting conflicting statuses.
func TestLLMReviewWorkflowHasPerPRConcurrencyGroup(t *testing.T) {
	p, err := filepath.Abs(filepath.Join("..", "..", ".github", "workflows", "llm-review.yml"))
	if err != nil {
		t.Fatalf("resolve workflow path: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	wf := string(data)
	if !strings.Contains(wf, "concurrency:") {
		t.Fatal("workflow has no concurrency block")
	}
	if !strings.Contains(wf, "llm-review-${{ github.event.pull_request.number || github.event.issue.number }}") {
		t.Fatal("concurrency group is not keyed per PR (pull_request.number || issue.number)")
	}
	if !strings.Contains(wf, "cancel-in-progress: true") {
		t.Fatal("superseded runs must be cancelled (cancel-in-progress: true)")
	}
}
