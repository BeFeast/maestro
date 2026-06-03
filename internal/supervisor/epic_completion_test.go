package supervisor

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
)

func TestComputeEpicProgress_PartialMergedInProgress(t *testing.T) {
	epic := github.Issue{
		Number: 500,
		Title:  "Epic: Scribe redesign",
		Body: `# Epic
## Issue Wave
- [ ] #501 — slice A
- [ ] #502 — slice B
- [ ] #503 — slice C
`,
	}
	resolver := func(n int) (bool, bool) {
		// only #501 is merged so far
		return n == 501, false
	}
	status := outcome.Status{HealthState: outcome.HealthHealthy}

	got := computeEpicProgress([]github.Issue{epic}, resolver, status)
	if len(got) != 1 {
		t.Fatalf("len(progress) = %d, want 1", len(got))
	}
	p := got[0]
	if p.TotalChildren != 3 || p.MergedCount != 1 || p.OpenCount != 2 {
		t.Fatalf("counts = total %d merged %d open %d, want 3/1/2", p.TotalChildren, p.MergedCount, p.OpenCount)
	}
	if p.AllChildrenDone || p.Complete {
		t.Fatalf("AllChildrenDone=%v Complete=%v, want false", p.AllChildrenDone, p.Complete)
	}
	if !strings.Contains(p.Summary, "1/3") {
		t.Fatalf("summary = %q, want it to mention 1/3", p.Summary)
	}
}

func TestComputeEpicProgress_AllMergedAndHealthyIsComplete(t *testing.T) {
	epic := github.Issue{
		Number: 600,
		Title:  "Epic: Migrate auth",
		Body: `## Children
- #601
- #602
`,
	}
	resolver := func(n int) (bool, bool) { return true, false } // all merged
	status := outcome.Status{HealthState: outcome.HealthHealthy}

	got := computeEpicProgress([]github.Issue{epic}, resolver, status)
	if len(got) != 1 || !got[0].Complete {
		t.Fatalf("progress = %#v, want one complete epic", got)
	}
	if got[0].MergedCount != 2 || got[0].OpenCount != 0 {
		t.Fatalf("counts = merged %d open %d, want 2/0", got[0].MergedCount, got[0].OpenCount)
	}
	if !strings.Contains(got[0].Summary, "complete") {
		t.Fatalf("summary = %q, want it to mention complete", got[0].Summary)
	}
}

func TestComputeEpicProgress_AllMergedButOutcomeFailingHoldsCompletion(t *testing.T) {
	epic := github.Issue{
		Number: 700,
		Title:  "Epic: ship",
		Body:   "## Children\n- #701\n- #702\n",
	}
	resolver := func(n int) (bool, bool) { return true, false }
	status := outcome.Status{HealthState: outcome.HealthFailing}

	got := computeEpicProgress([]github.Issue{epic}, resolver, status)
	if len(got) != 1 {
		t.Fatalf("len(progress) = %d, want 1", len(got))
	}
	p := got[0]
	if !p.AllChildrenDone {
		t.Fatalf("AllChildrenDone = false, want true")
	}
	if p.Complete {
		t.Fatalf("Complete = true, want false — outcome failing must hold completion")
	}
	if !strings.Contains(p.Summary, "outcome health is failing") {
		t.Fatalf("summary = %q, want it to flag failing outcome", p.Summary)
	}
}

func TestComputeEpicProgress_SkipsEpicsWithoutParseableChildren(t *testing.T) {
	epic := github.Issue{Number: 800, Title: "Epic: empty", Body: "no children referenced here"}
	got := computeEpicProgress([]github.Issue{epic},
		func(int) (bool, bool) { return false, false },
		outcome.Status{HealthState: outcome.HealthHealthy})
	if got != nil {
		t.Fatalf("progress = %#v, want nil for label-only epic with empty body", got)
	}
}

