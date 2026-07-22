package approver

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/specgroom"
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
	mergeCalls    []int
	mergeHeads    []string
	closeCalls    []closeCall
	updateCalls   []int
	labelCalls    []labelCall
	editBodyCalls []editBodyCall
	mergeErr      error
	closeErr      error
	labelErr      error
	editBodyErr   error

	// issueBody is what IssueBody returns (the "current" live body used by
	// executeEditIssueBody's stale-edit guard); issueBodyErr forces a fetch
	// failure.
	issueBody    string
	issueBodyErr error

	// #547 stubs for PRMergeStatus.
	mergeable      string
	mergeState     string
	mergeStatusErr error
	// updateErr is returned by UpdateBranch.
	updateErr                 error
	headSHA                   string
	headErr                   error
	merged                    bool
	mergedErr                 error
	mergedAfterConcurrentCall bool
}

const testMergeHeadSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type closeCall struct {
	issue   int
	comment string
}

type labelCall struct {
	issue int
	label string
}

type editBodyCall struct {
	issue int
	body  string
}

func (f *fakeGH) MergePR(pr int) error {
	f.mergeCalls = append(f.mergeCalls, pr)
	return f.mergeErr
}
func (f *fakeGH) MergePRAtHead(pr int, headSHA string) error {
	f.mergeHeads = append(f.mergeHeads, headSHA)
	return f.MergePR(pr)
}
func (f *fakeGH) PRHeadSHA(int) (string, error) {
	if f.headErr != nil {
		return "", f.headErr
	}
	if f.headSHA != "" {
		return f.headSHA, nil
	}
	return testMergeHeadSHA, nil
}
func (f *fakeGH) IsPRMerged(int) (bool, error) {
	if f.mergedErr != nil {
		return false, f.mergedErr
	}
	return f.merged || (f.mergedAfterConcurrentCall && len(f.mergeCalls) > 0), nil
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
func (f *fakeGH) AddIssueLabel(issue int, label string) error {
	if f.labelErr != nil {
		return f.labelErr
	}
	f.labelCalls = append(f.labelCalls, labelCall{issue: issue, label: label})
	return nil
}
func (f *fakeGH) EditIssueBody(issue int, body string) error {
	if f.editBodyErr != nil {
		return f.editBodyErr
	}
	f.editBodyCalls = append(f.editBodyCalls, editBodyCall{issue: issue, body: body})
	return nil
}
func (f *fakeGH) IssueBody(issue int) (string, error) {
	if f.issueBodyErr != nil {
		return "", f.issueBodyErr
	}
	return f.issueBody, nil
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

func TestExecute_ApplyLessonProposal_AppendsWorkerPromptAndMarksApplied(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "worker-prompt.md")
	if err := os.WriteFile(promptPath, []byte("base prompt\n"), 0644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	st := state.NewState()
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	proposal, approval, created := st.RecordLessonProposal(state.LessonProposal{
		FailureClass:  "retry_exhausted",
		Area:          "issue:640",
		MinimalRepro:  "Session sup-155 status=retry_exhausted retry_count=3",
		SuggestedRule: "Inspect failed attempts before retrying.",
		Target:        state.LessonProposalTargetWorkerPrompt,
	}, now, "owner/repo", "owner/repo")
	if !created {
		t.Fatal("proposal was not created")
	}
	approval.Status = state.ApprovalStatusApproved
	approval.Audit = append(approval.Audit, state.ApprovalAudit{At: now.Add(time.Minute), Event: state.ApprovalAuditApproved, Actor: "operator"})

	ex := &Executor{Cfg: &config.Config{Repo: "owner/repo", WorkerPrompt: promptPath}, State: st}
	res := ex.Execute(approval)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("status = %q, want executed; res=%+v", res.Status, res)
	}
	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, proposal.ID) || !strings.Contains(content, "Inspect failed attempts before retrying.") {
		t.Fatalf("prompt content missing lesson block:\n%s", content)
	}
	stored, ok := st.FindLessonProposal(proposal.ID)
	if !ok {
		t.Fatalf("proposal %s missing", proposal.ID)
	}
	if stored.Status != state.LessonProposalStatusApplied || stored.AppliedAt == nil {
		t.Fatalf("proposal = %+v, want applied", stored)
	}
}

