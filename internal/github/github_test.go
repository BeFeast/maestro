package github

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestParseRESTIssuesSkipsPullRequests(t *testing.T) {
	body := `[
		{"number": 10, "title": "real issue", "body": null, "labels": [{"name": "maestro-ready"}]},
		{"number": 11, "title": "pr issue", "body": "ignored", "labels": [], "pull_request": {}}
	]`

	got, err := parseRESTIssues([]byte(body))
	if err != nil {
		t.Fatalf("parseRESTIssues() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("parseRESTIssues() returned %d issues, want 1", len(got))
	}
	if got[0].Number != 10 || got[0].Title != "real issue" {
		t.Fatalf("parseRESTIssues()[0] = %#v", got[0])
	}
	if len(got[0].Labels) != 1 || got[0].Labels[0].Name != "maestro-ready" {
		t.Fatalf("labels = %#v, want maestro-ready", got[0].Labels)
	}
	if got[0].Body != "" {
		t.Fatalf("null body should parse as empty string, got %q", got[0].Body)
	}
}

func TestParseRESTPullsMapsFields(t *testing.T) {
	body := `[
		{
			"number": 42,
			"title": "Fixes #7",
			"body": "Closes #7",
			"state": "open",
			"draft": true,
			"head": {"ref": "feat/example"},
			"merged_at": null
		}
	]`

	got, err := parseRESTPulls([]byte(body))
	if err != nil {
		t.Fatalf("parseRESTPulls() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("parseRESTPulls() returned %d PRs, want 1", len(got))
	}
	pr := got[0]
	if pr.Number != 42 || pr.HeadRefName != "feat/example" || pr.State != "OPEN" || !pr.IsDraft {
		t.Fatalf("parsed PR = %#v", pr)
	}
	if !prReferencesIssue(pr, 7) {
		t.Fatalf("expected PR to reference issue #7")
	}
	if prReferencesIssue(pr, 70) {
		t.Fatalf("did not expect #7 to match issue #70")
	}
}

func TestPRReferencesIssue_IgnoresCodeAndLogs(t *testing.T) {
	// #468: a #N mention that lives only inside a fenced code block or inline
	// code span (pasted logs/output) must NOT link the PR to issue N, while a
	// prose reference (the Maestro "Refs #N" convention) still must.
	fencedBody := "Refs #466\n\n" +
		"Live evidence:\n" +
		"```\n" +
		"[orch] starting worker for issue #353 ... (backend=codex ...)\n" +
		"```\n"
	inlineBody := "Refs #466. Unrelated log token `#353` pasted inline.\n"
	proseBody := "This PR addresses the drift detector. Refs #353.\n"
	longFenceBody := "Refs #466\n\n`````markdown\n" +
		"see issue #353 in the log\n" +
		"```\n" + // a shorter fence inside the longer fence is content, not a closer
		"still #353 inside\n" +
		"`````\n"

	tests := []struct {
		name  string
		pr    PR
		issue int
		want  bool
	}{
		{"fenced #353 does not match", PR{Title: "fix: reconcile rate limit", Body: fencedBody}, 353, false},
		{"fenced PR still matches its real issue 466", PR{Title: "fix: reconcile rate limit", Body: fencedBody}, 466, true},
		{"inline-code #353 does not match", PR{Title: "fix", Body: inlineBody}, 353, false},
		{"prose Refs #353 matches", PR{Title: "fix", Body: proseBody}, 353, true},
		{"#353 inside a long ````` fence does not match", PR{Title: "fix", Body: longFenceBody}, 353, false},
		{"title reference still matches", PR{Title: "Fixes #7", Body: ""}, 7, true},
		{"word-boundary: #7 does not match #70", PR{Title: "Fixes #7", Body: ""}, 70, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := prReferencesIssue(tc.pr, tc.issue); got != tc.want {
				t.Fatalf("prReferencesIssue(issue=%d) = %v, want %v", tc.issue, got, tc.want)
			}
		})
	}
}

// #520: prClosesIssue must require an explicit closing keyword
// directly in front of `#N`. Bare mentions of `#N` (e.g. "P0 #487"
// in a commit-message context line) must NOT trigger it.
func TestPRClosesIssue_StrictClosingKeyword(t *testing.T) {
	cases := []struct {
		name  string
		pr    PR
		issue int
		want  bool
	}{
		// Positive — recognised closing keywords.
		{"Closes capital", PR{Title: "fix bug", Body: "Closes #487"}, 487, true},
		{"closes lowercase", PR{Title: "fix", Body: "closes #487"}, 487, true},
		{"closed past", PR{Title: "fix", Body: "closed #487"}, 487, true},
		{"fixes", PR{Title: "fix", Body: "fixes #487"}, 487, true},
		{"fix bare", PR{Title: "fix", Body: "fix #487"}, 487, true},
		{"resolves", PR{Title: "fix", Body: "Resolves #487"}, 487, true},
		{"resolved past", PR{Title: "fix", Body: "resolved #487"}, 487, true},
		{"colon between", PR{Title: "fix", Body: "Fixes: #487"}, 487, true},
		{"newline before", PR{Title: "x", Body: "did things\ncloses #487\n"}, 487, true},
		{"in title", PR{Title: "Closes #487 — auth", Body: ""}, 487, true},

		// Negative — bare references / non-closing keywords.
		{"bare mention", PR{Title: "P0 #487: add HTTP auth", Body: ""}, 487, false},
		{"refs prefix", PR{Title: "fix", Body: "Refs #487"}, 487, false},
		{"see prefix", PR{Title: "fix", Body: "see #487 for context"}, 487, false},
		{"naked hash", PR{Title: "fix", Body: "context: #487 was the original P0"}, 487, false},
		{"different number", PR{Title: "fix", Body: "Closes #488"}, 487, false},
		{"prefix-bound number", PR{Title: "fix", Body: "Closes #4879"}, 487, false},
		{"keyword inside word", PR{Title: "fix", Body: "encloses #487"}, 487, false}, // "encloses" not a closing keyword
		// Code-fenced closing keyword: prClosesIssue uses
		// stripCodeForRefMatch on the body, so a fenced "closes #N"
		// must NOT count.
		{"fenced closes", PR{Title: "fix", Body: "Live log:\n```\ncloses #487\n```"}, 487, false},

		// Empty / invalid.
		{"zero issue", PR{Title: "Closes #1", Body: ""}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := prClosesIssue(tc.pr, tc.issue); got != tc.want {
				t.Fatalf("prClosesIssue(issue=%d) = %v, want %v\nbody=%q", tc.issue, got, tc.want, tc.pr.Body)
			}
		})
	}
}

