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

// The llm-review bot is part of the actionable-feedback reviewer set.
func TestIsReviewBotLogin_IncludesLLMReviewBot(t *testing.T) {
	if !isReviewBotLogin("okbot") {
		t.Fatal("okbot must count as a review bot login")
	}
	if !isReviewBotLogin("greptile-apps[bot]") {
		t.Fatal("greptile regression: existing bot logins must keep matching")
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
