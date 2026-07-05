package orchestrator

import (
	"fmt"
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

// #693: when reconcile observes a dead worker whose log tail carries a
// backend auth-failure signature (live case: claude CLI "Failed to
// authenticate. API Error: 401 Invalid authentication credentials"), the
// death is a backend failure, not a work failure. The session must be
// respawned on the next fallback backend with the per-issue retry budget
// untouched, and the failed backend gated in BackendHealth.
func TestReconcileRunningSessions_AuthFailureDeadWorker_FallsOverToNextBackend(t *testing.T) {
	s := state.NewState()
	s.Sessions["kar-43"] = &state.Session{
		IssueNumber: 169,
		IssueTitle:  "karaoke conveyor",
		Status:      state.StatusRunning,
		PID:         424242,
		TmuxSession: "maestro-kar-43",
		Branch:      "feat/kar-43-169-conveyor",
		Backend:     "claude",
		StartedAt:   time.Now().Add(-2 * time.Minute),
		LogFile:     "/tmp/kar-43-auth.log",
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
		authFailureFromLogFn: func(logFile string) (bool, string) {
			return true, "failed_to_authenticate"
		},
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "karaoke conveyor"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawnedBackends = append(respawnedBackends, backendName)
			sess.Status = state.StatusRunning
			sess.PID = 9001
			sess.Backend = backendName
			sess.StartedAt = time.Now().UTC()
			sess.FinishedAt = nil
			return nil
		},
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["kar-43"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q (auth fallover should respawn the worker)", sess.Status, state.StatusRunning)
	}
	if len(respawnedBackends) != 1 || respawnedBackends[0] != "codex" {
		t.Fatalf("respawned backends = %v, want [codex]", respawnedBackends)
	}
	if len(sess.TriedBackends) != 1 || sess.TriedBackends[0] != "claude" {
		t.Fatalf("TriedBackends = %v, want [claude] (the auth-failed backend)", sess.TriedBackends)
	}
	if sess.RetryCount != 0 {
		t.Fatalf("RetryCount = %d, want 0 — backend auth failure must not consume the retry budget", sess.RetryCount)
	}
	if !sess.RateLimitHit {
		t.Fatal("RateLimitHit should be true so FailedAttemptsForIssue excludes the session")
	}
	if sess.ProviderLimitBackend != "claude" {
		t.Fatalf("ProviderLimitBackend = %q, want claude", sess.ProviderLimitBackend)
	}
	if sess.ProviderLimitReason != state.BackendBlockAuthFailure {
		t.Fatalf("ProviderLimitReason = %q, want %q", sess.ProviderLimitReason, state.BackendBlockAuthFailure)
	}
	if sess.BackendSelection == nil || sess.BackendSelection.SelectedBackend != "codex" || sess.BackendSelection.SelectionReason != selectionReasonAuthFailureFallback {
		t.Fatalf("BackendSelection = %+v, want SelectedBackend=codex SelectionReason=%s", sess.BackendSelection, selectionReasonAuthFailureFallback)
	}
	health, ok := s.BackendHealth["claude"]
	if !ok {
		t.Fatal("BackendHealth[claude] should be recorded by recordBackendFailure")
	}
	if health.State != state.BackendHealthCooldown || health.Reason != state.BackendBlockAuthFailure {
		t.Fatalf("BackendHealth[claude] = %+v, want cooldown/auth_failure", health)
	}
	if health.Pattern != "failed_to_authenticate" {
		t.Fatalf("BackendHealth[claude].Pattern = %q, want failed_to_authenticate", health.Pattern)
	}
	if health.RetryAfter == nil {
		t.Fatal("BackendHealth[claude].RetryAfter should be set so the backend is re-probed after the cooldown")
	}
	if remaining := time.Until(*health.RetryAfter); remaining <= 0 || remaining > backendAuthFailureCooldown+time.Minute {
		t.Fatalf("RetryAfter = %v (in %s), want ~%s from now", health.RetryAfter, remaining, backendAuthFailureCooldown)
	}
	if failed := s.FailedAttemptsForIssue(169); failed != 0 {
		t.Fatalf("FailedAttemptsForIssue(169) = %d, want 0", failed)
	}
}

// #693: with no fallback backend available, an auth-failed worker is marked
// dead with the backend_auth_failure notification token — and still must not
// count against the per-issue retry budget.
func TestReconcileRunningSessions_AuthFailureDeadWorker_NoFallback_DoesNotBurnRetryBudget(t *testing.T) {
	s := state.NewState()
	s.Sessions["kar-44"] = &state.Session{
		IssueNumber: 169,
		IssueTitle:  "karaoke conveyor",
		Status:      state.StatusRunning,
		PID:         424243,
		TmuxSession: "maestro-kar-44",
		Branch:      "feat/kar-44-169-conveyor",
		Backend:     "claude",
		StartedAt:   time.Now().Add(-90 * time.Second),
		LogFile:     "/tmp/kar-44-auth.log",
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
		authFailureFromLogFn: func(logFile string) (bool, string) {
			return true, "invalid_auth_credentials"
		},
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["kar-44"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.LastNotifiedStatus != "backend_auth_failure" {
		t.Fatalf("last_notified_status = %q, want backend_auth_failure", sess.LastNotifiedStatus)
	}
	if !sess.RateLimitHit {
		t.Fatal("RateLimitHit should be true so retry budget is preserved")
	}
	if failed := s.FailedAttemptsForIssue(169); failed != 0 {
		t.Fatalf("FailedAttemptsForIssue(169) = %d, want 0 — auth-failed dead session must not burn retry budget", failed)
	}
	if got := state.SessionDisplayStatusFor(sess, nil); got != string(state.DisplayBackendAuthFailure) {
		t.Fatalf("display status = %q, want %q", got, state.DisplayBackendAuthFailure)
	}
}

// #693 precision guard: an auth signature in the log of a worker that ran
// well past the early-death window is more likely incidental work content
// (the worker reading/writing auth-related code) than a backend outage. The
// session must take the ordinary dead path: no backend gating, no fallover.
func TestReconcileRunningSessions_AuthFailureLateDeath_NotClassified(t *testing.T) {
	s := state.NewState()
	s.Sessions["kar-45"] = &state.Session{
		IssueNumber: 169,
		Status:      state.StatusRunning,
		PID:         424244,
		TmuxSession: "maestro-kar-45",
		Branch:      "feat/kar-45-169-conveyor",
		Backend:     "claude",
		StartedAt:   time.Now().Add(-backendAuthFailureWindow - 20*time.Minute),
		LogFile:     "/tmp/kar-45-auth.log",
	}

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
		authFailureFromLogFn: func(logFile string) (bool, string) {
			t.Fatal("authFailureFromLogFn must not be consulted outside the early-death window")
			return false, ""
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			t.Fatal("respawnWorkerFn must not be called for an ordinary dead worker")
			return nil
		},
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["kar-45"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.RateLimitHit {
		t.Fatal("RateLimitHit must stay false on the ordinary dead path")
	}
	if _, ok := s.BackendHealth["claude"]; ok {
		t.Fatal("BackendHealth[claude] must not be gated for a late death")
	}
}

// #693: the main-loop dead-worker path (checkSessions) must classify the
// auth failure the same way reconcile does — fallback respawn, no retry
// budget burn. This is the path that torched kar-43/44/45/46 live: each
// death incremented RetryCount until the issue wedged on retry_exhausted.
func TestCheckSessions_AuthFailureDeadWorker_FallsOverToNextBackend(t *testing.T) {
	s := state.NewState()
	s.Sessions["kar-46"] = &state.Session{
		IssueNumber: 169,
		IssueTitle:  "karaoke conveyor",
		Status:      state.StatusRunning,
		PID:         424245,
		TmuxSession: "maestro-kar-46",
		Branch:      "feat/kar-46-169-conveyor",
		Backend:     "claude",
		StartedAt:   time.Now().UTC().Add(-2 * time.Minute),
		LogFile:     "/tmp/kar-46-auth.log",
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
	o.authFailureFromLogFn = func(logFile string) (bool, string) {
		return true, "failed_to_authenticate"
	}
	o.getIssueFn = func(number int) (github.Issue, error) {
		return github.Issue{Number: number, Title: "karaoke conveyor"}, nil
	}
	o.respawnWorkerFn = func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
		respawnedBackends = append(respawnedBackends, backendName)
		sess.Status = state.StatusRunning
		sess.PID = 9002
		sess.Backend = backendName
		sess.StartedAt = time.Now().UTC()
		sess.FinishedAt = nil
		return nil
	}

	o.checkSessions(s)

	sess := s.Sessions["kar-46"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q (auth fallover should respawn the worker)", sess.Status, state.StatusRunning)
	}
	if len(respawnedBackends) != 1 || respawnedBackends[0] != "codex" {
		t.Fatalf("respawned backends = %v, want [codex]", respawnedBackends)
	}
	if sess.RetryCount != 0 {
		t.Fatalf("RetryCount = %d, want 0 — backend auth failure must not consume max_retries_per_issue", sess.RetryCount)
	}
	if len(sess.TriedBackends) != 1 || sess.TriedBackends[0] != "claude" {
		t.Fatalf("TriedBackends = %v, want [claude]", sess.TriedBackends)
	}
	if sess.ProviderLimitReason != state.BackendBlockAuthFailure {
		t.Fatalf("ProviderLimitReason = %q, want %q", sess.ProviderLimitReason, state.BackendBlockAuthFailure)
	}
	if health, ok := s.BackendHealth["claude"]; !ok || health.Reason != state.BackendBlockAuthFailure {
		t.Fatalf("BackendHealth[claude] = %+v, want cooldown with reason auth_failure", health)
	}
	if failed := s.FailedAttemptsForIssue(169); failed != 0 {
		t.Fatalf("FailedAttemptsForIssue(169) = %d, want 0", failed)
	}
}

// #696: when the auth fallover in checkSessions aborts because the issue
// cannot be fetched, the session must carry the same status + display token
// as the reconcile paths (dead + backend_auth_failure), not a bare failed —
// otherwise the credential-outage context is lost on the dashboard.
func TestCheckSessions_AuthFailureGetIssueError_CarriesAuthFailureToken(t *testing.T) {
	s := state.NewState()
	s.Sessions["kar-47"] = &state.Session{
		IssueNumber: 169,
		IssueTitle:  "karaoke conveyor",
		Status:      state.StatusRunning,
		PID:         424246,
		TmuxSession: "maestro-kar-47",
		Branch:      "feat/kar-47-169-conveyor",
		Backend:     "claude",
		StartedAt:   time.Now().UTC().Add(-2 * time.Minute),
		LogFile:     "/tmp/kar-47-auth.log",
	}

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
	o.authFailureFromLogFn = func(logFile string) (bool, string) {
		return true, "failed_to_authenticate"
	}
	o.getIssueFn = func(number int) (github.Issue, error) {
		return github.Issue{}, fmt.Errorf("github unreachable")
	}
	o.respawnWorkerFn = func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
		t.Fatal("respawnWorkerFn must not be called when the issue fetch fails")
		return nil
	}

	o.checkSessions(s)

	sess := s.Sessions["kar-47"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q (same as the reconcile auth-failure paths)", sess.Status, state.StatusDead)
	}
	if sess.LastNotifiedStatus != "backend_auth_failure" {
		t.Fatalf("last_notified_status = %q, want backend_auth_failure", sess.LastNotifiedStatus)
	}
	if sess.FinishedAt == nil {
		t.Fatal("FinishedAt should be set")
	}
	if got := state.SessionDisplayStatusFor(sess, nil); got != string(state.DisplayBackendAuthFailure) {
		t.Fatalf("display status = %q, want %q", got, state.DisplayBackendAuthFailure)
	}
	if failed := s.FailedAttemptsForIssue(169); failed != 0 {
		t.Fatalf("FailedAttemptsForIssue(169) = %d, want 0 — auth-classified death must not burn retry budget", failed)
	}
}

// #696: same as above for the respawn-failed sub-path of the auth fallover
// in checkSessions.
func TestCheckSessions_AuthFailureRespawnError_CarriesAuthFailureToken(t *testing.T) {
	s := state.NewState()
	s.Sessions["kar-48"] = &state.Session{
		IssueNumber: 169,
		IssueTitle:  "karaoke conveyor",
		Status:      state.StatusRunning,
		PID:         424247,
		TmuxSession: "maestro-kar-48",
		Branch:      "feat/kar-48-169-conveyor",
		Backend:     "claude",
		StartedAt:   time.Now().UTC().Add(-2 * time.Minute),
		LogFile:     "/tmp/kar-48-auth.log",
	}

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
	o.authFailureFromLogFn = func(logFile string) (bool, string) {
		return true, "failed_to_authenticate"
	}
	o.getIssueFn = func(number int) (github.Issue, error) {
		return github.Issue{Number: number, Title: "karaoke conveyor"}, nil
	}
	o.respawnWorkerFn = func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
		return fmt.Errorf("tmux spawn failed")
	}

	o.checkSessions(s)

	sess := s.Sessions["kar-48"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q (same as the reconcile auth-failure paths)", sess.Status, state.StatusDead)
	}
	if sess.LastNotifiedStatus != "backend_auth_failure" {
		t.Fatalf("last_notified_status = %q, want backend_auth_failure", sess.LastNotifiedStatus)
	}
	if sess.FinishedAt == nil {
		t.Fatal("FinishedAt should be set")
	}
	if got := state.SessionDisplayStatusFor(sess, nil); got != string(state.DisplayBackendAuthFailure) {
		t.Fatalf("display status = %q, want %q", got, state.DisplayBackendAuthFailure)
	}
	if failed := s.FailedAttemptsForIssue(169); failed != 0 {
		t.Fatalf("FailedAttemptsForIssue(169) = %d, want 0 — auth-classified death must not burn retry budget", failed)
	}
}

// #693: repeated auth deaths across the fallback chain must never wedge the
// issue via retry_exhausted. With both backends auth-failed (claude gated,
// codex tried), canRetryIssue still sees zero consumed attempts.
func TestCanRetryIssue_AuthFailedSessions_DoNotConsumeBudget(t *testing.T) {
	s := state.NewState()
	for i := 0; i < 4; i++ {
		slot := fmt.Sprintf("kar-%d", 50+i)
		s.Sessions[slot] = &state.Session{
			IssueNumber:          169,
			Status:               state.StatusDead,
			Backend:              "claude",
			RateLimitHit:         true,
			ProviderLimitBackend: "claude",
			ProviderLimitReason:  state.BackendBlockAuthFailure,
		}
	}
	live := &state.Session{IssueNumber: 169, Status: state.StatusRunning, Backend: "codex"}
	s.Sessions["kar-54"] = live

	o := &Orchestrator{cfg: &config.Config{MaxRetriesPerIssue: 3}}
	if !o.canRetryIssue(s, live) {
		t.Fatal("canRetryIssue = false — four auth-failed sessions burned the retry budget (#693 regression)")
	}
}

// --- #695: fresh dispatch consults BackendHealth ---

func dispatchHealthConfig() *config.Config {
	cfg := cfgWithBackends("claude", "claude", "codex")
	cfg.Model.FallbackBackends = []string{"codex"}
	return cfg
}

func stateWithBackendCooldowns(now time.Time, retryAfterByBackend map[string]time.Time) *state.State {
	s := state.NewState()
	s.BackendHealth = make(map[string]state.BackendHealth, len(retryAfterByBackend))
	for backend, retryAfter := range retryAfterByBackend {
		retryAfter := retryAfter
		s.BackendHealth[backend] = state.BackendHealth{
			State:      state.BackendHealthCooldown,
			Reason:     state.BackendBlockAuthFailure,
			Since:      now,
			RetryAfter: &retryAfter,
		}
	}
	return s
}

// #695 acceptance 1 (selection path): with the default backend inside its
// auth-failure cooldown, a fresh dispatch resolves directly to the first
// healthy fallback backend instead of spawning a doomed worker.
func TestResolveDispatchBackend_DefaultInAuthCooldown_SubstitutesFirstHealthyFallback(t *testing.T) {
	cfg := dispatchHealthConfig()
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	now := time.Now().UTC()
	s := stateWithBackendCooldowns(now, map[string]time.Time{"claude": now.Add(8 * time.Minute)})

	decision, ok, retryAt := o.resolveDispatchBackend(s, makeIssue(695, "conveyor churn"), now)

	if !ok {
		t.Fatal("ok = false, want dispatchable — codex is healthy")
	}
	if decision.Backend != "codex" {
		t.Fatalf("Backend = %q, want codex (first healthy fallback)", decision.Backend)
	}
	if decision.Reason != selectionReasonDispatchBlockedFallback {
		t.Fatalf("Reason = %q, want %q", decision.Reason, selectionReasonDispatchBlockedFallback)
	}
	if retryAt != nil {
		t.Fatalf("retryAt = %v, want nil for a dispatchable decision", retryAt)
	}
}

// #695: a healthy routed backend passes through unchanged — same backend,
// same router-provided reason.
func TestResolveDispatchBackend_HealthyDefault_DecisionUnchanged(t *testing.T) {
	cfg := dispatchHealthConfig()
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	now := time.Now().UTC()

	decision, ok, _ := o.resolveDispatchBackend(state.NewState(), makeIssue(695, "conveyor churn"), now)

	if !ok || decision.Backend != "claude" || decision.Reason != router.ReasonDefault {
		t.Fatalf("decision = %+v ok=%v, want claude/%s dispatchable", decision, ok, router.ReasonDefault)
	}
}

// #695: an expired cooldown no longer blocks the routed backend, even before
// ReconcileBackendHealth has cleared the stale entry.
func TestResolveDispatchBackend_CooldownExpired_UsesRoutedBackend(t *testing.T) {
	cfg := dispatchHealthConfig()
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	now := time.Now().UTC()
	s := stateWithBackendCooldowns(now.Add(-20*time.Minute), map[string]time.Time{"claude": now.Add(-time.Minute)})

	decision, ok, _ := o.resolveDispatchBackend(s, makeIssue(695, "conveyor churn"), now)

	if !ok || decision.Backend != "claude" || decision.Reason != router.ReasonDefault {
		t.Fatalf("decision = %+v ok=%v, want claude/%s after cooldown expiry", decision, ok, router.ReasonDefault)
	}
}

// #695: the gate applies to label-pinned backends too — a pin onto a
// cooling-down backend is substituted exactly like the fallback selector
// would, instead of honoring the pin into a doomed spawn.
func TestResolveDispatchBackend_LabelPinnedBackendCooling_SubstitutesHealthyDefault(t *testing.T) {
	cfg := dispatchHealthConfig()
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	now := time.Now().UTC()
	s := stateWithBackendCooldowns(now, map[string]time.Time{"codex": now.Add(8 * time.Minute)})

	decision, ok, _ := o.resolveDispatchBackend(s, makeIssue(695, "conveyor churn", "model:codex"), now)

	if !ok {
		t.Fatal("ok = false, want dispatchable — claude is healthy")
	}
	if decision.Backend != "claude" || decision.Reason != selectionReasonDispatchBlockedFallback {
		t.Fatalf("decision = %+v, want claude/%s", decision, selectionReasonDispatchBlockedFallback)
	}
}

// #695 acceptance 2 (selection path): with every backend cooling down,
// resolveDispatchBackend reports not-dispatchable and the earliest cooldown
// expiry so dispatch can pause instead of spawn-die churning.
func TestResolveDispatchBackend_AllBackendsCoolingDown_NotDispatchable(t *testing.T) {
	cfg := dispatchHealthConfig()
	o := &Orchestrator{cfg: cfg, router: router.New(cfg)}
	now := time.Now().UTC()
	codexRetry := now.Add(4 * time.Minute)
	s := stateWithBackendCooldowns(now, map[string]time.Time{
		"claude": now.Add(9 * time.Minute),
		"codex":  codexRetry,
	})

	_, ok, retryAt := o.resolveDispatchBackend(s, makeIssue(695, "conveyor churn"), now)

	if ok {
		t.Fatal("ok = true, want not-dispatchable with all backends cooling down")
	}
	if retryAt == nil || !retryAt.Equal(codexRetry) {
		t.Fatalf("retryAt = %v, want earliest cooldown expiry %v (codex)", retryAt, codexRetry)
	}
}

// #695 acceptance 1 (dispatch loop): a newly dispatched issue whose default
// backend is in auth cooldown spawns directly on the first healthy fallback,
// with the substitution stamped on the session's BackendSelection.
func TestStartNewWorkers_DefaultInCooldown_SpawnsOnFirstHealthyFallback(t *testing.T) {
	cfg := dispatchHealthConfig()
	issues := []github.Issue{makeIssue(695, "conveyor churn")}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)

	now := time.Now().UTC()
	s := stateWithBackendCooldowns(now, map[string]time.Time{"claude": now.Add(8 * time.Minute)})
	o.startNewWorkers(s, 5)

	if len(*started) != 1 || (*started)[0] != 695 {
		t.Fatalf("started = %v, want [695]", *started)
	}
	sess := s.Sessions["slot-1"]
	if sess == nil || sess.Backend != "codex" {
		t.Fatalf("session = %+v, want spawn on codex", sess)
	}
	if sess.BackendSelection == nil || sess.BackendSelection.SelectedBackend != "codex" ||
		sess.BackendSelection.SelectionReason != selectionReasonDispatchBlockedFallback {
		t.Fatalf("BackendSelection = %+v, want codex/%s", sess.BackendSelection, selectionReasonDispatchBlockedFallback)
	}
}

// #695 acceptance 2 (dispatch loop): with all backends cooling down, no
// worker is spawned — dispatch pauses until a cooldown expires instead of
// churning one doomed spawn per poll cycle per issue.
func TestStartNewWorkers_AllBackendsCoolingDown_NoSpawn(t *testing.T) {
	cfg := dispatchHealthConfig()
	issues := []github.Issue{
		makeIssue(695, "conveyor churn"),
		makeIssue(696, "second eligible issue"),
	}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)

	now := time.Now().UTC()
	s := stateWithBackendCooldowns(now, map[string]time.Time{
		"claude": now.Add(9 * time.Minute),
		"codex":  now.Add(4 * time.Minute),
	})
	o.startNewWorkers(s, 5)

	if len(*started) != 0 {
		t.Fatalf("started = %v, want none while every backend is cooling down", *started)
	}
	if len(s.Sessions) != 0 {
		t.Fatalf("sessions = %d, want 0 — dispatch must pause, not spawn doomed workers", len(s.Sessions))
	}
}

