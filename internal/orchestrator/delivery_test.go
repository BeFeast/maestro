package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
)

// deliveryMergeSHA is the fixture merge commit main resolves to after a merge.
const deliveryMergeSHA = "abc123def456abc123def456abc123def456abcd"

func newDeliveryOrch(cfg *config.Config) *Orchestrator {
	return &Orchestrator{
		cfg:           cfg,
		notifier:      &notify.Notifier{},
		mainHeadSHAFn: func() (string, error) { return deliveryMergeSHA, nil },
	}
}

func deployApprovals(s *state.State) []state.Approval {
	var out []state.Approval
	for _, a := range s.Approvals {
		if a.Action == state.ApprovalActionDeployProject {
			out = append(out, a)
		}
	}
	return out
}

// TestDeliverAfterMerge_ApprovalRequiredMintsPending: a merge in
// approval_required mode enqueues exactly one pending, revision-pinned
// deploy_project approval and runs no delivery command (#872).
func TestDeliverAfterMerge_ApprovalRequiredMintsPending(t *testing.T) {
	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: "/tmp/checkout",
		Delivery: config.DeliveryConfig{
			Mode:           config.DeliveryModeApprovalRequired,
			Command:        "make deploy",
			Target:         "prod web",
			Rollback:       "helm rollback web",
			VerifyCommand:  "curl -fsS localhost/health",
			TimeoutMinutes: 10,
		},
		Outcome: outcome.Brief{RequiresDeploy: true},
	}
	o := newDeliveryOrch(cfg)
	s := &state.State{}
	sess := &state.Session{IssueNumber: 42}

	if o.deliverAfterMerge(s, sess, github.PR{Number: 7}) {
		t.Fatalf("approval_required with RequiresDeploy=true must not report deploy succeeded")
	}

	got := deployApprovals(s)
	if len(got) != 1 {
		t.Fatalf("want 1 deploy_project approval, got %d", len(got))
	}
	a := got[0]
	if a.Status != state.ApprovalStatusPending {
		t.Errorf("status = %q, want pending", a.Status)
	}
	if a.Delivery == nil {
		t.Fatal("approval has no delivery payload")
	}
	if a.Delivery.MergedSHA != deliveryMergeSHA {
		t.Errorf("MergedSHA = %q, want the pinned merge commit %q", a.Delivery.MergedSHA, deliveryMergeSHA)
	}
	if a.Delivery.PR != 7 || a.Delivery.Issue != 42 {
		t.Errorf("PR/Issue = %d/%d, want 7/42", a.Delivery.PR, a.Delivery.Issue)
	}
	if a.Delivery.Target != "prod web" || a.Delivery.Rollback != "helm rollback web" {
		t.Errorf("operator-safe context missing: target=%q rollback=%q", a.Delivery.Target, a.Delivery.Rollback)
	}
	if a.Delivery.CommandPreview != "make deploy" {
		t.Errorf("command preview = %q, want a sanitized preview", a.Delivery.CommandPreview)
	}
	if a.Delivery.VerificationPlan == "" {
		t.Error("verification plan should be set when a verifier is configured")
	}
	if a.Delivery.LocalPath != "/tmp/checkout" {
		t.Errorf("local path = %q, want the project checkout", a.Delivery.LocalPath)
	}
}

// TestDeliverAfterMerge_ApprovalRequiredSkipsWhenSHAUnresolvable: without a
// pinnable merge commit the delivery approval is NOT minted — never fall back
// to deploying whatever revision happens to be at local_path (#872 addendum).
func TestDeliverAfterMerge_ApprovalRequiredSkipsWhenSHAUnresolvable(t *testing.T) {
	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: "/tmp/checkout",
		Delivery:  config.DeliveryConfig{Mode: config.DeliveryModeApprovalRequired, Command: "make deploy"},
	}
	o := newDeliveryOrch(cfg)
	o.mainHeadSHAFn = func() (string, error) { return "", fmt.Errorf("no merge commit resolvable") }
	s := &state.State{}

	o.deliverAfterMerge(s, &state.Session{IssueNumber: 5}, github.PR{Number: 3})

	if n := len(deployApprovals(s)); n != 0 {
		t.Fatalf("want 0 delivery approvals when the merge SHA is unresolvable, got %d", n)
	}
}

