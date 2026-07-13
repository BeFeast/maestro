package approver

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

const deliveryStateDir = "/tmp/delivery-sd"

func openDeliveryStore(t *testing.T) *approvalstore.Store {
	t.Helper()
	s, err := approvalstore.Open(filepath.Join(t.TempDir(), "maestro.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedApproved seeds an approved deploy_project approval pinned to sha.
func seedApproved(t *testing.T, s *approvalstore.Store, id, sha string, delivery config.DeliveryConfig) {
	t.Helper()
	if delivery.LocalPath == "" {
		delivery.LocalPath = "/srv/app"
	}
	now := time.Now().UTC()
	a := &state.Approval{
		ID:        id,
		CreatedAt: now,
		UpdatedAt: now,
		Action:    state.ApprovalActionDeployProject,
		Status:    state.ApprovalStatusApproved,
		Repo:      "owner/app",
		Project:   "owner/app",
		Delivery: &state.DeliveryPayload{
			Project:           "owner/app",
			Repo:              "owner/app",
			MergedSHA:         sha,
			MergedAt:          time.Date(2026, 7, 13, 10, 9, 0, 0, time.UTC),
			TargetLabel:       "test target",
			VerificationLabel: "test verifier",
			RollbackLabel:     "none: test fixture",
			ConfigDigest:      delivery.ApprovalDigest(),
		},
	}
	a.PayloadHash = a.ComputePayloadHash()
	b := approvalstore.RowBinding{Project: "owner/app", Repo: "owner/app", StateDir: deliveryStateDir}
	if _, err := s.Put(context.Background(), a, b); err != nil {
		t.Fatalf("put: %v", err)
	}
}

// recordingRunner records the commands it runs and returns scripted outcomes.
type recordingRunner struct {
	mu    sync.Mutex
	runs  []string
	fail  map[string]error // command → error to return
	out   map[string]string
	delay time.Duration
}

func (r *recordingRunner) Run(ctx context.Context, dir, command string) (string, error) {
	if r.delay > 0 {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(r.delay):
		}
	}
	r.mu.Lock()
	r.runs = append(r.runs, command)
	r.mu.Unlock()
	return r.out[command], r.fail[command]
}

func (r *recordingRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}

func fixedCheckout(t *testing.T, sha string) CheckoutPreparer {
	t.Helper()
	root := t.TempDir()
	return CheckoutPreparerFunc(func(_ context.Context, _, _ string) (*PreparedCheckout, error) {
		dir, err := os.MkdirTemp(root, "checkout-")
		if err != nil {
			return nil, err
		}
		for _, name := range []string{"deploy.sh", "verify.sh"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				return nil, err
			}
		}
		return &PreparedCheckout{Dir: dir, Revision: sha, Cleanup: func() error { return os.RemoveAll(dir) }}, nil
	})
}

func newExecutor(s *approvalstore.Store, d config.DeliveryConfig, runner CommandRunner, checkout CheckoutPreparer) *DeliveryExecutor {
	if d.LocalPath == "" {
		d.LocalPath = "/srv/app"
	}
	return &DeliveryExecutor{
		Store:    s,
		StateDir: deliveryStateDir,
		Repo:     "owner/app",
		Delivery: d,
		Runner:   runner,
		Checkout: checkout,
		Actor:    "test",
		Freshness: DeliveryFreshnessFunc(func(context.Context, *state.DeliveryPayload) error {
			return nil
		}),
	}
}

// Happy path: claim → SHA pin ok → run once → verify → executed+verified.
func TestDeliver_HappyPath(t *testing.T) {
	s := openDeliveryStore(t)
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
	seedApproved(t, s, "approval-deploy-h", "sha-A", delivery)
	runner := &recordingRunner{}
	ex := newExecutor(s, delivery, runner, fixedCheckout(t, "sha-A"))

	res := ex.Deliver(context.Background(), "approval-deploy-h")
	if res.Status != state.ApprovalStatusExecuted {
		t.Fatalf("status = %q (%v), want executed", res.Status, res.Err)
	}
	if runner.count() != 2 {
		t.Fatalf("ran %d commands, want 2 (deploy + verify)", runner.count())
	}
	if res.Approval.Delivery == nil || !res.Approval.Delivery.Verified {
		t.Fatalf("delivery not verified: %+v", res.Approval.Delivery)
	}
	if code := res.Approval.Delivery.DeployExitCode; code == nil || *code != 0 {
		t.Fatalf("deploy exit code = %v, want 0", code)
	}
	if code := res.Approval.Delivery.VerifyExitCode; code == nil || *code != 0 {
		t.Fatalf("verify exit code = %v, want 0", code)
	}
	if res.Approval.Delivery.FailureStage != "" || res.Approval.Delivery.TimedOut || res.Approval.Delivery.CleanupFailed {
		t.Fatalf("unexpected structured failure metadata: %+v", res.Approval.Delivery)
	}
}

// Exactly-once: two concurrent Deliver calls run the command once.
func TestDeliver_ExactlyOnceUnderContention(t *testing.T) {
	s := openDeliveryStore(t)
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
	seedApproved(t, s, "approval-deploy-x", "sha-A", delivery)
	runner := &recordingRunner{delay: 20 * time.Millisecond}
	ex := newExecutor(s, delivery, runner, fixedCheckout(t, "sha-A"))

	var executed int64
	var skipped int64
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := ex.Deliver(context.Background(), "approval-deploy-x")
			switch {
			case res.Status == state.ApprovalStatusExecuted:
				atomic.AddInt64(&executed, 1)
			case res.Skipped:
				atomic.AddInt64(&skipped, 1)
			}
		}()
	}
	wg.Wait()
	if executed != 1 {
		t.Fatalf("executed = %d, want exactly 1", executed)
	}
	if runner.count() != 2 {
		t.Fatalf("commands ran %d times, want exactly 2 (one deploy + one verify)", runner.count())
	}
}

