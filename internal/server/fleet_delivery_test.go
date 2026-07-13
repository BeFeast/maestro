package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/state"
)

func TestFleetDeliveryPayloadIsDeepClonedStrictAllowList(t *testing.T) {
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	code := 7
	payload := &state.DeliveryPayload{
		Project: "clock", Repo: "owner/clock", PR: 42,
		MergedSHA:   "0123456789abcdef0123456789abcdef01234567",
		TargetLabel: "production kiosk", VerificationLabel: "service healthy", RollbackLabel: "previous release",
		ExpiresAt: now.Add(24 * time.Hour), FailureStage: state.DeliveryFailureStageDeploy, DeployExitCode: &code,
	}

	safe := safeFleetDeliveryPayload(payload)
	if safe == payload {
		t.Fatal("safeFleetDeliveryPayload returned the source pointer")
	}
	*safe.DeployExitCode = 9
	if *payload.DeployExitCode != 7 {
		t.Fatalf("read-path clone mutated source result metadata: %+v", payload)
	}
}

func TestFleetApprovalSnapshotExposesSafeDeliveryWithoutRawCommand(t *testing.T) {
	now := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	approval := state.Approval{
		ID:        "approval-deploy-clock",
		CreatedAt: now.Add(-time.Minute),
		UpdatedAt: now.Add(-time.Minute),
		Action:    state.ApprovalActionDeployProject,
		Status:    state.ApprovalStatusPending,
		Summary:   "summary opaque-secret",
		Risk:      "risk opaque-secret",
		Target:    &state.SupervisorTarget{PR: 42, Body: "body opaque-secret"},
		Delivery: &state.DeliveryPayload{
			Project: "clock", Repo: "owner/clock", PR: 42,
			MergedSHA:   "0123456789abcdef0123456789abcdef01234567",
			TargetLabel: "production kiosk", VerificationLabel: "service healthy", RollbackLabel: "previous release",
			ExpiresAt: now.Add(24 * time.Hour), ConfigDigest: "sha256:0123",
		},
	}

	got := makeFleetApprovalState(fleetProjectState{Name: "Clock", Repo: "owner/clock"}, &state.State{}, approval, now)
	if got.Delivery == nil || got.Delivery.MergedSHA != approval.Delivery.MergedSHA {
		t.Fatalf("delivery payload missing from Fleet approval: %+v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal Fleet approval: %v", err)
	}
	if strings.Contains(string(encoded), "opaque-secret") {
		t.Fatalf("Fleet approval leaked generic delivery free text: %s", encoded)
	}
	for _, forbidden := range []string{`"command":`, `"command_preview":`, `"output":`, `"exit_error":`, `"local_path":`, `"rollback":`} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("Fleet approval exposed forbidden field %s: %s", forbidden, encoded)
		}
	}
	for _, want := range []string{`"delivery"`, `"merged_sha"`, `"target_label"`, `"expires_at"`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("Fleet approval JSON missing %s: %s", want, encoded)
		}
	}

	html := renderFleetApprovalCard(got, true)
	for _, want := range []string{
		"Deploy project", "Merged revision", approval.Delivery.MergedSHA,
		"Approval expires", "Target label", "production kiosk", "Rollback label",
		"Verification label", "service healthy", "sha256:0123",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("delivery audit card missing %q: %s", want, html)
		}
	}
	if strings.Contains(html, "opaque-secret") {
		t.Fatalf("delivery audit card leaked credential: %s", html)
	}
}

func TestFleetDeliveryAuditExecutingShowsRecoveryAlertAndResult(t *testing.T) {
	approval := fleetApprovalState{
		Action: state.ApprovalActionDeployProject,
		Status: string(state.ApprovalStatusExecuting),
		Delivery: &state.DeliveryPayload{
			MergedSHA:        "fedcba9876543210fedcba9876543210fedcba98",
			ExecutedRevision: "fedcba9876543210fedcba9876543210fedcba98",
			StartedAt:        time.Date(2026, 7, 13, 9, 1, 0, 0, time.UTC),
			FailureStage:     state.DeliveryFailureStageDeploy,
			DeployExitCode:   func() *int { code := 17; return &code }(),
			TimedOut:         true,
		},
	}
	html := renderFleetApprovalCard(approval, true)
	for _, want := range []string{
		"role=\"alert\"", "will not replay it automatically", "Executed revision",
		"Failure stage", "deploy", "Deploy exit code", "17", "Timed out",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("executing delivery audit card missing %q: %s", want, html)
		}
	}
}

func TestFleetDeliveryApprovalUsesEffectSpecificNextActionCTA(t *testing.T) {
	got := fleetNextActionCTAForApproval(&fleetApprovalState{Action: state.ApprovalActionDeployProject})
	if got != "Deploy pinned revision" {
		t.Fatalf("delivery approval CTA = %q, want %q", got, "Deploy pinned revision")
	}
}
