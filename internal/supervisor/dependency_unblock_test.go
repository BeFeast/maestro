package supervisor

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// fakeEnroller is a test ProjectEnroller that records every issue number it
// was asked to enroll, plus an optional error to simulate transient failures.
type fakeEnroller struct {
	enrolled []int
	err      error
}

func (f *fakeEnroller) EnrollBlockedIssue(issueNumber int) error {
	if f.err != nil {
		return f.err
	}
	f.enrolled = append(f.enrolled, issueNumber)
	return nil
}

// Helpers ---------------------------------------------------------------

func enableDependencyUnblock(cfg *config.Config) {
	enableDynamicWave(cfg)
	cfg.Supervisor.ReadyLabel = "maestro-ready"
	cfg.Supervisor.BlockedLabel = "blocked"
	cfg.Supervisor.SafeActions = []string{
		config.SupervisorActionAddReadyLabel,
		config.SupervisorActionRemoveBlockedLabel,
		config.SupervisorActionAddIssueComment,
	}
	on := true
	cfg.Supervisor.DynamicWave.DependencyUnblock.Enabled = &on
}

func blockedIssue(number int, deps []int, extraLabels ...string) github.Issue {
	labels := append([]string{"blocked"}, extraLabels...)
	issue := testIssue(number, fmt.Sprintf("Issue %d", number), labels...)
	if len(deps) > 0 {
		refs := make([]string, len(deps))
		for i, d := range deps {
			refs[i] = fmt.Sprintf("#%d", d)
		}
		issue.Body = "Depends on: " + strings.Join(refs, ", ") + "\n"
	}
	return issue
}

// Test: FindDependencies parses inline + structured shapes ------------------

func TestFindDependencies_InlineSingle(t *testing.T) {
	got := github.FindDependencies("Depends on: #147")
	want := []int{147}
	if !equalInts(got, want) {
		t.Fatalf("FindDependencies = %v, want %v", got, want)
	}
}

func TestFindDependencies_InlineMultiple(t *testing.T) {
	got := github.FindDependencies("Depends on: #148, #149")
	want := []int{148, 149}
	if !equalInts(got, want) {
		t.Fatalf("FindDependencies = %v, want %v", got, want)
	}
}

func TestFindDependencies_StructuredSection(t *testing.T) {
	body := `
## Dependencies

- #147 — design export landed
- #148 — UX issues filed

## Notes

Should not match here #999.
`
	got := github.FindDependencies(body)
	want := []int{147, 148}
	if !equalInts(got, want) {
		t.Fatalf("FindDependencies = %v, want %v", got, want)
	}
}

func TestFindDependencies_NoDuplicates(t *testing.T) {
	body := `Depends on: #147

## Dependencies

- #147 (already mentioned above)
- #148
`
	got := github.FindDependencies(body)
	want := []int{147, 148}
	if !equalInts(got, want) {
		t.Fatalf("FindDependencies = %v, want %v", got, want)
	}
}

func TestFindDependencies_NoneWhenEmpty(t *testing.T) {
	if got := github.FindDependencies(""); got != nil {
		t.Fatalf("FindDependencies(empty) = %v, want nil", got)
	}
	if got := github.FindDependencies("No deps here, just text."); got != nil {
		t.Fatalf("FindDependencies(no marker) = %v, want nil", got)
	}
}

// Test: blocked issues are NOT treated as worker candidates -----------------

func TestEvaluate_BlockedIssuesAreNotSpawnCandidates(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDependencyUnblock(cfg)
	reader := &fakeReader{
		issues: []github.Issue{
			// Blocked wave member with unresolved dep — not a worker candidate.
			blockedIssue(148, []int{147}, "maestro-ready"),
		},
	}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction == ActionSpawnWorker {
		t.Fatalf("action = %q, want NOT spawn_worker for blocked dep #148", decision.RecommendedAction)
	}
}

// Test: unblock fires when every dependency is closed ----------------------

