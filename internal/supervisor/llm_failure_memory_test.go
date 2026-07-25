package supervisor

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
)

// Per-backend override wins, then the supervisor-level knob, then the 45s
// default.
func TestSupervisorAttemptTimeoutResolution(t *testing.T) {
	cfg := &config.Config{}
	if got := supervisorAttemptTimeoutFor(cfg, config.BackendDef{}); got != 45*time.Second {
		t.Fatalf("default = %v, want 45s", got)
	}
	cfg.Supervisor.AttemptTimeoutSeconds = 90
	if got := supervisorAttemptTimeoutFor(cfg, config.BackendDef{}); got != 90*time.Second {
		t.Fatalf("global = %v, want 90s", got)
	}
	def := config.BackendDef{SupervisorAttemptTimeoutSeconds: 180}
	if got := supervisorAttemptTimeoutFor(cfg, def); got != 180*time.Second {
		t.Fatalf("per-backend = %v, want 180s", got)
	}
	if got := supervisorTotalTimeoutFor(cfg); got != 3*time.Minute {
		t.Fatalf("total default = %v, want 3m", got)
	}
	cfg.Supervisor.TotalTimeoutSeconds = 300
	if got := supervisorTotalTimeoutFor(cfg); got != 300*time.Second {
		t.Fatalf("total = %v, want 300s", got)
	}
}

// Three consecutive failures open a skip window; a success resets everything;
// an expired window re-allows probing.
func TestSupervisorBackendMemoryLifecycle(t *testing.T) {
	m := newSupervisorBackendMemory()
	now := time.Now()

	for i := 0; i < supervisorBackendFailureThreshold-1; i++ {
		if _, opened := m.recordFailure("claude-fable", now); opened {
			t.Fatalf("skip window opened after %d failures, want threshold %d", i+1, supervisorBackendFailureThreshold)
		}
	}
	if _, skip := m.shouldSkip("claude-fable", now); skip {
		t.Fatal("skipping before threshold reached")
	}
	until, opened := m.recordFailure("claude-fable", now)
	if !opened {
		t.Fatal("threshold failure did not open a skip window")
	}
	if want := now.Add(supervisorBackendFailureSkipWindow); !until.Equal(want) {
		t.Fatalf("window until = %v, want %v", until, want)
	}
	if _, skip := m.shouldSkip("claude-fable", now.Add(time.Minute)); !skip {
		t.Fatal("not skipping inside the window")
	}
	if _, skip := m.shouldSkip("claude-fable", now.Add(supervisorBackendFailureSkipWindow+time.Second)); skip {
		t.Fatal("still skipping after the window expired")
	}

	// Success resets both the counter and any window.
	m.recordFailure("sol", now)
	m.recordFailure("sol", now)
	m.recordSuccess("sol")
	for i := 0; i < supervisorBackendFailureThreshold-1; i++ {
		if _, opened := m.recordFailure("sol", now); opened {
			t.Fatal("success did not reset the consecutive-failure count")
		}
	}
}

// A nil memory (custom clients) is inert.
func TestSupervisorBackendMemoryNilSafe(t *testing.T) {
	var m *supervisorBackendMemory
	if _, skip := m.shouldSkip("x", time.Now()); skip {
		t.Fatal("nil memory must not skip")
	}
	if _, opened := m.recordFailure("x", time.Now()); opened {
		t.Fatal("nil memory must not open windows")
	}
	m.recordSuccess("x")
}
