package worker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
)

func TestStrictWorkerOpaqueLeavesNeverLaunch(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "called")
	cfg := &config.Config{AIExecution: aiexecution.Policy{RequireVerifiedRoute: true}, Hooks: config.HooksConfig{TimeoutMs: 1000}}
	err := RunHook(cfg, "before_run", "touch "+marker, HookEnv{WorkspacePath: dir})
	var hold *aiexecution.Hold
	if !errors.As(err, &hold) || hold.Code != "opaque_hook_unsupported" {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("hook executed")
	}
	_, _, err = launchWorkerProcessLease(cfg, "slot", "tmux", dir, "missing-script", 1, 0, "initial_spawn")
	if !errors.As(err, &hold) || hold.Code != "worker_route_proof_unavailable" {
		t.Fatal(err)
	}
}
