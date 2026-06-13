package orchestrator

import (
	"fmt"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/state"
)

// #713: when reconcile observes a dead worker whose log tail carries a
// model-unavailable signature (live case: the claude CLI "It may not exist or
// you may not have access to it" after Fable was pulled), the death is a
// backend failure, not a work failure. The session must respawn on the next
// fallback backend with the per-issue retry budget untouched, the failed
// backend gated in BackendHealth with reason model_unavailable, and the audit
// record naming the model-unavailable fallover reason.
func TestReconcileRunningSessions_ModelUnavailableDeadWorker_FallsOverToNextBackend(t *testing.T) {
	s := state.NewState()
	s.Sessions["kar-65"] = &state.Session{
		IssueNumber: 192,
		IssueTitle:  "karaoke conveyor",
		Status:      state.StatusRunning,
		PID:         525252,
		TmuxSession: "maestro-kar-65",
		Branch:      "feat/kar-65-192-conveyor",
		Backend:     "claude",
		StartedAt:   time.Now().Add(-2 * time.Minute),
		LogFile:     "/tmp/kar-65-model.log",
	}

	respawnedBackends := []string{}
	o := &Orchestrator{
		cfg: &config.Config{
			Repo: "owner/repo",
			Model: config.ModelConfig{
				Default:          "claude",
				FallbackBackends: []string{"codex"},
				Backends: map[string]config.BackendDef{
					"claude": {Cmd: "claude"},
					"codex":  {Cmd: "codex"},
				},
			},
		},
		notifier:            &notify.Notifier{},
		pidAliveFn:          func(pid int) bool { return false },
		tmuxSessionExistsFn: func(name string) bool { return false },
		listOpenPRsFn:       func() ([]github.PR, error) { return []github.PR{}, nil },
		isRateLimitedFn:     func(logFile string) bool { return false },
		modelUnavailableFromLogFn: func(logFile string) (bool, string) {
			return true, "model_no_access"
		},
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "karaoke conveyor"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawnedBackends = append(respawnedBackends, backendName)
			sess.Status = state.StatusRunning
			sess.PID = 9101
			sess.Backend = backendName
			sess.StartedAt = time.Now().UTC()
			sess.FinishedAt = nil
			return nil
		},
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["kar-65"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q (model-unavailable fallover should respawn)", sess.Status, state.StatusRunning)
	}
	if len(respawnedBackends) != 1 || respawnedBackends[0] != "codex" {
		t.Fatalf("respawned backends = %v, want [codex]", respawnedBackends)
	}
	if len(sess.TriedBackends) != 1 || sess.TriedBackends[0] != "claude" {
		t.Fatalf("TriedBackends = %v, want [claude]", sess.TriedBackends)
	}
	if sess.RetryCount != 0 {
		t.Fatalf("RetryCount = %d, want 0 — model-unavailable must not consume the retry budget", sess.RetryCount)
	}
	if !sess.RateLimitHit {
		t.Fatal("RateLimitHit should be true so FailedAttemptsForIssue excludes the session")
	}
	if sess.ProviderLimitReason != state.BackendBlockModelUnavailable {
		t.Fatalf("ProviderLimitReason = %q, want %q", sess.ProviderLimitReason, state.BackendBlockModelUnavailable)
	}
	if sess.BackendSelection == nil || sess.BackendSelection.SelectedBackend != "codex" ||
		sess.BackendSelection.SelectionReason != selectionReasonModelUnavailableFallback {
		t.Fatalf("BackendSelection = %+v, want SelectedBackend=codex SelectionReason=%s", sess.BackendSelection, selectionReasonModelUnavailableFallback)
	}
	health, ok := s.BackendHealth["claude"]
	if !ok {
		t.Fatal("BackendHealth[claude] should be recorded by recordBackendFailure")
	}
	if health.State != state.BackendHealthCooldown || health.Reason != state.BackendBlockModelUnavailable {
		t.Fatalf("BackendHealth[claude] = %+v, want cooldown/model_unavailable", health)
	}
	if health.Pattern != "model_no_access" {
		t.Fatalf("BackendHealth[claude].Pattern = %q, want model_no_access", health.Pattern)
	}
	if health.RetryAfter == nil {
		t.Fatal("BackendHealth[claude].RetryAfter should be set so the backend is re-probed after cooldown")
	}
	if failed := s.FailedAttemptsForIssue(192); failed != 0 {
		t.Fatalf("FailedAttemptsForIssue(192) = %d, want 0", failed)
	}
}

