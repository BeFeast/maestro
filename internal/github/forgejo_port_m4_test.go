package github

// Forgejo-mode tests for the #1172 M4 port: statuses-only CI rollup (D1),
// PRMergeStatus synthesis (D3), review-gate reads (D4), and the mergegate
// review-thread shim (D5). Same harness as forgejo_port_test.go: the REAL
// stack against an httptest fixture server with contract-verified Forgejo
// 16.0.1 JSON shapes, gh transport armed so any leak fails the test.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const (
	fjM4Head     = "59e99c49c27d3e2f73bae1657f07cd2f9a15f926"
	fjM4BaseHead = "63d046eb546c32ca4492c724ac6371f04507b18d"
)

var (
	fjM4PullPath    = "/repos/" + fjTestRepo + "/pulls/7"
	fjM4StatusPath  = "/repos/" + fjTestRepo + "/commits/" + fjM4Head + "/status"
	fjM4BranchPath  = "/repos/" + fjTestRepo + "/branches/main"
	fjM4ReviewsPath = "/repos/" + fjTestRepo + "/pulls/7/reviews"
)

// fjM4PullJSON is an open non-draft pull fixture; merge_base is parameterized
// for the D3 synthesis tests.
func fjM4PullJSON(mergeable, draft bool, mergeBase string) string {
	return fmt.Sprintf(`{"number":7,"title":"t","body":"","state":"open","draft":%t,"mergeable":%t,`+
		`"merged_at":null,"merge_commit_sha":null,"merge_base":%q,`+
		`"head":{"ref":"feat/x","sha":%q},"base":{"ref":"main"}}`, draft, mergeable, mergeBase, fjM4Head)
}

func fjM4BranchJSON() string {
	return fmt.Sprintf(`{"name":"main","commit":{"id":%q,"message":"m"}}`, fjM4BaseHead)
}

func fjM4CountPaths(seen []fjSeenReq, sub string) int {
	n := 0
	for _, req := range seen {
		if strings.Contains(req.Path, sub) {
			n++
		}
	}
	return n
}

// --- D1: statuses-only rollup ------------------------------------------------

func TestForgejoM4PRCheckRollup_StatusesOnly(t *testing.T) {
	c, seen, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath: fjM4PullJSON(true, false, fjM4BaseHead),
		// Per-status state lives in `.status`; `"state"` per row is a DECOY
		// that must not be decoded (contract gotcha #1).
		fjM4StatusPath: `{"state":"pending","total_count":2,"statuses":[
			{"id":1,"context":"llm-review-opus","status":"pending","state":"success","description":"queued","target_url":"","created_at":"2026-08-11T10:00:00Z"},
			{"id":2,"context":"llm-review-terra","status":"success","state":"failure","description":"ok","target_url":"","created_at":"2026-08-11T10:05:00Z"}
		]}`,
	}))
	rollup, err := c.PRCheckRollup(7)
	if err != nil {
		t.Fatalf("PRCheckRollup: %v", err)
	}
	if rollup.HeadSHA != fjM4Head || rollup.Verdict != "pending" || !rollup.Complete {
		t.Fatalf("rollup = %+v, want head=%s verdict=pending complete", rollup, fjM4Head)
	}
	if rollup.PendingCheckRuns {
		t.Fatal("PendingCheckRuns must stay false on forgejo — no check-runs exist; the pending signal is the Verdict")
	}
	if len(rollup.Fingerprint) != 16 {
		t.Fatalf("fingerprint = %q, want 16-char digest", rollup.Fingerprint)
	}
	if len(rollup.Signals) != 2 {
		t.Fatalf("signals = %+v, want two commit_status entries", rollup.Signals)
	}
	for _, sig := range rollup.Signals {
		if sig.Source != "commit_status" || sig.Status != sig.Conclusion {
			t.Fatalf("signal %+v: want commit_status with Status==Conclusion (GitHub statuses parity)", sig)
		}
	}
	if rollup.Signals[0].Name != "llm-review-opus" || rollup.Signals[0].Status != "pending" ||
		rollup.Signals[1].Name != "llm-review-terra" || rollup.Signals[1].Status != "success" {
		t.Fatalf("signals mis-mapped (the per-row .state decoy must lose to .status): %+v", rollup.Signals)
	}
	if n := fjM4CountPaths(*seen, "check-runs"); n != 0 {
		t.Fatalf("a check-runs read was attempted on forgejo (%d)", n)
	}

	// PRCIStatus rides the same rollup.
	status, err := c.PRCIStatus(7)
	if err != nil || status != "pending" {
		t.Fatalf("PRCIStatus = %q, %v; want pending, nil", status, err)
	}
}

