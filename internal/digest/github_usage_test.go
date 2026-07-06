package digest

import (
	"strings"
	"testing"
)

func TestGitHubUsageLine_MirrorNotEnabled(t *testing.T) {
	u := GitHubUsage{Requests: 100, Billed: 60, NotModified: 40, RateLimited: 2}
	line := u.Line()
	if !strings.Contains(line, "100 REST exchange(s)") {
		t.Fatalf("line missing request count: %q", line)
	}
	if !strings.Contains(line, "not enabled") {
		t.Fatalf("line should say mirror-first not enabled when no mirror reads: %q", line)
	}
}

func TestGitHubUsageLine_WithMirrorReads(t *testing.T) {
	u := GitHubUsage{Requests: 20, Billed: 20, MirrorHits: 80, APIFallbacks: 20}
	line := u.Line()
	if !strings.Contains(line, "80 served locally") {
		t.Fatalf("line missing mirror hits: %q", line)
	}
	if !strings.Contains(line, "20 fell back to API") {
		t.Fatalf("line missing fallbacks: %q", line)
	}
	if !strings.Contains(line, "80% hit") {
		t.Fatalf("line missing hit rate: %q", line)
	}
}

func TestReportMarkdownIncludesGitHubReads(t *testing.T) {
	r := &Report{GitHub: GitHubUsage{Requests: 5, Billed: 5}}
	md := r.Markdown()
	if !strings.Contains(md, "GitHub reads:") {
		t.Fatalf("markdown missing GitHub reads line:\n%s", md)
	}
}

func TestReconcileSummaryLine(t *testing.T) {
	if got := (ReconcileSummary{}).Line(); !strings.Contains(got, "not enabled") {
		t.Fatalf("empty reconcile summary should say not enabled: %q", got)
	}
	s := ReconcileSummary{Repos: 2, Runs: 8, Failures: 1, DriftRepairs: 3}
	got := s.Line()
	if !strings.Contains(got, "3 drift repair(s)") {
		t.Fatalf("line missing drift repairs: %q", got)
	}
	if !strings.Contains(got, "2 repo(s)") {
		t.Fatalf("line missing repo count: %q", got)
	}
}

func TestReportMarkdownIncludesReconcile(t *testing.T) {
	r := &Report{Reconcile: ReconcileSummary{Repos: 1, Runs: 1, DriftRepairs: 2}}
	md := r.Markdown()
	if !strings.Contains(md, "Mirror reconcile:") {
		t.Fatalf("markdown missing reconcile line:\n%s", md)
	}
	if !strings.Contains(md, "2 drift repair(s)") {
		t.Fatalf("markdown missing drift-repair count:\n%s", md)
	}
}
