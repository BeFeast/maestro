package worker

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
)

// WorktreeCleanupLease captures the exact session generation selected for
// cleanup. It is revalidated against canonical state under the same per-slot
// lease used by respawn, immediately before any destructive filesystem work.
type WorktreeCleanupLease struct {
	Slot             string
	IssueNumber      int
	PRNumber         int
	PID              int
	TmuxSession      string
	Worktree         string
	Branch           string
	StartedAt        time.Time
	FinishedAt       *time.Time
	Status           state.SessionStatus
	WorkerGeneration uint64
}

// CleanupProbes supplies deterministic runtime checks. Production callers may
// leave either function nil to use the worker/tmux defaults.
type CleanupProbes struct {
	PIDAlive  func(int) bool
	TmuxAlive func(string) bool
}

// CleanupPolicy selects whether this cleanup is an automatic terminal-session
// reclamation (which must also prove the worktree clean) or an explicitly
// approved/manual deletion.
type CleanupPolicy struct {
	RequireTerminal bool
	RequireClean    bool
}

// CleanupHooks lets callers retain their normal before-remove hook and inject
// deterministic removal/restoration behavior in tests.
type CleanupHooks struct {
	BeforeRemove func() error
	Remove       func(localPath, worktreePath string) error
	Restore      func(localPath, worktreeBase, slotName, worktree, branch string) error
}

// ErrCleanupLeaseChanged means cleanup lost ownership before mutation. The
// caller must leave the worktree untouched.
var ErrCleanupLeaseChanged = errors.New("worktree cleanup lease changed")

// ErrCleanupConsistencyViolation marks a failure after the durable cleanup
// transition. Cleanup compensates by preserving/restoring the exact worktree
// and reattaching its state claim before returning this P0-class error.
var ErrCleanupConsistencyViolation = errors.New("P0 worktree cleanup consistency violation")

// CaptureCleanupLease snapshots the exact session/process/worktree generation
// a cleanup cycle selected.
func CaptureCleanupLease(slot string, sess *state.Session) WorktreeCleanupLease {
	lease := WorktreeCleanupLease{Slot: slot}
	if sess == nil {
		return lease
	}
	lease.IssueNumber = sess.IssueNumber
	lease.PRNumber = sess.PRNumber
	lease.PID = sess.PID
	lease.TmuxSession = sess.TmuxSession
	lease.Worktree = sess.Worktree
	lease.Branch = sess.Branch
	lease.StartedAt = sess.StartedAt
	lease.Status = sess.Status
	lease.WorkerGeneration = sess.WorkerGeneration
	if sess.FinishedAt != nil {
		finished := *sess.FinishedAt
		lease.FinishedAt = &finished
	}
	return lease
}

