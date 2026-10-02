package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// Regression coverage for #1240: the deterministic supervisor promoted an
// unrelated P2 epic to the ready label under the default queue policy and the
// orchestrator started a worker for it.

// pilotPromotionConfig mirrors the incident project: supervisor LLM disabled,
// no dynamic wave, an empty disabled ordered queue, one ready label and
// add_ready_label whitelisted as a safe action.
func pilotPromotionConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.MaxParallel = 2
	cfg.IssueLabels = []string{"pilot-ready"}
	cfg.ExcludeLabels = []string{"epic"}
	cfg.Supervisor.Enabled = false
	disabled := false
	cfg.Supervisor.DynamicWave.Enabled = &disabled
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{Enabled: false}
	cfg.Supervisor.SafeActions = []string{
		config.SupervisorActionAddReadyLabel,
		config.SupervisorActionRemoveReadyLabel,
	}
	return cfg
}

func incidentEpicIssue() github.Issue {
	return testIssue(7, "[P2] Web redesign: BeFeast restyle of the SPA (epic)")
}

// pilotStateWithOpenPR models the running pilot: ready issue #3 already has a
// worker session whose PR is open, so the supervise cycle looks for backlog to
// fill the spare slot.
func pilotStateWithOpenPR() (*state.State, *fakeReader) {
	st := state.NewState()
	st.Sessions["pilot-1"] = &state.Session{
		IssueNumber: 3,
		Status:      state.StatusPROpen,
		Branch:      "fix/3-pilot",
		PRNumber:    30,
		StartedAt:   time.Date(2026, 4, 29, 11, 0, 0, 0, time.UTC),
	}
	reader := &fakeReader{
		issues: []github.Issue{
			testIssue(3, "Pilot repair", "pilot-ready"),
			incidentEpicIssue(),
			testIssue(8, "Unrelated cleanup"),
		},
		prs:          []github.PR{{Number: 30, HeadRefName: "fix/3-pilot", State: "OPEN"}},
		openPRIssues: map[int]bool{3: true},
	}
	return st, reader
}

func requireNoAddReadyMutation(t *testing.T, decision state.SupervisorDecision) {
	t.Helper()
	for _, mutation := range decision.Mutations {
		if mutation.Type == MutationAddReadyLabel {
			t.Fatalf("decision planned %s for issue #%d; want no ready-label promotion (decision %s: %s)", mutation.Type, mutation.Issue, decision.RecommendedAction, decision.Summary)
		}
	}
}

func requireReasonContains(t *testing.T, decision state.SupervisorDecision, want string) {
	t.Helper()
	for _, reason := range decision.Reasons {
		if strings.Contains(reason, want) {
			return
		}
	}
	t.Fatalf("decision reasons %q do not mention %q", decision.Reasons, want)
}

