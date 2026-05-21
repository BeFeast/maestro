package outcome

import (
	"testing"
	"time"
)

func TestDetectDriftsClosedIssueWithFailingRuntime(t *testing.T) {
	landed := time.Date(2026, 5, 10, 9, 0, 0, 0, time.UTC)
	brief := Brief{
		DesiredOutcome:     "App is live",
		HealthcheckCommand: "status.sh",
	}
	sessions := []DriftSession{{
		Slot:        "slot-1",
		IssueNumber: 99,
		IssueTitle:  "Ship feature",
		IssueClosed: true,
		LandedAt:    landed,
	}}
	check := &HealthCheckResult{
		CheckedAt: landed.Add(time.Minute),
		Signal:    "healthcheck_command",
		State:     HealthFailing,
		Summary:   "status.sh exited 1",
	}

	drifts := DetectDrifts(brief, sessions, check, landed.Add(2*time.Minute))
	if len(drifts) != 1 {
		t.Fatalf("len(drifts) = %d, want 1: %+v", len(drifts), drifts)
	}
	drift := drifts[0]
	if drift.HealthState != HealthFailing {
		t.Fatalf("HealthState = %q, want %q", drift.HealthState, HealthFailing)
	}
	if !drift.IssueClosed || drift.IssueNumber != 99 {
		t.Fatalf("drift = %+v, want closed issue #99", drift)
	}
	if drift.HealthSignal != "healthcheck_command" || drift.HealthSummary == "" {
		t.Fatalf("drift = %+v, want runtime signal/summary populated", drift)
	}
	if drift.ActionPolicy != DriftActionPolicyApprovalRequired {
		t.Fatalf("ActionPolicy = %q, want %q", drift.ActionPolicy, DriftActionPolicyApprovalRequired)
	}
	if drift.RecommendedAction == "" {
		t.Fatal("RecommendedAction should describe a follow-up")
	}
	if drift.Reason == "" || !contains(drift.Reason, "failing") {
		t.Fatalf("Reason = %q, want failing runtime reason", drift.Reason)
	}
}

func TestDetectDriftsMergedPRWithUnknownRuntime(t *testing.T) {
	landed := time.Date(2026, 5, 11, 8, 0, 0, 0, time.UTC)
	brief := Brief{
		DesiredOutcome: "App is live",
		HealthcheckURL: "https://app.example.com/healthz",
	}
	sessions := []DriftSession{{
		Slot:        "slot-2",
		IssueNumber: 101,
		PRNumber:    42,
		PRMerged:    true,
		LandedAt:    landed,
	}}

	drifts := DetectDrifts(brief, sessions, nil, landed.Add(time.Hour))
	if len(drifts) != 1 {
		t.Fatalf("len(drifts) = %d, want 1: %+v", len(drifts), drifts)
	}
	drift := drifts[0]
	if drift.HealthState != HealthUnknown {
		t.Fatalf("HealthState = %q, want %q", drift.HealthState, HealthUnknown)
	}
	if drift.PRNumber != 42 || !drift.PRMerged {
		t.Fatalf("drift = %+v, want merged PR #42", drift)
	}
	if drift.IssueNumber != 101 {
		t.Fatalf("drift.IssueNumber = %d, want 101", drift.IssueNumber)
	}
	if drift.ActionPolicy != DriftActionPolicyApprovalRequired {
		t.Fatalf("ActionPolicy = %q, want %q", drift.ActionPolicy, DriftActionPolicyApprovalRequired)
	}
	if drift.RecommendedAction == "" || !contains(drift.RecommendedAction, "PR #42") {
		t.Fatalf("RecommendedAction = %q, want to mention PR #42", drift.RecommendedAction)
	}
	if drift.Reason == "" || !contains(drift.Reason, "unknown") {
		t.Fatalf("Reason = %q, want unknown runtime reason", drift.Reason)
	}
}