// ValidateCleanupLease verifies every ownership signal required by #963. It
// does not mutate state or files.
func ValidateCleanupLease(lease WorktreeCleanupLease, current *state.Session, probes CleanupProbes, policy CleanupPolicy) error {
	pidAlive := probes.PIDAlive
	if pidAlive == nil {
		pidAlive = IsAlive
	}
	tmuxAlive := probes.TmuxAlive
	if tmuxAlive == nil {
		tmuxAlive = func(name string) bool {
			if strings.TrimSpace(name) == "" {
				return false
			}
			return tmuxsession.HasSession(name)
		}
	}

	if current == nil {
		return fmt.Errorf("%w: slot %s no longer holds a session", ErrCleanupLeaseChanged, lease.Slot)
	}
	if current.IssueNumber != lease.IssueNumber {
		return fmt.Errorf("%w: slot %s issue changed #%d->#%d", ErrCleanupLeaseChanged, lease.Slot, lease.IssueNumber, current.IssueNumber)
	}
	if current.PRNumber != lease.PRNumber {
		return fmt.Errorf("%w: slot %s PR changed #%d->#%d", ErrCleanupLeaseChanged, lease.Slot, lease.PRNumber, current.PRNumber)
	}
	if current.WorkerGeneration != lease.WorkerGeneration {
		return fmt.Errorf("%w: slot %s worker generation changed %d->%d", ErrCleanupLeaseChanged, lease.Slot, lease.WorkerGeneration, current.WorkerGeneration)
	}
	if current.PID != lease.PID {
		return fmt.Errorf("%w: slot %s PID changed %d->%d", ErrCleanupLeaseChanged, lease.Slot, lease.PID, current.PID)
	}
	if strings.TrimSpace(current.TmuxSession) != strings.TrimSpace(lease.TmuxSession) {
		return fmt.Errorf("%w: slot %s tmux identity changed %q->%q", ErrCleanupLeaseChanged, lease.Slot, lease.TmuxSession, current.TmuxSession)
	}
	if filepath.Clean(current.Worktree) != filepath.Clean(lease.Worktree) {
		return fmt.Errorf("%w: slot %s worktree claim changed %q->%q", ErrCleanupLeaseChanged, lease.Slot, lease.Worktree, current.Worktree)
	}
	if strings.TrimSpace(current.Branch) != strings.TrimSpace(lease.Branch) {
		return fmt.Errorf("%w: slot %s branch changed %q->%q", ErrCleanupLeaseChanged, lease.Slot, lease.Branch, current.Branch)
	}
	if !current.StartedAt.Equal(lease.StartedAt) {
		return fmt.Errorf("%w: slot %s attempt start changed", ErrCleanupLeaseChanged, lease.Slot)
	}
	if !sameOptionalTime(current.FinishedAt, lease.FinishedAt) {
		return fmt.Errorf("%w: slot %s terminal timestamp changed", ErrCleanupLeaseChanged, lease.Slot)
	}
	if current.Status != lease.Status {
		return fmt.Errorf("%w: slot %s status changed %s->%s", ErrCleanupLeaseChanged, lease.Slot, lease.Status, current.Status)
	}
	if policy.RequireTerminal && !state.IsTerminal(current.Status) {
		return fmt.Errorf("%w: slot %s is %s, not terminal", ErrCleanupLeaseChanged, lease.Slot, current.Status)
	}
	if current.PID > 0 && pidAlive(current.PID) {
		return fmt.Errorf("%w: slot %s worker PID %d is alive", ErrCleanupLeaseChanged, lease.Slot, current.PID)
	}
	tmuxName := strings.TrimSpace(current.TmuxSession)
	if tmuxName == "" {
		tmuxName = TmuxSessionName(lease.Slot)
	}
	if tmuxAlive(tmuxName) {
		return fmt.Errorf("%w: slot %s tmux session %q is alive", ErrCleanupLeaseChanged, lease.Slot, tmuxName)
	}
	return nil
}