func TestFirstCompletedEpic_PicksLowestNumber(t *testing.T) {
	progresses := []state.EpicProgress{
		{Number: 200, Complete: false},
		{Number: 201, Complete: true},
		{Number: 202, Complete: true},
	}
	got := firstCompletedEpic(progresses)
	if got == nil || got.Number != 201 {
		t.Fatalf("firstCompletedEpic = %#v, want 201", got)
	}
}

func TestFirstCompletedEpic_NoneComplete(t *testing.T) {
	progresses := []state.EpicProgress{
		{Number: 200, Complete: false},
		{Number: 201, Complete: false},
	}
	if got := firstCompletedEpic(progresses); got != nil {
		t.Fatalf("firstCompletedEpic = %#v, want nil", got)
	}
}

// TestDecide_EpicCompletionMintsApprovalGatedClose verifies the full path:
// an open epic whose children are all merged AND a healthy outcome
// signal produces an approval-gated close_issue recommendation tagged
// PolicyRuleEpicCompletion. Never auto-closes (#650).
func TestDecide_EpicCompletionMintsApprovalGatedClose(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableHandoffPlanner(cfg)
	cfg.Outcome = outcome.Brief{
		DesiredOutcome:      "Live app works",
		HealthcheckCommand:  "check-live",
		PassRequiredForDone: boolPtr(true),
	}

	epic := github.Issue{
		Number: 900,
		Title:  "Epic: Scribe redesign",
		Body: `## Children
- #901 — slice A
- #902 — slice B
`,
	}
	for _, label := range []string{"epic"} {
		epic.Labels = append(epic.Labels, struct {
			Name string `json:"name"`
		}{Name: label})
	}

	reader := &fakeReader{
		issues: []github.Issue{epic},
		mergedPRIssues: map[int]bool{
			901: true,
			902: true,
		},
	}
	st := state.NewState()
	checkedAt := testEngineNow()
	st.OutcomeHealth = &outcome.HealthCheckResult{
		CheckedAt: checkedAt,
		State:     outcome.HealthHealthy,
		Signal:    "healthcheck_command",
		Summary:   "live verifier passed",
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if decision.RecommendedAction != config.SupervisorActionCloseIssue {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, config.SupervisorActionCloseIssue)
	}
	if decision.Risk != RiskApprovalGated {
		t.Fatalf("risk = %q, want approval_gated", decision.Risk)
	}
	if !decision.RequiresApproval {
		t.Fatalf("RequiresApproval = false, want true — epic close MUST be approval-gated")
	}
	if decision.Target == nil || decision.Target.Issue != epic.Number {
		t.Fatalf("target = %#v, want epic #%d", decision.Target, epic.Number)
	}
	if decision.PolicyRule != PolicyRuleEpicCompletion {
		t.Fatalf("policy_rule = %q, want %q", decision.PolicyRule, PolicyRuleEpicCompletion)
	}
	if len(decision.Epics) != 1 || !decision.Epics[0].Complete {
		t.Fatalf("decision.Epics = %#v, want one complete epic", decision.Epics)
	}
}

// TestDecide_EpicProgressStampedEvenWhenNotComplete verifies the snapshot
// is recorded on every dynamic-wave decision so the fleet UI can render
// "epic in progress" without re-listing issues.
func TestDecide_EpicProgressStampedEvenWhenNotComplete(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableHandoffPlanner(cfg)

	epic := github.Issue{
		Number: 910,
		Title:  "Epic: in flight",
		Body:   "## Children\n- #911\n- #912\n",
	}
	epic.Labels = append(epic.Labels, struct {
		Name string `json:"name"`
	}{Name: "epic"})

	reader := &fakeReader{
		issues:         []github.Issue{epic},
		mergedPRIssues: map[int]bool{911: true},
	}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(decision.Epics) != 1 {
		t.Fatalf("decision.Epics = %#v, want one entry", decision.Epics)
	}
	got := decision.Epics[0]
	if got.Number != 910 || got.MergedCount != 1 || got.OpenCount != 1 || got.Complete {
		t.Fatalf("epic progress = %#v, want #910 1/2 not complete", got)
	}
	// The decision itself must NOT be a close_issue verb when children
	// remain open — the close-epic branch only fires when Complete=true.
	if decision.RecommendedAction == config.SupervisorActionCloseIssue {
		t.Fatalf("action = %q, want NOT close_issue while children remain open", decision.RecommendedAction)
	}
}

