package outcome

import (
	"strings"
	"testing"
	"time"
)

func TestDetectDriftClosedIssueWithFailingRuntime(t *testing.T) {
	closedAt := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	checkedAt := closedAt.Add(time.Minute)
	item := DetectDrift(DriftInput{
		Brief: Brief{
			DesiredOutcome:     "App is live at /settings",
			HealthcheckCommand: "./verify-live.sh",
		},
		LastMergeAt: time.Time{},
		ClosedIssues: []DriftIssue{
			{Number: 353, Title: "P0 outcome drift", ClosedAt: closedAt},
		},
		Health: &HealthCheckResult{
			CheckedAt: checkedAt,
			Signal:    "healthcheck_command",
			State:     HealthFailing,
			Summary:   "verify-live.sh exited 7",
			Detail:    "route /settings missing",
		},
		Policy: DriftPolicy{CommentIssue: true},
	})
	if item == nil {
		t.Fatal("DetectDrift = nil, want active drift item")
	}
	if item.HealthState != HealthFailing {
		t.Fatalf("HealthState = %q, want %q", item.HealthState, HealthFailing)
	}
	if !strings.Contains(item.Summary, "#353") || !strings.Contains(item.Summary, "failing") {
		t.Fatalf("Summary = %q, want closed issue + failing runtime", item.Summary)
	}
	if !strings.Contains(item.NextAction, "verify-live.sh") {
		t.Fatalf("NextAction = %q, want healthcheck command guidance", item.NextAction)
	}
	if item.RecommendedAction != DriftActionRequestApproval || !item.RequiresApproval {
		t.Fatalf("RecommendedAction/RequiresApproval = %q/%v, want approval-gated mutating action", item.RecommendedAction, item.RequiresApproval)
	}
	if item.SignalSummary != "verify-live.sh exited 7" || item.SignalDetail == "" {
		t.Fatalf("Signal metadata = %+v, want failing signal details", item)
	}
	if len(item.ClosedIssues) != 1 || item.ClosedIssues[0].Number != 353 {
		t.Fatalf("ClosedIssues = %+v, want issue #353", item.ClosedIssues)
	}
}

func TestDetectDriftMergedPRWithUnknownRuntime(t *testing.T) {
	mergedAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	item := DetectDrift(DriftInput{
		Brief: Brief{
			DesiredOutcome:     "App is live",
			HealthcheckCommand: "./status.sh",
		},
		LastMergeAt: mergedAt,
		MergedPRs: []DriftPR{
			{Number: 468, Title: "fix(github): ignore #N inside code blocks", MergedAt: mergedAt},
		},
		Health: nil,
	})
	if item == nil {
		t.Fatal("DetectDrift = nil, want active drift item for unknown runtime")
	}
	if item.HealthState != HealthUnknown {
		t.Fatalf("HealthState = %q, want %q", item.HealthState, HealthUnknown)
	}
	if !strings.Contains(item.Summary, "#468") || !strings.Contains(item.Summary, "unknown") {
		t.Fatalf("Summary = %q, want merged PR + unknown runtime", item.Summary)
	}
	if item.RecommendedAction != DriftActionCheckHealth || item.RequiresApproval {
		t.Fatalf("RecommendedAction/RequiresApproval = %q/%v, want read-only check action", item.RecommendedAction, item.RequiresApproval)
	}
	if len(item.MergedPRs) != 1 || item.MergedPRs[0].Number != 468 {
		t.Fatalf("MergedPRs = %+v, want PR #468", item.MergedPRs)
	}
}

func TestDetectDriftStaleHealthCheckTreatedAsUnknown(t *testing.T) {
	lastMerge := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	item := DetectDrift(DriftInput{
		Brief: Brief{
			DesiredOutcome: "App is live",
			HealthcheckURL: "https://app.example.com/healthz",
		},
		LastMergeAt: lastMerge,
		MergedPRs:   []DriftPR{{Number: 99, MergedAt: lastMerge}},
		Health: &HealthCheckResult{
			CheckedAt: lastMerge.Add(-time.Hour),
			State:     HealthHealthy,
		},
	})
	if item == nil {
		t.Fatal("DetectDrift = nil, want unknown drift when health check predates merge")
	}
	if item.HealthState != HealthUnknown {
		t.Fatalf("HealthState = %q, want %q", item.HealthState, HealthUnknown)
	}
}

func TestDetectDriftHealthyOutcomeReturnsNil(t *testing.T) {
	mergedAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	checkedAt := mergedAt.Add(time.Minute)
	item := DetectDrift(DriftInput{
		Brief: Brief{
			DesiredOutcome: "App is live",
			HealthcheckURL: "https://app.example.com/healthz",
		},
		LastMergeAt: mergedAt,
		MergedPRs:   []DriftPR{{Number: 470, MergedAt: mergedAt}},
		ClosedIssues: []DriftIssue{
			{Number: 353, ClosedAt: mergedAt},
		},
		Health: &HealthCheckResult{
			CheckedAt: checkedAt,
			Signal:    "healthcheck_url",
			State:     HealthHealthy,
			Summary:   "GET returned 200 OK",
		},
	})
	if item != nil {
		t.Fatalf("DetectDrift = %+v, want nil for healthy outcome", item)
	}
}

