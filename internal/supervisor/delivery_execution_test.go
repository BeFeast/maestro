package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/approver"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func TestRunOnceDeliveryUsesConfiguredApprovalsDB(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repoDir, sha := initDeliveryTestRepo(t)
	marker := filepath.Join(t.TempDir(), "delivery-marker")
	t.Setenv("MAESTRO_TEST_MARKER", marker)
	t.Setenv("MAESTRO_TEST_VALUE", "delivered")

	cfg := testConfig(t)
	cfg.LocalPath = repoDir
	cfg.Delivery = config.DeliveryConfig{
		Mode:          config.DeliveryModeApprovalRequired,
		Command:       "./deploy.sh",
		VerifyCommand: "./verify.sh",
		Target:        "test target",
	}
	effective := cfg.EffectiveDelivery()
	now := time.Now().UTC()
	st := state.NewState()
	approval := st.RecordDeliveryApproval(state.DeliveryPayload{
		Project:      cfg.Repo,
		Repo:         cfg.Repo,
		MergedSHA:    sha,
		MergedAt:     now,
		ConfigDigest: effective.ApprovalDigest(),
		ExpiresAt:    now.Add(time.Hour),
	}, now)
	if _, err := st.ApproveApproval(approval.ID, now, "operator", "ship it"); err != nil {
		t.Fatalf("ApproveApproval: %v", err)
	}
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	customDB := filepath.Join(t.TempDir(), "custom-approvals.db")
	seed, err := approvalstore.Open(customDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.PutDelivery(context.Background(), approval, approvalstore.RowBinding{Project: cfg.Repo, Repo: cfg.Repo, StateDir: cfg.StateDir}, now); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	reader := newDeliveryExecutionReader(t, cfg, repoDir, sha, now)
	if _, err := RunOnce(context.Background(), cfg, reader, WithApprovalsDBPath(customDB)); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := loaded.FindApproval(approval.ID)
	if !ok {
		t.Fatalf("approval %s missing", approval.ID)
	}
	if got.Status != state.ApprovalStatusExecuted || got.Delivery == nil || !got.Delivery.Verified {
		t.Fatalf("JSON delivery = status %q payload %+v, want executed+verified", got.Status, got.Delivery)
	}
	if data, err := os.ReadFile(marker); err != nil || strings.TrimSpace(string(data)) != "delivered" {
		t.Fatalf("delivery marker = %q, err=%v", data, err)
	}

	store, err := approvalstore.Open(customDB)
	if err != nil {
		t.Fatalf("open configured approvals DB: %v", err)
	}
	defer store.Close()
	stored, err := store.Get(context.Background(), cfg.StateDir, approval.ID)
	if err != nil {
		t.Fatalf("get configured DB approval: %v", err)
	}
	if stored.Status != state.ApprovalStatusExecuted || stored.Delivery == nil || !stored.Delivery.Verified {
		t.Fatalf("SQLite delivery = status %q payload %+v, want executed+verified", stored.Status, stored.Delivery)
	}
	if _, err := os.Stat(approvalstore.DefaultDBPath()); !os.IsNotExist(err) {
		t.Fatalf("default approvals DB was touched despite custom path: err=%v", err)
	}
}