// SHA mismatch: never deploy whatever revision is at LocalPath — no command run.
func TestDeliver_RevisionMismatch(t *testing.T) {
	s := openDeliveryStore(t)
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
	seedApproved(t, s, "approval-deploy-m", "sha-PINNED", delivery)
	runner := &recordingRunner{}
	ex := newExecutor(s, delivery, runner, fixedCheckout(t, "sha-OTHER"))

	res := ex.Deliver(context.Background(), "approval-deploy-m")
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed", res.Status)
	}
	if runner.count() != 0 {
		t.Fatalf("command ran %d times on revision mismatch, want 0", runner.count())
	}
	if res.Approval.Delivery.ExecutedRevision != "sha-other" {
		t.Fatalf("ExecutedRevision = %q, want the observed checkout", res.Approval.Delivery.ExecutedRevision)
	}
	if res.Approval.Delivery.FailureStage != state.DeliveryFailureStageCheckout ||
		res.Approval.Delivery.DeployExitCode != nil || res.Approval.Delivery.VerifyExitCode != nil {
		t.Fatalf("revision mismatch metadata = %+v", res.Approval.Delivery)
	}
}

// Command failure → execution_failed, verifier never runs.
func TestDeliver_CommandFails(t *testing.T) {
	s := openDeliveryStore(t)
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
	seedApproved(t, s, "approval-deploy-f", "sha-A", delivery)
	runner := &recordingRunner{fail: map[string]error{"./deploy.sh": context.Canceled}}
	ex := newExecutor(s, delivery, runner, fixedCheckout(t, "sha-A"))

	res := ex.Deliver(context.Background(), "approval-deploy-f")
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed", res.Status)
	}
	if runner.count() != 1 {
		t.Fatalf("ran %d commands, want 1 (verify must not run after deploy fails)", runner.count())
	}
	if res.Approval.Delivery.FailureStage != state.DeliveryFailureStageDeploy ||
		res.Approval.Delivery.DeployExitCode == nil || *res.Approval.Delivery.DeployExitCode != -1 ||
		res.Approval.Delivery.VerifyExitCode != nil {
		t.Fatalf("command failure metadata = %+v", res.Approval.Delivery)
	}
}

// Verifier failure → execution_failed even though the deploy command succeeded.
func TestDeliver_VerifierFails(t *testing.T) {
	s := openDeliveryStore(t)
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
	seedApproved(t, s, "approval-deploy-v", "sha-A", delivery)
	runner := &recordingRunner{fail: map[string]error{"./verify.sh": context.Canceled}}
	ex := newExecutor(s, delivery, runner, fixedCheckout(t, "sha-A"))

	res := ex.Deliver(context.Background(), "approval-deploy-v")
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed", res.Status)
	}
	got, _ := s.Get(context.Background(), deliveryStateDir, "approval-deploy-v")
	if got.Delivery.Verified {
		t.Fatal("verified must be false when the verifier fails")
	}
	if got.Delivery.FailureStage != state.DeliveryFailureStageVerify ||
		got.Delivery.DeployExitCode == nil || *got.Delivery.DeployExitCode != 0 ||
		got.Delivery.VerifyExitCode == nil || *got.Delivery.VerifyExitCode != -1 {
		t.Fatalf("verifier failure metadata = %+v", got.Delivery)
	}
}

// Timeout: a slow command hitting the configured timeout is a failure, once.
func TestDeliver_Timeout(t *testing.T) {
	s := openDeliveryStore(t)
	d := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh", TimeoutMinutes: 0}
	seedApproved(t, s, "approval-deploy-t", "sha-A", d)
	// Return the runner's deadline sentinel directly. A tiny parent deadline is
	// no longer a valid fixture: it may expire during the (correctly retryable)
	// pre-side-effect freshness/materialization stages under -race.
	var runs atomic.Int64
	runner := CommandRunnerFunc(func(context.Context, string, string) (string, error) {
		runs.Add(1)
		return "", context.DeadlineExceeded
	})
	ex := newExecutor(s, d, runner, fixedCheckout(t, "sha-A"))
	res := ex.Deliver(context.Background(), "approval-deploy-t")
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed on timeout", res.Status)
	}
	if res.Approval == nil || res.Approval.Delivery == nil || res.Approval.Delivery.FailureStage != state.DeliveryFailureStageDeploy || !res.Approval.Delivery.TimedOut {
		t.Fatalf("timeout approval = %+v", res.Approval)
	}
	if runs.Load() != 1 {
		t.Fatalf("runner calls = %d, want deploy only", runs.Load())
	}
}

