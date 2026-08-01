package github

import (
	"fmt"
	"strings"
	"testing"
)

// --- severity parser (#1148: plain-text markers join the Greptile badges) ---

func TestIsHighSeverity_PlainTextMarkers(t *testing.T) {
	blocking := []string{
		"[P0] internal/github/github.go:42 — nil deref on empty verdict",
		"[P1] cmd/maestro/main.go:10 — leaked file handle",
		"severity: P0 — data loss on restart",
		"Severity: P1 — wrong stream attribution",
		"severity:p1 compact form",
	}
	for _, body := range blocking {
		if !isHighSeverity(body) {
			t.Errorf("isHighSeverity(%q) = false, want true", body)
		}
	}
	advisory := []string{
		"[P2] internal/config/config.go:7 — naming nit",
		"[P3] docs typo",
		"severity: P2 — advisory only",
		"looks good to me",
		"",
	}
	for _, body := range advisory {
		if isHighSeverity(body) {
			t.Errorf("isHighSeverity(%q) = true, want false", body)
		}
	}
}

// Regression: the Greptile badge encodings must keep matching after the
// plain-text extension.
func TestIsHighSeverity_GreptileBadgesStillMatch(t *testing.T) {
	badges := []string{
		`<img alt="P0" src="x">`,
		`<img alt="P1" src="x">`,
		`https://img.shields.io/badge/P0-critical-red`,
		`something/P1 marker`,
	}
	for _, body := range badges {
		if !isHighSeverity(body) {
			t.Errorf("isHighSeverity(%q) = false, want true (badge regression)", body)
		}
	}
}

func TestIsCriticalSeverity_PlainTextMarkers(t *testing.T) {
	if !isCriticalSeverity("[P0] boom") {
		t.Error("[P0] plain-text marker must read as critical")
	}
	if !isCriticalSeverity("severity: P0") {
		t.Error("severity: P0 must read as critical")
	}
	if isCriticalSeverity("[P1] bad but not critical") {
		t.Error("[P1] must not read as critical")
	}
	if isCriticalSeverity("[P2] nit") {
		t.Error("[P2] must not read as critical")
	}
}

// --- stream whitelist + pair alias ------------------------------------------

func TestNormalizeReviewStreams_LLMReviewNames(t *testing.T) {
	got := normalizeReviewStreams([]string{"llm-review-opus", "llm-review-terra", "greptile"})
	want := []string{"llm-review-opus", "llm-review-terra", "greptile"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("normalizeReviewStreams = %v, want %v", got, want)
	}
}

func TestNormalizeReviewStreams_PairAliasExpands(t *testing.T) {
	got := normalizeReviewStreams([]string{"llm-review"})
	want := []string{"llm-review-opus", "llm-review-terra"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("normalizeReviewStreams(llm-review) = %v, want %v", got, want)
	}
}

func TestNormalizeReviewStreams_AliasDedupesAgainstExplicitNames(t *testing.T) {
	got := normalizeReviewStreams([]string{"llm-review-opus", "llm-review"})
	want := []string{"llm-review-opus", "llm-review-terra"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("normalizeReviewStreams = %v, want %v", got, want)
	}
}

func TestStreamEnabled_LLMReviewAlias(t *testing.T) {
	streams := []string{"llm-review"}
	if !streamEnabled(streams, "llm-review-opus") || !streamEnabled(streams, "llm-review-terra") {
		t.Fatal("the llm-review alias must enable both model streams")
	}
	if streamEnabled(streams, "greptile") {
		t.Fatal("the llm-review alias must not enable greptile")
	}
}

// --- external verdict decisions ---------------------------------------------

func TestNamedCheckDecision_LLMReviewCheckNames(t *testing.T) {
	checks := []greptileCheckRun{
		{Name: "llm-review-opus", Status: "completed", Conclusion: "success"},
		{Name: "llm-review-terra", Status: "completed", Conclusion: "failure"},
	}
	found, passed, pending := namedCheckDecision(checks, []string{"llm-review-opus"})
	if !found || !passed || pending {
		t.Fatalf("opus check: found=%v passed=%v pending=%v, want true/true/false", found, passed, pending)
	}
	found, passed, pending = namedCheckDecision(checks, []string{"llm-review-terra"})
	if !found || passed || pending {
		t.Fatalf("terra check: found=%v passed=%v pending=%v, want true/false/false", found, passed, pending)
	}
}

