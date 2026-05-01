package outcome

import (
	"testing"
	"time"
)

func TestStatusForMissingBrief(t *testing.T) {
	status := StatusFor(Brief{}, 0, time.Time{})
	if status.Configured {
		t.Fatal("Configured = true, want false")
	}
	if status.HealthState != HealthNotConfigured {
		t.Fatalf("HealthState = %q, want %q", status.HealthState, HealthNotConfigured)
	}
	if status.NextAction == "" {
		t.Fatal("NextAction should explain how to add outcome context")
	}
}

func TestStatusForConfiguredBriefUnknownHealth(t *testing.T) {
	lastMerge := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	status := StatusFor(Brief{
		DesiredOutcome:          "App is live",
		RuntimeTarget:           "https://app.example.com",
		DeploymentStatusCommand: "systemctl status app",
		NonGoals:                []string{"Rewrite", "Rewrite", ""},
	}, 2, lastMerge)
	if !status.Configured {
		t.Fatal("Configured = false, want true")
	}
	if status.Goal != "App is live" || status.RuntimeTarget != "https://app.example.com" {
		t.Fatalf("status = %+v, want goal and runtime target", status)
	}
	if status.HealthState != HealthUnknown {
		t.Fatalf("HealthState = %q, want %q", status.HealthState, HealthUnknown)
	}
	if status.MergedPRs != 2 || status.LastMergeAt == "" {
		t.Fatalf("merge metadata = %d/%q, want populated", status.MergedPRs, status.LastMergeAt)
	}
	if len(status.NonGoals) != 1 || status.NonGoals[0] != "Rewrite" {
		t.Fatalf("NonGoals = %#v, want compacted", status.NonGoals)
	}
}
