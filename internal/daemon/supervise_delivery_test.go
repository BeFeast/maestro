package daemon

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

type idleDeliveryReader struct {
	latest   []github.PRMergeInfo
	checkout approver.CheckoutPreparer
}

func (idleDeliveryReader) ListOpenIssues([]string) ([]github.Issue, error) { return nil, nil }
func (idleDeliveryReader) ListOpenPRs() ([]github.PR, error)               { return nil, nil }
func (idleDeliveryReader) HasOpenPRForIssue(int) (bool, error)             { return false, nil }
func (idleDeliveryReader) HasMergedPRForIssue(int) (bool, error)           { return false, nil }
func (idleDeliveryReader) IsIssueClosed(int) (bool, error)                 { return false, nil }
func (idleDeliveryReader) IsPRMerged(int) (bool, error)                    { return false, nil }
func (r idleDeliveryReader) LatestMergedPRGenerations(context.Context) ([]github.PRMergeInfo, error) {
	return r.latest, nil
}
func (r idleDeliveryReader) DeliveryCheckoutPreparer() approver.CheckoutPreparer { return r.checkout }

func TestRunSuperviseThreadsConfiguredApprovalsDB(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := filepath.Join(t.TempDir(), "daemon-delivery-marker")
	repoDir, sha := initDaemonDeliveryRepo(t, marker)
	cfg := &config.Config{
		Repo:        "owner/daemon-delivery",
		StateDir:    t.TempDir(),
		LocalPath:   repoDir,
		MaxParallel: 1,
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

	customDB := filepath.Join(t.TempDir(), "daemon-approvals.db")
	seedStore, err := approvalstore.Open(customDB)
	if err != nil {
		t.Fatalf("open configured approvals DB for seed: %v", err)
	}
	if _, err := seedStore.PutDelivery(t.Context(), approval, approvalstore.RowBinding{Project: cfg.Repo, Repo: cfg.Repo, StateDir: cfg.StateDir}, now); err != nil {
		t.Fatalf("seed configured approvals DB: %v", err)
	}
	if err := seedStore.Close(); err != nil {
		t.Fatalf("close configured approvals DB seed: %v", err)
	}
	origin := strings.TrimSpace(runDaemonDeliveryGit(t, repoDir, "remote", "get-url", "origin"))
	reader := idleDeliveryReader{
		latest:   []github.PRMergeInfo{{SHA: sha, MergedAt: now}},
		checkout: approver.NewLocalFixtureCheckoutPreparer(cfg.Repo, origin),
	}
	runSupervise(t.Context(), "daemon-delivery", func() *config.Config { return cfg }, reader, 0, customDB, nil, nil)

	loaded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := loaded.FindApproval(approval.ID)
	if !ok || got.Status != state.ApprovalStatusExecuted || got.Delivery == nil || !got.Delivery.Verified {
		t.Fatalf("daemon delivery = %+v, want executed+verified", got)
	}
	if data, err := os.ReadFile(marker); err != nil || strings.TrimSpace(string(data)) != "daemon-delivered" {
		t.Fatalf("delivery marker = %q, err=%v", data, err)
	}
	if _, err := os.Stat(customDB); err != nil {
		t.Fatalf("configured approvals DB missing: %v", err)
	}
	if _, err := os.Stat(approvalstore.DefaultDBPath()); !os.IsNotExist(err) {
		t.Fatalf("default approvals DB was touched despite daemon custom path: err=%v", err)
	}
}

func initDaemonDeliveryRepo(t *testing.T, marker string) (string, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	dir := filepath.Join(root, "source")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create source repo: %v", err)
	}
	runDaemonDeliveryGit(t, root, "init", "--bare", "-q", remote)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("daemon delivery test\n"), 0o644); err != nil {
		t.Fatalf("write repo fixture: %v", err)
	}
	writeDaemonDeliveryExecutable(t, filepath.Join(dir, "deploy.sh"), "#!/bin/sh\nprintf daemon-delivered > "+shellQuoteDaemon(marker)+"\n")
	writeDaemonDeliveryExecutable(t, filepath.Join(dir, "verify.sh"), "#!/bin/sh\ntest -s "+shellQuoteDaemon(marker)+"\n")
	runDaemonDeliveryGit(t, dir, "init", "-q")
	runDaemonDeliveryGit(t, dir, "add", "README.md", "deploy.sh", "verify.sh")
	runDaemonDeliveryGit(t, dir, "-c", "user.name=Maestro Test", "-c", "user.email=maestro@example.invalid", "commit", "-qm", "fixture")
	runDaemonDeliveryGit(t, dir, "remote", "add", "origin", remote)
	runDaemonDeliveryGit(t, dir, "push", "-q", "-u", "origin", "HEAD:main")
	return dir, strings.TrimSpace(runDaemonDeliveryGit(t, dir, "rev-parse", "HEAD"))
}

func writeDaemonDeliveryExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatalf("write executable fixture: %v", err)
	}
}

func shellQuoteDaemon(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func runDaemonDeliveryGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