func TestRunOnceImportsStoreAuthoritativeDeliveryBeforeApprovedScan(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repoDir, sha := initDeliveryTestRepo(t)
	marker := filepath.Join(t.TempDir(), "recovered-delivery-marker")
	t.Setenv("MAESTRO_TEST_MARKER", marker)
	t.Setenv("MAESTRO_TEST_VALUE", "recovered")
	cfg := testConfig(t)
	cfg.LocalPath = repoDir
	cfg.Delivery = config.DeliveryConfig{
		Mode:          config.DeliveryModeApprovalRequired,
		Command:       "./deploy.sh",
		VerifyCommand: "./verify.sh",
		Target:        "test target",
	}
	effective := cfg.EffectiveDelivery()
	now := time.Now().UTC()
	mintState := state.NewState()
	approval := mintState.RecordDeliveryApproval(state.DeliveryPayload{
		Project:      cfg.Repo,
		Repo:         cfg.Repo,
		MergedSHA:    sha,
		MergedAt:     now,
		ConfigDigest: effective.ApprovalDigest(),
		ExpiresAt:    now.Add(time.Hour),
	}, now)

	customDB := filepath.Join(t.TempDir(), "recovery-approvals.db")
	store, err := approvalstore.Open(customDB)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	binding := approvalstore.RowBinding{Project: cfg.Repo, Repo: cfg.Repo, StateDir: cfg.StateDir}
	if _, err := store.PutDelivery(context.Background(), approval, binding, now); err != nil {
		store.Close()
		t.Fatalf("PutDelivery: %v", err)
	}
	if _, err := store.Approve(context.Background(), cfg.StateDir, approval.ID, now, "operator", "ship it"); err != nil {
		store.Close()
		t.Fatalf("Approve: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	// Simulate a crash after the authoritative SQLite approve but before the
	// JSON mirror/save: state.json has no delivery row at all.
	if err := state.Save(cfg.StateDir, state.NewState()); err != nil {
		t.Fatalf("Save empty crash snapshot: %v", err)
	}

	reader := newDeliveryExecutionReader(t, cfg, repoDir, sha, now)
	if _, err := RunOnce(context.Background(), cfg, reader, WithApprovalsDBPath(customDB)); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := loaded.FindApproval(approval.ID)
	if !ok || got.Status != state.ApprovalStatusExecuted || got.Delivery == nil || !got.Delivery.Verified {
		t.Fatalf("recovered delivery = %+v, want executed+verified", got)
	}
	if data, err := os.ReadFile(marker); err != nil || strings.TrimSpace(string(data)) != "recovered" {
		t.Fatalf("delivery marker = %q, err=%v", data, err)
	}
}

func TestReconcileDeliveryApprovalsExpiresAuthoritativeRow(t *testing.T) {
	cfg := testConfig(t)
	cfg.Delivery = config.DeliveryConfig{
		Mode:          config.DeliveryModeApprovalRequired,
		Command:       "./deploy.sh",
		VerifyCommand: "./verify.sh",
	}
	now := time.Now().UTC()
	st := state.NewState()
	approval := st.RecordDeliveryApproval(state.DeliveryPayload{
		Project:      cfg.Repo,
		Repo:         cfg.Repo,
		MergedSHA:    "0123456789abcdef0123456789abcdef01234567",
		MergedAt:     now.Add(-2 * time.Hour),
		ConfigDigest: "expired-digest",
		ExpiresAt:    now.Add(-time.Hour),
	}, now.Add(-2*time.Hour))
	customDB := filepath.Join(t.TempDir(), "expired-approvals.db")
	store, err := approvalstore.Open(customDB)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.PutDelivery(context.Background(), approval, approvalstore.RowBinding{
		Project: cfg.Repo, Repo: cfg.Repo, StateDir: cfg.StateDir,
	}, now.Add(-2*time.Hour)); err != nil {
		store.Close()
		t.Fatalf("PutDelivery: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reconcileDeliveryApprovalsFromStore(cfg, st, customDB)
	got, ok := st.FindApproval(approval.ID)
	if !ok || got.Status != state.ApprovalStatusStale {
		t.Fatalf("JSON approval = %+v, want stale", got)
	}
	store, err = approvalstore.Open(customDB)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()
	stored, err := store.Get(context.Background(), cfg.StateDir, approval.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status != state.ApprovalStatusStale {
		t.Fatalf("SQLite status = %q, want stale", stored.Status)
	}
}

func initDeliveryTestRepo(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	dir := filepath.Join(root, "source")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create source repo: %v", err)
	}
	runDeliveryGit(t, root, "init", "--bare", "-q", remote)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("delivery test\n"), 0o644); err != nil {
		t.Fatalf("write repo fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy.sh"), []byte("#!/bin/sh\nprintf '%s' \"$MAESTRO_TEST_VALUE\" > \"$MAESTRO_TEST_MARKER\"\n"), 0o700); err != nil {
		t.Fatalf("write deploy fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "verify.sh"), []byte("#!/bin/sh\ntest -s \"$MAESTRO_TEST_MARKER\"\n"), 0o700); err != nil {
		t.Fatalf("write verifier fixture: %v", err)
	}
	runDeliveryGit(t, dir, "init", "-q")
	runDeliveryGit(t, dir, "add", "README.md", "deploy.sh", "verify.sh")
	runDeliveryGit(t, dir, "-c", "user.name=Maestro Test", "-c", "user.email=maestro@example.invalid", "commit", "-qm", "fixture")
	runDeliveryGit(t, dir, "remote", "add", "origin", remote)
	runDeliveryGit(t, dir, "push", "-q", "-u", "origin", "HEAD:main")
	sha := strings.TrimSpace(runDeliveryGit(t, dir, "rev-parse", "HEAD"))
	return dir, sha
}

type deliveryExecutionReader struct {
	fakeReader
	latest   []github.PRMergeInfo
	checkout approver.CheckoutPreparer
}

func (r *deliveryExecutionReader) LatestMergedPRGenerations(context.Context) ([]github.PRMergeInfo, error) {
	return r.latest, nil
}

func (r *deliveryExecutionReader) DeliveryCheckoutPreparer() approver.CheckoutPreparer {
	return r.checkout
}

func newDeliveryExecutionReader(t *testing.T, cfg *config.Config, repoDir, sha string, mergedAt time.Time) *deliveryExecutionReader {
	t.Helper()
	origin := strings.TrimSpace(runDeliveryGit(t, repoDir, "remote", "get-url", "origin"))
	return &deliveryExecutionReader{
		latest:   []github.PRMergeInfo{{SHA: sha, MergedAt: mergedAt}},
		checkout: approver.NewLocalFixtureCheckoutPreparer(cfg.Repo, origin),
	}
}

func runDeliveryGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
