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

// #808 live Claude/Fable subscription signatures (BeFeast ok-player / ok-folio
// fleet, 2026-07-03). Each was printed by the claude CLI as it exited on an
// account-level quota; the daemon saw only "pid dead, tmux missing" and looped
// respawns on the same exhausted backend until the issue was wrongly blocked.
// The Codex-shaped built-in patterns from #806 matched none of them.
const (
	// ok-player-23.log — no reset stated.
	claudeFableLimitDeath = "You've reached your Fable 5 limit. Run /usage-credits to continue or switch models with /model."
	// ok-player-18.log — "resets 9am (UTC)".
	claudeSessionLimitDeath = "You've hit your session limit · resets 9am (UTC)"
	// bot-34.log — "resets 4:10pm (UTC)".
	claudeExtraUsageDeath = "You're out of extra usage · resets 4:10pm (UTC)"
)

// claudeChainTestConfig models the desired failover chain fable → opus →
// codex: the default backend (claude/fable) with an ordered fallback list. The
// backend names are what the config store carries; the classifier itself is
// backend-agnostic (it scans the worker log), so the in-memory defs only need
// to exist and be enabled for the selector to pick them.
func claudeChainTestConfig() *config.Config {
	return &config.Config{
		Repo: "owner/repo",
		Model: config.ModelConfig{
			Default:          "claude",
			FallbackBackends: []string{"opus", "codex"},
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude"},
				"opus":   {Cmd: "claude"},
				"codex":  {Cmd: "codex"},
			},
		},
	}
}

// TestClassifyBackendFailure_ClaudeSignatures replays the three live #808
// strings through the real post-mortem log classifiers (detection +
// classifyBackendFailure). Each must classify as a backend usage-limit — not
// an ordinary work failure — so the attempt keeps its retry budget and fails
// over. The matched signature label records which built-in pattern fired.
func TestClassifyBackendFailure_ClaudeSignatures(t *testing.T) {
	cases := []struct {
		name    string
		death   string
		pattern string
	}{
		{"fable plan limit", claudeFableLimitDeath, "reached_limit"},
		{"session limit", claudeSessionLimitDeath, "hit_limit"},
		{"out of extra usage", claudeExtraUsageDeath, "out_of_usage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logFile := filepath.Join(dir, "worker.log")
			content := "Starting claude worker for issue #151\n" + tc.death + "\n"
			if err := os.WriteFile(logFile, []byte(content), 0644); err != nil {
				t.Fatalf("write log: %v", err)
			}

			o := &Orchestrator{cfg: claudeChainTestConfig()}
			sess := &state.Session{
				Backend:   "claude",
				StartedAt: time.Now().UTC().Add(-90 * time.Second),
				LogFile:   logFile,
			}
			hit, reason, pattern := o.classifyBackendFailure(sess, time.Now().UTC())
			if !hit {
				t.Fatalf("classifyBackendFailure(%q) = false, want a backend-failure classification", tc.death)
			}
			if reason != state.BackendBlockUsageLimit {
				t.Fatalf("reason = %q, want %q", reason, state.BackendBlockUsageLimit)
			}
			if pattern != tc.pattern {
				t.Errorf("pattern = %q, want %q", pattern, tc.pattern)
			}
		})
	}
}

