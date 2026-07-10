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
				ck.Output.Text = "Running agent-lint\n::error::agent-lint: possible secret detected (github-token) in added diff lines.\nsome noise\n##[error]agent-lint: 1 check(s) failed"
				return ck
			}(),
			want: "agent-lint: possible secret detected (github-token) in added diff lines.\nagent-lint: 1 check(s) failed",
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

// A GitHub Actions step that fails emits "Error: Process completed with exit
// code 1" into its output body; checkErrorLineRe matches it (via the `error:`
// prefix) and strips it to "Process completed with exit code 1". That excerpt
// is non-empty but names no actionable error, so PRFailingChecks must recognize
// it as generic and fall through to the failure annotations instead of skipping
// them — otherwise the real agent-lint error never reaches the retry prompt
// (#857 review: generic output hid annotations).
func TestCheckRunOutputErrorLines_GenericBoilerplateStaysGeneric(t *testing.T) {
	var ck greptileCheckRun
	ck.Output.Text = "Error: Process completed with exit code 1"
	got := checkRunOutputErrorLines(ck)
	if got == "" {
		t.Fatalf("expected non-empty excerpt for generic boilerplate, got empty")
	}
	if !isGenericCheckExcerpt(got) {
		t.Fatalf("excerpt %q should be classified generic so annotations fallback runs", got)
	}
}

func TestIsGenericCheckExcerpt(t *testing.T) {
	generic := []string{
		"Process completed with exit code 1",
		"process completed with exit code 137",
		"Process completed with exit code 1.",
		"The process '/usr/bin/bash' failed with exit code 1",
		"  Process completed with exit code 2  ",
		"Process completed with exit code 1\n\nProcess completed with exit code 2",
	}
	for _, s := range generic {
		if !isGenericCheckExcerpt(s) {
			t.Errorf("isGenericCheckExcerpt(%q) = false, want true", s)
		}
	}
	actionable := []string{
		"",
		"agent-lint: possible secret detected (github-token) in added diff lines.",
		"agent-lint: 1 check(s) failed",
		// A generic line mixed with a real error must NOT be treated as generic,
		// so a genuine excerpt is never discarded in favor of annotations.
		"Process completed with exit code 1\nagent-lint: possible secret detected",
	}
	for _, s := range actionable {
		if isGenericCheckExcerpt(s) {
			t.Errorf("isGenericCheckExcerpt(%q) = true, want false", s)
		}
	}
}

func TestFormatFailureAnnotations(t *testing.T) {
	anns := []checkAnnotation{
		{AnnotationLevel: "failure", Message: "possible secret detected (github-token) in added diff lines.", Path: ".github"},
		{AnnotationLevel: "warning", Message: "draft without WIP marker", Path: ".github"},
		{AnnotationLevel: "failure", Message: "forbidden artifact committed", Path: "internal/x.go", StartLine: 12},
		{AnnotationLevel: "failure", Message: "", Title: "titled failure"},
	}
	got := formatFailureAnnotations(anns)
	if strings.Contains(got, "draft without WIP marker") {
		t.Error("warning-level annotation should be dropped")
	}
	if !strings.Contains(got, "possible secret detected (github-token)") {
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

// The annotations endpoint is a plain JSON array, so `gh api --paginate` emits
// each page as its own back-to-back `[...]` document. A check-run with more than
// 100 annotations therefore returns `[...][...]`, which a single json.Unmarshal
// rejects. parseCheckAnnotations must merge the pages so the failing-log excerpt
// still parses instead of silently degrading to name + conclusion (#857 review).
func TestParseCheckAnnotations_MergesPaginatedArrays(t *testing.T) {
	page1 := `[{"annotation_level":"failure","message":"boom one","path":"a.go","start_line":1}]`
	page2 := `[{"annotation_level":"failure","message":"boom two","path":"b.go","start_line":2}]`

	anns, err := parseCheckAnnotations([]byte(page1 + page2))
	if err != nil {
		t.Fatalf("parseCheckAnnotations over two pages: %v", err)
	}
	if len(anns) != 2 {
		t.Fatalf("want 2 merged annotations across pages, got %d: %+v", len(anns), anns)
	}
	if anns[0].Message != "boom one" || anns[1].Message != "boom two" {
		t.Fatalf("annotations not merged in order: %+v", anns)
	}

	// A single-page body is one `[...]` document and must still parse.
	single, err := parseCheckAnnotations([]byte(page1))
	if err != nil {
		t.Fatalf("parseCheckAnnotations single page: %v", err)
	}
	if len(single) != 1 {
		t.Fatalf("want 1 annotation for single page, got %d", len(single))
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
