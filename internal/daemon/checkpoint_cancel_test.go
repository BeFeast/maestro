package daemon

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
	"github.com/befeast/maestro/internal/worker"
)

// #1121: the shutdown budget bounds the daemon's own goroutines, but what keeps
// maestro.service deactivating is the checkpoint git child, not the goroutine
// waiting on it. Timing out must cancel the capture — and join it — so the child
// is gone before shutdown continues.
func TestSaveCheckpointWithinCancelsAndJoinsTimedOutCapture(t *testing.T) {
	oldCheckpointFn := checkpointFn
	defer func() { checkpointFn = oldCheckpointFn }()

	released := make(chan struct{})
	checkpointFn = func(ctx context.Context, _ *state.Session) (string, error) {
		<-ctx.Done()
		close(released)
		return "", ctx.Err()
	}

	path, err, timedOut := saveCheckpointWithin(&state.Session{IssueNumber: 1121, Worktree: t.TempDir()}, 20*time.Millisecond)
	if !timedOut || path != "" || err != nil {
		t.Fatalf("want timed-out capture with no path and no error, got path=%q err=%v timedOut=%v", path, err, timedOut)
	}
	select {
	case <-released:
	default:
		t.Fatal("saveCheckpointWithin returned while the timed-out capture was still running; its git child would outlive the drain deadline")
	}
}

// A capture that ignores cancellation must still not hold shutdown open: the
// post-cancel join is bounded by checkpointCancelGrace.
func TestSaveCheckpointWithinBoundsThePostCancelJoin(t *testing.T) {
	oldCheckpointFn, oldCancelGrace := checkpointFn, checkpointCancelGrace
	checkpointCancelGrace = 20 * time.Millisecond
	blocked := make(chan struct{})
	finished := make(chan struct{})
	checkpointFn = func(context.Context, *state.Session) (string, error) {
		defer close(finished)
		<-blocked
		return "", nil
	}
	defer func() {
		close(blocked)
		<-finished
		checkpointFn = oldCheckpointFn
		checkpointCancelGrace = oldCancelGrace
	}()

	started := time.Now()
	if _, _, timedOut := saveCheckpointWithin(&state.Session{IssueNumber: 1121}, 20*time.Millisecond); !timedOut {
		t.Fatal("want timedOut=true for a capture that never returns")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("uncancellable capture held shutdown for %v", elapsed)
	}
}

// End-to-end over the real capture: a wedged git that has itself forked leaves
// nothing behind once saveCheckpointWithin reports timedOut=true.
func TestSaveCheckpointWithinLeavesNoGitChildAfterTimeout(t *testing.T) {
	oldCheckpointFn := checkpointFn
	checkpointFn = worker.SaveCheckpointContext
	defer func() { checkpointFn = oldCheckpointFn }()

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	script := "#!/bin/sh\n" +
		"sh -c 'echo $$ > \"$MAESTRO_TEST_CHECKPOINT_CHILD_PID\"; exec sleep 300' &\n" +
		"sleep 300\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("MAESTRO_TEST_CHECKPOINT_CHILD_PID", pidFile)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	sess := &state.Session{IssueNumber: 1121, Worktree: t.TempDir()}
	if _, _, timedOut := saveCheckpointWithin(sess, 200*time.Millisecond); !timedOut {
		t.Fatal("want timedOut=true for a wedged git capture")
	}

	grandchild := 0
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && grandchild == 0; {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				grandchild = pid
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if grandchild == 0 {
		t.Fatalf("fake git never published its grandchild pid to %s", pidFile)
	}

	gone := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if syscall.Kill(grandchild, 0) != nil {
			gone = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !gone {
		t.Fatalf("git child %d is still in the unit cgroup after saveCheckpointWithin reported timedOut=true", grandchild)
	}
}
