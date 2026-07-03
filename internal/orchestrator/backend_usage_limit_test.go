package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/state"
)

func usageLimitTestConfig() *config.Config {
	return &config.Config{
		Repo: "owner/repo",
		Model: config.ModelConfig{
			Default:          "codex",
			FallbackBackends: []string{"opencode"},
			Backends: map[string]config.BackendDef{
				"codex":    {Cmd: "codex"},
				"opencode": {Cmd: "opencode"},
			},
		},
	}
}

// #805: a dead worker whose log tail carries a quota-exhaustion signature
// (live: codex "You've hit your usage limit") within the early-death window
// is a backend failure, not a work failure. The backend is gated in
// BackendHealth with reason usage_limit and the attempt respawns on the next
// fallback backend with the per-issue retry budget untouched.
func TestReconcileRunningSessions_UsageLimitDeadWorker_FallsOverToNextBackend(t *testing.T) {
	s := state.NewState()
	s.Sessions["ok-12"] = &state.Session{
		IssueNumber: 217,
		IssueTitle:  "folio conveyor",
		Status:      state.StatusRunning,
		PID:         515151,
		TmuxSession: "maestro-ok-12",
		Branch:      "feat/ok-12-217-conveyor",
		Backend:     "codex",
		StartedAt:   time.Now().Add(-90 * time.Second),
		LogFile:     "/tmp/ok-12-usage.log",
	}

	respawnedBackends := []string{}
	o := &Orchestrator{
		cfg:                 usageLimitTestConfig(),
		notifier:            &notify.Notifier{},
		pidAliveFn:          func(pid int) bool { return false },
		tmuxSessionExistsFn: func(name string) bool { return false },
		listOpenPRsFn:       func() ([]github.PR, error) { return []github.PR{}, nil },
		isRateLimitedFn:     func(logFile string) bool { return false },
		authFailureFromLogFn: func(logFile string) (bool, string) {
			return false, ""
		},
		modelUnavailableFromLogFn: func(logFile string) (bool, string) {
			return false, ""
		},
		usageLimitFromLogFn: func(logFile string, extraPatterns []string) (bool, string) {
			return true, "hit_limit"
		},
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "folio conveyor"}, nil
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

	sess := s.Sessions["ok-12"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q (usage-limit fallover should respawn the worker)", sess.Status, state.StatusRunning)
	}
	if len(respawnedBackends) != 1 || respawnedBackends[0] != "opencode" {
		t.Fatalf("respawned backends = %v, want [opencode]", respawnedBackends)
	}
	if len(sess.TriedBackends) != 1 || sess.TriedBackends[0] != "codex" {
		t.Fatalf("TriedBackends = %v, want [codex] (the quota-dead backend)", sess.TriedBackends)
	}
	if sess.RetryCount != 0 {
		t.Fatalf("RetryCount = %d, want 0 — a quota death must not consume the retry budget", sess.RetryCount)
	}
	if !sess.RateLimitHit {
		t.Fatal("RateLimitHit should be true so FailedAttemptsForIssue excludes the session")
	}
	if sess.ProviderLimitReason != state.BackendBlockUsageLimit {
		t.Fatalf("ProviderLimitReason = %q, want %q", sess.ProviderLimitReason, state.BackendBlockUsageLimit)
	}
	if sess.BackendSelection == nil || sess.BackendSelection.SelectedBackend != "opencode" || sess.BackendSelection.SelectionReason != selectionReasonUsageLimitFallback {
		t.Fatalf("BackendSelection = %+v, want SelectedBackend=opencode SelectionReason=%s", sess.BackendSelection, selectionReasonUsageLimitFallback)
	}
	health, ok := s.BackendHealth["codex"]
	if !ok {
		t.Fatal("BackendHealth[codex] should be recorded for the quota death")
	}
	if health.State != state.BackendHealthCooldown || health.Reason != state.BackendBlockUsageLimit {
		t.Fatalf("BackendHealth[codex] = %+v, want cooldown/usage_limit", health)
	}
	if health.Pattern != "hit_limit" {
		t.Fatalf("BackendHealth[codex].Pattern = %q, want hit_limit", health.Pattern)
	}
	if health.RetryAfter == nil {
		t.Fatal("BackendHealth[codex].RetryAfter should be set so the backend is re-probed after the cooldown")
	}
	remaining := time.Until(*health.RetryAfter)
	if remaining <= backendAuthFailureCooldown || remaining > backendUsageLimitCooldown+time.Minute {
		t.Fatalf("RetryAfter in %s, want ~%s from now (quota cooldown, longer than the auth cooldown)", remaining, backendUsageLimitCooldown)
	}
	if failed := s.FailedAttemptsForIssue(217); failed != 0 {
		t.Fatalf("FailedAttemptsForIssue(217) = %d, want 0", failed)
	}
}

