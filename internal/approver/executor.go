// Package approver turns an approved supervisor Approval into the real
// side-effecting action it represents (gh.MergePR, gh.CloseIssue,
// worker.RemoveWorktree). It is the execution stage of the cautious gate;
// state.go only records the transition, this package actually performs
// the work.
//
// Execution is intentionally synchronous and blocking — callers (CLI
// approve, supervisor poll) drive the loop, the executor never spawns
// goroutines. Each verb returns a clear error or nil, and the caller is
// responsible for transitioning state via MarkApprovalExecuted /
// MarkApprovalExecutionFailed / MarkApprovalExecutionSkipped.
package approver

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// GitHubClient is the surface the executor needs from a GitHub client.
// *github.Client satisfies this; tests inject fakes.
type GitHubClient interface {
	MergePR(prNumber int) error
	CloseIssue(number int, comment string) error
}

// WorktreeRemover removes a git worktree. *worker.RemoveWorktree-shaped
// callers satisfy this. Tests inject a fake.
type WorktreeRemover interface {
	RemoveWorktree(localPath, worktreePath string) error
}

// WorktreeRemoverFunc adapts a plain function to the WorktreeRemover
// interface (so cmd/maestro can pass worker.RemoveWorktree directly).
type WorktreeRemoverFunc func(localPath, worktreePath string) error

func (f WorktreeRemoverFunc) RemoveWorktree(localPath, worktreePath string) error {
	return f(localPath, worktreePath)
}

// SessionLookup is the narrow surface the executor needs from a state
// container to fence slot-reuse races (#488). state.State satisfies this
// trivially via SessionLookupFunc(st.SessionAt); tests inject fakes.
type SessionLookup interface {
	// LookupSession returns the live session at the given slot if any.
	// Returns (nil, false) when no session is bound.
	LookupSession(slot string) (*state.Session, bool)
}

// SessionLookupFunc adapts a plain function to the SessionLookup
// interface. Pass `approver.SessionLookupFunc(st.SessionAt)` from any
// caller that already has a *state.State in hand.
type SessionLookupFunc func(slot string) (*state.Session, bool)

// LookupSession satisfies the SessionLookup interface.
func (f SessionLookupFunc) LookupSession(slot string) (*state.Session, bool) {
	if f == nil {
		return nil, false
	}
	return f(slot)
}

// Executor carries the dependencies needed to run any of the four
// cautious-gate verbs.
type Executor struct {
	GH        GitHubClient
	Worktrees WorktreeRemover
	Cfg       *config.Config

	// Sessions, when non-nil, lets executeDeleteWorktree refuse to remove
	// a slot that has been recycled to a different live issue between
	// approval enqueue and execute (premortem failure mode #7). Optional;
	// nil disables the fence.
	Sessions SessionLookup

	// locks serializes Execute(approval) per approval.ID within the same
	// process. The second concurrent caller for the same ID returns
	// ApprovalStatusExecutionSkipped without firing any side effect.
	// Lazy-initialized.
	locks sync.Map // approval.ID -> *sync.Mutex
}

// Result describes the outcome of executing one approval.
//
//   - Status         — terminal state to transition the approval to:
//                      executed | execution_failed | execution_skipped.
//   - Summary        — short human-facing line for the audit entry and
//                      CLI output.
//   - Err            — non-nil only when Status == execution_failed; the
//                      caller marks failure and surfaces err to its
//                      operator (CLI exit code, supervisor log).
type Result struct {
	Status  state.ApprovalStatus
	Summary string
	Err     error
}

// ErrUnknownAction is returned when the approval's Action does not match
// any of the four cautious-gate verbs the executor knows.
var ErrUnknownAction = errors.New("unknown approval action")

// ErrMissingTarget is returned when the approval lacks the per-verb
// target field the executor needs (PR number, issue number, session).
var ErrMissingTarget = errors.New("approval target is missing required fields")