func TestNamedStatusDecision_CommitStatusStates(t *testing.T) {
	var combined combinedStatusResponse
	combined.Statuses = []struct {
		Context     string `json:"context"`
		State       string `json:"state"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url"`
	}{
		{Context: "llm-review-opus", State: "success"},
		{Context: "llm-review-terra", State: "failure"},
		{Context: "ci/build", State: "pending"},
	}

	found, passed, pending := namedStatusDecision(combined, []string{"llm-review-opus"})
	if !found || !passed || pending {
		t.Fatalf("opus status: found=%v passed=%v pending=%v, want true/true/false", found, passed, pending)
	}
	found, passed, pending = namedStatusDecision(combined, []string{"llm-review-terra"})
	if !found || passed || pending {
		t.Fatalf("terra status: found=%v passed=%v pending=%v, want true/false/false", found, passed, pending)
	}
	found, _, _ = namedStatusDecision(combined, []string{"llm-review-grok"})
	if found {
		t.Fatal("an absent context must not be found")
	}
	found, passed, pending = namedStatusDecision(combined, []string{"ci/build"})
	if !found || passed || !pending {
		t.Fatalf("pending status: found=%v passed=%v pending=%v, want true/false/true", found, passed, pending)
	}
}

// --- inline finding attribution + severity contract --------------------------

func llmComment(login, body, sha string) greptileReviewComment {
	cm := greptileReviewComment{Body: body, Path: "internal/foo.go", Line: 12, CommitID: sha}
	cm.User.Login = login
	return cm
}

func TestFilterStreamFindings_LLMReviewBlocksOnlyHighSeverity(t *testing.T) {
	spec := llmReviewStreamSpec("llm-review-opus")
	sha := "headsha"
	comments := []greptileReviewComment{
		llmComment("okbot", "[P0] nil deref\n\n<sub>llm-review-opus @ headsha</sub>", sha),
		llmComment("okbot", "[P2] naming nit\n\n<sub>llm-review-opus @ headsha</sub>", sha),
		llmComment("okbot", "[P3] typo\n\n<sub>llm-review-opus @ headsha</sub>", sha),
	}
	findings := filterStreamFindings(comments, sha, spec)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1 (only the P0 blocks; P2/P3 stay advisory)", len(findings))
	}
	if !strings.Contains(findings[0].Body, "[P0]") {
		t.Fatalf("finding body = %q, want the P0 comment", findings[0].Body)
	}
}

func TestFilterStreamFindings_StreamMarkerAttribution(t *testing.T) {
	spec := llmReviewStreamSpec("llm-review-opus")
	sha := "headsha"
	comments := []greptileReviewComment{
		llmComment("okbot", "[P1] opus finding\n\n<sub>llm-review-opus @ headsha</sub>", sha),
		llmComment("okbot", "[P1] terra finding\n\n<sub>llm-review-terra @ headsha</sub>", sha),
	}
	findings := filterStreamFindings(comments, sha, spec)
	if len(findings) != 1 || !strings.Contains(findings[0].Body, "opus finding") {
		t.Fatalf("findings = %+v, want only the opus-marked comment", findings)
	}
}

func TestFilterStreamFindings_IgnoresOtherLoginsAndOldHeads(t *testing.T) {
	spec := llmReviewStreamSpec("llm-review-opus")
	comments := []greptileReviewComment{
		llmComment("random-human", "[P0] scary but not the bot\n<sub>llm-review-opus</sub>", "headsha"),
		llmComment("okbot", "[P0] stale head\n<sub>llm-review-opus</sub>", "oldsha"),
	}
	if findings := filterStreamFindings(comments, "headsha", spec); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none (wrong login / stale head)", findings)
	}
}

func TestIsLLMReviewBotLogin(t *testing.T) {
	for _, login := range []string{"okbot", "OKBot", "llm-review-bot", "acme-llm-review"} {
		if !isLLMReviewBotLogin(login) {
			t.Errorf("isLLMReviewBotLogin(%q) = false, want true", login)
		}
	}
	for _, login := range []string{"greptile-apps[bot]", "kossoy", ""} {
		if isLLMReviewBotLogin(login) {
			t.Errorf("isLLMReviewBotLogin(%q) = true, want false", login)
		}
	}
}

