package approver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

type forgeGitFixture struct {
	source, base, approved, marker string
	forge                          config.ForgeConfig
	requests                       atomic.Int64
}

// No fetchURL override: this fixture exercises the production canonical URL
// construction and real isolated Git fetch over loopback HTTP.
func newForgeGitFixture(t *testing.T) *forgeGitFixture {
	t.Helper()
	root := t.TempDir()
	f := &forgeGitFixture{source: filepath.Join(root, "source"), marker: filepath.Join(root, "delivery-marker")}
	web := filepath.Join(root, "web")
	origin := filepath.Join(web, "instance", "owner", "app.git")
	if err := os.MkdirAll(filepath.Dir(origin), 0700); err != nil {
		t.Fatal(err)
	}
	testGit(t, root, "init", "--bare", origin)
	testGit(t, root, "init", "--initial-branch=main", f.source)
	testGit(t, f.source, "config", "user.name", "Offline Forge Fixture")
	testGit(t, f.source, "config", "user.email", "fixture@example.invalid")
	writeTestFile(t, filepath.Join(f.source, "artifact.txt"), "baseline\n")
	testGit(t, f.source, "add", "artifact.txt")
	testGit(t, f.source, "commit", "-m", "baseline")
	f.base = testGit(t, f.source, "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(f.source, "artifact.txt"), "approved\n")
	writeExecutable(t, filepath.Join(f.source, "deploy.sh"), "#!/bin/sh\nset -eu\ntest \"$(cat artifact.txt)\" = approved\nprintf x >> \"$MAESTRO_TEST_MARKER\"\nprintf mutated > artifact.txt\nprintf '#!/bin/sh\\nexit 1\\n' > verify.sh\n")
	writeExecutable(t, filepath.Join(f.source, "verify.sh"), "#!/bin/sh\nset -eu\ntest \"$(cat artifact.txt)\" = approved\ntest \"$(cat \"$MAESTRO_TEST_MARKER\")\" = x\n")
	testGit(t, f.source, "add", "artifact.txt", "deploy.sh", "verify.sh")
	testGit(t, f.source, "commit", "-m", "approved delivery")
	f.approved = testGit(t, f.source, "rev-parse", "HEAD")
	testGit(t, f.source, "remote", "add", "origin", origin)
	testGit(t, f.source, "push", "origin", "main")
	testGit(t, origin, "update-server-info")
	files := http.FileServer(http.Dir(web))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.requests.Add(1); files.ServeHTTP(w, r) }))
	t.Cleanup(server.Close)
	f.forge = config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: server.URL + "/instance"}
	testGit(t, f.source, "remote", "set-url", "origin", f.forge.BaseURL+"/owner/app.git")
	testGit(t, f.source, "checkout", "--detach", f.base)
	writeTestFile(t, filepath.Join(f.source, "operator-local.txt"), "untouched\n")
	t.Setenv("MAESTRO_TEST_MARKER", f.marker)
	return f
}

