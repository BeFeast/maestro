package orchestrator

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
)

func TestDispatchHoldForCycleCoversRequiredReasonClasses(t *testing.T) {
	now := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	cfg := cfgWithBackends("claude", "claude", "codex")
	cfg.MaxParallel = 1
	o := &Orchestrator{cfg: cfg}

	t.Run("paused", func(t *testing.T) {
		s := state.NewState()
		s.SetPaused(now.Add(-time.Minute))
		hold := o.dispatchHoldForCycle(s, s.Capacity(capacityInput(cfg)), 1, now)
		if !hold.Active || hold.ReasonClass != state.DispatchHoldPaused || !hold.Since.Equal(now.Add(-time.Minute)) {
			t.Fatalf("hold = %+v, want paused since operator timestamp", hold)
		}
	})

	t.Run("blocking outcome check", func(t *testing.T) {
		s := state.NewState()
		s.OutcomeHealth = &outcome.HealthCheckResult{
			CheckedAt: now.Add(-2 * time.Minute),
			State:     outcome.HealthFailing,
			Summary:   "free-form summary must-not-enter-dispatch-hold",
			Detail:    "raw verifier output must-not-enter-dispatch-hold",
			Checks: []outcome.HealthCheckItem{{
				Name: "source-main-ci", Blocking: true, Status: "fail",
			}},
		}
		s.RecordSupervisorDecision(state.SupervisorDecision{
			ID:                "outcome-red",
			CreatedAt:         now,
			RecommendedAction: supervisor.ActionCheckOutcomeHealth,
		}, state.DefaultSupervisorDecisionLimit)
		hold := o.dispatchHoldForCycle(s, s.Capacity(capacityInput(cfg)), 1, now)
		if !hold.Active || hold.ReasonClass != state.DispatchHoldBlockingOutcomeCheck || !strings.Contains(hold.Detail, "source-main-ci") {
			t.Fatalf("hold = %+v, want failing blocking check name", hold)
		}
		if strings.Contains(hold.Detail, "must-not-enter") {
			t.Fatalf("unsafe outcome summary/detail leaked into hold: %q", hold.Detail)
		}
	})

	t.Run("all backends cooling down", func(t *testing.T) {
		s := state.NewState()
		retry := now.Add(5 * time.Minute)
		s.BackendHealth["claude"] = state.BackendHealth{State: state.BackendHealthCooldown, Reason: state.BackendBlockAuthFailure, RetryAfter: &retry}
		s.BackendHealth["codex"] = state.BackendHealth{State: state.BackendHealthCooldown, Reason: state.BackendBlockUsageLimit, RetryAfter: &retry}
		hold := o.dispatchHoldForCycle(s, s.Capacity(capacityInput(cfg)), 1, now)
		if !hold.Active || hold.ReasonClass != state.DispatchHoldBackendsCoolingDown || !strings.Contains(hold.Detail, retry.Format(time.RFC3339)) {
			t.Fatalf("hold = %+v, want backend cooldown with recovery clock", hold)
		}
	})

	t.Run("capacity blocked by PR gates", func(t *testing.T) {
		s := state.NewState()
		s.Sessions["gate-1"] = &state.Session{IssueNumber: 7, PRNumber: 70, Status: state.StatusPROpen}
		capacity := s.Capacity(capacityInput(cfg))
		hold := o.dispatchHoldForCycle(s, capacity, 1, now)
		if !hold.Active || hold.ReasonClass != state.DispatchHoldPRGateCapacity || !strings.Contains(hold.Detail, "1 PR gate") {
			t.Fatalf("hold = %+v, want PR-gate capacity blocker", hold)
		}
	})

	t.Run("queue empty with zero live workers", func(t *testing.T) {
		s := state.NewState()
		hold := o.dispatchHoldForCycle(s, s.Capacity(capacityInput(cfg)), 0, now)
		if !hold.Active || hold.ReasonClass != state.DispatchHoldQueueEmpty {
			t.Fatalf("hold = %+v, want queue_empty idle stall", hold)
		}
	})
}

func TestRunOnceEmitsOneIdleStallAfterTwoOutcomeHeldCycles(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := cfgWithBackends("codex", "codex")
	cfg.Repo = "owner/dispatch-visibility"
	cfg.StateDir = t.TempDir()
	cfg.MaxParallel = 1
	cfg.IssueLabels = []string{"maestro-ready"}
	enabled := true
	cfg.Supervisor.DynamicWave.Enabled = &enabled
	cfg.Supervisor.DynamicWave.OwnsReadyLabel = true

	now := time.Now().UTC()
	s := state.NewState()
	s.OutcomeHealth = &outcome.HealthCheckResult{
		CheckedAt: now.Add(-time.Minute),
		State:     outcome.HealthFailing,
		Summary:   "raw outcome summary is not a dispatch payload",
		Checks: []outcome.HealthCheckItem{{
			Name: "source-main-ci", Blocking: true, Status: "fail",
		}},
	}
	s.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "outcome-hold",
		CreatedAt:         now,
		Project:           cfg.Repo,
		RecommendedAction: supervisor.ActionCheckOutcomeHealth,
		PolicyRule:        supervisor.PolicyRuleRuntimeState,
		QueueAnalysis: &state.SupervisorQueueAnalysis{
			OpenIssues:         1,
			EligibleCandidates: 1,
			SelectedCandidate:  &state.SupervisorIssueCandidate{Number: 1023, Title: "ready issue"},
			EligibleRanked:     []state.SupervisorIssueCandidate{{Number: 1023, Title: "ready issue"}},
		},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(cfg.StateDir, s); err != nil {
		t.Fatalf("save state: %v", err)
	}

	n := (&notify.Notifier{}).WithNtfy(server.URL, "dispatch-alerts", "")
	o := &Orchestrator{
		cfg:      cfg,
		repo:     cfg.Repo,
		notifier: n,
		router:   router.New(cfg),
		listOpenPRsFn: func() ([]github.PR, error) {
			return nil, nil
		},
		listOpenIssuesFn: func([]string) ([]github.Issue, error) {
			return []github.Issue{makeIssue(1023, "ready issue", "maestro-ready")}, nil
		},
	}

	for cycle := 1; cycle <= 3; cycle++ {
		if err := o.RunOnce(); err != nil {
			t.Fatalf("RunOnce cycle %d: %v", cycle, err)
		}
		mu.Lock()
		got := len(bodies)
		mu.Unlock()
		want := 0
		if cycle >= 2 {
			want = 1
		}
		if got != want {
			t.Fatalf("notifications after cycle %d = %d, want %d", cycle, got, want)
		}
	}

	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load final state: %v", err)
	}
	if !loaded.DispatchHold.Active || loaded.DispatchHold.ReasonClass != state.DispatchHoldBlockingOutcomeCheck || !strings.Contains(loaded.DispatchHold.Detail, "source-main-ci") {
		t.Fatalf("dispatch hold = %+v, want active source-main-ci blocker", loaded.DispatchHold)
	}
	if !loaded.IdleStall.Notified || loaded.IdleStall.ConsecutiveCycles != 3 {
		t.Fatalf("idle stall = %+v, want one notified three-cycle streak", loaded.IdleStall)
	}
	mu.Lock()
	body := bodies[0]
	mu.Unlock()
	if !strings.Contains(body, "dispatch_hold.reason_class="+state.DispatchHoldBlockingOutcomeCheck) || !strings.Contains(body, "source-main-ci") {
		t.Fatalf("notification body = %q, want reason class and blocking check", body)
	}
	if strings.Contains(body, "raw outcome summary") {
		t.Fatalf("notification leaked raw outcome summary: %q", body)
	}
}