func TestDetectDriftNoActivityReturnsNil(t *testing.T) {
	item := DetectDrift(DriftInput{
		Brief: Brief{
			DesiredOutcome:     "App is live",
			HealthcheckCommand: "./status.sh",
		},
		Health: &HealthCheckResult{
			CheckedAt: time.Now(),
			State:     HealthFailing,
			Summary:   "still failing",
		},
	})
	if item != nil {
		t.Fatalf("DetectDrift = %+v, want nil when no PR was merged or issue closed", item)
	}
}

func TestDetectDriftMissingBriefReturnsNil(t *testing.T) {
	item := DetectDrift(DriftInput{
		Brief:        Brief{},
		ClosedIssues: []DriftIssue{{Number: 12, ClosedAt: time.Now()}},
		Health: &HealthCheckResult{
			CheckedAt: time.Now(),
			State:     HealthFailing,
		},
	})
	if item != nil {
		t.Fatalf("DetectDrift = %+v, want nil when no outcome brief is configured", item)
	}
}

func TestDetectDriftPolicyReopenRequiresApproval(t *testing.T) {
	closedAt := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	policy := DriftPolicy{
		ReopenIssue: true,
		Labels:      []string{"outcome-drift", "", "outcome-drift"},
	}
	item := DetectDrift(DriftInput{
		Brief: Brief{
			DesiredOutcome:     "App is live",
			HealthcheckCommand: "./status.sh",
		},
		ClosedIssues: []DriftIssue{{Number: 99, ClosedAt: closedAt}},
		Health: &HealthCheckResult{
			CheckedAt: closedAt.Add(time.Minute),
			State:     HealthFailing,
			Summary:   "verify failed",
		},
		Policy: policy,
	})
	if item == nil {
		t.Fatal("DetectDrift = nil, want drift item with reopen action")
	}
	if item.RecommendedAction != DriftActionRequestApproval || !item.RequiresApproval {
		t.Fatalf("recommended/approval = %q/%v, want approval-gated mutating action", item.RecommendedAction, item.RequiresApproval)
	}
	if len(item.Labels) != 1 || item.Labels[0] != "outcome-drift" {
		t.Fatalf("Labels = %#v, want compacted [outcome-drift]", item.Labels)
	}
}

func TestDetectDriftPolicyAutoApproveSkipsApproval(t *testing.T) {
	closedAt := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	item := DetectDrift(DriftInput{
		Brief: Brief{
			DesiredOutcome:     "App is live",
			HealthcheckCommand: "./status.sh",
		},
		ClosedIssues: []DriftIssue{{Number: 99, ClosedAt: closedAt}},
		Health: &HealthCheckResult{
			CheckedAt: closedAt.Add(time.Minute),
			State:     HealthFailing,
		},
		Policy: DriftPolicy{ReopenIssue: true, AutoApprove: true},
	})
	if item == nil {
		t.Fatal("DetectDrift = nil, want drift item")
	}
	if item.RecommendedAction != DriftActionReopenIssue || item.RequiresApproval {
		t.Fatalf("recommended/approval = %q/%v, want direct reopen without approval", item.RecommendedAction, item.RequiresApproval)
	}
}

func TestDetectDriftUnmonitoredSignal(t *testing.T) {
	mergedAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	item := DetectDrift(DriftInput{
		Brief: Brief{
			DesiredOutcome: "App is live",
		},
		LastMergeAt: mergedAt,
		MergedPRs:   []DriftPR{{Number: 200, MergedAt: mergedAt}},
	})
	if item == nil {
		t.Fatal("DetectDrift = nil, want drift item for unmonitored outcome")
	}
	if item.HealthState != HealthUnmonitored {
		t.Fatalf("HealthState = %q, want %q", item.HealthState, HealthUnmonitored)
	}
	if !strings.Contains(item.NextAction, "outcome health signal") {
		t.Fatalf("NextAction = %q, want unmonitored guidance", item.NextAction)
	}
}

func TestDetectDriftSortsMergedPRsRecentFirst(t *testing.T) {
	older := time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	item := DetectDrift(DriftInput{
		Brief: Brief{
			DesiredOutcome:     "App is live",
			HealthcheckCommand: "./status.sh",
		},
		LastMergeAt: newer,
		MergedPRs: []DriftPR{
			{Number: 1, MergedAt: older},
			{Number: 2, MergedAt: newer},
			{Number: 2, MergedAt: newer}, // duplicate
		},
		Health: &HealthCheckResult{
			CheckedAt: newer.Add(time.Minute),
			State:     HealthFailing,
		},
	})
	if item == nil {
		t.Fatal("DetectDrift = nil")
	}
	if len(item.MergedPRs) != 2 || item.MergedPRs[0].Number != 2 || item.MergedPRs[1].Number != 1 {
		t.Fatalf("MergedPRs = %+v, want newest first deduped", item.MergedPRs)
	}
}
