package state

import (
	"testing"
	"time"
)

func TestRecordLessonProposal_DedupsPendingFingerprint(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	s := NewState()
	input := LessonProposal{
		FailureClass:  "retry_exhausted",
		Area:          "issue:640",
		MinimalRepro:  "Session sup-155 status=retry_exhausted retry_count=3",
		SuggestedRule: "Inspect failed attempts before retrying.",
		Target:        LessonProposalTargetAgentsMD,
	}

	first, approval, created := s.RecordLessonProposal(input, now, "owner/repo", "owner/repo")
	if !created || first == nil || approval == nil {
		t.Fatalf("first created=%v proposal=%+v approval=%+v", created, first, approval)
	}
	if approval.LessonProposalID != first.ID {
		t.Fatalf("approval lesson id = %q, want %q", approval.LessonProposalID, first.ID)
	}

	second, secondApproval, created := s.RecordLessonProposal(input, now.Add(time.Minute), "owner/repo", "owner/repo")
	if created {
		t.Fatal("duplicate proposal was created for same fingerprint")
	}
	if second == nil || second.ID != first.ID {
		t.Fatalf("second = %+v, want existing %s", second, first.ID)
	}
	if secondApproval == nil || secondApproval.ID != approval.ID {
		t.Fatalf("second approval = %+v, want existing %s", secondApproval, approval.ID)
	}
	if len(s.LessonProposals) != 1 || len(s.Approvals) != 1 {
		t.Fatalf("counts proposals=%d approvals=%d, want 1/1", len(s.LessonProposals), len(s.Approvals))
	}
}

func TestRejectLessonProposalApprovalMarksDeclined(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	s := NewState()
	proposal, approval, created := s.RecordLessonProposal(LessonProposal{
		FailureClass:  "review_repair_exhausted",
		Area:          "pr:640",
		MinimalRepro:  "PR #640 still has P1 findings after repair budget.",
		SuggestedRule: "Address unresolved review findings only.",
		Target:        LessonProposalTargetAgentsMD,
	}, now, "owner/repo", "owner/repo")
	if !created {
		t.Fatal("proposal was not created")
	}

	rejected, err := s.RejectApproval(approval.ID, now.Add(time.Minute), "operator", "not general enough")
	if err != nil {
		t.Fatalf("RejectApproval: %v", err)
	}
	if rejected.Status != ApprovalStatusRejected {
		t.Fatalf("approval status = %q, want rejected", rejected.Status)
	}
	stored, ok := s.FindLessonProposal(proposal.ID)
	if !ok {
		t.Fatalf("proposal %s missing", proposal.ID)
	}
	if stored.Status != LessonProposalStatusDeclined || stored.DeclinedAt == nil {
		t.Fatalf("proposal = %+v, want declined timestamp", stored)
	}
	if stored.ResolutionActor != "operator" || stored.ResolutionNote != "not general enough" {
		t.Fatalf("resolution = actor %q note %q", stored.ResolutionActor, stored.ResolutionNote)
	}
}

func TestRecordLessonProposal_DoesNotReopenDeclinedFingerprint(t *testing.T) {
	now := time.Date(2026, 6, 5, 10, 59, 0, 0, time.UTC)
	s := NewState()
	input := LessonProposal{
		FailureClass:   "retry_exhausted",
		Area:           "issue:672",
		MinimalRepro:   "Session sup-1 status=retry_exhausted",
		SuggestedRule:  "Inspect retry context before retrying.",
		Target:         LessonProposalTargetAgentsMD,
		SourceDecision: "sup-1",
	}
	proposal, approval, created := s.RecordLessonProposal(input, now, "owner/repo", "owner/repo")
	if !created || proposal == nil || approval == nil {
		t.Fatalf("created=%v proposal=%+v approval=%+v", created, proposal, approval)
	}
	if _, err := s.RejectApproval(approval.ID, now.Add(time.Minute), "operator", "covered"); err != nil {
		t.Fatalf("RejectApproval: %v", err)
	}

	reemitted := input
	reemitted.MinimalRepro = "Session sup-1 status=retry_exhausted | still stuck"
	reemitted.SourceDecision = "sup-2"
	got, gotApproval, created := s.RecordLessonProposal(reemitted, now.Add(9*time.Minute), "owner/repo", "owner/repo")
	if created {
		t.Fatal("declined fingerprint reopened as a fresh pending proposal")
	}
	if got == nil || got.ID != proposal.ID || got.Status != LessonProposalStatusDeclined {
		t.Fatalf("proposal = %+v, want existing declined %s", got, proposal.ID)
	}
	if gotApproval == nil || gotApproval.ID != approval.ID || gotApproval.Status != ApprovalStatusRejected {
		t.Fatalf("approval = %+v, want original rejected %s", gotApproval, approval.ID)
	}
	if len(s.LessonProposals) != 1 || len(s.Approvals) != 1 {
		t.Fatalf("counts proposals=%d approvals=%d, want 1/1", len(s.LessonProposals), len(s.Approvals))
	}
}

