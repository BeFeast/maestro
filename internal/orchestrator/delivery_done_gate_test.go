package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
)

func approvalGatedTestConfig() *config.Config {
	return &config.Config{
		Repo:      "owner/repo",
		LocalPath: "/tmp/checkout",
		Delivery: config.DeliveryConfig{
			Mode: config.DeliveryModeApprovalRequired, Command: "deploy", VerifyCommand: "verify",
			TargetLabel: "production", VerificationLabel: "health check", RollbackLabel: "rollback release",
		},
	}
}

func TestCanMarkDoneForOutcome_ExternalMergeMustEnterDeliveryGate(t *testing.T) {
	cfg := approvalGatedTestConfig()
	o := newDeliveryOrch(cfg)
	o.isPRMergedFn = func(pr int) (bool, error) { return pr == 7, nil }
	s := state.NewState()
	sess := &state.Session{IssueNumber: 42, PRNumber: 7, Status: state.StatusFailed}

	if o.canMarkDoneForOutcome(s, sess, "external issue close") {
		t.Fatal("merged session passed Done before delivery approval execution")
	}
	if sess.Status != state.StatusCodeLanded {
		t.Fatalf("status = %q, want code_landed", sess.Status)
	}
	approvals := deployApprovals(s)
	if len(approvals) != 1 || approvals[0].Status != state.ApprovalStatusPending {
		t.Fatalf("delivery approvals = %+v, want one pending", approvals)
	}
}

func TestDeliverAfterMerge_ApprovalGateBlocksImmediateHealthyOutcomeEvenWhenDeployNotRequired(t *testing.T) {
	cfg := approvalGatedTestConfig()
	required := true
	cfg.Outcome = outcome.Brief{PassRequiredForDone: &required, RequiresDeploy: false, HealthcheckCommand: "true"}
	o := newDeliveryOrch(cfg)
	s := state.NewState()
	sess := &state.Session{IssueNumber: 42, PRNumber: 7, Status: state.StatusCodeLanded}

	if o.deliverAfterMerge(s, sess, github.PR{Number: 7}) {
		t.Fatal("approval_required reported deploy success while approval is pending")
	}
	if o.shouldVerifyOutcomeAfterMerge(false) {
		t.Fatal("approval gate allowed immediate outcome completion")
	}
	if sess.Status != state.StatusCodeLanded {
		t.Fatalf("status = %q, want code_landed", sess.Status)
	}
}

func TestVerifiedDeliveryCovering_RejectsDescendantFromDifferentConfigDigest(t *testing.T) {
	o := newDeliveryOrch(approvalGatedTestConfig())
	o.deliveryRevisionContainsFn = func(ancestor, descendant string) (bool, error) { return true, nil }
	required := &state.DeliveryPayload{
		Project: "owner/repo", Repo: "owner/repo",
		MergedSHA: "1111111111111111111111111111111111111111", ConfigDigest: "sha256:new",
	}
	s := state.NewState()
	s.Approvals = append(s.Approvals, state.Approval{
		ID: "old-config-descendant", Action: state.ApprovalActionDeployProject,
		Status: state.ApprovalStatusExecuted, CreatedAt: time.Now().UTC(),
		Delivery: &state.DeliveryPayload{
			Project: "owner/repo", Repo: "owner/repo",
			MergedSHA:    "2222222222222222222222222222222222222222",
			ConfigDigest: "sha256:old", Verified: true,
		},
	})
	if got := o.verifiedDeliveryCovering(s, required); got != nil {
		t.Fatalf("different-config delivery covered required generation: %+v", got)
	}
}
