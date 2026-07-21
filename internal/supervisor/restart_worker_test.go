package supervisor

import (
	"os"
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
	// A real directory on disk stands in for the retained canonical worktree;
	// the guard must return before worker.Stop can remove it.
	wt := t.TempDir()
	sess := &state.Session{
		IssueNumber: 42,
		Status:      state.StatusDead,
		Worktree:    wt,
		NextRetryAt: nil,
	}
	wc := NewWorkerController(&config.Config{})

	err := wc.Restart("ok-player-297", sess)
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
