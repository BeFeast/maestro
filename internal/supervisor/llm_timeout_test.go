package supervisor

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
)

func TestOutputWithTimeoutKillsHungBackend(t *testing.T) {
	started := time.Now()
	// Ignore SIGTERM so this catches accidental use of the graceful worker
	// reaper, whose two-second grace period would violate the hard deadline.
	_, err := outputWithTimeout(exec.Command("sh", "-c", `trap '' TERM; while :; do sleep 1; done`), 25*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("hung backend returned after %s, want bounded cancellation", elapsed)
	}
}

func TestSupervisorBackendCandidatesUsesOrderedFallbacks(t *testing.T) {
	disabled := false
	cfg := &config.Config{Model: config.ModelConfig{
		Default:          "claude",
		FallbackBackends: []string{"claude", "sol", "gpt55"},
		Backends: map[string]config.BackendDef{
			"claude": {Cmd: "claude"},
			"sol":    {Cmd: "codex"},
			"gpt55":  {Enabled: &disabled, Cmd: "codex"},
		},
	}}
	candidates, err := supervisorBackendCandidates(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].name != "claude" || candidates[1].name != "sol" {
		t.Fatalf("candidates = %+v, want [claude sol]", candidates)
	}
}