// #713 acceptance 2: with no fallback backend available (every backend's model
// is unavailable), the worker is marked dead with the backend_model_unavailable
// token and still must not burn the per-issue retry budget — a single Blocked
// state, no churn.
func TestReconcileRunningSessions_ModelUnavailableDeadWorker_NoFallback_DoesNotBurnRetryBudget(t *testing.T) {
	s := state.NewState()
	s.Sessions["kar-66"] = &state.Session{
		IssueNumber: 192,
		IssueTitle:  "karaoke conveyor",
		Status:      state.StatusRunning,
		PID:         525253,
		TmuxSession: "maestro-kar-66",
		Branch:      "feat/kar-66-192-conveyor",
		Backend:     "claude",
		StartedAt:   time.Now().Add(-90 * time.Second),
		LogFile:     "/tmp/kar-66-model.log",
	}

	o := &Orchestrator{
		cfg: &config.Config{
			Repo: "owner/repo",
			Model: config.ModelConfig{
				Default:  "claude",
				Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
			},
		},
		notifier:            &notify.Notifier{},
		pidAliveFn:          func(pid int) bool { return false },
		tmuxSessionExistsFn: func(name string) bool { return false },
		listOpenPRsFn:       func() ([]github.PR, error) { return []github.PR{}, nil },
		isRateLimitedFn:     func(logFile string) bool { return false },
		modelUnavailableFromLogFn: func(logFile string) (bool, string) {
			return true, "model_not_found"
		},
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["kar-66"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.LastNotifiedStatus != "backend_model_unavailable" {
		t.Fatalf("last_notified_status = %q, want backend_model_unavailable", sess.LastNotifiedStatus)
	}
	if !sess.RateLimitHit {
		t.Fatal("RateLimitHit should be true so retry budget is preserved")
	}
	if failed := s.FailedAttemptsForIssue(192); failed != 0 {
		t.Fatalf("FailedAttemptsForIssue(192) = %d, want 0 — model-unavailable dead session must not burn retry budget", failed)
	}
	if got := state.SessionDisplayStatusFor(sess, nil); got != string(state.DisplayBackendModelUnavailable) {
		t.Fatalf("display status = %q, want %q", got, state.DisplayBackendModelUnavailable)
	}
}

// #713: an auth signature takes precedence over a model-unavailable one when
// both are (improbably) present, so a 401 is never mislabeled as a model
// outage — the remediation differs.
func TestClassifyBackendFailure_AuthTakesPrecedenceOverModel(t *testing.T) {
	o := &Orchestrator{
		authFailureFromLogFn:      func(string) (bool, string) { return true, "failed_to_authenticate" },
		modelUnavailableFromLogFn: func(string) (bool, string) { return true, "model_no_access" },
	}
	sess := &state.Session{StartedAt: time.Now().Add(-time.Minute), LogFile: "/tmp/x.log"}
	hit, class, pattern := o.classifyBackendFailure(sess, time.Now().UTC())
	if !hit || class != state.BackendBlockAuthFailure || pattern != "failed_to_authenticate" {
		t.Fatalf("classify = (%v, %q, %q), want (true, auth_failure, failed_to_authenticate)", hit, class, pattern)
	}
}

// #713: the main-loop dead-worker path (checkSessions) must classify the
// model-unavailable death the same way reconcile does — fallback respawn, no
// retry budget burn. This is the path that torched kar-65..68 live.
func TestCheckSessions_ModelUnavailableDeadWorker_FallsOverToNextBackend(t *testing.T) {
	s := state.NewState()
	s.Sessions["kar-67"] = &state.Session{
		IssueNumber: 192,
		IssueTitle:  "karaoke conveyor",
		Status:      state.StatusRunning,
		PID:         525254,
		TmuxSession: "maestro-kar-67",
		Branch:      "feat/kar-67-192-conveyor",
		Backend:     "claude",
		StartedAt:   time.Now().UTC().Add(-2 * time.Minute),
		LogFile:     "/tmp/kar-67-model.log",
	}

	respawnedBackends := []string{}
	cfg := &config.Config{
		Repo:               "owner/repo",
		MaxRuntimeMinutes:  999,
		MaxRetriesPerIssue: 3,
		Model: config.ModelConfig{
			Default:          "claude",
			FallbackBackends: []string{"codex"},
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
	}
	o, _ := newCheckSessionsOrchestrator(cfg, "")
	o.pidAliveFn = func(pid int) bool { return false }
	o.isRateLimitedFn = func(logFile string) bool { return false }
	o.modelUnavailableFromLogFn = func(logFile string) (bool, string) {
		return true, "model_no_access"
	}
	o.getIssueFn = func(number int) (github.Issue, error) {
		return github.Issue{Number: number, Title: "karaoke conveyor"}, nil
	}
	o.respawnWorkerFn = func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
		respawnedBackends = append(respawnedBackends, backendName)
		sess.Status = state.StatusRunning
		sess.PID = 9102
		sess.Backend = backendName
		sess.StartedAt = time.Now().UTC()
		sess.FinishedAt = nil
		return nil
	}

	o.checkSessions(s)

	sess := s.Sessions["kar-67"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q (model-unavailable fallover should respawn)", sess.Status, state.StatusRunning)
	}
	if len(respawnedBackends) != 1 || respawnedBackends[0] != "codex" {
		t.Fatalf("respawned backends = %v, want [codex]", respawnedBackends)
	}
	if sess.RetryCount != 0 {
		t.Fatalf("RetryCount = %d, want 0 — model-unavailable must not consume max_retries_per_issue", sess.RetryCount)
	}
	if sess.ProviderLimitReason != state.BackendBlockModelUnavailable {
		t.Fatalf("ProviderLimitReason = %q, want %q", sess.ProviderLimitReason, state.BackendBlockModelUnavailable)
	}
	if health, ok := s.BackendHealth["claude"]; !ok || health.Reason != state.BackendBlockModelUnavailable {
		t.Fatalf("BackendHealth[claude] = %+v, want cooldown with reason model_unavailable", health)
	}
	if failed := s.FailedAttemptsForIssue(192); failed != 0 {
		t.Fatalf("FailedAttemptsForIssue(192) = %d, want 0", failed)
	}
}

// #713 acceptance 1: repeated model-unavailable deaths across the fallback
// chain must never wedge the issue via retry_exhausted — four dead workers
// (kar-65/66/67/68, live) leave the issue fully retryable.
func TestCanRetryIssue_ModelUnavailableSessions_DoNotConsumeBudget(t *testing.T) {
	s := state.NewState()
	for i := 0; i < 4; i++ {
		slot := fmt.Sprintf("kar-%d", 65+i)
		s.Sessions[slot] = &state.Session{
			IssueNumber:          192,
			Status:               state.StatusDead,
			Backend:              "claude",
			RateLimitHit:         true,
			ProviderLimitBackend: "claude",
			ProviderLimitReason:  state.BackendBlockModelUnavailable,
		}
	}
	live := &state.Session{IssueNumber: 192, Status: state.StatusRunning, Backend: "codex"}
	s.Sessions["kar-69"] = live

	o := &Orchestrator{cfg: &config.Config{MaxRetriesPerIssue: 3}}
	if !o.canRetryIssue(s, live) {
		t.Fatal("canRetryIssue = false — four model-unavailable sessions burned the retry budget (#713 regression)")
	}
}

// #713 (req 4): the #695 fresh-dispatch BackendHealth consult skips a backend
// gated by a model-unavailable cooldown too, not just auth — a new dispatch
// during a model outage spawns on the first healthy fallback instead of a
// doomed worker on the gated default.
func TestResolveDispatchBackend_DefaultInModelUnavailableCooldown_SubstitutesFallback(t *testing.T) {
	cfg := dispatchHealthConfig()
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	now := time.Now().UTC()
	retryAfter := now.Add(8 * time.Minute)
	s := state.NewState()
	s.BackendHealth["claude"] = state.BackendHealth{
		State:      state.BackendHealthCooldown,
		Reason:     state.BackendBlockModelUnavailable,
		Pattern:    "model_no_access",
		Since:      now,
		RetryAfter: &retryAfter,
	}

	decision, ok, _ := o.resolveDispatchBackend(s, makeIssue(713, "conveyor churn"), now)

	if !ok {
		t.Fatal("ok = false, want dispatchable — codex is healthy")
	}
	if decision.Backend != "codex" {
		t.Fatalf("Backend = %q, want codex (first healthy fallback during model outage)", decision.Backend)
	}
	if decision.Reason != selectionReasonDispatchBlockedFallback {
		t.Fatalf("Reason = %q, want %q", decision.Reason, selectionReasonDispatchBlockedFallback)
	}
}