func TestEvaluate_UnblocksWhenAllDependenciesClosed(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDependencyUnblock(cfg)
	reader := &fakeReader{
		issues:       []github.Issue{blockedIssue(148, []int{147})},
		closedIssues: map[int]bool{147: true},
	}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionUnblockIssue {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionUnblockIssue)
	}
	if decision.Target == nil || decision.Target.Issue != 148 {
		t.Fatalf("target = %#v, want issue 148", decision.Target)
	}
	if decision.Risk != RiskSafe {
		t.Fatalf("risk = %q, want safe (safe_actions cover the mutations)", decision.Risk)
	}
	hasRemoveBlocked, hasAddReady, hasComment := false, false, false
	for _, m := range decision.Mutations {
		switch m.Type {
		case MutationRemoveBlockedLabel:
			if m.Label != "blocked" {
				t.Errorf("remove_blocked label = %q, want blocked", m.Label)
			}
			hasRemoveBlocked = true
		case MutationAddReadyLabel:
			if m.Label != "maestro-ready" {
				t.Errorf("add_ready label = %q, want maestro-ready", m.Label)
			}
			hasAddReady = true
		case MutationIssueComment:
			if !strings.Contains(m.Body, "#147 closed") {
				t.Errorf("comment body = %q, want dependency evidence for #147", m.Body)
			}
			hasComment = true
		}
	}
	if !hasRemoveBlocked || !hasAddReady || !hasComment {
		t.Fatalf("mutations = %#v, want remove_blocked + add_ready + comment", decision.Mutations)
	}
}

// Test: dep merged-PR also resolves the dependency --------------------------

func TestEvaluate_UnblocksWhenDependencyPRMerged(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDependencyUnblock(cfg)
	reader := &fakeReader{
		issues:         []github.Issue{blockedIssue(149, []int{147})},
		mergedPRIssues: map[int]bool{147: true},
	}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionUnblockIssue {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionUnblockIssue)
	}
	commentSeen := false
	for _, m := range decision.Mutations {
		if m.Type == MutationIssueComment {
			commentSeen = true
			if !strings.Contains(m.Body, "#147 PR merged") {
				t.Errorf("comment body = %q, want '#147 PR merged' evidence", m.Body)
			}
		}
	}
	if !commentSeen {
		t.Fatalf("mutations = %#v, want a dependency-evidence comment", decision.Mutations)
	}
}

// Test: open dep blocks unblock --------------------------------------------