// #1148 review round 1, P1-3: the llm-review bot logins must NOT be part of
// the global reviewer set. The glue posts as the fleet bot ("okbot"), which
// also comments on PRs for reasons that have nothing to do with reviews;
// folding it into isReviewBotLogin made every fleet-bot comment with a
// file:line reference look like review feedback on ALL projects, including
// greptile-gated rows, and triggered retry/close churn.
func TestIsReviewBotLogin_ExcludesLLMReviewBot(t *testing.T) {
	for _, login := range []string{"okbot", "OKBot", "llm-review-bot"} {
		if isReviewBotLogin(login) {
			t.Errorf("isReviewBotLogin(%q) = true, want false — llm-review logins are stream-scoped, not global", login)
		}
	}
	if !isReviewBotLogin("greptile-apps[bot]") {
		t.Fatal("greptile regression: existing bot logins must keep matching")
	}
	if !isReviewBotLogin("chatgpt-codex-connector[bot]") {
		t.Fatal("codex regression: existing bot logins must keep matching")
	}
}

// P1-3 regression through the real collector: a fleet-bot-style comment with a
// file:line position on a greptile row must NOT be collected as review
// feedback (pre-PR behavior), while a genuine Greptile comment still is.
func TestCollectReviewFeedback_FleetBotCommentNotCollected(t *testing.T) {
	sha := "abcdef1234567890"
	stubLLMReviewAPI(t, sha, `{"state":"success","statuses":[]}`,
		`[{"body":"deploy note: rollout toggle lives in internal/foo.go:12",
		   "path":"internal/foo.go","line":12,"commit_id":"abcdef1234567890",
		   "user":{"login":"okbot"}},
		  {"body":"[P1] leaked handle","path":"a.go","line":3,
		   "commit_id":"abcdef1234567890","user":{"login":"greptile-apps[bot]"}}]`)
	c := &Client{Repo: "owner/repo"}
	feedback, err := c.CollectReviewFeedback(7, []string{"greptile"})
	if err != nil {
		t.Fatalf("CollectReviewFeedback: %v", err)
	}
	if len(feedback) != 1 {
		t.Fatalf("feedback = %+v, want exactly the greptile comment — fleet-bot comments must not be review feedback", feedback)
	}
	if feedback[0].User != "greptile-apps[bot]" {
		t.Fatalf("feedback user = %q, want greptile-apps[bot]", feedback[0].User)
	}
}

// #1148 round 2, P1 — the complement of the fleet-bot regression above: on a
// row whose configured streams include llm-review, the bot's [P0] inline
// finding MUST be collected as review feedback. Round 1 removed okbot from
// the global reviewer set without adding the stream-scoped path, which left
// feedback permanently empty on llm-review rows: AutoRetryReviewFeedback
// never fired, sessions never reached retry_exhausted, and convergence
// (#565), review-repair, and the exhaustion notification were unreachable.
func TestCollectReviewFeedback_LLMReviewRowCollectsBotFinding(t *testing.T) {
	sha := "abcdef1234567890"
	stubLLMReviewAPI(t, sha, `{"state":"success","statuses":[]}`,
		`[{"body":"[P0] nil deref on empty verdict\n\n<sub>llm-review-opus @ abcdef123456</sub>",
		   "path":"internal/foo.go","line":12,"commit_id":"abcdef1234567890",
		   "user":{"login":"okbot"}}]`)
	c := &Client{Repo: "owner/repo"}
	feedback, err := c.CollectReviewFeedback(7, []string{"llm-review"})
	if err != nil {
		t.Fatalf("CollectReviewFeedback: %v", err)
	}
	if len(feedback) != 1 {
		t.Fatalf("feedback = %+v, want the bot's P0 finding — the llm-review repair circuit depends on it", feedback)
	}
	if feedback[0].User != "okbot" || !strings.Contains(feedback[0].Body, "[P0]") {
		t.Fatalf("feedback = %+v, want the okbot [P0] comment", feedback[0])
	}
}

