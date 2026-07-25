package orphan

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeActuator struct {
	terminated  []int
	removed     []string
	termErr     error
	removeErr   error
	removeCalls int
}

func (f *fakeActuator) TerminateGroup(pgid int, pids []int) error {
	f.terminated = append(f.terminated, pgid)
	return f.termErr
}

func (f *fakeActuator) RemoveContainer(name string) error {
	f.removeCalls++
	f.removed = append(f.removed, name)
	return f.removeErr
}

// Acceptance 3 + 4: the reaper actuates the plan (terminate the stale wrapper
// group, remove the exited container) and records durable, secret-free cleanup
// evidence for each action.
func TestReap_ActuatesAndRecordsEvidence(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	plan := Decide(incidentSnapshot(now))
	act := &fakeActuator{}
	cleanups := NewReaper(act).Reap(plan)

	if len(act.terminated) != 1 || act.terminated[0] != 100 {
		t.Fatalf("terminated groups = %v, want [100]", act.terminated)
	}
	if len(act.removed) != 1 || act.removed[0] != "pkg-verify-42" {
		t.Fatalf("removed containers = %v, want [pkg-verify-42]", act.removed)
	}
	if len(cleanups) != 2 {
		t.Fatalf("cleanups = %d, want 2 (one terminate, one remove)", len(cleanups))
	}

	var sawTerminate, sawRemove bool
	for _, c := range cleanups {
		if c.Outcome != CleanupSucceeded {
			t.Errorf("cleanup %+v outcome = %q, want succeeded", c, c.Outcome)
		}
		switch c.Action {
		case CleanupTerminateGroup:
			sawTerminate = true
			if c.PGID != 100 || c.Reason != ReapTerminalContainerNonLiveSession {
				t.Errorf("terminate cleanup = %+v", c)
			}
		case CleanupRemoveContainer:
			sawRemove = true
			if c.ContainerName != "pkg-verify-42" || c.IssueNumber != 42 {
				t.Errorf("remove cleanup = %+v", c)
			}
		}
	}
	if !sawTerminate || !sawRemove {
		t.Fatalf("missing cleanup action: terminate=%v remove=%v", sawTerminate, sawRemove)
	}

	summary := SummarizeCleanups(cleanups)
	if !strings.Contains(summary, "terminated 1") || !strings.Contains(summary, "removed 1") {
		t.Errorf("summary = %q, want it to report 1 terminated + 1 removed", summary)
	}
	if strings.Contains(summary, "/") {
		t.Errorf("summary leaked a path-like token: %q", summary)
	}
}

// A failed actuation is recorded as a failed cleanup and does not abort the
// remaining actions.
func TestReap_FailedActionRecordedAndContinues(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	plan := Decide(incidentSnapshot(now))
	act := &fakeActuator{termErr: errors.New("/proc/100: operation not permitted")}
	cleanups := NewReaper(act).Reap(plan)

	if act.removeCalls != 1 {
		t.Fatalf("remove not attempted after terminate failure: calls=%d", act.removeCalls)
	}
	var termCleanup *Cleanup
	for i := range cleanups {
		if cleanups[i].Action == CleanupTerminateGroup {
			termCleanup = &cleanups[i]
		}
	}
	if termCleanup == nil || termCleanup.Outcome != CleanupFailed {
		t.Fatalf("terminate cleanup = %+v, want a failed record", termCleanup)
	}
	// Detail must be a bounded class token, never the raw path from the error.
	if strings.Contains(termCleanup.Detail, "/proc") || strings.Contains(termCleanup.Detail, "permitted") {
		t.Errorf("cleanup detail leaked raw error content: %q", termCleanup.Detail)
	}
}

// The OSActuator refuses to signal an unsafe process group (0 or 1) so a bug in
// the collector can never target init or the daemon's own group.
func TestOSActuator_RefusesUnsafeGroup(t *testing.T) {
	a := &OSActuator{}
	for _, pgid := range []int{0, 1, -5} {
		if err := a.TerminateGroup(pgid, nil); err == nil {
			t.Errorf("TerminateGroup(%d) = nil, want refusal", pgid)
		}
	}
}

// The OSActuator treats an already-gone process group (ESRCH) as success and
// escalates SIGTERM -> SIGKILL only when the group survives the grace period.
func TestOSActuator_TerminateEscalatesAndToleratesGone(t *testing.T) {
	t.Run("already gone is success", func(t *testing.T) {
		a := &OSActuator{killGroup: func(int, syscall.Signal) error { return syscall.ESRCH }}
		if err := a.TerminateGroup(1234, nil); err != nil {
			t.Fatalf("TerminateGroup gone group = %v, want nil", err)
		}
	})

	t.Run("escalates to sigkill when still alive", func(t *testing.T) {
		var sigs []syscall.Signal
		a := &OSActuator{
			KillGrace:  10 * time.Millisecond,
			killGroup:  func(_ int, s syscall.Signal) error { sigs = append(sigs, s); return nil },
			groupAlive: func(int) bool { return true }, // never dies
		}
		if err := a.TerminateGroup(1234, nil); err != nil {
			t.Fatalf("TerminateGroup = %v", err)
		}
		if len(sigs) != 2 || sigs[0] != syscall.SIGTERM || sigs[1] != syscall.SIGKILL {
			t.Fatalf("signals = %v, want [SIGTERM SIGKILL]", sigs)
		}
	})

	t.Run("no sigkill when group exits within grace", func(t *testing.T) {
		var sigs []syscall.Signal
		a := &OSActuator{
			KillGrace:  time.Second,
			killGroup:  func(_ int, s syscall.Signal) error { sigs = append(sigs, s); return nil },
			groupAlive: func(int) bool { return false },
		}
		if err := a.TerminateGroup(1234, nil); err != nil {
			t.Fatalf("TerminateGroup = %v", err)
		}
		if len(sigs) != 1 || sigs[0] != syscall.SIGTERM {
			t.Fatalf("signals = %v, want [SIGTERM] only", sigs)
		}
	})
}

// RemoveContainer routes through the configured docker args and reports a
// bounded error class on failure.
func TestOSActuator_RemoveContainerUsesConfiguredArgs(t *testing.T) {
	var gotArgs []string
	a := &OSActuator{
		DockerArgs:      []string{"sudo", "docker"},
		removeContainer: func(_ context.Context, args []string) error { gotArgs = args; return nil },
	}
	if err := a.RemoveContainer("pkg-verify-42"); err != nil {
		t.Fatalf("RemoveContainer = %v", err)
	}
	want := []string{"sudo", "docker", "rm", "pkg-verify-42"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Fatalf("docker args = %v, want %v", gotArgs, want)
	}
	if err := a.RemoveContainer(""); err == nil {
		t.Fatal("RemoveContainer(\"\") = nil, want error")
	}
}