// TestForgejoM4PRCheckRollup_NoSignalIsSuccess pins the LOUDLY-documented
// GitHub no-signal parity: the live no-signal shape (state:"", statuses:null)
// rolls up as "success". The review gate — not CI — is what still blocks such
// a head until the producer's pending-first statuses land.
func TestForgejoM4PRCheckRollup_NoSignalIsSuccess(t *testing.T) {
	c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath:   fjM4PullJSON(true, false, fjM4BaseHead),
		fjM4StatusPath: `{"state":"","total_count":0,"statuses":null}`,
	}))
	rollup, err := c.PRCheckRollup(7)
	if err != nil {
		t.Fatalf("PRCheckRollup: %v", err)
	}
	if rollup.Verdict != "success" || !rollup.Complete {
		t.Fatalf("rollup = %+v, want no-signal verdict=success (GitHub parity)", rollup)
	}
	if len(rollup.Signals) != 0 {
		t.Fatalf("signals = %+v, want none", rollup.Signals)
	}
	if rollup.Fingerprint == "" {
		t.Fatal("fingerprint must still be present (the combined= part) so no-signal->signal advances it")
	}
}

func TestForgejoM4PRCheckRollup_FailureAndNonFailureStates(t *testing.T) {
	c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath: fjM4PullJSON(true, false, fjM4BaseHead),
		fjM4StatusPath: `{"state":"failure","total_count":2,"statuses":[
			{"id":1,"context":"ci/test","status":"failure","description":"2 tests failed","target_url":"","created_at":"2026-08-11T10:00:00Z"},
			{"id":2,"context":"agent-lint","status":"success","description":"","target_url":"","created_at":"2026-08-11T10:01:00Z"}
		]}`,
	}))
	rollup, err := c.PRCheckRollup(7)
	if err != nil || rollup.Verdict != "failure" {
		t.Fatalf("rollup = %+v, %v; want verdict=failure", rollup, err)
	}

	// warning/skipped are real per-status values and must NOT read as failure.
	c2, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath: fjM4PullJSON(true, false, fjM4BaseHead),
		fjM4StatusPath: `{"state":"warning","total_count":2,"statuses":[
			{"id":1,"context":"lint","status":"warning","description":"","target_url":"","created_at":"2026-08-11T10:00:00Z"},
			{"id":2,"context":"optional","status":"skipped","description":"","target_url":"","created_at":"2026-08-11T10:01:00Z"}
		]}`,
	}))
	rollup2, err := c2.PRCheckRollup(7)
	if err != nil {
		t.Fatalf("PRCheckRollup(warning/skipped): %v", err)
	}
	if rollup2.Verdict != "success" {
		t.Fatalf("verdict = %q for warning+skipped statuses, want success (never failure)", rollup2.Verdict)
	}
}

// TestForgejoM4RollupFingerprintAdvancesOnStatusRerun pins the D1 fingerprint
// contract: forgejo has no check-run IDs, so a re-posted status with an
// UNCHANGED context+state must still advance the fingerprint via created_at —
// otherwise a manual producer re-run looks identical to the exhausted run it
// replaced and material progress never fires.
func TestForgejoM4RollupFingerprintAdvancesOnStatusRerun(t *testing.T) {
	statusBody := func(createdAt string) string {
		return fmt.Sprintf(`{"state":"pending","total_count":1,"statuses":[
			{"id":1,"context":"llm-review-opus","status":"pending","description":"","target_url":"","created_at":%q}]}`, createdAt)
	}
	run := func(createdAt string) string {
		c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
			fjM4PullPath:   fjM4PullJSON(true, false, fjM4BaseHead),
			fjM4StatusPath: statusBody(createdAt),
		}))
		rollup, err := c.PRCheckRollup(7)
		if err != nil {
			t.Fatalf("PRCheckRollup: %v", err)
		}
		return rollup.Fingerprint
	}
	first := run("2026-08-11T10:00:00Z")
	same := run("2026-08-11T10:00:00Z")
	rerun := run("2026-08-11T11:30:00Z")
	if first != same {
		t.Fatalf("identical statuses produced different fingerprints: %q vs %q", first, same)
	}
	if first == rerun {
		t.Fatalf("re-run (new created_at, same context+state) did not advance the fingerprint: %q", first)
	}
}