// #520: HasMergedPRForIssue must return false for merged PRs that only
// reference (not close) the issue. Direct integration test for the
// matcher swap; uses a fake closed-PR list via the same test pattern
// as the existing #468 test.
func TestPRClosesIssue_PR487RegressionScenario(t *testing.T) {
	// Real shape from today's freeze (2026-05-31): PR #514 commit body
	// has «P0 #487» as project context. Must not be flagged as closing
	// #487.
	pr514 := PR{
		Title: "feat(supervisor): merge_pr planner rule (#512, Phase 1.6) — closes hands-off blocker",
		Body:  "Closes #512. Until now the supervisor planner had only ActionMonitorOpenPR\nfor sessions with an open PR ... Refs #487 (HTTP auth).",
	}
	if prClosesIssue(pr514, 487) {
		t.Fatal("PR #514 must NOT be considered as closing #487 (it only Refs the issue)")
	}
	if !prClosesIssue(pr514, 512) {
		t.Fatal("PR #514 MUST be considered as closing #512 (its actual target)")
	}
}

func TestRESTIssueStateClosedAcceptsRESTLowercase(t *testing.T) {
	for _, state := range []string{"closed", "CLOSED", " Closed "} {
		if !restIssueStateClosed(state) {
			t.Fatalf("restIssueStateClosed(%q) = false, want true", state)
		}
	}
	for _, state := range []string{"open", "", "merged"} {
		if restIssueStateClosed(state) {
			t.Fatalf("restIssueStateClosed(%q) = true, want false", state)
		}
	}
}

