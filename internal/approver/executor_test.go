package approver

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// fakeGH lets tests stub MergePR, CloseIssue, PRMergeStatus and
// UpdateBranch.
//
// By default PRMergeStatus returns ("", "", nil) which executeMergePR
// treats as "don't know, proceed to merge" — so the pre-#547 happy-path
// tests keep merging without any extra setup. Tests that exercise the
// #547 paths set mergeable/mergeState explicitly.
type fakeGH struct {
	mergeCalls  []int
	closeCalls  []closeCall
	updateCalls []int
	mergeErr    error
	closeErr    error

	// #547 stubs for PRMergeStatus.
	mergeable      string
	mergeState     string
	mergeStatusErr error
	// updateErr is returned by UpdateBranch.
	updateErr error
}

type closeCall struct {
	issue   int
	comment string
}

func (f *fakeGH) MergePR(pr int) error {
	f.mergeCalls = append(f.mergeCalls, pr)
	return f.mergeErr
}
func (f *fakeGH) CloseIssue(issue int, comment string) error {
	f.closeCalls = append(f.closeCalls, closeCall{issue: issue, comment: comment})
	return f.closeErr
}
func (f *fakeGH) PRMergeStatus(pr int) (string, string, error) {
	return f.mergeable, f.mergeState, f.mergeStatusErr
}
func (f *fakeGH) UpdateBranch(pr int) error {
	f.updateCalls = append(f.updateCalls, pr)
	return f.updateErr
}

type fakeWT struct {
	calls []wtCall
	err   error
}
type wtCall struct {
	localPath, worktree string
}

func (f *fakeWT) RemoveWorktree(localPath, worktreePath string) error {
	f.calls = append(f.calls, wtCall{localPath: localPath, worktree: worktreePath})
	return f.err
}

func newCfg() *config.Config {
	return &config.Config{
		Repo:         "owner/repo",
		LocalPath:    "/srv/maestro",
		WorktreeBase: "/srv/wt",
	}
}

func mkApproval(action string, target *state.SupervisorTarget, summary, approveReason string) *state.Approval {
	now := time.Now().UTC()
	a := &state.Approval{
		ID:        "approval-test",
		CreatedAt: now,
		UpdatedAt: now,
		Action:    action,
		Target:    target,
		Summary:   summary,
		Risk:      "high",
		Status:    state.ApprovalStatusApproved,
	}
	a.Audit = []state.ApprovalAudit{
		{At: now, Event: state.ApprovalAuditCreated},
	}
	if approveReason != "" {
		a.Audit = append(a.Audit, state.ApprovalAudit{At: now, Event: state.ApprovalAuditApproved, Actor: "cli", Reason: approveReason})
	}
	return a
}

// --- spawn_worker / open_child_issue approval semantics --------------------

// TestExecute_SpawnWorker_ReturnsAwaitingDispatchWithActionableSummary verifies
// the post-#515 contract: approving a spawn_worker approval moves it to
// status=awaiting_dispatch (not execution_skipped) so the at-mint dedup in
// state.RecordPendingApprovalForDecision still treats it as effective and
// supervisor does not mint a fresh pending duplicate while the dispatcher
// loop has not yet ticked. (Original #443 fix preserved: the operator
// still sees an actionable summary, never the old "No risky action was
// executed" line.)
func TestExecute_SpawnWorker_ReturnsAwaitingDispatchWithActionableSummary(t *testing.T) {
	ex := &Executor{Cfg: newCfg()}
	a := mkApproval("spawn_worker", &state.SupervisorTarget{Issue: 170}, "spawn me", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusAwaitingDispatch {
		t.Fatalf("status = %q, want %q (#515)", res.Status, state.ApprovalStatusAwaitingDispatch)
	}
	if !strings.Contains(res.Summary, "#170") {
		t.Fatalf("summary = %q, want issue ref", res.Summary)
	}
	if !strings.Contains(strings.ToLower(res.Summary), "next dispatcher loop") {
		t.Fatalf("summary = %q, want operator-facing hint about dispatcher loop", res.Summary)
	}
	if res.Err != nil {
		t.Fatalf("err = %v, want nil for awaiting-dispatch", res.Err)
	}
}

func TestExecute_OpenChildIssue_ReturnsAwaitingDispatchWithActionableSummary(t *testing.T) {
	ex := &Executor{Cfg: newCfg()}
	a := mkApproval("open_child_issue", &state.SupervisorTarget{Issue: 146}, "create child", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusAwaitingDispatch {
		t.Fatalf("status = %q, want %q (#515)", res.Status, state.ApprovalStatusAwaitingDispatch)
	}
	if !strings.Contains(res.Summary, "#146") {
		t.Fatalf("summary = %q, want epic ref", res.Summary)
	}
	if !strings.Contains(strings.ToLower(res.Summary), "manually") {
		t.Fatalf("summary = %q, want manual-create hint for v1", res.Summary)
	}
}

// --- merge_pr ---------------------------------------------------------------

func TestExecute_MergePR_HappyPath(t *testing.T) {
	gh := &fakeGH{}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 99}, "merge me", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("status = %q, want executed; res=%+v", res.Status, res)
	}
	if !strings.Contains(res.Summary, "#99") {
		t.Fatalf("summary = %q", res.Summary)
	}
	if len(gh.mergeCalls) != 1 || gh.mergeCalls[0] != 99 {
		t.Fatalf("mergeCalls = %v", gh.mergeCalls)
	}
}

