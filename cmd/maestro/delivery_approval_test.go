package main

import (
	"context"
	"errors"
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

func TestExecuteApprovedDeliveryCLIUsesConfiguredDBAndMirrorsResult(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := filepath.Join(t.TempDir(), "cli-delivery-marker")
	repoDir, sha := initCLIDeliveryRepo(t, marker)
	previousFreshnessFactory := deliveryFreshnessCheckerFactory
	previousCheckoutFactory := deliveryCheckoutPreparerFactory
	origin := strings.TrimSpace(runCLIDeliveryGit(t, repoDir, "remote", "get-url", "origin"))
	deliveryFreshnessCheckerFactory = func(*config.Config) approver.DeliveryFreshnessChecker {
		return approver.DeliveryFreshnessFunc(func(context.Context, *state.DeliveryPayload) error { return nil })
	}
	deliveryCheckoutPreparerFactory = func(cfg *config.Config) approver.CheckoutPreparer {
		return approver.NewLocalFixtureCheckoutPreparer(cfg.Repo, origin)
	}
	t.Cleanup(func() {
		deliveryFreshnessCheckerFactory = previousFreshnessFactory
		deliveryCheckoutPreparerFactory = previousCheckoutFactory
	})
	cfg := &config.Config{
		Repo:      "owner/app",
		StateDir:  t.TempDir(),
		LocalPath: repoDir,
		Delivery: config.DeliveryConfig{
			Mode:          config.DeliveryModeApprovalRequired,
			Command:       "./deploy.sh",
			VerifyCommand: "./verify.sh",
			Target:        "test target",
		},
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

	customDB := filepath.Join(t.TempDir(), "cli-approvals.db")
	seedStore, err := approvalstore.Open(customDB)
	if err != nil {
		t.Fatalf("open configured approvals DB for seed: %v", err)
	}
	if _, err := seedStore.PutDelivery(context.Background(), approval, approvalstore.RowBinding{Project: cfg.Repo, Repo: cfg.Repo, StateDir: cfg.StateDir}, now); err != nil {
		t.Fatalf("seed configured approvals DB: %v", err)
	}
	if err := seedStore.Close(); err != nil {
		t.Fatalf("close configured approvals DB seed: %v", err)
	}
	res, err := executeApprovedDeliveryCLI(cfg, st, approval, customDB, "cli-test")
	if err != nil {
		t.Fatalf("executeApprovedDeliveryCLI: %v (result=%+v)", err, res)
	}
	if res.Status != state.ApprovalStatusExecuted || res.Approval == nil || res.Approval.Delivery == nil || !res.Approval.Delivery.Verified {
		t.Fatalf("result = %+v, want executed+verified", res)
	}

	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := loaded.FindApproval(approval.ID)
	if !ok || got.Status != state.ApprovalStatusExecuted || got.Delivery == nil || !got.Delivery.Verified {
		t.Fatalf("mirrored JSON approval = %+v, want executed+verified", got)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "x" {
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
		t.Fatalf("stored approval = %+v, want executed+verified", stored)
	}

	// A second claimant converges on the authoritative completed row and must
	// not run the command again.
	secondState, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load second claimant state: %v", err)
	}
	secondApproval, ok := secondState.FindApproval(approval.ID)
	if !ok {
		t.Fatalf("second claimant approval %s missing", approval.ID)
	}
	second, err := executeApprovedDeliveryCLI(cfg, secondState, secondApproval, customDB, "cli-racer")
	if err != nil {
		t.Fatalf("second claimant convergence: %v (result=%+v)", err, second)
	}
	if second.Status != state.ApprovalStatusExecuted || !strings.Contains(second.Summary, "already completed") {
		t.Fatalf("second claimant result = %+v, want converged executed", second)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "x" {
		t.Fatalf("delivery replayed for second claimant: marker=%q err=%v", data, err)
	}
	if _, err := os.Stat(approvalstore.DefaultDBPath()); !os.IsNotExist(err) {
		t.Fatalf("default approvals DB was touched despite custom path: err=%v", err)
	}
}

// Delivery freshness is GitHub-anchored end to end (RevisionContains proves
// ancestry against https://github.com/<repo>.git — the MIRROR of a forgejo
// row). The factory must fail loud on forgejo rows so no delivery ever
// validates Forgejo merge SHAs against a possibly-stale GitHub mirror
// (#1172 M2; forge-aware delivery lands in M3/M4).
func TestDeliveryFreshnessCheckerFactoryFailsLoudOnForgejo(t *testing.T) {
	cfg := &config.Config{
		Repo:  "o/r",
		Forge: config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: "https://forge.example.com"},
	}
	checker := deliveryFreshnessCheckerFactory(cfg)
	if checker == nil {
		t.Fatal("factory must return a fail-loud checker on forgejo rows, not nil")
	}
	err := checker.CheckDeliveryFreshness(context.Background(), &state.DeliveryPayload{})
	if !errors.Is(err, github.ErrForgejoNotSupported) {
		t.Fatalf("forgejo delivery freshness = %v; want errors.Is ErrForgejoNotSupported", err)
	}
}

func initCLIDeliveryRepo(t *testing.T, marker string) (string, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	dir := filepath.Join(root, "source")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create source repo: %v", err)
	}
	runCLIDeliveryGit(t, root, "init", "--bare", "-q", remote)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("cli delivery test\n"), 0o644); err != nil {
		t.Fatalf("write repo fixture: %v", err)
	}
	writeCLIDeliveryExecutable(t, filepath.Join(dir, "deploy.sh"), "#!/bin/sh\nprintf x >> "+shellQuoteCLI(marker)+"\n")
	writeCLIDeliveryExecutable(t, filepath.Join(dir, "verify.sh"), "#!/bin/sh\ntest -s "+shellQuoteCLI(marker)+"\n")
	runCLIDeliveryGit(t, dir, "init", "-q")
	runCLIDeliveryGit(t, dir, "add", "README.md", "deploy.sh", "verify.sh")
	runCLIDeliveryGit(t, dir, "-c", "user.name=Maestro Test", "-c", "user.email=maestro@example.invalid", "commit", "-qm", "fixture")
	runCLIDeliveryGit(t, dir, "remote", "add", "origin", remote)
	runCLIDeliveryGit(t, dir, "push", "-q", "-u", "origin", "HEAD:main")
	return dir, strings.TrimSpace(runCLIDeliveryGit(t, dir, "rev-parse", "HEAD"))
}

func writeCLIDeliveryExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatalf("write executable fixture: %v", err)
	}
}

func shellQuoteCLI(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func runCLIDeliveryGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
