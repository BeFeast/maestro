package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// #425 (sup-98): when an open PR is fully ready to merge but the project
// policy lists merge_pr in approval_required, the supervisor must
// recommend merge_pr (so the cautious gate mints an approval) AND attach
// a policy_blocks_merge stuck state so the dashboard/CLI can show the
// operator that a human click is the missing link. Before the fix the
// supervisor either looped on monitor_open_pr or showed a passive
// "monitoring PR" tile with no indication that approval was required.
func TestDecide_OpenPRGreen_PolicyBlocksMerge_SurfacesStuckState(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	cfg.Supervisor.ApprovalRequired = []string{
		config.SupervisorActionMergePR,
		config.SupervisorActionCloseIssue,
		config.SupervisorActionDeleteWorktree,
	}
	reader := &fakeReader{
		prs:        []github.PR{{Number: 115, HeadRefName: "feat/auth-wave", Mergeable: "MERGEABLE"}},
		ciStatuses: map[int]string{115: "success"},
	}
	st := state.NewState()
	st.Sessions["scribe-1"] = &state.Session{
		IssueNumber: 200,
		IssueTitle:  "auth wave PR",
		Status:      state.StatusPROpen,
		Branch:      "feat/auth-wave",
		PRNumber:    115,
		StartedAt:   time.Now().UTC().Add(-time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionMergePR {
		t.Fatalf("action = %q, want merge_pr (ready PR, cautious gate mints approval)", decision.RecommendedAction)
	}
	stuck := requireStuckState(t, decision, state.StuckPolicyBlocksMerge)
	if stuck.Severity != SeverityWarning {
		t.Errorf("severity = %q, want warning", stuck.Severity)
	}
	if stuck.Target == nil || stuck.Target.PR != 115 {
		t.Errorf("target = %#v, want PR=115", stuck.Target)
	}
	if !strings.Contains(stuck.Summary, "merge_pr") {
		t.Errorf("summary = %q, want it to name the gated verb (merge_pr)", stuck.Summary)
	}
	// close_issue is also approval-required and should appear in the same
	// stuck state so the operator sees the full gate set in one place.
	if !strings.Contains(stuck.Summary, "close_issue") {
		t.Errorf("summary = %q, want it to also name close_issue", stuck.Summary)
	}
	// delete_worktree is approval-required but does NOT block the
	// green-PR completion path, so it must not surface here.
	if strings.Contains(stuck.Summary, "delete_worktree") {
		t.Errorf("summary = %q, must not name delete_worktree (not a merge-gating verb)", stuck.Summary)
	}
}

// When merge_pr is NOT in approval_required, the supervisor still emits
// merge_pr but does NOT attach a policy_blocks_merge stuck state — the
// gate is policy-permissive and the executor will merge on the next tick.
func TestDecide_OpenPRGreen_AutomergeAllowed_NoPolicyStuckState(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	// Only delete_worktree gated — does not block merge.
	cfg.Supervisor.ApprovalRequired = []string{
		config.SupervisorActionDeleteWorktree,
	}
	reader := &fakeReader{
		prs:        []github.PR{{Number: 116, HeadRefName: "feat/auto", Mergeable: "MERGEABLE"}},
		ciStatuses: map[int]string{116: "success"},
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 201,
		Status:      state.StatusPROpen,
		Branch:      "feat/auto",
		PRNumber:    116,
		StartedAt:   time.Now().UTC().Add(-30 * time.Minute),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != ActionMergePR {
		t.Fatalf("action = %q, want merge_pr", decision.RecommendedAction)
	}
	for _, stuck := range decision.StuckStates {
		if stuck.Code == state.StuckPolicyBlocksMerge {
			t.Fatalf("policy_blocks_merge must NOT be present when merge_pr is not approval-required: %#v", stuck)
		}
	}
}

// pending_checks must be attached when the supervisor stays on
// monitor_open_pr so the dashboard can render the specific blocker
// (CI pending, mergeable unknown, draft, ...) instead of a generic tile.
func TestDecide_OpenPR_PendingCI_AttachesPendingChecksStuckState(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	reader := &fakeReader{
		prs:        []github.PR{{Number: 117, HeadRefName: "feat/pending", Mergeable: "MERGEABLE"}},
		ciStatuses: map[int]string{117: "pending"},
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 202,
		Status:      state.StatusPROpen,
		Branch:      "feat/pending",
		PRNumber:    117,
		StartedAt:   time.Now().UTC().Add(-10 * time.Minute),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionMonitorOpenPR {
		t.Fatalf("action = %q, want monitor_open_pr", decision.RecommendedAction)
	}
	stuck := requireStuckState(t, decision, state.StuckPendingChecks)
	if stuck.Target == nil || stuck.Target.PR != 117 {
		t.Errorf("target = %#v, want PR=117", stuck.Target)
	}
	if stuck.Severity != SeverityInfo {
		t.Errorf("severity = %q, want info", stuck.Severity)
	}
	if !strings.Contains(strings.ToLower(stuck.Summary), "ci status=pending") {
		t.Errorf("summary = %q, want it to name CI pending", stuck.Summary)
	}
}

// #425 evidence loop: PRCIStatus="pending" because a legacy commit
// status hangs, but the per-PR mergeable_state is "clean" (all required
// checks passed). Supervisor must trust the GitHub-computed
// mergeable_state and recommend merge_pr instead of looping on
// monitor_open_pr.
func TestDecide_OpenPR_CIAggregateStaleButMergeStateClean_Merges(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	reader := &fakeReader{
		prs:         []github.PR{{Number: 115, HeadRefName: "feat/auth", Mergeable: "MERGEABLE"}},
		ciStatuses:  map[int]string{115: "pending"},
		mergeStates: map[int]string{115: "clean"},
		checkRollups: map[int]github.PRCheckRollup{
			115: {
				Verdict:  "pending",
				Complete: true,
				Signals: []github.PRCheckSignal{
					{Source: "check_run", Name: "Fedora 43", Status: "completed", Conclusion: "success"},
					{Source: "commit_status", Name: "legacy/review", Status: "pending"},
				},
			},
		},
	}
	st := state.NewState()
	st.Sessions["scribe-1"] = &state.Session{
		IssueNumber: 200,
		Status:      state.StatusPROpen,
		Branch:      "feat/auth",
		PRNumber:    115,
		StartedAt:   time.Now().UTC().Add(-time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionMergePR {
		t.Fatalf("action = %q, want merge_pr (mergeable_state=clean must override stale aggregate CI=pending)", decision.RecommendedAction)
	}
	if decision.Target == nil || decision.Target.HeadSHA == "" {
		t.Fatalf("target = %#v, want merge approval bound to checked head SHA", decision.Target)
	}
}

func TestDecide_OpenPR420_RealPendingCheckRunBlocksCleanMergeState(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	reader := &fakeReader{
		prs:         []github.PR{{Number: 420, HeadRefName: "ok-player/fedora", Mergeable: "MERGEABLE"}},
		ciStatuses:  map[int]string{420: "pending"},
		mergeStates: map[int]string{420: "clean"},
		checkRollups: map[int]github.PRCheckRollup{
			420: {
				HeadSHA:          strings.Repeat("a", 40),
				Verdict:          "pending",
				Complete:         true,
				PendingCheckRuns: true,
				Signals: []github.PRCheckSignal{
					{Source: "check_run", Name: "Fedora 43", Status: "in_progress"},
					{Source: "check_run", Name: "Fedora 44", Status: "queued"},
				},
			},
		},
	}
	st := state.NewState()
	st.Sessions["ok-player-1"] = &state.Session{
		IssueNumber: 201,
		Status:      state.StatusPROpen,
		Branch:      "ok-player/fedora",
		PRNumber:    420,
		StartedAt:   time.Now().UTC().Add(-time.Minute),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionMonitorOpenPR {
		t.Fatalf("action = %q, want monitor_open_pr while current-head check-runs are pending", decision.RecommendedAction)
	}
}

func TestDecide_OpenPR_LegacyPendingDoesNotMaskTerminalFailedCheckRun(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	reader := &fakeReader{
		prs:         []github.PR{{Number: 123, HeadRefName: "feat/failed-check", Mergeable: "MERGEABLE"}},
		mergeStates: map[int]string{123: "clean"},
		checkRollups: map[int]github.PRCheckRollup{
			123: {
				Verdict:  "pending",
				Complete: true,
				Signals: []github.PRCheckSignal{
					{Source: "check_run", Name: "build", Status: "completed", Conclusion: "failure"},
					{Source: "commit_status", Name: "legacy/review", Status: "pending"},
				},
			},
		},
	}
	st := state.NewState()
	st.Sessions["failed-check-1"] = &state.Session{
		IssueNumber: 206,
		Status:      state.StatusPROpen,
		Branch:      "feat/failed-check",
		PRNumber:    123,
		StartedAt:   time.Now().UTC().Add(-time.Minute),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionMonitorOpenPR {
		t.Fatalf("action = %q, want monitor_open_pr when a terminal check-run failed", decision.RecommendedAction)
	}
}

func TestDecide_OpenPR_PendingRollupWithoutLegacyPendingSignalBlocksCleanMergeState(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	reader := &fakeReader{
		prs:         []github.PR{{Number: 124, HeadRefName: "feat/unproven-pending", Mergeable: "MERGEABLE"}},
		mergeStates: map[int]string{124: "clean"},
		checkRollups: map[int]github.PRCheckRollup{
			124: {
				Verdict:  "pending",
				Complete: true,
				Signals: []github.PRCheckSignal{
					{Source: "check_run", Name: "build", Status: "completed", Conclusion: "success"},
				},
			},
		},
	}
	st := state.NewState()
	st.Sessions["unproven-pending-1"] = &state.Session{
		IssueNumber: 207,
		Status:      state.StatusPROpen,
		Branch:      "feat/unproven-pending",
		PRNumber:    124,
		StartedAt:   time.Now().UTC().Add(-time.Minute),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionMonitorOpenPR {
		t.Fatalf("action = %q, want monitor_open_pr without a legacy pending status signal", decision.RecommendedAction)
	}
}

type aggregateOnlyReader struct {
	Reader
	ciStatus   string
	mergeState string
}

func (r *aggregateOnlyReader) PRCIStatus(int) (string, error) {
	return r.ciStatus, nil
}

func (r *aggregateOnlyReader) PRMergeStatus(int) (string, string, error) {
	return "MERGEABLE", r.mergeState, nil
}

func TestDecide_OpenPR_CheckRollupUnavailableBlocksCleanMergeState(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	base := &fakeReader{
		prs: []github.PR{{Number: 121, HeadRefName: "feat/no-rollup", Mergeable: "MERGEABLE"}},
	}
	reader := &aggregateOnlyReader{
		Reader:     base,
		ciStatus:   "pending",
		mergeState: "clean",
	}
	st := state.NewState()
	st.Sessions["ok-player-3"] = &state.Session{
		IssueNumber: 204,
		Status:      state.StatusPROpen,
		Branch:      "feat/no-rollup",
		PRNumber:    121,
		StartedAt:   time.Now().UTC().Add(-time.Minute),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionMonitorOpenPR {
		t.Fatalf("action = %q, want monitor_open_pr when current-head check rollup is unavailable", decision.RecommendedAction)
	}
}

func TestDecide_OpenPR_PartialCheckRollupBlocksCleanMergeState(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	reader := &fakeReader{
		prs:         []github.PR{{Number: 122, HeadRefName: "feat/partial-rollup", Mergeable: "MERGEABLE"}},
		mergeStates: map[int]string{122: "clean"},
		checkRollups: map[int]github.PRCheckRollup{
			122: {Verdict: "pending", Complete: false},
		},
	}
	st := state.NewState()
	st.Sessions["ok-player-4"] = &state.Session{
		IssueNumber: 205,
		Status:      state.StatusPROpen,
		Branch:      "feat/partial-rollup",
		PRNumber:    122,
		StartedAt:   time.Now().UTC().Add(-time.Minute),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionMonitorOpenPR {
		t.Fatalf("action = %q, want monitor_open_pr when current-head check rollup is partial", decision.RecommendedAction)
	}
}

type headChangingReader struct {
	*fakeReader
	headSHA string
	headErr error
}

func (r *headChangingReader) PRHeadSHA(int) (string, error) {
	return r.headSHA, r.headErr
}

func TestDecide_OpenPR_HeadChangedAfterCheckRollup_StaysMonitor(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	checkedHead := strings.Repeat("a", 40)
	reader := &headChangingReader{
		fakeReader: &fakeReader{
			prs:         []github.PR{{Number: 120, HeadRefName: "feat/force-pushed", Mergeable: "MERGEABLE"}},
			mergeStates: map[int]string{120: "clean"},
			checkRollups: map[int]github.PRCheckRollup{
				120: {HeadSHA: checkedHead, Verdict: "success", Complete: true},
			},
		},
		headSHA: strings.Repeat("b", 40),
	}
	st := state.NewState()
	st.Sessions["ok-player-2"] = &state.Session{
		IssueNumber: 202,
		Status:      state.StatusPROpen,
		Branch:      "feat/force-pushed",
		PRNumber:    120,
		StartedAt:   time.Now().UTC().Add(-time.Minute),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionMonitorOpenPR {
		t.Fatalf("action = %q, want monitor_open_pr after PR head changed", decision.RecommendedAction)
	}
}

// mergeable_state="blocked" means a required check is still failing;
// stay on monitor_open_pr even if a legacy commit status would otherwise
// have been pending-but-not-failure.
func TestDecide_OpenPR_CIPendingAndMergeStateBlocked_StaysMonitor(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	reader := &fakeReader{
		prs:         []github.PR{{Number: 118, HeadRefName: "feat/blocked", Mergeable: "MERGEABLE"}},
		ciStatuses:  map[int]string{118: "pending"},
		mergeStates: map[int]string{118: "blocked"},
	}
	st := state.NewState()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 203,
		Status:      state.StatusPROpen,
		Branch:      "feat/blocked",
		PRNumber:    118,
		StartedAt:   time.Now().UTC().Add(-10 * time.Minute),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionMonitorOpenPR {
		t.Fatalf("action = %q, want monitor_open_pr (mergeable_state=blocked must not override pending CI)", decision.RecommendedAction)
	}
}

func TestMergeGatedApprovalVerbs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "empty", in: nil, want: nil},
		{name: "only merge", in: []string{"merge_pr"}, want: []string{"merge_pr"}},
		{name: "merge and close, dedup", in: []string{"merge_pr", "close_issue", "merge_pr"}, want: []string{"close_issue", "merge_pr"}},
		{name: "non-merge verbs ignored", in: []string{"delete_worktree", "change_global_config"}, want: nil},
		{name: "mixed", in: []string{"merge_pr", "delete_worktree", "close_issue"}, want: []string{"close_issue", "merge_pr"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeGatedApprovalVerbs(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
