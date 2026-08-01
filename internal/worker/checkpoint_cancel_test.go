package worker

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// wedgedGitOnPath installs a fake git that forks a grandchild and then blocks
// forever. The grandchild's pid is published through a pid file so a test can
// prove cancellation killed the whole process group and not merely the leader.
func wedgedGitOnPath(t *testing.T) (pidFile string) {
	t.Helper()
	dir := t.TempDir()
	pidFile = filepath.Join(dir, "grandchild.pid")
	script := "#!/bin/sh\n" +
		"sh -c 'echo $$ > \"$MAESTRO_TEST_CHECKPOINT_CHILD_PID\"; exec sleep 300' &\n" +
		"sleep 300\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("MAESTRO_TEST_CHECKPOINT_CHILD_PID", pidFile)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return pidFile
}

func waitForPublishedPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fake git never published its grandchild pid to %s", pidFile)
	return 0
}

func waitProcessGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) != nil
}

// #1121: checkpoint capture shells out to git against worktrees that surviving
// workers still own, so wedging on index.lock is a normal occurrence. Cancelling
// the capture must terminate the git process group: maestro.service runs with
// KillMode=mixed, so a descendant that outlives the daemon stays in the unit
// cgroup and holds the unit in deactivating until TimeoutStopSec.
func TestSaveCheckpointContextKillsWedgedGitProcessGroup(t *testing.T) {
	pidFile := wedgedGitOnPath(t)
	worktree := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := SaveCheckpointContext(ctx, &state.Session{IssueNumber: 1121, Worktree: worktree})
		done <- err
	}()

	grandchild := waitForPublishedPID(t, pidFile)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled capture must report the cancellation instead of writing a truncated checkpoint")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("SaveCheckpointContext did not return after its context was cancelled")
	}
	if _, err := os.Stat(filepath.Join(worktree, "CHECKPOINT.md")); !os.IsNotExist(err) {
		t.Fatalf("cancelled capture must not write CHECKPOINT.md into a live worker worktree: %v", err)
	}
	if !waitProcessGone(grandchild, 10*time.Second) {
		t.Fatalf("git grandchild %d survived cancellation; only the process leader was killed", grandchild)
	}
}
