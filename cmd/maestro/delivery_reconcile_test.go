package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/approver"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/configstore"
	"github.com/befeast/maestro/internal/state"
)

func seedCLIInterruptedDelivery(t *testing.T, cfg *config.Config, dbPath, sha string) *state.Approval {
	t.Helper()
	now := time.Now().UTC()
	st := state.NewState()
	a := st.RecordDeliveryApproval(state.DeliveryPayload{
		Project:           cfg.Repo,
		Repo:              cfg.Repo,
		MergedSHA:         sha,
		MergedAt:          now,
		TargetLabel:       "test target",
		VerificationLabel: "fixture verifier",
		RollbackLabel:     "restore fixture",
		ConfigDigest:      cfg.EffectiveDelivery().ApprovalDigest(),
		ExpiresAt:         now.Add(time.Hour),
	}, now)
	if _, err := st.ApproveApproval(a.ID, now, "operator", "approve"); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatal(err)
	}
	store, err := approvalstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.PutDelivery(context.Background(), a, approvalstore.RowBinding{
		Project: cfg.Repo, Repo: cfg.Repo, StateDir: cfg.StateDir,
	}, now); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDeliveryExecuting(context.Background(), cfg.StateDir, a.ID,
		cfg.EffectiveDelivery().ApprovalDigest(), now.Add(time.Second), "daemon", "claim")
	if err != nil {
		t.Fatal(err)
	}
	return claimed
}

func TestExecuteDeliveryReconciliationCLIVerifiesWithoutReplayingDeployAndMirrors(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "already-deployed")
	repoDir, sha := initCLIDeliveryRepo(t, marker)
	if err := os.WriteFile(marker, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Repo: "owner/app", StateDir: t.TempDir(), LocalPath: repoDir,
		Delivery: config.DeliveryConfig{
			Mode: config.DeliveryModeApprovalRequired, Command: "./deploy.sh", VerifyCommand: "./verify.sh",
			TargetLabel: "test target", VerificationLabel: "fixture verifier", RollbackLabel: "restore fixture",
		},
	}
	dbPath := filepath.Join(t.TempDir(), "approvals.db")
	claimed := seedCLIInterruptedDelivery(t, cfg, dbPath, sha)
	origin := strings.TrimSpace(runCLIDeliveryGit(t, repoDir, "remote", "get-url", "origin"))
	previousCheckoutFactory := deliveryCheckoutPreparerFactory
	deliveryCheckoutPreparerFactory = func(cfg *config.Config) approver.CheckoutPreparer {
		return approver.NewLocalFixtureCheckoutPreparer(cfg.Repo, origin)
	}
	t.Cleanup(func() { deliveryCheckoutPreparerFactory = previousCheckoutFactory })

	res, err := executeDeliveryReconciliationCLI(context.Background(), cfg, dbPath, approver.DeliveryReconcileRequest{
		ID: claimed.ID, Outcome: "verified", ObservedRevision: sha, RunnerGone: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != state.ApprovalStatusExecuted || res.Approval == nil || res.Approval.Delivery == nil ||
		res.Approval.Delivery.CompletionSource != state.DeliveryCompletionSourceOperatorReconcile ||
		res.Approval.Delivery.ReconcileOutcome != state.DeliveryReconcileOutcomeVerified || !res.Approval.Delivery.Verified {
		t.Fatalf("reconcile result = %+v", res)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "existing" {
		t.Fatalf("deployment entrypoint replayed: marker=%q err=%v", data, err)
	}
	st, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	mirrored, ok := st.FindApproval(claimed.ID)
	if !ok || mirrored.Status != state.ApprovalStatusExecuted || mirrored.Delivery == nil || !mirrored.Delivery.Verified {
		t.Fatalf("state mirror = %+v", mirrored)
	}
}

func TestExecuteDeliveryReconciliationCLINegativeOutcomeDoesNotMaterialize(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/app", StateDir: t.TempDir(), LocalPath: t.TempDir(),
		Delivery: config.DeliveryConfig{
			Mode: config.DeliveryModeApprovalRequired, Command: "./deploy.sh", VerifyCommand: "./verify.sh",
			TargetLabel: "test target", VerificationLabel: "fixture verifier", RollbackLabel: "restore fixture",
		},
	}
	dbPath := filepath.Join(t.TempDir(), "approvals.db")
	claimed := seedCLIInterruptedDelivery(t, cfg, dbPath, strings.Repeat("a", 40))
	previousCheckoutFactory := deliveryCheckoutPreparerFactory
	var materializations atomic.Int64
	deliveryCheckoutPreparerFactory = func(*config.Config) approver.CheckoutPreparer {
		return approver.CheckoutPreparerFunc(func(context.Context, string, string) (*approver.PreparedCheckout, error) {
			materializations.Add(1)
			return nil, nil
		})
	}
	t.Cleanup(func() { deliveryCheckoutPreparerFactory = previousCheckoutFactory })

	res, err := executeDeliveryReconciliationCLI(context.Background(), cfg, dbPath, approver.DeliveryReconcileRequest{
		ID: claimed.ID, Outcome: "not-applied", RunnerGone: true, TargetSafe: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != state.ApprovalStatusExecutionFailed || res.Approval.Delivery.ReconcileOutcome != state.DeliveryReconcileOutcomeNotApplied {
		t.Fatalf("negative reconcile result = %+v", res)
	}
	if materializations.Load() != 0 {
		t.Fatalf("negative reconciliation materialized %d checkout(s)", materializations.Load())
	}
}

func TestLoadDeliveryReconcileConfigFromStoreIsExplicitAndExact(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "maestro.db")
	store, err := configstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(t.TempDir(), "state")
	localPath := filepath.Join(t.TempDir(), "repo")
	yaml := fmt.Sprintf(`repo: owner/clock
state_dir: %q
local_path: %q
delivery:
  mode: approval_required
  command: "./deploy.sh"
  verify_command: "./verify.sh"
  target_label: "TX10"
  verification_label: "installed revision is healthy"
  rollback_label: "restore previous APK"
`, stateDir, localPath)
	if err := store.UpsertProject(context.Background(), "clock", yaml); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadDeliveryReconcileConfig("", dbPath, "clock")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repo != "owner/clock" || cfg.StateDir != stateDir || cfg.LocalPath != localPath ||
		cfg.EffectiveDelivery().VerifyCommand != "./verify.sh" {
		t.Fatalf("store config = %+v", cfg)
	}
}

func TestLoadDeliveryReconcileConfigRejectsAmbiguousSources(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		store   string
		project string
		want    string
	}{
		{name: "yaml and store", config: "maestro.yaml", store: "maestro.db", want: "mutually exclusive"},
		{name: "project without store", project: "clock", want: "requires --config-store"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadDeliveryReconcileConfig(tt.config, tt.store, tt.project)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}
