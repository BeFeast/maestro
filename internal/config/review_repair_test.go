package config

import (
	"testing"
)

// #565: supervisor.review_repair defaults — enabled by default, "claude"
// backend, MaxRetries=1 (one repair worker per pr_number, head_sha).
func TestParse_ReviewRepairDefaults(t *testing.T) {
	cfg, err := parse([]byte("repo: owner/repo\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.Supervisor.ReviewRepair.Active() {
		t.Fatal("supervisor.review_repair.enabled must default to true")
	}
	if got := cfg.Supervisor.ReviewRepair.EffectiveBackend(); got != "claude" {
		t.Errorf("EffectiveBackend = %q, want claude", got)
	}
	if got := cfg.Supervisor.ReviewRepair.EffectiveMaxRetries(); got != 1 {
		t.Errorf("EffectiveMaxRetries = %d, want 1 (one attempt per pr+head)", got)
	}
	// fall_through_to_merge_approval defaults to false (attention path).
	if cfg.Supervisor.ReviewRepair.FallThroughMergeEnabled() {
		t.Error("FallThroughMergeEnabled must default to false")
	}

	// Default policy lists must contain spawn_review_repair so cautious
	// mode gates it and the executor registry mints it.
	wantInAllowed := false
	for _, a := range cfg.Supervisor.AllowedActions {
		if a == SupervisorActionSpawnReviewRepair {
			wantInAllowed = true
			break
		}
	}
	if !wantInAllowed {
		t.Errorf("AllowedActions missing %q: %v", SupervisorActionSpawnReviewRepair, cfg.Supervisor.AllowedActions)
	}
	wantInApprovalRequired := false
	for _, a := range cfg.Supervisor.ApprovalRequiredActions {
		if a == SupervisorActionSpawnReviewRepair {
			wantInApprovalRequired = true
			break
		}
	}
	if !wantInApprovalRequired {
		t.Errorf("ApprovalRequiredActions missing %q: %v", SupervisorActionSpawnReviewRepair, cfg.Supervisor.ApprovalRequiredActions)
	}
}

// Explicit YAML overrides — operators can flip the feature off, swap the
// backend, change the budget, and route to merge_pr instead of attention.
func TestParse_ReviewRepairExplicitOverrides(t *testing.T) {
	yaml := `
repo: owner/repo
supervisor:
  review_repair:
    enabled: false
    backend: codex
    model: opus-4.8
    effort: high
    max_retries: 3
    fall_through_to_merge_approval: true
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Supervisor.ReviewRepair.Active() {
		t.Fatal("enabled=false must disable the feature")
	}
	if got := cfg.Supervisor.ReviewRepair.EffectiveBackend(); got != "codex" {
		t.Errorf("backend = %q, want codex", got)
	}
	if got := cfg.Supervisor.ReviewRepair.Model; got != "opus-4.8" {
		t.Errorf("model = %q, want opus-4.8", got)
	}
	if got := cfg.Supervisor.ReviewRepair.Effort; got != "high" {
		t.Errorf("effort = %q, want high", got)
	}
	if got := cfg.Supervisor.ReviewRepair.EffectiveMaxRetries(); got != 3 {
		t.Errorf("max_retries = %d, want 3", got)
	}
	if !cfg.Supervisor.ReviewRepair.FallThroughMergeEnabled() {
		t.Error("fall_through_to_merge_approval=true must flip the flag")
	}
}
