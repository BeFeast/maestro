package outcome

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
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

func TestCheckerCommandUsesBriefTimeout(t *testing.T) {
	var gotTimeout time.Duration
	result := Checker{
		RunCommand: func(ctx context.Context, command, dir string) ([]byte, int, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("command context has no deadline")
			}
			gotTimeout = time.Until(deadline)
			return []byte("ok"), 0, nil
		},
	}.Check(context.Background(), Brief{
		DesiredOutcome:            "App is live",
		HealthcheckCommand:        "status.sh",
		HealthcheckTimeoutSeconds: 42,
	})
	if result.State != HealthHealthy {
		t.Fatalf("result = %+v, want healthy", result)
	}
	if gotTimeout < 41*time.Second || gotTimeout > 42*time.Second {
		t.Fatalf("command deadline = %s, want about 42s", gotTimeout)
	}
}

func TestCheckerCommandSerializesConcurrentChecks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var mu sync.Mutex
	active := 0
	maxActive := 0
	enteredFirst := make(chan struct{})
	releaseFirst := make(chan struct{})
	var first sync.Once

	checker := Checker{
		RunCommand: func(ctx context.Context, command, dir string) ([]byte, int, error) {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()

			first.Do(func() {
				close(enteredFirst)
				select {
				case <-releaseFirst:
				case <-ctx.Done():
				}
			})

			time.Sleep(10 * time.Millisecond)

			mu.Lock()
			active--
			mu.Unlock()
			return []byte("ok"), 0, nil
		},
	}
	brief := Brief{
		DesiredOutcome:            "App is live",
		HealthcheckCommand:        "verify.sh",
		HealthcheckTimeoutSeconds: 5,
		SourceRepoPath:            t.TempDir(),
	}

	results := make(chan HealthCheckResult, 2)
	go func() { results <- checker.Check(ctx, brief) }()

	select {
	case <-enteredFirst:
	case <-ctx.Done():
		t.Fatal("first check did not enter command")
	}
	go func() { results <- checker.Check(ctx, brief) }()

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if maxActive != 1 || active != 1 {
		t.Fatalf("concurrent command active=%d maxActive=%d, want one active command", active, maxActive)
	}
	mu.Unlock()
	close(releaseFirst)

	for range 2 {
		select {
		case result := <-results:
			if result.State != HealthHealthy {
				t.Fatalf("result = %+v, want healthy", result)
			}
		case <-ctx.Done():
			t.Fatal("checks did not finish")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if maxActive != 1 {
		t.Fatalf("maxActive = %d, want serialized checks", maxActive)
	}
}
