package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func TestFleetMergeRequestImmediatelyOverridesOpenGateWhilePreservingFact(t *testing.T) {
	for _, tt := range []struct {
		name       string
		score      int
		decision   state.PRGateReviewDecision
		passed     bool
		wantReview string
	}{
		{name: "passing gate", score: 4, decision: state.PRGateReviewPassed, passed: true, wantReview: "Greptile 4/5 · passed"},
		{name: "failing gate", score: 3, decision: state.PRGateReviewBlocked, wantReview: "Greptile 3/5 · repair required"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			stateDir := filepath.Join(dir, "state")
			now := time.Date(2026, 7, 15, 14, 16, 0, 0, time.UTC)
			st := state.NewState()
			st.Sessions["pcp-11"] = &state.Session{
				IssueNumber: 19, IssueTitle: "Honor merge intent", PRNumber: 26,
				Branch: "feat/merge-intent", Status: state.StatusPROpen, StartedAt: now.Add(-time.Hour),
			}
			transition := state.PRGateTransition{
				Project: "BeFeast/project-control-plane", IssueNumber: 19, PRNumber: 26, HeadSHA: strings.Repeat("a", 40),
				CIObserved: true, CIRollupVerdict: state.PRGateCISuccess, CIEffectiveVerdict: state.PRGateCISuccess,
				ReviewObserved: true, ReviewDecision: tt.decision, ReviewVerdictFingerprint: strings.Repeat("1", 16),
				ReviewStreams: []state.PRGateReviewStream{{
					Name: "greptile", Passed: tt.passed, Score: tt.score, ScoreMax: 5,
					Verdict: func() state.PRGateReviewVerdict {
						if tt.passed {
							return state.PRGateReviewVerdictOKToMerge
						}
						return state.PRGateReviewVerdictRepairRequired
					}(),
				}},
			}
			if tt.decision == state.PRGateReviewBlocked {
				transition.ActionableFindingsFingerprint = strings.Repeat("2", 16)
				transition.ActionableFindingsCount = 1
				transition.ReviewStreams[0].FindingsCount = 1
			}
			if _, _, err := st.RecordPRGateTransition(transition, now); err != nil {
				t.Fatal(err)
			}
			if err := state.Save(stateDir, st); err != nil {
				t.Fatal(err)
			}

			cfg := &config.Config{Repo: "BeFeast/project-control-plane", StateDir: stateDir, MaxParallel: 2}
			srv := NewFleet([]FleetProject{NewFleetProject("pcp", "", "", cfg)}, "127.0.0.1", 8786, false)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/actions", bytes.NewBufferString(`{"action_id":"merge_pr","project":"pcp","pr_number":26,"issue_number":19}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.handleFleetAction(w, req)
			if w.Code != http.StatusAccepted {
				t.Fatalf("POST status=%d body=%s", w.Code, w.Body.String())
			}
			var enqueue approvalEnqueueResponse
			if err := json.Unmarshal(w.Body.Bytes(), &enqueue); err != nil {
				t.Fatal(err)
			}

			resp := srv.snapshot()
			worker := findFleetWorker(t, resp.Workers, "pcp-11")
			if worker.PRGate == nil || worker.PRGate.MergeAction == nil {
				t.Fatalf("worker missing merge lifecycle: %+v", worker)
			}
			if worker.PRGate.MergeAction.ApprovalID != enqueue.ApprovalID || worker.PRGate.MergeAction.Status != string(state.ApprovalStatusPending) {
				t.Fatalf("merge action = %+v, enqueue=%+v", worker.PRGate.MergeAction, enqueue)
			}
			for _, text := range []string{worker.StatusReason, worker.NextAction, worker.PRGate.Summary} {
				if !strings.Contains(text, "Merge requested") || !strings.Contains(text, tt.wantReview) {
					t.Fatalf("lifecycle text %q missing merge intent/review fact %q", text, tt.wantReview)
				}
			}
			project := findFleetProject(t, resp.Projects, "pcp")
			if project.OperatorState.Kind != "merge_action_required" || project.OperatorState.ApprovalID != enqueue.ApprovalID {
				t.Fatalf("project operator state = %+v", project.OperatorState)
			}
			if !strings.Contains(resp.OperatorBrief.Sentence, "Merge requested") || !strings.Contains(resp.OperatorBrief.Sentence, tt.wantReview) {
				t.Fatalf("operator brief = %+v", resp.OperatorBrief)
			}
			if resp.NextAction == nil || !strings.Contains(resp.NextAction.Reason, tt.wantReview) {
				t.Fatalf("next action = %+v, want real review score", resp.NextAction)
			}
			merge := findControlAction(t, worker.Actions, "approve_merge")
			if !merge.Disabled || !strings.Contains(merge.DisabledReason, enqueue.ApprovalID) {
				t.Fatalf("duplicate merge CTA remained enabled: %+v", merge)
			}
		})
	}
}

func TestFleetMergeActionLifecycleTextIsDistinct(t *testing.T) {
	statuses := []state.ApprovalStatus{
		state.ApprovalStatusPending,
		state.ApprovalStatusApproved,
		state.ApprovalStatusAwaitingDispatch,
		state.ApprovalStatusExecuting,
		state.ApprovalStatusExecuted,
		state.ApprovalStatusRejected,
		state.ApprovalStatusExecutionFailed,
	}
	labels := map[string]state.ApprovalStatus{}
	for _, status := range statuses {
		got := makeFleetMergeActionState(state.Approval{
			ID: "approval-" + string(status), Action: config.SupervisorActionMergePR, Status: status,
			CreatedAt: time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC), Target: &state.SupervisorTarget{PR: 26},
		}, 26)
		if got.Status != string(status) || got.Label == "" || got.Summary == "" || got.NextAction == "" {
			t.Fatalf("status %q mapped incompletely: %+v", status, got)
		}
		if prior, duplicate := labels[got.Label]; duplicate {
			t.Fatalf("statuses %q and %q share operator label %q", prior, status, got.Label)
		}
		labels[got.Label] = status
	}
}

