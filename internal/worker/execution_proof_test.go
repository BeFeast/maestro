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
	f := nativeTestFixture(t)
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