// Same circuit one level up: CollectPRReviewFeedback on an llm-review row
// returns a non-empty prompt-ready string for a bot [P0] inline finding, so
// the orchestrator's AutoRetryReviewFeedback path has something to act on.
func TestCollectPRReviewFeedback_LLMReviewRowFeedbackNonEmpty(t *testing.T) {
	sha := "abcdef1234567890"
	stubLLMReviewAPI(t, sha, `{"state":"success","statuses":[]}`,
		`[{"body":"[P0] nil deref on empty verdict\n\n<sub>llm-review-opus @ abcdef123456</sub>",
		   "path":"internal/foo.go","line":12,"commit_id":"abcdef1234567890",
		   "user":{"login":"okbot"}}]`)
	c := &Client{Repo: "owner/repo"}
	feedback, err := c.CollectPRReviewFeedback(7, []string{"llm-review"})
	if err != nil {
		t.Fatalf("CollectPRReviewFeedback: %v", err)
	}
	if !strings.Contains(feedback, "[P0]") || !strings.Contains(feedback, "internal/foo.go") {
		t.Fatalf("feedback = %q, want the formatted P0 finding with its path", feedback)
	}
}

// --- convergence-merge critical check (#565 escape × #1148 streams) ----------

// P1-1 regression: on an llm-review row, the bot's P0 on head must hard-block
// the convergence merge exactly like a Greptile P0 does.
func TestPRHasCriticalReviewOnHead_LLMReviewP0Blocks(t *testing.T) {
	sha := "abcdef1234567890"
	stubLLMReviewAPI(t, sha, `{"state":"success","statuses":[]}`,
		`[{"body":"[P0] nil deref\n\n<sub>llm-review-opus @ abcdef123456</sub>",
		   "path":"internal/foo.go","line":3,"commit_id":"abcdef1234567890",
		   "user":{"login":"okbot"}}]`)
	c := &Client{Repo: "owner/repo"}
	critical, err := c.PRHasCriticalReviewOnHead(7, []string{"llm-review"})
	if err != nil {
		t.Fatalf("PRHasCriticalReviewOnHead: %v", err)
	}
	if !critical {
		t.Fatal("an llm-review P0 on head must block the convergence merge")
	}
}

// P0/P1 is the llm-review gate's entire blocking contract, so a P1 blocks the
// convergence escape too (unlike greptile, where only P0 is critical).
func TestPRHasCriticalReviewOnHead_LLMReviewP1Blocks(t *testing.T) {
	sha := "abcdef1234567890"
	stubLLMReviewAPI(t, sha, `{"state":"success","statuses":[]}`,
		`[{"body":"[P1] auth bypass on the retry path\n\n<sub>llm-review-terra @ abcdef123456</sub>",
		   "path":"internal/foo.go","line":3,"commit_id":"abcdef1234567890",
		   "user":{"login":"okbot"}}]`)
	c := &Client{Repo: "owner/repo"}
	critical, err := c.PRHasCriticalReviewOnHead(7, []string{"llm-review"})
	if err != nil {
		t.Fatalf("PRHasCriticalReviewOnHead: %v", err)
	}
	if !critical {
		t.Fatal("an llm-review P1 on head must block the convergence merge — P0/P1 is that gate's blocking contract")
	}
}

// Advisory llm-review findings (P2/P3) must not block the convergence escape.
func TestPRHasCriticalReviewOnHead_LLMReviewAdvisoryDoesNotBlock(t *testing.T) {
	sha := "abcdef1234567890"
	stubLLMReviewAPI(t, sha, `{"state":"success","statuses":[]}`,
		`[{"body":"[P2] naming nit\n\n<sub>llm-review-opus @ abcdef123456</sub>",
		   "path":"internal/foo.go","line":3,"commit_id":"abcdef1234567890",
		   "user":{"login":"okbot"}}]`)
	c := &Client{Repo: "owner/repo"}
	critical, err := c.PRHasCriticalReviewOnHead(7, []string{"llm-review"})
	if err != nil {
		t.Fatalf("PRHasCriticalReviewOnHead: %v", err)
	}
	if critical {
		t.Fatal("a P2 advisory finding must not block the convergence merge")
	}
}

