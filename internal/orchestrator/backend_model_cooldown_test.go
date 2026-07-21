package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/state"
)

func credentialAwareFallbackConfig() *config.Config {
	return &config.Config{
		Repo: "owner/repo",
		Model: config.ModelConfig{
			Default:          "fable",
			FallbackBackends: []string{"codex"},
			Backends: map[string]config.BackendDef{
				"fable":  {Cmd: "claude --model claude-fable-5"},
				"sonnet": {Cmd: "claude --model claude-sonnet-4-6"},
				"codex":  {Cmd: "codex --model gpt-5.5"},
			},
		},
	}
}

func TestReconcileRunningSessions_ModelCredentialPoolExhausted_GatesOnlyRoute(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	logFile := filepath.Join(dir, "worker.log")
	payload := `{"error":{"code":"model_cooldown","message":"All credentials for model claude-fable-5 are cooling down via provider fable","model":"claude-fable-5","provider":"fable","candidate_count":2,"usable_count":0,"aggregate_reason":"all_model_credentials_cooling_down","reset_seconds":90}}`
	if err := os.WriteFile(logFile, []byte(payload+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := state.NewState()
	s.Sessions["sup-908"] = &state.Session{
		IssueNumber: 908,
		IssueTitle:  "credential-aware fallback",
		Status:      state.StatusRunning,
		PID:         515151,
		TmuxSession: "maestro-sup-908",
		Backend:     "fable",
		StartedAt:   now.Add(-time.Minute),
		LogFile:     logFile,
	}

	var respawned string
	o := &Orchestrator{
		cfg:                       credentialAwareFallbackConfig(),
		notifier:                  &notify.Notifier{},
		pidAliveFn:                func(int) bool { return false },
		tmuxSessionExistsFn:       func(string) bool { return false },
		listOpenPRsFn:             func() ([]github.PR, error) { return nil, nil },
		isRateLimitedFn:           func(string) bool { return false },
		authFailureFromLogFn:      func(string) (bool, string) { return false, "" },
		modelUnavailableFromLogFn: func(string) (bool, string) { return false, "" },
		usageLimitFromLogFn:       func(string, []string) (bool, string) { return false, "" },
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "credential-aware fallback"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawned = backendName
			sess.Backend = backendName
			sess.Status = state.StatusRunning
			sess.PID = 9191
			sess.StartedAt = time.Now().UTC()
			sess.FinishedAt = nil
			return nil
		},
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected model cooldown reconciliation")
	}
	if respawned != "codex" {
		t.Fatalf("respawned backend = %q, want codex after both Fable credentials were unavailable", respawned)
	}
	if _, ok := s.BackendHealth["fable"]; ok {
		t.Fatalf("model cooldown must not gate the whole backend: %+v", s.BackendHealth["fable"])
	}
	health, ok := s.ProviderModelHealth["fable"]["claude-fable-5"]
	if !ok {
		t.Fatalf("provider/model health missing: %+v", s.ProviderModelHealth)
	}
	if health.Reason != state.BackendBlockModelCooldown || !health.CredentialCandidatesKnown || health.CredentialCandidates != 2 || !health.CredentialUsableKnown || health.CredentialUsable != 0 {
		t.Fatalf("route health = %+v, want model_cooldown with 2 candidates / 0 usable", health)
	}
	if health.RetryAfter == nil || !health.RetryAfter.After(now) {
		t.Fatalf("route retry = %v, want proxy retry time", health.RetryAfter)
	}
	sess := s.Sessions["sup-908"]
	if sess.ProviderLimitReason != state.BackendBlockModelCooldown || sess.ProviderLimitModel != "claude-fable-5" {
		t.Fatalf("session aggregate = %+v", sess)
	}
	failed := s.FailedAttemptsForIssue(908)
	if sess.RetryCount != 0 || failed != 0 {
		t.Fatalf("model credential cooldown burned retry budget: retry=%d failed=%d", sess.RetryCount, failed)
	}
}

func TestResolveDispatchBackend_ModelCooldownLeavesOtherProviderModelEligible(t *testing.T) {
	now := time.Now().UTC()
	retry := now.Add(10 * time.Minute)
	s := state.NewState()
	s.ProviderModelHealth["fable"] = map[string]state.BackendHealth{
		"claude-fable-5": {
			State:      state.BackendHealthCooldown,
			Reason:     state.BackendBlockModelCooldown,
			RetryAfter: &retry,
		},
	}
	o := &Orchestrator{cfg: credentialAwareFallbackConfig()}
	o.router = router.New(o.cfg)

	fableIssue := makeIssue(908, "Fable route", "model:fable")
	decision, ok, _ := o.resolveDispatchBackend(s, fableIssue, now)
	if !ok || decision.Backend != "codex" {
		t.Fatalf("Fable decision = %+v ok=%v, want precise fallback to codex", decision, ok)
	}

	sonnetIssue := makeIssue(909, "Sonnet route", "model:sonnet")
	decision, ok, retryAt := o.resolveDispatchBackend(s, sonnetIssue, now)
	if !ok || decision.Backend != "sonnet" || retryAt != nil {
		t.Fatalf("Sonnet decision = %+v ok=%v retry=%v, want same provider's healthy model", decision, ok, retryAt)
	}
}

func TestProviderLaneFallback_ClaudeUnavailableThenSOLModelCooldownThenGPT55(t *testing.T) {
	cfg := &config.Config{Model: config.ModelConfig{
		Default: "claude",
		ProviderLanes: []config.ProviderLane{
			{Provider: "anthropic", Default: "claude"},
			{Provider: "openai", Default: "sol", FallbackBackends: []string{"gpt55"}},
		},
		Backends: map[string]config.BackendDef{
			"claude": {Cmd: "claude", Provider: "anthropic", Model: "fable-5", Effort: "high"},
			"sol":    {Cmd: "codex", Provider: "openai", Model: "gpt-5.6-sol", Effort: "high"},
			"gpt55":  {Cmd: "codex", Provider: "openai", Model: "gpt-5.5", Effort: "high"},
		},
	}}
	o := &Orchestrator{cfg: cfg}
	now := time.Now().UTC()
	st := state.NewState()

	first := o.selectBackendFallback(st, &state.Session{
		Backend:       "claude",
		TriedBackends: []string{"claude"},
	}, now, selectionReasonModelUnavailableFallback)
	if first.SelectedBackend != "sol" || first.RouteSelectionReason != config.ModelRouteProviderLanes {
		t.Fatalf("claude fallback = %+v, want sol via provider_lanes", first)
	}

	retryAfter := now.Add(10 * time.Minute)
	st.ProviderModelHealth["openai"] = map[string]state.BackendHealth{
		"gpt-5.6-sol": {
			State:      state.BackendHealthCooldown,
			Reason:     state.BackendBlockModelCooldown,
			RetryAfter: &retryAfter,
		},
	}
	second := o.selectBackendFallback(st, &state.Session{
		Backend:       "sol",
		TriedBackends: []string{"claude", "sol"},
	}, now, selectionReasonModelCooldownFallback)
	if second.SelectedBackend != "gpt55" {
		t.Fatalf("SOL fallback = %+v, want gpt55", second)
	}
	if len(second.CandidateScores) != 1 || second.CandidateScores[0].Backend != "gpt55" || !second.CandidateScores[0].Available {
		t.Fatalf("GPT-5.5 candidate = %+v, want available despite SOL model cooldown", second.CandidateScores)
	}
}

func TestClassifyBackendFailure_SingleCredentialRotationDoesNotGateRoute(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "worker.log")
	if err := os.WriteFile(logFile, []byte("candidate entered cooldown for claude-fable-5; selecting another compatible credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{cfg: credentialAwareFallbackConfig()}
	sess := &state.Session{Backend: "fable", StartedAt: time.Now().Add(-time.Minute), LogFile: logFile}
	if failure, ok := o.classifyBackendFailure(sess, time.Now().UTC()); ok {
		t.Fatalf("one exhausted credential must not gate provider/model route: %+v", failure)
	}
}

func TestRespawnDueRetries_ModelRouteCooldownBreaksSessionAffinity(t *testing.T) {
	cfg := credentialAwareFallbackConfig()
	cfg.MaxRetryBackoffMs = 300000
	cfg.MaxRuntimeMinutes = 999
	var respawned string
	o := &Orchestrator{
		cfg:             cfg,
		notifier:        &notify.Notifier{},
		promptBase:      "test prompt",
		isIssueClosedFn: func(int) (bool, error) { return false, nil },
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "credential-aware retry"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
			respawned = backend
			sess.Backend = backend
			sess.Status = state.StatusRunning
			sess.PID = 9292
			return nil
		},
	}
	now := time.Now().UTC()
	past := now.Add(-time.Second)
	retry := now.Add(10 * time.Minute)
	s := state.NewState()
	s.ProviderModelHealth["fable"] = map[string]state.BackendHealth{
		"claude-fable-5": {
			State:      state.BackendHealthCooldown,
			Reason:     state.BackendBlockModelCooldown,
			RetryAfter: &retry,
		},
	}
	s.Sessions["sup-909"] = &state.Session{
		IssueNumber: 909,
		IssueTitle:  "credential-aware retry",
		Status:      state.StatusDead,
		Backend:     "fable",
		NextRetryAt: &past,
		Branch:      "feat/sup-909-credential-aware-retry",
	}

	o.respawnDueRetries(s, 10)

	sess := s.Sessions["sup-909"]
	if respawned != "codex" || sess.Backend != "codex" {
		t.Fatalf("retry remained pinned to unavailable route: respawned=%q session=%q", respawned, sess.Backend)
	}
	if sess.BackendSelection == nil || sess.BackendSelection.SelectionReason != selectionReasonRetryBlockedFallback {
		t.Fatalf("selection = %+v, want retry blocked fallback", sess.BackendSelection)
	}
}