// TestReconcileRunningSessions_ClaudeUsageLimit_IncidentReplay is the #808
// end-to-end regression: a claude/fable worker dies with each live signature
// and the fleet must fail over to the next backend in the chain (opus) with the
// per-issue retry budget preserved, instead of looping respawns on the
// exhausted default until the issue is wrongly blocked.
//
// The two signatures that state a reset ("resets 9am (UTC)", "resets 4:10pm
// (UTC)") take the provider-limit path and carry the provider-stated reset as
// the backend cooldown; the Fable-limit signature states no reset and takes the
// usage-limit path with the fixed re-probe window. Both preserve the budget.
func TestReconcileRunningSessions_ClaudeUsageLimit_IncidentReplay(t *testing.T) {
	cases := []struct {
		name       string
		death      string
		wantReason string
		wantReset  bool // provider-stated reset resolved onto RetryAfter
		resetHour  int
		resetMin   int
	}{
		{"fable plan limit no reset", claudeFableLimitDeath, state.BackendBlockUsageLimit, false, 0, 0},
		{"session limit resets 9am", claudeSessionLimitDeath, state.BackendBlockProviderLimit, true, 9, 0},
		{"out of extra usage resets 4:10pm", claudeExtraUsageDeath, state.BackendBlockProviderLimit, true, 16, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logFile := filepath.Join(dir, "ok-player.log")
			content := "Starting claude worker for issue #151\n" + tc.death + "\n"
			if err := os.WriteFile(logFile, []byte(content), 0644); err != nil {
				t.Fatalf("write log: %v", err)
			}

			s := state.NewState()
			s.Sessions["ok-player-1"] = &state.Session{
				IssueNumber: 151,
				IssueTitle:  "player pipeline",
				Status:      state.StatusRunning,
				PID:         515160,
				TmuxSession: "maestro-ok-player-1",
				Branch:      "feat/ok-player-1-151-pipeline",
				Backend:     "claude",
				StartedAt:   time.Now().UTC().Add(-90 * time.Second),
				LogFile:     logFile,
			}

			respawnedBackends := []string{}
			o := &Orchestrator{
				cfg:                 claudeChainTestConfig(),
				notifier:            &notify.Notifier{},
				pidAliveFn:          func(pid int) bool { return false },
				tmuxSessionExistsFn: func(name string) bool { return false },
				listOpenPRsFn:       func() ([]github.PR, error) { return []github.PR{}, nil },
				getIssueFn: func(number int) (github.Issue, error) {
					return github.Issue{Number: number, Title: "player pipeline"}, nil
				},
				respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
					respawnedBackends = append(respawnedBackends, backendName)
					sess.Status = state.StatusRunning
					sess.PID = 9110
					sess.Backend = backendName
					sess.StartedAt = time.Now().UTC()
					sess.FinishedAt = nil
					return nil
				},
			}

			if !o.reconcileRunningSessions(s) {
				t.Fatal("expected reconciliation to report changes")
			}

			sess := s.Sessions["ok-player-1"]
			if sess.Status != state.StatusRunning {
				t.Fatalf("status = %q, want running (fail over to opus), got — the #808 regression is a blocked loop", sess.Status)
			}
			if len(respawnedBackends) != 1 || respawnedBackends[0] != "opus" {
				t.Fatalf("respawned backends = %v, want [opus] (next in fable→opus→codex)", respawnedBackends)
			}
			if len(sess.TriedBackends) != 1 || sess.TriedBackends[0] != "claude" {
				t.Fatalf("TriedBackends = %v, want [claude] (the exhausted default)", sess.TriedBackends)
			}
			if sess.RetryCount != 0 {
				t.Fatalf("RetryCount = %d, want 0 — a quota death must not consume the retry budget", sess.RetryCount)
			}
			if !sess.RateLimitHit {
				t.Fatal("RateLimitHit should be true so FailedAttemptsForIssue excludes the session")
			}
			if failed := s.FailedAttemptsForIssue(151); failed != 0 {
				t.Fatalf("FailedAttemptsForIssue(151) = %d, want 0", failed)
			}
			health, ok := s.BackendHealth["claude"]
			if !ok {
				t.Fatal("BackendHealth[claude] should be gated — it stayed empty during the incident")
			}
			if health.State != state.BackendHealthCooldown {
				t.Fatalf("BackendHealth[claude].State = %q, want cooldown", health.State)
			}
			if health.Reason != tc.wantReason {
				t.Fatalf("BackendHealth[claude].Reason = %q, want %q", health.Reason, tc.wantReason)
			}
			if health.RetryAfter == nil {
				t.Fatal("BackendHealth[claude].RetryAfter should be set so the backend is re-probed after the cooldown")
			}
			if tc.wantReset {
				if hh, mm := health.RetryAfter.Hour(), health.RetryAfter.Minute(); hh != tc.resetHour || mm != tc.resetMin {
					t.Fatalf("RetryAfter = %v, want a %02d:%02d wall-clock resolution from the provider-stated reset", health.RetryAfter, tc.resetHour, tc.resetMin)
				}
			}
		})
	}
}