// The stream scoping cuts both ways: on a greptile row a fleet-bot P0-looking
// comment is not part of the gate and must not block, while a Greptile P0
// still does (and P1 stays non-critical for greptile).
func TestPRHasCriticalReviewOnHead_StreamScoping(t *testing.T) {
	sha := "abcdef1234567890"
	stubLLMReviewAPI(t, sha, `{"state":"success","statuses":[]}`,
		`[{"body":"[P0] scary-looking fleet-bot note",
		   "path":"internal/foo.go","line":3,"commit_id":"abcdef1234567890",
		   "user":{"login":"okbot"}},
		  {"body":"<img alt=\"P1\" src=\"x\"> real greptile finding","path":"a.go","line":4,
		   "commit_id":"abcdef1234567890","user":{"login":"greptile-apps[bot]"}}]`)
	c := &Client{Repo: "owner/repo"}
	critical, err := c.PRHasCriticalReviewOnHead(7, []string{"greptile"})
	if err != nil {
		t.Fatalf("PRHasCriticalReviewOnHead: %v", err)
	}
	if critical {
		t.Fatal("greptile row: a fleet-bot comment must not block, and a greptile P1 is not critical")
	}
}

// #1148 round 2, P2: a Greptile P0 on head blocks the convergence merge even
// when greptile is NOT among the configured streams. The Greptile app keeps
// reviewing repos it is installed on after a project row migrates to another
// gate; the stream migration must not silently downgrade its P0 from
// hard-block to advisory.
func TestPRHasCriticalReviewOnHead_GreptileP0BlocksOutsideConfiguredStreams(t *testing.T) {
	sha := "abcdef1234567890"
	stubLLMReviewAPI(t, sha, `{"state":"success","statuses":[]}`,
		`[{"body":"<img alt=\"P0\" src=\"x\"> data loss on restart","path":"a.go","line":4,
		   "commit_id":"abcdef1234567890","user":{"login":"greptile-apps[bot]"}}]`)
	c := &Client{Repo: "owner/repo"}
	critical, err := c.PRHasCriticalReviewOnHead(7, []string{"simplicity"})
	if err != nil {
		t.Fatalf("PRHasCriticalReviewOnHead: %v", err)
	}
	if !critical {
		t.Fatal("a greptile P0 must block unconditionally — migration off the greptile stream must not downgrade it")
	}
}

func TestPRHasCriticalReviewOnHead_GreptileP0StillBlocks(t *testing.T) {
	sha := "abcdef1234567890"
	stubLLMReviewAPI(t, sha, `{"state":"success","statuses":[]}`,
		`[{"body":"<img alt=\"P0\" src=\"x\"> data loss on restart","path":"a.go","line":4,
		   "commit_id":"abcdef1234567890","user":{"login":"greptile-apps[bot]"}}]`)
	c := &Client{Repo: "owner/repo"}
	critical, err := c.PRHasCriticalReviewOnHead(7, []string{"greptile"})
	if err != nil {
		t.Fatalf("PRHasCriticalReviewOnHead: %v", err)
	}
	if !critical {
		t.Fatal("greptile P0 regression: the legacy critical block must keep working")
	}
}

// --- full verdict through the gh seam ----------------------------------------

// stubLLMReviewAPI dispatches stubbed gh api responses by endpoint. Responses
// are plain JSON bodies (no header block): resolveConditional trusts a
// successful run's raw output, so nothing is etag-cached between tests.
func stubLLMReviewAPI(t *testing.T, sha string, statuses string, comments string) {
	t.Helper()
	orig := ghAPIRunner
	resetPrimaryLimitForTest()
	ghAPIRunner = func(args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "/pulls/7/comments"):
			return []byte(comments), nil
		case strings.Contains(joined, "/issues/7/comments"):
			return []byte(`[]`), nil
		case strings.Contains(joined, "/check-runs"):
			return []byte(`{"check_runs":[]}`), nil
		case strings.Contains(joined, "/commits/"+sha+"/status"):
			return []byte(statuses), nil
		case strings.Contains(joined, "/pulls/7"):
			return []byte(fmt.Sprintf(`{"number":7,"head":{"sha":%q}}`, sha)), nil
		}
		return nil, fmt.Errorf("unexpected gh call: %s", joined)
	}
	t.Cleanup(func() {
		ghAPIRunner = orig
		resetPrimaryLimitForTest()
	})
}