// Crash-after-claim / non-replay recovery: a delivery interrupted mid-flight is
// left in executing, and a later Deliver call does NOT re-run the command.
func TestDeliver_NoReplayAfterClaim(t *testing.T) {
	s := openDeliveryStore(t)
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
	seedApproved(t, s, "approval-deploy-c", "sha-A", delivery)
	ctx := context.Background()
	// Simulate a crash after the durable claim but before FinishDelivery.
	if _, err := s.ClaimExecuting(ctx, deliveryStateDir, "approval-deploy-c", time.Now().UTC(), "crashed", "claim"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	runner := &recordingRunner{}
	ex := newExecutor(s, delivery, runner, fixedCheckout(t, "sha-A"))
	res := ex.Deliver(ctx, "approval-deploy-c")
	if !res.Skipped {
		t.Fatalf("expected skipped (no replay), got status %q", res.Status)
	}
	if runner.count() != 0 {
		t.Fatalf("command ran %d times after crash-claim, want 0 (no replay)", runner.count())
	}
	got, _ := s.Get(ctx, deliveryStateDir, "approval-deploy-c")
	if got.Status != state.ApprovalStatusExecuting {
		t.Fatalf("status = %q, want executing (operator reconcile)", got.Status)
	}
}

// A non-delivery / pending approval is refused without a claim or side effect.
func TestDeliver_RefusesNonApproved(t *testing.T) {
	s := openDeliveryStore(t)
	// pending (never approved)
	now := time.Now().UTC()
	sha := strings.Repeat("a", 40)
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh", LocalPath: "/srv/app"}
	a := &state.Approval{
		ID: "approval-deploy-p", CreatedAt: now, UpdatedAt: now,
		Action: state.ApprovalActionDeployProject, Status: state.ApprovalStatusPending,
		Repo: "owner/app", Project: "owner/app",
		Delivery: &state.DeliveryPayload{
			Project: "owner/app", Repo: "owner/app", MergedSHA: sha, MergedAt: now,
			TargetLabel: "test target", VerificationLabel: "test verifier", RollbackLabel: "none: test fixture",
			ConfigDigest: delivery.ApprovalDigest(),
		},
	}
	a.PayloadHash = a.ComputePayloadHash()
	if _, err := s.Put(context.Background(), a, approvalstore.RowBinding{Repo: "owner/app", Project: "owner/app", StateDir: deliveryStateDir}); err != nil {
		t.Fatalf("put: %v", err)
	}
	runner := &recordingRunner{}
	ex := newExecutor(s, delivery, runner, fixedCheckout(t, sha))
	res := ex.Deliver(context.Background(), "approval-deploy-p")
	if !res.Skipped || runner.count() != 0 {
		t.Fatalf("pending delivery ran (skipped=%v, runs=%d), want no side effect", res.Skipped, runner.count())
	}
}

// Cross-repo binding is refused (no side effect).
func TestDeliver_RepoGuard(t *testing.T) {
	s := openDeliveryStore(t)
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh", LocalPath: "/srv/app"}
	seedApproved(t, s, "approval-deploy-g", "sha-A", delivery)
	runner := &recordingRunner{}
	ex := newExecutor(s, delivery, runner, fixedCheckout(t, "sha-A"))
	ex.Repo = "other/repo"
	res := ex.Deliver(context.Background(), "approval-deploy-g")
	if !res.Skipped || runner.count() != 0 {
		t.Fatalf("cross-repo delivery ran, want refusal")
	}
	got, _ := s.Get(context.Background(), deliveryStateDir, "approval-deploy-g")
	if got.Status != state.ApprovalStatusApproved {
		t.Fatalf("status = %q, want unchanged approved (no claim consumed)", got.Status)
	}
}

func TestCheckLatestDeliveryGenerations_SameSecondUsesAncestryNotPROrder(t *testing.T) {
	mergedAt := time.Date(2026, 7, 13, 10, 9, 0, 0, time.UTC)
	first := strings.Repeat("a", 40)  // PR #101 merged first
	second := strings.Repeat("b", 40) // PR #100 merged second, despite lower number
	approved := &state.DeliveryPayload{MergedSHA: first, MergedAt: mergedAt}
	latest := []github.PRMergeInfo{{SHA: first, MergedAt: mergedAt}, {SHA: second, MergedAt: mergedAt}}

	err := checkLatestDeliveryGenerations(context.Background(), approved, latest, func(_ context.Context, ancestor, descendant string) (bool, error) {
		return ancestor == first && descendant == second, nil
	})
	if !errors.Is(err, ErrDeliverySuperseded) {
		t.Fatalf("same-second descendant check err = %v, want ErrDeliverySuperseded", err)
	}
}

func TestCheckLatestDeliveryGenerations_SameSecondIncomparableFailsClosed(t *testing.T) {
	mergedAt := time.Date(2026, 7, 13, 10, 9, 0, 0, time.UTC)
	approved := &state.DeliveryPayload{MergedSHA: strings.Repeat("a", 40), MergedAt: mergedAt}
	latest := []github.PRMergeInfo{{SHA: strings.Repeat("b", 40), MergedAt: mergedAt}}
	err := checkLatestDeliveryGenerations(context.Background(), approved, latest, func(context.Context, string, string) (bool, error) {
		return false, nil
	})
	if !errors.Is(err, ErrDeliveryGenerationAmbiguous) {
		t.Fatalf("incomparable same-second generations err = %v, want ambiguous", err)
	}
}

func TestDeliver_SameSecondReversedPRsStalesAncestorAndExecutesDescendant(t *testing.T) {
	s := openDeliveryStore(t)
	mergedAt := time.Date(2026, 7, 13, 10, 9, 0, 0, time.UTC)
	ancestor := strings.Repeat("a", 40)   // PR #101 merged first
	descendant := strings.Repeat("b", 40) // PR #100 merged second
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh", LocalPath: "/srv/app"}
	binding := approvalstore.RowBinding{Project: "owner/app", Repo: "owner/app", StateDir: deliveryStateDir}
	for _, fixture := range []struct {
		id  string
		sha string
		pr  int
	}{{"approval-tie-ancestor", ancestor, 101}, {"approval-tie-descendant", descendant, 100}} {
		a := &state.Approval{
			ID: fixture.id, CreatedAt: mergedAt, UpdatedAt: mergedAt,
			Action: state.ApprovalActionDeployProject, Status: state.ApprovalStatusApproved,
			Repo: "owner/app", Project: "owner/app",
			Delivery: &state.DeliveryPayload{
				Project: "owner/app", Repo: "owner/app", PR: fixture.pr,
				MergedSHA: fixture.sha, MergedAt: mergedAt, ConfigDigest: delivery.ApprovalDigest(),
			},
		}
		a.PayloadHash = a.ComputePayloadHash()
		if _, err := s.Put(context.Background(), a, binding); err != nil {
			t.Fatal(err)
		}
	}
	latest := []github.PRMergeInfo{{SHA: ancestor, MergedAt: mergedAt}, {SHA: descendant, MergedAt: mergedAt}}
	freshness := DeliveryFreshnessFunc(func(ctx context.Context, approved *state.DeliveryPayload) error {
		return checkLatestDeliveryGenerations(ctx, approved, latest, func(_ context.Context, a, b string) (bool, error) {
			return a == ancestor && b == descendant, nil
		})
	})
	runner := &recordingRunner{}
	oldExecutor := newExecutor(s, delivery, runner, fixedCheckout(t, ancestor))
	oldExecutor.Freshness = freshness
	oldResult := oldExecutor.Deliver(context.Background(), "approval-tie-ancestor")
	if oldResult.Status != state.ApprovalStatusStale || !oldResult.Skipped {
		t.Fatalf("ancestor result = %+v, want stale without execution", oldResult)
	}
	storedDescendant, err := s.Get(context.Background(), deliveryStateDir, "approval-tie-descendant")
	if err != nil {
		t.Fatal(err)
	}
	if storedDescendant.Delivery.ConfigDigest != delivery.ApprovalDigest() {
		t.Fatalf("fixture digest = %q, executor digest = %q", storedDescendant.Delivery.ConfigDigest, delivery.ApprovalDigest())
	}
	descendantExecutor := newExecutor(s, delivery, runner, fixedCheckout(t, descendant))
	descendantExecutor.Freshness = freshness
	newResult := descendantExecutor.Deliver(context.Background(), "approval-tie-descendant")
	if newResult.Status != state.ApprovalStatusExecuted || newResult.Err != nil {
		t.Fatalf("descendant result = %+v, want executed", newResult)
	}
	if runner.count() != 2 {
		t.Fatalf("commands ran %d times, want descendant deploy + verifier only", runner.count())
	}
}

func TestDeliver_NewerMergeBetweenPrecheckAndClaimTerminalFailsBeforeSideEffect(t *testing.T) {
	s := openDeliveryStore(t)
	sha := strings.Repeat("a", 40)
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
	seedApproved(t, s, "approval-freshness-race", sha, delivery)
	runner := &recordingRunner{}
	ex := newExecutor(s, delivery, runner, fixedCheckout(t, sha))
	var checks atomic.Int64
	ex.Freshness = DeliveryFreshnessFunc(func(context.Context, *state.DeliveryPayload) error {
		if checks.Add(1) == 1 {
			return nil
		}
		return ErrDeliverySuperseded
	})

	res := ex.Deliver(context.Background(), "approval-freshness-race")
	if res.Status != state.ApprovalStatusExecutionFailed || !errors.Is(res.Err, ErrDeliverySuperseded) {
		t.Fatalf("result = %+v, want terminal superseded failure", res)
	}
	if checks.Load() != 2 || runner.count() != 0 {
		t.Fatalf("freshness checks=%d runs=%d, want two checks and zero side effects", checks.Load(), runner.count())
	}
	if res.Approval == nil || res.Approval.Delivery == nil || res.Approval.Delivery.FailureStage != state.DeliveryFailureStagePrecondition {
		t.Fatalf("structured terminal result = %+v", res.Approval)
	}
}

func TestDeliver_NewerMergeDuringCheckoutFailsFinalFenceBeforeSideEffect(t *testing.T) {
	s := openDeliveryStore(t)
	sha := strings.Repeat("a", 40)
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
	seedApproved(t, s, "approval-freshness-checkout-race", sha, delivery)
	runner := &recordingRunner{}
	fallback := fixedCheckout(t, sha)
	var merged atomic.Bool
	checkout := CheckoutPreparerFunc(func(ctx context.Context, sourceDir, revision string) (*PreparedCheckout, error) {
		prepared, err := fallback.Prepare(ctx, sourceDir, revision)
		merged.Store(true) // external/UI merge lands while exact SHA is fetched
		return prepared, err
	})
	ex := newExecutor(s, delivery, runner, checkout)
	var checks atomic.Int64
	ex.Freshness = DeliveryFreshnessFunc(func(context.Context, *state.DeliveryPayload) error {
		checks.Add(1)
		if merged.Load() {
			return ErrDeliverySuperseded
		}
		return nil
	})

	res := ex.Deliver(context.Background(), "approval-freshness-checkout-race")
	if res.Status != state.ApprovalStatusExecutionFailed || !errors.Is(res.Err, ErrDeliverySuperseded) {
		t.Fatalf("result = %+v, want terminal superseded failure", res)
	}
	if checks.Load() != 3 || runner.count() != 0 {
		t.Fatalf("freshness checks=%d runs=%d, want final post-checkout fence and zero side effects", checks.Load(), runner.count())
	}
}

func TestDeliver_PostClaimTransientFreshnessFailureReleasesThenRetriesExactlyOnce(t *testing.T) {
	s := openDeliveryStore(t)
	sha := strings.Repeat("a", 40)
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
	seedApproved(t, s, "approval-freshness-retry", sha, delivery)
	runner := &recordingRunner{}
	ex := newExecutor(s, delivery, runner, fixedCheckout(t, sha))
	var checks atomic.Int64
	ex.Freshness = DeliveryFreshnessFunc(func(context.Context, *state.DeliveryPayload) error {
		if checks.Add(1) == 2 {
			return ErrDeliveryFreshnessUnverified
		}
		return nil
	})

	first := ex.Deliver(context.Background(), "approval-freshness-retry")
	if first.Status != state.ApprovalStatusApproved || !first.Skipped || !errors.Is(first.Err, ErrDeliveryFreshnessUnverified) {
		t.Fatalf("first result = %+v, want released approved retry", first)
	}
	if runner.count() != 0 {
		t.Fatalf("transient freshness failure ran %d commands, want zero", runner.count())
	}
	second := ex.Deliver(context.Background(), "approval-freshness-retry")
	if second.Status != state.ApprovalStatusExecuted || second.Err != nil {
		t.Fatalf("retry result = %+v, want executed", second)
	}
	if runner.count() != 2 {
		t.Fatalf("retry ran %d commands, want one deploy + one verifier", runner.count())
	}
}

func TestDeliver_PostClaimFreshnessCancellationReleasesLease(t *testing.T) {
	s := openDeliveryStore(t)
	sha := strings.Repeat("a", 40)
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
	seedApproved(t, s, "approval-freshness-cancel", sha, delivery)
	runner := &recordingRunner{}
	ex := newExecutor(s, delivery, runner, fixedCheckout(t, sha))
	var checks atomic.Int64
	ex.Freshness = DeliveryFreshnessFunc(func(ctx context.Context, _ *state.DeliveryPayload) error {
		if checks.Add(1) == 1 {
			return nil
		}
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	res := ex.Deliver(ctx, "approval-freshness-cancel")
	if res.Status != state.ApprovalStatusApproved || !res.Skipped || runner.count() != 0 {
		t.Fatalf("cancelled result = %+v runs=%d, want released approved", res, runner.count())
	}
	stored, err := s.Get(context.Background(), deliveryStateDir, "approval-freshness-cancel")
	if err != nil || stored.Status != state.ApprovalStatusApproved {
		t.Fatalf("durable row = %+v err=%v, want approved retryable", stored, err)
	}
}

func TestDeliver_TransientPreDeployCheckoutFailureReleasesThenRetries(t *testing.T) {
	s := openDeliveryStore(t)
	sha := strings.Repeat("a", 40)
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
	seedApproved(t, s, "approval-checkout-retry", sha, delivery)
	runner := &recordingRunner{}
	fallback := fixedCheckout(t, sha)
	var prepares atomic.Int64
	checkout := CheckoutPreparerFunc(func(ctx context.Context, sourceDir, revision string) (*PreparedCheckout, error) {
		if prepares.Add(1) == 1 {
			return nil, errors.New("transient fetch failure")
		}
		return fallback.Prepare(ctx, sourceDir, revision)
	})
	ex := newExecutor(s, delivery, runner, checkout)

	first := ex.Deliver(context.Background(), "approval-checkout-retry")
	if first.Status != state.ApprovalStatusApproved || !first.Skipped || runner.count() != 0 {
		t.Fatalf("first result = %+v runs=%d, want released retry with no side effect", first, runner.count())
	}
	second := ex.Deliver(context.Background(), "approval-checkout-retry")
	if second.Status != state.ApprovalStatusExecuted || second.Err != nil || runner.count() != 2 {
		t.Fatalf("retry result = %+v runs=%d, want one deploy + verifier", second, runner.count())
	}
}

func TestDeliver_NewerMergeBeforeClaimDurablyStalesApproval(t *testing.T) {
	s := openDeliveryStore(t)
	sha := strings.Repeat("a", 40)
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
	seedApproved(t, s, "approval-stale-before-claim", sha, delivery)
	runner := &recordingRunner{}
	ex := newExecutor(s, delivery, runner, fixedCheckout(t, sha))
	ex.Freshness = DeliveryFreshnessFunc(func(context.Context, *state.DeliveryPayload) error { return ErrDeliverySuperseded })

	res := ex.Deliver(context.Background(), "approval-stale-before-claim")
	if res.Status != state.ApprovalStatusStale || !res.Skipped || runner.count() != 0 {
		t.Fatalf("result = %+v runs=%d, want stale with no side effect", res, runner.count())
	}
	stored, err := s.Get(context.Background(), deliveryStateDir, "approval-stale-before-claim")
	if err != nil || stored.Status != state.ApprovalStatusStale {
		t.Fatalf("durable row = %+v err=%v, want stale", stored, err)
	}
}

func TestDeliver_RejectsSymlinkEntrypointOutsideCheckout(t *testing.T) {
	s := openDeliveryStore(t)
	sha := strings.Repeat("a", 40)
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
	seedApproved(t, s, "approval-symlink-entrypoint", sha, delivery)
	outside := filepath.Join(t.TempDir(), "outside.sh")
	marker := filepath.Join(t.TempDir(), "outside-ran")
	writeExecutable(t, outside, "#!/bin/sh\nprintf unsafe > "+shellQuote(marker)+"\n")
	checkout := CheckoutPreparerFunc(func(context.Context, string, string) (*PreparedCheckout, error) {
		dir := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(dir, "deploy.sh")); err != nil {
			return nil, err
		}
		writeExecutable(t, filepath.Join(dir, "verify.sh"), "#!/bin/sh\nexit 0\n")
		return &PreparedCheckout{Dir: dir, Revision: sha, Cleanup: func() error { return nil }}, nil
	})
	ex := newExecutor(s, delivery, nil, checkout)

	res := ex.Deliver(context.Background(), "approval-symlink-entrypoint")
	if res.Status != state.ApprovalStatusExecutionFailed || !errors.Is(res.Err, ErrDeliveryEntrypointUnsafe) {
		t.Fatalf("result = %+v, want unsafe-entrypoint failure", res)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside symlink target executed (stat err=%v)", err)
	}
}

func TestDeliver_VerifierRunsFromSecondPristineCheckout(t *testing.T) {
	s := openDeliveryStore(t)
	sha := strings.Repeat("a", 40)
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
	seedApproved(t, s, "approval-pristine-verifier", sha, delivery)
	var prepares atomic.Int64
	checkout := CheckoutPreparerFunc(func(context.Context, string, string) (*PreparedCheckout, error) {
		dir := t.TempDir()
		call := prepares.Add(1)
		deploy := "#!/bin/sh\ncat > ./verify.sh <<'EOF'\n#!/bin/sh\nexit 0\nEOF\nchmod +x ./verify.sh\nexit 0\n"
		verify := "#!/bin/sh\nexit 1\n"
		if call > 1 {
			deploy = "#!/bin/sh\nexit 0\n"
		}
		writeExecutable(t, filepath.Join(dir, "deploy.sh"), deploy)
		writeExecutable(t, filepath.Join(dir, "verify.sh"), verify)
		return &PreparedCheckout{Dir: dir, Revision: sha, Cleanup: func() error { return nil }}, nil
	})
	ex := newExecutor(s, delivery, nil, checkout)

	res := ex.Deliver(context.Background(), "approval-pristine-verifier")
	if res.Status != state.ApprovalStatusExecutionFailed || !errors.Is(res.Err, ErrDeliveryVerificationFailed) {
		t.Fatalf("result = %+v, want pristine verifier failure", res)
	}
	if prepares.Load() != 2 {
		t.Fatalf("prepared %d checkouts, want deploy + pristine verifier", prepares.Load())
	}
}

func TestDeliver_DirectEntrypointStripsBashEnv(t *testing.T) {
	s := openDeliveryStore(t)
	sha := strings.Repeat("a", 40)
	delivery := config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
	seedApproved(t, s, "approval-sanitized-env", sha, delivery)
	marker := filepath.Join(t.TempDir(), "bash-env-ran")
	hook := filepath.Join(t.TempDir(), "bash-env")
	writeTestFile(t, hook, "printf injected > "+shellQuote(marker)+"\n")
	t.Setenv("BASH_ENV", hook)
	checkout := CheckoutPreparerFunc(func(context.Context, string, string) (*PreparedCheckout, error) {
		dir := t.TempDir()
		writeExecutable(t, filepath.Join(dir, "deploy.sh"), "#!/bin/bash\nexit 0\n")
		writeExecutable(t, filepath.Join(dir, "verify.sh"), "#!/bin/sh\nexit 0\n")
		return &PreparedCheckout{Dir: dir, Revision: sha, Cleanup: func() error { return nil }}, nil
	})
	ex := newExecutor(s, delivery, nil, checkout)

	res := ex.Deliver(context.Background(), "approval-sanitized-env")
	if res.Status != state.ApprovalStatusExecuted || res.Err != nil {
		t.Fatalf("result = %+v, want executed", res)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("BASH_ENV executed before committed entrypoint (stat err=%v)", err)
	}
}

func TestDeliver_DirectEntrypointStripsPythonPathStartupInjection(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required for the interpreter environment canary")
	}
	s := openDeliveryStore(t)
	sha := strings.Repeat("a", 40)
	delivery := config.DeliveryConfig{Command: "./deploy.py", VerifyCommand: "./verify.sh"}
	seedApproved(t, s, "approval-sanitized-python-env", sha, delivery)
	marker := filepath.Join(t.TempDir(), "sitecustomize-ran")
	hostileModules := t.TempDir()
	writeTestFile(t, filepath.Join(hostileModules, "sitecustomize.py"), "from pathlib import Path\nPath("+pythonQuote(marker)+").write_text('injected')\n")
	t.Setenv("PYTHONPATH", hostileModules)
	checkout := CheckoutPreparerFunc(func(context.Context, string, string) (*PreparedCheckout, error) {
		dir := t.TempDir()
		writeExecutable(t, filepath.Join(dir, "deploy.py"), "#!/usr/bin/env python3\nraise SystemExit(0)\n")
		writeExecutable(t, filepath.Join(dir, "verify.sh"), "#!/bin/sh\nexit 0\n")
		return &PreparedCheckout{Dir: dir, Revision: sha, Cleanup: func() error { return nil }}, nil
	})
	ex := newExecutor(s, delivery, nil, checkout)

	res := ex.Deliver(context.Background(), "approval-sanitized-python-env")
	if res.Status != state.ApprovalStatusExecuted || res.Err != nil {
		t.Fatalf("result = %+v, want executed", res)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PYTHONPATH sitecustomize executed before committed entrypoint (stat err=%v)", err)
	}
}

// Real canary: fetch the approved commit from a local bare origin, execute from
// a clean detached isolated checkout at that SHA, leave the authoritative
// checkout (including its untracked operator file) untouched, then reopen the
// SQLite store and prove a terminal delivery cannot replay.
func TestDeliver_RealIsolatedCheckoutAndSQLiteReopenNoReplay(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for the real delivery checkout canary")
	}

	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	source := filepath.Join(root, "source")
	marker := filepath.Join(root, "delivery-runs")
	testGit(t, root, "init", "--bare", origin)
	testGit(t, root, "init", "--initial-branch=main", source)
	testGit(t, source, "config", "user.name", "Maestro Canary")
	testGit(t, source, "config", "user.email", "maestro-canary@example.invalid")

	writeTestFile(t, filepath.Join(source, "artifact.txt"), "baseline\n")
	testGit(t, source, "add", "artifact.txt")
	testGit(t, source, "commit", "-m", "baseline")
	baseline := testGit(t, source, "rev-parse", "HEAD")

	writeTestFile(t, filepath.Join(source, "artifact.txt"), "approved\n")
	writeExecutable(t, filepath.Join(source, "deploy.sh"), "#!/bin/sh\nset -eu\ntest \"$(cat artifact.txt)\" = approved\ntest -z \"$(git symbolic-ref -q HEAD || true)\"\ngit rev-parse HEAD >> \"$MAESTRO_TEST_MARKER\"\n")
	writeExecutable(t, filepath.Join(source, "verify.sh"), "#!/bin/sh\nset -eu\ntest \"$(cat \"$MAESTRO_TEST_MARKER\")\" = \"$(git rev-parse HEAD)\"\n")
	testGit(t, source, "add", "artifact.txt", "deploy.sh", "verify.sh")
	testGit(t, source, "commit", "-m", "approved delivery")
	approved := testGit(t, source, "rev-parse", "HEAD")
	testGit(t, source, "remote", "add", "origin", origin)
	testGit(t, source, "push", "-u", "origin", "main")

	// Deliberately leave LocalPath on a different revision with an untracked
	// operator file. Delivery must neither run from nor modify this worktree.
	testGit(t, source, "checkout", "--detach", baseline)
	writeTestFile(t, filepath.Join(source, "operator-local.txt"), "must survive unchanged\n")
	beforeHead := testGit(t, source, "rev-parse", "HEAD")
	beforeStatus := testGit(t, source, "status", "--porcelain=v1", "--untracked-files=all")

	delivery := config.DeliveryConfig{
		Mode:          config.DeliveryModeApprovalRequired,
		LocalPath:     source,
		Command:       "./deploy.sh",
		VerifyCommand: "./verify.sh",
	}
	t.Setenv("MAESTRO_TEST_MARKER", marker)
	dbPath := filepath.Join(root, "maestro.db")
	store, err := approvalstore.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedApproved(t, store, "approval-real-worktree", approved, delivery)
	ex := newExecutor(store, delivery, nil, nil)
	ex.Checkout = gitIsolatedPreparer{expectedRepo: ex.Repo, fetchURL: origin}
	first := ex.Deliver(context.Background(), "approval-real-worktree")
	if first.Status != state.ApprovalStatusExecuted || first.Err != nil {
		t.Fatalf("first delivery = status %q err %v, want executed", first.Status, first.Err)
	}
	if first.Approval == nil || first.Approval.Delivery == nil || !first.Approval.Delivery.Verified {
		t.Fatalf("real delivery was not verified: %+v", first.Approval)
	}
	if got := first.Approval.Delivery.ExecutedRevision; got != approved {
		t.Fatalf("executed revision = %q, want approved %q", got, approved)
	}
	if code := first.Approval.Delivery.DeployExitCode; code == nil || *code != 0 {
		t.Fatalf("deploy exit code = %v, want 0", code)
	}
	if code := first.Approval.Delivery.VerifyExitCode; code == nil || *code != 0 {
		t.Fatalf("verify exit code = %v, want 0", code)
	}

	if got := testGit(t, source, "rev-parse", "HEAD"); got != beforeHead {
		t.Fatalf("authoritative HEAD changed: got %q want %q", got, beforeHead)
	}
	if got := testGit(t, source, "status", "--porcelain=v1", "--untracked-files=all"); got != beforeStatus {
		t.Fatalf("authoritative worktree changed:\ngot:  %q\nwant: %q", got, beforeStatus)
	}
	if list := testGit(t, source, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Fatalf("temporary worktree was not cleaned up:\n%s", list)
	}
	markerBefore, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if strings.TrimSpace(string(markerBefore)) != approved {
		t.Fatalf("marker = %q, want one run at %q", markerBefore, approved)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close store before reopen: %v", err)
	}
	reopened, err := approvalstore.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	second := newExecutor(reopened, delivery, nil, nil).Deliver(context.Background(), "approval-real-worktree")
	if !second.Skipped {
		t.Fatalf("terminal delivery replayed after SQLite reopen: status=%q err=%v", second.Status, second.Err)
	}
	markerAfter, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker after reopen: %v", err)
	}
	if string(markerAfter) != string(markerBefore) {
		t.Fatalf("delivery marker changed after replay attempt: before=%q after=%q", markerBefore, markerAfter)
	}
}