// TestDeliverAfterMerge_SupersedingMergeInvalidatesStale: a second merge (a new
// revision) supersedes the earlier pending delivery so the stale revision can
// never be approved into a deploy (#872).
func TestDeliverAfterMerge_SupersedingMergeInvalidatesStale(t *testing.T) {
	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: "/tmp/checkout",
		Delivery:  config.DeliveryConfig{Mode: config.DeliveryModeApprovalRequired, Command: "make deploy"},
	}
	o := newDeliveryOrch(cfg)
	s := &state.State{}

	o.deliverAfterMerge(s, &state.Session{IssueNumber: 1}, github.PR{Number: 1})

	// A newer merge advances main to a different commit.
	const newerSHA = "fedcba987654fedcba987654fedcba987654fedc"
	o.mainHeadSHAFn = func() (string, error) { return newerSHA, nil }
	o.deliverAfterMerge(s, &state.Session{IssueNumber: 2}, github.PR{Number: 2})

	var pending, superseded int
	var pendingSHA string
	for _, a := range deployApprovals(s) {
		switch a.Status {
		case state.ApprovalStatusPending:
			pending++
			pendingSHA = a.Delivery.MergedSHA
		case state.ApprovalStatusSuperseded:
			superseded++
		}
	}
	if pending != 1 || superseded != 1 {
		t.Fatalf("want exactly 1 pending + 1 superseded delivery, got pending=%d superseded=%d", pending, superseded)
	}
	if pendingSHA != newerSHA {
		t.Errorf("the live pending delivery pins %q, want the newest merge %q", pendingSHA, newerSHA)
	}
}

// TestDeliverAfterMerge_AutomaticRunsCommand: automatic mode runs the delivery
// command immediately (legacy deploy_cmd parity) and mints no approval.
func TestDeliverAfterMerge_AutomaticRunsCommand(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: dir,
		Delivery: config.DeliveryConfig{
			Mode:           config.DeliveryModeAutomatic,
			Command:        "touch delivered.marker",
			TimeoutMinutes: 1,
		},
	}
	o := newDeliveryOrch(cfg)
	s := &state.State{}

	if !o.deliverAfterMerge(s, &state.Session{}, github.PR{Number: 1}) {
		t.Fatal("automatic delivery should report deploy succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "delivered.marker")); err != nil {
		t.Fatalf("automatic delivery command did not run: %v", err)
	}
	if n := len(deployApprovals(s)); n != 0 {
		t.Fatalf("automatic mode must not mint a deploy_project approval, got %d", n)
	}
}

// TestDeliverAfterMerge_DisabledDoesNothing: disabled delivery runs nothing and
// mints nothing; deploy-success parity with the legacy empty-deploy_cmd path.
func TestDeliverAfterMerge_DisabledDoesNothing(t *testing.T) {
	cfg := &config.Config{
		Repo:      "owner/repo",
		LocalPath: "/tmp/checkout",
		Delivery:  config.DeliveryConfig{Mode: config.DeliveryModeDisabled},
		Outcome:   outcome.Brief{RequiresDeploy: false},
	}
	o := newDeliveryOrch(cfg)
	s := &state.State{}

	if !o.deliverAfterMerge(s, &state.Session{}, github.PR{Number: 1}) {
		t.Fatal("disabled delivery with RequiresDeploy=false must report succeeded (legacy parity)")
	}
	if n := len(deployApprovals(s)); n != 0 {
		t.Fatalf("disabled mode must mint no approvals, got %d", n)
	}
}

// TestDeliverAfterMerge_LegacyDeployCmdIsAutomatic: a legacy deploy_cmd (no
// delivery block) resolves to automatic and runs immediately — no silent
// behavior change for existing fleet projects (#872).
func TestDeliverAfterMerge_LegacyDeployCmdIsAutomatic(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:                 "owner/repo",
		LocalPath:            dir,
		DeployCmd:            "touch legacy.marker",
		DeployTimeoutMinutes: 1,
	}
	o := newDeliveryOrch(cfg)
	s := &state.State{}

	if !o.deliverAfterMerge(s, &state.Session{}, github.PR{Number: 9}) {
		t.Fatal("legacy deploy_cmd should run automatically and report succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "legacy.marker")); err != nil {
		t.Fatalf("legacy deploy_cmd did not run: %v", err)
	}
	if n := len(deployApprovals(s)); n != 0 {
		t.Fatalf("legacy deploy_cmd must not mint an approval, got %d", n)
	}
}