func (f *forgeGitFixture) delivery() config.DeliveryConfig {
	return config.DeliveryConfig{Mode: config.DeliveryModeApprovalRequired, LocalPath: f.source, Forge: f.forge, Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
}

type fixtureMergedGenerations []github.PRMergeInfo

func (r fixtureMergedGenerations) LatestMergedPRGenerations(context.Context) ([]github.PRMergeInfo, error) {
	return r, nil
}

func TestForgejoDeliveryExactSHAAndPristineReconciliation(t *testing.T) {
	f := newForgeGitFixture(t)
	store := openDeliveryStore(t)
	delivery := f.delivery()
	base, err := url.Parse(f.forge.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	// An SSH origin proves repository identity; materialization still uses the
	// configured HTTP instance, with its separate web port and path prefix.
	testGit(t, f.source, "remote", "set-url", "origin", "ssh://git@"+base.Hostname()+":2222/owner/app.git")
	seedApproved(t, store, "forge-delivery", f.approved, delivery)
	var dirs []string
	runner := CommandRunnerFunc(func(ctx context.Context, dir, command string) (string, error) {
		dirs = append(dirs, dir)
		if head := testGit(t, dir, "rev-parse", "HEAD"); head != f.approved {
			t.Fatalf("materialized %s, want %s", head, f.approved)
		}
		return (entrypointRunner{}).Run(ctx, dir, command)
	})
	ex := newExecutor(store, delivery, runner, nil)
	if res := ex.Deliver(context.Background(), "forge-delivery"); res.Err != nil || res.Status != state.ApprovalStatusExecuted || !res.Approval.Delivery.Verified {
		t.Fatalf("Forgejo delivery: %+v", res)
	}
	if len(dirs) != 2 || dirs[0] == dirs[1] || dirs[0] == f.source || dirs[1] == f.source {
		t.Fatalf("deploy/verifier checkouts not isolated: %v", dirs)
	}
	if got := testGit(t, f.source, "rev-parse", "HEAD"); got != f.base {
		t.Fatalf("source HEAD mutated: %s", got)
	}
	if data, err := os.ReadFile(filepath.Join(f.source, "operator-local.txt")); err != nil || string(data) != "untouched\n" {
		t.Fatalf("operator worktree changed: %q, %v", data, err)
	}
	seedInterruptedDelivery(t, store, "forge-reconcile", f.approved, delivery)
	testGit(t, f.source, "remote", "set-url", "origin", f.forge.BaseURL+"/owner/app.git")
	dirs = nil
	reconciler := newReconciler(store, delivery, runner, nil)
	res, err := reconciler.Reconcile(context.Background(), DeliveryReconcileRequest{ID: "forge-reconcile", Outcome: "verified", ObservedRevision: f.approved, RunnerGone: true})
	if err != nil || res.Status != state.ApprovalStatusExecuted || len(dirs) != 1 {
		t.Fatalf("Forgejo verifier-only reconcile: %+v, %v, dirs=%v", res, err, dirs)
	}
	if data, err := os.ReadFile(f.marker); err != nil || string(data) != "x" {
		t.Fatalf("reconciliation replayed deploy: %q, %v", data, err)
	}
	if f.requests.Load() == 0 {
		t.Fatal("canonical HTTP remote was not fetched")
	}
}

func TestForgejoSameSecondFreshnessUsesCanonicalAncestry(t *testing.T) {
	f := newForgeGitFixture(t)
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	latest := fixtureMergedGenerations{{SHA: f.base, MergedAt: at}, {SHA: f.approved, MergedAt: at}}
	checker := NewDeliveryFreshnessChecker(latest, "owner/app", f.source, f.forge)
	if err := checker.CheckDeliveryFreshness(context.Background(), &state.DeliveryPayload{MergedSHA: f.approved, MergedAt: at}); err != nil {
		t.Fatalf("descendant freshness: %v", err)
	}
	if err := checker.CheckDeliveryFreshness(context.Background(), &state.DeliveryPayload{MergedSHA: f.base, MergedAt: at}); !errors.Is(err, ErrDeliverySuperseded) {
		t.Fatalf("ancestor freshness: %v", err)
	}
	// Changing the source to the GitHub mirror must hold before any Git fetch.
	testGit(t, f.source, "remote", "set-url", "origin", "https://github.com/owner/app.git")
	before := f.requests.Load()
	if _, err := RevisionContains(context.Background(), "owner/app", f.source, f.base, f.approved, f.forge); err == nil {
		t.Fatal("mirror accepted for ancestry")
	}
	if f.requests.Load() != before {
		t.Fatal("wrong-origin ancestry fetched")
	}
}

func TestForgejoWrongOriginsRejectBeforeFetchOrCommand(t *testing.T) {
	f := newForgeGitFixture(t)
	root := strings.TrimSuffix(f.forge.BaseURL, "/instance")
	urls := []string{
		"https://github.com/owner/app.git",
		strings.Replace(root, "127.0.0.1", "localhost", 1) + "/instance/owner/app.git",
		strings.Replace(root, "http://", "https://", 1) + "/instance/owner/app.git",
		"http://127.0.0.1:1/instance/owner/app.git",
		root + "/other/owner/app.git", root + "/instance/owner/other.git", root + "/instance/other/app.git",
		strings.Replace(root, "http://", "http://user:secret@", 1) + "/instance/owner/app.git",
		f.forge.BaseURL + "/owner/app.git?token=secret", f.forge.BaseURL + "/owner/app.git#fragment",
	}
	for _, remote := range urls {
		testGit(t, f.source, "remote", "set-url", "origin", remote)
		p := gitIsolatedPreparer{expectedRepo: "owner/app", forge: f.forge}
		if checkout, err := p.Prepare(context.Background(), f.source, f.approved); err == nil {
			if checkout != nil {
				_ = checkout.Cleanup()
			}
			t.Fatalf("wrong origin accepted: %s", remote)
		}
	}
	testGit(t, f.source, "remote", "set-url", "origin", f.forge.BaseURL+"/owner/app.git")
	testGit(t, f.source, "config", "--add", "remote.origin.url", "https://github.com/owner/app.git")
	p := gitIsolatedPreparer{expectedRepo: "owner/app", forge: f.forge}
	if checkout, err := p.Prepare(context.Background(), f.source, f.approved); err == nil {
		if checkout != nil {
			_ = checkout.Cleanup()
		}
		t.Fatal("ambiguous origin accepted")
	}
	testGit(t, f.source, "config", "--unset-all", "remote.origin.url")
	testGit(t, f.source, "remote", "set-url", "origin", "https://github.com/owner/app.git")
	store := openDeliveryStore(t)
	seedApproved(t, store, "wrong-forge", f.approved, f.delivery())
	runner := &recordingRunner{}
	res := newExecutor(store, f.delivery(), runner, nil).Deliver(context.Background(), "wrong-forge")
	if res.Err == nil || runner.count() != 0 || f.requests.Load() != 0 {
		t.Fatalf("mirror escaped source fence: %+v, commands=%d, fetches=%d", res, runner.count(), f.requests.Load())
	}
}

func TestForgeConfigDriftStalesBeforeFreshnessAndPreservesDurableLeases(t *testing.T) {
	sha := strings.Repeat("a", 40)
	old := config.DeliveryConfig{Mode: config.DeliveryModeApprovalRequired, Command: "./deploy.sh", VerifyCommand: "./verify.sh", LocalPath: "/srv/app", Forge: config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: "https://forge.example/instance"}}
	for _, change := range []struct {
		name  string
		forge config.ForgeConfig
	}{
		{"kind", config.ForgeConfig{}},
		{"scheme", config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: "http://forge.example/instance"}},
		{"host", config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: "https://other.example/instance"}},
		{"port", config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: "https://forge.example:8443/instance"}},
		{"prefix", config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: "https://forge.example/other"}},
		{"legacy", old.Forge},
	} {
		t.Run(change.name, func(t *testing.T) {
			original := old
			if change.name == "legacy" {
				original.Forge = config.ForgeConfig{}
			}
			changed := old
			changed.Forge = change.forge
			for _, status := range []state.ApprovalStatus{state.ApprovalStatusPending, state.ApprovalStatusApproved, state.ApprovalStatusExecuting, state.ApprovalStatusExecuted} {
				t.Run(string(status), func(t *testing.T) {
					db := filepath.Join(t.TempDir(), "approvals.db")
					s, err := approvalstore.Open(db)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = s.Close() })
					id := "forge-drift"
					if status == state.ApprovalStatusPending {
						now := time.Now().UTC()
						a := &state.Approval{ID: id, CreatedAt: now, UpdatedAt: now, Action: state.ApprovalActionDeployProject, Status: status, Repo: "owner/app", Project: "owner/app", Delivery: &state.DeliveryPayload{Project: "owner/app", Repo: "owner/app", MergedSHA: sha, MergedAt: now, TargetLabel: "target", VerificationLabel: "verify", RollbackLabel: "none: fixture", ConfigDigest: original.ApprovalDigest()}}
						a.PayloadHash = a.ComputePayloadHash()
						if _, err := s.Put(context.Background(), a, approvalstore.RowBinding{Project: "owner/app", Repo: "owner/app", StateDir: deliveryStateDir}); err != nil {
							t.Fatal(err)
						}
					} else {
						seedApproved(t, s, id, sha, original)
					}
					if status == state.ApprovalStatusExecuting {
						if _, err := s.ClaimDeliveryExecuting(context.Background(), deliveryStateDir, id, original.ApprovalDigest(), time.Now().UTC(), "fixture", "claim"); err != nil {
							t.Fatal(err)
						}
					}
					if status == state.ApprovalStatusExecuted {
						if res := newExecutor(s, original, &recordingRunner{}, fixedCheckout(t, sha)).Deliver(context.Background(), id); res.Err != nil {
							t.Fatal(res.Err)
						}
					}
					before, err := s.Get(context.Background(), deliveryStateDir, id)
					if err != nil {
						t.Fatal(err)
					}
					beforeJSON, _ := json.Marshal(before)
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}
					s, err = approvalstore.Open(db)
					if err != nil {
						t.Fatal(err)
					}
					var effects atomic.Int64
					checkout := CheckoutPreparerFunc(func(context.Context, string, string) (*PreparedCheckout, error) {
						effects.Add(1)
						return nil, errors.New("must not fetch")
					})
					runner := &recordingRunner{}
					ex := newExecutor(s, changed, runner, checkout)
					ex.Freshness = DeliveryFreshnessFunc(func(context.Context, *state.DeliveryPayload) error {
						effects.Add(1)
						return errors.New("must not read ancestry")
					})
					res := ex.Deliver(context.Background(), id)
					if !res.Skipped || effects.Load() != 0 || runner.count() != 0 {
						t.Fatalf("drift caused effects: %+v, %d", res, effects.Load())
					}
					after, err := s.Get(context.Background(), deliveryStateDir, id)
					if err != nil {
						t.Fatal(err)
					}
					if status == state.ApprovalStatusPending || status == state.ApprovalStatusApproved {
						if after.Status != state.ApprovalStatusStale || after.Delivery.StaleCause != state.DeliveryStaleCauseConfigDrift || after.Delivery.ConfigDigest != before.Delivery.ConfigDigest {
							t.Fatalf("old approval was not durably retired intact: %+v", after)
						}
					} else {
						afterJSON, _ := json.Marshal(after)
						if string(beforeJSON) != string(afterJSON) {
							t.Fatalf("durable %s row changed after restart/config drift", status)
						}
						if status == state.ApprovalStatusExecuting {
							_, err := newReconciler(s, changed, runner, checkout).Reconcile(context.Background(), DeliveryReconcileRequest{ID: id, Outcome: "verified", ObservedRevision: sha, RunnerGone: true})
							if !errors.Is(err, ErrDeliveryConfigMismatch) || effects.Load() != 0 || runner.count() != 0 {
								t.Fatalf("reconcile drift escaped: %v", err)
							}
						}
					}
				})
			}
		})
	}
}

func TestForgejoCanonicalFetchDoesNotFollowRedirects(t *testing.T) {
	f := newForgeGitFixture(t)
	mirrorReads := atomic.Int64{}
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorReads.Add(1)
		http.Error(w, "mirror must not be read", 500)
	}))
	defer mirror.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, mirror.URL+r.URL.Path, http.StatusMovedPermanently)
	}))
	defer source.Close()
	f.forge.BaseURL = source.URL
	testGit(t, f.source, "remote", "set-url", "origin", source.URL+"/owner/app.git")
	checkout, err := (gitIsolatedPreparer{expectedRepo: "owner/app", forge: f.forge}).Prepare(context.Background(), f.source, f.approved)
	if checkout != nil {
		_ = checkout.Cleanup()
	}
	if err == nil || mirrorReads.Load() != 0 {
		t.Fatalf("canonical source silently followed mirror redirect: err=%v reads=%d", err, mirrorReads.Load())
	}
}
