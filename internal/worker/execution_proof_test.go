package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
)

func TestWorkerExecProofRefusesBeforeActualProcessStart(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "launched")
	args := []string{"/bin/sh", "-c", "touch " + marker}
	policy := aiexecution.Policy{RequireVerifiedRoute: true}
	policy = policy.BindController(policy, dir)
	pin, err := policy.ControllerPin()
	if err != nil {
		t.Fatal(err)
	}
	lease, err := policy.LiveControllerLease()
	if err != nil {
		t.Fatal(err)
	}
	proof := workerExecutionProof{Version: 1, Policy: policy, ControllerRevision: pin, ControllerLease: lease, Arguments: args}
	path := filepath.Join(dir, "proof.json")
	data, _ := json.Marshal(proof)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	sha, err := workerExecutionProofPin(path)
	if err != nil {
		t.Fatal(err)
	}
	err = RunWorkerWithExecutionProof("", path, sha, args, bytes.NewReader(nil), io.Discard)
	var hold *aiexecution.Hold
	if !errors.As(err, &hold) || hold.Code != "manifest_pin_required" {
		t.Fatal(err)
	}
	if err := policy.Invalidate(); err != nil {
		t.Fatal(err)
	}
	err = RunWorkerWithExecutionProof("", path, sha, args, bytes.NewReader(nil), io.Discard)
	if !errors.As(err, &hold) || hold.Code != "controller_lease_unavailable" {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("invalid proof executed child")
	}
}

func TestStrictNativeWorkerActualRoutePreflightHoldsBeforeLease(t *testing.T) {
	if os.Getenv("MAESTRO_NATIVE_KERNEL_TESTS") != "1" {
		t.Skip("set MAESTRO_NATIVE_KERNEL_TESTS=1 for native clone kernel fixture")
	}
	f := nativeTestFixture(t)
	f.cfg.WorkerRuntime = config.WorkerRuntimeConfig{Mode: config.WorkerRuntimeModeIsolated, Scope: config.WorkerRuntimeScopeSystem}
	runBranchGit(t, f.cfg.LocalPath, "worktree", "remove", "--force", filepath.Join(f.cfg.WorktreeBase, f.slot))
	f.cfg.Repo = "acme/widget"
	f.cfg.Forge = config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: "https://forge.example.test"}
	runBranchGit(t, f.cfg.LocalPath, "remote", "set-url", "origin", fixtureNativeOrigin)
	f.cfg.AIExecution.RequireVerifiedRoute = true
	f.cfg.AIExecution = f.cfg.AIExecution.BindController(f.cfg.AIExecution, f.cfg.StateDir)
	_, err := f.start()
	var hold *aiexecution.Hold
	if !errors.As(err, &hold) || hold.Code != "manifest_pin_required" || f.spawned != 0 {
		t.Fatalf("error=%v spawned=%d", err, f.spawned)
	}
	if len(f.registered) != 1 {
		t.Fatal("native identity not registered before proof")
	}
}
