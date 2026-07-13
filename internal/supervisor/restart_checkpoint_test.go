package supervisor

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// #877: a session the daemon deliberately checkpointed on shutdown is marked
// running with a dead pid only until the next reconcile resumes it in place. It
// is self-healing, not stuck, so the supervisor must NOT raise a blocked
// dead_running_pid finding that nudges an operator to reconcile it. An
// unmarked running-with-dead-pid session must still surface, so the guard is
// scoped strictly to the restart-checkpoint marker.
func TestDetectWorkerStuckStates_RestartCheckpointSuppressesDeadPid(t *testing.T) {
	now := time.Now().UTC()
	hasDeadPid := func(findings []state.SupervisorStuckState) bool {
		for _, f := range findings {
			if f.Code == "dead_running_pid" {
				return true
			}
		}
		return false
	}

	t.Run("checkpointed session is suppressed", func(t *testing.T) {
		stamp := now.Add(-10 * time.Second)
		st := state.NewState()
		st.Sessions["sup-310"] = &state.Session{
			IssueNumber:         310,
			Status:              state.StatusRunning,
			PID:                 0, // process gone after the restart
			StartedAt:           now,
			RestartCheckpointAt: &stamp,
		}
		eng := testEngine(testConfig(t), &fakeReader{})
		findings := eng.detectWorkerStuckStates(st, now, newResolutionCache(eng.reader))
		if hasDeadPid(findings) {
			t.Fatal("a restart-checkpointed session must not raise dead_running_pid")
		}
	})

	t.Run("unmarked dead-pid session still surfaces", func(t *testing.T) {
		st := state.NewState()
		st.Sessions["sup-311"] = &state.Session{
			IssueNumber: 311,
			Status:      state.StatusRunning,
			PID:         0,
			StartedAt:   now,
			// RestartCheckpointAt nil.
		}
		eng := testEngine(testConfig(t), &fakeReader{})
		findings := eng.detectWorkerStuckStates(st, now, newResolutionCache(eng.reader))
		if !hasDeadPid(findings) {
			t.Fatal("an unmarked running-with-dead-pid session must still raise dead_running_pid")
		}
	})
}
