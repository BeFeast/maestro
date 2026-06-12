package supervisor

import (
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/state"
)

func TestDetectVisualEvidenceStuckStates(t *testing.T) {
	st := state.NewState()
	st.Sessions["sup-1"] = &state.Session{
		IssueNumber:          705,
		PRNumber:             710,
		Status:               state.StatusPROpen,
		VisualEvidence:       state.VisualEvidenceMissing,
		VisualEvidenceDetail: "capture command failed: exit 1",
	}
	// Stamped but fine — no finding.
	st.Sessions["sup-2"] = &state.Session{
		IssueNumber:    1,
		PRNumber:       2,
		Status:         state.StatusPROpen,
		VisualEvidence: state.VisualEvidenceAttached,
	}
	st.Sessions["sup-3"] = &state.Session{
		IssueNumber:    3,
		PRNumber:       4,
		Status:         state.StatusPROpen,
		VisualEvidence: state.VisualEvidenceNotRequired,
	}
	// Missing evidence but the session is terminal — self-clears.
	st.Sessions["sup-4"] = &state.Session{
		IssueNumber:    5,
		PRNumber:       6,
		Status:         state.StatusDone,
		VisualEvidence: state.VisualEvidenceMissing,
	}
	// Not yet checked — no finding.
	st.Sessions["sup-5"] = &state.Session{
		IssueNumber: 7,
		PRNumber:    8,
		Status:      state.StatusPROpen,
	}

	findings := detectVisualEvidenceStuckStates(st)
	if len(findings) != 1 {
		t.Fatalf("expected exactly one finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.Code != "visual_evidence_missing" {
		t.Fatalf("Code = %q", f.Code)
	}
	if f.Severity != SeverityWarning {
		t.Fatalf("Severity = %q, want %q (advisory, never a merge block)", f.Severity, SeverityWarning)
	}
	if f.SupervisorCanAct {
		t.Fatal("visual_evidence_missing must not be supervisor-actionable in v1")
	}
	if !strings.Contains(f.Summary, "PR #710") || !strings.Contains(f.Summary, "issue #705") {
		t.Fatalf("Summary = %q", f.Summary)
	}
	if f.Target == nil || f.Target.PR != 710 || f.Target.Issue != 705 || f.Target.Session != "sup-1" {
		t.Fatalf("Target = %+v", f.Target)
	}
	foundDetail := false
	for _, e := range f.Evidence {
		if strings.Contains(e, "capture command failed") {
			foundDetail = true
		}
	}
	if !foundDetail {
		t.Fatalf("Evidence missing capture detail: %v", f.Evidence)
	}
}

func TestDetectVisualEvidenceStuckStates_EmptyState(t *testing.T) {
	if got := detectVisualEvidenceStuckStates(nil); len(got) != 0 {
		t.Fatalf("nil state should yield no findings, got %v", got)
	}
	if got := detectVisualEvidenceStuckStates(state.NewState()); len(got) != 0 {
		t.Fatalf("empty state should yield no findings, got %v", got)
	}
}