func TestRunOnce_DisabledSupervisorDefaultPolicyDoesNotPromoteIssues(t *testing.T) {
	cfg := pilotPromotionConfig(t)
	st, reader := pilotStateWithOpenPR()
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	decision, err := RunOnce(context.Background(), cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(reader.addedLabels) != 0 {
		t.Fatalf("added labels = %q, want none: the default policy must not promote issues unless supervisor.auto_promote_ready is set", reader.addedLabels)
	}
	requireNoAddReadyMutation(t, decision)
	if decision.RecommendedAction == ActionLabelIssueReady {
		t.Fatalf("action = %q, want no label recommendation", decision.RecommendedAction)
	}
}

func TestDecide_DisabledSupervisorNeverPromotesEpicOrUnlabeledIssue(t *testing.T) {
	cfg := pilotPromotionConfig(t)
	reader := &fakeReader{issues: []github.Issue{incidentEpicIssue(), testIssue(8, "Unrelated cleanup")}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	requireNoAddReadyMutation(t, decision)
	if decision.RecommendedAction == ActionLabelIssueReady {
		t.Fatalf("action = %q target = %#v, want no label recommendation", decision.RecommendedAction, decision.Target)
	}
	requireReasonContains(t, decision, "Ready-label promotion withheld for issue #7")
}

func TestRunOnce_AutoPromoteReadyLabelsNormalIssue(t *testing.T) {
	cfg := pilotPromotionConfig(t)
	cfg.Supervisor.AutoPromoteReady = true
	reader := &fakeReader{issues: []github.Issue{testIssue(8, "Unrelated cleanup")}}

	decision, err := RunOnce(context.Background(), cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if decision.RecommendedAction != ActionLabelIssueReady {
		t.Fatalf("action = %q, want %q", decision.RecommendedAction, ActionLabelIssueReady)
	}
	if got, want := strings.Join(reader.addedLabels, ","), "#8:pilot-ready"; got != want {
		t.Fatalf("added labels = %q, want %q", got, want)
	}
}

func TestDecide_AutoPromoteReadySkipsEpicTitlesAndPromotesNextIssue(t *testing.T) {
	for _, title := range []string{
		"[P2] Web redesign: BeFeast restyle of the SPA (epic)",
		"[Epic] Billing overhaul",
		"Epic: onboarding flow",
		"[P1] EPIC: search rewrite",
	} {
		t.Run(title, func(t *testing.T) {
			cfg := pilotPromotionConfig(t)
			cfg.Supervisor.AutoPromoteReady = true
			reader := &fakeReader{issues: []github.Issue{testIssue(7, title), testIssue(8, "Unrelated cleanup")}}

			decision, err := testEngine(cfg, reader).Decide(state.NewState())
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if decision.RecommendedAction != ActionLabelIssueReady || decision.Target == nil || decision.Target.Issue != 8 {
				t.Fatalf("action = %q target = %#v, want label_issue_ready for #8 after skipping the epic", decision.RecommendedAction, decision.Target)
			}
			for _, mutation := range decision.Mutations {
				if mutation.Issue == 7 {
					t.Fatalf("mutations = %#v, want none for epic #7", decision.Mutations)
				}
			}
			requireReasonContains(t, decision, "Ready-label promotion withheld for issue #7: issue title marks it as an epic/parent issue")
		})
	}
}

func TestDecide_OrderedQueueNeverPromotesEpicHead(t *testing.T) {
	cfg := pilotPromotionConfig(t)
	cfg.Supervisor.OrderedQueue = config.SupervisorOrderedQueueConfig{Enabled: true, Issues: []int{7}}
	reader := &fakeReader{issues: []github.Issue{incidentEpicIssue()}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	requireNoAddReadyMutation(t, decision)
	requireReasonContains(t, decision, "Ready-label promotion withheld for issue #7")
}

// An exclude label is never lifted by promotion. With remove_blocked_label not
// whitelisted the blocked label stays on the issue, so the ready label must
// not be added next to it.
func TestDecide_ExcludedLabelNeverPromoted(t *testing.T) {
	cfg := pilotPromotionConfig(t)
	cfg.Supervisor.AutoPromoteReady = true
	cfg.ExcludeLabels = []string{"epic", "blocked"}
	reader := &fakeReader{issues: []github.Issue{testIssue(9, "Waiting on vendor", "blocked")}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	requireNoAddReadyMutation(t, decision)
	// A mutation-less label_issue_ready decision would mint an approval whose
	// execution adds the ready label to #9 (review round 1).
	if decision.RecommendedAction == ActionLabelIssueReady {
		t.Fatalf("action = %q target = %#v mutations = %#v, want no label_issue_ready decision for held #9", decision.RecommendedAction, decision.Target, decision.Mutations)
	}
	requireReasonContains(t, decision, `Ready-label promotion withheld for issue #9: issue carries excluded label "blocked"`)
}

// requireNoReadyApproval loads the state RunOnce saved and fails if any
// label_issue_ready approval was recorded: approving one adds the ready label
// to the target regardless of the decision's planned mutations.
func requireNoReadyApproval(t *testing.T, cfg *config.Config) {
	t.Helper()
	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	for _, approval := range loaded.Approvals {
		if approval.Action == ActionLabelIssueReady {
			t.Fatalf("approval %s action=%s target=%#v status=%s summary=%q, want no label_issue_ready approval for a held issue", approval.ID, approval.Action, approval.Target, approval.Status, approval.Summary)
		}
	}
}

// A held promotion must not reach the ready label through the approval
// route. With remove_blocked_label not whitelisted, the remaining
// blocked-label removal used to become a mutation-less label_issue_ready
// decision, and RunOnce minted a pending approval whose execution adds the
// ready label to the held issue.
func TestRunOnce_HeldPromotionNeverMintsReadyApproval(t *testing.T) {
	for _, tc := range []struct {
		name  string
		issue github.Issue
	}{
		{name: "epic title", issue: testIssue(9, "Epic: big parent", "blocked")},
		{name: "excluded label", issue: testIssue(9, "Waiting on vendor", "blocked")},
	} {
		for _, autoPromote := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/auto_promote_ready=%v", tc.name, autoPromote), func(t *testing.T) {
				cfg := pilotPromotionConfig(t)
				cfg.ExcludeLabels = []string{"epic", "blocked"}
				cfg.Supervisor.BlockedLabel = "blocked"
				cfg.Supervisor.SafeActions = []string{config.SupervisorActionAddReadyLabel}
				cfg.Supervisor.AutoPromoteReady = autoPromote
				reader := &fakeReader{issues: []github.Issue{tc.issue}}

				decision, err := RunOnce(context.Background(), cfg, reader)
				if err != nil {
					t.Fatalf("RunOnce: %v", err)
				}
				if len(reader.addedLabels) != 0 || len(reader.removedLabels) != 0 {
					t.Fatalf("added=%q removed=%q, want no label change on held #9", reader.addedLabels, reader.removedLabels)
				}
				if decision.RecommendedAction == ActionLabelIssueReady {
					t.Fatalf("action = %q target = %#v summary = %q, want no label_issue_ready decision for held #9", decision.RecommendedAction, decision.Target, decision.Summary)
				}
				if decision.ApprovalID != "" {
					t.Fatalf("decision.ApprovalID = %q, want no approval", decision.ApprovalID)
				}
				requireNoReadyApproval(t, cfg)
				requireReasonContains(t, decision, "Ready-label promotion withheld for issue #9")
			})
		}
	}
}

// An epic keeps its blocked label even when remove_blocked_label is a safe
// action: lifting it alone can make the epic dispatchable when the project
// does not require a ready label.
func TestRunOnce_EpicKeepsBlockedLabelWhenRemovalIsSafe(t *testing.T) {
	for _, autoPromote := range []bool{false, true} {
		t.Run(fmt.Sprintf("auto_promote_ready=%v", autoPromote), func(t *testing.T) {
			cfg := pilotPromotionConfig(t)
			cfg.Supervisor.BlockedLabel = "blocked"
			cfg.Supervisor.SafeActions = []string{
				config.SupervisorActionAddReadyLabel,
				config.SupervisorActionRemoveBlockedLabel,
			}
			cfg.Supervisor.AutoPromoteReady = autoPromote
			reader := &fakeReader{issues: []github.Issue{testIssue(9, "Epic: big parent", "blocked")}}

			decision, err := RunOnce(context.Background(), cfg, reader)
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if len(reader.addedLabels) != 0 || len(reader.removedLabels) != 0 {
				t.Fatalf("added=%q removed=%q, want no label change on epic #9", reader.addedLabels, reader.removedLabels)
			}
			if decision.RecommendedAction == ActionLabelIssueReady {
				t.Fatalf("action = %q mutations = %#v, want no queue mutation for epic #9", decision.RecommendedAction, decision.Mutations)
			}
			requireNoReadyApproval(t, cfg)
		})
	}
}

// A policy hold (auto_promote_ready off) withholds only the ready label: a
// whitelisted blocked-label removal still applies as before #1240.
func TestRunOnce_PolicyHoldStillRemovesSafeBlockedLabel(t *testing.T) {
	cfg := pilotPromotionConfig(t)
	cfg.Supervisor.BlockedLabel = "blocked"
	cfg.Supervisor.SafeActions = []string{
		config.SupervisorActionAddReadyLabel,
		config.SupervisorActionRemoveBlockedLabel,
	}
	reader := &fakeReader{issues: []github.Issue{testIssue(9, "Waiting on vendor", "blocked")}}

	decision, err := RunOnce(context.Background(), cfg, reader)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(reader.addedLabels) != 0 {
		t.Fatalf("added labels = %q, want none with auto_promote_ready off", reader.addedLabels)
	}
	if got, want := strings.Join(reader.removedLabels, ","), "#9:blocked"; got != want {
		t.Fatalf("removed labels = %q, want %q", got, want)
	}
	requireNoAddReadyMutation(t, decision)
	requireNoReadyApproval(t, cfg)
}

// With auto_promote_ready on, a held backlog must not cost a forge read per
// held issue every cycle, and the decision lists only the first few holds.
func TestFirstQueueActionCandidate_HeldBacklogSkipsForgeReadsAndCapsNotes(t *testing.T) {
	cfg := pilotPromotionConfig(t)
	cfg.Supervisor.AutoPromoteReady = true
	cfg.BlockerPatterns = []string{`blocked by #(\d+)`}
	reader := &fakeReader{}
	eng := testEngine(cfg, reader)
	running := testIssue(3, "Pilot repair", "pilot-ready", "p0")

	var lower []github.Issue
	for number := 10; number < 15; number++ {
		issue := testIssue(number, fmt.Sprintf("Polish page %d", number), "p2")
		issue.Body = "blocked by #500"
		lower = append(lower, issue)
	}
	cand, holds, err := eng.firstQueueActionCandidate(state.NewState(), lower, append([]github.Issue{running}, lower...), PolicyRuleIssueLabels)
	if err != nil {
		t.Fatalf("firstQueueActionCandidate: %v", err)
	}
	if cand != nil {
		t.Fatalf("candidate = %+v, want none: every p2 issue is held while p0 ready work is open", cand)
	}
	if got := reader.closedIssueCalls[500]; got != 0 {
		t.Fatalf("blocker reads = %d, want 0 for issues the promotion guard already held", got)
	}
	if len(holds) != maxPromotionHoldNotes+1 {
		t.Fatalf("holds = %q, want %d notes plus a count", holds, maxPromotionHoldNotes)
	}
	if got, want := holds[len(holds)-1], "Ready-label promotion withheld for 2 more issue(s)"; got != want {
		t.Fatalf("last hold note = %q, want %q", got, want)
	}
}

// The dependency-unblock controller (dynamic wave) may lift the blocked label
// it owns, but a member carrying another exclude label is never promoted.
func TestDecide_DependencyUnblockNeverPromotesExcludedMember(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.ExcludeLabels = []string{"needs-design"}
	enableDependencyUnblock(cfg)
	reader := &fakeReader{
		issues:       []github.Issue{blockedIssue(148, []int{147}, "needs-design")},
		closedIssues: map[int]bool{147: true},
	}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction == ActionUnblockIssue {
		t.Fatalf("action = %q mutations = %#v, want no unblock-and-promote for an excluded member", decision.RecommendedAction, decision.Mutations)
	}
	requireNoAddReadyMutation(t, decision)
}

func TestDecide_DynamicWaveHoldsEpicMarkedTitles(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.Supervisor.SafeActions = []string{config.SupervisorActionAddReadyLabel}
	enableDynamicWave(cfg)
	reader := &fakeReader{issues: []github.Issue{incidentEpicIssue(), testIssue(8, "Unrelated cleanup")}}

	decision, err := testEngine(cfg, reader).Decide(state.NewState())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Target == nil || decision.Target.Issue != 8 {
		t.Fatalf("action = %q target = %#v, want dynamic wave to select #8, not epic #7", decision.RecommendedAction, decision.Target)
	}
	for _, mutation := range decision.Mutations {
		if mutation.Issue == 7 {
			t.Fatalf("mutations = %#v, want none for epic #7", decision.Mutations)
		}
	}
}

// The dynamic wave skip rules already hold epics and excluded labels; the
// promotion point itself must refuse them too, and must not strip the ready
// label from other issues on behalf of a held selection.
func TestDynamicQueueActionCandidate_HeldSelectionPlansNoMutation(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueLabels = []string{"maestro-ready"}
	cfg.ExcludeLabels = []string{"needs-design"}
	cfg.Supervisor.SafeActions = []string{config.SupervisorActionAddReadyLabel, config.SupervisorActionRemoveReadyLabel}
	enableDynamicWave(cfg)
	cfg.Supervisor.DynamicWave.OwnsReadyLabel = true
	eng := testEngine(cfg, &fakeReader{})
	other := testIssue(5, "Already ready", "maestro-ready")

	for _, selected := range []github.Issue{incidentEpicIssue(), testIssue(9, "Redo the logo", "needs-design")} {
		if cand := eng.dynamicQueueActionCandidate(state.NewState(), selected, []github.Issue{other, selected}); cand != nil {
			t.Fatalf("candidate for #%d = %+v, want nil (no promotion, no ready-label removal)", selected.Number, cand)
		}
	}
}

func TestFirstQueueActionCandidate_AutoPromoteReadyRespectsReadyWorkPriority(t *testing.T) {
	cfg := pilotPromotionConfig(t)
	cfg.Supervisor.AutoPromoteReady = true
	eng := testEngine(cfg, &fakeReader{})
	running := testIssue(3, "Pilot repair", "pilot-ready", "p1")

	lower := testIssue(9, "Polish settings page", "p2")
	cand, holds, err := eng.firstQueueActionCandidate(state.NewState(), []github.Issue{lower}, []github.Issue{running, lower}, PolicyRuleIssueLabels)
	if err != nil {
		t.Fatalf("firstQueueActionCandidate: %v", err)
	}
	if cand != nil && cand.addReady {
		t.Fatalf("candidate = %+v, want p2 issue withheld while p1 ready work is open", cand)
	}
	if len(holds) != 1 || !strings.Contains(holds[0], "priority p2 ranks below open ready issue #3 (p1)") {
		t.Fatalf("holds = %q, want a lower-priority hold naming #3", holds)
	}

	same := testIssue(10, "Fix login redirect", "p1")
	cand, _, err = eng.firstQueueActionCandidate(state.NewState(), []github.Issue{same}, []github.Issue{running, same}, PolicyRuleIssueLabels)
	if err != nil {
		t.Fatalf("firstQueueActionCandidate: %v", err)
	}
	if cand == nil || !cand.addReady || cand.issue.Number != 10 {
		t.Fatalf("candidate = %+v, want p1 issue promoted alongside p1 ready work", cand)
	}
}

func TestJournalPromotionHold_OncePerIssue(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })

	cfg := pilotPromotionConfig(t)
	reader := &fakeReader{issues: []github.Issue{incidentEpicIssue()}}
	journal := newInMemoryPromotionHoldJournal()
	for cycle := 0; cycle < 3; cycle++ {
		eng := testEngine(cfg, reader)
		eng.promotionJournal = journal
		if _, err := eng.Decide(state.NewState()); err != nil {
			t.Fatalf("Decide cycle %d: %v", cycle, err)
		}
	}

	line := "withholding add_ready_label from issue #7"
	if got := strings.Count(buf.String(), line); got != 1 {
		t.Fatalf("journal lines for issue #7 = %d, want exactly 1:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "epic/parent") {
		t.Fatalf("journal line does not say why the promotion was skipped:\n%s", buf.String())
	}
}

func TestTitleMarksEpic(t *testing.T) {
	cases := map[string]bool{
		"Epic: onboarding":                       true,
		"  epic : spaced prefix":                 true,
		"[P2] Epic: tagged prefix":               true,
		"[P2] Web redesign of the SPA (epic)":    true,
		"(EPIC) uppercase tag":                   true,
		"[epic] bracket tag":                     true,
		"Billing overhaul [ Epic ]":              true,
		"Fix epic-label filter in the dashboard": false,
		"Epics: list view":                       false,
		"[epic-123] tracking id":                 false,
		"Handle the word epic in titles":         false,
		"Plain bug fix":                          false,
	}
	for title, want := range cases {
		if got := titleMarksEpic(title); got != want {
			t.Errorf("titleMarksEpic(%q) = %v, want %v", title, got, want)
		}
	}
}