func TestPRReviewGateVerdict_LLMReviewPairPassesOnGreenStatuses(t *testing.T) {
	sha := "abcdef1234567890"
	stubLLMReviewAPI(t, sha,
		`{"state":"success","statuses":[
			{"context":"llm-review-opus","state":"success"},
			{"context":"llm-review-terra","state":"success"}]}`,
		`[]`)
	c := &Client{Repo: "owner/repo"}
	verdict, err := c.PRReviewGateVerdict(7, []string{"llm-review"})
	if err != nil {
		t.Fatalf("PRReviewGateVerdict: %v", err)
	}
	if !verdict.Passed || verdict.Pending {
		t.Fatalf("verdict = %+v, want passed and not pending", verdict)
	}
	if !verdict.Observed {
		t.Fatal("two green statuses must count as an observed gate")
	}
	if len(verdict.Streams) != 2 {
		t.Fatalf("streams = %d, want 2 (opus + terra)", len(verdict.Streams))
	}
}

func TestPRReviewGateVerdict_LLMReviewOneRedStatusBlocks(t *testing.T) {
	sha := "abcdef1234567890"
	stubLLMReviewAPI(t, sha,
		`{"state":"failure","statuses":[
			{"context":"llm-review-opus","state":"success"},
			{"context":"llm-review-terra","state":"failure"}]}`,
		`[]`)
	c := &Client{Repo: "owner/repo"}
	verdict, err := c.PRReviewGateVerdict(7, []string{"llm-review"})
	if err != nil {
		t.Fatalf("PRReviewGateVerdict: %v", err)
	}
	if verdict.Passed {
		t.Fatal("a red terra status must block the gate")
	}
	if verdict.Pending {
		t.Fatal("a settled failure is not pending")
	}
}

func TestPRReviewGateVerdict_LLMReviewP0CommentBlocksDespiteGreenStatus(t *testing.T) {
	sha := "abcdef1234567890"
	stubLLMReviewAPI(t, sha,
		`{"state":"success","statuses":[
			{"context":"llm-review-opus","state":"success"},
			{"context":"llm-review-terra","state":"success"}]}`,
		`[{"body":"[P0] nil deref\n\n<sub>llm-review-opus @ abcdef123456</sub>",
		   "path":"internal/foo.go","line":3,"commit_id":"abcdef1234567890",
		   "user":{"login":"okbot"}}]`)
	c := &Client{Repo: "owner/repo"}
	verdict, err := c.PRReviewGateVerdict(7, []string{"llm-review"})
	if err != nil {
		t.Fatalf("PRReviewGateVerdict: %v", err)
	}
	if verdict.Passed {
		t.Fatal("a P0 inline finding must block even when the statuses are green")
	}
}

func TestPRReviewGateVerdict_LLMReviewAbsentStatusesArePending(t *testing.T) {
	sha := "abcdef1234567890"
	stubLLMReviewAPI(t, sha, `{"state":"pending","statuses":[]}`, `[]`)
	c := &Client{Repo: "owner/repo"}
	verdict, err := c.PRReviewGateVerdict(7, []string{"llm-review"})
	if err != nil {
		t.Fatalf("PRReviewGateVerdict: %v", err)
	}
	if !verdict.Pending {
		t.Fatal("a silent llm-review gate must read as pending")
	}
	if verdict.Observed {
		t.Fatal("no status, no check run, no comment — the gate was not observed (missing_after_minutes owns the escape)")
	}
	if verdict.LookupFailed {
		t.Fatal("clean reads must not be marked as lookup failures")
	}
}

// PRBlockingReviewFindingsOnHead is what scopes the auto-review-repair prompt.
func TestPRBlockingReviewFindingsOnHead_LLMReviewOnlyHighSeverity(t *testing.T) {
	sha := "abcdef1234567890"
	stubLLMReviewAPI(t, sha, `{"state":"success","statuses":[]}`,
		`[{"body":"[P1] leaked handle\n<sub>llm-review-terra</sub>","path":"a.go","line":1,
		   "commit_id":"abcdef1234567890","user":{"login":"okbot"}},
		  {"body":"[P2] nit\n<sub>llm-review-terra</sub>","path":"a.go","line":2,
		   "commit_id":"abcdef1234567890","user":{"login":"okbot"}}]`)
	c := &Client{Repo: "owner/repo"}
	_, findings, has, err := c.PRBlockingReviewFindingsOnHead(7, []string{"llm-review"})
	if err != nil {
		t.Fatalf("PRBlockingReviewFindingsOnHead: %v", err)
	}
	if !has || len(findings) != 1 {
		t.Fatalf("findings = %+v, want exactly the P1", findings)
	}
	if !strings.Contains(findings[0].Body, "[P1]") {
		t.Fatalf("finding = %q, want the P1 comment", findings[0].Body)
	}
}