// A source checkout is mutable operator state and is not part of the approved
// commit. Its common config may therefore contain hostile hooks, filters,
// fsmonitor commands, credential helpers, or URL rewrite helpers. Exact-SHA
// materialization must inherit and execute none of them.
func TestGitIsolatedPreparer_DoesNotExecuteSourceHooksFiltersOrHelpers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for the isolated materialization canary")
	}

	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	source := filepath.Join(root, "source")
	testGit(t, root, "init", "--bare", origin)
	testGit(t, root, "init", "--initial-branch=main", source)
	testGit(t, source, "config", "user.name", "Maestro Adversary")
	testGit(t, source, "config", "user.email", "maestro-adversary@example.invalid")

	writeTestFile(t, filepath.Join(source, ".gitattributes"), "artifact.txt filter=maestro-evil\n")
	writeTestFile(t, filepath.Join(source, "artifact.txt"), "approved bytes\n")
	testGit(t, source, "add", ".gitattributes", "artifact.txt")
	testGit(t, source, "commit", "-m", "approved tree")
	approved := testGit(t, source, "rev-parse", "HEAD")
	testGit(t, source, "remote", "add", "origin", origin)
	testGit(t, source, "push", "origin", "main")

	hookMarker := filepath.Join(root, "post-checkout-ran")
	filterMarker := filepath.Join(root, "filter-ran")
	fsmonitorMarker := filepath.Join(root, "fsmonitor-ran")
	helperMarker := filepath.Join(root, "url-helper-ran")
	hooksDir := filepath.Join(root, "hostile-hooks")
	if err := os.Mkdir(hooksDir, 0o700); err != nil {
		t.Fatalf("mkdir hostile hooks: %v", err)
	}
	writeExecutable(t, filepath.Join(hooksDir, "post-checkout"), "#!/bin/sh\nprintf ran >> "+shellQuote(hookMarker)+"\n")
	filterScript := filepath.Join(root, "hostile-filter")
	writeExecutable(t, filterScript, "#!/bin/sh\nprintf ran >> "+shellQuote(filterMarker)+"\ncat\n")
	fsmonitorScript := filepath.Join(root, "hostile-fsmonitor")
	writeExecutable(t, fsmonitorScript, "#!/bin/sh\nprintf ran >> "+shellQuote(fsmonitorMarker)+"\nprintf '0\\n'\n")
	helperScript := filepath.Join(root, "hostile-remote-helper")
	writeExecutable(t, helperScript, "#!/bin/sh\nprintf ran >> "+shellQuote(helperMarker)+"\nexit 1\n")

	// These are common-repository settings: the old `git worktree add` path
	// would execute both post-checkout and the smudge filter, while its status
	// inspection could execute core.fsmonitor.
	testGit(t, source, "config", "core.hooksPath", hooksDir)
	testGit(t, source, "config", "filter.maestro-evil.clean", filterScript)
	testGit(t, source, "config", "filter.maestro-evil.smudge", filterScript)
	testGit(t, source, "config", "filter.maestro-evil.required", "true")
	testGit(t, source, "config", "core.fsmonitor", fsmonitorScript)
	testGit(t, source, "config", "credential.helper", "!"+helperScript)

	// Also inject a global URL rewrite to the ext helper. The preparer must
	// discard inherited GIT_CONFIG_* state and prohibit the ext protocol.
	hostileGlobal := filepath.Join(root, "hostile-global.gitconfig")
	globalConfig(t, hostileGlobal, "url.ext::"+helperScript+".insteadOf", origin)
	globalConfig(t, hostileGlobal, "protocol.ext.allow", "always")
	t.Setenv("GIT_CONFIG_GLOBAL", hostileGlobal)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", hooksDir)

	prepared, err := (gitIsolatedPreparer{expectedRepo: "owner/app", fetchURL: origin}).Prepare(context.Background(), source, approved)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = prepared.Cleanup() })
	if got, err := os.ReadFile(filepath.Join(prepared.Dir, "artifact.txt")); err != nil || string(got) != "approved bytes\n" {
		t.Fatalf("materialized artifact = %q, err=%v", got, err)
	}
	if head, err := runIsolatedGitText(context.Background(), prepared.Dir, 256, "rev-parse", "HEAD"); err != nil || head != approved {
		t.Fatalf("isolated HEAD = %q, err=%v, want %q", head, err, approved)
	}
	if status, err := runIsolatedGitText(context.Background(), prepared.Dir, 4<<10, "status", "--porcelain=v1", "--untracked-files=all"); err != nil || status != "" {
		t.Fatalf("isolated checkout status = %q, err=%v, want clean", status, err)
	}
	gitConfig, err := os.ReadFile(filepath.Join(prepared.Dir, ".git", "config"))
	if err != nil {
		t.Fatalf("read isolated git config: %v", err)
	}
	if strings.Contains(string(gitConfig), origin) || strings.Contains(strings.ToLower(string(gitConfig)), "credential") {
		t.Fatalf("fetch remote/helper persisted in isolated git config: %q", gitConfig)
	}
	if fetchHead, err := os.ReadFile(filepath.Join(prepared.Dir, ".git", "FETCH_HEAD")); err == nil && len(fetchHead) != 0 {
		t.Fatalf("fetch history persisted despite --no-write-fetch-head: %q", fetchHead)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspect FETCH_HEAD: %v", err)
	}
	for name, marker := range map[string]string{
		"post-checkout hook":    hookMarker,
		"clean/smudge filter":   filterMarker,
		"fsmonitor helper":      fsmonitorMarker,
		"URL/credential helper": helperMarker,
	} {
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("mutable source %s executed (stat err=%v)", name, err)
		}
	}
}

