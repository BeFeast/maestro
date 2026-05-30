package approver

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// fakeGH lets tests stub MergePR and CloseIssue.
type fakeGH struct {
	mergeCalls []int
	closeCalls []closeCall
	mergeErr   error
	closeErr   error
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

// TestExecute_SpawnWorker_ReturnsExecutionSkippedWithActionableSummary verifies
// the fix for issue #443: approving a spawn_worker approval used to print
// "No risky action was executed". The executor now records the approval as
// execution_skipped with a clear summary so the operator knows the next
// dispatcher loop will start the worker.
func TestExecute_SpawnWorker_ReturnsExecutionSkippedWithActionableSummary(t *testing.T) {
	ex := &Executor{Cfg: newCfg()}
	a := mkApproval("spawn_worker", &state.SupervisorTarget{Issue: 170}, "spawn me", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionSkipped {
		t.Fatalf("status = %q, want %q", res.Status, state.ApprovalStatusExecutionSkipped)
	}
	if !strings.Contains(res.Summary, "#170") {
		t.Fatalf("summary = %q, want issue ref", res.Summary)
	}
	if !strings.Contains(strings.ToLower(res.Summary), "next dispatcher loop") {
		t.Fatalf("summary = %q, want operator-facing hint about dispatcher loop", res.Summary)
	}
	if res.Err != nil {
		t.Fatalf("err = %v, want nil for skipped-status", res.Err)
	}
}

func TestExecute_OpenChildIssue_ReturnsExecutionSkippedWithActionableSummary(t *testing.T) {
	ex := &Executor{Cfg: newCfg()}
	a := mkApproval("open_child_issue", &state.SupervisorTarget{Issue: 146}, "create child", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionSkipped {
		t.Fatalf("status = %q, want %q", res.Status, state.ApprovalStatusExecutionSkipped)
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
