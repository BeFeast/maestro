package supervisor

import (
	"testing"

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