func TestIsolatedGitCommandIgnoresAmbientPathAndLoaderInjection(t *testing.T) {
	if _, ok := trustedSystemExecutable("git"); !ok {
		t.Skip("trusted system git is required")
	}
	root := t.TempDir()
	marker := filepath.Join(root, "fake-git-ran")
	fakeBin := filepath.Join(root, "bin")
	if err := os.Mkdir(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "git"), "#!/bin/sh\nprintf unsafe > "+shellQuote(marker)+"\nexit 0\n")
	t.Setenv("PATH", fakeBin)
	t.Setenv("LD_AUDIT", filepath.Join(root, "hostile-audit.so"))

	out, err := runIsolatedGitText(context.Background(), "", 256, "--version")
	if err != nil || !strings.HasPrefix(out, "git version ") {
		t.Fatalf("trusted git version = %q err=%v", out, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ambient PATH git executed (stat err=%v)", err)
	}
}

func TestTrustedExecutableRejectsUserWritableAllowlistedRoot(t *testing.T) {
	root := t.TempDir()
	writeExecutable(t, filepath.Join(root, "git"), "#!/bin/sh\nexit 0\n")
	if got, ok := trustedExecutableWithinRoots("git", []string{root}); ok {
		t.Fatalf("user-owned executable trusted as %q", got)
	}
}

