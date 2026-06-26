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
	// #792 P1-A: the override is ALSO mirrored onto the distinct TierModel/
	// TierEffort carriers — the only fields threaded into the worker argv — so the
	// #513 attribution Model/Effort never leak for a non-policy dispatch.
	if got := out.Model.Backends["codex"].TierEffort; got != "high" {
		t.Fatalf("override TierEffort = %q, want high", got)
	}
	if got := out.Model.Backends["codex"].TierModel; got != "gpt-5.5" {
		t.Fatalf("override TierModel = %q, want gpt-5.5", got)
	}
	// Base config must be untouched — the codex backend def carries no effort
	// of its own (the override lived on the tier).
	if got := cfg.Model.Backends["codex"].Effort; got != "" {
		t.Fatalf("base config mutated: codex effort = %q, want empty", got)
	}
	if got := cfg.Model.Backends["codex"].TierEffort; got != "" {
		t.Fatalf("base config mutated: codex TierEffort = %q, want empty", got)
	}
}

func TestApplyTierOverride_NoOverrideReturnsSame(t *testing.T) {
	cfg := policyCfg()
	out := applyTierOverride(cfg, "codex", router.BackendDecision{Backend: "codex"})
	if out != cfg {
		t.Fatalf("expected the same config pointer when no override present")
	}
}

// TestTierOverrideConfigForSession_ReappliesPolicyOverride is the #792 checkpoint
// regression guard: a policy-routed session that hits the soft-token checkpoint
// respawns through RespawnInPlace, which reads the override off the backend def.
// The checkpoint path passes the base o.cfg (no override), so the override must
// be reconstructed from the session's durable BackendSelection audit record or
// the resumed worker silently drops the tier model/effort for the rest of the run.
func TestTierOverrideConfigForSession_ReappliesPolicyOverride(t *testing.T) {
	o := policyOrch(policyCfg())
	sess := &state.Session{
		IssueNumber: 1, Backend: "claude",
		BackendSelection: &state.BackendSelection{Tier: "strong", Effort: "high", Model: "opus-4.8"},
	}
	out := o.tierOverrideConfigForSession(sess)
	if out == o.cfg {
		t.Fatalf("expected a cloned config carrying the tier override")
	}
	// The argv-threaded carriers must be set so the respawned worker dispatches
	// on the selected tier (see worker.appendTierModelEffort).
	if got := out.Model.Backends["claude"].TierEffort; got != "high" {
		t.Fatalf("respawn TierEffort = %q, want high", got)
	}
	if got := out.Model.Backends["claude"].TierModel; got != "opus-4.8" {
		t.Fatalf("respawn TierModel = %q, want opus-4.8", got)
	}
	// Base config must be untouched (override lives on the clone only).
	if got := o.cfg.Model.Backends["claude"].TierEffort; got != "" {
		t.Fatalf("base config mutated: claude TierEffort = %q, want empty", got)
	}
}

