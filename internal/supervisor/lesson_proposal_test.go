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
	st.Sessions["sup-155"] = &state.Session{
		IssueNumber:         640,
		Status:              state.StatusRetryExhausted,
		LastOutputChangedAt: now.Add(-30 * time.Minute),
	}
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

func TestRecordLessonProposalForDecision_SkipsPreflightOnlyRetryExhausted(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:         "owner/repo",
		WorkerPrompt: filepath.Join(dir, "worker-prompt.md"),
	}
	st := state.NewState()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	st.Sessions["scr-525"] = &state.Session{IssueNumber: 528, Status: state.StatusDead, StartedAt: now.Add(-4 * time.Hour)}
	st.Sessions["scr-526"] = &state.Session{IssueNumber: 528, Status: state.StatusDead, StartedAt: now.Add(-3 * time.Hour)}
	st.Sessions["scr-527"] = &state.Session{IssueNumber: 528, Status: state.StatusDead, StartedAt: now.Add(-2 * time.Hour)}
	st.Sessions["scr-528"] = &state.Session{IssueNumber: 528, Status: state.StatusRetryExhausted, StartedAt: now.Add(-time.Hour)}
	decision := state.SupervisorDecision{
		ID:        "decision-preflight-only",
		CreatedAt: now,
		Repo:      "owner/repo",
		Project:   "owner/repo",
		Target:    &state.SupervisorTarget{Issue: 999, Session: "scr-focus"},
		StuckStates: []state.SupervisorStuckState{{
			Code:              "retry_exhausted",
			Severity:          SeverityBlocked,
			Summary:           "Issue #528 exhausted its retry budget.",
			Evidence:          []string{"Session scr-528 status=retry_exhausted retry_count=3"},
			RecommendedAction: ActionReviewRetryExhausted,
			Target:            &state.SupervisorTarget{Issue: 528, Session: "scr-528"},
		}},
	}

	proposal, created := recordLessonProposalForDecision(cfg, st, decision)
	if created || proposal != nil {
		t.Fatalf("proposal created=%v proposal=%+v, want none for preflight-only attempts", created, proposal)
	}
	if len(st.LessonProposals) != 0 || len(st.Approvals) != 0 {
		t.Fatalf("counts proposals=%d approvals=%d, want 0/0", len(st.LessonProposals), len(st.Approvals))
	}
}

func TestRecordLessonProposalForDecision_AreaUsesFailingSourceTarget(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:         "owner/repo",
		WorkerPrompt: filepath.Join(dir, "worker-prompt.md"),
	}
	st := state.NewState()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	st.Sessions["scr-528"] = &state.Session{
		IssueNumber:         528,
		Status:              state.StatusRetryExhausted,
		StartedAt:           now.Add(-time.Hour),
		LastOutputChangedAt: now.Add(-30 * time.Minute),
	}
	decision := state.SupervisorDecision{
		ID:        "decision-area-source",
		CreatedAt: now,
		Repo:      "owner/repo",
		Project:   "owner/repo",
		Target:    &state.SupervisorTarget{Issue: 999, Session: "scr-focus"},
		StuckStates: []state.SupervisorStuckState{{
			Code:              "retry_exhausted",
			Severity:          SeverityBlocked,
			Summary:           "Issue #528 exhausted its retry budget.",
			Evidence:          []string{"Session scr-528 status=retry_exhausted retry_count=3"},
			RecommendedAction: ActionReviewRetryExhausted,
			Target:            &state.SupervisorTarget{Issue: 528, Session: "scr-528"},
		}},
	}

	proposal, created := recordLessonProposalForDecision(cfg, st, decision)
	if !created || proposal == nil {
		t.Fatalf("created=%v proposal=%+v, want proposal", created, proposal)
	}
	if proposal.Area != "issue:528" {
		t.Fatalf("area = %q, want failing source issue", proposal.Area)
	}
	if proposal.SourceTarget == nil || proposal.SourceTarget.Issue != 528 || proposal.SourceTarget.Session != "scr-528" {
		t.Fatalf("source target = %#v, want failing source", proposal.SourceTarget)
	}
}
