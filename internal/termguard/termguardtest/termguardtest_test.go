package termguardtest

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/termguard"
)

// beyondPIDMax can never name a process: Linux caps pid_max at 2^22.
const beyondPIDMax = 1<<22 + 1

func TestGuardAllowsOnlyDescendantProcessTrees(t *testing.T) {
	// self=100; 200 is a child, 300 a grandchild; 400 hangs off init; 500
	// reports itself as its own parent.
	parents := map[int]int{200: 100, 300: 200, 400: 1, 500: 500, 100: 50, 50: 1}
	g := newGuard(100, func(pid int) (int, bool) {
		ppid, ok := parents[pid]
		return ppid, ok
	})
	allowed := []int{200, 300}
	refusedPIDs := []int{0, -1, 100, 50, 1, 400, 500, 4242, beyondPIDMax}
	for _, pid := range allowed {
		if err := g.decide(termguard.Attempt{Op: termguard.OpKillProcessTree, PID: pid}); err != nil {
			t.Errorf("decide(kill-process-tree pid=%d) = %v, want allowed descendant", pid, err)
		}
	}
	for _, pid := range refusedPIDs {
		err := g.decide(termguard.Attempt{Op: termguard.OpKillProcessTree, PID: pid})
		if err == nil || !strings.Contains(err.Error(), "#1252") {
			t.Errorf("decide(kill-process-tree pid=%d) = %v, want a #1252 refusal", pid, err)
		}
	}
}

func TestGuardRefusesEveryTmuxKill(t *testing.T) {
	g := newGuard(100, func(int) (int, bool) { return 100, true })
	err := g.decide(termguard.Attempt{Op: termguard.OpKillTmuxSession, Target: "maestro-ok-player-1"})
	if err == nil || !strings.Contains(err.Error(), `session="maestro-ok-player-1"`) {
		t.Fatalf("decide(kill-tmux-session) = %v, want a refusal naming the session", err)
	}
}

func TestGuardRecognisesARealSpawnedChild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process ancestry is read from /proc")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	g := newGuard(os.Getpid(), parentPID)
	if err := g.decide(termguard.Attempt{Op: termguard.OpKillProcessTree, PID: cmd.Process.Pid}); err != nil {
		t.Fatalf("decide(own child) = %v, want allowed", err)
	}
	if err := g.decide(termguard.Attempt{Op: termguard.OpKillProcessTree, PID: os.Getppid()}); err == nil {
		t.Fatal("decide(parent of the test binary) allowed, want refused")
	}
	if err := g.decide(termguard.Attempt{Op: termguard.OpKillProcessTree, PID: beyondPIDMax}); err == nil {
		t.Fatal("decide(nonexistent pid) allowed, want refused")
	}
}

func TestCheckRefusesWithPanicAndExpectRefusalAbsorbsIt(t *testing.T) {
	g := newGuard(100, func(int) (int, bool) { return 0, false })
	before := refused.Load()
	err := ExpectRefusal(t, func() {
		_ = g.check(termguard.Attempt{Op: termguard.OpKillProcessTree, PID: 4242})
	})
	if err == nil || !strings.Contains(err.Error(), "pid=4242") {
		t.Fatalf("ExpectRefusal = %v, want the pid=4242 refusal", err)
	}
	if got := refused.Load(); got != before {
		t.Fatalf("refused counter = %d after an expected refusal, want %d", got, before)
	}
}

func TestRefuseCountsUnexpectedRefusals(t *testing.T) {
	before := refused.Load()
	defer func() {
		r := recover()
		var refusal *Refusal
		if e, ok := r.(error); !ok || !errors.As(e, &refusal) {
			t.Fatalf("Refuse panicked with %v, want *Refusal", r)
		}
		if got := refused.Load(); got != before+1 {
			t.Fatalf("refused counter = %d, want %d", got, before+1)
		}
		refused.Add(-1)
	}()
	Refuse(errors.New("boom"))
}
