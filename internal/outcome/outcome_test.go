package outcome

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
			Name:       "source-main-ci",
			Blocking:   true,
			Status:     "pending",
			DeadlineAt: "2026-07-18T12:45:56Z",
		}},
	})

	if status.HealthState != HealthPending {
		t.Fatalf("HealthState = %q, want %q", status.HealthState, HealthPending)
	}
	if len(status.Checks) != 1 || status.Checks[0].Status != "pending" || status.Checks[0].DeadlineAt != "2026-07-18T12:45:56Z" {
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
		if got := r.Header.Get(HealthProbeHeader); got != HealthProbeHeaderValue {
			t.Errorf("%s = %q, want %q", HealthProbeHeader, got, HealthProbeHeaderValue)
		}
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

func TestCheckerProjectsStructuredHealthDeadline(t *testing.T) {
	output := []byte(`{"healthy":false,"deadline_at":"2026-07-18T09:35:00-04:00","checks":[{"name":"linux-candidate-delivery","blocking":true,"status":"fail"},{"name":"feed","blocking":false,"status":"warning","deadline":"2026-07-18T14:00:00Z"}]}`)
	result := Checker{
		RunCommand: func(context.Context, string, string) ([]byte, int, error) {
			return output, 0, nil
		},
	}.Check(context.Background(), Brief{DesiredOutcome: "candidate is fresh", HealthcheckCommand: "check"})
	if len(result.Checks) != 2 {
		t.Fatalf("projected checks=%+v", result.Checks)
	}
	if got := result.Checks[0]; got.Name != "linux-candidate-delivery" || got.DeadlineAt != "2026-07-18T13:35:00Z" {
		t.Fatalf("blocking deadline=%+v, want normalized envelope deadline", got)
	}
	if got := result.Checks[1]; got.DeadlineAt != "2026-07-18T14:00:00Z" {
		t.Fatalf("per-check deadline=%+v, want safe deadline alias", got)
	}
	if strings.Contains(result.Detail, "-04:00") {
		t.Fatalf("detail retained unnormalized deadline: %q", result.Detail)
	}
}

func TestCheckerDropsInvalidStructuredHealthDeadline(t *testing.T) {
	output := []byte(`{"healthy":false,"checks":[{"name":"candidate","blocking":true,"status":"fail","deadline_at":"not-a-timestamp"}]}`)
	result := Checker{
		RunCommand: func(context.Context, string, string) ([]byte, int, error) {
			return output, 0, nil
		},
	}.Check(context.Background(), Brief{DesiredOutcome: "candidate is fresh", HealthcheckCommand: "check"})
	if len(result.Checks) != 1 || result.Checks[0].DeadlineAt != "" {
		t.Fatalf("invalid deadline was not discarded: %+v", result.Checks)
	}
	if strings.Contains(result.Detail, "not-a-timestamp") {
		t.Fatalf("invalid deadline entered durable detail: %q", result.Detail)
	}
}

func TestCheckerBoundsStructuredHealthChecksWithoutHidingFailure(t *testing.T) {
	var output strings.Builder
	output.WriteString(`{"healthy":false,"checks":[`)
	for i := 0; i < maxStructuredHealthChecks+20; i++ {
		if i > 0 {
			output.WriteByte(',')
		}
		fmt.Fprintf(&output, `{"name":"pass-%02d","blocking":false,"status":"pass"}`, i)
	}
	output.WriteString(`,{"name":"candidate","blocking":true,"status":"fail","deadline_at":"2026-07-18T13:35:00Z"}]}`)

	result := Checker{
		RunCommand: func(context.Context, string, string) ([]byte, int, error) {
			return []byte(output.String()), 0, nil
		},
	}.Check(context.Background(), Brief{DesiredOutcome: "candidate is fresh", HealthcheckCommand: "check"})
	if len(result.Checks) != maxStructuredHealthChecks {
		t.Fatalf("checks=%d, want bounded %d", len(result.Checks), maxStructuredHealthChecks)
	}
	if got := result.Checks[0]; got.Name != "candidate" || got.Status != "fail" {
		t.Fatalf("blocking failure was crowded out by passing checks: %+v", result.Checks)
	}
}

func TestCheckerBoundsCommandOutput(t *testing.T) {
	result := Checker{}.Check(context.Background(), Brief{
		DesiredOutcome:     "candidate is fresh",
		HealthcheckCommand: fmt.Sprintf("head -c %d /dev/zero", maxCheckOutputBytes+1),
	})
	if result.State != HealthFailing || !strings.Contains(result.Summary, "safety limit") {
		t.Fatalf("oversized command output result=%+v, want bounded failure", result)
	}
	if result.Detail != "" || len(result.Checks) != 0 {
		t.Fatalf("oversized command output entered durable state: %+v", result)
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

func TestBriefEffectiveHealthcheckTimeout(t *testing.T) {
	if got := (Brief{}).EffectiveHealthcheckTimeout(); got != defaultCheckTimeout {
		t.Fatalf("default healthcheck timeout = %s, want %s", got, defaultCheckTimeout)
	}
	if got := (Brief{HealthcheckTimeoutSeconds: 60}).EffectiveHealthcheckTimeout(); got != 60*time.Second {
		t.Fatalf("configured healthcheck timeout = %s, want 1m0s", got)
	}
	if err := (Brief{HealthcheckTimeoutSeconds: -1}).Validate(); err == nil || !strings.Contains(err.Error(), "healthcheck_timeout_seconds") {
		t.Fatalf("negative healthcheck_timeout_seconds error = %v", err)
	}
}

func TestCheckerAppliesConfiguredHealthcheckTimeoutBudget(t *testing.T) {
	cases := []struct {
		name    string
		seconds int
		want    time.Duration
	}{
		{name: "default", seconds: 0, want: defaultCheckTimeout},
		{name: "configured", seconds: 60, want: 60 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var budget time.Duration
			result := Checker{
				RunCommand: func(ctx context.Context, command, dir string) ([]byte, int, error) {
					deadline, ok := ctx.Deadline()
					if !ok {
						t.Fatal("healthcheck command ran without a deadline")
					}
					budget = time.Until(deadline)
					return nil, 0, nil
				},
			}.Check(context.Background(), Brief{
				DesiredOutcome:            "App is live",
				HealthcheckCommand:        "status.sh",
				HealthcheckTimeoutSeconds: tc.seconds,
			})
			if result.State != HealthHealthy {
				t.Fatalf("State = %q, want %q", result.State, HealthHealthy)
			}
			// The deadline is set just before the command runs, so the observed
			// budget is the configured one minus scheduling slack.
			if budget > tc.want || budget < tc.want-2*time.Second {
				t.Fatalf("healthcheck budget = %s, want ~%s", budget, tc.want)
			}
		})
	}
}

func TestCheckerHealthcheckTimeoutKillsDescendantProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant-survived")
	result := Checker{}.Check(context.Background(), Brief{
		DesiredOutcome:            "App is live",
		HealthcheckCommand:        fmt.Sprintf("(sleep 2; touch %q) & wait", marker),
		HealthcheckTimeoutSeconds: 1,
	})
	if result.State != HealthFailing || !strings.Contains(result.Summary, "timed out after 1s") {
		t.Fatalf("result = %+v, want a 1s timeout failure", result)
	}

	time.Sleep(2200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("healthcheck descendant survived the timeout and produced its marker")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat marker: %v", err)
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

func TestBriefRecoveryMaxFutileAttemptsDefaultsAndValidates(t *testing.T) {
	if got := (Brief{}).EffectiveRecoveryMaxFutileAttempts(); got != 3 {
		t.Fatalf("default recovery_max_futile_attempts = %d, want 3", got)
	}
	if got := (Brief{RecoveryMaxFutileAttempts: 5}).EffectiveRecoveryMaxFutileAttempts(); got != 5 {
		t.Fatalf("configured recovery_max_futile_attempts = %d, want 5", got)
	}
	if err := (Brief{RecoveryMaxFutileAttempts: -1}).Validate(); err == nil || !strings.Contains(err.Error(), "recovery_max_futile_attempts") {
		t.Fatalf("negative recovery_max_futile_attempts error = %v", err)
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
