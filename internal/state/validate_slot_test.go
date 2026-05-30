package state

import (
	"strings"
	"testing"
	"time"
)

func TestValidateSlotID_AcceptsCanonicalSlots(t *testing.T) {
	for _, slot := range []string{"sup-77", "scr-241", "ok-1", "fin-99", "a", "abc-XYZ_42"} {
		if err := ValidateSlotID(slot); err != nil {
			t.Errorf("ValidateSlotID(%q) = %v, want nil", slot, err)
		}
	}
}

func TestValidateSlotID_RejectsTraversalAndSeparators(t *testing.T) {
	cases := []string{
		"",
		" ",
		".",
		"..",
		"../etc",
		"a/b",
		`a\b`,
		"slot/",
		"/slot",
		"with space",
		"abc.def",
		"abc:def",
		"foo;bar",
		"<script>",
	}
	for _, slot := range cases {
		if err := ValidateSlotID(slot); err == nil {
			t.Errorf("ValidateSlotID(%q) = nil, want error", slot)
		}
	}
}

func TestValidateSlotID_RejectsNULAndControl(t *testing.T) {
	cases := []string{
		"a\x00b",
		"a\nb",
		"a\tb",
		"a\rb",
	}
	for _, slot := range cases {
		if err := ValidateSlotID(slot); err == nil {
			t.Errorf("ValidateSlotID(%q) = nil, want error (control byte)", slot)
		}
	}
}

func TestValidateSlotID_RejectsOversize(t *testing.T) {
	long := strings.Repeat("a", 97)
	if err := ValidateSlotID(long); err == nil {
		t.Errorf("ValidateSlotID(97-byte input) = nil, want error")
	}
	exact := strings.Repeat("a", 96)
	if err := ValidateSlotID(exact); err != nil {
		t.Errorf("ValidateSlotID(96-byte input) = %v, want nil", err)
	}
}

// --- ingress: state-write boundary should refuse a malformed slot ----------

func TestRecordPendingApprovalForDecision_RejectsMalformedSession(t *testing.T) {
	for _, slot := range []string{"../etc/passwd", "a/b", `a\b`, "..", "."} {
		s := NewState()
		now := time.Now().UTC()
		decision := SupervisorDecision{
			ID:                "test-decision",
			CreatedAt:         now,
			RecommendedAction: "delete_worktree",
			Risk:              "high",
			RequiresApproval:  true,
			Target:            &SupervisorTarget{Session: slot, Issue: 42},
		}
		approval := s.RecordPendingApprovalForDecision(decision, now)
		if approval != nil {
			t.Errorf("slot %q: RecordPendingApprovalForDecision returned %+v, want nil (malformed slot must be rejected at ingress)", slot, approval)
		}
		if len(s.Approvals) != 0 {
			t.Errorf("slot %q: %d approvals were appended, want 0", slot, len(s.Approvals))
		}
	}
}

func TestRecordPendingApprovalForDecision_AcceptsValidSession(t *testing.T) {
	s := NewState()
	now := time.Now().UTC()
	decision := SupervisorDecision{
		ID:                "test-decision",
		CreatedAt:         now,
		RecommendedAction: "delete_worktree",
		Risk:              "high",
		RequiresApproval:  true,
		Target:            &SupervisorTarget{Session: "sup-77", Issue: 42},
	}
	approval := s.RecordPendingApprovalForDecision(decision, now)
	if approval == nil {
		t.Fatalf("RecordPendingApprovalForDecision returned nil for valid slot")
	}
	if approval.Target.Session != "sup-77" {
		t.Fatalf("approval.Target.Session = %q, want sup-77", approval.Target.Session)
	}
}

func TestRecordPendingApprovalForDecision_NilTarget_NoSessionCheck(t *testing.T) {
	// Decisions with no target (e.g. global config changes) skip the
	// session check entirely.
	s := NewState()
	now := time.Now().UTC()
	decision := SupervisorDecision{
		ID:                "test-decision",
		CreatedAt:         now,
		RecommendedAction: "change_global_config",
		Risk:              "high",
		RequiresApproval:  true,
		// Target is nil — common for non-session-targeted decisions.
	}
	approval := s.RecordPendingApprovalForDecision(decision, now)
	if approval == nil {
		t.Fatalf("RecordPendingApprovalForDecision returned nil for nil-target decision")
	}
}

func TestRecordPendingApprovalForDecision_TargetWithoutSession_NoCheck(t *testing.T) {
	// Target with PR/Issue but no Session — common for merge_pr / close_issue
	// approvals. Validator must not trip on absent Session.
	s := NewState()
	now := time.Now().UTC()
	decision := SupervisorDecision{
		ID:                "test-decision",
		CreatedAt:         now,
		RecommendedAction: "merge_pr",
		Risk:              "high",
		RequiresApproval:  true,
		Target:            &SupervisorTarget{Issue: 42, PR: 99},
	}
	approval := s.RecordPendingApprovalForDecision(decision, now)
	if approval == nil {
		t.Fatalf("RecordPendingApprovalForDecision returned nil for absent-session decision")
	}
}