func TestEvaluate_DoesNotUnblockWhileDependencyStillOpen(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDependencyUnblock(cfg)
	// #147 stays open; supervisor must not unblock #148.
	reader := &fakeReader{issues: []github.Issue{blockedIssue(148, []int{147})}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction == ActionUnblockIssue {
		t.Fatalf("action = %q, want NOT unblock_issue while #147 is open", decision.RecommendedAction)
	}
}

// Test: max_runnable cap stops further unblocks ----------------------------

func TestEvaluate_MaxRunnableCapPreventsExtraUnblocks(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDependencyUnblock(cfg)
	cfg.Supervisor.DynamicWave.DependencyUnblock.MaxRunnable = 1

	// One issue is already runnable (carries the ready label). The next
	// blocked one with resolved deps must NOT be unblocked — cap is 1.
	already := testIssue(140, "already runnable", "maestro-ready")
	pending := blockedIssue(148, []int{147})
	reader := &fakeReader{
		issues:       []github.Issue{already, pending},
		closedIssues: map[int]bool{147: true},
	}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction == ActionUnblockIssue {
		t.Fatalf("action = %q, want NOT unblock_issue (max_runnable=1 already met)", decision.RecommendedAction)
	}
}

// Test: cap reached → enrollment still happens, but no unblock (issue #568) -

func TestEvaluate_MaxRunnableCapStillEnrollsBlockedWaveMembers(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDependencyUnblock(cfg)
	cfg.Supervisor.DynamicWave.DependencyUnblock.MaxRunnable = 1
	cfg.GitHubProjects.Enabled = true
	cfg.GitHubProjects.ProjectNumber = 7

	// Pool already at cap (one ready-labeled issue). #148 is the next blocked
	// wave member with a resolved dep — supervisor must not unblock it, but
	// must still enroll it onto the Project board so operators see the
	// upcoming wave even when the runnable pool is saturated.
	already := testIssue(140, "already runnable", "maestro-ready")
	pending := blockedIssue(148, []int{147})
	reader := &fakeReader{
		issues:       []github.Issue{already, pending},
		closedIssues: map[int]bool{147: true},
	}

	enroller := &fakeEnroller{}
	eng := testEngine(cfg, reader)
	eng.SetProjectEnroller(enroller)
	decision, err := eng.Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction == ActionUnblockIssue {
		t.Fatalf("action = %q, want NOT unblock_issue (cap reached)", decision.RecommendedAction)
	}
	if !equalInts(enroller.enrolled, []int{148}) {
		t.Fatalf("enrolled = %v, want [148] (visibility must happen even at cap)", enroller.enrolled)
	}
}

// Test: idempotency — once recorded as succeeded, no duplicate mutation ----

func TestEvaluate_DoesNotRecommendDuplicateUnblockAfterSuccess(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDependencyUnblock(cfg)
	// Issue body still says "depends on #147" (#147 closed). But supervisor
	// state already records succeeded remove_blocked + add_ready mutations
	// for this issue. The labels on the issue should also have updated, but
	// we test the idempotency guard separately: even with stale labels, a
	// prior succeeded mutation must dedupe.
	reader := &fakeReader{
		issues:       []github.Issue{blockedIssue(148, []int{147})},
		closedIssues: map[int]bool{147: true},
	}
	st := state.NewState()
	st.SupervisorDecisions = []state.SupervisorDecision{{
		Mutations: []state.SupervisorMutation{
			{Type: MutationRemoveBlockedLabel, Issue: 148, Label: "blocked", Status: MutationStatusSucceeded},
			{Type: MutationAddReadyLabel, Issue: 148, Label: "maestro-ready", Status: MutationStatusSucceeded},
		},
	}}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction == ActionUnblockIssue {
		t.Fatalf("action = %q, want NOT a duplicate unblock_issue (succeeded mutation already recorded)", decision.RecommendedAction)
	}
}

// Test: project enrollment for blocked items -------------------------------

func TestEvaluate_EnrollsBlockedWaveMembersInProject(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDependencyUnblock(cfg)
	cfg.GitHubProjects.Enabled = true
	cfg.GitHubProjects.ProjectNumber = 7

	// #147 still open → #148 is blocked but should be enrolled regardless.
	reader := &fakeReader{issues: []github.Issue{blockedIssue(148, []int{147})}}

	enroller := &fakeEnroller{}
	eng := testEngine(cfg, reader)
	eng.SetProjectEnroller(enroller)
	if _, err := eng.Decide(state.NewState()); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !equalInts(enroller.enrolled, []int{148}) {
		t.Fatalf("enrolled = %v, want [148]", enroller.enrolled)
	}
}

// Test: enrollment respects enroll_in_project=false -------------------------

func TestEvaluate_EnrollmentSkippedWhenDisabled(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDependencyUnblock(cfg)
	cfg.GitHubProjects.Enabled = true
	cfg.GitHubProjects.ProjectNumber = 7
	off := false
	cfg.Supervisor.DynamicWave.DependencyUnblock.EnrollInProject = &off

	reader := &fakeReader{issues: []github.Issue{blockedIssue(148, []int{147})}}
	enroller := &fakeEnroller{}
	eng := testEngine(cfg, reader)
	eng.SetProjectEnroller(enroller)
	if _, err := eng.Decide(state.NewState()); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(enroller.enrolled) != 0 {
		t.Fatalf("enrolled = %v, want none (enroll_in_project=false)", enroller.enrolled)
	}
}

// Test: enrollment failure does not crash the supervisor cycle --------------

func TestEvaluate_EnrollmentFailureIsBestEffort(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDependencyUnblock(cfg)
	cfg.GitHubProjects.Enabled = true
	cfg.GitHubProjects.ProjectNumber = 7

	reader := &fakeReader{issues: []github.Issue{blockedIssue(148, []int{147})}}
	enroller := &fakeEnroller{err: errors.New("graphql rate limited")}
	eng := testEngine(cfg, reader)
	eng.SetProjectEnroller(enroller)
	if _, err := eng.Decide(state.NewState()); err != nil {
		t.Fatalf("Decide failed despite best-effort enroller error: %v", err)
	}
}

// Test: blocked issue without parseable deps is skipped silently -----------

func TestEvaluate_BlockedIssueWithoutDepsIsLeftAlone(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDependencyUnblock(cfg)

	// Body without "Depends on:" — operator-controlled, do not guess.
	noDeps := testIssue(148, "manually blocked", "blocked")
	noDeps.Body = "Free-form notes, no dependency marker."
	reader := &fakeReader{issues: []github.Issue{noDeps}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction == ActionUnblockIssue {
		t.Fatalf("action = %q, want NOT unblock_issue (no parseable deps)", decision.RecommendedAction)
	}
}

// Test: controller inactive when feature flag is off ------------------------

func TestEvaluate_InactiveWithoutFeatureFlag(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDynamicWave(cfg) // No DependencyUnblock.Enabled set.
	cfg.Supervisor.ReadyLabel = "maestro-ready"
	cfg.Supervisor.BlockedLabel = "blocked"

	reader := &fakeReader{
		issues:       []github.Issue{blockedIssue(148, []int{147})},
		closedIssues: map[int]bool{147: true},
	}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction == ActionUnblockIssue {
		t.Fatalf("action = %q, want NOT unblock_issue (feature flag off)", decision.RecommendedAction)
	}
}

// Test: end-to-end through RunOnce executes the planned mutations ----------

func TestRunOnce_DependencyUnblockExecutesMutations(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	enableDependencyUnblock(cfg)
	reader := &fakeReader{
		issues:       []github.Issue{blockedIssue(148, []int{147})},
		closedIssues: map[int]bool{147: true},
	}

	decision, err := RunOnce(cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if decision.Status != DecisionStatusSucceeded {
		t.Fatalf("status = %q (summary=%q), want %q", decision.Status, decision.Summary, DecisionStatusSucceeded)
	}
	if got := reader.removedLabels; len(got) != 1 || got[0] != "#148:blocked" {
		t.Fatalf("removed labels = %v, want [#148:blocked]", got)
	}
	if got := reader.addedLabels; len(got) != 1 || got[0] != "#148:maestro-ready" {
		t.Fatalf("added labels = %v, want [#148:maestro-ready]", got)
	}
	if len(reader.comments) != 1 || !strings.Contains(reader.comments[0], "#147 closed") {
		t.Fatalf("comments = %#v, want one comment with #147 closed evidence", reader.comments)
	}

	// Second cycle must be a no-op (idempotency).
	if _, err := RunOnce(cfg, reader); err != nil {
		t.Fatalf("RunOnce(2): %v", err)
	}
	if len(reader.removedLabels) != 1 || len(reader.addedLabels) != 1 || len(reader.comments) != 1 {
		t.Fatalf("second RunOnce produced duplicates: removed=%v added=%v comments=%v", reader.removedLabels, reader.addedLabels, reader.comments)
	}
}

// Test: default allowed actions include unblock_issue ----------------------

func TestDefaultAllowedActions_IncludesUnblockIssue(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range defaultAllowedActions() {
		seen[a] = true
	}
	if !seen[ActionUnblockIssue] {
		t.Errorf("defaultAllowedActions missing %q", ActionUnblockIssue)
	}
}

func TestCanonicalAction_AliasesForUnblockIssue(t *testing.T) {
	for _, in := range []string{"unblock_issue", "unblock", "remove_blocked_label_and_label_ready"} {
		if got := canonicalAction(in); got != ActionUnblockIssue {
			t.Errorf("canonicalAction(%q) = %q, want %q", in, got, ActionUnblockIssue)
		}
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
