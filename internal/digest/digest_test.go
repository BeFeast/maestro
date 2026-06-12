package digest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

var now = time.Date(2026, 6, 12, 7, 0, 0, 0, time.UTC)

// makeIssue creates a github.Issue with the given labels (works around json struct tags).
func makeIssue(number int, title, body string, labelNames ...string) github.Issue {
	issue := github.Issue{Number: number, Title: title, Body: body}
	for _, name := range labelNames {
		issue.Labels = append(issue.Labels, struct {
			Name string `json:"name"`
		}{Name: name})
	}
	return issue
}

type fakeGH struct {
	issues    []github.Issue
	issuesErr error
	prs       []github.PR
	prsErr    error
	closed    map[int]bool
	merged    map[int]bool
	lookupErr error
}

func (f *fakeGH) ListOpenIssues(labels []string) ([]github.Issue, error) {
	return f.issues, f.issuesErr
}
func (f *fakeGH) ListOpenPRs() ([]github.PR, error) { return f.prs, f.prsErr }
func (f *fakeGH) IsIssueClosed(n int) (bool, error) {
	if f.lookupErr != nil {
		return false, f.lookupErr
	}
	return f.closed[n], nil
}
func (f *fakeGH) HasMergedPRForIssue(n int) (bool, error) {
	if f.lookupErr != nil {
		return false, f.lookupErr
	}
	return f.merged[n], nil
}

func testOptions() Options {
	return Options{Now: now}
}

