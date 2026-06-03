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