// Execute drives one approval to its real-world side effect. The caller
// is responsible for the state transition (Mark*) using the returned
// Result. Execute itself does NOT touch state — keeping state mutation
// at the caller boundary makes the unit a pure function of (approval,
// executor) and trivial to test.
//
// Execute does NOT validate the approval's status — callers must only
// pass approvals in status=approved (use state.ListApprovedApprovals).
//
// Execute is safe under same-process concurrency: a per-approval-ID
// advisory lock serializes calls. The second concurrent caller for the
// same approval.ID short-circuits to ApprovalStatusExecutionSkipped
// with a clear reason — no GH or fs side effect fires (#488).
func (e *Executor) Execute(approval *state.Approval) Result {
	if approval == nil {
		return Result{Status: state.ApprovalStatusExecutionFailed, Err: errors.New("nil approval")}
	}

	id := strings.TrimSpace(approval.ID)
	if id != "" {
		lockI, _ := e.locks.LoadOrStore(id, &sync.Mutex{})
		lock := lockI.(*sync.Mutex)
		if !lock.TryLock() {
			// Another goroutine in this process is already executing the
			// same approval. The bookkeeping idempotency check
			// (MarkApproval*) protects against double state writes; this
			// short-circuit additionally protects against a double SIDE
			// EFFECT (gh.MergePR twice, RemoveWorktree on a recycled
			// slot, etc.).
			return Result{
				Status:  state.ApprovalStatusExecutionSkipped,
				Summary: fmt.Sprintf("approval %s already executing in another goroutine", id),
			}
		}
		defer lock.Unlock()
		// Drop the lock from the map after release so long-lived processes
		// don't accumulate one mutex per ever-seen approval ID.
		defer e.locks.Delete(id)
	}

	// Repo guard (#489 / premortem #3): refuse any approval whose
	// stamped Repo does not match the executor's cfg.Repo. Defends
	// against a refactor that pools Executors across projects and
	// silently fires merge_pr against the wrong owner/repo. Empty
	// approval.Repo is back-compat — approvals created before #489 had
	// no stamp; we fall through to the existing behaviour to avoid
	// breaking already-pending approvals on upgrade.
	if strings.TrimSpace(approval.Repo) != "" && e.Cfg != nil {
		cfgRepo := strings.TrimSpace(e.Cfg.Repo)
		if cfgRepo != "" && approval.Repo != cfgRepo {
			return Result{
				Status: state.ApprovalStatusExecutionFailed,
				Summary: fmt.Sprintf(
					"approval %s is bound to repo %q but executor cfg.Repo is %q — refusing cross-project mutation",
					approval.ID, approval.Repo, cfgRepo,
				),
				Err: fmt.Errorf("approval %s repo mismatch: approval=%s cfg=%s", approval.ID, approval.Repo, cfgRepo),
			}
		}
	}

	switch approval.Action {
	case config.SupervisorActionMergePR:
		return e.executeMergePR(approval)
	case config.SupervisorActionCloseIssue:
		return e.executeCloseIssue(approval)
	case config.SupervisorActionDeleteWorktree:
		return e.executeDeleteWorktree(approval)
	case config.SupervisorActionChangeGlobalConfig:
		// Intentionally NOT implemented in this PR — the YAML mutation
		// pipeline (whitelist, atomic write, restart coordination) is
		// risky enough to warrant its own change. The approver records
		// the intent; an operator still has to edit the config and
		// restart the daemon. See follow-up issue.
		return Result{
			Status:  state.ApprovalStatusExecutionSkipped,
			Summary: "change_global_config requires a manual edit + systemctl restart (executor not implemented)",
		}
	}
	return Result{
		Status:  state.ApprovalStatusExecutionFailed,
		Summary: fmt.Sprintf("unknown approval action %q", approval.Action),
		Err:     fmt.Errorf("%w: %s", ErrUnknownAction, approval.Action),
	}
}

func (e *Executor) executeMergePR(approval *state.Approval) Result {
	if approval.Target == nil || approval.Target.PR <= 0 {
		return Result{Status: state.ApprovalStatusExecutionFailed, Err: fmt.Errorf("%w: PR number missing", ErrMissingTarget)}
	}
	if e.GH == nil {
		return Result{Status: state.ApprovalStatusExecutionFailed, Err: errors.New("no GitHub client wired into executor")}
	}
	pr := approval.Target.PR
	if err := e.GH.MergePR(pr); err != nil {
		return Result{
			Status:  state.ApprovalStatusExecutionFailed,
			Summary: fmt.Sprintf("merge PR #%d: %v", pr, err),
			Err:     fmt.Errorf("merge PR #%d: %w", pr, err),
		}
	}
	return Result{
		Status:  state.ApprovalStatusExecuted,
		Summary: fmt.Sprintf("merged PR #%d", pr),
	}
}