// TestGitHubFingerprintUnchangedByCreatedAtField pins github-mode invariance
// of the M4 struct change: the gh path never populates CreatedAt (json:"-"),
// so a gh-parsed status fingerprints exactly like the pre-M4 format.
func TestGitHubFingerprintUnchangedByCreatedAtField(t *testing.T) {
	var withField combinedStatusResponse
	withField.State = "pending"
	withField.Statuses = []combinedStatusEntry{{Context: "legacy", State: "pending"}}
	parsed, err := parseCombinedStatus([]byte(`{"state":"pending","statuses":[{"context":"legacy","state":"pending","created_at":"2026-08-11T10:00:00Z"}]}`))
	if err != nil {
		t.Fatalf("parseCombinedStatus: %v", err)
	}
	if parsed.Statuses[0].CreatedAt != "" {
		t.Fatalf("gh parse populated CreatedAt %q — GitHub fingerprints would all advance once on deploy", parsed.Statuses[0].CreatedAt)
	}
	if got, want := ciCheckRollupFingerprint(nil, parsed), ciCheckRollupFingerprint(nil, withField); got != want {
		t.Fatalf("gh-parsed fingerprint %q != literal fingerprint %q", got, want)
	}
}

func TestForgejoM4PRCheckRollup_HTTPError(t *testing.T) {
	c, _, _ := newForgejoPortClient(t, func(r *http.Request) (int, string) {
		if strings.HasSuffix(r.URL.Path, "/pulls/7") {
			return 200, fjM4PullJSON(true, false, fjM4BaseHead)
		}
		return 500, `{"message":"boom"}`
	})
	rollup, err := c.PRCheckRollup(7)
	if err == nil {
		t.Fatal("a 500 on the status read must surface as an error")
	}
	if rollup.Verdict != "unknown" || rollup.HeadSHA != fjM4Head {
		t.Fatalf("rollup = %+v, want verdict=unknown with the head preserved", rollup)
	}
	if errors.Is(err, ErrForgejoNotSupported) {
		t.Fatalf("err = %v: a transport failure on a PORTED read must not claim not-supported", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want the HTTP status visible", err)
	}
}

func TestForgejoM4PRChecksOutput(t *testing.T) {
	c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath: fjM4PullJSON(true, false, fjM4BaseHead),
		fjM4StatusPath: `{"state":"pending","total_count":2,"statuses":[
			{"id":1,"context":"llm-review-opus","status":"pending","description":"review queued","target_url":"","created_at":"2026-08-11T10:00:00Z"},
			{"id":2,"context":"ci/test","status":"success","description":"","target_url":"","created_at":"2026-08-11T10:01:00Z"}
		]}`,
	}))
	out, err := c.PRChecksOutput(7)
	if err != nil {
		t.Fatalf("PRChecksOutput: %v", err)
	}
	if !strings.Contains(out, "llm-review-opus\tpending\treview queued") || !strings.Contains(out, "ci/test\tsuccess") {
		t.Fatalf("overview = %q, want status rows with description column", out)
	}

	c2, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath:   fjM4PullJSON(true, false, fjM4BaseHead),
		fjM4StatusPath: `{"state":"","total_count":0,"statuses":null}`,
	}))
	out2, err := c2.PRChecksOutput(7)
	if err != nil || out2 != "no checks\n" {
		t.Fatalf("no-signal overview = %q, %v; want \"no checks\\n\"", out2, err)
	}
}

func TestForgejoM4PRFailingChecks(t *testing.T) {
	c, seen, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath: fjM4PullJSON(true, false, fjM4BaseHead),
		fjM4StatusPath: `{"state":"failure","total_count":5,"statuses":[
			{"id":1,"context":"ci/test","status":"failure","description":"2 tests failed in internal/worker","target_url":"","created_at":"2026-08-11T10:00:00Z"},
			{"id":2,"context":"llm-review-opus","status":"error","description":"producer credentials missing","target_url":"","created_at":"2026-08-11T10:01:00Z"},
			{"id":3,"context":"lint","status":"warning","description":"style nit","target_url":"","created_at":"2026-08-11T10:02:00Z"},
			{"id":4,"context":"optional","status":"skipped","description":"","target_url":"","created_at":"2026-08-11T10:03:00Z"},
			{"id":5,"context":"build","status":"success","description":"","target_url":"","created_at":"2026-08-11T10:04:00Z"}
		]}`,
	}))
	failing, err := c.PRFailingChecks(7)
	if err != nil {
		t.Fatalf("PRFailingChecks: %v", err)
	}
	if len(failing) != 2 {
		t.Fatalf("failing = %+v, want exactly failure+error (warning/skipped/success excluded)", failing)
	}
	if failing[0].Name != "ci/test" || failing[0].Conclusion != "failure" || failing[0].Excerpt != "2 tests failed in internal/worker" {
		t.Fatalf("failing[0] = %+v: the status description must be the excerpt", failing[0])
	}
	if failing[1].Name != "llm-review-opus" || failing[1].Conclusion != "error" || failing[1].Excerpt != "producer credentials missing" {
		t.Fatalf("failing[1] = %+v", failing[1])
	}
	if n := fjM4CountPaths(*seen, "check-runs"); n != 0 {
		t.Fatal("check-run/annotation reads must not happen on forgejo")
	}
}

