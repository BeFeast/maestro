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
	_, err := outputWithTimeout(exec.Command("sh", "-c", "sleep 5"), 25*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want timeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
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
