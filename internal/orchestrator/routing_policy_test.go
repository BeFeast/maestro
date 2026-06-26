package orchestrator

import (
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/state"
)

func policyCfg() *config.Config {
	return &config.Config{
		MaxRetriesPerIssue: 5,
		Model: config.ModelConfig{
			Default: "codex",
			Backends: map[string]config.BackendDef{
				"gemini": {Cmd: "gemini"},
				"codex":  {Cmd: "codex"},
				"claude": {Cmd: "claude"},
			},
		},
		Routing: config.RoutingConfig{
			Mode: "policy",
			Tiers: map[string]config.RoutingTier{
				"cheap":    {Backend: "gemini", Rank: 0},
				"standard": {Backend: "codex", Effort: "medium", Rank: 1},
				"strong":   {Backend: "claude", Effort: "high", Rank: 2},
			},
			Policy: &config.RoutingPolicy{
				DefaultTier: "standard",
				Rules: []config.RoutingPolicyRule{
					{When: config.RoutingSignalMatch{Labels: []string{"migration"}}, Tier: "strong"},
				},
				Escalation: config.RoutingEscalation{Enabled: true, On: []string{"ci_failure", "retry"}, MaxTier: "strong"},
			},
		},
	}
}

func policyOrch(cfg *config.Config) *Orchestrator {
	return &Orchestrator{cfg: cfg, router: router.New(cfg)}
}

func issueWithLabels(n int, title string, labels ...string) github.Issue {
	issue := github.Issue{Number: n, Title: title}
	for _, l := range labels {
		issue.Labels = append(issue.Labels, struct {
			Name string `json:"name"`
		}{Name: l})
	}
	return issue
}

func TestApplyTierOverride_AppliesToClone(t *testing.T) {
	cfg := policyCfg()
	dec := router.BackendDecision{Backend: "codex", Effort: "high", Model: "gpt-5.5"}
	out := applyTierOverride(cfg, "codex", dec)
	if out == cfg {
		t.Fatalf("expected a cloned config")
	}
	if got := out.Model.Backends["codex"].Effort; got != "high" {
		t.Fatalf("override effort = %q, want high", got)
	}
	if got := out.Model.Backends["codex"].Model; got != "gpt-5.5" {
		t.Fatalf("override model = %q, want gpt-5.5", got)
	}
	// Base config must be untouched — the codex backend def carries no effort
	// of its own (the override lived on the tier).
	if got := cfg.Model.Backends["codex"].Effort; got != "" {
		t.Fatalf("base config mutated: codex effort = %q, want empty", got)
	}
}

func TestApplyTierOverride_NoOverrideReturnsSame(t *testing.T) {
	cfg := policyCfg()
	out := applyTierOverride(cfg, "codex", router.BackendDecision{Backend: "codex"})
	if out != cfg {
		t.Fatalf("expected the same config pointer when no override present")
	}
}

func TestRetryTrigger(t *testing.T) {
	if got := retryTrigger(&state.Session{CIFailureOutput: "boom"}); got != config.EscalationOnCIFailure {
		t.Fatalf("ci trigger = %q", got)
	}
	if got := retryTrigger(&state.Session{RetryReason: state.RetryReasonReviewFeedback}); got != config.EscalationOnReviewRejection {
		t.Fatalf("review trigger = %q", got)
	}
	if got := retryTrigger(&state.Session{}); got != config.EscalationOnRetry {
		t.Fatalf("plain trigger = %q", got)
	}
}

func TestEscalationSteps(t *testing.T) {
	pol := policyCfg().Routing.Policy
	// retry trigger enabled, RetryCount drives the climb.
	sess := &state.Session{IssueNumber: 1, RetryCount: 2}
	if got := escalationSteps(pol, sess); got != 2 {
		t.Fatalf("escalationSteps = %d, want 2", got)
	}
	// review trigger NOT in escalation.on → no climb.
	sess2 := &state.Session{IssueNumber: 2, RetryCount: 3, RetryReason: state.RetryReasonReviewFeedback}
	if got := escalationSteps(pol, sess2); got != 0 {
		t.Fatalf("escalationSteps (disabled trigger) = %d, want 0", got)
	}
	// escalation disabled entirely.
	disabled := policyCfg().Routing.Policy
	disabled.Escalation.Enabled = false
	if got := escalationSteps(disabled, sess); got != 0 {
		t.Fatalf("escalationSteps (disabled) = %d, want 0", got)
	}
}