func TestDetectDriftsHealthyOutcomeReturnsNoDrifts(t *testing.T) {
	landed := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	brief := Brief{
		DesiredOutcome: "App is live",
		HealthcheckURL: "https://app.example.com/healthz",
	}
	sessions := []DriftSession{{
		Slot:        "slot-3",
		IssueNumber: 5,
		PRNumber:    7,
		PRMerged:    true,
		IssueClosed: true,
		LandedAt:    landed,
	}}
	check := &HealthCheckResult{
		CheckedAt: landed.Add(time.Minute),
		Signal:    "healthcheck_url",
		State:     HealthHealthy,
		Summary:   "GET /healthz returned 200 OK",
	}

	drifts := DetectDrifts(brief, sessions, check, landed.Add(5*time.Minute))
	if len(drifts) != 0 {
		t.Fatalf("len(drifts) = %d, want 0 for healthy outcome: %+v", len(drifts), drifts)
	}
}

func TestDetectDriftsStaleHealthCheckBeforeLandedIsUnknownDrift(t *testing.T) {
	landed := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	brief := Brief{
		DesiredOutcome: "App is live",
		HealthcheckURL: "https://app.example.com/healthz",
	}
	sessions := []DriftSession{{
		Slot:        "slot-4",
		IssueNumber: 8,
		PRNumber:    9,
		PRMerged:    true,
		LandedAt:    landed,
	}}
	check := &HealthCheckResult{
		CheckedAt: landed.Add(-time.Hour),
		Signal:    "healthcheck_url",
		State:     HealthHealthy,
		Summary:   "GET /healthz returned 200 OK",
	}

	drifts := DetectDrifts(brief, sessions, check, landed.Add(time.Minute))
	if len(drifts) != 1 {
		t.Fatalf("len(drifts) = %d, want 1: %+v", len(drifts), drifts)
	}
	if drifts[0].HealthState != HealthUnknown {
		t.Fatalf("HealthState = %q, want %q for stale check", drifts[0].HealthState, HealthUnknown)
	}
	if drifts[0].ActionPolicy != DriftActionPolicyApprovalRequired {
		t.Fatalf("ActionPolicy = %q, want approval-required for stale check", drifts[0].ActionPolicy)
	}
}

func TestDetectDriftsMissingHealthSignalIsReadOnly(t *testing.T) {
	landed := time.Date(2026, 5, 14, 14, 0, 0, 0, time.UTC)
	brief := Brief{DesiredOutcome: "App is live"} // no health signal configured
	sessions := []DriftSession{{
		Slot:        "slot-5",
		IssueNumber: 12,
		IssueClosed: true,
		LandedAt:    landed,
	}}

	drifts := DetectDrifts(brief, sessions, nil, landed)
	if len(drifts) != 1 {
		t.Fatalf("len(drifts) = %d, want 1: %+v", len(drifts), drifts)
	}
	if drifts[0].HealthState != HealthUnmonitored {
		t.Fatalf("HealthState = %q, want %q", drifts[0].HealthState, HealthUnmonitored)
	}
	if drifts[0].ActionPolicy != DriftActionPolicyReadOnly {
		t.Fatalf("ActionPolicy = %q, want read-only", drifts[0].ActionPolicy)
	}
}

func TestDetectDriftsIgnoresWorkNotLanded(t *testing.T) {
	landed := time.Date(2026, 5, 15, 16, 0, 0, 0, time.UTC)
	brief := Brief{
		DesiredOutcome: "App is live",
		HealthcheckURL: "https://app.example.com/healthz",
	}
	sessions := []DriftSession{
		{Slot: "slot-running", IssueNumber: 20, PRNumber: 21, PRMerged: false, IssueClosed: false, LandedAt: landed},
		{Slot: "slot-merged", IssueNumber: 22, PRNumber: 23, PRMerged: true, LandedAt: landed},
	}

	drifts := DetectDrifts(brief, sessions, nil, landed.Add(time.Hour))
	if len(drifts) != 1 {
		t.Fatalf("len(drifts) = %d, want 1 (only the merged PR): %+v", len(drifts), drifts)
	}
	if drifts[0].Slot != "slot-merged" {
		t.Fatalf("slot = %q, want %q", drifts[0].Slot, "slot-merged")
	}
}

func TestDetectDriftsRequiresConfiguredBrief(t *testing.T) {
	landed := time.Date(2026, 5, 16, 18, 0, 0, 0, time.UTC)
	sessions := []DriftSession{{IssueNumber: 1, PRNumber: 2, PRMerged: true, LandedAt: landed}}

	drifts := DetectDrifts(Brief{}, sessions, nil, landed)
	if drifts != nil {
		t.Fatalf("DetectDrifts without configured brief = %+v, want nil", drifts)
	}
}

func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
