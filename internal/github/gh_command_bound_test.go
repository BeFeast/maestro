package github

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestGhCommandCarriesADeadline pins the #1119 invariant at the construction
// site: every plain `gh` call in this package goes through ghCommand, so
// ghCommand must never hand back a command that can run forever. Before the fix
// it was a bare exec.Command — no context, no deadline, no way to kill it — and
// a gh that never returned wedged the whole supervise cycle.
func TestGhCommandCarriesADeadline(t *testing.T) {
	cmd := ghCommand("api", "rate_limit")
	if cmd.Cancel == nil {
		t.Error("ghCommand returned a command with no Cancel hook: it was not built from a deadline context, so a hung gh can never be killed (#1119)")
	}
	if cmd.WaitDelay <= 0 {
		t.Error("ghCommand returned a command with no WaitDelay: a child holding the output pipe still blocks Wait after the deadline kill (#1119)")
	}
}

// TestGhCommandKillsProcessGroupOnDeadline runs the real ghCommand path against
// a stub that outlives its deadline and leaves a background child holding the
// output pipe — the shape that makes an unbounded gh hang a cycle. The call must
// return on the deadline, and the child must die with the group: killing only
// the leader leaves the pipe open and Wait blocked.
func TestGhCommandKillsProcessGroupOnDeadline(t *testing.T) {
	childPID := filepath.Join(t.TempDir(), "child.pid")
	stubGHExecutable(t, "#!/bin/sh\nsleep 60 &\necho $! > "+childPID+"\nsleep 60\n")

	restoreTimeout := ghTimeout
	ghTimeout = 200 * time.Millisecond
	t.Cleanup(func() { ghTimeout = restoreTimeout })

	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		_, err := ghCommand("api", "rate_limit").CombinedOutput()
		done <- result{err: err, elapsed: time.Since(start)}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("ghCommand never returned: the gh subprocess is unbounded, which is exactly the wedge in #1119")
	}
	if got.err == nil {
		t.Fatalf("ghCommand succeeded after %s, want a deadline failure", got.elapsed)
	}
	if got.elapsed > 10*time.Second {
		t.Fatalf("ghCommand returned after %s, want ~%s: the deadline did not bound it", got.elapsed, ghTimeout)
	}

	pid := readStubChildPID(t, childPID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("gh child pid %d survived the deadline: only the process leader was killed, so a child can still hold the output pipe open (#1119)", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stubGHExecutable points ghExecutable at a throwaway script so a bound can be
// exercised without spawning the real CLI (and without touching the network).
func stubGHExecutable(t *testing.T, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gh-stub")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}
	restore := ghExecutable
	ghExecutable = path
	t.Cleanup(func() { ghExecutable = restore })
}

// readStubChildPID waits for the stub to record its background child's pid; the
// stub writes it after the fork, so a plain read can lose the race.
func readStubChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("gh stub never recorded its child pid at %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