// Codex with --profile proxy prints a "Model metadata for `fw-kimi-k2.7-code`
// not found" warning followed by "Missing environment variable: `CLIPROXY_API_KEY`"
// when the proxy key is absent. Without an auth-failure pattern for the missing
// env var, the model-unavailable classifier would fire on the metadata warning
// and gate codex with the wrong reason. The death must be classified as an auth
// (credential/config) failure so the attempt falls back instead of being
// mislabelled model-unavailable.
func TestClassifyBackendFailure_CodexCLIProxyMissingEnvVar_IsAuthFailure(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "codex-cliproxy.log")
	content := "OpenAI Codex v0.142.5\n" +
		"model: fw-kimi-k2p7-code\n" +
		"provider: cliproxy\n" +
		"warning: Model metadata for `fw-kimi-k2p7-code` not found. Defaulting to fallback metadata; this can degrade performance and cause issues.\n" +
		"ERROR: Missing environment variable: `CLIPROXY_API_KEY`.\n"
	if err := os.WriteFile(logFile, []byte(content), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	o := &Orchestrator{cfg: claudeChainTestConfig()}
	sess := &state.Session{
		Backend:   "codex",
		StartedAt: time.Now().UTC().Add(-90 * time.Second),
		LogFile:   logFile,
	}
	hit, reason, pattern := o.classifyBackendFailure(sess, time.Now().UTC())
	if !hit {
		t.Fatal("classifyBackendFailure = false, want true")
	}
	if reason != state.BackendBlockAuthFailure {
		t.Fatalf("reason = %q, want %q", reason, state.BackendBlockAuthFailure)
	}
	if pattern != "missing_api_key_env_var" {
		t.Fatalf("pattern = %q, want missing_api_key_env_var", pattern)
	}
}
