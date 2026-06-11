package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/befeast/maestro/internal/state"
)

// TestPauseAndResumeCmd_TogglePersistedFlag exercises the #683 CLI verbs
// end-to-end against a real state dir: pause sets the persisted flag the
// orchestrator honors, resume clears it.
func TestPauseAndResumeCmd_TogglePersistedFlag(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	configPath := filepath.Join(dir, "maestro.yaml")
	cfgYAML := "repo: owner/repo\nstate_dir: " + stateDir + "\n"
	if err := os.WriteFile(configPath, []byte(cfgYAML), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	pauseCmd([]string{"--config", configPath})

	s, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("load state after pause: %v", err)
	}
	if !s.PauseActive() {
		t.Fatal("paused flag not set after pauseCmd")
	}
	if s.PausedAt.IsZero() {
		t.Fatal("PausedAt not stamped by pauseCmd")
	}

	// Idempotent: pausing an already-paused project must not flip or
	// re-stamp anything.
	before := s.PausedAt
	pauseCmd([]string{"--config", configPath})
	s, err = state.Load(stateDir)
	if err != nil {
		t.Fatalf("load state after second pause: %v", err)
	}
	if !s.PauseActive() || !s.PausedAt.Equal(before) {
		t.Fatalf("second pause changed state: active=%v at=%v want at=%v", s.PauseActive(), s.PausedAt, before)
	}

	resumeCmd([]string{"--config", configPath})

	s, err = state.Load(stateDir)
	if err != nil {
		t.Fatalf("load state after resume: %v", err)
	}
	if s.PauseActive() {
		t.Fatal("paused flag still set after resumeCmd")
	}
}