// --- #547: BEHIND-but-mergeable PR -> update-branch, do not fail --------------

// TestExecute_MergePR_Behind_UpdatesBranchAndSkips verifies the core #547
// fix: a green PR that has fallen behind main is brought up to date via
// UpdateBranch and returns a NON-failure status (execution_skipped) with a
// re-validate summary, WITHOUT calling MergePR. The next supervisor cycle
// re-mints against the new head.
func TestExecute_MergePR_Behind_UpdatesBranchAndSkips(t *testing.T) {
	gh := &fakeGH{mergeable: "MERGEABLE", mergeState: "behind"}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 542}, "merge", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionSkipped {
		t.Fatalf("status = %q, want execution_skipped; res=%+v", res.Status, res)
	}
	if len(gh.updateCalls) != 1 || gh.updateCalls[0] != 542 {
		t.Fatalf("updateCalls = %v, want [542]", gh.updateCalls)
	}
	if len(gh.mergeCalls) != 0 {
		t.Fatalf("MergePR must NOT be called when behind; got %v", gh.mergeCalls)
	}
	if res.Err != nil {
		t.Fatalf("err = %v, want nil (behind is recoverable, not a failure)", res.Err)
	}
	if !strings.Contains(res.Summary, "#542") || !strings.Contains(strings.ToLower(res.Summary), "behind") {
		t.Fatalf("summary = %q, want PR ref + behind explanation", res.Summary)
	}
	if !strings.Contains(strings.ToLower(res.Summary), "re-validate") && !strings.Contains(strings.ToLower(res.Summary), "next supervisor cycle") {
		t.Fatalf("summary = %q, want hint about re-validation on next cycle", res.Summary)
	}
}

// TestExecute_MergePR_Clean_Merges verifies a CLEAN+mergeable PR still
// merges normally (no update-branch detour).
func TestExecute_MergePR_Clean_Merges(t *testing.T) {
	gh := &fakeGH{mergeable: "MERGEABLE", mergeState: "clean"}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 100}, "merge", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("status = %q, want executed; res=%+v", res.Status, res)
	}
	if len(gh.mergeCalls) != 1 || gh.mergeCalls[0] != 100 {
		t.Fatalf("mergeCalls = %v, want [100]", gh.mergeCalls)
	}
	if len(gh.updateCalls) != 0 {
		t.Fatalf("UpdateBranch must NOT be called for a clean PR; got %v", gh.updateCalls)
	}
}

// TestExecute_MergePR_Conflicting_Fails verifies a genuinely unmergeable PR
// (real conflict) returns execution_failed and never touches MergePR or
// UpdateBranch — execution_failed is reserved for this case.
func TestExecute_MergePR_Conflicting_Fails(t *testing.T) {
	gh := &fakeGH{mergeable: "CONFLICTING", mergeState: "dirty"}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 7}, "merge", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed || res.Err == nil {
		t.Fatalf("res = %+v, want execution_failed with non-nil Err", res)
	}
	if len(gh.mergeCalls) != 0 {
		t.Fatalf("MergePR must NOT be called for a conflicting PR; got %v", gh.mergeCalls)
	}
	if len(gh.updateCalls) != 0 {
		t.Fatalf("UpdateBranch must NOT be called for a conflicting PR; got %v", gh.updateCalls)
	}
	if !strings.Contains(strings.ToLower(res.Summary), "conflict") {
		t.Fatalf("summary = %q, want conflict explanation", res.Summary)
	}
}

