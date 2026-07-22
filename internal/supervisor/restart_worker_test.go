package supervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// #874: after a restart tears down the worktree, the session's stale
// worktree/PR pointers must be cleared so respawnDueRetries fresh-respawns
// instead of choosing RespawnInPlace against a directory that no longer exists.
func TestClearWorktreeAfterRestart(t *testing.T) {
	sess := &state.Session{
		IssueNumber: 42,
		Status:      state.StatusDead,
		Worktree:    "/wt/slot-1",
		PRNumber:    867,
	}
	clearWorktreeAfterRestart(sess)
	if sess.Worktree != "" {
		t.Fatalf("Worktree = %q, want cleared", sess.Worktree)
	}
	if sess.PRNumber != 0 {
		t.Fatalf("PRNumber = %d, want cleared", sess.PRNumber)
	}
	// Nil-safe.
	clearWorktreeAfterRestart(nil)
}

// #964: the restart controller is the destructive boundary — worker.Stop runs
// `git worktree remove --force`. Enforce "restart_worker must never delete a
// retained canonical worktree" here, independent of the approver executor gate:
// a session that still retains a worktree (PR-less durable work included) is
// refused before any process teardown or directory removal, and the worktree
// on disk is left intact for spawn_repair_worker to recover in place.
func TestNewWorkerController_RestartRefusesRetainedWorktree(t *testing.T) {
	// Use a real registered git worktree with uncommitted content. Without the
	// guard, worker.Stop's `git worktree remove --force` would delete it.
	repo := t.TempDir()
	runRestartWorkerGit(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatalf("write repository fixture: %v", err)
	}
	runRestartWorkerGit(t, repo, "add", "README.md")
	runRestartWorkerGit(t, repo, "-c", "user.name=Maestro Test", "-c", "user.email=maestro@example.invalid", "commit", "-qm", "fixture")
	wt := filepath.Join(t.TempDir(), "slot-1")
	runRestartWorkerGit(t, repo, "worktree", "add", "-q", "-b", "issue-42", wt)
	dirtyPath := filepath.Join(wt, "uncommitted.txt")
	wantDirty := "canonical retained work\n"
	if err := os.WriteFile(dirtyPath, []byte(wantDirty), 0o644); err != nil {
		t.Fatalf("write dirty worktree fixture: %v", err)
	}
	sess := &state.Session{
		IssueNumber: 42,
		Status:      state.StatusDead,
		Worktree:    wt,
		NextRetryAt: nil,
	}
	wc := NewWorkerController(&config.Config{LocalPath: repo})

	err := wc.Restart("test-retained-restart", sess)
	if err == nil {
		t.Fatalf("Restart returned nil, want refusal for retained worktree %q", wt)
	}
	if !strings.Contains(err.Error(), wt) || !strings.Contains(err.Error(), "spawn_repair_worker") {
		t.Fatalf("err = %v, want it to name the retained worktree and the in-place repair path", err)
	}
	// No destructive side effects: the directory survives and the session is
	// left untouched so recovery can resume in place.
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Fatalf("retained worktree %q was removed by a refused restart: %v", wt, statErr)
	}
	if got, readErr := os.ReadFile(dirtyPath); readErr != nil || string(got) != wantDirty {
		t.Fatalf("dirty retained work changed: got %q, err=%v; want %q", got, readErr, wantDirty)
	}
	if sess.Worktree != wt {
		t.Fatalf("sess.Worktree = %q, want it preserved as %q", sess.Worktree, wt)
	}
	if sess.Status != state.StatusDead {
		t.Fatalf("sess.Status = %q, want unchanged StatusDead", sess.Status)
	}
	if sess.NextRetryAt != nil {
		t.Fatalf("sess.NextRetryAt = %v, want unchanged (nil) — refused restart must not queue a respawn", sess.NextRetryAt)
	}
}

func runRestartWorkerGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", dir}, args...)
	if out, err := exec.Command("git", cmdArgs...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