// CleanupLeasedWorktree performs a lossless two-phase cleanup:
//
//  1. under the cross-process slot lease, re-read and validate canonical
//     session generation, PID/tmux liveness, worktree/PR identity, and active
//     approved repair ownership;
//  2. persist worktree="" through the normal state CAS before deletion;
//  3. re-read once more immediately before mutation;
//  4. if removal fails or partially invalidates Git metadata, restore/preserve
//     the exact canonical worktree and reattach its state claim.
//
// This removes the post-side-effect CAS window that caused #963.
func CleanupLeasedWorktree(cfg *config.Config, s *state.State, lease WorktreeCleanupLease, probes CleanupProbes, policy CleanupPolicy, hooks CleanupHooks) error {
	if cfg == nil {
		return fmt.Errorf("cleanup leased worktree: nil config")
	}
	if s == nil {
		return fmt.Errorf("cleanup leased worktree: nil state")
	}
	if strings.TrimSpace(lease.Worktree) == "" {
		return nil
	}
	remove := hooks.Remove
	if remove == nil {
		remove = RemoveWorktree
	}
	restore := hooks.Restore
	if restore == nil {
		restore = RestoreMissingWorktree
	}

	return state.WithSessionLease(cfg.StateDir, lease.Slot, func() error {
		canonical := s
		if strings.TrimSpace(cfg.StateDir) != "" {
			loaded, err := state.Load(cfg.StateDir)
			if err != nil {
				return fmt.Errorf("%w: re-read canonical state: %v", ErrCleanupLeaseChanged, err)
			}
			canonical = loaded
		}
		current := canonical.Sessions[lease.Slot]
		if err := ValidateCleanupLease(lease, current, probes, policy); err != nil {
			return err
		}
		if approval, ok := canonical.ApprovedRepairOwnsSession(lease.Slot, lease.IssueNumber, lease.PRNumber); ok {
			return fmt.Errorf("%w: approved repair %s owns slot %s / PR #%d", ErrCleanupLeaseChanged, approval.ID, lease.Slot, lease.PRNumber)
		}
		if policy.RequireClean {
			dirty, err := WorktreeDirty(lease.Worktree)
			if err != nil {
				return fmt.Errorf("%w: could not prove worktree clean: %v", ErrCleanupLeaseChanged, err)
			}
			if dirty {
				return fmt.Errorf("%w: worktree contains uncommitted work", ErrCleanupLeaseChanged)
			}
		}

		local := s.Sessions[lease.Slot]
		if err := ValidateCleanupLease(lease, local, probes, policy); err != nil {
			return err
		}
		local.Worktree = ""
		if strings.TrimSpace(cfg.StateDir) != "" {
			if err := state.Save(cfg.StateDir, s); err != nil {
				local.Worktree = lease.Worktree
				return fmt.Errorf("%w: commit cleanup claim before filesystem mutation: %v", ErrCleanupLeaseChanged, err)
			}
		}

		// Re-read after the committed transition. Project the cleared worktree
		// back to the captured path solely for identity validation; every other
		// canonical field must still match the selected generation.
		if strings.TrimSpace(cfg.StateDir) != "" {
			latest, err := state.Load(cfg.StateDir)
			if err != nil {
				_ = restoreCleanupStateClaim(cfg.StateDir, s, lease)
				return fmt.Errorf("%w: final canonical re-read before removal: %v", ErrCleanupLeaseChanged, err)
			}
			current = latest.Sessions[lease.Slot]
			if current == nil || strings.TrimSpace(current.Worktree) != "" {
				_ = restoreCleanupStateClaim(cfg.StateDir, s, lease)
				return fmt.Errorf("%w: committed cleanup claim was replaced", ErrCleanupLeaseChanged)
			}
			projected := *current
			projected.Worktree = lease.Worktree
			if err := ValidateCleanupLease(lease, &projected, probes, policy); err != nil {
				_ = restoreCleanupStateClaim(cfg.StateDir, s, lease)
				return err
			}
			if approval, ok := latest.ApprovedRepairOwnsSession(lease.Slot, lease.IssueNumber, lease.PRNumber); ok {
				_ = restoreCleanupStateClaim(cfg.StateDir, s, lease)
				return fmt.Errorf("%w: approved repair %s acquired slot %s before removal", ErrCleanupLeaseChanged, approval.ID, lease.Slot)
			}
		}

		if hooks.BeforeRemove != nil {
			if err := hooks.BeforeRemove(); err != nil {
				_ = restoreCleanupStateClaim(cfg.StateDir, s, lease)
				return fmt.Errorf("before-remove hook: %w", err)
			}
		}
		if policy.RequireClean {
			dirty, err := WorktreeDirty(lease.Worktree)
			if err != nil || dirty {
				_ = restoreCleanupStateClaim(cfg.StateDir, s, lease)
				if err != nil {
					return fmt.Errorf("%w: final worktree cleanliness check: %v", ErrCleanupLeaseChanged, err)
				}
				return fmt.Errorf("%w: worktree became dirty before removal", ErrCleanupLeaseChanged)
			}
		}

		if err := remove(cfg.LocalPath, lease.Worktree); err != nil {
			restoreErr := preserveOrRestoreWorktree(cfg, lease, restore)
			stateErr := restoreCleanupStateClaim(cfg.StateDir, s, lease)
			return fmt.Errorf("%w: remove %s: %v (restore=%v, state=%v)", ErrCleanupConsistencyViolation, lease.Worktree, err, restoreErr, stateErr)
		}
		return nil
	})
}

func restoreCleanupStateClaim(stateDir string, s *state.State, lease WorktreeCleanupLease) error {
	if strings.TrimSpace(stateDir) == "" {
		if current := s.Sessions[lease.Slot]; current != nil {
			current.Worktree = lease.Worktree
		}
		return nil
	}
	err := state.Update(stateDir, func(latest *state.State) error {
		current := latest.Sessions[lease.Slot]
		if current == nil {
			return fmt.Errorf("%w: slot vanished during cleanup compensation", ErrCleanupLeaseChanged)
		}
		if current.IssueNumber != lease.IssueNumber || current.PRNumber != lease.PRNumber || current.WorkerGeneration != lease.WorkerGeneration ||
			strings.TrimSpace(current.Branch) != strings.TrimSpace(lease.Branch) || !current.StartedAt.Equal(lease.StartedAt) {
			return fmt.Errorf("%w: slot changed during cleanup compensation", ErrCleanupLeaseChanged)
		}
		if strings.TrimSpace(current.Worktree) == "" {
			current.Worktree = lease.Worktree
		}
		return nil
	})
	latest, loadErr := state.Load(stateDir)
	if loadErr == nil {
		*s = *latest
	}
	if err != nil {
		return err
	}
	return loadErr
}

func preserveOrRestoreWorktree(cfg *config.Config, lease WorktreeCleanupLease, restore func(localPath, worktreeBase, slotName, worktree, branch string) error) error {
	if worktreeUsable(lease.Worktree) {
		return nil
	}
	return restore(cfg.LocalPath, cfg.WorktreeBase, lease.Slot, lease.Worktree, lease.Branch)
}

func worktreeUsable(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	out, err := exec.Command("git", "-C", path, "rev-parse", "--is-inside-work-tree").CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func sameOptionalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
