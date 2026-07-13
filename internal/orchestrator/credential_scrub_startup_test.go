package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// writeLegacyCredentialArtifacts drops a pre-#888 daemon's leavings into
// stateDir: a per-worker `*-run.env` credential copy and a runner that inlined
// the credential export. The canary is assembled by concatenation so no
// contiguous secret literal exists in the source (agent-lint secret scanner).
func writeLegacyCredentialArtifacts(t *testing.T, stateDir string) (envPath, runnerPath, canary string) {
	t.Helper()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatalf("chmod state dir: %v", err)
	}
	canary = "CANARY-" + "oneshot-scrub-" + "d0n0t-persist-" + "0888"

	envPath = filepath.Join(stateDir, "slot-earlier-run.env")
	if err := os.WriteFile(envPath, []byte("export ANTHROPIC_AUTH_TOKEN="+canary+"\n"), 0o600); err != nil {
		t.Fatalf("write legacy env: %v", err)
	}

	runnerPath = filepath.Join(stateDir, "slot-a-run.sh")
	runnerBody := "#!/bin/bash\n" +
		"export MAESTRO_WORKTREE='/w'\n" +
		"export ANTHROPIC_AUTH_TOKEN='" + canary + "'\n" +
		"exec claude -p\n"
	if err := os.WriteFile(runnerPath, []byte(runnerBody), 0o755); err != nil {
		t.Fatalf("write legacy runner: %v", err)
	}
	return envPath, runnerPath, canary
}

// assertLegacyCredentialArtifactsScrubbed verifies the scrub removed the
// per-worker env copy and stripped the inlined credential value/export from the
// runner (the go-forward, value-free state).
func assertLegacyCredentialArtifactsScrubbed(t *testing.T, stateDir, runnerPath, canary string) {
	t.Helper()
	if perSlot, _ := filepath.Glob(filepath.Join(stateDir, "*-run.env")); len(perSlot) != 0 {
		t.Fatalf("per-worker credential copies survived the scrub: %v", perSlot)
	}
	runner, err := os.ReadFile(runnerPath)
	if err != nil {
		t.Fatalf("read runner: %v", err)
	}
	if strings.Contains(string(runner), canary) {
		t.Fatal("runner still holds the credential value after scrub")
	}
	if strings.Contains(string(runner), "export ANTHROPIC_AUTH_TOKEN=") {
		t.Fatal("runner still holds the inlined credential export after scrub")
	}
}

// TestRunStartupTasks_ScrubsCredentialsInOneShotMode is the #890 greptile P1
// regression guard: the credential-artifact scrub must run during a `run --once`
// reconcile, not only when a long-running daemon starts. A one-shot deployment
// would otherwise retain every historical `*-run.env` credential copy forever.
// It must also NOT lift a graceful-drain flag — that reconciliation is
// deliberately daemon-only (a `run --once` tick must not resume a drain an
// operator is mid-way through).
func TestRunStartupTasks_ScrubsCredentialsInOneShotMode(t *testing.T) {
	stateDir := t.TempDir()
	_, runnerPath, canary := writeLegacyCredentialArtifacts(t, stateDir)

	// A drain flag persisted by a previous process: the daemon-only reconcile
	// would clear it, a one-shot tick must not.
	s := state.NewState()
	s.SetSpawnDrain(time.Now().UTC().Add(-time.Minute))
	if err := state.Save(stateDir, s); err != nil {
		t.Fatalf("save drained state: %v", err)
	}

	o := &Orchestrator{cfg: &config.Config{StateDir: stateDir}}
	o.runStartupTasks(true)

	assertLegacyCredentialArtifactsScrubbed(t, stateDir, runnerPath, canary)

	reloaded, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if !reloaded.DrainActive() {
		t.Fatal("run --once lifted the drain flag; daemon-only reconcile must be skipped in one-shot mode")
	}
}

// TestRunStartupTasks_DaemonModeScrubsAndReconciles is the control: the
// long-running daemon path scrubs the same legacy artifacts AND performs the
// daemon-only reconciliation (here: clearing the leftover drain flag). This
// proves the scrub is shared by both modes while the reconcile steps remain
// daemon-only.
func TestRunStartupTasks_DaemonModeScrubsAndReconciles(t *testing.T) {
	stateDir := t.TempDir()
	_, runnerPath, canary := writeLegacyCredentialArtifacts(t, stateDir)

	s := state.NewState()
	s.SetSpawnDrain(time.Now().UTC().Add(-time.Minute))
	if err := state.Save(stateDir, s); err != nil {
		t.Fatalf("save drained state: %v", err)
	}

	o := &Orchestrator{cfg: &config.Config{StateDir: stateDir}}
	o.runStartupTasks(false)

	assertLegacyCredentialArtifactsScrubbed(t, stateDir, runnerPath, canary)

	reloaded, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if reloaded.DrainActive() {
		t.Fatal("daemon startup did not clear the leftover drain flag")
	}
}