// #805: with no fallback backend available, a quota-dead worker is marked
// dead with the backend_usage_limit display token — and still must not count
// against the per-issue retry budget.
func TestReconcileRunningSessions_UsageLimitDeadWorker_NoFallback_DoesNotBurnRetryBudget(t *testing.T) {
	s := state.NewState()
	s.Sessions["ok-13"] = &state.Session{
		IssueNumber: 218,
		IssueTitle:  "folio conveyor",
		Status:      state.StatusRunning,
		PID:         515152,
		TmuxSession: "maestro-ok-13",
		Branch:      "feat/ok-13-218-conveyor",
		Backend:     "codex",
		StartedAt:   time.Now().Add(-2 * time.Minute),
		LogFile:     "/tmp/ok-13-usage.log",
	}

	o := &Orchestrator{
		cfg: &config.Config{
			Repo: "owner/repo",
			Model: config.ModelConfig{
				Default:  "codex",
				Backends: map[string]config.BackendDef{"codex": {Cmd: "codex"}},
			},
		},
		notifier:            &notify.Notifier{},
		pidAliveFn:          func(pid int) bool { return false },
		tmuxSessionExistsFn: func(name string) bool { return false },
		listOpenPRsFn:       func() ([]github.PR, error) { return []github.PR{}, nil },
		isRateLimitedFn:     func(logFile string) bool { return false },
		authFailureFromLogFn: func(logFile string) (bool, string) {
			return false, ""
		},
		modelUnavailableFromLogFn: func(logFile string) (bool, string) {
			return false, ""
		},
		usageLimitFromLogFn: func(logFile string, extraPatterns []string) (bool, string) {
			return true, "codex_usage_limit"
		},
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["ok-13"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.LastNotifiedStatus != string(state.DisplayBackendUsageLimit) {
		t.Fatalf("last_notified_status = %q, want %q", sess.LastNotifiedStatus, state.DisplayBackendUsageLimit)
	}
	if !sess.RateLimitHit {
		t.Fatal("RateLimitHit should be true so retry budget is preserved")
	}
	if failed := s.FailedAttemptsForIssue(218); failed != 0 {
		t.Fatalf("FailedAttemptsForIssue(218) = %d, want 0 — quota-dead session must not burn retry budget", failed)
	}
	if got := state.SessionDisplayStatusFor(sess, nil); got != string(state.DisplayBackendUsageLimit) {
		t.Fatalf("display status = %q, want %q", got, state.DisplayBackendUsageLimit)
	}
}

// #805 precision guard: a usage-limit signature in the log of a worker that
// ran well past the early-death window is more likely incidental work content
// (a prompt echo of a quota-related issue) than a backend outage. The session
// must take the ordinary dead path: no backend gating, no fallover.
func TestReconcileRunningSessions_UsageLimitLateDeath_NotClassified(t *testing.T) {
	s := state.NewState()
	s.Sessions["ok-14"] = &state.Session{
		IssueNumber: 219,
		Status:      state.StatusRunning,
		PID:         515153,
		TmuxSession: "maestro-ok-14",
		Branch:      "feat/ok-14-219-conveyor",
		Backend:     "codex",
		StartedAt:   time.Now().Add(-backendAuthFailureWindow - 25*time.Minute),
		LogFile:     "/tmp/ok-14-usage.log",
	}

	o := &Orchestrator{
		cfg:                 usageLimitTestConfig(),
		notifier:            &notify.Notifier{},
		pidAliveFn:          func(pid int) bool { return false },
		tmuxSessionExistsFn: func(name string) bool { return false },
		listOpenPRsFn:       func() ([]github.PR, error) { return []github.PR{}, nil },
		isRateLimitedFn:     func(logFile string) bool { return false },
		usageLimitFromLogFn: func(logFile string, extraPatterns []string) (bool, string) {
			t.Fatal("usageLimitFromLogFn must not be consulted outside the early-death window")
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

	sess := s.Sessions["ok-14"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want %q", sess.Status, state.StatusDead)
	}
	if sess.RateLimitHit {
		t.Fatal("RateLimitHit must stay false on the ordinary dead path")
	}
	if _, ok := s.BackendHealth["codex"]; ok {
		t.Fatal("BackendHealth[codex] must not be gated for a late death")
	}
}

// #805: the main-loop dead-worker path (checkSessions) must classify the
// quota death the same way reconcile does — fallback respawn, no retry budget
// burn. This is the path that parked ok-folio #217/#218 in retry_exhausted
// overnight.
func TestCheckSessions_UsageLimitDeadWorker_FallsOverToNextBackend(t *testing.T) {
	s := state.NewState()
	s.Sessions["ok-15"] = &state.Session{
		IssueNumber: 217,
		IssueTitle:  "folio conveyor",
		Status:      state.StatusRunning,
		PID:         515154,
		TmuxSession: "maestro-ok-15",
		Branch:      "feat/ok-15-217-conveyor",
		Backend:     "codex",
		StartedAt:   time.Now().UTC().Add(-90 * time.Second),
		LogFile:     "/tmp/ok-15-usage.log",
	}

	respawnedBackends := []string{}
	cfg := usageLimitTestConfig()
	cfg.MaxRuntimeMinutes = 999
	cfg.MaxRetriesPerIssue = 3
	o, _ := newCheckSessionsOrchestrator(cfg, "")
	o.pidAliveFn = func(pid int) bool { return false }
	o.isRateLimitedFn = func(logFile string) bool { return false }
	o.authFailureFromLogFn = func(logFile string) (bool, string) { return false, "" }
	o.modelUnavailableFromLogFn = func(logFile string) (bool, string) { return false, "" }
	o.usageLimitFromLogFn = func(logFile string, extraPatterns []string) (bool, string) {
		return true, "hit_limit"
	}
	o.getIssueFn = func(number int) (github.Issue, error) {
		return github.Issue{Number: number, Title: "folio conveyor"}, nil
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

	sess := s.Sessions["ok-15"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q (usage-limit fallover should respawn the worker)", sess.Status, state.StatusRunning)
	}
	if len(respawnedBackends) != 1 || respawnedBackends[0] != "opencode" {
		t.Fatalf("respawned backends = %v, want [opencode]", respawnedBackends)
	}
	if sess.RetryCount != 0 {
		t.Fatalf("RetryCount = %d, want 0 — a quota death must not consume max_retries_per_issue", sess.RetryCount)
	}
	if sess.ProviderLimitReason != state.BackendBlockUsageLimit {
		t.Fatalf("ProviderLimitReason = %q, want %q", sess.ProviderLimitReason, state.BackendBlockUsageLimit)
	}
	if health, ok := s.BackendHealth["codex"]; !ok || health.Reason != state.BackendBlockUsageLimit {
		t.Fatalf("BackendHealth[codex] = %+v, want cooldown with reason usage_limit", health)
	}
	if failed := s.FailedAttemptsForIssue(217); failed != 0 {
		t.Fatalf("FailedAttemptsForIssue(217) = %d, want 0", failed)
	}
}

// #805: the operator's per-backend usage_limit_patterns reach the classifier
// for the session's backend.
func TestBackendUsageLimitFromLog_ThreadsConfiguredExtraPatterns(t *testing.T) {
	cfg := usageLimitTestConfig()
	def := cfg.Model.Backends["codex"]
	def.UsageLimitPatterns = []string{`(?i)monthly spend cap reached`}
	cfg.Model.Backends["codex"] = def

	var gotExtra []string
	o := &Orchestrator{
		cfg: cfg,
		usageLimitFromLogFn: func(logFile string, extraPatterns []string) (bool, string) {
			gotExtra = extraPatterns
			return false, ""
		},
	}
	sess := &state.Session{Backend: "codex", StartedAt: time.Now().UTC().Add(-time.Minute), LogFile: "/tmp/x.log"}
	o.backendUsageLimitFromLog(sess, time.Now().UTC())
	if len(gotExtra) != 1 || gotExtra[0] != `(?i)monthly spend cap reached` {
		t.Fatalf("extra patterns = %v, want the configured codex entry", gotExtra)
	}
}

// #805: a scheduled retry whose session backend is gated in BackendHealth
// must respawn on the first healthy fallback instead of re-burning the retry
// budget on the dead backend (the "reason=default" respawn loop from the
// incident).
func TestRespawnDueRetries_BackendInCooldown_SubstitutesHealthyFallback(t *testing.T) {
	cfg := usageLimitTestConfig()
	cfg.MaxRetryBackoffMs = 300000
	cfg.MaxRuntimeMinutes = 999

	respawnedBackends := []string{}
	o := &Orchestrator{
		cfg:             cfg,
		notifier:        &notify.Notifier{},
		promptBase:      "test prompt",
		isIssueClosedFn: func(issueNumber int) (bool, error) { return false, nil },
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "folio conveyor"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
			respawnedBackends = append(respawnedBackends, backend)
			sess.Status = state.StatusRunning
			sess.PID = 9104
			return nil
		},
	}

	now := time.Now().UTC()
	pastTime := now.Add(-time.Second)
	retryAfter := now.Add(20 * time.Minute)
	s := state.NewState()
	s.BackendHealth["codex"] = state.BackendHealth{
		State:      state.BackendHealthCooldown,
		Reason:     state.BackendBlockUsageLimit,
		Since:      now.Add(-10 * time.Minute),
		RetryAfter: &retryAfter,
	}
	s.Sessions["ok-17"] = &state.Session{
		IssueNumber: 217,
		IssueTitle:  "folio conveyor",
		Status:      state.StatusDead,
		Backend:     "codex",
		RetryCount:  1,
		NextRetryAt: &pastTime,
		Branch:      "feat/ok-17-217-conveyor",
	}

	o.respawnDueRetries(s, 10)

	sess := s.Sessions["ok-17"]
	if len(respawnedBackends) != 1 || respawnedBackends[0] != "opencode" {
		t.Fatalf("respawned backends = %v, want [opencode] — the retry must not respawn on the gated backend", respawnedBackends)
	}
	if sess.Backend != "opencode" {
		t.Fatalf("sess.Backend = %q, want opencode", sess.Backend)
	}
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want running", sess.Status)
	}
	if sess.BackendSelection == nil || sess.BackendSelection.SelectedBackend != "opencode" ||
		sess.BackendSelection.SelectionReason != selectionReasonRetryBlockedFallback {
		t.Fatalf("BackendSelection = %+v, want opencode/%s", sess.BackendSelection, selectionReasonRetryBlockedFallback)
	}
	if sess.BackendSelection.PreviousBackend != "codex" {
		t.Fatalf("PreviousBackend = %q, want codex (the blocked backend)", sess.BackendSelection.PreviousBackend)
	}
}

// #805: with every backend blocked, the due retry is deferred to the
// cooldown expiry instead of respawning a doomed worker — and instead of
// consuming the attempt.
func TestRespawnDueRetries_AllBackendsBlocked_DefersRetryToCooldownExpiry(t *testing.T) {
	cfg := usageLimitTestConfig()
	cfg.MaxRetryBackoffMs = 300000
	cfg.MaxRuntimeMinutes = 999

	o := &Orchestrator{
		cfg:             cfg,
		notifier:        &notify.Notifier{},
		promptBase:      "test prompt",
		isIssueClosedFn: func(issueNumber int) (bool, error) { return false, nil },
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "folio conveyor"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
			t.Fatal("respawnWorkerFn must not be called while every backend is blocked")
			return nil
		},
	}

	now := time.Now().UTC()
	pastTime := now.Add(-time.Second)
	codexRetry := now.Add(25 * time.Minute)
	opencodeRetry := now.Add(8 * time.Minute)
	s := state.NewState()
	s.BackendHealth["codex"] = state.BackendHealth{
		State: state.BackendHealthCooldown, Reason: state.BackendBlockUsageLimit,
		Since: now.Add(-5 * time.Minute), RetryAfter: &codexRetry,
	}
	s.BackendHealth["opencode"] = state.BackendHealth{
		State: state.BackendHealthCooldown, Reason: state.BackendBlockAuthFailure,
		Since: now.Add(-5 * time.Minute), RetryAfter: &opencodeRetry,
	}
	s.Sessions["ok-18"] = &state.Session{
		IssueNumber: 218,
		IssueTitle:  "folio conveyor",
		Status:      state.StatusDead,
		Backend:     "codex",
		RetryCount:  1,
		NextRetryAt: &pastTime,
		Branch:      "feat/ok-18-218-conveyor",
	}

	o.respawnDueRetries(s, 10)

	sess := s.Sessions["ok-18"]
	if sess.Status != state.StatusDead {
		t.Fatalf("status = %q, want dead (retry stays queued)", sess.Status)
	}
	if sess.NextRetryAt == nil {
		t.Fatal("NextRetryAt should be re-armed so the retry fires after the cooldown")
	}
	if !sess.NextRetryAt.Equal(codexRetry) {
		t.Fatalf("NextRetryAt = %v, want the blocked backend's cooldown expiry %v", sess.NextRetryAt, codexRetry)
	}
	if sess.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want unchanged 1 — a deferral must not consume the budget", sess.RetryCount)
	}
}

// A retry on a healthy backend keeps the pre-#805 behavior: no substitution,
// no deferral.
func TestRespawnDueRetries_HealthyBackend_RespawnsUnchanged(t *testing.T) {
	cfg := usageLimitTestConfig()
	cfg.MaxRetryBackoffMs = 300000
	cfg.MaxRuntimeMinutes = 999

	respawnedBackends := []string{}
	o := &Orchestrator{
		cfg:             cfg,
		notifier:        &notify.Notifier{},
		promptBase:      "test prompt",
		isIssueClosedFn: func(issueNumber int) (bool, error) { return false, nil },
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "folio conveyor"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backend string) error {
			respawnedBackends = append(respawnedBackends, backend)
			sess.Status = state.StatusRunning
			return nil
		},
	}

	pastTime := time.Now().UTC().Add(-time.Second)
	s := state.NewState()
	s.Sessions["ok-19"] = &state.Session{
		IssueNumber: 219,
		Status:      state.StatusDead,
		Backend:     "codex",
		RetryCount:  1,
		NextRetryAt: &pastTime,
		Branch:      "feat/ok-19-219-conveyor",
	}

	o.respawnDueRetries(s, 10)

	if len(respawnedBackends) != 1 || respawnedBackends[0] != "codex" {
		t.Fatalf("respawned backends = %v, want [codex] unchanged", respawnedBackends)
	}
	if sel := s.Sessions["ok-19"].BackendSelection; sel != nil {
		t.Fatalf("BackendSelection = %+v, want nil (no substitution happened)", sel)
	}
}