// TestExecute_MergePR_BehindButConflicting_DoesNotUpdate guards the AND in
// the BEHIND branch: a PR reported behind but also CONFLICTING must fall to
// the conflict failure, not silently update-branch.
func TestExecute_MergePR_BehindButConflicting_DoesNotUpdate(t *testing.T) {
	gh := &fakeGH{mergeable: "CONFLICTING", mergeState: "behind"}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 8}, "merge", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("res = %+v, want execution_failed", res)
	}
	if len(gh.updateCalls) != 0 {
		t.Fatalf("UpdateBranch must NOT run for a conflicting PR; got %v", gh.updateCalls)
	}
}

// TestExecute_MergePR_Behind_UpdateBranchError_Fails verifies that if the
// update-branch call itself errors, that surfaces as execution_failed (so
// the operator sees the real blocker) and MergePR is not attempted.
func TestExecute_MergePR_Behind_UpdateBranchError_Fails(t *testing.T) {
	gh := &fakeGH{mergeable: "MERGEABLE", mergeState: "behind", updateErr: errors.New("update boom")}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 9}, "merge", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed || res.Err == nil {
		t.Fatalf("res = %+v, want execution_failed", res)
	}
	if len(gh.mergeCalls) != 0 {
		t.Fatalf("MergePR must NOT be called after a failed update-branch; got %v", gh.mergeCalls)
	}
	if !strings.Contains(res.Err.Error(), "update branch for PR #9") {
		t.Fatalf("err = %v, want update-branch context", res.Err)
	}
}

// TestExecute_MergePR_StatusFetchError_StillMerges verifies that a failure
// to fetch the merge status does NOT block the merge — we fall through to
// the original MergePR path (gh pr merge does its own gating). This keeps
// the change additive and avoids a status-endpoint flake stalling merges.
func TestExecute_MergePR_StatusFetchError_StillMerges(t *testing.T) {
	gh := &fakeGH{mergeStatusErr: errors.New("rate limited")}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 11}, "merge", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("res = %+v, want executed (fall through on status error)", res)
	}
	if len(gh.mergeCalls) != 1 || gh.mergeCalls[0] != 11 {
		t.Fatalf("mergeCalls = %v, want [11]", gh.mergeCalls)
	}
	if len(gh.updateCalls) != 0 {
		t.Fatalf("UpdateBranch must NOT run on a status-fetch error; got %v", gh.updateCalls)
	}
}

func TestExecute_MergePR_RequiresPRTarget(t *testing.T) {
	ex := &Executor{GH: &fakeGH{}, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionMergePR, nil, "no target", "")
	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed || !errors.Is(res.Err, ErrMissingTarget) {
		t.Fatalf("res = %+v, want execution_failed + ErrMissingTarget", res)
	}
}

func TestExecute_MergePR_GHError_Surfaces(t *testing.T) {
	gh := &fakeGH{mergeErr: errors.New("boom")}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 1}, "x", "")
	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed || res.Err == nil {
		t.Fatalf("res = %+v, want execution_failed", res)
	}
	if !strings.Contains(res.Err.Error(), "merge PR #1") {
		t.Fatalf("err message lacks PR id: %v", res.Err)
	}
}

func TestExecute_MergePR_NilGH(t *testing.T) {
	ex := &Executor{Cfg: newCfg()} // no GH
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 1}, "x", "")
	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed || res.Err == nil {
		t.Fatalf("res = %+v, want execution_failed", res)
	}
}

// --- close_issue ------------------------------------------------------------

func TestExecute_CloseIssue_HappyPath_UsesApproveReasonAsComment(t *testing.T) {
	gh := &fakeGH{}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionCloseIssue, &state.SupervisorTarget{Issue: 7}, "summary fallback", "obsolete after refactor")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("res = %+v", res)
	}
	if len(gh.closeCalls) != 1 || gh.closeCalls[0].issue != 7 || gh.closeCalls[0].comment != "obsolete after refactor" {
		t.Fatalf("closeCalls = %+v", gh.closeCalls)
	}
}