// TestEscalationSteps_IgnoresStaleAttempts is the regression guard for the
// review finding: a stale dead/failed no-PR session for the same issue inflates
// FailedAttemptsForIssue, but the climb must track only the live session's own
// RetryCount so retries do not skip tiers the current attempt history does not
// warrant.
func TestEscalationSteps_IgnoresStaleAttempts(t *testing.T) {
	pol := policyCfg().Routing.Policy
	// The issue would have a high FailedAttemptsForIssue from old dead sessions,
	// but escalationSteps no longer reads state — it climbs by RetryCount only.
	sess := &state.Session{IssueNumber: 7, RetryCount: 1}
	if got := escalationSteps(pol, sess); got != 1 {
		t.Fatalf("escalationSteps with stale attempts = %d, want 1 (RetryCount only)", got)
	}
	// A fresh retry (RetryCount 0) still climbs at least one tier when triggered.
	if got := escalationSteps(pol, &state.Session{IssueNumber: 7}); got != 1 {
		t.Fatalf("escalationSteps floor = %d, want 1", got)
	}
}

func TestEscalateRetryBackend_ClimbsTier(t *testing.T) {
	o := policyOrch(policyCfg())
	s := state.NewState()
	// Ordinary issue starts at default standard (codex); retry 1 climbs to strong.
	sess := &state.Session{IssueNumber: 1, RetryCount: 1, Backend: "codex"}
	issue := issueWithLabels(1, "Ordinary change")
	dec, ok := o.escalateRetryBackend(s, sess, issue)
	if !ok {
		t.Fatalf("escalateRetryBackend ok = false, want true")
	}
	if dec.Backend != "claude" || dec.Tier != "strong" {
		t.Fatalf("escalated decision = %+v, want claude/strong", dec)
	}
}

func TestEscalateRetryBackend_InertWhenManual(t *testing.T) {
	cfg := policyCfg()
	cfg.Routing.Mode = "manual"
	o := policyOrch(cfg)
	s := state.NewState()
	sess := &state.Session{IssueNumber: 1, RetryCount: 1, Backend: "codex"}
	if _, ok := o.escalateRetryBackend(s, sess, issueWithLabels(1, "x")); ok {
		t.Fatalf("escalateRetryBackend ok = true in manual mode, want false")
	}
}

func TestEscalateRetryBackend_SkipsBlockedTarget(t *testing.T) {
	o := policyOrch(policyCfg())
	s := state.NewState()
	// claude (strong tier) is disabled → escalation must not target it.
	cfg := o.cfg
	def := cfg.Model.Backends["claude"]
	disabled := false
	def.Enabled = &disabled
	cfg.Model.Backends["claude"] = def
	sess := &state.Session{IssueNumber: 1, RetryCount: 1, Backend: "codex"}
	if _, ok := o.escalateRetryBackend(s, sess, issueWithLabels(1, "Ordinary")); ok {
		t.Fatalf("escalateRetryBackend ok = true onto disabled backend, want false")
	}
}

func TestEscalateRetryBackend_SkipsPipelinePhase(t *testing.T) {
	o := policyOrch(policyCfg())
	s := state.NewState()
	// A planner/validator pipeline phase keeps its own per-role backend.
	sess := &state.Session{IssueNumber: 1, RetryCount: 1, Backend: "codex", Phase: state.PhasePlan}
	if _, ok := o.escalateRetryBackend(s, sess, issueWithLabels(1, "Plan")); ok {
		t.Fatalf("escalateRetryBackend ok = true for planner phase, want false")
	}
}

