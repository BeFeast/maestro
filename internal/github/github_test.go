package github

import (
	"reflect"
	"sort"
	"testing"
)

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

func TestHasGreptileInlineCommentOnHead(t *testing.T) {
	makeComment := func(login, sha, body string) greptileReviewComment {
		var c greptileReviewComment
		c.CommitID = sha
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
