package daemon

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

func TestClearSupervisorStuckOnStartupClearsStaleFlag(t *testing.T) {
	stateDir := t.TempDir()
	st := state.NewState()
	st.LastRunOnceAt = time.Now().UTC().Add(-time.Hour)
	st.SupervisorStuck = true
	st.SupervisorStuckReason = "old watchdog finding"
	if err := state.Save(stateDir, st); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	clearSupervisorStuckOnStartup("project", stateDir)

	reloaded, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if reloaded.SupervisorStuck {
		t.Fatal("SupervisorStuck still true after startup clear")
	}
	if reloaded.SupervisorStuckReason != "" {
		t.Fatalf("SupervisorStuckReason = %q, want empty", reloaded.SupervisorStuckReason)
	}
	if reloaded.LastRunOnceAt.IsZero() {
		t.Fatal("LastRunOnceAt was cleared; startup clear should only reset the stale watchdog flag")
	}
}