func TestApplyPolicyBudget_DowngradesOverCap(t *testing.T) {
	cfg := policyCfg()
	cfg.Routing.Policy.Budget.MaxStrongPerWave = 1
	o := policyOrch(cfg)
	s := state.NewState()
	// One active strong-tier session already.
	s.Sessions["slot-a"] = &state.Session{
		IssueNumber: 9, Status: state.StatusRunning,
		BackendSelection: &state.BackendSelection{Tier: "strong"},
	}
	strongDecision := router.BackendDecision{Backend: "claude", Tier: "strong", Reason: "policy:strong"}
	out := o.applyPolicyBudget(s, strongDecision)
	if out.Tier != "standard" || out.Backend != "codex" {
		t.Fatalf("budget downgrade = %+v, want standard/codex", out)
	}
}

func TestApplyPolicyBudget_UnderCapKeepsStrong(t *testing.T) {
	cfg := policyCfg()
	cfg.Routing.Policy.Budget.MaxStrongPerWave = 2
	o := policyOrch(cfg)
	s := state.NewState()
	s.Sessions["slot-a"] = &state.Session{
		IssueNumber: 9, Status: state.StatusRunning,
		BackendSelection: &state.BackendSelection{Tier: "strong"},
	}
	strongDecision := router.BackendDecision{Backend: "claude", Tier: "strong", Reason: "policy:strong"}
	out := o.applyPolicyBudget(s, strongDecision)
	if out.Tier != "strong" {
		t.Fatalf("under-cap decision = %+v, want strong unchanged", out)
	}
}

// TestApplyPolicyBudget_DefaultTierIsTop is the regression guard for the review
// finding: when default_tier == the top (strong) tier, the cap must still apply
// — an over-budget dispatch downgrades to the tier one rank below top instead of
// silently dispatching unlimited strong workers.
func TestApplyPolicyBudget_DefaultTierIsTop(t *testing.T) {
	cfg := policyCfg()
	cfg.Routing.Policy.DefaultTier = "strong" // default IS the top tier
	cfg.Routing.Policy.Budget.MaxStrongPerWave = 1
	o := policyOrch(cfg)
	s := state.NewState()
	s.Sessions["slot-a"] = &state.Session{
		IssueNumber: 9, Status: state.StatusRunning,
		BackendSelection: &state.BackendSelection{Tier: "strong"},
	}
	strongDecision := router.BackendDecision{Backend: "claude", Tier: "strong", Reason: "policy:strong"}
	out := o.applyPolicyBudget(s, strongDecision)
	if out.Tier != "standard" || out.Backend != "codex" {
		t.Fatalf("budget downgrade with default_tier=strong = %+v, want standard/codex (tier below top)", out)
	}
}

// TestApplyPolicyBudget_SingleTierUnenforceable documents that a config with a
// single tier cannot enforce the cap (there is nowhere to downgrade) — the
// decision is returned unchanged rather than downgrading to self.
func TestApplyPolicyBudget_SingleTierUnenforceable(t *testing.T) {
	cfg := policyCfg()
	cfg.Routing.Tiers = map[string]config.RoutingTier{"only": {Backend: "claude", Rank: 0}}
	cfg.Routing.Policy.DefaultTier = "only"
	cfg.Routing.Policy.Budget.MaxStrongPerWave = 1
	o := policyOrch(cfg)
	s := state.NewState()
	s.Sessions["slot-a"] = &state.Session{
		IssueNumber: 9, Status: state.StatusRunning,
		BackendSelection: &state.BackendSelection{Tier: "only"},
	}
	dec := router.BackendDecision{Backend: "claude", Tier: "only", Reason: "policy:only"}
	out := o.applyPolicyBudget(s, dec)
	if out.Tier != "only" || out.Backend != "claude" {
		t.Fatalf("single-tier budget = %+v, want unchanged only/claude", out)
	}
}

func TestPolicyCandidateScores_SelectedHighest(t *testing.T) {
	o := policyOrch(policyCfg())
	scores := o.policyCandidateScores("strong")
	if len(scores) != 3 {
		t.Fatalf("scores len = %d, want 3", len(scores))
	}
	var strong state.BackendCandidate
	for _, c := range scores {
		if c.Backend == "claude" {
			strong = c
		}
	}
	if strong.Fit != 1.0 {
		t.Fatalf("selected tier fit = %v, want 1.0", strong.Fit)
	}
	if strong.Policy != 1.0 {
		t.Fatalf("top-rank policy score = %v, want 1.0", strong.Policy)
	}
}