func baseProject(st *state.State, gh GitHubReader) Project {
	return Project{
		Name:            "demo",
		Repo:            "acme/demo",
		State:           st,
		GH:              gh,
		ReadyLabel:      "maestro-ready",
		BlockedLabel:    "maestro-blocked",
		ExcludedLabels:  []string{"epic", "meta"},
		BlockerPatterns: []string{`blocked by.*?#(\d+)`, `depends on.*?#(\d+)`},
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestPendingApprovalsRankedByRisk(t *testing.T) {
	st := state.NewState()
	st.Approvals = []state.Approval{
		{ID: "a-safe", Status: state.ApprovalStatusPending, Risk: "safe", Summary: "close issue #1", CreatedAt: now.Add(-time.Hour), Target: &state.SupervisorTarget{Issue: 1}},
		{ID: "a-danger", Status: state.ApprovalStatusPending, Risk: "danger", Summary: "force merge PR #2", CreatedAt: now.Add(-time.Hour), Target: &state.SupervisorTarget{PR: 2}},
		{ID: "a-done", Status: state.ApprovalStatusExecuted, Risk: "safe", Summary: "already executed", CreatedAt: now.Add(-time.Hour)},
	}
	rep := CollectProject(baseProject(st, &fakeGH{}), testOptions())

	if len(rep.DecideToday) != 2 {
		t.Fatalf("expected 2 decide-today items, got %d: %+v", len(rep.DecideToday), rep.DecideToday)
	}
	if rep.DecideToday[0].Title != "Approve or reject: force merge PR #2" {
		t.Errorf("danger approval should rank first, got %q", rep.DecideToday[0].Title)
	}
	if got, want := rep.DecideToday[0].URL, "https://github.com/acme/demo/pull/2"; got != want {
		t.Errorf("approval URL = %q, want %q", got, want)
	}
	if got, want := rep.DecideToday[1].URL, "https://github.com/acme/demo/issues/1"; got != want {
		t.Errorf("approval URL = %q, want %q", got, want)
	}
}

func TestRetryExhaustedRequiresOpenPR(t *testing.T) {
	st := state.NewState()
	st.Sessions = map[string]*state.Session{
		"w1": {Status: state.StatusRetryExhausted, PRNumber: 10, IssueNumber: 100, IssueTitle: "open pr", FinishedAt: ptrTime(now.Add(-2 * time.Hour))},
		"w2": {Status: state.StatusRetryExhausted, PRNumber: 11, IssueNumber: 101, IssueTitle: "closed pr", FinishedAt: ptrTime(now.Add(-2 * time.Hour))},
		"w3": {Status: state.StatusRetryExhausted, PRNumber: 0, IssueNumber: 102, IssueTitle: "no pr"},
		"w4": {Status: state.StatusFailed, PRNumber: 12, IssueNumber: 103, IssueTitle: "plain failure"},
	}
	gh := &fakeGH{prs: []github.PR{{Number: 10}}}
	rep := CollectProject(baseProject(st, gh), testOptions())

	if len(rep.DecideToday) != 1 {
		t.Fatalf("expected 1 item, got %d: %+v", len(rep.DecideToday), rep.DecideToday)
	}
	item := rep.DecideToday[0]
	if item.Kind != KindRetryExhaustedPR {
		t.Errorf("kind = %s, want %s", item.Kind, KindRetryExhaustedPR)
	}
	if !strings.Contains(item.Title, "PR #10") || !strings.Contains(item.Title, "issue #100") {
		t.Errorf("title should reference PR and issue: %q", item.Title)
	}
}

func TestRetryExhaustedDedupedByPRAndOverReportsWhenPRListUnavailable(t *testing.T) {
	st := state.NewState()
	st.Sessions = map[string]*state.Session{
		"w1": {Status: state.StatusRetryExhausted, PRNumber: 10, IssueNumber: 100},
		"w2": {Status: state.StatusRetryExhausted, PRNumber: 10, IssueNumber: 100},
	}
	gh := &fakeGH{prsErr: errors.New("gh down")}
	rep := CollectProject(baseProject(st, gh), testOptions())

	count := 0
	for _, item := range rep.DecideToday {
		if item.Kind == KindRetryExhaustedPR {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected 1 deduped retry-exhausted item despite PR list failure, got %d", count)
	}
	if len(rep.Errors) == 0 {
		t.Errorf("expected a collection error recorded for the PR list failure")
	}
}

func TestUnblockedIssueSurfacedOnlyWhenAllBlockersResolved(t *testing.T) {
	st := state.NewState()
	gh := &fakeGH{
		issues: []github.Issue{
			makeIssue(20, "ready to unblock", "Blocked by #5 and depends on #6", "maestro-blocked"),
			makeIssue(21, "still blocked", "Blocked by #7", "maestro-blocked"),
			makeIssue(22, "no parseable deps", "Some text", "maestro-blocked"),
		},
		closed: map[int]bool{5: true},
		merged: map[int]bool{6: true},
	}
	rep := CollectProject(baseProject(st, gh), testOptions())

	var unblocked []Item
	for _, item := range rep.DecideToday {
		if item.Kind == KindUnblockedIssue {
			unblocked = append(unblocked, item)
		}
	}
	if len(unblocked) != 1 {
		t.Fatalf("expected exactly 1 unblocked item, got %d: %+v", len(unblocked), unblocked)
	}
	if !strings.Contains(unblocked[0].Title, "#20") {
		t.Errorf("expected issue #20, got %q", unblocked[0].Title)
	}
	if !strings.Contains(unblocked[0].Detail, "#5") || !strings.Contains(unblocked[0].Detail, "#6") {
		t.Errorf("detail should list resolved deps: %q", unblocked[0].Detail)
	}
}

func TestStaleReviewFindingsRespectAgeThreshold(t *testing.T) {
	st := state.NewState()
	st.ReviewRepairTracks = map[string]state.ReviewRepairTrack{
		"PR#30@abc": {PRNumber: 30, Attempts: 2, Exhausted: true, ExhaustedAt: now.Add(-30 * time.Hour), UnresolvedSummary: "2 P1 findings"},
		"PR#31@def": {PRNumber: 31, Attempts: 1, Exhausted: true, ExhaustedAt: now.Add(-2 * time.Hour)},
		"PR#32@ghi": {PRNumber: 32, Attempts: 1, Exhausted: false},
	}
	gh := &fakeGH{prs: []github.PR{{Number: 30}, {Number: 31}, {Number: 32}}}
	rep := CollectProject(baseProject(st, gh), testOptions())

	var stale []Item
	for _, item := range rep.DecideToday {
		if item.Kind == KindStaleReviewPR {
			stale = append(stale, item)
		}
	}
	if len(stale) != 1 {
		t.Fatalf("expected 1 stale review item, got %d: %+v", len(stale), stale)
	}
	if !strings.Contains(stale[0].Title, "PR #30") {
		t.Errorf("expected PR #30, got %q", stale[0].Title)
	}
	if !strings.Contains(stale[0].Detail, "2 P1 findings") {
		t.Errorf("detail should carry the unresolved summary: %q", stale[0].Detail)
	}
}

func TestDecideTodayKindOrdering(t *testing.T) {
	st := state.NewState()
	st.Approvals = []state.Approval{
		{ID: "a1", Status: state.ApprovalStatusPending, Risk: "safe", Summary: "approve", CreatedAt: now.Add(-time.Hour)},
	}
	st.Sessions = map[string]*state.Session{
		"w1": {Status: state.StatusRetryExhausted, PRNumber: 10, IssueNumber: 100, FinishedAt: ptrTime(now.Add(-time.Hour))},
	}
	st.ReviewRepairTracks = map[string]state.ReviewRepairTrack{
		"PR#30@abc": {PRNumber: 30, Exhausted: true, ExhaustedAt: now.Add(-48 * time.Hour)},
	}
	gh := &fakeGH{
		issues: []github.Issue{makeIssue(20, "unblock me", "Blocked by #5", "maestro-blocked")},
		prs:    []github.PR{{Number: 10}, {Number: 30}},
		closed: map[int]bool{5: true},
	}
	rep := CollectProject(baseProject(st, gh), testOptions())

	wantOrder := []ItemKind{KindPendingApproval, KindRetryExhaustedPR, KindUnblockedIssue, KindStaleReviewPR}
	if len(rep.DecideToday) != len(wantOrder) {
		t.Fatalf("expected %d items, got %d: %+v", len(wantOrder), len(rep.DecideToday), rep.DecideToday)
	}
	for i, kind := range wantOrder {
		if rep.DecideToday[i].Kind != kind {
			t.Errorf("position %d: got %s, want %s", i, rep.DecideToday[i].Kind, kind)
		}
	}
}

func TestPromotableFiltersAndRanking(t *testing.T) {
	st := state.NewState()
	st.Sessions = map[string]*state.Session{
		"w1": {Status: state.StatusRunning, IssueNumber: 45},
	}
	rich := strings.Repeat("Implement the parser for the config file. ", 10) +
		"\n## Acceptance criteria\n1. parses\n## Affected surfaces\n- internal/config"
	gh := &fakeGH{
		issues: []github.Issue{
			makeIssue(40, "already ready", "body", "maestro-ready"),
			makeIssue(41, "an epic", "body", "epic"),
			makeIssue(42, "blocked one", "body", "maestro-blocked"),
			makeIssue(43, "well specified small fix", rich),
			makeIssue(44, "vague idea", ""),
			makeIssue(45, "in progress elsewhere", rich),
			makeIssue(46, "investigate flaky CI", rich),
			makeIssue(47, "has open dependency", "Depends on #99\n"+rich),
		},
	}
	rep := CollectProject(baseProject(st, gh), testOptions())

	if len(rep.Promotable) != 3 {
		t.Fatalf("expected 3 promotable items, got %d: %+v", len(rep.Promotable), rep.Promotable)
	}
	if !strings.Contains(rep.Promotable[0].Title, "#43") {
		t.Errorf("best-specified issue should rank first, got %q", rep.Promotable[0].Title)
	}
	if rep.Promotable[0].Score <= rep.Promotable[len(rep.Promotable)-1].Score {
		t.Errorf("expected strictly ranked scores, got %+v", rep.Promotable)
	}
	for _, item := range rep.Promotable {
		for _, excluded := range []string{"#40", "#41", "#42", "#45", "#47"} {
			if strings.Contains(item.Title, excluded+" ") {
				t.Errorf("issue %s should not be promotable: %+v", excluded, item)
			}
		}
		if item.URL == "" {
			t.Errorf("promotable item should link to the issue: %+v", item)
		}
	}
}

func TestPromotableCapped(t *testing.T) {
	st := state.NewState()
	var issues []github.Issue
	for i := 1; i <= 15; i++ {
		issues = append(issues, makeIssue(i, "task", "a reasonable description of the work to do"))
	}
	gh := &fakeGH{issues: issues}
	opts := testOptions()
	opts.MaxPromotable = 5
	rep := CollectProject(baseProject(st, gh), opts)
	if len(rep.Promotable) != 5 {
		t.Fatalf("expected promotable capped at 5, got %d", len(rep.Promotable))
	}
}

func TestHealthRollup(t *testing.T) {
	st := state.NewState()
	st.Sessions = map[string]*state.Session{
		"w1": {Status: state.StatusDone, Backend: "claude", StartedAt: now.Add(-3 * time.Hour), FinishedAt: ptrTime(now.Add(-2 * time.Hour))},
		"w2": {Status: state.StatusCodeLanded, Backend: "claude", StartedAt: now.Add(-5 * time.Hour), FinishedAt: ptrTime(now.Add(-4 * time.Hour))},
		"w3": {Status: state.StatusFailed, Backend: "codex", StartedAt: now.Add(-6 * time.Hour), FinishedAt: ptrTime(now.Add(-5 * time.Hour))},
		"w4": {Status: state.StatusRunning, Backend: "gemini", StartedAt: now.Add(-time.Hour)},
		// Outside the 24h window entirely:
		"w5": {Status: state.StatusDone, Backend: "claude", StartedAt: now.Add(-50 * time.Hour), FinishedAt: ptrTime(now.Add(-49 * time.Hour))},
	}
	rep := CollectProject(baseProject(st, &fakeGH{}), testOptions())

	h := rep.Health
	if h.Sessions != 4 {
		t.Errorf("sessions = %d, want 4", h.Sessions)
	}
	if h.Merged != 2 {
		t.Errorf("merged = %d, want 2", h.Merged)
	}
	if h.Failed != 1 {
		t.Errorf("failed = %d, want 1", h.Failed)
	}
	if h.Backends["claude"] != 2 || h.Backends["codex"] != 1 || h.Backends["gemini"] != 1 {
		t.Errorf("backend distribution wrong: %+v", h.Backends)
	}
	line := h.Line()
	for _, want := range []string{"4 session(s)", "2 merged", "1 failed", "claude ×2", "codex ×1", "gemini ×1"} {
		if !strings.Contains(line, want) {
			t.Errorf("health line missing %q: %q", want, line)
		}
	}
}

func TestMarkdownReportSectionsAndLinks(t *testing.T) {
	st := state.NewState()
	st.Approvals = []state.Approval{
		{ID: "a1", Status: state.ApprovalStatusPending, Risk: "caution", Summary: "merge PR #9", CreatedAt: now.Add(-time.Hour), Target: &state.SupervisorTarget{PR: 9}},
	}
	gh := &fakeGH{
		issues: []github.Issue{makeIssue(43, "small fix", "Body with acceptance criteria section.\n## Acceptance criteria\n1. works")},
	}
	rep := Collect([]Project{baseProject(st, gh)}, testOptions())
	md := rep.Markdown()

	for _, want := range []string{
		"# Maestro morning digest — 2026-06-12",
		"## 1. Decide today (1)",
		"## 2. Promotable (1)",
		"## 3. Fleet health (24h)",
		"https://github.com/acme/demo/pull/9",
		"https://github.com/acme/demo/issues/43",
		"**[demo]**",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, md)
		}
	}
}

func TestMarkdownEmptyReport(t *testing.T) {
	rep := Collect([]Project{baseProject(state.NewState(), &fakeGH{})}, testOptions())
	md := rep.Markdown()
	if !strings.Contains(md, "Nothing needs an operator decision") {
		t.Errorf("empty decide-today section should say so:\n%s", md)
	}
	if !strings.Contains(md, "No promotable candidates found") {
		t.Errorf("empty promotable section should say so:\n%s", md)
	}
}

func TestNotifySummaryCountsByKind(t *testing.T) {
	st := state.NewState()
	st.Approvals = []state.Approval{
		{ID: "a1", Status: state.ApprovalStatusPending, Risk: "safe", Summary: "one", CreatedAt: now.Add(-time.Hour)},
		{ID: "a2", Status: state.ApprovalStatusPending, Risk: "safe", Summary: "two", CreatedAt: now.Add(-time.Hour)},
	}
	st.Sessions = map[string]*state.Session{
		"w1": {Status: state.StatusRetryExhausted, PRNumber: 10, IssueNumber: 100},
	}
	gh := &fakeGH{prs: []github.PR{{Number: 10}}}
	rep := Collect([]Project{baseProject(st, gh)}, testOptions())

	if got := rep.DecideTodayCount(); got != 3 {
		t.Fatalf("DecideTodayCount = %d, want 3", got)
	}
	msg := rep.NotifySummary("/vault/digest.md")
	for _, want := range []string{"3 decision(s)", "2 approval(s)", "1 retry-exhausted PR(s)", "/vault/digest.md", "2026-06-12"} {
		if !strings.Contains(msg, want) {
			t.Errorf("summary missing %q: %q", want, msg)
		}
	}
}

func TestCollectErrorsRecordedOnIssueListFailure(t *testing.T) {
	gh := &fakeGH{issuesErr: errors.New("api rate limited")}
	rep := CollectProject(baseProject(state.NewState(), gh), testOptions())
	if len(rep.Errors) == 0 {
		t.Fatalf("expected collection error recorded")
	}
	md := Collect([]Project{baseProject(state.NewState(), gh)}, testOptions()).Markdown()
	if !strings.Contains(md, "Collection warnings") {
		t.Errorf("markdown should surface collection warnings:\n%s", md)
	}
}

func TestResolverLookupErrorMeansUnresolved(t *testing.T) {
	st := state.NewState()
	gh := &fakeGH{
		issues:    []github.Issue{makeIssue(20, "blocked", "Blocked by #5", "maestro-blocked")},
		lookupErr: errors.New("boom"),
	}
	rep := CollectProject(baseProject(st, gh), testOptions())
	for _, item := range rep.DecideToday {
		if item.Kind == KindUnblockedIssue {
			t.Errorf("lookup failure must not surface an unblock item: %+v", item)
		}
	}
	if len(rep.Errors) == 0 {
		t.Errorf("expected resolver errors recorded")
	}
}
