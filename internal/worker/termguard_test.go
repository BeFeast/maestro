package worker

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/termguard"
	"github.com/befeast/maestro/internal/termguard/termguardtest"
)

// beyondPIDMax can never name a process: Linux caps pid_max at 2^22.
const beyondPIDMax = 1<<22 + 1

// TestKillProcessTreeConsultsTerminationGuardBeforeSignalling proves both
// tree-kill primitives ask the guard first and send nothing when it refuses:
// the spawned child must survive both calls (#1252).
func TestKillProcessTreeConsultsTerminationGuardBeforeSignalling(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("start sleep: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})

	var seen []termguard.Attempt
	restore := termguard.SetHookForTesting(func(a termguard.Attempt) error {
		seen = append(seen, a)
		return errors.New("refused by test")
	})
	KillProcessTree(cmd.Process.Pid)
	ForceKillProcessTree(cmd.Process.Pid)
	restore()

	want := termguard.Attempt{Op: termguard.OpKillProcessTree, PID: cmd.Process.Pid}
	if len(seen) != 2 || seen[0] != want || seen[1] != want {
		t.Fatalf("guard saw %v, want two %v attempts", seen, want)
	}
	select {
	case <-exited:
		t.Fatal("a refused tree kill still signalled the child")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestStopProcessRefusesLiteralLegacyPIDBeforeLivenessProbe pins the
// deterministic guard on the legacy teardown: a fixture PID that names no
// process (so IsAlive is false) is still refused, so a test that forgets its
// fake fails on every host instead of only where the number happens to be live.
func TestStopProcessRefusesLiteralLegacyPIDBeforeLivenessProbe(t *testing.T) {
	sess := &state.Session{Status: state.StatusRunning, PID: beyondPIDMax}
	err := termguardtest.ExpectRefusal(t, func() {
		_ = StopProcess("ok-player-1", sess)
	})
	if !strings.Contains(err.Error(), "pid="+strconv.Itoa(beyondPIDMax)) {
		t.Fatalf("refusal = %v, want it to name pid=%d", err, beyondPIDMax)
	}
}

// TestTerminationGuardStillAllowsOwnChildTree keeps real teardown coverage
// possible under the guard: a process this test binary spawned is killed.
func TestTerminationGuardStillAllowsOwnChildTree(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("start sleep: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	KillProcessTree(cmd.Process.Pid)

	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("wait = %v, want a signal exit", err)
		}
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() {
			t.Fatalf("child exit = %v, want killed by a signal", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("own child survived KillProcessTree")
	}
}
