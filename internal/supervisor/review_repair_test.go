package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// #565 — green+mergeable+settled-retry_exhausted PR with ≥1 Greptile
// P0/P1 inline comment on head must trigger a spawn_review_repair
// recommendation. The verb MUST be in the executor registry — the
// "refused at mint" loop the issue calls out only fires on
// spawn_repair_worker, never on spawn_review_repair.
func TestDecide_RetryExhaustedGreenPR_WithGreptileP1OnHead_SpawnsReviewRepair(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	enabled := true
	cfg.Supervisor.ReviewRepair.Enabled = &enabled
	cfg.Supervisor.ReviewRepair.MaxRetries = 1

	reader := &fakeReader{
		prs:        []github.PR{{Number: 564, HeadRefName: "feat/sup-111", State: "OPEN", Mergeable: "MERGEABLE"}},
		ciStatuses: map[int]string{564: "success"},
		highSeverityHeadSHA: map[int]string{
			564: "deadbeefcafebabe1234567890abcdef12345678",
		},
		highSeverityFindings: map[int][]github.ReviewComment{
			564: {
				{
					Path: "internal/supervisor/dependency_unblock.go",
					Line: 42,
					Body: `<img alt="P1">P1: resolutionCache parameter is never consulted`,
					User: "greptile-apps",
				},
			},
		},
	}
	st := state.NewState()
	st.Sessions["sup-111"] = &state.Session{
		IssueNumber:                 442,
		IssueTitle:                  "scribe redesign",
		Status:                      state.StatusRetryExhausted,
		Branch:                      "feat/sup-111",
		PRNumber:                    564,
		RetryCount:                  2,
		LastNotifiedStatus:          "review_retry_exhausted",
		PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
		StartedAt:                   time.Now().UTC().Add(-2 * time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionSpawnReviewRepair {
		t.Fatalf("action = %q, want %q (green PR with P0/P1 on head must auto-spawn review-repair)", decision.RecommendedAction, ActionSpawnReviewRepair)
	}
	if decision.Risk != RiskMutating {
		t.Fatalf("risk = %q, want %q", decision.Risk, RiskMutating)
	}
	if decision.Target == nil || decision.Target.PR != 564 || decision.Target.Issue != 442 {
		t.Fatalf("target = %#v, want PR=564 issue=442", decision.Target)
	}
	if decision.Target.HeadSHA == "" {
		t.Fatalf("target.HeadSHA is empty — review-repair decisions must stamp the head SHA for (pr, head) idempotency")
	}
	if decision.ReviewRepair == nil {
		t.Fatalf("decision.ReviewRepair payload missing — dispatcher cannot build the scoped prompt")
	}
	if len(decision.ReviewRepair.Findings) != 1 {
		t.Fatalf("review-repair findings count = %d, want 1", len(decision.ReviewRepair.Findings))
	}
	if decision.ReviewRepair.Findings[0].Path != "internal/supervisor/dependency_unblock.go" {
		t.Fatalf("finding path = %q, want internal/supervisor/dependency_unblock.go", decision.ReviewRepair.Findings[0].Path)
	}
	if decision.ReviewRepair.Backend == "" {
		t.Fatalf("review-repair backend is empty; want the configured strong backend")
	}
	stuck := requireStuckState(t, decision, state.StuckReviewRepairSpawned)
	if stuck.Severity != SeverityWarning {
		t.Errorf("stuck severity = %q, want warning", stuck.Severity)
	}
	if !strings.Contains(strings.ToLower(stuck.Summary), "auto review-repair") {
		t.Errorf("stuck summary = %q, want it to name the auto review-repair branch", stuck.Summary)
	}
}

func TestDecide_RetryExhaustedGreenPR_WithSimplicityFinding_SpawnsReviewRepair(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "greptile"
	cfg.ReviewGateStreams = []string{"greptile", "simplicity"}
	cfg.Supervisor.ReviewRepair.MaxRetries = 1

	reader := &fakeReader{
		prs:        []github.PR{{Number: 650, HeadRefName: "feat/sup-161", State: "OPEN", Mergeable: "MERGEABLE"}},
		ciStatuses: map[int]string{650: "success"},
		reviewVerdicts: map[int]github.ReviewGateVerdict{
			650: {
				Passed: false,
				Streams: []github.ReviewStreamVerdict{
					{Name: "greptile", Passed: true},
					{Name: "simplicity", Passed: false, Findings: []github.ReviewComment{{
						Path: "internal/foo.go",
						Line: 27,
						Body: "blocking: this adds an unnecessary abstraction; inline the single caller",
						User: "maestro-simplicity-reviewer",
					}}},
				},
			},
		},
		highSeverityHeadSHA: map[int]string{650: "feedfacecafebeef"},
		highSeverityFindings: map[int][]github.ReviewComment{
			650: {{
				Path: "internal/foo.go",
				Line: 27,
				Body: "blocking: this adds an unnecessary abstraction; inline the single caller",
				User: "maestro-simplicity-reviewer",
			}},
		},
	}
	st := state.NewState()
	st.Sessions["sup-161"] = &state.Session{
		IssueNumber:                 649,
		IssueTitle:                  "add simplicity reviewer",
		Status:                      state.StatusRetryExhausted,
		Branch:                      "feat/sup-161",
		PRNumber:                    650,
		LastNotifiedStatus:          "review_retry_exhausted",
		PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
		StartedAt:                   time.Now().UTC().Add(-time.Hour),
	}

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionSpawnReviewRepair {
		t.Fatalf("action = %q, want %q for blocking simplicity finding", decision.RecommendedAction, ActionSpawnReviewRepair)
	}
	if decision.ReviewRepair == nil || len(decision.ReviewRepair.Findings) != 1 {
		t.Fatalf("review repair payload = %#v, want one simplicity finding", decision.ReviewRepair)
	}
	got := decision.ReviewRepair.Findings[0]
	if got.User != "maestro-simplicity-reviewer" || !strings.Contains(got.Body, "unnecessary abstraction") {
		t.Fatalf("review repair finding = %#v, want simplicity finding", got)
	}
}

// Negative: the review-repair branch must NOT fire on a green PR that
// has no Greptile P0/P1 findings on head (convergence-merge handles
// that case).
func TestDecide_RetryExhaustedGreenPR_NoP0P1Findings_DoesNotSpawnReviewRepair(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	reader := &fakeReader{
		prs:                  []github.PR{{Number: 564, HeadRefName: "feat/sup-111", State: "OPEN", Mergeable: "MERGEABLE"}},
		ciStatuses:           map[int]string{564: "success"},
		highSeverityHeadSHA:  map[int]string{564: "abc123"},
		highSeverityFindings: map[int][]github.ReviewComment{
			// no findings -> nil
		},
	}
	st := state.NewState()
	st.Sessions["sup-111"] = &state.Session{
		IssueNumber:                 442,
		Status:                      state.StatusRetryExhausted,
		Branch:                      "feat/sup-111",
		PRNumber:                    564,
		LastNotifiedStatus:          "review_retry_exhausted",
		PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
		StartedAt:                   time.Now().UTC().Add(-time.Hour),
	}
	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction == ActionSpawnReviewRepair {
		t.Fatalf("review-repair must not fire without P0/P1 findings; got %+v", decision)
	}
}

// Disabled config: even with findings, the feature stays off.
func TestDecide_ReviewRepairDisabled_DoesNotSpawn(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	disabled := false
	cfg.Supervisor.ReviewRepair.Enabled = &disabled
	cfg.Supervisor.ReviewRepair.MaxRetries = 1

	reader := &fakeReader{
		prs:                 []github.PR{{Number: 564, HeadRefName: "feat/sup-111", State: "OPEN", Mergeable: "MERGEABLE"}},
		ciStatuses:          map[int]string{564: "success"},
		highSeverityHeadSHA: map[int]string{564: "deadbeef"},
		highSeverityFindings: map[int][]github.ReviewComment{
			564: {{Path: "x.go", Line: 1, Body: "P1: bad", User: "greptile-apps"}},
		},
	}
	st := state.NewState()
	st.Sessions["sup-111"] = &state.Session{
		IssueNumber:        442,
		Status:             state.StatusRetryExhausted,
		Branch:             "feat/sup-111",
		PRNumber:           564,
		LastNotifiedStatus: "review_retry_exhausted",
		StartedAt:          time.Now().UTC().Add(-time.Hour),
	}
	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction == ActionSpawnReviewRepair {
		t.Fatalf("disabled review-repair must not fire; got %q", decision.RecommendedAction)
	}
}

// Exhausted budget per (pr,head_sha): supervisor must fall through to
// a visible operator decision, NEVER a silent dead-end.
func TestDecide_ReviewRepairExhausted_FallsThroughToAttention(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	cfg.Supervisor.ReviewRepair.MaxRetries = 1
	reader := &fakeReader{
		prs:                 []github.PR{{Number: 564, HeadRefName: "feat/sup-111", State: "OPEN", Mergeable: "MERGEABLE"}},
		ciStatuses:          map[int]string{564: "success"},
		highSeverityHeadSHA: map[int]string{564: "deadbeef0000"},
		highSeverityFindings: map[int][]github.ReviewComment{
			564: {{Path: "internal/foo.go", Line: 12, Body: "P0: nil panic", User: "greptile-apps"}},
		},
	}
	st := state.NewState()
	st.Sessions["sup-111"] = &state.Session{
		IssueNumber:        442,
		Status:             state.StatusRetryExhausted,
		Branch:             "feat/sup-111",
		PRNumber:           564,
		LastNotifiedStatus: "review_retry_exhausted",
		StartedAt:          time.Now().UTC().Add(-time.Hour),
	}
	// Simulate one attempt already burnt.
	st.RecordReviewRepairAttempt(564, "deadbeef0000", 442, "approval-1", cfg.Supervisor.ReviewRepair.MaxRetries, time.Now().UTC())

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction == ActionSpawnReviewRepair {
		t.Fatalf("exhausted budget must not respawn; got %q", decision.RecommendedAction)
	}
	if decision.RecommendedAction != ActionMonitorOpenPR && decision.RecommendedAction != ActionMergePR {
		t.Fatalf("expected fall-through to monitor_open_pr or merge_pr, got %q", decision.RecommendedAction)
	}
	stuck := requireStuckState(t, decision, state.StuckReviewRepairExhausted)
	if !strings.Contains(strings.ToLower(stuck.Summary), "exhausted") {
		t.Errorf("stuck summary = %q, want it to name exhausted", stuck.Summary)
	}
	if len(stuck.Evidence) == 0 {
		t.Errorf("stuck evidence is empty; operators need the unresolved findings listed")
	}
}

// Fall-through to merge_pr approval: explicit opt-in routes the residual
// findings to a cautious-gate merge instead of a hold-for-review.
func TestDecide_ReviewRepairExhausted_FallThroughMergeEnabled_EmitsMergePR(t *testing.T) {
	cfg := testConfig(t)
	cfg.ReviewGate = "none"
	cfg.Supervisor.ReviewRepair.MaxRetries = 1
	on := true
	cfg.Supervisor.ReviewRepair.FallThroughToMergeApproval = &on

	reader := &fakeReader{
		prs:                 []github.PR{{Number: 564, HeadRefName: "feat/sup-111", State: "OPEN", Mergeable: "MERGEABLE"}},
		ciStatuses:          map[int]string{564: "success"},
		highSeverityHeadSHA: map[int]string{564: "deadbeef0000"},
		highSeverityFindings: map[int][]github.ReviewComment{
			564: {{Path: "internal/foo.go", Line: 12, Body: "P0: nil panic", User: "greptile-apps"}},
		},
	}
	st := state.NewState()
	st.Sessions["sup-111"] = &state.Session{
		IssueNumber:        442,
		Status:             state.StatusRetryExhausted,
		Branch:             "feat/sup-111",
		PRNumber:           564,
		LastNotifiedStatus: "review_retry_exhausted",
		StartedAt:          time.Now().UTC().Add(-time.Hour),
	}
	st.RecordReviewRepairAttempt(564, "deadbeef0000", 442, "approval-1", cfg.Supervisor.ReviewRepair.MaxRetries, time.Now().UTC())

	decision, err := testEngine(cfg, reader).Decide(st)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.RecommendedAction != ActionMergePR {
		t.Fatalf("fall_through_to_merge_approval=true must emit merge_pr; got %q", decision.RecommendedAction)
	}
}

// The dispatched prompt must mention the specific Greptile finding(s)
// — not a generic issue restatement (#565 acceptance criterion).
func TestFormatReviewRepairPrompt_ScopedToFindings(t *testing.T) {
	findings := []github.ReviewComment{
		{Path: "internal/foo.go", Line: 42, Body: "P1: nil pointer in resolve()", User: "greptile-apps"},
		{Path: "internal/bar.go", Line: 10, Body: "P0: missing rollback on error", User: "greptile-apps"},
	}
	prompt := FormatReviewRepairPrompt(442, 564, "deadbeef000000000000000000000000", findings)
	for _, want := range []string{
		"PR #564",
		"issue #442",
		"internal/foo.go:42",
		"internal/bar.go:10",
		"P1", "P0",
		"nil pointer in resolve()",
		"missing rollback on error",
		"Do NOT re-implement the original issue",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\nprompt:\n%s", want, prompt)
		}
	}
}

// The verb must be a registry-supported action; the supervisor's
// at-mint guard wraps approver.IsKnownApprovalAction over this exact
// constant.
func TestSpawnReviewRepair_VerbConstant(t *testing.T) {
	if ActionSpawnReviewRepair != "spawn_review_repair" {
		t.Fatalf("ActionSpawnReviewRepair = %q, want spawn_review_repair", ActionSpawnReviewRepair)
	}
	if config.SupervisorActionSpawnReviewRepair != ActionSpawnReviewRepair {
		t.Fatalf("config constant drift: config=%q supervisor=%q", config.SupervisorActionSpawnReviewRepair, ActionSpawnReviewRepair)
	}
}