// --- merge_pr ---------------------------------------------------------------

func TestExecute_MergePR_HappyPath(t *testing.T) {
	gh := &fakeGH{}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 99, HeadSHA: testMergeHeadSHA}, "merge me", "")

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
	if len(gh.mergeHeads) != 1 || gh.mergeHeads[0] != testMergeHeadSHA {
		t.Fatalf("mergeHeads = %v, want atomic merge at %s", gh.mergeHeads, testMergeHeadSHA)
	}
}

func TestExecute_MergePR_AlreadyMergedIsIdempotentSuccess(t *testing.T) {
	gh := &fakeGH{merged: true}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 99, HeadSHA: testMergeHeadSHA}, "merge me", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted || res.Err != nil {
		t.Fatalf("already-merged result = %+v, want executed idempotent success", res)
	}
	if len(gh.mergeCalls) != 0 || len(gh.mergeHeads) != 0 {
		t.Fatalf("already-merged PR was merged again: calls=%v heads=%v", gh.mergeCalls, gh.mergeHeads)
	}
	if !strings.Contains(strings.ToLower(res.Summary), "already merged") {
		t.Fatalf("summary = %q, want already-merged truth", res.Summary)
	}
}

func TestExecute_MergePR_ConcurrentMergeAfterClickConvergesToExecuted(t *testing.T) {
	gh := &fakeGH{mergeErr: errors.New("pull request is already merged"), mergedAfterConcurrentCall: true}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 99, HeadSHA: testMergeHeadSHA}, "merge me", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted || res.Err != nil {
		t.Fatalf("concurrent-merge result = %+v, want executed idempotent success", res)
	}
	if len(gh.mergeCalls) != 1 {
		t.Fatalf("merge calls = %v, want one raced call", gh.mergeCalls)
	}
	if !strings.Contains(strings.ToLower(res.Summary), "concurrently") {
		t.Fatalf("summary = %q, want concurrent merge truth", res.Summary)
	}
}

func TestExecute_MergePR_ChangedHeadSkipsWithoutMerge(t *testing.T) {
	gh := &fakeGH{headSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 99, HeadSHA: testMergeHeadSHA}, "merge me", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionSkipped {
		t.Fatalf("status = %q, want execution_skipped; res=%+v", res.Status, res)
	}
	if len(gh.mergeCalls) != 0 || len(gh.mergeHeads) != 0 {
		t.Fatalf("changed head must not merge: calls=%v heads=%v", gh.mergeCalls, gh.mergeHeads)
	}
	if !strings.Contains(strings.ToLower(res.Summary), "head changed") {
		t.Fatalf("summary = %q, want head-changed explanation", res.Summary)
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
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 542, HeadSHA: testMergeHeadSHA}, "merge", "")

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
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 100, HeadSHA: testMergeHeadSHA}, "merge", "")

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
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 7, HeadSHA: testMergeHeadSHA}, "merge", "")

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
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 8, HeadSHA: testMergeHeadSHA}, "merge", "")

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
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 9, HeadSHA: testMergeHeadSHA}, "merge", "")

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
	a := mkApproval(config.SupervisorActionMergePR, &state.SupervisorTarget{PR: 11, HeadSHA: testMergeHeadSHA}, "merge", "")

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

func TestExecute_EditIssueBody_AppliesRewriteOnApprove(t *testing.T) {
	gh := &fakeGH{}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionEditIssueBody, &state.SupervisorTarget{Issue: 12, Body: "## Summary\nGroomed body"}, "apply rewrite", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("res = %+v", res)
	}
	if len(gh.editBodyCalls) != 1 || gh.editBodyCalls[0].issue != 12 || gh.editBodyCalls[0].body != "## Summary\nGroomed body" {
		t.Fatalf("editBodyCalls = %+v", gh.editBodyCalls)
	}
}