// TestDecide_EpicAllChildrenDoneButOutcomeFailingHoldsClose verifies the
// safety gate: even when every child is merged, an unhealthy outcome
// signal blocks the close-epic recommendation. The supervisor falls
// through to the regular handoff-planner / none branch instead.
func TestDecide_EpicAllChildrenDoneButOutcomeFailingHoldsClose(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableHandoffPlanner(cfg)
	cfg.Outcome = outcome.Brief{
		DesiredOutcome:      "Live app works",
		HealthcheckCommand:  "check-live",
		PassRequiredForDone: boolPtr(true),
	}

	epic := github.Issue{
		Number: 920,
		Title:  "Epic: ship",
		Body:   "## Children\n- #921\n",
	}
	epic.Labels = append(epic.Labels, struct {
		Name string `json:"name"`
	}{Name: "epic"})

	reader := &fakeReader{
		issues:         []github.Issue{epic},
		mergedPRIssues: map[int]bool{921: true},
	}
	st := state.NewState()
	checkedAt := testEngineNow()
	st.OutcomeHealth = &outcome.HealthCheckResult{
		CheckedAt: checkedAt,
		State:     outcome.HealthFailing,
		Signal:    "healthcheck_command",
		Summary:   "runtime check failed",
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction == config.SupervisorActionCloseIssue {
		t.Fatalf("action = %q, want NOT close_issue while outcome is failing", decision.RecommendedAction)
	}
	// The failing outcome can short-circuit decideDeterministic into the
	// outcome-health branch BEFORE the dynamic wave stamps decision.Epics;
	// that is fine. The behavior under test is that the cautious gate
	// never lets close_issue fire while the runtime is unhealthy. The
	// computed aggregate is verified directly here so the unit covers
	// both the gate AND the Complete=false semantics.
	eng := testEngine(cfg, reader)
	progresses := eng.epicProgressForIssues([]github.Issue{epic},
		newResolutionCache(reader),
		eng.outcomeStatus(st))
	if len(progresses) != 1 {
		t.Fatalf("epicProgressForIssues = %#v, want one entry", progresses)
	}
	if !progresses[0].AllChildrenDone {
		t.Fatalf("AllChildrenDone = false, want true")
	}
	if progresses[0].Complete {
		t.Fatalf("Complete = true, want false — outcome failing must hold completion")
	}
}

func testEngineNow() time.Time {
	return time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
}

func sortInts(in []int) []int {
	out := append([]int(nil), in...)
	sort.Ints(out)
	return out
}

func TestEpicEvidence_ListsMergedAndOpen(t *testing.T) {
	p := state.EpicProgress{
		Number:         10,
		TotalChildren:  3,
		MergedCount:    1,
		OpenCount:      2,
		MergedChildren: []int{11},
		OpenChildren:   []int{12, 13},
		OutcomeHealth:  outcome.HealthHealthy,
	}
	ev := epicEvidence(p)
	joined := strings.Join(ev, " ")
	if !strings.Contains(joined, "children_total=3") {
		t.Errorf("evidence missing total: %v", ev)
	}
	if !strings.Contains(joined, "children_merged=1") {
		t.Errorf("evidence missing merged: %v", ev)
	}
	if !strings.Contains(joined, "outcome_health=healthy") {
		t.Errorf("evidence missing outcome: %v", ev)
	}
	if !strings.Contains(joined, "merged=#11") {
		t.Errorf("evidence missing merged list: %v", ev)
	}
	if !reflect.DeepEqual(sortInts(p.OpenChildren), []int{12, 13}) {
		t.Errorf("open children = %v, want [12 13]", p.OpenChildren)
	}
}
