package github

import (
	"strings"
	"testing"
)

func TestIsFailingConclusion(t *testing.T) {
	failing := []string{"failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale", "FAILURE", "  failure  "}
	for _, c := range failing {
		if !isFailingConclusion(c) {
			t.Errorf("isFailingConclusion(%q) = false, want true", c)
		}
	}
	notFailing := []string{"success", "neutral", "skipped", "", "pending"}
	for _, c := range notFailing {
		if isFailingConclusion(c) {
			t.Errorf("isFailingConclusion(%q) = true, want false", c)
		}
	}
}

func TestCheckRunOutputErrorLines(t *testing.T) {
	tests := []struct {
		name string
		ck   greptileCheckRun
		want string
	}{
		{
			name: "extracts workflow-command error lines and strips the prefix",
			ck: func() greptileCheckRun {
				var ck greptileCheckRun
				ck.Output.Text = "Running agent-lint\n::error::agent-lint: possible secret detected (ghp) in added diff lines.\nsome noise\n##[error]agent-lint: 1 check(s) failed"
				return ck
			}(),
			want: "agent-lint: possible secret detected (ghp) in added diff lines.\nagent-lint: 1 check(s) failed",
		},
		{
			name: "strips ::error file=...:: annotation prefix",
			ck: func() greptileCheckRun {
				var ck greptileCheckRun
				ck.Output.Text = "::error file=x.go,line=3::boom"
				return ck
			}(),
			want: "boom",
		},
		{
			name: "falls back to summary when no error line matches",
			ck: func() greptileCheckRun {
				var ck greptileCheckRun
				ck.Output.Summary = "agent-lint failed"
				ck.Output.Text = "just some log output with no annotations"
				return ck
			}(),
			want: "agent-lint failed",
		},
		{
			name: "empty output yields empty excerpt (degrade)",
			ck:   greptileCheckRun{},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := checkRunOutputErrorLines(tt.ck); got != tt.want {
				t.Fatalf("checkRunOutputErrorLines() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatFailureAnnotations(t *testing.T) {
	anns := []checkAnnotation{
		{AnnotationLevel: "failure", Message: "possible secret detected (ghp) in added diff lines.", Path: ".github"},
		{AnnotationLevel: "warning", Message: "draft without WIP marker", Path: ".github"},
		{AnnotationLevel: "failure", Message: "forbidden artifact committed", Path: "internal/x.go", StartLine: 12},
		{AnnotationLevel: "failure", Message: "", Title: "titled failure"},
	}
	got := formatFailureAnnotations(anns)
	if strings.Contains(got, "draft without WIP marker") {
		t.Error("warning-level annotation should be dropped")
	}
	if !strings.Contains(got, "possible secret detected (ghp)") {
		t.Errorf("failure annotation message missing: %q", got)
	}
	if !strings.Contains(got, "internal/x.go:12: forbidden artifact committed") {
		t.Errorf("path:line prefix missing: %q", got)
	}
	if !strings.Contains(got, "titled failure") {
		t.Errorf("title fallback missing when message empty: %q", got)
	}
	// The ".github" path is generic action noise and should not be prefixed.
	if strings.Contains(got, ".github:") {
		t.Errorf("generic .github path should not be prefixed: %q", got)
	}
}

func TestFailingChecksFieldsRoundTrip(t *testing.T) {
	// FailingCheck must carry name + conclusion even when no excerpt is
	// available, so a caller can still name the check (graceful degradation).
	fc := FailingCheck{Name: "agent-lint", Conclusion: "failure"}
	if fc.Excerpt != "" {
		t.Fatalf("expected empty excerpt, got %q", fc.Excerpt)
	}
	if fc.Name != "agent-lint" || fc.Conclusion != "failure" {
		t.Fatalf("unexpected fields: %+v", fc)
	}
}
