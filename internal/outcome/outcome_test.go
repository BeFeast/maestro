package outcome

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestBriefSupportsOutcomeControlledDeliveryAliases(t *testing.T) {
	passRequired := true
	failRequiresWork := true
	brief := Brief{
		DesiredOutcome:          "Accepted runtime route matches the design artifact",
		RuntimeURL:              "https://app.example.com",
		VerifierCommand:         "./verify-live.sh",
		RequiredRoutes:          []string{"/", "/", " /settings "},
		RequiresDeploy:          true,
		PassRequiredForDone:     &passRequired,
		FailRequiresVisibleWork: &failRequiresWork,
	}

	normalized := brief.Normalized()
	if normalized.RuntimeTarget != "https://app.example.com" || normalized.RuntimeURL != "https://app.example.com" {
		t.Fatalf("runtime aliases = %q/%q, want both populated", normalized.RuntimeTarget, normalized.RuntimeURL)
	}
	if normalized.HealthcheckCommand != "./verify-live.sh" || normalized.VerifierCommand != "./verify-live.sh" {
		t.Fatalf("verifier aliases = %q/%q, want both populated", normalized.HealthcheckCommand, normalized.VerifierCommand)
	}

	status := StatusFor(brief, 0, time.Time{})
	if status.RuntimeTarget != "https://app.example.com" || status.RuntimeURL != "https://app.example.com" {
		t.Fatalf("status runtime aliases = %+v, want runtime target/url", status)
	}
	if status.HealthcheckCommand != "./verify-live.sh" || status.VerifierCommand != "./verify-live.sh" {
		t.Fatalf("status verifier aliases = %+v, want command aliases", status)
	}
	if !status.RequiresDeploy || !status.PassRequiredForDone || !status.FailRequiresVisibleWork {
		t.Fatalf("status policy flags = %+v, want enabled", status)
	}
	if len(status.RequiredRoutes) != 2 || status.RequiredRoutes[0] != "/" || status.RequiredRoutes[1] != "/settings" {
		t.Fatalf("RequiredRoutes = %#v, want compacted routes", status.RequiredRoutes)
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

func TestStatusForPersistsPendingHealthCheck(t *testing.T) {
	checkedAt := time.Date(2026, 7, 18, 12, 15, 56, 0, time.UTC)
	status := StatusFor(Brief{
		DesiredOutcome:     "Merged main passes required CI",
		HealthcheckCommand: "check-main-ci",
	}, 1, checkedAt.Add(-time.Minute), HealthCheckResult{
		CheckedAt: checkedAt,
		Signal:    "healthcheck_command",
		State:     HealthPending,
		Summary:   "source-main-ci reported pending",
		Checks: []HealthCheckItem{{
			Name:     "source-main-ci",
			Blocking: true,
			Status:   "pending",
		}},
	})

	if status.HealthState != HealthPending {
		t.Fatalf("HealthState = %q, want %q", status.HealthState, HealthPending)
	}
	if len(status.Checks) != 1 || status.Checks[0].Status != "pending" {
		t.Fatalf("Checks = %+v, want persisted pending check", status.Checks)
	}
	if strings.Contains(status.NextAction, "before dispatching") {
		t.Fatalf("NextAction = %q, pending must not block dispatch", status.NextAction)
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
	if result.State != HealthFailing || result.ExitCode != 7 || result.Detail != "" {
		t.Fatalf("result = %+v, want failing command result", result)
	}
}

func TestCheckerProjectsStructuredHealthWithoutRawDetails(t *testing.T) {
	output := []byte(`{"healthy":false,"checks":[{"name":"candidate","blocking":true,"status":"fail","summary":"stale; api_key=do-not-store","details":["token=also-secret"]}],"unknown":"discard"}`)
	result := Checker{
		RunCommand: func(context.Context, string, string) ([]byte, int, error) {
			return output, 1, context.DeadlineExceeded
		},
	}.Check(context.Background(), Brief{DesiredOutcome: "candidate is fresh", HealthcheckCommand: "check"})
	if len(result.Checks) != 1 || result.Checks[0].Name != "candidate" || result.Checks[0].Status != "fail" {
		t.Fatalf("projected checks=%+v", result.Checks)
	}
	if strings.Contains(result.Summary, "do-not-store") || strings.Contains(result.Detail, "also-secret") || strings.Contains(result.Detail, "unknown") || strings.Contains(result.Detail, "do-not-store") {
		t.Fatalf("raw structured output leaked into detail: %q", result.Detail)
	}
}

func TestCheckerStructuredHealthRejectsFreeFormFields(t *testing.T) {
	output := []byte(`{"healthy":false,"summary":"bearer top-secret","checks":[{"name":"candidate token=secret","blocking":true,"status":"secret-status","summary":"password: top-secret"}]}`)
	result := Checker{
		RunCommand: func(context.Context, string, string) ([]byte, int, error) {
			return output, 0, nil
		},
	}.Check(context.Background(), Brief{DesiredOutcome: "candidate is fresh", HealthcheckCommand: "check"})
	if len(result.Checks) != 1 || result.Checks[0].Name != "structured-check" || result.Checks[0].Status != "unknown" {
		t.Fatalf("unsafe structured fields were not projected: %+v", result.Checks)
	}
	persisted := result.Summary + result.Detail
	if strings.Contains(persisted, "top-secret") || strings.Contains(persisted, "token=secret") || strings.Contains(persisted, "secret-status") {
		t.Fatalf("free-form structured output entered durable result: %q", persisted)
	}
}

func TestBriefAutomaticRecoveryRequiresDesiredOutcome(t *testing.T) {
	brief := Brief{HealthcheckCommand: "check", RecoveryMode: RecoveryModeAutomatic, RecoveryCommand: "recover"}
	if err := brief.Validate(); err == nil || !strings.Contains(err.Error(), "desired_outcome") {
		t.Fatalf("Validate() error = %v, want desired_outcome requirement", err)
	}
	if brief.AutomaticRecoveryEnabled() {
		t.Fatal("AutomaticRecoveryEnabled = true without desired_outcome")
	}
}

func TestCheckerHonorsStructuredUnhealthyWhenCommandExitsZero(t *testing.T) {
	result := Checker{
		RunCommand: func(context.Context, string, string) ([]byte, int, error) {
			return []byte(`{"healthy":false,"checks":[{"name":"candidate","blocking":true,"status":"fail","summary":"stale"}]}`), 0, nil
		},
	}.Check(context.Background(), Brief{DesiredOutcome: "candidate is fresh", HealthcheckCommand: "check"})
	if result.State != HealthFailing || result.ExitCode != 0 || len(result.Checks) != 1 {
		t.Fatalf("structured unhealthy result=%+v", result)
	}
}

func TestCheckerTreatsBlockingStructuredPendingAsTransitional(t *testing.T) {
	result := Checker{
		RunCommand: func(context.Context, string, string) ([]byte, int, error) {
			return []byte(`{"healthy":false,"checks":[{"name":"source-main-ci","blocking":true,"status":"in_progress"}]}`), 1, context.DeadlineExceeded
		},
	}.Check(context.Background(), Brief{DesiredOutcome: "main is healthy", HealthcheckCommand: "check"})

	if result.State != HealthPending || result.ExitCode != 1 {
		t.Fatalf("structured pending result=%+v, want pending", result)
	}
	if len(result.Checks) != 1 || result.Checks[0].Status != "in_progress" {
		t.Fatalf("Checks = %+v, want projected in-progress check", result.Checks)
	}
}

func TestCheckerTreatsBlockingStructuredFailureAsFailing(t *testing.T) {
	result := Checker{
		RunCommand: func(context.Context, string, string) ([]byte, int, error) {
			return []byte(`{"healthy":true,"checks":[{"name":"source-main-ci","blocking":true,"status":"error"},{"name":"deploy","blocking":true,"status":"queued"}]}`), 0, nil
		},
	}.Check(context.Background(), Brief{DesiredOutcome: "main is healthy", HealthcheckCommand: "check"})

	if result.State != HealthFailing {
		t.Fatalf("State = %q, want %q: %+v", result.State, HealthFailing, result)
	}
}

func TestCheckerTreatsConcludedStructuredSuccessAsHealthy(t *testing.T) {
	result := Checker{
		RunCommand: func(context.Context, string, string) ([]byte, int, error) {
			return []byte(`{"healthy":true,"checks":[{"name":"source-main-ci","blocking":true,"status":"pass"}]}`), 0, nil
		},
	}.Check(context.Background(), Brief{DesiredOutcome: "main is healthy", HealthcheckCommand: "check"})

	if result.State != HealthHealthy {
		t.Fatalf("State = %q, want %q: %+v", result.State, HealthHealthy, result)
	}
}