// #805 incident replay, end to end through the real log classifiers: a codex
// worker dies with the exact live message ("You've hit your usage limit ...
// try again at 12:30 PM."). The time-only reset now parses, so the death is a
// HIGH-confidence provider limit: BackendHealth gates codex with the resolved
// reset time and the attempt respawns on the fallback within the same cycle —
// the journal on 2026-07-02 instead showed running->dead and reason=default
// respawns until retry_exhausted.
func TestReconcileRunningSessions_CodexTimeOnlyUsageLimit_IncidentReplay(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "ok-16.log")
	logContent := "Starting codex worker for issue #217\n" +
		"You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/codex/settings/usage) or try again at 12:30 PM.\n"
	if err := os.WriteFile(logFile, []byte(logContent), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	s := state.NewState()
	s.Sessions["ok-16"] = &state.Session{
		IssueNumber: 217,
		IssueTitle:  "folio conveyor",
		Status:      state.StatusRunning,
		PID:         515155,
		TmuxSession: "maestro-ok-16",
		Branch:      "feat/ok-16-217-conveyor",
		Backend:     "codex",
		StartedAt:   time.Now().UTC().Add(-90 * time.Second),
		LogFile:     logFile,
	}

	respawnedBackends := []string{}
	o := &Orchestrator{
		cfg:                 usageLimitTestConfig(),
		notifier:            &notify.Notifier{},
		pidAliveFn:          func(pid int) bool { return false },
		tmuxSessionExistsFn: func(name string) bool { return false },
		listOpenPRsFn:       func() ([]github.PR, error) { return []github.PR{}, nil },
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: number, Title: "folio conveyor"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			respawnedBackends = append(respawnedBackends, backendName)
			sess.Status = state.StatusRunning
			sess.PID = 9103
			sess.Backend = backendName
			sess.StartedAt = time.Now().UTC()
			sess.FinishedAt = nil
			return nil
		},
	}

	if !o.reconcileRunningSessions(s) {
		t.Fatal("expected reconciliation to report changes")
	}

	sess := s.Sessions["ok-16"]
	if sess.Status != state.StatusRunning {
		t.Fatalf("status = %q, want running (failover respawn), got dead — the #805 regression", sess.Status)
	}
	if len(respawnedBackends) != 1 || respawnedBackends[0] != "opencode" {
		t.Fatalf("respawned backends = %v, want [opencode]", respawnedBackends)
	}
	health, ok := s.BackendHealth["codex"]
	if !ok {
		t.Fatal("BackendHealth[codex] should be recorded — it stayed empty during the incident")
	}
	if health.Reason != state.BackendBlockProviderLimit {
		t.Fatalf("BackendHealth[codex].Reason = %q, want %q (parseable reset → provider-limit path)", health.Reason, state.BackendBlockProviderLimit)
	}
	if health.RetryAfter == nil {
		t.Fatal("RetryAfter should carry the resolved 12:30 PM reset")
	}
	if hh, mm := health.RetryAfter.Hour(), health.RetryAfter.Minute(); hh != 12 || mm != 30 {
		t.Fatalf("RetryAfter = %v, want a 12:30 wall-clock resolution", health.RetryAfter)
	}
	if failed := s.FailedAttemptsForIssue(217); failed != 0 {
		t.Fatalf("FailedAttemptsForIssue(217) = %d, want 0", failed)
	}
}