func TestExecute_CloseIssue_NoApproveReason_UsesSummary(t *testing.T) {
	gh := &fakeGH{}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionCloseIssue, &state.SupervisorTarget{Issue: 8}, "summary text", "")
	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("res = %+v", res)
	}
	if gh.closeCalls[0].comment != "summary text" {
		t.Fatalf("expected summary fallback, got %q", gh.closeCalls[0].comment)
	}
}

func TestExecute_CloseIssue_RequiresTarget(t *testing.T) {
	ex := &Executor{GH: &fakeGH{}, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionCloseIssue, nil, "x", "")
	res := ex.Execute(a)
	if !errors.Is(res.Err, ErrMissingTarget) {
		t.Fatalf("res = %+v, want ErrMissingTarget", res)
	}
}

func TestExecute_CloseIssueBatch_ClosesAllTargetsAfterApproval(t *testing.T) {
	gh := &fakeGH{}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionCloseIssueBatch, &state.SupervisorTarget{
		Issues: []state.SupervisorIssueTarget{
			{Issue: 7, PR: 70},
			{Issue: 8, PR: 80},
		},
	}, "", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("res = %+v, want executed", res)
	}
	if len(gh.closeCalls) != 2 {
		t.Fatalf("closeCalls = %d, want 2: %+v", len(gh.closeCalls), gh.closeCalls)
	}
	if gh.closeCalls[0].issue != 7 || !strings.Contains(gh.closeCalls[0].comment, "PR #70") {
		t.Fatalf("first close call = %+v, want issue #7 comment naming PR #70", gh.closeCalls[0])
	}
	if gh.closeCalls[1].issue != 8 || !strings.Contains(gh.closeCalls[1].comment, "PR #80") {
		t.Fatalf("second close call = %+v, want issue #8 comment naming PR #80", gh.closeCalls[1])
	}
}

// --- delete_worktree --------------------------------------------------------

func TestExecute_DeleteWorktree_HappyPath_AnchoredUnderBase(t *testing.T) {
	wt := &fakeWT{}
	cfg := newCfg()
	ex := &Executor{Worktrees: wt, Cfg: cfg}
	a := mkApproval(config.SupervisorActionDeleteWorktree, &state.SupervisorTarget{Session: "sup-77"}, "stale", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("res = %+v", res)
	}
	if len(wt.calls) != 1 {
		t.Fatalf("wt.calls = %v", wt.calls)
	}
	if wt.calls[0].localPath != "/srv/maestro" {
		t.Fatalf("localPath = %q", wt.calls[0].localPath)
	}
	if wt.calls[0].worktree != "/srv/wt/sup-77" {
		t.Fatalf("worktree = %q, want /srv/wt/sup-77", wt.calls[0].worktree)
	}
}

func TestExecute_DeleteWorktree_RefusesPathTraversal(t *testing.T) {
	wt := &fakeWT{}
	ex := &Executor{Worktrees: wt, Cfg: newCfg()}
	for _, evil := range []string{"../etc", "../../tmp", "/tmp/x", "a/b", "."} {
		a := mkApproval(config.SupervisorActionDeleteWorktree, &state.SupervisorTarget{Session: evil}, "x", "")
		res := ex.Execute(a)
		if res.Status != state.ApprovalStatusExecutionFailed || res.Err == nil {
			t.Fatalf("slot %q: res = %+v, want execution_failed", evil, res)
		}
		if len(wt.calls) != 0 {
			t.Fatalf("slot %q: RemoveWorktree was called: %+v", evil, wt.calls)
		}
	}
}

func TestExecute_DeleteWorktree_RequiresWorktreeBase(t *testing.T) {
	cfg := newCfg()
	cfg.WorktreeBase = ""
	ex := &Executor{Worktrees: &fakeWT{}, Cfg: cfg}
	a := mkApproval(config.SupervisorActionDeleteWorktree, &state.SupervisorTarget{Session: "sup-1"}, "x", "")
	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("res = %+v", res)
	}
}

// --- change_global_config (intentionally skipped) ---------------------------

func TestExecute_ChangeGlobalConfig_Skipped(t *testing.T) {
	ex := &Executor{Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionChangeGlobalConfig, nil, "swap backend", "")
	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionSkipped {
		t.Fatalf("res = %+v, want execution_skipped", res)
	}
	if !strings.Contains(strings.ToLower(res.Summary), "manual") {
		t.Fatalf("summary should explain manual edit, got %q", res.Summary)
	}
}