func TestExecute_EditIssueBody_MissingIssueFails(t *testing.T) {
	ex := &Executor{GH: &fakeGH{}, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionEditIssueBody, &state.SupervisorTarget{Body: "x"}, "s", "")
	res := ex.Execute(a)
	if !errors.Is(res.Err, ErrMissingTarget) {
		t.Fatalf("res = %+v, want ErrMissingTarget", res)
	}
}

func TestExecute_EditIssueBody_EmptyBodyRefused(t *testing.T) {
	gh := &fakeGH{}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionEditIssueBody, &state.SupervisorTarget{Issue: 12, Body: "   "}, "s", "")
	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("empty body must fail, got %+v", res)
	}
	if len(gh.editBodyCalls) != 0 {
		t.Fatalf("must not call EditIssueBody with an empty body: %+v", gh.editBodyCalls)
	}
}

func TestExecute_EditIssueBody_PropagatesGitHubError(t *testing.T) {
	gh := &fakeGH{editBodyErr: errors.New("gh boom")}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionEditIssueBody, &state.SupervisorTarget{Issue: 12, Body: "body"}, "s", "")
	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed || res.Err == nil {
		t.Fatalf("expected execution_failed with err, got %+v", res)
	}
}

// TestExecute_EditIssueBody_RefusesStaleBody pins the #851-review guard: if the
// live issue body changed after the rewrite was proposed (BaseBodyHash no longer
// matches), the executor must refuse rather than clobber the intervening edit.
func TestExecute_EditIssueBody_RefusesStaleBody(t *testing.T) {
	gh := &fakeGH{issueBody: "the CURRENT body after a manual edit"}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	base := specgroom.BodyHash("the body the rewrite was groomed against")
	a := mkApproval(config.SupervisorActionEditIssueBody,
		&state.SupervisorTarget{Issue: 12, Body: "## Summary\nGroomed", BaseBodyHash: base}, "apply", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("a body that changed since the proposal must fail, got %+v", res)
	}
	if len(gh.editBodyCalls) != 0 {
		t.Fatalf("must not overwrite an intervening edit: %+v", gh.editBodyCalls)
	}
}

// TestExecute_EditIssueBody_AppliesWhenBaseBodyMatches confirms the guard is not
// over-eager: when the live body still matches what was groomed, the rewrite is
// applied.
func TestExecute_EditIssueBody_AppliesWhenBaseBodyMatches(t *testing.T) {
	const current = "the exact body the rewrite was groomed against"
	gh := &fakeGH{issueBody: current}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionEditIssueBody,
		&state.SupervisorTarget{Issue: 12, Body: "## Summary\nGroomed", BaseBodyHash: specgroom.BodyHash(current)}, "apply", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("a matching base body should apply, got %+v", res)
	}
	if len(gh.editBodyCalls) != 1 || gh.editBodyCalls[0].body != "## Summary\nGroomed" {
		t.Fatalf("expected the rewrite applied once: %+v", gh.editBodyCalls)
	}
}

