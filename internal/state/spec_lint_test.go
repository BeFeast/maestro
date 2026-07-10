package state

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRecordSpecLint_IdempotencyByBodyHash(t *testing.T) {
	s := NewState()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	if s.IssueLintedForBody(101, "hashA") {
		t.Fatalf("fresh state should report not-linted")
	}
	s.RecordSpecLint(101, "hashA", false, now)
	if !s.IssueLintedForBody(101, "hashA") {
		t.Fatalf("issue should be linted for hashA")
	}
	// A different body hash means the body changed → re-lint required.
	if s.IssueLintedForBody(101, "hashB") {
		t.Fatalf("changed body hash must report not-linted so a re-lint runs")
	}
	// Distinct issues do not collide.
	if s.IssueLintedForBody(202, "hashA") {
		t.Fatalf("distinct issue must not share issue 101's lint mark")
	}
}

func TestSpecLintPassedForBody_DefaultClosed(t *testing.T) {
	s := NewState()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	if s.SpecLintPassedForBody(1, "h") {
		t.Fatalf("unlinted issue must not report passed")
	}
	s.RecordSpecLint(1, "h", false, now)
	if s.SpecLintPassedForBody(1, "h") {
		t.Fatalf("failing lint must not report passed")
	}
	s.RecordSpecLint(1, "h", true, now)
	if !s.SpecLintPassedForBody(1, "h") {
		t.Fatalf("passing lint should report passed for the same body hash")
	}
	if s.SpecLintPassedForBody(1, "other") {
		t.Fatalf("pass is scoped to the exact body hash")
	}
}

func TestGroomMention_HandledOnce(t *testing.T) {
	s := NewState()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	if s.GroomMentionHandled(5, 900) {
		t.Fatalf("no mention handled yet")
	}
	s.MarkGroomMentionHandled(5, 900, now)
	if !s.GroomMentionHandled(5, 900) {
		t.Fatalf("mention 900 should be marked handled")
	}
	// A newer comment id is a new mention.
	if s.GroomMentionHandled(5, 901) {
		t.Fatalf("a newer comment id must be treated as an unhandled mention")
	}
	// Marking does not clobber the lint verdict.
	s.RecordSpecLint(5, "h", false, now)
	s.MarkGroomMentionHandled(5, 950, now)
	if !s.IssueLintedForBody(5, "h") {
		t.Fatalf("marking a mention handled must preserve the lint mark")
	}
}

func TestRecordEditIssueBodyApproval_MintAndRefreshInPlace(t *testing.T) {
	s := NewState()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	a := s.RecordEditIssueBodyApproval(77, "## Summary\nv1", "basehash-v1", "Apply rewrite", "owner/repo", "owner/repo", []string{"reason"}, now)
	if a == nil {
		t.Fatalf("expected an approval")
	}
	if a.Action != "edit_issue_body" || a.Status != ApprovalStatusPending {
		t.Fatalf("unexpected approval: action=%s status=%s", a.Action, a.Status)
	}
	if a.Target == nil || a.Target.Issue != 77 || a.Target.Body != "## Summary\nv1" {
		t.Fatalf("target not carried: %+v", a.Target)
	}
	if a.Target.BaseBodyHash != "basehash-v1" {
		t.Fatalf("base body hash not carried: %q", a.Target.BaseBodyHash)
	}
	if a.PayloadHash == "" || a.ComputePayloadHash() != a.PayloadHash {
		t.Fatalf("payload hash must be set and self-consistent")
	}
	if len(s.Approvals) != 1 {
		t.Fatalf("expected exactly one approval, got %d", len(s.Approvals))
	}
	firstID := a.ID

	// A re-groom with a fresh rewrite refreshes IN PLACE under the same id.
	b := s.RecordEditIssueBodyApproval(77, "## Summary\nv2", "basehash-v2", "Apply rewrite", "owner/repo", "owner/repo", nil, now.Add(time.Minute))
	if len(s.Approvals) != 1 {
		t.Fatalf("re-groom must not mint a duplicate; got %d approvals", len(s.Approvals))
	}
	if b.ID != firstID {
		t.Fatalf("refreshed approval id changed: %s != %s", b.ID, firstID)
	}
	if b.Target.Body != "## Summary\nv2" {
		t.Fatalf("refreshed approval must carry the new body, got %q", b.Target.Body)
	}
	if b.Target.BaseBodyHash != "basehash-v2" {
		t.Fatalf("refreshed approval must carry the new base body hash, got %q", b.Target.BaseBodyHash)
	}
	if b.ComputePayloadHash() != b.PayloadHash {
		t.Fatalf("payload hash must be recomputed after body refresh")
	}
}

func TestRecordEditIssueBodyApproval_DistinctIssuesGetDistinctApprovals(t *testing.T) {
	s := NewState()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	s.RecordEditIssueBodyApproval(1, "a", "ha", "s", "owner/repo", "owner/repo", nil, now)
	s.RecordEditIssueBodyApproval(2, "b", "hb", "s", "owner/repo", "owner/repo", nil, now)
	if len(s.Approvals) != 2 {
		t.Fatalf("expected 2 approvals for 2 issues, got %d", len(s.Approvals))
	}
	if _, ok := s.PendingEditIssueBodyApproval(1); !ok {
		t.Fatalf("expected a pending approval for issue 1")
	}
	if _, ok := s.PendingEditIssueBodyApproval(3); ok {
		t.Fatalf("did not expect a pending approval for issue 3")
	}
}

func TestSpecLintTracks_SurvivesJSONRoundTrip(t *testing.T) {
	s := NewState()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	s.RecordSpecLint(9, "hash9", true, now)
	s.MarkGroomMentionHandled(9, 4242, now)

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var loaded State
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !loaded.SpecLintPassedForBody(9, "hash9") {
		t.Fatalf("lint pass lost across round trip")
	}
	if !loaded.GroomMentionHandled(9, 4242) {
		t.Fatalf("groom mention marker lost across round trip")
	}
}

func TestMergeSpecLintTracks_LatestWriteWins(t *testing.T) {
	older := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)

	current := map[int]SpecLintTrack{
		1: {Issue: 1, BodyHash: "old", Pass: false, UpdatedAt: older},
		2: {Issue: 2, BodyHash: "keep", Pass: true, UpdatedAt: newer},
	}
	ours := map[int]SpecLintTrack{
		1: {Issue: 1, BodyHash: "new", Pass: true, UpdatedAt: newer},
		3: {Issue: 3, BodyHash: "fresh", Pass: false, UpdatedAt: older},
	}
	merged := mergeSpecLintTracks(current, ours)
	if merged[1].BodyHash != "new" || !merged[1].Pass {
		t.Fatalf("issue 1 should take the newer write: %+v", merged[1])
	}
	if merged[2].BodyHash != "keep" {
		t.Fatalf("issue 2 (only in current) should survive: %+v", merged[2])
	}
	if merged[3].BodyHash != "fresh" {
		t.Fatalf("issue 3 (only in ours) should survive: %+v", merged[3])
	}
}