func (e *Executor) executeCloseIssue(approval *state.Approval) Result {
	if approval.Target == nil || approval.Target.Issue <= 0 {
		return Result{Status: state.ApprovalStatusExecutionFailed, Err: fmt.Errorf("%w: issue number missing", ErrMissingTarget)}
	}
	if e.GH == nil {
		return Result{Status: state.ApprovalStatusExecutionFailed, Err: errors.New("no GitHub client wired into executor")}
	}
	issue := approval.Target.Issue
	comment := strings.TrimSpace(approvalCloseIssueComment(approval))
	if err := e.GH.CloseIssue(issue, comment); err != nil {
		return Result{
			Status:  state.ApprovalStatusExecutionFailed,
			Summary: fmt.Sprintf("close issue #%d: %v", issue, err),
			Err:     fmt.Errorf("close issue #%d: %w", issue, err),
		}
	}
	return Result{
		Status:  state.ApprovalStatusExecuted,
		Summary: fmt.Sprintf("closed issue #%d", issue),
	}
}

func approvalCloseIssueComment(approval *state.Approval) string {
	// Prefer the operator's reason (it is the most recent audit entry's
	// Reason for the Approve event). Fall back to the approval Summary.
	for i := len(approval.Audit) - 1; i >= 0; i-- {
		if approval.Audit[i].Event == state.ApprovalAuditApproved {
			if r := strings.TrimSpace(approval.Audit[i].Reason); r != "" {
				return r
			}
			break
		}
	}
	return strings.TrimSpace(approval.Summary)
}

func (e *Executor) executeDeleteWorktree(approval *state.Approval) Result {
	if approval.Target == nil || strings.TrimSpace(approval.Target.Session) == "" {
		return Result{Status: state.ApprovalStatusExecutionFailed, Err: fmt.Errorf("%w: session/slot missing", ErrMissingTarget)}
	}
	if e.Worktrees == nil {
		return Result{Status: state.ApprovalStatusExecutionFailed, Err: errors.New("no worktree remover wired into executor")}
	}
	if e.Cfg == nil {
		return Result{Status: state.ApprovalStatusExecutionFailed, Err: errors.New("no project config wired into executor")}
	}
	slot := approval.Target.Session
	worktreePath, err := WorktreePathForSlot(e.Cfg, slot)
	if err != nil {
		return Result{Status: state.ApprovalStatusExecutionFailed, Err: err}
	}

	// Slot-reuse fence (#488 / premortem #7). The approval payload names
	// a slot, but slots are recycled — by the time we execute, this slot
	// may be running a different worker on a different issue. Refuse to
	// remove a worktree whose live session belongs to another issue.
	// When the approval lacks a target issue (rare; older approvals), or
	// the session lookup is not wired, fall through to the previous
	// behaviour to preserve compatibility.
	if e.Sessions != nil && approval.Target.Issue > 0 {
		if live, ok := e.Sessions.LookupSession(slot); ok && live != nil {
			if live.IssueNumber != approval.Target.Issue {
				return Result{
					Status: state.ApprovalStatusExecutionFailed,
					Summary: fmt.Sprintf(
						"slot %s now bound to issue #%d (approval expected #%d) — refusing to delete a recycled worktree",
						slot, live.IssueNumber, approval.Target.Issue,
					),
					Err: fmt.Errorf("slot %s reused: live=#%d approval=#%d", slot, live.IssueNumber, approval.Target.Issue),
				}
			}
		}
	}

	if err := e.Worktrees.RemoveWorktree(e.Cfg.LocalPath, worktreePath); err != nil {
		return Result{
			Status:  state.ApprovalStatusExecutionFailed,
			Summary: fmt.Sprintf("delete worktree %s: %v", worktreePath, err),
			Err:     fmt.Errorf("delete worktree %s: %w", worktreePath, err),
		}
	}
	return Result{
		Status:  state.ApprovalStatusExecuted,
		Summary: fmt.Sprintf("removed worktree for slot %s (%s)", slot, worktreePath),
	}
}

// WorktreePathForSlot returns the absolute worktree path for a session
// slot, anchored under cfg.WorktreeBase. Refuses paths that escape that
// base — defensive guard against a malformed slot ("../etc", "/tmp/x")
// being smuggled into delete_worktree.
func WorktreePathForSlot(cfg *config.Config, slot string) (string, error) {
	base := strings.TrimSpace(cfg.WorktreeBase)
	if base == "" {
		return "", errors.New("cfg.worktree_base is not set")
	}
	clean := strings.TrimSpace(slot)
	if err := state.ValidateSlotID(clean); err != nil {
		return "", err
	}
	// filepath.Join intentionally not imported here — concatenate via "/"
	// to keep the boundary explicit and predictable on the path.
	if strings.HasSuffix(base, "/") {
		return base + clean, nil
	}
	return base + "/" + clean, nil
}