// TestExecute_EditIssueBody_StaleGuardFailsClosedOnFetchError verifies the guard
// fails closed: if the live body cannot be re-read, the executor refuses rather
// than apply a possibly-stale overwrite.
func TestExecute_EditIssueBody_StaleGuardFailsClosedOnFetchError(t *testing.T) {
	gh := &fakeGH{issueBodyErr: errors.New("gh down")}
	ex := &Executor{GH: gh, Cfg: newCfg()}
	a := mkApproval(config.SupervisorActionEditIssueBody,
		&state.SupervisorTarget{Issue: 12, Body: "x", BaseBodyHash: "somehash"}, "apply", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed || res.Err == nil {
		t.Fatalf("a failed body fetch must fail closed, got %+v", res)
	}
	if len(gh.editBodyCalls) != 0 {
		t.Fatalf("must not edit when the guard cannot verify the live body: %+v", gh.editBodyCalls)
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

// #964: an approval stamped for an earlier session snapshot must not execute
// after the canonical session changes. This fences delayed watchdog approvals
// before the worker controller can terminate a replacement worker.
func TestExecute_RestartWorker_StaleTargetStateSkipped(t *testing.T) {
	wc := &fakeWorkers{}
	target := &state.SupervisorTarget{Session: "slot-1", Issue: 42}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{IssueNumber: 42, Status: state.StatusDead, RetryCount: 1}
	a := mkApproval(config.SupervisorActionRestartWorker, target, "restart me", "")
	a.TargetStateHash = st.ApprovalTargetStateHash(target)

	// A repair has already started since the approval was minted.
	st.Sessions["slot-1"].Status = state.StatusRunning
	st.Sessions["slot-1"].StartedAt = time.Now().UTC()
	ex := &Executor{
		Cfg:      newCfg(),
		Workers:  wc,
		Sessions: fakeSessions{"slot-1": st.Sessions["slot-1"]},
		State:    st,
	}

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionSkipped {
		t.Fatalf("status = %q, want execution_skipped; res=%+v", res.Status, res)
	}
	if len(wc.restartCalls) != 0 || len(wc.stopCalls) != 0 {
		t.Fatalf("stale approval must not touch the worker; restart=%+v stop=%+v", wc.restartCalls, wc.stopCalls)
	}
	if !strings.Contains(res.Summary, "changed after approval") || !strings.Contains(res.Summary, "no worker or worktree was touched") {
		t.Fatalf("summary = %q, want stale-state and no-side-effect explanation", res.Summary)
	}
}

// #964: a PR-less retained worktree is still canonical durable work. The
// destructive restart controller must not run; repair resumes it in place.
func TestExecute_RestartWorker_RefusedForRetainedWorktree(t *testing.T) {
	wc := &fakeWorkers{}
	sess := &state.Session{IssueNumber: 42, Status: state.StatusDead, Worktree: "/srv/wt/slot-1"}
	ex := &Executor{Cfg: newCfg(), Workers: wc, Sessions: fakeSessions{"slot-1": sess}}
	a := mkApproval(config.SupervisorActionRestartWorker, &state.SupervisorTarget{Session: "slot-1", Issue: 42}, "restart me", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed; res=%+v", res.Status, res)
	}
	if len(wc.restartCalls) != 0 || len(wc.stopCalls) != 0 {
		t.Fatalf("retained worktree must not be touched; restart=%+v stop=%+v", wc.restartCalls, wc.stopCalls)
	}
	if !strings.Contains(res.Summary, "/srv/wt/slot-1") || !strings.Contains(res.Summary, "spawn_repair_worker") {
		t.Fatalf("summary = %q, want retained path and in-place repair guidance", res.Summary)
	}
}

// #874: restart_worker on a session that owns an open PR is refused before the
// controller runs — restart deletes the worktree, which would strand or discard
// the PR branch. The failure summary names the supported in-place alternative.
func TestExecute_RestartWorker_RefusedForOpenPR(t *testing.T) {
	wc := &fakeWorkers{}
	// Finished pr_open gate: worker ended, only live state is the open PR.
	sess := &state.Session{IssueNumber: 42, Status: state.StatusPROpen, PRNumber: 867, Worktree: "/wt/slot-1"}
	ex := &Executor{Cfg: newCfg(), Workers: wc, Sessions: fakeSessions{"slot-1": sess}}
	a := mkApproval(config.SupervisorActionRestartWorker, &state.SupervisorTarget{Session: "slot-1", Issue: 42}, "restart me", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed (open PR); res=%+v", res.Status, res)
	}
	if len(wc.restartCalls) != 0 {
		t.Fatalf("RestartWorker must NOT be called for an open-PR session; got %+v", wc.restartCalls)
	}
	if !strings.Contains(res.Summary, "PR #867") || !strings.Contains(res.Summary, "spawn_review_repair") {
		t.Fatalf("summary = %q, want it to name the open PR and the review-repair alternative", res.Summary)
	}
}

// #874: stop_worker is still allowed for an open-PR session — terminating a
// worker whose PR is open is legitimate and does not touch the PR on GitHub.
func TestExecute_StopWorker_AllowedForOpenPR(t *testing.T) {
	wc := &fakeWorkers{}
	sess := &state.Session{IssueNumber: 42, Status: state.StatusPROpen, PRNumber: 867}
	ex := &Executor{Cfg: newCfg(), Workers: wc, Sessions: fakeSessions{"slot-1": sess}}
	a := mkApproval(config.SupervisorActionStopWorker, &state.SupervisorTarget{Session: "slot-1", Issue: 42}, "stop me", "")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("status = %q, want executed (stop allowed with open PR); res=%+v", res.Status, res)
	}
	if len(wc.stopCalls) != 1 {
		t.Fatalf("stopCalls = %+v, want exactly one", wc.stopCalls)
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

// --- label_issue_ready (#736) ----------------------------------------------

// TestExecute_LabelIssueReady_AppliesConfiguredReadyLabel verifies the
// approval-gated ready-label handoff (#736): when the operator gates the
// verb behind approval_required, approving the minted approval applies the
// configured ready label to the target issue. The label name comes from
// cfg (the approval payload only carries the target issue).
func TestExecute_LabelIssueReady_AppliesConfiguredReadyLabel(t *testing.T) {
	gh := &fakeGH{}
	cfg := newCfg()
	cfg.Supervisor.ReadyLabel = "maestro-ready"
	ex := &Executor{GH: gh, Cfg: cfg}
	a := mkApproval("label_issue_ready", &state.SupervisorTarget{Issue: 266}, "label ready", "operator ok")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("status = %q, want executed; res=%+v", res.Status, res)
	}
	if len(gh.labelCalls) != 1 || gh.labelCalls[0].issue != 266 || gh.labelCalls[0].label != "maestro-ready" {
		t.Fatalf("label calls = %#v, want one #266:maestro-ready", gh.labelCalls)
	}
	if !strings.Contains(res.Summary, "#266") || !strings.Contains(res.Summary, "maestro-ready") {
		t.Fatalf("summary = %q, want it to name the issue and label", res.Summary)
	}
}

// Falls back to the first issue_labels entry when supervisor.ready_label
// is unset — mirroring supervisor.Engine.readyLabel.
func TestExecute_LabelIssueReady_FallsBackToIssueLabels(t *testing.T) {
	gh := &fakeGH{}
	cfg := newCfg()
	cfg.IssueLabels = []string{"ready"}
	ex := &Executor{GH: gh, Cfg: cfg}
	a := mkApproval("label_issue_ready", &state.SupervisorTarget{Issue: 7}, "label ready", "ok")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("status = %q, want executed; res=%+v", res.Status, res)
	}
	if len(gh.labelCalls) != 1 || gh.labelCalls[0].label != "ready" {
		t.Fatalf("label calls = %#v, want one #7:ready", gh.labelCalls)
	}
}

func TestExecute_LabelIssueReady_MissingIssueFails(t *testing.T) {
	gh := &fakeGH{}
	cfg := newCfg()
	cfg.Supervisor.ReadyLabel = "maestro-ready"
	ex := &Executor{GH: gh, Cfg: cfg}
	a := mkApproval("label_issue_ready", &state.SupervisorTarget{}, "label ready", "ok")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed for missing issue", res.Status)
	}
	if len(gh.labelCalls) != 0 {
		t.Fatalf("label calls = %#v, want none on failure", gh.labelCalls)
	}
}

func TestExecute_LabelIssueReady_NoReadyLabelConfiguredFails(t *testing.T) {
	gh := &fakeGH{}
	ex := &Executor{GH: gh, Cfg: newCfg()} // no ready_label / issue_labels
	a := mkApproval("label_issue_ready", &state.SupervisorTarget{Issue: 9}, "label ready", "ok")

	res := ex.Execute(a)
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed when no ready label is configured", res.Status)
	}
	if len(gh.labelCalls) != 0 {
		t.Fatalf("label calls = %#v, want none on failure", gh.labelCalls)
	}
}
