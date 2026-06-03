package supervisor

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func TestRecordLessonProposalForDecision_GeneratesOnceForRetryExhausted(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:         "owner/repo",
		WorkerPrompt: filepath.Join(dir, "worker-prompt.md"),
	}
	st := state.NewState()
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	decision := state.SupervisorDecision{
		ID:        "decision-lesson",
		CreatedAt: now,
		Repo:      "owner/repo",
		Project:   "owner/repo",
		Target:    &state.SupervisorTarget{Issue: 640, Session: "sup-155"},
		StuckStates: []state.SupervisorStuckState{{
			Code:              "retry_exhausted",
			Severity:          SeverityBlocked,
			Summary:           "Issue #640 exhausted its retry budget.",
			Evidence:          []string{"Session sup-155 status=retry_exhausted retry_count=3"},
			RecommendedAction: ActionReviewRetryExhausted,
			Target:            &state.SupervisorTarget{Issue: 640, Session: "sup-155"},
		}},
	}

	first, created := recordLessonProposalForDecision(cfg, st, decision)
	if !created || first == nil {
		t.Fatalf("created = %v proposal=%+v, want first proposal", created, first)
	}
	if first.FailureClass != "retry_exhausted" || first.Area != "issue:640" {
		t.Fatalf("proposal = %+v, want retry_exhausted issue area", first)
	}
	if first.Target != state.LessonProposalTargetWorkerPrompt {
		t.Fatalf("target = %q, want worker_prompt", first.Target)
	}
	if !strings.Contains(first.MinimalRepro, "retry_count=3") {
		t.Fatalf("minimal repro = %q, want evidence", first.MinimalRepro)
	}
	if len(st.Approvals) != 1 || st.Approvals[0].Action != config.SupervisorActionApplyLessonProposal {
		t.Fatalf("approvals = %+v, want one apply_lesson_proposal approval", st.Approvals)
	}

	second, created := recordLessonProposalForDecision(cfg, st, decision)
	if created {
		t.Fatal("duplicate retry_exhausted proposal created")
	}
	if second == nil || second.ID != first.ID {
		t.Fatalf("duplicate proposal = %+v, want existing %s", second, first.ID)
	}
	if len(st.LessonProposals) != 1 || len(st.Approvals) != 1 {
		t.Fatalf("counts proposals=%d approvals=%d, want 1/1", len(st.LessonProposals), len(st.Approvals))
	}
}