func TestForgejoM4CIFailureSummary(t *testing.T) {
	c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath: fjM4PullJSON(true, false, fjM4BaseHead),
		fjM4StatusPath: `{"state":"failure","total_count":2,"statuses":[
			{"id":1,"context":"ci/test","status":"failure","description":"TestUmask failed","target_url":"https://forge.example.com/run/1","created_at":"2026-08-11T10:00:00Z"},
			{"id":2,"context":"build","status":"success","description":"","target_url":"","created_at":"2026-08-11T10:01:00Z"}
		]}`,
	}))
	summary, err := c.CIFailureSummary(7)
	if err != nil {
		t.Fatalf("CIFailureSummary: %v", err)
	}
	for _, want := range []string{
		"CI Check Overview:",
		"=== Failed check: ci/test ===",
		"TestUmask failed",
		"Details: https://forge.example.com/run/1",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q missing %q", summary, want)
		}
	}
	if strings.Contains(summary, "=== Failed check: build ===") {
		t.Fatal("a successful status must not get a failed-check section")
	}

	// No failed statuses -> the summary degrades to the plain overview.
	c2, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath:   fjM4PullJSON(true, false, fjM4BaseHead),
		fjM4StatusPath: `{"state":"success","total_count":1,"statuses":[{"id":1,"context":"build","status":"success","description":"","target_url":"","created_at":"2026-08-11T10:00:00Z"}]}`,
	}))
	summary2, err := c2.CIFailureSummary(7)
	if err != nil || strings.Contains(summary2, "CI Check Overview:") {
		t.Fatalf("summary = %q, %v; want the bare overview when nothing failed", summary2, err)
	}
}

func TestForgejoM4CIFailureSummary_CapAt8000(t *testing.T) {
	long := strings.Repeat("x", 9000)
	c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath:   fjM4PullJSON(true, false, fjM4BaseHead),
		fjM4StatusPath: fmt.Sprintf(`{"state":"failure","total_count":1,"statuses":[{"id":1,"context":"ci/test","status":"failure","description":%q,"target_url":"","created_at":"2026-08-11T10:00:00Z"}]}`, long),
	}))
	summary, err := c.CIFailureSummary(7)
	if err != nil {
		t.Fatalf("CIFailureSummary: %v", err)
	}
	if !strings.HasSuffix(summary, "\n... (truncated)") || len(summary) > 8000+len("\n... (truncated)") {
		t.Fatalf("summary length %d, truncated=%v; want the gh path's 8000-byte cap", len(summary), strings.HasSuffix(summary, "(truncated)"))
	}
}

// --- D3: PRMergeStatus synthesis ---------------------------------------------

func TestForgejoM4PRMergeStatus_UpToDate(t *testing.T) {
	c, seen, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath:   fjM4PullJSON(true, false, fjM4BaseHead),
		fjM4BranchPath: fjM4BranchJSON(),
	}))
	mergeable, mergeState, err := c.PRMergeStatus(7)
	if err != nil {
		t.Fatalf("PRMergeStatus: %v", err)
	}
	if mergeable != "MERGEABLE" || mergeState != "" {
		t.Fatalf("= (%q, %q), want (MERGEABLE, \"\") for merge_base == base head", mergeable, mergeState)
	}
	if n := fjM4CountPaths(*seen, "/branches/"); n != 1 {
		t.Fatalf("base branch head read %d times, want exactly 1 (never base.sha from the payload)", n)
	}
}

func TestForgejoM4PRMergeStatus_Behind(t *testing.T) {
	c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath:   fjM4PullJSON(true, false, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		fjM4BranchPath: fjM4BranchJSON(),
	}))
	mergeable, mergeState, err := c.PRMergeStatus(7)
	if err != nil {
		t.Fatalf("PRMergeStatus: %v", err)
	}
	if mergeable != "MERGEABLE" || mergeState != "behind" {
		t.Fatalf("= (%q, %q), want (MERGEABLE, behind): merge_base != current base head", mergeable, mergeState)
	}
}

