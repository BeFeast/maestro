package outcome

import (
	"strings"
	"testing"
	"time"
)

func TestDetectDriftClosedIssueWithFailingRuntime(t *testing.T) {
	finished := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	drifts := DetectDrift(
		Brief{DesiredOutcome: "App is live", HealthcheckURL: "https://app.example.com/healthz"},
		[]CompletedWork{{Session: "w1", ClosedIssue: 42, FinishedAt: finished}},
		&HealthCheckResult{CheckedAt: finished.Add(time.Minute), State: HealthFailing, Signal: "healthcheck_url"},
	)
	if len(drifts) != 1 {
		t.Fatalf("len(drifts) = %d, want 1", len(drifts))
	}
	d := drifts[0]
	if d.ClosedIssue != 42 || d.MergedPR != 0 {
		t.Fatalf("drift target = %+v, want closed issue 42 only", d)
	}
	if d.HealthState != HealthFailing {
		t.Fatalf("HealthState = %q, want %q", d.HealthState, HealthFailing)
	}
	if d.Signal != "healthcheck_url" {
		t.Fatalf("Signal = %q, want healthcheck_url", d.Signal)
	}
	if !strings.Contains(d.Summary, "Issue #42") || !strings.Contains(d.Summary, "App is live") {
		t.Fatalf("Summary = %q, want issue number and goal", d.Summary)
	}
	if d.NextAction == "" {
		t.Fatalf("NextAction is empty, want a recommendation")
	}
}

func TestDetectDriftMergedPRWithUnknownRuntime(t *testing.T) {
	finished := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	drifts := DetectDrift(
		Brief{DesiredOutcome: "App is live", HealthcheckCommand: "status.sh"},
		[]CompletedWork{{Session: "w1", MergedPR: 99, ClosedIssue: 42, FinishedAt: finished}},
		nil,
	)
	if len(drifts) != 1 {
		t.Fatalf("len(drifts) = %d, want 1", len(drifts))
	}
	d := drifts[0]
	if d.HealthState != HealthUnknown {
		t.Fatalf("HealthState = %q, want %q", d.HealthState, HealthUnknown)
	}
	if d.MergedPR != 99 || d.ClosedIssue != 42 {
		t.Fatalf("drift target = %+v, want merged PR 99 and closed issue 42", d)
	}
	if d.Signal != "healthcheck_command" {
		t.Fatalf("Signal = %q, want healthcheck_command", d.Signal)
	}
	if !strings.Contains(d.Summary, "PR #99") || !strings.Contains(d.Summary, "issue #42") {
		t.Fatalf("Summary = %q, want PR and issue numbers", d.Summary)
	}
	if !strings.Contains(d.NextAction, "healthcheck") {
		t.Fatalf("NextAction = %q, want healthcheck guidance", d.NextAction)
	}
}

func TestDetectDriftClearedByHealthyCheckAfterFinish(t *testing.T) {
	finished := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	drifts := DetectDrift(
		Brief{DesiredOutcome: "App is live", HealthcheckURL: "https://app.example.com/healthz"},
		[]CompletedWork{
			{Session: "w1", MergedPR: 99, ClosedIssue: 42, FinishedAt: finished},
			{Session: "w2", MergedPR: 100, ClosedIssue: 43, FinishedAt: finished.Add(-time.Minute)},
		},
		&HealthCheckResult{CheckedAt: finished.Add(2 * time.Minute), State: HealthHealthy, Signal: "healthcheck_url"},
	)
	if len(drifts) != 0 {
		t.Fatalf("len(drifts) = %d, want 0 (healthy check after both finish times clears drift)", len(drifts))
	}
}

func TestDetectDriftStaleHealthyCheckCountsAsUnknown(t *testing.T) {
	finished := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	drifts := DetectDrift(
		Brief{DesiredOutcome: "App is live", HealthcheckURL: "https://app.example.com/healthz"},
		[]CompletedWork{{Session: "w1", MergedPR: 99, ClosedIssue: 42, FinishedAt: finished}},
		&HealthCheckResult{CheckedAt: finished.Add(-time.Hour), State: HealthHealthy, Signal: "healthcheck_url"},
	)
	if len(drifts) != 1 {
		t.Fatalf("len(drifts) = %d, want 1 (healthy check before finish should not clear drift)", len(drifts))
	}
	if drifts[0].HealthState != HealthUnknown {
		t.Fatalf("HealthState = %q, want %q for stale healthy check", drifts[0].HealthState, HealthUnknown)
	}
}

func TestDetectDriftUnmonitoredBrief(t *testing.T) {
	drifts := DetectDrift(
		Brief{DesiredOutcome: "App is live"},
		[]CompletedWork{{Session: "w1", MergedPR: 99, ClosedIssue: 42}},
		nil,
	)
	if len(drifts) != 1 {
		t.Fatalf("len(drifts) = %d, want 1", len(drifts))
	}
	if drifts[0].HealthState != HealthUnmonitored {
		t.Fatalf("HealthState = %q, want %q", drifts[0].HealthState, HealthUnmonitored)
	}
	if drifts[0].Signal != "" {
		t.Fatalf("Signal = %q, want empty for unmonitored brief", drifts[0].Signal)
	}
	if !strings.Contains(drifts[0].NextAction, "Configure") {
		t.Fatalf("NextAction = %q, want configure guidance", drifts[0].NextAction)
	}
}

func TestDetectDriftMissingBriefReturnsNil(t *testing.T) {
	drifts := DetectDrift(
		Brief{},
		[]CompletedWork{{Session: "w1", MergedPR: 99}},
		&HealthCheckResult{State: HealthFailing},
	)
	if len(drifts) != 0 {
		t.Fatalf("len(drifts) = %d, want 0 (missing brief is a separate stuck state)", len(drifts))
	}
}

func TestDetectDriftIgnoresRecordsWithoutTarget(t *testing.T) {
	drifts := DetectDrift(
		Brief{DesiredOutcome: "App is live", HealthcheckURL: "https://app.example.com/healthz"},
		[]CompletedWork{{Session: "w1"}},
		&HealthCheckResult{State: HealthFailing},
	)
	if len(drifts) != 0 {
		t.Fatalf("len(drifts) = %d, want 0 for record with no PR or issue", len(drifts))
	}
}

func TestDetectDriftSortsByTargetForStableUI(t *testing.T) {
	drifts := DetectDrift(
		Brief{DesiredOutcome: "App is live", HealthcheckURL: "https://app.example.com/healthz"},
		[]CompletedWork{
			{Session: "b", MergedPR: 20, ClosedIssue: 200},
			{Session: "a", MergedPR: 10, ClosedIssue: 100},
		},
		nil,
	)
	if len(drifts) != 2 {
		t.Fatalf("len(drifts) = %d, want 2", len(drifts))
	}
	if drifts[0].MergedPR != 10 || drifts[1].MergedPR != 20 {
		t.Fatalf("drifts = %+v, want sorted by merged PR ascending", drifts)
	}
}
