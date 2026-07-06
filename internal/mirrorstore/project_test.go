package mirrorstore

import (
	"context"
	"testing"
	"time"
)

func TestProjectIssueAndLabels(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	recv := mustTime(t, "2026-07-01T13:00:00Z")

	payload := []byte(`{
		"action": "labeled",
		"issue": {
			"number": 42,
			"title": "hello",
			"body": "world",
			"state": "open",
			"updated_at": "2026-07-01T12:00:00Z",
			"labels": [{"name": "bug"}, {"name": "p1"}]
		},
		"repository": {"full_name": "o/r"}
	}`)
	if err := s.ProjectWebhook(ctx, "issues", payload, recv); err != nil {
		t.Fatalf("ProjectWebhook: %v", err)
	}
	row, ok, err := s.GetIssue(ctx, "o/r", 42)
	if err != nil || !ok {
		t.Fatalf("GetIssue ok=%v err=%v", ok, err)
	}
	if row.Title != "hello" || row.Body != "world" || row.State != "open" || row.Source != SourceWebhook {
		t.Fatalf("issue row = %+v", row)
	}
	if !row.LastSeenAt.Equal(mustTime(t, "2026-07-01T12:00:00Z")) {
		t.Fatalf("last_seen_at = %v, want issue.updated_at", row.LastSeenAt)
	}
	labels, _ := s.Labels(ctx, "o/r", SubjectIssue, 42)
	if len(labels) != 2 || labels[0] != "bug" || labels[1] != "p1" {
		t.Fatalf("labels = %v", labels)
	}
}

// TestProjectOutOfOrderIssue covers AC 2 at the projection layer: a later-arriving
// but older event neither regresses the issue row nor its label set.
func TestProjectOutOfOrderIssue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	newer := []byte(`{
		"action": "edited",
		"issue": {"number": 1, "title": "NEW", "state": "closed",
			"updated_at": "2026-07-01T12:00:00Z",
			"labels": [{"name": "keep"}]},
		"repository": {"full_name": "o/r"}
	}`)
	older := []byte(`{
		"action": "edited",
		"issue": {"number": 1, "title": "OLD", "state": "open",
			"updated_at": "2026-07-01T09:00:00Z",
			"labels": [{"name": "stale-label"}]},
		"repository": {"full_name": "o/r"}
	}`)

	// Apply the newer event first, then replay the older one out of order.
	if err := s.ProjectWebhook(ctx, "issues", newer, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := s.ProjectWebhook(ctx, "issues", older, time.Time{}); err != nil {
		t.Fatal(err)
	}

	row, _, _ := s.GetIssue(ctx, "o/r", 1)
	if row.Title != "NEW" || row.State != "closed" {
		t.Fatalf("older event regressed issue: %+v", row)
	}
	labels, _ := s.Labels(ctx, "o/r", SubjectIssue, 1)
	if len(labels) != 1 || labels[0] != "keep" {
		t.Fatalf("older event regressed labels: %v", labels)
	}

	// Replaying the newer event again is idempotent.
	if err := s.ProjectWebhook(ctx, "issues", newer, time.Time{}); err != nil {
		t.Fatal(err)
	}
	labels, _ = s.Labels(ctx, "o/r", SubjectIssue, 1)
	if len(labels) != 1 || labels[0] != "keep" {
		t.Fatalf("duplicate replay changed labels: %v", labels)
	}
}

func TestProjectPullRequest(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	payload := []byte(`{
		"action": "synchronize",
		"number": 828,
		"pull_request": {
			"number": 828, "title": "feat", "state": "open",
			"draft": true, "merged": false,
			"updated_at": "2026-07-02T09:15:00Z",
			"head": {"sha": "abc123"}, "base": {"ref": "main"},
			"labels": [{"name": "maestro-ready"}]
		},
		"repository": {"full_name": "o/r"}
	}`)
	if err := s.ProjectWebhook(ctx, "pull_request", payload, time.Time{}); err != nil {
		t.Fatalf("ProjectWebhook: %v", err)
	}
	pr, ok, err := s.GetPullRequest(ctx, "o/r", 828)
	if err != nil || !ok {
		t.Fatalf("GetPullRequest ok=%v err=%v", ok, err)
	}
	if pr.Title != "feat" || !pr.Draft || pr.Merged || pr.HeadSHA != "abc123" || pr.BaseRef != "main" {
		t.Fatalf("pr row = %+v", pr)
	}
	labels, _ := s.Labels(ctx, "o/r", SubjectPullRequest, 828)
	if len(labels) != 1 || labels[0] != "maestro-ready" {
		t.Fatalf("pr labels = %v", labels)
	}
}

func TestProjectCheckRunAndStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	checkRun := []byte(`{
		"check_run": {"name": "go test", "status": "completed", "conclusion": "success",
			"head_sha": "sha1", "completed_at": "2026-07-02T09:20:00Z"},
		"repository": {"full_name": "o/r"}
	}`)
	status := []byte(`{
		"sha": "sha1", "context": "ci/lint", "state": "failure",
		"updated_at": "2026-07-02T09:21:00Z",
		"repository": {"full_name": "o/r"}
	}`)
	if err := s.ProjectWebhook(ctx, "check_run", checkRun, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := s.ProjectWebhook(ctx, "status", status, time.Time{}); err != nil {
		t.Fatal(err)
	}
	checks, err := s.Checks(ctx, "o/r", "sha1")
	if err != nil {
		t.Fatalf("Checks: %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("want 2 checks for sha1, got %d: %+v", len(checks), checks)
	}
	byName := map[string]Check{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	if c := byName["go test"]; c.Kind != CheckKindRun || c.Conclusion != "success" {
		t.Fatalf("check_run row = %+v", c)
	}
	if c := byName["ci/lint"]; c.Kind != CheckKindStatus || c.Status != "failure" || c.Conclusion != "failure" {
		t.Fatalf("status row = %+v", c)
	}
}

func TestProjectReviewAndComments(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	review := []byte(`{
		"review": {"id": 555, "state": "approved", "body": "LGTM",
			"user": {"login": "rev"}, "submitted_at": "2026-07-02T10:05:00Z"},
		"pull_request": {"number": 828},
		"repository": {"full_name": "o/r"}
	}`)
	if err := s.ProjectWebhook(ctx, "pull_request_review", review, time.Time{}); err != nil {
		t.Fatal(err)
	}
	rev, ok, err := s.GetReview(ctx, "o/r", 555)
	if err != nil || !ok {
		t.Fatalf("GetReview ok=%v err=%v", ok, err)
	}
	if rev.PRNumber != 828 || rev.State != "approved" || rev.Author != "rev" {
		t.Fatalf("review row = %+v", rev)
	}

	// issue_comment on a PR (issue.pull_request present) is recorded as a PR subject.
	prComment := []byte(`{
		"issue": {"number": 828, "pull_request": {"url": "x"}},
		"comment": {"id": 900, "body": "note", "user": {"login": "u"},
			"updated_at": "2026-07-02T11:00:00Z"},
		"repository": {"full_name": "o/r"}
	}`)
	if err := s.ProjectWebhook(ctx, "issue_comment", prComment, time.Time{}); err != nil {
		t.Fatal(err)
	}
	c, ok, err := s.GetComment(ctx, "o/r", 900)
	if err != nil || !ok {
		t.Fatalf("GetComment ok=%v err=%v", ok, err)
	}
	if c.SubjectType != SubjectPullRequest || c.SubjectNumber != 828 || c.Body != "note" {
		t.Fatalf("comment row = %+v", c)
	}
}

func TestProjectProjectItem(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	payload := []byte(`{
		"action": "edited",
		"projects_v2_item": {"node_id": "PVTI_x", "content_type": "Issue",
			"updated_at": "2026-07-02T12:00:00Z"},
		"changes": {"field_value": {"to": {"name": "In Progress"}}},
		"repository": {"full_name": "o/r"}
	}`)
	if err := s.ProjectWebhook(ctx, "projects_v2_item", payload, time.Time{}); err != nil {
		t.Fatal(err)
	}
	it, ok, err := s.GetProjectItem(ctx, "o/r", "PVTI_x")
	if err != nil || !ok {
		t.Fatalf("GetProjectItem ok=%v err=%v", ok, err)
	}
	if it.ContentType != "Issue" || it.Status != "In Progress" {
		t.Fatalf("project item row = %+v", it)
	}
}

// TestProjectUnmodelledEventIsNoop confirms an event the mirror does not model is
// silently skipped (and never errors), so widening coverage later needs no
// re-ingestion.
func TestProjectUnmodelledEventIsNoop(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, ev := range []string{"ping", "label", "check_suite", "membership"} {
		if err := s.ProjectWebhook(ctx, ev, []byte(`{"repository":{"full_name":"o/r"}}`), time.Time{}); err != nil {
			t.Fatalf("ProjectWebhook(%s) = %v, want nil", ev, err)
		}
	}
	counts, _ := s.Counts(ctx)
	if counts.Total() != 0 {
		t.Fatalf("unmodelled events wrote rows: %+v", counts)
	}
}

// TestProjectFallsBackToReceivedAt confirms a payload with no resource timestamp
// orders by the delivery's received_at.
func TestProjectFallsBackToReceivedAt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	recv := mustTime(t, "2026-07-05T08:00:00Z")
	payload := []byte(`{"issue":{"number":3,"title":"t","state":"open"},"repository":{"full_name":"o/r"}}`)
	if err := s.ProjectWebhook(ctx, "issues", payload, recv); err != nil {
		t.Fatal(err)
	}
	row, _, _ := s.GetIssue(ctx, "o/r", 3)
	if !row.LastSeenAt.Equal(recv) {
		t.Fatalf("last_seen_at = %v, want received_at %v", row.LastSeenAt, recv)
	}
}