func TestSanitizedGitEnv_AllowsAuthButDropsTargetCredentials(t *testing.T) {
	env := sanitizedGitEnv([]string{
		"HOME=/home/operator",
		"GH_TOKEN=github-auth",
		"GH_HOST=attacker.invalid",
		"GH_ENTERPRISE_TOKEN=enterprise-secret",
		"HTTPS_PROXY=http://proxy.invalid",
		"LANG=C.UTF-8",
		"AWS_SECRET_ACCESS_KEY=target-secret",
		"ADB_VENDOR_KEYS=/target/adb-keys",
		"DATABASE_URL=postgres://target",
		"LD_AUDIT=/target/inject.so",
		"PATH=/target/bin",
	})
	joined := strings.Join(env, "\n")
	for _, allowed := range []string{"GH_TOKEN=github-auth", "HTTPS_PROXY=http://proxy.invalid", "HOME=/home/operator", "PATH=/usr/bin:"} {
		if !strings.Contains(joined, allowed) {
			t.Fatalf("allowed materializer env %q missing from %q", allowed, joined)
		}
	}
	for _, forbidden := range []string{"AWS_SECRET_ACCESS_KEY", "ADB_VENDOR_KEYS", "DATABASE_URL", "GH_HOST", "GH_ENTERPRISE_TOKEN", "LD_AUDIT", "PATH=/target/bin"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("target/injection env %q reached materializer: %q", forbidden, joined)
		}
	}
}

