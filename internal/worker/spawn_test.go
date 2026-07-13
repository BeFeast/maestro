package worker

import (
	"fmt"
	"reflect"
	"testing"
)

// #877: a worker's tmux server must be launched outside maestro.service's
// control group so a self-deploy `systemctl restart` cannot sweep an in-flight
// worker out with the daemon. These tests pin the argv the launcher builds and
// its fallback behavior when the scope launch is unavailable.

func TestTmuxNewSessionArgv(t *testing.T) {
	got := tmuxNewSessionArgv("maestro-slot-1", "/wt/slot-1", "/state/slot-1-run.sh")
	want := []string{"tmux", "new-session", "-d", "-s", "maestro-slot-1", "-c", "/wt/slot-1", "bash", "/state/slot-1-run.sh"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tmuxNewSessionArgv = %v, want %v", got, want)
	}
}

func TestScopeWrapArgv(t *testing.T) {
	base := tmuxNewSessionArgv("maestro-slot-1", "/wt/slot-1", "/state/slot-1-run.sh")
	got := scopeWrapArgv(base)

	// systemd-run --user --scope places the tmux server in a transient scope
	// cgroup that is a SIBLING of maestro.service.
	if got[0] != "systemd-run" {
		t.Fatalf("scope argv[0] = %q, want systemd-run", got[0])
	}
	if !contains(got, "--user") || !contains(got, "--scope") {
		t.Fatalf("scope argv missing --user/--scope: %v", got)
	}
	// The `--` terminator must precede the wrapped command so tmux's own args
	// are never parsed by systemd-run.
	dashIdx, tmuxIdx := indexOf(got, "--"), indexOf(got, "tmux")
	if dashIdx < 0 || tmuxIdx < 0 || dashIdx >= tmuxIdx {
		t.Fatalf("expected `--` before tmux in %v (dash=%d tmux=%d)", got, dashIdx, tmuxIdx)
	}
	// The wrapped command must be the base argv verbatim, in order.
	if !reflect.DeepEqual(got[tmuxIdx:], base) {
		t.Fatalf("wrapped argv = %v, want base %v", got[tmuxIdx:], base)
	}
}

func TestLaunchTmuxSession_BareWhenNotUnderSystemd(t *testing.T) {
	defer restoreSpawnSeams(runUnderSystemdScope, execTmuxCombined)
	runUnderSystemdScope = func() bool { return false }

	var calls [][]string
	execTmuxCombined = func(argv []string) ([]byte, error) {
		calls = append(calls, argv)
		return nil, nil
	}

	if _, err := LaunchTmuxSession("maestro-slot-1", "/wt/slot-1", "/state/run.sh"); err != nil {
		t.Fatalf("LaunchTmuxSession: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected exactly one exec, got %d: %v", len(calls), calls)
	}
	if calls[0][0] != "tmux" {
		t.Fatalf("expected bare tmux launch, got %v", calls[0])
	}
}

func TestLaunchTmuxSession_ScopeWhenUnderSystemd(t *testing.T) {
	defer restoreSpawnSeams(runUnderSystemdScope, execTmuxCombined)
	runUnderSystemdScope = func() bool { return true }

	var calls [][]string
	execTmuxCombined = func(argv []string) ([]byte, error) {
		calls = append(calls, argv)
		return nil, nil
	}

	if _, err := LaunchTmuxSession("maestro-slot-1", "/wt/slot-1", "/state/run.sh"); err != nil {
		t.Fatalf("LaunchTmuxSession: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected exactly one exec, got %d: %v", len(calls), calls)
	}
	if calls[0][0] != "systemd-run" {
		t.Fatalf("expected scope-wrapped launch, got %v", calls[0])
	}
}

func TestLaunchTmuxSession_FallsBackOnScopeFailure(t *testing.T) {
	defer restoreSpawnSeams(runUnderSystemdScope, execTmuxCombined)
	runUnderSystemdScope = func() bool { return true }

	var calls [][]string
	execTmuxCombined = func(argv []string) ([]byte, error) {
		calls = append(calls, argv)
		if argv[0] == "systemd-run" {
			return []byte("Failed to connect to bus"), fmt.Errorf("systemd-run failed")
		}
		return nil, nil // kill-session + bare new-session succeed
	}

	if _, err := LaunchTmuxSession("maestro-slot-1", "/wt/slot-1", "/state/run.sh"); err != nil {
		t.Fatalf("LaunchTmuxSession should fall back cleanly, got err: %v", err)
	}
	// Expect: scope attempt (fails) → kill-session (clear partial) → bare launch.
	if len(calls) != 3 {
		t.Fatalf("expected 3 execs (scope, kill-session, bare), got %d: %v", len(calls), calls)
	}
	if calls[0][0] != "systemd-run" {
		t.Fatalf("first exec should be scope attempt, got %v", calls[0])
	}
	if !(calls[1][0] == "tmux" && calls[1][1] == "kill-session") {
		t.Fatalf("second exec should clear a partial session, got %v", calls[1])
	}
	if !(calls[2][0] == "tmux" && calls[2][1] == "new-session") {
		t.Fatalf("third exec should be the bare in-cgroup launch, got %v", calls[2])
	}
}

func restoreSpawnSeams(underSystemd func() bool, exec func([]string) ([]byte, error)) {
	runUnderSystemdScope = underSystemd
	execTmuxCombined = exec
}

func contains(ss []string, want string) bool { return indexOf(ss, want) >= 0 }

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}