func TestFleetExplicitGreptileOKToMergeStaysOperatorVisible(t *testing.T) {
	got := fleetPRReviewStreamSummary(fleetPRReviewStream{
		Name: "greptile", Passed: true, Verdict: string(state.PRGateReviewVerdictOKToMerge),
	})
	if got != "Greptile · OK to merge" {
		t.Fatalf("explicit Greptile verdict = %q, want OK to merge", got)
	}
}

func TestFleetMergedTruthOutranksPendingApprovalAndStaleReview(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	now := time.Date(2026, 7, 15, 14, 18, 52, 0, time.UTC)
	st := state.NewState()
	finished := now.Add(-time.Minute)
	st.Sessions["pcp-11"] = &state.Session{
		IssueNumber: 19, IssueTitle: "Honor terminal truth", PRNumber: 26,
		Branch: "feat/terminal", Status: state.StatusRetryExhausted, StartedAt: now.Add(-time.Hour), FinishedAt: &finished,
	}
	st.SupervisorDecisions = append(st.SupervisorDecisions, state.SupervisorDecision{
		ID: "stale-review", CreatedAt: now.Add(-time.Minute), RecommendedAction: "monitor_open_pr",
		StuckStates: []state.SupervisorStuckState{{
			Code: "greptile_not_approved", Summary: "PR #26 is not approved by Greptile",
			RecommendedAction: "Address Greptile feedback", Target: &state.SupervisorTarget{Issue: 19, PR: 26},
		}},
	})
	approval := st.RecordPendingApprovalForDecision(state.SupervisorDecision{
		ID: "merge-request", CreatedAt: now.Add(-2 * time.Minute), Project: "pcp", Repo: "BeFeast/project-control-plane",
		RecommendedAction: config.SupervisorActionMergePR, Risk: "high", RequiresApproval: true,
		Target: &state.SupervisorTarget{Issue: 19, PR: 26}, Summary: "merge requested",
	}, now.Add(-2*time.Minute))
	if approval == nil {
		t.Fatal("pending merge approval not recorded")
	}
	head := strings.Repeat("a", 40)
	if _, _, err := st.RecordPRGateTransition(state.PRGateTransition{
		Project: "BeFeast/project-control-plane", IssueNumber: 19, PRNumber: 26, HeadSHA: head,
		CIObserved: true, CIRollupVerdict: state.PRGateCISuccess, CIEffectiveVerdict: state.PRGateCISuccess,
		ReviewObserved: true, ReviewDecision: state.PRGateReviewPassed, ReviewVerdictFingerprint: strings.Repeat("1", 16),
		ReviewStreams: []state.PRGateReviewStream{{Name: "greptile", Passed: true, Score: 4, ScoreMax: 5, Verdict: state.PRGateReviewVerdictOKToMerge}},
	}, now.Add(-3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RecordPRGateTransition(state.PRGateTransition{
		Project: "BeFeast/project-control-plane", IssueNumber: 19, PRNumber: 26, HeadSHA: head,
		MergeObserved: true, MergeCommitSHA: strings.Repeat("f", 40), MergedAt: now,
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(stateDir, st); err != nil {
		t.Fatal(err)
	}

	srv := NewFleet([]FleetProject{NewFleetProject("pcp", "", "", &config.Config{
		Repo: "BeFeast/project-control-plane", StateDir: stateDir, MaxParallel: 2,
	})}, "127.0.0.1", 8786, false)
	resp := srv.snapshot()
	worker := findFleetWorker(t, resp.Workers, "pcp-11")
	if worker.Status != string(state.StatusCodeLanded) || worker.NeedsAttention || worker.PRGate == nil || !worker.PRGate.Merged {
		t.Fatalf("merged worker projection = %+v", worker)
	}
	if !strings.Contains(worker.PRGate.Summary, "Greptile 4/5 · passed · PR merged") {
		t.Fatalf("merged gate summary = %q", worker.PRGate.Summary)
	}
	for _, forbidden := range []string{"not approved", "Address Greptile feedback"} {
		if strings.Contains(worker.StatusReason+" "+worker.NextAction, forbidden) {
			t.Fatalf("terminal worker retained stale review copy %q: %+v", forbidden, worker)
		}
	}
	merge := findControlAction(t, worker.Actions, "approve_merge")
	if !merge.Disabled || !strings.Contains(strings.ToLower(merge.DisabledReason), "already merged") {
		t.Fatalf("merged PR retained enabled merge CTA: %+v", merge)
	}
	project := findFleetProject(t, resp.Projects, "pcp")
	if project.OperatorState.Kind != "code_landed" || !strings.Contains(project.OperatorState.Summary, "Greptile 4/5 · passed · PR merged") {
		t.Fatalf("terminal project state = %+v", project.OperatorState)
	}
	if resp.OperatorBrief.Kind != "code_landed" || resp.OperatorBrief.ActionRequired || strings.Contains(resp.OperatorBrief.Sentence, "Approval pending") {
		t.Fatalf("terminal operator brief = %+v", resp.OperatorBrief)
	}
	if resp.NextAction != nil {
		t.Fatalf("merged PR produced an operator CTA: %+v", resp.NextAction)
	}
	if resp.Summary.ApprovalsPending != 0 || len(resp.Approvals) != 1 || !resp.Approvals[0].TargetTerminal {
		t.Fatalf("moot pending approval remained actionable: summary=%+v approvals=%+v", resp.Summary, resp.Approvals)
	}
}