func TestGitIsolatedPreparer_RejectsMismatchedSourceOriginBeforeFetch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for the remote identity test")
	}
	source := filepath.Join(t.TempDir(), "source")
	testGit(t, filepath.Dir(source), "init", source)
	testGit(t, source, "remote", "add", "origin", "https://github.com/attacker/wrong.git")
	_, err := (gitIsolatedPreparer{expectedRepo: "owner/app"}).Prepare(context.Background(), source, strings.Repeat("a", 40))
	if err == nil || !strings.Contains(err.Error(), "does not match the approved GitHub repository") {
		t.Fatalf("Prepare mismatch error = %v", err)
	}
}

func TestRevisionContains_IgnoresMutableReplaceRefsAndGitEnvironment(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for the isolated ancestry canary")
	}

	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	source := filepath.Join(root, "source")
	testGit(t, root, "init", "--bare", origin)
	testGit(t, root, "init", "--initial-branch=main", source)
	testGit(t, source, "config", "user.name", "Maestro Ancestry")
	testGit(t, source, "config", "user.email", "maestro-ancestry@example.invalid")
	writeTestFile(t, filepath.Join(source, "tree.txt"), "ancestor\n")
	testGit(t, source, "add", "tree.txt")
	testGit(t, source, "commit", "-m", "ancestor")
	ancestor := testGit(t, source, "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(source, "tree.txt"), "descendant\n")
	testGit(t, source, "commit", "-am", "descendant")
	descendant := testGit(t, source, "rev-parse", "HEAD")
	testGit(t, source, "remote", "add", "origin", origin)
	testGit(t, source, "push", "origin", "main")

	// Forge a local view in which the real descendant is replaced by an
	// unrelated root commit. A merge-base check against LocalPath now lies.
	testGit(t, source, "checkout", "--orphan", "unrelated")
	testGit(t, source, "rm", "-rf", ".")
	writeTestFile(t, filepath.Join(source, "unrelated.txt"), "unrelated\n")
	testGit(t, source, "add", "unrelated.txt")
	testGit(t, source, "commit", "-m", "unrelated root")
	unrelated := testGit(t, source, "rev-parse", "HEAD")
	testGit(t, source, "replace", descendant, unrelated)
	localCheck := exec.Command("git", "-C", source, "merge-base", "--is-ancestor", ancestor, descendant)
	if err := localCheck.Run(); err == nil {
		t.Fatal("fixture did not poison the LocalPath ancestry result")
	} else {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("poisoned local merge-base error = %v, want exit 1", err)
		}
	}

	helperMarker := filepath.Join(root, "ancestry-url-helper-ran")
	helperScript := filepath.Join(root, "ancestry-hostile-helper")
	writeExecutable(t, helperScript, "#!/bin/sh\nprintf ran >> "+shellQuote(helperMarker)+"\nexit 1\n")
	hostileGlobal := filepath.Join(root, "ancestry-hostile.gitconfig")
	globalConfig(t, hostileGlobal, "url.ext::"+helperScript+".insteadOf", origin)
	globalConfig(t, hostileGlobal, "protocol.ext.allow", "always")
	t.Setenv("GIT_CONFIG_GLOBAL", hostileGlobal)
	t.Setenv("GIT_REPLACE_REF_BASE", "refs/replace")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.ext.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")

	p := gitIsolatedPreparer{expectedRepo: "owner/app", fetchURL: origin}
	contains, err := revisionContainsFromRemote(context.Background(), p, source, ancestor, descendant)
	if err != nil || !contains {
		t.Fatalf("isolated ancestry = %v, err=%v, want true", contains, err)
	}
	reverse, err := revisionContainsFromRemote(context.Background(), p, source, descendant, ancestor)
	if err != nil || reverse {
		t.Fatalf("reverse isolated ancestry = %v, err=%v, want false", reverse, err)
	}
	if _, err := os.Stat(helperMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mutable URL helper executed during ancestry check (stat err=%v)", err)
	}
}

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", args[0], err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatalf("write executable fixture: %v", err)
	}
}

func globalConfig(t *testing.T, path, key, value string) {
	t.Helper()
	cmd := exec.Command("git", "config", "--file", path, "--add", key, value)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config --file %s: %v\n%s", key, err, out)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func pythonQuote(value string) string { return strconv.Quote(value) }
