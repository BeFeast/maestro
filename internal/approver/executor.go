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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// GitHubClient is the surface the executor needs from a GitHub client.
// *github.Client satisfies this; tests inject fakes.
type GitHubClient interface {
	MergePR(prNumber int) error
	CloseIssue(number int, comment string) error

	// PRMergeStatus returns the normalized mergeable verdict
	// ("MERGEABLE"/"CONFLICTING"/"UNKNOWN") and the raw GitHub
	// mergeable_state ("clean", "behind", "dirty", ...). executeMergePR
	// uses it to tell a recoverable BEHIND PR apart from a real
	// conflict before deciding whether to merge, update-branch, or fail
	// (#547).
	PRMergeStatus(prNumber int) (mergeable string, mergeStateStatus string, err error)

	// UpdateBranch brings a BEHIND PR head up to date with its base
	// branch (no --admin bypass). Used by executeMergePR (#547).
	UpdateBranch(prNumber int) error
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

// WorkerController is the narrow surface the executor needs to drive
// the per-session worker-control verbs (#567: restart_worker /
// stop_worker). Implementations kill the tmux session + reap the worker
// process tree, and — for restart — leave the slot in a state the
// orchestrator's dispatcher loop will respawn on the next cycle.
//
// Both methods MUST mutate sess in place: stop_worker transitions the
// session to a terminal status; restart_worker leaves it in StatusDead
// with NextRetryAt = now so respawnDueRetries picks it up. The caller of
// ex.Execute() is responsible for persisting state via state.Save.
type WorkerController interface {
	StopWorker(slot string, sess *state.Session) error
	RestartWorker(slot string, sess *state.Session) error
}

// WorkerControllerFuncs adapts a pair of plain functions to the
// WorkerController interface so cmd/maestro and supervisor can plug in
// worker.Stop + small state mutations via closures without defining a
// concrete type. Either field may be nil — calling the corresponding
// method returns a clear error instead of panicking.
type WorkerControllerFuncs struct {
	Stop    func(slot string, sess *state.Session) error
	Restart func(slot string, sess *state.Session) error
}

// StopWorker satisfies the WorkerController interface.
func (w WorkerControllerFuncs) StopWorker(slot string, sess *state.Session) error {
	if w.Stop == nil {
		return errors.New("no stop function configured for WorkerController")
	}
	return w.Stop(slot, sess)
}

// RestartWorker satisfies the WorkerController interface.
func (w WorkerControllerFuncs) RestartWorker(slot string, sess *state.Session) error {
	if w.Restart == nil {
		return errors.New("no restart function configured for WorkerController")
	}
	return w.Restart(slot, sess)
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

	// Workers, when non-nil, drives the per-session worker-control verbs
	// (#567: restart_worker / stop_worker). nil disables both verbs —
	// the executor returns execution_failed with a clear summary.
	Workers WorkerController

	// State is optional for most actions. apply_lesson_proposal uses it to
	// fetch the proposed rule and mark it applied after the gated append.
	State *state.State

	// locks serializes Execute(approval) per approval.ID within the same
	// process. The second concurrent caller for the same ID returns
	// ApprovalStatusExecutionSkipped without firing any side effect.
	// Lazy-initialized.
	locks sync.Map // approval.ID -> *sync.Mutex
}

// Result describes the outcome of executing one approval.
//
//   - Status         — terminal state to transition the approval to:
//     executed | execution_failed | execution_skipped.
//   - Summary        — short human-facing line for the audit entry and
//     CLI output.
//   - Err            — non-nil only when Status == execution_failed; the
//     caller marks failure and surfaces err to its
//     operator (CLI exit code, supervisor log).
//   - Warning        — non-fatal advisory the caller should log out-of-band
//     (#489: unstamped legacy approval fell through the repo
//     guard). Empty in the happy path.
type Result struct {
	Status  state.ApprovalStatus
	Summary string
	Err     error
	Warning string
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
		// Defers are LIFO: register Delete first so it runs LAST (after
		// Unlock). The inverse ordering — Delete runs first, then Unlock —
		// would leave a window in which a second goroutine for the same ID
		// calls LoadOrStore and receives a brand-new unlocked mutex,
		// succeeds on TryLock against that fresh entry, and proceeds to
		// fire the side effect concurrently with this still-locked one.
		// Unlock-then-Delete keeps map and mutex state aligned: while the
		// mutex is held, the map entry is also present, so every concurrent
		// caller observes the same locked mutex and short-circuits.
		defer e.locks.Delete(id)
		defer lock.Unlock()
	}

	// Repo guard (#489 / premortem #3): refuse any approval whose
	// stamped Repo does not match the executor's cfg.Repo. Defends
	// against a refactor that pools Executors across projects and
	// silently fires merge_pr against the wrong owner/repo. Empty
	// approval.Repo is back-compat — approvals created before #489 had
	// no stamp; we fall through to the existing behaviour (with a
	// deprecation warning) so already-pending approvals don't abort on
	// upgrade. state.MigrateApprovalsBindRepo back-fills the stamp on
	// each supervisor cycle, so the unstamped window is narrow.
	var legacyWarning string
	stampedRepo := strings.TrimSpace(approval.Repo)
	if stampedRepo != "" && e.Cfg != nil {
		cfgRepo := strings.TrimSpace(e.Cfg.Repo)
		if cfgRepo != "" && stampedRepo != cfgRepo {
			return Result{
				Status: state.ApprovalStatusExecutionFailed,
				Summary: fmt.Sprintf(
					"approval %s is bound to repo %q but executor cfg.Repo is %q — refusing cross-project mutation",
					approval.ID, stampedRepo, cfgRepo,
				),
				Err: fmt.Errorf("approval %s repo mismatch: approval=%s cfg=%s", approval.ID, stampedRepo, cfgRepo),
			}
		}
	} else if stampedRepo == "" && e.Cfg != nil && strings.TrimSpace(e.Cfg.Repo) != "" {
		legacyWarning = fmt.Sprintf(
			"approval %s has no Repo stamp (pre-#489); executing against ambient cfg.Repo=%q — back-fill via state.MigrateApprovalsBindRepo",
			approval.ID, strings.TrimSpace(e.Cfg.Repo),
		)
	}

	res := e.dispatchAction(approval)
	if legacyWarning != "" && res.Warning == "" {
		res.Warning = legacyWarning
	}
	return res
}