// --- unknown / nil ----------------------------------------------------------

func TestExecute_UnknownAction_Failed(t *testing.T) {
	ex := &Executor{Cfg: newCfg()}
	a := mkApproval("do_a_barrel_roll", nil, "x", "")
	res := ex.Execute(a)
	if !errors.Is(res.Err, ErrUnknownAction) {
		t.Fatalf("res = %+v, want ErrUnknownAction", res)
	}
}

func TestExecute_NilApproval(t *testing.T) {
	ex := &Executor{}
	res := ex.Execute(nil)
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("res = %+v", res)
	}
}

// --- WorktreePathForSlot edges --------------------------------------------

func TestWorktreePathForSlot_TrailingSlashBase(t *testing.T) {
	cfg := newCfg()
	cfg.WorktreeBase = "/srv/wt/"
	got, err := WorktreePathForSlot(cfg, "scr-99")
	if err != nil || got != "/srv/wt/scr-99" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

// --- #567: restart_worker / stop_worker ----------------------------------

type fakeWorkers struct {
	stopCalls    []wcCall
	restartCalls []wcCall
	stopErr      error
	restartErr   error
}

type wcCall struct {
	slot string
	sess *state.Session
}

func (f *fakeWorkers) StopWorker(slot string, sess *state.Session) error {
	f.stopCalls = append(f.stopCalls, wcCall{slot: slot, sess: sess})
	return f.stopErr
}

func (f *fakeWorkers) RestartWorker(slot string, sess *state.Session) error {
	f.restartCalls = append(f.restartCalls, wcCall{slot: slot, sess: sess})
	return f.restartErr
}

type fakeSessions map[string]*state.Session

func (f fakeSessions) LookupSession(slot string) (*state.Session, bool) {
	s, ok := f[slot]
	return s, ok
}

func TestExecute_StopWorker_HappyPath(t *testing.T) {
	wc := &fakeWorkers{}
	sess := &state.Session{IssueNumber: 42, Status: state.StatusRunning}
	ex := &Executor{Cfg: newCfg(), Workers: wc, Sessions: fakeSessions{"slot-1": sess}}
	a := mkApproval(config.SupervisorActionStopWorker, &state.SupervisorTarget{Session: "slot-1", Issue: 42}, "stop me", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("status = %q, want executed; res=%+v", res.Status, res)
	}
	if len(wc.stopCalls) != 1 || wc.stopCalls[0].slot != "slot-1" {
		t.Fatalf("stopCalls = %+v", wc.stopCalls)
	}
	if len(wc.restartCalls) != 0 {
		t.Fatalf("restart must NOT be called for stop_worker; got %+v", wc.restartCalls)
	}
	if !strings.Contains(res.Summary, "slot-1") {
		t.Fatalf("summary = %q, want slot ref", res.Summary)
	}
}

func TestExecute_RestartWorker_HappyPath(t *testing.T) {
	wc := &fakeWorkers{}
	sess := &state.Session{IssueNumber: 42, Status: state.StatusRunning}
	ex := &Executor{Cfg: newCfg(), Workers: wc, Sessions: fakeSessions{"slot-1": sess}}
	a := mkApproval(config.SupervisorActionRestartWorker, &state.SupervisorTarget{Session: "slot-1", Issue: 42}, "restart me", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("status = %q, want executed; res=%+v", res.Status, res)
	}
	if len(wc.restartCalls) != 1 || wc.restartCalls[0].slot != "slot-1" {
		t.Fatalf("restartCalls = %+v", wc.restartCalls)
	}
	if len(wc.stopCalls) != 0 {
		t.Fatalf("stop must NOT be called for restart_worker; got %+v", wc.stopCalls)
	}
}

// TestExecute_StopWorker_SlotReuseFence pins the cautious-gate slot-reuse
// fence (#488 pattern): if the slot now binds a different issue than the
// approval targeted, refuse the stop without calling the controller.
func TestExecute_StopWorker_SlotReuseFence(t *testing.T) {
	wc := &fakeWorkers{}
	// Live session bound to issue 999, but approval targets issue 42.
	live := &state.Session{IssueNumber: 999, Status: state.StatusRunning}
	ex := &Executor{Cfg: newCfg(), Workers: wc, Sessions: fakeSessions{"slot-1": live}}
	a := mkApproval(config.SupervisorActionStopWorker, &state.SupervisorTarget{Session: "slot-1", Issue: 42}, "stop", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed (slot-reuse fence)", res.Status)
	}
	if len(wc.stopCalls) != 0 {
		t.Fatalf("StopWorker must NOT be called when slot is recycled; got %+v", wc.stopCalls)
	}
	if !strings.Contains(res.Summary, "recycled") {
		t.Fatalf("summary = %q, want mention of recycled slot", res.Summary)
	}
}

func TestExecute_StopWorker_NoControllerFails(t *testing.T) {
	// Workers nil → executor must report the misconfiguration loudly so
	// the operator doesn't get a silent no-op approval.
	ex := &Executor{Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionStopWorker, &state.SupervisorTarget{Session: "slot-1", Issue: 42}, "stop", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed (no controller wired)", res.Status)
	}
}

func TestExecute_StopWorker_MissingSlot(t *testing.T) {
	wc := &fakeWorkers{}
	ex := &Executor{Cfg: newCfg(), Workers: wc}
	a := mkApproval(config.SupervisorActionStopWorker, &state.SupervisorTarget{Issue: 42}, "stop", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed (missing slot)", res.Status)
	}
	if !errors.Is(res.Err, ErrMissingTarget) {
		t.Fatalf("err = %v, want ErrMissingTarget", res.Err)
	}
}

// TestExecute_RestartWorker_ControllerErrorSurfaces ensures the
// controller's error is wrapped and surfaced as execution_failed.
func TestExecute_RestartWorker_ControllerErrorSurfaces(t *testing.T) {
	wc := &fakeWorkers{restartErr: errors.New("tmux kill: ENOENT")}
	sess := &state.Session{IssueNumber: 42, Status: state.StatusRunning}
	ex := &Executor{Cfg: newCfg(), Workers: wc, Sessions: fakeSessions{"slot-1": sess}}
	a := mkApproval(config.SupervisorActionRestartWorker, &state.SupervisorTarget{Session: "slot-1", Issue: 42}, "restart", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed (controller failed)", res.Status)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "tmux kill") {
		t.Fatalf("err = %v, want wrapped controller error", res.Err)
	}
}

func TestExecute_StopWorker_NoLiveSessionSkipped(t *testing.T) {
	// Sessions wired but no session at the slot — return execution_skipped
	// rather than failing, so the operator sees an actionable summary
	// instead of a noisy execution_failed when the slot has already been
	// freed before the approval lands.
	wc := &fakeWorkers{}
	ex := &Executor{Cfg: newCfg(), Workers: wc, Sessions: fakeSessions{}}
	a := mkApproval(config.SupervisorActionStopWorker, &state.SupervisorTarget{Session: "slot-1", Issue: 42}, "stop", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionSkipped {
		t.Fatalf("status = %q, want execution_skipped (no live session)", res.Status)
	}
	if len(wc.stopCalls) != 0 {
		t.Fatalf("StopWorker must NOT be called when no session at slot; got %+v", wc.stopCalls)
	}
}

// TestWorkerControllerFuncs_AdapterDispatches verifies the function-pair
// adapter routes correctly to Stop / Restart.
func TestWorkerControllerFuncs_AdapterDispatches(t *testing.T) {
	var stopSlot, restartSlot string
	wc := WorkerControllerFuncs{
		Stop:    func(slot string, _ *state.Session) error { stopSlot = slot; return nil },
		Restart: func(slot string, _ *state.Session) error { restartSlot = slot; return nil },
	}
	if err := wc.StopWorker("a", nil); err != nil || stopSlot != "a" {
		t.Fatalf("stop dispatch: slot=%q err=%v", stopSlot, err)
	}
	if err := wc.RestartWorker("b", nil); err != nil || restartSlot != "b" {
		t.Fatalf("restart dispatch: slot=%q err=%v", restartSlot, err)
	}
}

func TestWorkerControllerFuncs_NilFunctionsReturnError(t *testing.T) {
	wc := WorkerControllerFuncs{}
	if err := wc.StopWorker("x", nil); err == nil {
		t.Fatal("StopWorker with nil Stop func should return an error, not panic")
	}
	if err := wc.RestartWorker("x", nil); err == nil {
		t.Fatal("RestartWorker with nil Restart func should return an error, not panic")
	}
}