// TestTierOverrideConfigForSession_NonPolicyUnchanged guards the #792 P1-A
// inertness on the checkpoint path: a session with no recorded tier override
// (non-policy, or shadow mode where Effort/Model stay empty and only ShadowTier
// is set) must respawn on the base config so a non-policy checkpoint dispatch is
// byte-for-byte unchanged.
func TestTierOverrideConfigForSession_NonPolicyUnchanged(t *testing.T) {
	o := policyOrch(policyCfg())
	if out := o.tierOverrideConfigForSession(&state.Session{Backend: "claude"}); out != o.cfg {
		t.Fatalf("nil BackendSelection: expected base config, got clone")
	}
	shadow := &state.Session{Backend: "codex", BackendSelection: &state.BackendSelection{ShadowTier: "strong"}}
	if out := o.tierOverrideConfigForSession(shadow); out != o.cfg {
		t.Fatalf("shadow selection (no effort/model): expected base config, got clone")
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

// TestEscalationSteps_ReviewRejectionCountsMaintenance is the #792 P1-C
// regression guard: the review-feedback maintenance-retry path bumps
// MaintenanceRetryCount (not RetryCount), so a review_rejection escalation must
// count it or the climb is stuck at the floor (1) and never tracks the number of
// review retries.
func TestEscalationSteps_ReviewRejectionCountsMaintenance(t *testing.T) {
	cfg := policyCfg()
	cfg.Routing.Policy.Escalation.On = []string{"review_rejection"}
	pol := cfg.Routing.Policy
	sess := &state.Session{
		IssueNumber: 1, RetryCount: 0, MaintenanceRetryCount: 2,
		RetryReason: state.RetryReasonReviewFeedback,
	}
	if got := escalationSteps(pol, sess); got != 2 {
		t.Fatalf("escalationSteps (review_rejection) = %d, want 2 (maintenance retries)", got)
	}
}

// TestEscalateRetryBackend_ReviewRejectionClimbsViaMaintenance proves the P1-C
// fix end-to-end: a 2nd review-feedback retry of an issue that started at cheap
// climbs two tiers to strong. The pre-fix climb counted RetryCount(0)→floor 1
// and stranded the retry at standard.
func TestEscalateRetryBackend_ReviewRejectionClimbsViaMaintenance(t *testing.T) {
	cfg := policyCfg()
	cfg.Routing.Policy.Escalation.On = []string{"review_rejection"}
	cfg.Routing.Policy.Rules = append(cfg.Routing.Policy.Rules,
		config.RoutingPolicyRule{When: config.RoutingSignalMatch{Size: "small", Dependency: "leaf"}, Tier: "cheap"})
	o := policyOrch(cfg)
	s := state.NewState()
	sess := &state.Session{
		IssueNumber: 1, Backend: "gemini",
		RetryReason: state.RetryReasonReviewFeedback, MaintenanceRetryCount: 2,
	}
	issue := issueWithLabels(1, "Tiny", "size:small", "dependency:leaf") // starts cheap (rank 0)
	dec, ok := o.escalateRetryBackend(s, sess, issue)
	if !ok {
		t.Fatalf("escalateRetryBackend ok = false, want true")
	}
	if dec.Tier != "strong" || dec.Backend != "claude" {
		t.Fatalf("review escalation decision = %+v, want strong/claude (climbed 2 from cheap)", dec)
	}
}

// TestEscalateRetryBackend_ShadowKeepsBackend is the #792 P2-E regression guard:
// in shadow mode the retry/escalation path must log the would-pick WITHOUT
// changing the dispatched backend, so escalateRetryBackend returns ok=false and
// the retry reuses sess.Backend (pre-fix it returned model.default with ok=true,
// silently moving a strong-tier session to the default backend).
func TestEscalateRetryBackend_ShadowKeepsBackend(t *testing.T) {
	cfg := policyCfg()
	cfg.Routing.Policy.Shadow = true
	o := policyOrch(cfg)
	s := state.NewState()
	sess := &state.Session{IssueNumber: 1, RetryCount: 1, Backend: "claude"}
	if _, ok := o.escalateRetryBackend(s, sess, issueWithLabels(1, "Ordinary")); ok {
		t.Fatalf("shadow mode: escalateRetryBackend ok = true, want false (dispatch unchanged)")
	}
}

// TestEscalateRetryBackend_ShadowHonorsLabelOverride is the #792 review
// regression guard: shadow mode shadows only the *policy*. A model:<backend>
// label override is resolved before policy evaluation (resolve.go precedence 1)
// and is a deliberate operator move to relocate a stuck session, so the retry
// must still honor it even in shadow mode (pre-fix the unconditional shadow
// branch returned ok=false and the retry silently reused sess.Backend).
func TestEscalateRetryBackend_ShadowHonorsLabelOverride(t *testing.T) {
	cfg := policyCfg()
	cfg.Routing.Policy.Shadow = true
	o := policyOrch(cfg)
	s := state.NewState()
	sess := &state.Session{IssueNumber: 1, RetryCount: 1, Backend: "codex"}
	// Operator relabels the stuck session model:claude to move it off codex.
	issue := issueWithLabels(1, "Stuck", "model:claude")
	dec, ok := o.escalateRetryBackend(s, sess, issue)
	if !ok {
		t.Fatalf("shadow mode: label override dropped (ok=false); want the label honored")
	}
	if dec.Backend != "claude" || dec.Reason != router.ReasonLabel {
		t.Fatalf("shadow retry decision = %+v, want claude/%s (label override)", dec, router.ReasonLabel)
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

// TestApplyPolicyBudget_SkipsDowngradeOntoCoolingDownBackend is the #792 P1-B
// regression guard: resolveDispatchBackend health-gates the original (strong)
// decision, but applyPolicyBudget runs AFTER and could swap to a downgrade tier
// via DecisionForTier, which only checks config existence — never BackendHealth.
// A downgrade onto a cooling-down standard backend would spawn the exact #695
// doomed worker the gate prevents. So when the downgrade target is blocked, the
// budget path must keep the already-gated strong dispatch instead.
func TestApplyPolicyBudget_SkipsDowngradeOntoCoolingDownBackend(t *testing.T) {
	cfg := policyCfg()
	cfg.Routing.Policy.Budget.MaxStrongPerWave = 1
	o := policyOrch(cfg)
	s := state.NewState()
	// One active strong session → the next strong dispatch is over the cap and
	// would normally downgrade to standard (codex).
	s.Sessions["slot-a"] = &state.Session{
		IssueNumber: 9, Status: state.StatusRunning,
		BackendSelection: &state.BackendSelection{Tier: "strong"},
	}
	// But codex (the downgrade target) is cooling down after a provider limit.
	s.BackendHealth = map[string]state.BackendHealth{
		"codex": {State: state.BackendHealthCooldown, Reason: "provider_limit"},
	}
	strongDecision := router.BackendDecision{Backend: "claude", Tier: "strong", Reason: "policy:strong"}
	out := o.applyPolicyBudget(s, strongDecision)
	if out.Backend != "claude" || out.Tier != "strong" {
		t.Fatalf("must not downgrade onto cooling-down codex; got %+v", out)
	}
}

// TestApplyPolicyBudget_DowngradesWhenTargetHealthy confirms the P1-B gate does
// not over-fire: a healthy downgrade target still downgrades as before.
func TestApplyPolicyBudget_DowngradesWhenTargetHealthy(t *testing.T) {
	cfg := policyCfg()
	cfg.Routing.Policy.Budget.MaxStrongPerWave = 1
	o := policyOrch(cfg)
	s := state.NewState()
	s.Sessions["slot-a"] = &state.Session{
		IssueNumber: 9, Status: state.StatusRunning,
		BackendSelection: &state.BackendSelection{Tier: "strong"},
	}
	// A cooldown on an unrelated backend must not block the codex downgrade.
	s.BackendHealth = map[string]state.BackendHealth{
		"gemini": {State: state.BackendHealthCooldown, Reason: "provider_limit"},
	}
	strongDecision := router.BackendDecision{Backend: "claude", Tier: "strong", Reason: "policy:strong"}
	out := o.applyPolicyBudget(s, strongDecision)
	if out.Backend != "codex" || out.Tier != "standard" {
		t.Fatalf("healthy downgrade target = %+v, want standard/codex", out)
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