func TestForgejoM4PRMergeStatus_DirtyConflict(t *testing.T) {
	c, seen, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath: fjM4PullJSON(false, false, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	}))
	mergeable, mergeState, err := c.PRMergeStatus(7)
	if err != nil {
		t.Fatalf("PRMergeStatus: %v", err)
	}
	if mergeable != "CONFLICTING" || mergeState != "dirty" {
		t.Fatalf("= (%q, %q), want (CONFLICTING, dirty): non-draft wire mergeable=false wins over behind", mergeable, mergeState)
	}
	if n := fjM4CountPaths(*seen, "/branches/"); n != 0 {
		t.Fatal("dirty must short-circuit before the base-head read")
	}
}

// TestForgejoM4PRMergeStatus_DraftFalseMergeableIsNotDirty pins the draft rule
// carried over from fjPullToREST: a draft's wire mergeable=false carries zero
// conflict information (the server forces it false on every draft), so it maps
// to UNKNOWN — and must NOT synthesize "dirty".
func TestForgejoM4PRMergeStatus_DraftFalseMergeableIsNotDirty(t *testing.T) {
	c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath:   fjM4PullJSON(false, true, fjM4BaseHead),
		fjM4BranchPath: fjM4BranchJSON(),
	}))
	mergeable, mergeState, err := c.PRMergeStatus(7)
	if err != nil {
		t.Fatalf("PRMergeStatus: %v", err)
	}
	if mergeable != "UNKNOWN" || mergeState != "" {
		t.Fatalf("= (%q, %q), want (UNKNOWN, \"\") for a draft with the contaminated false", mergeable, mergeState)
	}
}

func TestForgejoM4PRMergeStatus_EmptyMergeBase(t *testing.T) {
	c, seen, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath: fjM4PullJSON(true, false, ""),
	}))
	mergeable, mergeState, err := c.PRMergeStatus(7)
	if err != nil {
		t.Fatalf("PRMergeStatus: %v", err)
	}
	if mergeable != "MERGEABLE" || mergeState != "" {
		t.Fatalf("= (%q, %q), want (MERGEABLE, \"\") when merge_base is absent", mergeable, mergeState)
	}
	if n := fjM4CountPaths(*seen, "/branches/"); n != 0 {
		t.Fatal("no base-head read without a merge_base to compare against")
	}
}

// TestForgejoM4PRMergeStatus_NeverCleanOrUnstable sweeps the synthesis inputs
// and pins the D3 invariant that unlocks nothing GitHub-shaped: "clean" and
// "unstable" (the #424 promotion and mergeStateAllowsMerge triggers) are never
// produced on forgejo.
func TestForgejoM4PRMergeStatus_NeverCleanOrUnstable(t *testing.T) {
	cases := []struct {
		name      string
		pull      string
		wantState string
	}{
		{"up-to-date green", fjM4PullJSON(true, false, fjM4BaseHead), ""},
		{"behind", fjM4PullJSON(true, false, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), "behind"},
		{"conflict", fjM4PullJSON(false, false, fjM4BaseHead), "dirty"},
		{"draft", fjM4PullJSON(false, true, fjM4BaseHead), ""},
		{"no merge_base", fjM4PullJSON(true, false, ""), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
				fjM4PullPath:   tc.pull,
				fjM4BranchPath: fjM4BranchJSON(),
			}))
			_, mergeState, err := c.PRMergeStatus(7)
			if err != nil {
				t.Fatalf("PRMergeStatus: %v", err)
			}
			if mergeState == "clean" || mergeState == "unstable" {
				t.Fatalf("mergeState = %q — must never be synthesized on forgejo (#424/#425 escapes)", mergeState)
			}
			if mergeState != tc.wantState {
				t.Fatalf("mergeState = %q, want %q", mergeState, tc.wantState)
			}
		})
	}
}

// --- D4: review-gate reads ---------------------------------------------------