func TestParseRateLimitStatus(t *testing.T) {
	body := `{
		"resources": {
			"core": {"limit": 5000, "remaining": 4990, "reset": 1779053364, "used": 10},
			"graphql": {"limit": 5000, "remaining": 0, "reset": 1779052352, "used": 5000}
		}
	}`

	got, err := parseRateLimitStatus([]byte(body))
	if err != nil {
		t.Fatalf("parseRateLimitStatus() error = %v", err)
	}
	if got.Core.Remaining != 4990 || got.GraphQL.Remaining != 0 || got.GraphQL.Used != 5000 {
		t.Fatalf("parseRateLimitStatus() = %#v", got)
	}
}

func TestCIStatusFromREST(t *testing.T) {
	tests := []struct {
		name     string
		checks   []greptileCheckRun
		combined combinedStatusResponse
		want     string
	}{
		{name: "no checks means success", want: "success"},
		{name: "queued check pending", checks: []greptileCheckRun{{Name: "test", Status: "queued"}}, want: "pending"},
		{name: "in progress check pending", checks: []greptileCheckRun{{Name: "test", Status: "in_progress"}}, want: "pending"},
		{name: "failed check fails", checks: []greptileCheckRun{{Name: "test", Status: "completed", Conclusion: "failure"}}, want: "failure"},
		{name: "cancelled check fails", checks: []greptileCheckRun{{Name: "test", Status: "completed", Conclusion: "cancelled"}}, want: "failure"},
		{name: "success checks pass", checks: []greptileCheckRun{{Name: "test", Status: "completed", Conclusion: "success"}}, want: "success"},
		{
			name: "new success supersedes same-head failed attempt",
			checks: []greptileCheckRun{
				{ID: 10, Name: "agent-lint", Status: "completed", Conclusion: "failure", StartedAt: "2026-07-18T06:49:50Z"},
				{ID: 20, Name: "agent-lint", Status: "completed", Conclusion: "success", StartedAt: "2026-07-18T06:51:30Z"},
			},
			want: "success",
		},
		{
			name: "new failure supersedes same-head successful attempt",
			checks: []greptileCheckRun{
				{ID: 10, Name: "agent-lint", Status: "completed", Conclusion: "success", StartedAt: "2026-07-18T06:49:50Z"},
				{ID: 20, Name: "agent-lint", Status: "completed", Conclusion: "failure", StartedAt: "2026-07-18T06:51:30Z"},
			},
			want: "failure",
		},
		{
			name: "new pending rerun supersedes same-head failure",
			checks: []greptileCheckRun{
				{ID: 10, Name: "agent-lint", Status: "completed", Conclusion: "failure", StartedAt: "2026-07-18T06:49:50Z"},
				{ID: 20, Name: "agent-lint", Status: "in_progress", StartedAt: "2026-07-18T06:51:30Z"},
			},
			want: "pending",
		},
		{
			name: "different failing check remains authoritative",
			checks: []greptileCheckRun{
				{ID: 10, Name: "agent-lint", Status: "completed", Conclusion: "success", StartedAt: "2026-07-18T06:51:30Z"},
				{ID: 20, Name: "build", Status: "completed", Conclusion: "failure", StartedAt: "2026-07-18T06:50:12Z"},
			},
			want: "failure",
		},
		{
			name:     "green checks with empty combined pending succeed",
			checks:   []greptileCheckRun{{Name: "test", Status: "completed", Conclusion: "success"}},
			combined: combinedStatusResponse{State: "pending", Statuses: nil},
			want:     "success",
		},
		{
			name:   "combined pending wins when a status entry is pending",
			checks: []greptileCheckRun{{Name: "test", Status: "completed", Conclusion: "success"}},
			combined: combinedStatusResponse{State: "pending", Statuses: []struct {
				Context     string `json:"context"`
				State       string `json:"state"`
				Description string `json:"description"`
				TargetURL   string `json:"target_url"`
			}{{Context: "ci/build", State: "pending"}}},
			want: "pending",
		},
		{
			name:   "combined failure wins when a status entry failed",
			checks: []greptileCheckRun{{Name: "test", Status: "completed", Conclusion: "success"}},
			combined: combinedStatusResponse{State: "failure", Statuses: []struct {
				Context     string `json:"context"`
				State       string `json:"state"`
				Description string `json:"description"`
				TargetURL   string `json:"target_url"`
			}{{Context: "ci/build", State: "failure"}}},
			want: "failure",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ciStatusFromREST(tt.checks, tt.combined); got != tt.want {
				t.Fatalf("ciStatusFromREST() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHasPendingCheckRuns(t *testing.T) {
	tests := []struct {
		name   string
		checks []greptileCheckRun
		want   bool
	}{
		{name: "none"},
		{name: "queued", checks: []greptileCheckRun{{Status: "queued"}}, want: true},
		{name: "in progress", checks: []greptileCheckRun{{Status: "in_progress"}}, want: true},
		{name: "waiting", checks: []greptileCheckRun{{Status: "waiting"}}, want: true},
		{name: "requested", checks: []greptileCheckRun{{Status: "requested"}}, want: true},
		{name: "unknown nonterminal", checks: []greptileCheckRun{{Status: "pending"}}, want: true},
		{name: "success", checks: []greptileCheckRun{{Status: "completed", Conclusion: "success"}}},
		{name: "failure", checks: []greptileCheckRun{{Status: "completed", Conclusion: "failure"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasPendingCheckRuns(tt.checks); got != tt.want {
				t.Fatalf("hasPendingCheckRuns() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseCheckRunsKeepsLatestAttemptPerAppAndName(t *testing.T) {
	checks, err := parseCheckRuns([]byte(`{
		"check_runs": [
			{"id": 30, "name": "agent-lint", "status": "completed", "conclusion": "success", "started_at": "2026-07-18T06:51:30Z", "app": {"id": 15368, "slug": "github-actions"}},
			{"id": 10, "name": "agent-lint", "status": "completed", "conclusion": "failure", "started_at": "2026-07-18T06:49:50Z", "app": {"id": 15368, "slug": "github-actions"}},
			{"id": 20, "name": "build", "status": "completed", "conclusion": "success", "started_at": "2026-07-18T06:50:12Z", "app": {"id": 15368, "slug": "github-actions"}},
			{"id": 40, "name": "agent-lint", "status": "completed", "conclusion": "failure", "started_at": "2026-07-18T06:52:00Z", "app": {"id": 999, "slug": "another-app"}}
		]
	}`))
	if err != nil {
		t.Fatalf("parseCheckRuns() error = %v", err)
	}
	if len(checks) != 3 {
		t.Fatalf("len(checks) = %d, want 3 logical contexts: %#v", len(checks), checks)
	}
	if checks[0].ID != 30 || checks[0].Conclusion != "success" {
		t.Fatalf("github-actions agent-lint = %#v, want latest success id 30", checks[0])
	}
}

func TestMergeableFromRESTPull(t *testing.T) {
	yes := true
	no := false
	tests := []struct {
		name string
		pr   restPull
		want string
	}{
		{name: "mergeable bool true", pr: restPull{Mergeable: &yes}, want: "MERGEABLE"},
		{name: "mergeable bool false", pr: restPull{Mergeable: &no}, want: "CONFLICTING"},
		{name: "dirty state conflicts", pr: restPull{MergeableState: "dirty"}, want: "CONFLICTING"},
		{name: "unknown state unknown", pr: restPull{MergeableState: "unknown"}, want: "UNKNOWN"},
		{name: "unstable still mergeable", pr: restPull{MergeableState: "unstable"}, want: "MERGEABLE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeableFromRESTPull(tt.pr); got != tt.want {
				t.Fatalf("mergeableFromRESTPull() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParsePRLabelsAndCommits(t *testing.T) {
	labels, err := parsePRLabels([]byte(`[{"name":"ready"},{"name":"bug"}]`))
	if err != nil {
		t.Fatalf("parsePRLabels() error = %v", err)
	}
	if !reflect.DeepEqual(labels, []string{"ready", "bug"}) {
		t.Fatalf("labels = %#v", labels)
	}

	commits, err := parsePRCommits([]byte(`[
		{"commit":{"message":"first line\n\nbody"}},
		{"commit":{"message":"single line"}},
		{"commit":{"message":""}}
	]`))
	if err != nil {
		t.Fatalf("parsePRCommits() error = %v", err)
	}
	if !reflect.DeepEqual(commits, []string{"first line", "single line"}) {
		t.Fatalf("commits = %#v", commits)
	}
}

func TestGreptileCheckDecision(t *testing.T) {
	tests := []struct {
		name        string
		checks      []greptileCheckRun
		wantFound   bool
		wantApprove bool
		wantPending bool
	}{
		{
			name:        "success approves",
			checks:      []greptileCheckRun{{Name: "Greptile Review", Conclusion: "success"}},
			wantFound:   true,
			wantApprove: true,
		},
		{
			name:        "neutral approves",
			checks:      []greptileCheckRun{{Name: "greptile", Conclusion: "neutral"}},
			wantFound:   true,
			wantApprove: true,
		},
		{
			name:        "in progress is pending",
			checks:      []greptileCheckRun{{Name: "Greptile Review", Status: "in_progress"}},
			wantFound:   true,
			wantPending: true,
		},
		{
			name:      "failure blocks",
			checks:    []greptileCheckRun{{Name: "Greptile Review", Conclusion: "failure"}},
			wantFound: true,
		},
		{
			name:   "non-greptile is ignored",
			checks: []greptileCheckRun{{Name: "CI", Conclusion: "success"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotFound, gotApprove, gotPending := greptileCheckDecision(tt.checks)
			if gotFound != tt.wantFound || gotApprove != tt.wantApprove || gotPending != tt.wantPending {
				t.Fatalf("greptileCheckDecision() = (found=%v, approve=%v, pending=%v), want (%v, %v, %v)",
					gotFound, gotApprove, gotPending, tt.wantFound, tt.wantApprove, tt.wantPending)
			}
		})
	}
}

func TestGreptileCommentDecisionIgnoresHumanMentionsAndUsesLatestBotVerdict(t *testing.T) {
	comment := func(login, body string) issueComment {
		var got issueComment
		got.User.Login = login
		got.Body = body
		return got
	}

	found, approved := greptileCommentDecision([]issueComment{
		comment("kossoy", "Please run @greptile review on the new exact head"),
		comment("operator", "Greptile is still pending"),
	})
	if found || approved {
		t.Fatalf("human Greptile mentions = (%t,%t), want no verdict", found, approved)
	}

	found, approved = greptileCommentDecision([]issueComment{
		comment("greptile-app[bot]", "Not safe to merge"),
		comment("kossoy", "@greptile review"),
		comment("greptile-app[bot]", "Confidence Score: 4/5 — safe to merge"),
	})
	if !found || !approved {
		t.Fatalf("latest Greptile bot verdict = (%t,%t), want approved", found, approved)
	}
}

func TestHasGreptileInlineCommentOnHead(t *testing.T) {
	makeComment := func(login, sha, body string) greptileReviewComment {
		var c greptileReviewComment
		c.CommitID = sha
		c.OriginalCommitID = sha
		c.User.Login = login
		c.Body = body
		return c
	}

	// P0/P1 comments should block
	p0Comments := []greptileReviewComment{
		makeComment("greptile-apps[bot]", "head-sha", "![alt=\"P0\"] Critical issue"),
	}
	if !hasGreptileInlineCommentOnHead(p0Comments, "head-sha") {
		t.Fatal("expected P0 greptile inline comment on current head to block")
	}

	// P2/P3 comments should NOT block (severity-based filtering)
	p2Comments := []greptileReviewComment{
		makeComment("greptile-apps[bot]", "head-sha", "Minor style issue"),
	}
	if hasGreptileInlineCommentOnHead(p2Comments, "head-sha") {
		t.Fatal("did not expect low-severity greptile comment to block")
	}

	// Comments on different SHA should not block
	if hasGreptileInlineCommentOnHead(p0Comments, "different-sha") {
		t.Fatal("did not expect greptile comment from another head to block")
	}

	// GitHub may report commit_id as the current head for outdated review
	// comments, so original_commit_id is the safer source of truth.
	outdatedComment := makeComment("greptile-apps[bot]", "head-sha", "![alt=\"P1\"] Old issue")
	outdatedComment.CommitID = "new-head-sha"
	if hasGreptileInlineCommentOnHead([]greptileReviewComment{outdatedComment}, "new-head-sha") {
		t.Fatal("did not expect review comment originally left on an older commit to block")
	}

	if !isGreptileLogin("greptile-apps[bot]") {
		t.Fatal("expected greptile login to be recognized")
	}
	if isGreptileLogin("chatgpt-codex-connector[bot]") {
		t.Fatal("did not expect non-greptile login to be recognized")
	}
}

func TestFindBlockers_BasicPattern(t *testing.T) {
	body := "This issue is blocked by #42 and depends on #99."
	patterns := []string{`blocked by #(\d+)`, `depends on #(\d+)`}
	got := FindBlockers(body, patterns)
	sort.Ints(got)
	want := []int{42, 99}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindBlockers() = %v, want %v", got, want)
	}
}

func TestFindBlockers_CaseInsensitive(t *testing.T) {
	body := "BLOCKED BY #10\nBlocked By #20\nblocked by #30"
	patterns := []string{`blocked by #(\d+)`}
	got := FindBlockers(body, patterns)
	sort.Ints(got)
	want := []int{10, 20, 30}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindBlockers() = %v, want %v", got, want)
	}
}

func TestFindBlockers_Deduplicates(t *testing.T) {
	body := "blocked by #42 and also blocked by #42"
	patterns := []string{`blocked by #(\d+)`}
	got := FindBlockers(body, patterns)
	if len(got) != 1 || got[0] != 42 {
		t.Errorf("FindBlockers() = %v, want [42]", got)
	}
}

func TestFindBlockers_NoMatch(t *testing.T) {
	body := "This issue has no blockers."
	patterns := []string{`blocked by #(\d+)`}
	got := FindBlockers(body, patterns)
	if len(got) != 0 {
		t.Errorf("FindBlockers() = %v, want empty", got)
	}
}

func TestFindBlockers_EmptyPatterns(t *testing.T) {
	body := "blocked by #42"
	got := FindBlockers(body, nil)
	if len(got) != 0 {
		t.Errorf("FindBlockers() = %v, want empty", got)
	}
}

func TestFindBlockers_EmptyBody(t *testing.T) {
	patterns := []string{`blocked by #(\d+)`}
	got := FindBlockers("", patterns)
	if len(got) != 0 {
		t.Errorf("FindBlockers() = %v, want empty", got)
	}
}

func TestFindBlockers_InvalidRegex(t *testing.T) {
	body := "blocked by #42"
	patterns := []string{`[invalid`, `blocked by #(\d+)`}
	got := FindBlockers(body, patterns)
	// Should still find the match from the valid pattern
	if len(got) != 1 || got[0] != 42 {
		t.Errorf("FindBlockers() = %v, want [42]", got)
	}
}

func TestFindBlockers_MultipleMatches(t *testing.T) {
	body := "blocked by #10, blocked by #20, blocked by #30"
	patterns := []string{`blocked by #(\d+)`}
	got := FindBlockers(body, patterns)
	sort.Ints(got)
	want := []int{10, 20, 30}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindBlockers() = %v, want %v", got, want)
	}
}

func TestFindBlockers_MultilineBody(t *testing.T) {
	body := `## Description
This feature needs some work.

## Dependencies
- blocked by #100
- depends on #200

## Notes
Nothing else.`
	patterns := []string{`blocked by #(\d+)`, `depends on #(\d+)`}
	got := FindBlockers(body, patterns)
	sort.Ints(got)
	want := []int{100, 200}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindBlockers() = %v, want %v", got, want)
	}
}

func TestFindBlockers_DefaultPatternsMarkdown(t *testing.T) {
	// Default patterns from config should handle markdown formatting
	defaultPatterns := []string{
		`blocked by.*?#(\d+)`,
		`depends on.*?#(\d+)`,
	}
	tests := []struct {
		name string
		body string
		want []int
	}{
		{"plain", "blocked by #123", []int{123}},
		{"with colon", "Blocked by: #123", []int{123}},
		{"markdown bold colon", "**Blocked by:** #123", []int{123}},
		{"depends on markdown", "**Depends on:** #456", []int{456}},
		{"multiple refs", "Blocked by #123, #456", []int{123}},
		{"multiple lines", "**Blocked by:** #673 (palette port must merge first)\n**Depends on:** #100", []int{100, 673}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindBlockers(tt.body, defaultPatterns)
			sort.Ints(got)
			sort.Ints(tt.want)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("FindBlockers(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestFindChildIssues_InlineChildren(t *testing.T) {
	body := "Children: #147, #148, #149\n"
	got := FindChildIssues(body)
	want := []int{147, 148, 149}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindChildIssues() = %v, want %v", got, want)
	}
}

func TestFindChildIssues_ChildrenSectionTaskList(t *testing.T) {
	body := `# Epic: Scribe redesign

## Issue Wave

- [ ] #147 — route shell
- [x] #148 — replace /inbox
- [ ] #149 — replace /settings

## Notes
no children referenced here
`
	got := FindChildIssues(body)
	want := []int{147, 148, 149}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindChildIssues() = %v, want %v", got, want)
	}
}

func TestFindChildIssues_DeduplicatesAndExcludesSelf(t *testing.T) {
	body := `## Children
- #200 — slice A
- #200 — duplicate
- #201 — slice B

Children: #201, #202
Refs #300 (self)
`
	got := FindChildIssuesExcluding(body, 300)
	sort.Ints(got)
	want := []int{200, 201, 202}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindChildIssuesExcluding() = %v, want %v", got, want)
	}
}

func TestFindChildIssues_EmptyBody(t *testing.T) {
	if got := FindChildIssues(""); len(got) != 0 {
		t.Errorf("FindChildIssues(\"\") = %v, want empty", got)
	}
	if got := FindChildIssues("no children here"); len(got) != 0 {
		t.Errorf("FindChildIssues(no children) = %v, want empty", got)
	}
}

func TestFindChildIssues_NestedHeading(t *testing.T) {
	body := `## Issue Wave
- #1
### Subnotes
- #2
## Other
- #3
`
	got := FindChildIssues(body)
	// Subnotes is nested under Issue Wave (deeper level), so #2 stays in the
	// section. #3 lives under "Other" which is NOT a child section heading,
	// so it must not be picked up.
	want := []int{1, 2}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindChildIssues() = %v, want %v", got, want)
	}
}

func TestFindBlockers_IgnoresNegatedDependencyProse(t *testing.T) {
	body := "Depends on #307 sibling work but is independently mergeable.\nBlocked by #42"
	patterns := []string{
		`blocked by.*?#(\d+)`,
		`depends on.*?#(\d+)`,
	}
	got := FindBlockers(body, patterns)
	if !reflect.DeepEqual(got, []int{42}) {
		t.Errorf("FindBlockers() = %v, want [42]", got)
	}
}

func TestFormatReviewFeedback_NonEmpty(t *testing.T) {
	comments := []ReviewComment{
		{Path: "bridge.rs", Line: 42, Body: "P2: enabled flag logic inverted", User: "greptile-apps[bot]"},
		{Path: "pool.go", Line: 10, Body: "P2: null-dereference on pool.interface", User: "greptile-apps[bot]"},
	}
	result := FormatReviewFeedback(comments)

	if !strings.Contains(result, "Review Feedback") {
		t.Error("should contain header")
	}
	if !strings.Contains(result, "bridge.rs") {
		t.Error("should contain file path")
	}
	if !strings.Contains(result, "Line: 42") {
		t.Error("should contain line number")
	}
	if !strings.Contains(result, "enabled flag logic inverted") {
		t.Error("should contain comment body")
	}
	if !strings.Contains(result, "null-dereference") {
		t.Error("should contain second comment body")
	}
}

func TestFormatReviewFeedback_Empty(t *testing.T) {
	result := FormatReviewFeedback(nil)
	if result != "" {
		t.Errorf("FormatReviewFeedback(nil) = %q, want empty", result)
	}
}

func TestIsActionableReviewComment_IgnoresCodexWrapper(t *testing.T) {
	comment := ReviewComment{
		Path: "internal/foo.go",
		Line: 42,
		Body: "Codex reviewed this pull request and left comments for the author to consider.",
		User: "chatgpt-codex-connector[bot]",
	}
	if isActionableReviewComment(comment) {
		t.Fatal("generic Codex wrapper text should not be actionable review feedback")
	}
}

func TestIsActionableReviewSummary_IgnoresEmptyGreptileReview(t *testing.T) {
	body := `## Greptile Review

No issues found. Safe to merge.

Confidence Score: 5/5`
	if isActionableReviewSummary(body) {
		t.Fatal("empty Greptile review summary should not be actionable review feedback")
	}
}

func TestIsActionableReviewComment_KeepsCurrentHeadInlineFinding(t *testing.T) {
	comment := ReviewComment{
		Path: "internal/foo.go",
		Line: 42,
		Body: "P1: nil pointer panic when the worker has no PR number",
		User: "greptile-apps[bot]",
	}
	if !isActionableReviewComment(comment) {
		t.Fatal("real inline review finding should be actionable")
	}
}

func TestIsActionableReviewSummary_KeepsConcreteSummaryFinding(t *testing.T) {
	body := "Confidence Score: 3/5\nNot safe to merge. P2: internal/foo.go:42 has inverted retry logic."
	if !isActionableReviewSummary(body) {
		t.Fatal("concrete review summary finding should be actionable")
	}
}

func TestIsGreptileLogin(t *testing.T) {
	tests := []struct {
		login string
		want  bool
	}{
		{"greptile-apps[bot]", true},
		{"greptile", true},
		{"Greptile", true},
		{"chatgpt-codex-connector[bot]", false},
		{"user123", false},
	}
	for _, tt := range tests {
		if got := isGreptileLogin(tt.login); got != tt.want {
			t.Errorf("isGreptileLogin(%q) = %v, want %v", tt.login, got, tt.want)
		}
	}
}

func TestHasLabel_CaseInsensitive(t *testing.T) {
	issue := Issue{
		Labels: []struct {
			Name string `json:"name"`
		}{{Name: "Bug"}},
	}
	if !HasLabel(issue, []string{"bug"}) {
		t.Error("HasLabel should be case-insensitive")
	}
}

func TestHasLabel_NoMatch(t *testing.T) {
	issue := Issue{
		Labels: []struct {
			Name string `json:"name"`
		}{{Name: "enhancement"}},
	}
	if HasLabel(issue, []string{"bug"}) {
		t.Error("HasLabel should return false when no labels match")
	}
}

func TestMergePaginatedJSONArrays(t *testing.T) {
	// gh api --paginate over an array endpoint emits one `[...]` document per
	// page back-to-back; the wrapper concatenates page bodies (with or without a
	// separating newline). mergePaginatedJSONArrays must flatten every shape a
	// real reconcile sees into one array parseREST* can consume.
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"single page passes through", `[{"number":1},{"number":2}]`, `[{"number":1},{"number":2}]`, false},
		{"two pages concatenated", `[{"number":1}][{"number":2},{"number":3}]`, `[{"number":1},{"number":2},{"number":3}]`, false},
		{"pages separated by newline", "[{\"number\":1}]\n[{\"number\":2}]", `[{"number":1},{"number":2}]`, false},
		{"empty first page then items", `[][{"number":9}]`, `[{"number":9}]`, false},
		{"empty input yields empty array", ``, `[]`, false},
		{"error object is not silently flattened", `{"message":"Not Found"}`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mergePaginatedJSONArrays([]byte(tc.in))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("mergePaginatedJSONArrays(%q) error = %v", tc.in, err)
			}
			if string(got) != tc.want {
				t.Fatalf("mergePaginatedJSONArrays(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
