package outcome

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func TestStatusForRequiresDesiredOutcome(t *testing.T) {
	brief := Brief{
		RuntimeTarget:           "https://app.example.com",
		DeploymentStatusCommand: "systemctl status app",
		SourceRepoPath:          "/srv/app",
		RuntimeHost:             "app-host",
		NonGoals:                []string{"Rewrite"},
	}
	if brief.Configured() {
		t.Fatal("Configured = true, want false without desired_outcome")
	}
	status := StatusFor(brief, 2, time.Time{})
	if status.Configured || status.HealthState != HealthNotConfigured {
		t.Fatalf("status = %+v, want unconfigured outcome", status)
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

func TestStatusForUsesFreshHealthCheck(t *testing.T) {
	lastMerge := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	status := StatusFor(Brief{
		DesiredOutcome: "App is live",
		HealthcheckURL: "https://app.example.com/healthz",
	}, 2, lastMerge, HealthCheckResult{
		CheckedAt: lastMerge.Add(time.Minute),
		Signal:    "healthcheck_url",
		State:     HealthHealthy,
		Summary:   "GET returned 200 OK",
	})
	if status.HealthState != HealthHealthy {
		t.Fatalf("HealthState = %q, want %q", status.HealthState, HealthHealthy)
	}
	if status.HealthCheckedAt == "" || status.HealthSignal != "healthcheck_url" || status.HealthSummary == "" {
		t.Fatalf("health metadata = %+v, want persisted check metadata", status)
	}
	if status.NextAction == "" {
		t.Fatal("NextAction should explain healthy outcome")
	}
}

func TestStatusForIgnoresHealthCheckBeforeLastMerge(t *testing.T) {
	lastMerge := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	status := StatusFor(Brief{
		DesiredOutcome: "App is live",
		HealthcheckURL: "https://app.example.com/healthz",
	}, 2, lastMerge, HealthCheckResult{
		CheckedAt: lastMerge.Add(-time.Minute),
		State:     HealthHealthy,
	})
	if status.HealthState != HealthUnknown {
		t.Fatalf("HealthState = %q, want %q for stale check", status.HealthState, HealthUnknown)
	}
}

func TestCheckerHealthcheckURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	result := Checker{}.Check(context.Background(), Brief{
		DesiredOutcome: "App is live",
		HealthcheckURL: server.URL,
	})
	if result.State != HealthHealthy {
		t.Fatalf("State = %q, want %q: %+v", result.State, HealthHealthy, result)
	}
	if result.Signal != "healthcheck_url" || result.Summary == "" {
		t.Fatalf("result = %+v, want URL signal summary", result)
	}
}

func TestCheckerCommandFailure(t *testing.T) {
	result := Checker{
		RunCommand: func(ctx context.Context, command, dir string) ([]byte, int, error) {
			return []byte("not healthy"), 7, context.DeadlineExceeded
		},
	}.Check(context.Background(), Brief{
		DesiredOutcome:     "App is live",
		HealthcheckCommand: "status.sh",
	})
	if result.State != HealthFailing || result.ExitCode != 7 || result.Detail != "not healthy" {
		t.Fatalf("result = %+v, want failing command result", result)
	}
}