// fjM4ReviewFixtures wires reviews + per-review comments for the inline
// reader: review 45 is a producer-style one-comment COMMENT review anchored
// to the head; review 46 is a migrated review with an EMPTY review-level
// commit_id (per-comment commit_id is the fallback); review 47 is body-only
// (comments_count 0) and must not trigger a comments round trip.
func fjM4ReviewFixtures() map[string]string {
	return map[string]string{
		fjM4ReviewsPath: fmt.Sprintf(`[
			{"id":45,"user":{"login":"oklabs-bot"},"state":"COMMENT","body":"","commit_id":%q,"submitted_at":"2026-08-11T10:00:00Z","comments_count":1},
			{"id":46,"user":{"login":"Ghost"},"state":"COMMENT","body":"","commit_id":"","submitted_at":"2026-08-11T10:01:00Z","comments_count":1},
			{"id":47,"user":{"login":"oklabs-bot"},"state":"APPROVED","body":"lgtm","commit_id":%q,"submitted_at":"2026-08-11T10:02:00Z","comments_count":0}
		]`, fjM4Head, fjM4Head),
		fjM4ReviewsPath + "/45/comments": `[
			{"id":901,"body":"[P0] llm-review-opus: nil deref in reconcile","user":{"login":"oklabs-bot"},"path":"internal/x/x.go","commit_id":"1111111111111111111111111111111111111111","original_commit_id":"","position":12,"original_position":0,"created_at":"2026-08-11T10:00:00Z","line":999,"new_position":888}
		]`,
		fjM4ReviewsPath + "/46/comments": fmt.Sprintf(`[
			{"id":902,"body":"[P2] llm-review-terra: naming nit","user":{"login":"Ghost"},"path":"internal/y/y.go","commit_id":%q,"original_commit_id":"","position":3,"original_position":0,"created_at":"2026-08-11T10:01:00Z"}
		]`, fjM4Head),
	}
}

func TestForgejoM4ReviewComments_MappingAndAnchoring(t *testing.T) {
	routes := fjM4ReviewFixtures()
	routes[fjM4PullPath] = fjM4PullJSON(true, false, fjM4BaseHead)
	c, seen, _ := newForgejoPortClient(t, fjRoute(t, routes))

	comments, err := c.greptileReviewComments(7)
	if err != nil {
		t.Fatalf("greptileReviewComments: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("comments = %+v, want two (body-only review skipped)", comments)
	}
	// Review 45: the REVIEW-level commit_id anchors, beating the per-comment
	// decoy — without this reviewCommentTargetsHead leaks historical findings
	// across heads.
	if comments[0].CommitID != fjM4Head {
		t.Fatalf("comment[0].CommitID = %q, want the review-level anchor %q (per-comment decoy must lose)", comments[0].CommitID, fjM4Head)
	}
	// Line comes from `position` (the read-side line number); the `line` /
	// `new_position` decoys in the fixture must not be decoded.
	if comments[0].Line != 12 {
		t.Fatalf("comment[0].Line = %d, want 12 (wire position)", comments[0].Line)
	}
	if comments[0].User.Login != "oklabs-bot" || comments[0].Path != "internal/x/x.go" {
		t.Fatalf("comment[0] = %+v", comments[0])
	}
	if comments[0].OriginalCommitID != "" {
		t.Fatalf("OriginalCommitID = %q, want empty (deliberately unmapped — CommitID is the head anchor)", comments[0].OriginalCommitID)
	}
	// Review 46: empty review-level commit_id falls back to the per-comment one.
	if comments[1].CommitID != fjM4Head {
		t.Fatalf("comment[1].CommitID = %q, want the per-comment fallback %q", comments[1].CommitID, fjM4Head)
	}
	for _, req := range *seen {
		if strings.Contains(req.Path, "/47/comments") {
			t.Fatal("a body-only review (comments_count 0) must not trigger a comments round trip")
		}
	}
}

// TestForgejoM4PRReviewGateVerdict_DefaultStreamsAndSuccess pins the D4
// default: a forgejo-mode client with EMPTY streams gates on the llm-review
// pair (never the greptile default, which would sit unobserved forever).
func TestForgejoM4PRReviewGateVerdict_DefaultStreamsAndSuccess(t *testing.T) {
	c, seen, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath: fjM4PullJSON(true, false, fjM4BaseHead),
		fjM4StatusPath: `{"state":"success","total_count":2,"statuses":[
			{"id":1,"context":"llm-review-opus","status":"success","description":"","target_url":"","created_at":"2026-08-11T10:00:00Z"},
			{"id":2,"context":"llm-review-terra","status":"success","description":"","target_url":"","created_at":"2026-08-11T10:01:00Z"}
		]}`,
		fjM4ReviewsPath: `[]`,
	}))
	verdict, err := c.PRReviewGateVerdict(7, nil)
	if err != nil {
		t.Fatalf("PRReviewGateVerdict: %v", err)
	}
	if len(verdict.Streams) != 2 || verdict.Streams[0].Name != "llm-review-opus" || verdict.Streams[1].Name != "llm-review-terra" {
		t.Fatalf("streams = %+v, want the llm-review pair as the forgejo default", verdict.Streams)
	}
	if !verdict.Passed || verdict.Pending || !verdict.Observed {
		t.Fatalf("verdict = %+v, want passed+observed on two success statuses", verdict)
	}
	if n := fjM4CountPaths(*seen, "check-runs"); n != 0 {
		t.Fatal("the check-runs read must be skipped entirely on forgejo")
	}
}