func TestRecordLessonProposal_DoesNotReopenAppliedFingerprint(t *testing.T) {
	now := time.Date(2026, 6, 5, 11, 0, 0, 0, time.UTC)
	s := NewState()
	input := LessonProposal{
		FailureClass:   "retry_exhausted",
		Area:           "issue:672",
		MinimalRepro:   "Session sup-1 status=retry_exhausted",
		SuggestedRule:  "Inspect retry context before retrying.",
		Target:         LessonProposalTargetAgentsMD,
		SourceDecision: "sup-1",
	}
	proposal, _, created := s.RecordLessonProposal(input, now, "owner/repo", "owner/repo")
	if !created || proposal == nil {
		t.Fatalf("created=%v proposal=%+v", created, proposal)
	}
	if !s.MarkLessonProposalApplied(proposal.ID, now.Add(time.Minute), "operator", "added") {
		t.Fatalf("MarkLessonProposalApplied(%s) failed", proposal.ID)
	}

	got, _, created := s.RecordLessonProposal(input, now.Add(2*time.Minute), "owner/repo", "owner/repo")
	if created {
		t.Fatal("applied fingerprint reopened as a fresh pending proposal")
	}
	if got == nil || got.ID != proposal.ID || got.Status != LessonProposalStatusApplied {
		t.Fatalf("proposal = %+v, want existing applied %s", got, proposal.ID)
	}
	if len(s.LessonProposals) != 1 || len(s.Approvals) != 1 {
		t.Fatalf("counts proposals=%d approvals=%d, want 1/1", len(s.LessonProposals), len(s.Approvals))
	}
}

func TestApprovalResolutionPrefersPendingDuplicateID(t *testing.T) {
	now := time.Date(2026, 6, 5, 11, 8, 0, 0, time.UTC)
	s := NewState()
	id := "approval-lesson-3b5238ee4626"
	s.Approvals = []Approval{
		{ID: id, CreatedAt: now.Add(-9 * time.Minute), UpdatedAt: now.Add(-9 * time.Minute), Action: "apply_lesson_proposal", Status: ApprovalStatusRejected},
		{ID: id, CreatedAt: now, UpdatedAt: now, Action: "apply_lesson_proposal", Status: ApprovalStatusPending},
	}

	rejected, err := s.RejectApproval(id, now.Add(time.Minute), "cli", "duplicate regression")
	if err != nil {
		t.Fatalf("RejectApproval duplicate id: %v", err)
	}
	if rejected != &s.Approvals[1] {
		t.Fatalf("RejectApproval returned first duplicate; statuses=%s/%s", s.Approvals[0].Status, s.Approvals[1].Status)
	}
	if s.Approvals[1].Status != ApprovalStatusRejected {
		t.Fatalf("pending twin status = %q, want rejected", s.Approvals[1].Status)
	}
}

func TestLessonProposalApprovalAddressableBySourceDecisionID(t *testing.T) {
	now := time.Date(2026, 6, 5, 11, 10, 0, 0, time.UTC)
	s := NewState()
	proposal, _, created := s.RecordLessonProposal(LessonProposal{
		FailureClass:   "retry_exhausted",
		Area:           "issue:672",
		MinimalRepro:   "Session sup-20260605T110844 status=retry_exhausted",
		SuggestedRule:  "Inspect retry context before retrying.",
		Target:         LessonProposalTargetAgentsMD,
		SourceDecision: "sup-20260605T110844",
	}, now, "owner/repo", "owner/repo")
	if !created || proposal == nil {
		t.Fatalf("created=%v proposal=%+v", created, proposal)
	}

	rejected, err := s.RejectApproval("sup-20260605T110844", now.Add(time.Minute), "cli", "source decision")
	if err != nil {
		t.Fatalf("RejectApproval by source decision: %v", err)
	}
	if rejected.ID != proposal.ApprovalID || rejected.Status != ApprovalStatusRejected {
		t.Fatalf("rejected = %+v, want lesson approval %s rejected", rejected, proposal.ApprovalID)
	}
}

func TestMigrateDuplicateApprovalIDsKeepsPendingCanonical(t *testing.T) {
	now := time.Date(2026, 6, 5, 11, 8, 0, 0, time.UTC)
	s := NewState()
	id := "approval-lesson-3b5238ee4626"
	s.LessonProposals = []LessonProposal{{
		ID:             "lesson-3b5238ee4626",
		CreatedAt:      now.Add(-10 * time.Minute),
		UpdatedAt:      now,
		FailureClass:   "retry_exhausted",
		Area:           "issue:672",
		MinimalRepro:   "Session sup-20260605T110844 status=retry_exhausted",
		SuggestedRule:  "Inspect retry context before retrying.",
		Target:         LessonProposalTargetAgentsMD,
		Fingerprint:    "3b5238ee4626",
		Status:         LessonProposalStatusPending,
		ApprovalID:     id,
		SourceDecision: "sup-20260605T110844",
	}}
	s.Approvals = []Approval{
		{ID: id, CreatedAt: now.Add(-9 * time.Minute), UpdatedAt: now.Add(-9 * time.Minute), Action: "apply_lesson_proposal", Status: ApprovalStatusRejected, LessonProposalID: "lesson-3b5238ee4626"},
		{ID: id, CreatedAt: now, UpdatedAt: now, Action: "apply_lesson_proposal", Status: ApprovalStatusPending, LessonProposalID: "lesson-3b5238ee4626"},
	}

	if migrated := s.MigrateDuplicateApprovalIDs(); migrated != 3 {
		t.Fatalf("migrated = %d, want 3 id/link updates", migrated)
	}
	if s.Approvals[1].ID != id {
		t.Fatalf("pending approval id = %q, want canonical %q", s.Approvals[1].ID, id)
	}
	if s.Approvals[0].ID == id || s.Approvals[0].ID == s.Approvals[1].ID {
		t.Fatalf("historical duplicate was not split: %+v", s.Approvals)
	}
	if s.LessonProposals[0].ApprovalID != id {
		t.Fatalf("proposal approval_id = %q, want pending canonical %q", s.LessonProposals[0].ApprovalID, id)
	}
	if s.Approvals[1].DecisionID != "sup-20260605T110844" {
		t.Fatalf("pending decision_id = %q, want source decision", s.Approvals[1].DecisionID)
	}
	if _, err := s.RejectApproval(id, now.Add(time.Minute), "cli", "after migration"); err != nil {
		t.Fatalf("RejectApproval canonical pending after migration: %v", err)
	}
}