// dispatchAction is the post-guard verb switch, kept separate so the
// repo-guard / legacy-warning prelude in Execute can decorate the
// returned Result uniformly.
func (e *Executor) dispatchAction(approval *state.Approval) Result {
	switch approval.Action {
	case config.SupervisorActionMergePR:
		return e.executeMergePR(approval)
	case config.SupervisorActionCloseIssue:
		return e.executeCloseIssue(approval)
	case config.SupervisorActionCloseIssueBatch:
		return e.executeCloseIssueBatch(approval)
	case config.SupervisorActionDeleteWorktree:
		return e.executeDeleteWorktree(approval)
	case config.SupervisorActionStopWorker:
		return e.executeStopWorker(approval)
	case config.SupervisorActionRestartWorker:
		return e.executeRestartWorker(approval)
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
	case config.SupervisorActionApplyLessonProposal:
		return e.executeApplyLessonProposal(approval)
	case "spawn_worker":
		// The approver runs in the supervisor cycle; it does not own the
		// dispatch loop that actually spawns workers. Mark the approval
		// as execution_skipped with a clear, actionable summary so the
		// CLI / dashboard stops printing "No risky action was executed"
		// and the operator knows the next supervisor/dispatch loop will
		// pick the issue up automatically. ReconcileSpawnWorkerApprovals*
		// then supersedes this approval once the matching worker starts.
		issue := 0
		if approval.Target != nil {
			issue = approval.Target.Issue
		}
		summary := "spawn_worker approval recorded; next dispatcher loop will start the worker"
		if issue > 0 {
			summary = fmt.Sprintf("spawn_worker for issue #%d approved; next dispatcher loop will start the worker (no extra command required)", issue)
		}
		// #515: AwaitingDispatch (not ExecutionSkipped). The dispatcher
		// loop owns the actual worker.Start; until it runs we keep this
		// approval as "still effective" so the at-mint dedup in
		// RecordPendingApprovalForDecision coalesces fresh supervisor
		// recommendations on the same (action, target) instead of
		// minting a duplicate pending in the race window.
		return Result{
			Status:  state.ApprovalStatusAwaitingDispatch,
			Summary: summary,
		}
	case "spawn_review_repair":
		// #565: same dispatcher shape as spawn_worker. The supervisor
		// records the intent; the orchestrator's dispatcher loop owns
		// the actual worker spawn and gates idempotency on
		// (pr_number, head_sha). Returning AwaitingDispatch keeps the
		// approval effective so RecordPendingApprovalForDecision's
		// at-mint dedup coalesces fresh recommendations for the same
		// (action, target) until the dispatcher resolves it.
		pr, issue := 0, 0
		if approval.Target != nil {
			pr = approval.Target.PR
			issue = approval.Target.Issue
		}
		summary := "spawn_review_repair approval recorded; next dispatcher loop will start the scoped repair worker"
		if pr > 0 {
			summary = fmt.Sprintf("spawn_review_repair for PR #%d (issue #%d) approved; next dispatcher loop will start the scoped repair worker (no extra command required)", pr, issue)
		}
		return Result{
			Status:  state.ApprovalStatusAwaitingDispatch,
			Summary: summary,
		}
	case "spawn_repair_worker":
		// #662: classic repair worker. Same shape as spawn_worker —
		// the orchestrator's dispatcher loop (supervisorSelectedRepairSpawn)
		// owns the actual respawn for the open-PR-not-progressing and
		// retry-exhausted-without-PR cases. Returning AwaitingDispatch
		// keeps the approval effective so RecordPendingApprovalForDecision's
		// at-mint dedup coalesces fresh recommendations until the
		// dispatcher resolves it; before #662 this verb fell through
		// to the unknown-action default and cautious projects refused
		// to mint the approval every cycle (silent stall).
		pr, issue := 0, 0
		if approval.Target != nil {
			pr = approval.Target.PR
			issue = approval.Target.Issue
		}
		summary := "spawn_repair_worker approval recorded; next dispatcher loop will start the repair worker"
		switch {
		case pr > 0 && issue > 0:
			summary = fmt.Sprintf("spawn_repair_worker for issue #%d (PR #%d) approved; next dispatcher loop will start the repair worker (no extra command required)", issue, pr)
		case issue > 0:
			summary = fmt.Sprintf("spawn_repair_worker for issue #%d approved; next dispatcher loop will start the repair worker (no extra command required)", issue)
		}
		return Result{
			Status:  state.ApprovalStatusAwaitingDispatch,
			Summary: summary,
		}
	case "open_child_issue":
		// Same shape as spawn_worker: the v1 supervisor records the
		// intent, but the safe-action executor for create_issue lands
		// in a follow-up PR. Skipped, not failed, so the operator sees
		// an actionable summary instead of a noisy execution_failed.
		issue := 0
		if approval.Target != nil {
			issue = approval.Target.Issue
		}
		summary := "open_child_issue approval recorded; safe-action executor not yet wired (operator must create the next child issue manually for now)"
		if issue > 0 {
			summary = fmt.Sprintf("open_child_issue for handoff epic #%d approved; operator must create the next child issue manually until the safe-action executor lands", issue)
		}
		// #515: AwaitingDispatch — the side effect is "operator must
		// create the next child issue manually". Until it lands as a
		// real GitHub issue, we keep this record effective so dedup
		// suppresses fresh duplicates on the same handoff target.
		return Result{
			Status:  state.ApprovalStatusAwaitingDispatch,
			Summary: summary,
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

	// #547: a green PR that has fallen BEHIND main cannot merge under
	// "branches must be up to date" protection — `gh pr merge` fails with
	// "the head branch is not up to date". That is a recoverable state,
	// not a genuine failure: bring the branch up to date (no --admin
	// bypass) and let the next supervisor cycle re-validate and re-mint
	// against the new head. Reserve execution_failed for PRs that are
	// truly unmergeable (real conflicts).
	//
	// We inspect the fresh single-PR mergeable_state first. Only two
	// states change behaviour here:
	//   - "behind" + mergeable  -> UpdateBranch, return non-failure
	//   - genuine conflict       -> execution_failed without touching GH
	// Everything else (clean / blocked / unstable / unknown, or a status
	// fetch error) falls through to the original MergePR path, so the
	// happy path and `gh pr merge`'s own gating are preserved unchanged.
	mergeable, mergeState, statusErr := e.GH.PRMergeStatus(pr)
	if statusErr == nil {
		switch {
		case mergeState == "behind" && mergeable != "CONFLICTING":
			if err := e.GH.UpdateBranch(pr); err != nil {
				return Result{
					Status:  state.ApprovalStatusExecutionFailed,
					Summary: fmt.Sprintf("update branch for PR #%d (was behind main): %v", pr, err),
					Err:     fmt.Errorf("update branch for PR #%d: %w", pr, err),
				}
			}
			// Non-failure terminal status: the branch was updated, the
			// head SHA moved, and this approval's target_state_hash is now
			// stale. We deliberately do NOT poll CI or merge here. The next
			// supervisor cycle re-evaluates against the new head and mints
			// a fresh merge_pr (the at-mint dedup in
			// RecordPendingApprovalForDecision supersedes this stale one).
			return Result{
				Status:  state.ApprovalStatusExecutionSkipped,
				Summary: fmt.Sprintf("PR #%d was behind main; branch updated, will re-validate and merge on the next supervisor cycle", pr),
			}
		case mergeable == "CONFLICTING":
			return Result{
				Status:  state.ApprovalStatusExecutionFailed,
				Summary: fmt.Sprintf("PR #%d is not mergeable (conflicts with base) — resolve conflicts before merging", pr),
				Err:     fmt.Errorf("PR #%d unmergeable: mergeable_state=%q", pr, mergeState),
			}
		}
	}

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

func (e *Executor) executeCloseIssueBatch(approval *state.Approval) Result {
	if approval.Target == nil || len(approval.Target.Issues) == 0 {
		return Result{Status: state.ApprovalStatusExecutionFailed, Err: fmt.Errorf("%w: issues missing", ErrMissingTarget)}
	}
	if e.GH == nil {
		return Result{Status: state.ApprovalStatusExecutionFailed, Err: errors.New("no GitHub client wired into executor")}
	}
	comment := strings.TrimSpace(approvalCloseIssueComment(approval))
	if comment == "" {
		comment = "Maestro verified the configured runtime outcome after code landed; closing this issue as done."
	}

	closed := make([]int, 0, len(approval.Target.Issues))
	for _, target := range approval.Target.Issues {
		if target.Issue <= 0 {
			continue
		}
		issueComment := comment
		if target.PR > 0 && issueComment == "Maestro verified the configured runtime outcome after code landed; closing this issue as done." {
			issueComment = fmt.Sprintf("Maestro verified the configured runtime outcome after PR #%d landed; closing this issue as done.", target.PR)
		}
		if err := e.GH.CloseIssue(target.Issue, issueComment); err != nil {
			return Result{
				Status:  state.ApprovalStatusExecutionFailed,
				Summary: fmt.Sprintf("close issue batch after %d/%d issue(s): issue #%d: %v", len(closed), len(approval.Target.Issues), target.Issue, err),
				Err:     fmt.Errorf("close issue batch: issue #%d: %w", target.Issue, err),
			}
		}
		closed = append(closed, target.Issue)
	}
	if len(closed) == 0 {
		return Result{Status: state.ApprovalStatusExecutionFailed, Err: fmt.Errorf("%w: no valid issue numbers in batch", ErrMissingTarget)}
	}
	return Result{
		Status:  state.ApprovalStatusExecuted,
		Summary: fmt.Sprintf("closed %d verified issue(s): %s", len(closed), formatIssueList(closed)),
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

func formatIssueList(issues []int) string {
	if len(issues) == 0 {
		return ""
	}
	parts := make([]string, 0, len(issues))
	for _, issue := range issues {
		parts = append(parts, fmt.Sprintf("#%d", issue))
	}
	return strings.Join(parts, ", ")
}

func (e *Executor) executeApplyLessonProposal(approval *state.Approval) Result {
	if e.State == nil {
		return Result{Status: state.ApprovalStatusExecutionFailed, Err: errors.New("no state wired into executor")}
	}
	if e.Cfg == nil {
		return Result{Status: state.ApprovalStatusExecutionFailed, Err: errors.New("no project config wired into executor")}
	}
	id := strings.TrimSpace(approval.LessonProposalID)
	if id == "" {
		return Result{Status: state.ApprovalStatusExecutionFailed, Err: fmt.Errorf("%w: lesson proposal id missing", ErrMissingTarget)}
	}
	proposal, ok := e.State.FindLessonProposal(id)
	if !ok {
		return Result{Status: state.ApprovalStatusExecutionFailed, Err: fmt.Errorf("lesson proposal %s not found", id)}
	}
	if proposal.Status == state.LessonProposalStatusApplied {
		return Result{
			Status:  state.ApprovalStatusExecutionSkipped,
			Summary: fmt.Sprintf("lesson proposal %s was already applied", proposal.ID),
		}
	}
	path, targetLabel, err := e.lessonProposalTargetPath(proposal)
	if err != nil {
		return Result{Status: state.ApprovalStatusExecutionFailed, Summary: err.Error(), Err: err}
	}
	block := formatLessonProposalBlock(*proposal)
	if err := appendLessonBlock(path, block); err != nil {
		return Result{
			Status:  state.ApprovalStatusExecutionFailed,
			Summary: fmt.Sprintf("apply lesson proposal %s to %s: %v", proposal.ID, targetLabel, err),
			Err:     err,
		}
	}
	e.State.MarkLessonProposalApplied(proposal.ID, time.Now().UTC(), approvalApprovedActor(approval), fmt.Sprintf("appended to %s", targetLabel))
	return Result{
		Status:  state.ApprovalStatusExecuted,
		Summary: fmt.Sprintf("applied lesson proposal %s to %s", proposal.ID, targetLabel),
	}
}

func (e *Executor) lessonProposalTargetPath(proposal *state.LessonProposal) (string, string, error) {
	switch strings.TrimSpace(proposal.Target) {
	case state.LessonProposalTargetWorkerPrompt:
		path := strings.TrimSpace(e.Cfg.WorkerPrompt)
		if path == "" {
			return "", "", errors.New("worker_prompt is not configured; cannot apply worker prompt lesson")
		}
		return path, "worker prompt", nil
	case state.LessonProposalTargetAgentsMD, "":
		root := strings.TrimSpace(e.Cfg.LocalPath)
		if root == "" {
			return "", "", errors.New("local_path is not configured; cannot apply AGENTS.md lesson")
		}
		return filepath.Join(root, "AGENTS.md"), "AGENTS.md", nil
	default:
		return "", "", fmt.Errorf("unknown lesson proposal target %q", proposal.Target)
	}
}

func formatLessonProposalBlock(proposal state.LessonProposal) string {
	rule := strings.TrimSpace(proposal.SuggestedRule)
	repro := strings.TrimSpace(proposal.MinimalRepro)
	var b strings.Builder
	b.WriteString("\n\n<!-- maestro:lesson-proposal ")
	b.WriteString(proposal.ID)
	b.WriteString(" -->\n")
	b.WriteString("## Maestro Lesson: ")
	b.WriteString(proposal.FailureClass)
	b.WriteString("\n\n")
	if repro != "" {
		b.WriteString("Context: ")
		b.WriteString(repro)
		b.WriteString("\n\n")
	}
	b.WriteString(rule)
	b.WriteString("\n")
	b.WriteString("<!-- /maestro:lesson-proposal -->\n")
	return b.String()
}

func appendLessonBlock(path, block string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("empty lesson target path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create lesson target dir: %w", err)
	}
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read lesson target: %w", err)
	}
	if strings.Contains(string(existing), strings.TrimSpace(block)) {
		return nil
	}
	return os.WriteFile(path, append(existing, []byte(block)...), 0644)
}

func approvalApprovedActor(approval *state.Approval) string {
	if approval == nil {
		return ""
	}
	for i := len(approval.Audit) - 1; i >= 0; i-- {
		if approval.Audit[i].Event == state.ApprovalAuditApproved {
			return approval.Audit[i].Actor
		}
	}
	return ""
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

// executeStopWorker terminates the worker session at approval.Target.Session
// via the WorkerController. The controller is expected to kill the tmux
// pane + reap the worker process tree AND mutate sess.Status to a
// terminal state (typically StatusFailed with FinishedAt set). #567.
func (e *Executor) executeStopWorker(approval *state.Approval) Result {
	return e.executeWorkerControl(approval, "stop_worker", true)
}

// executeRestartWorker stops the worker at approval.Target.Session and
// leaves the slot in a state the orchestrator's respawnDueRetries loop
// will pick up on the next cycle. The controller is expected to mutate
// sess.Status to StatusDead with NextRetryAt = now. #567.
func (e *Executor) executeRestartWorker(approval *state.Approval) Result {
	return e.executeWorkerControl(approval, "restart_worker", false)
}

func (e *Executor) executeWorkerControl(approval *state.Approval, verb string, stop bool) Result {
	if approval.Target == nil || strings.TrimSpace(approval.Target.Session) == "" {
		return Result{Status: state.ApprovalStatusExecutionFailed, Err: fmt.Errorf("%w: session/slot missing", ErrMissingTarget)}
	}
	if e.Workers == nil {
		return Result{
			Status:  state.ApprovalStatusExecutionFailed,
			Summary: fmt.Sprintf("%s: no worker controller wired into executor", verb),
			Err:     errors.New("nil worker controller"),
		}
	}

	slot := approval.Target.Session

	// Slot-reuse fence — same shape as executeDeleteWorktree (#488 /
	// premortem #7). By the time the operator approves a stop/restart,
	// the slot may have been recycled to a different live issue.
	// Refuse to act when the live session belongs to another issue.
	// When Sessions is not wired (older callers / tests) fall through
	// without the fence so the verb still functions.
	var sess *state.Session
	if e.Sessions != nil {
		live, ok := e.Sessions.LookupSession(slot)
		if !ok || live == nil {
			return Result{
				Status:  state.ApprovalStatusExecutionSkipped,
				Summary: fmt.Sprintf("%s: no session bound to slot %s", verb, slot),
			}
		}
		if approval.Target.Issue > 0 && live.IssueNumber != approval.Target.Issue {
			return Result{
				Status: state.ApprovalStatusExecutionFailed,
				Summary: fmt.Sprintf(
					"slot %s now bound to issue #%d (approval expected #%d) — refusing to %s a recycled worker",
					slot, live.IssueNumber, approval.Target.Issue, verb,
				),
				Err: fmt.Errorf("slot %s reused: live=#%d approval=#%d", slot, live.IssueNumber, approval.Target.Issue),
			}
		}
		sess = live
	}

	var err error
	if stop {
		err = e.Workers.StopWorker(slot, sess)
	} else {
		err = e.Workers.RestartWorker(slot, sess)
	}
	if err != nil {
		return Result{
			Status:  state.ApprovalStatusExecutionFailed,
			Summary: fmt.Sprintf("%s on slot %s: %v", verb, slot, err),
			Err:     fmt.Errorf("%s on slot %s: %w", verb, slot, err),
		}
	}
	summary := fmt.Sprintf("stopped worker on slot %s", slot)
	if !stop {
		summary = fmt.Sprintf("restarted worker on slot %s (queued for respawn)", slot)
	}
	return Result{
		Status:  state.ApprovalStatusExecuted,
		Summary: summary,
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