// TestForgejoM4NamedStreamVerdict_ErrorStatusIsRejection pins the #1148
// producer escape on forgejo: a creds-missing lens posts state "error", which
// must read as found-NOT-passed (hard rejection with the named finding) —
// never as pending, which would hold the PR forever.
func TestForgejoM4NamedStreamVerdict_ErrorStatusIsRejection(t *testing.T) {
	c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath:    fjM4PullJSON(true, false, fjM4BaseHead),
		fjM4StatusPath:  `{"state":"error","total_count":1,"statuses":[{"id":1,"context":"llm-review-opus","status":"error","description":"credentials missing","target_url":"","created_at":"2026-08-11T10:00:00Z"}]}`,
		fjM4ReviewsPath: `[]`,
	}))
	verdict, err := c.PRReviewGateVerdict(7, []string{"llm-review-opus"})
	if err != nil {
		t.Fatalf("PRReviewGateVerdict: %v", err)
	}
	if verdict.Passed || verdict.Pending {
		t.Fatalf("verdict = %+v, want rejection (not passed, NOT pending) on an error status", verdict)
	}
	if len(verdict.Streams) != 1 || !verdict.Streams[0].Observed {
		t.Fatalf("streams = %+v, want one observed stream", verdict.Streams)
	}
	findings := verdict.BlockingFindings()
	if len(findings) != 1 || findings[0].Body != "llm-review-opus review check did not pass" {
		t.Fatalf("findings = %+v, want the named did-not-pass finding", findings)
	}
}

func TestForgejoM4NamedStreamVerdict_PendingStatusBlocks(t *testing.T) {
	c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath:    fjM4PullJSON(true, false, fjM4BaseHead),
		fjM4StatusPath:  `{"state":"pending","total_count":1,"statuses":[{"id":1,"context":"llm-review-opus","status":"pending","description":"queued","target_url":"","created_at":"2026-08-11T10:00:00Z"}]}`,
		fjM4ReviewsPath: `[]`,
	}))
	verdict, err := c.PRReviewGateVerdict(7, []string{"llm-review-opus"})
	if err != nil {
		t.Fatalf("PRReviewGateVerdict: %v", err)
	}
	if !verdict.Pending || verdict.Passed || !verdict.Observed {
		t.Fatalf("verdict = %+v, want pending+observed on a pending status", verdict)
	}
}

// TestForgejoM4SimplicityStreamUnobserved pins the D4 decision that a stream
// without AllowCommitStatus has NO external verdict source on forgejo: no
// status read happens and the stream reports unobserved-pending (the
// missing-review policy's signal), not a fabricated verdict.
func TestForgejoM4SimplicityStreamUnobserved(t *testing.T) {
	c, seen, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath:    fjM4PullJSON(true, false, fjM4BaseHead),
		fjM4ReviewsPath: `[]`,
	}))
	verdict, err := c.PRReviewGateVerdict(7, []string{"simplicity"})
	if err != nil {
		t.Fatalf("PRReviewGateVerdict: %v", err)
	}
	if len(verdict.Streams) != 1 {
		t.Fatalf("streams = %+v", verdict.Streams)
	}
	sv := verdict.Streams[0]
	if !sv.Pending || sv.Observed || sv.LookupFailed {
		t.Fatalf("simplicity stream = %+v, want unobserved pending with clean reads", sv)
	}
	if n := fjM4CountPaths(*seen, "/status"); n != 0 {
		t.Fatal("a stream without AllowCommitStatus must not read the combined status")
	}
}

func TestForgejoM4PRHasCriticalReviewOnHead(t *testing.T) {
	routes := fjM4ReviewFixtures()
	routes[fjM4PullPath] = fjM4PullJSON(true, false, fjM4BaseHead)
	c, _, _ := newForgejoPortClient(t, fjRoute(t, routes))

	// llm arm live: the P0 comment on head from the fleet bot blocks when the
	// llm streams are configured.
	critical, err := c.PRHasCriticalReviewOnHead(7, []string{"llm-review"})
	if err != nil {
		t.Fatalf("PRHasCriticalReviewOnHead: %v", err)
	}
	if !critical {
		t.Fatal("a P0 llm-review comment on head must block with llm streams configured")
	}
	// Without llm streams the llm arm is off, and the greptile arm never
	// matches on forgejo (no greptile bot) — naturally skipped, not guarded.
	critical, err = c.PRHasCriticalReviewOnHead(7, nil)
	if err != nil {
		t.Fatalf("PRHasCriticalReviewOnHead(nil streams): %v", err)
	}
	if critical {
		t.Fatal("without llm streams the fleet-bot P0 must not count (greptile arm cannot match on forgejo)")
	}
}

func TestForgejoM4CollectReviewFeedback(t *testing.T) {
	routes := fjM4ReviewFixtures()
	routes[fjM4PullPath] = fjM4PullJSON(true, false, fjM4BaseHead)
	c, _, _ := newForgejoPortClient(t, fjRoute(t, routes))

	comments, err := c.CollectReviewFeedback(7, []string{"llm-review"})
	if err != nil {
		t.Fatalf("CollectReviewFeedback: %v", err)
	}
	if len(comments) != 1 || comments[0].User != "oklabs-bot" || comments[0].Path != "internal/x/x.go" || comments[0].Line != 12 {
		t.Fatalf("comments = %+v, want exactly the head-anchored oklabs-bot finding (Ghost is not a collectable login)", comments)
	}
}

func TestForgejoM4CollectPRReviewFeedback(t *testing.T) {
	routes := fjM4ReviewFixtures()
	routes[fjM4PullPath] = fjM4PullJSON(true, false, fjM4BaseHead)
	routes["/repos/"+fjTestRepo+"/issues/7/comments"] = `[
		{"id":1,"body":"llm-review summary: changes requested in internal/x/x.go:12","user":{"login":"oklabs-bot"},"created_at":"2026-08-11T10:00:00Z"},
		{"id":2,"body":"unrelated human chatter","user":{"login":"oleg"},"created_at":"2026-08-11T10:01:00Z"}
	]`
	c, seen, _ := newForgejoPortClient(t, fjRoute(t, routes))

	feedback, err := c.CollectPRReviewFeedback(7, []string{"llm-review"})
	if err != nil {
		t.Fatalf("CollectPRReviewFeedback: %v", err)
	}
	if !strings.Contains(feedback, "llm-review summary: changes requested") {
		t.Fatalf("feedback %q missing the issue-level summary section", feedback)
	}
	if !strings.Contains(feedback, "internal/x/x.go") || !strings.Contains(feedback, "[P0] llm-review-opus") {
		t.Fatalf("feedback %q missing the inline finding", feedback)
	}
	if strings.Contains(feedback, "unrelated human chatter") {
		t.Fatal("non-reviewer comments must not be collected")
	}
	if n := fjM4CountPaths(*seen, "/issues/7/comments"); n != 1 {
		t.Fatalf("issue comments read %d times via the forgejo route, want 1", n)
	}
}

// --- D5: mergegate review-thread shim ----------------------------------------

func TestForgejoM4ReviewThreadShim(t *testing.T) {
	c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath: fjM4PullJSON(true, false, fjM4BaseHead),
	}))
	head, threads, err := c.PRUnresolvedReviewThreadsOnHead(7)
	if err != nil {
		t.Fatalf("PRUnresolvedReviewThreadsOnHead: %v", err)
	}
	if head != fjM4Head {
		t.Fatalf("head = %q, want the pull's current head %q (the boundary's second head read)", head, fjM4Head)
	}
	if threads != nil {
		t.Fatalf("threads = %+v, want nil (no unresolved-conversations API on forgejo)", threads)
	}
}

func TestForgejoM4ReviewThreadShim_EmptyHeadRefuses(t *testing.T) {
	c, _, _ := newForgejoPortClient(t, fjRoute(t, map[string]string{
		fjM4PullPath: `{"number":7,"title":"t","body":"","state":"open","draft":false,"mergeable":true,"merged_at":null,"merge_commit_sha":null,"merge_base":"","head":{"ref":"feat/x","sha":""},"base":{"ref":"main"}}`,
	}))
	_, _, err := c.PRUnresolvedReviewThreadsOnHead(7)
	if err == nil || !strings.Contains(err.Error(), "head SHA is empty") {
		t.Fatalf("err = %v, want the empty-head refusal preserved", err)
	}
}
