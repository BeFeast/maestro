package approver

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/config"
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
func seedApproved(t *testing.T, s *approvalstore.Store, id, sha string) {
	t.Helper()
	now := time.Now().UTC()
	a := &state.Approval{
		ID:        id,
		CreatedAt: now,
		UpdatedAt: now,
		Action:    state.ApprovalActionDeployProject,
		Status:    state.ApprovalStatusApproved,
		Repo:      "owner/app",
		Project:   "owner/app",
		Delivery:  &state.DeliveryPayload{Project: "owner/app", Repo: "owner/app", MergedSHA: sha, LocalPath: "/srv/app", Target: "prod"},
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

func fixedRevision(sha string) RevisionChecker {
	return RevisionCheckerFunc(func(dir string) (string, error) { return sha, nil })
}

func newExecutor(s *approvalstore.Store, d config.DeliveryConfig, runner CommandRunner, rev RevisionChecker) *DeliveryExecutor {
	return &DeliveryExecutor{
		Store:    s,
		StateDir: deliveryStateDir,
		Repo:     "owner/app",
		Delivery: d,
		Runner:   runner,
		Revision: rev,
		Actor:    "test",
	}
}

// Happy path: claim → SHA pin ok → run once → verify → executed+verified.
func TestDeliver_HappyPath(t *testing.T) {
	s := openDeliveryStore(t)
	seedApproved(t, s, "approval-deploy-h", "sha-A")
	runner := &recordingRunner{}
	ex := newExecutor(s, config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}, runner, fixedRevision("sha-A"))

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
}

// Exactly-once: two concurrent Deliver calls run the command once.
func TestDeliver_ExactlyOnceUnderContention(t *testing.T) {
	s := openDeliveryStore(t)
	seedApproved(t, s, "approval-deploy-x", "sha-A")
	runner := &recordingRunner{delay: 20 * time.Millisecond}
	ex := newExecutor(s, config.DeliveryConfig{Command: "./deploy.sh"}, runner, fixedRevision("sha-A"))

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
	if runner.count() != 1 {
		t.Fatalf("command ran %d times, want exactly 1", runner.count())
	}
}

// SHA mismatch: never deploy whatever revision is at LocalPath — no command run.
func TestDeliver_RevisionMismatch(t *testing.T) {
	s := openDeliveryStore(t)
	seedApproved(t, s, "approval-deploy-m", "sha-PINNED")
	runner := &recordingRunner{}
	ex := newExecutor(s, config.DeliveryConfig{Command: "./deploy.sh"}, runner, fixedRevision("sha-OTHER"))

	res := ex.Deliver(context.Background(), "approval-deploy-m")
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed", res.Status)
	}
	if runner.count() != 0 {
		t.Fatalf("command ran %d times on revision mismatch, want 0", runner.count())
	}
	if res.Approval.Delivery.ExecutedRevision != "sha-OTHER" {
		t.Fatalf("ExecutedRevision = %q, want the observed checkout", res.Approval.Delivery.ExecutedRevision)
	}
}

// Command failure → execution_failed, verifier never runs.
func TestDeliver_CommandFails(t *testing.T) {
	s := openDeliveryStore(t)
	seedApproved(t, s, "approval-deploy-f", "sha-A")
	runner := &recordingRunner{fail: map[string]error{"./deploy.sh": context.Canceled}}
	ex := newExecutor(s, config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}, runner, fixedRevision("sha-A"))

	res := ex.Deliver(context.Background(), "approval-deploy-f")
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed", res.Status)
	}
	if runner.count() != 1 {
		t.Fatalf("ran %d commands, want 1 (verify must not run after deploy fails)", runner.count())
	}
}

// Verifier failure → execution_failed even though the deploy command succeeded.
func TestDeliver_VerifierFails(t *testing.T) {
	s := openDeliveryStore(t)
	seedApproved(t, s, "approval-deploy-v", "sha-A")
	runner := &recordingRunner{fail: map[string]error{"./verify.sh": context.Canceled}}
	ex := newExecutor(s, config.DeliveryConfig{Command: "./deploy.sh", VerifyCommand: "./verify.sh"}, runner, fixedRevision("sha-A"))

	res := ex.Deliver(context.Background(), "approval-deploy-v")
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed", res.Status)
	}
	got, _ := s.Get(context.Background(), deliveryStateDir, "approval-deploy-v")
	if got.Delivery.Verified {
		t.Fatal("verified must be false when the verifier fails")
	}
}

// Timeout: a slow command hitting the configured timeout is a failure, once.
func TestDeliver_Timeout(t *testing.T) {
	s := openDeliveryStore(t)
	seedApproved(t, s, "approval-deploy-t", "sha-A")
	// bashRunner-backed timeout via the real runner would need a real shell;
	// exercise the timeout contract through the injected runner + a tiny budget.
	runner := &recordingRunner{delay: 200 * time.Millisecond}
	d := config.DeliveryConfig{Command: "sleep", TimeoutMinutes: 0}
	ex := newExecutor(s, d, runner, fixedRevision("sha-A"))
	// Drive the timeout via a canceling parent context to keep the test fast.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	res := ex.Deliver(ctx, "approval-deploy-t")
	if res.Status != state.ApprovalStatusExecutionFailed {
		t.Fatalf("status = %q, want execution_failed on timeout", res.Status)
	}
}

// Crash-after-claim / non-replay recovery: a delivery interrupted mid-flight is
// left in executing, and a later Deliver call does NOT re-run the command.
func TestDeliver_NoReplayAfterClaim(t *testing.T) {
	s := openDeliveryStore(t)
	seedApproved(t, s, "approval-deploy-c", "sha-A")
	ctx := context.Background()
	// Simulate a crash after the durable claim but before FinishDelivery.
	if _, err := s.ClaimExecuting(ctx, deliveryStateDir, "approval-deploy-c", time.Now().UTC(), "crashed", "claim"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	runner := &recordingRunner{}
	ex := newExecutor(s, config.DeliveryConfig{Command: "./deploy.sh"}, runner, fixedRevision("sha-A"))
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
	a := &state.Approval{
		ID: "approval-deploy-p", CreatedAt: now, UpdatedAt: now,
		Action: state.ApprovalActionDeployProject, Status: state.ApprovalStatusPending,
		Repo: "owner/app", Project: "owner/app",
		Delivery: &state.DeliveryPayload{Repo: "owner/app", MergedSHA: "sha-A", LocalPath: "/srv/app"},
	}
	a.PayloadHash = a.ComputePayloadHash()
	if _, err := s.Put(context.Background(), a, approvalstore.RowBinding{Repo: "owner/app", Project: "owner/app", StateDir: deliveryStateDir}); err != nil {
		t.Fatalf("put: %v", err)
	}
	runner := &recordingRunner{}
	ex := newExecutor(s, config.DeliveryConfig{Command: "./deploy.sh"}, runner, fixedRevision("sha-A"))
	res := ex.Deliver(context.Background(), "approval-deploy-p")
	if !res.Skipped || runner.count() != 0 {
		t.Fatalf("pending delivery ran (skipped=%v, runs=%d), want no side effect", res.Skipped, runner.count())
	}
}

// Cross-repo binding is refused (no side effect).
func TestDeliver_RepoGuard(t *testing.T) {
	s := openDeliveryStore(t)
	seedApproved(t, s, "approval-deploy-g", "sha-A")
	runner := &recordingRunner{}
	ex := newExecutor(s, config.DeliveryConfig{Command: "./deploy.sh"}, runner, fixedRevision("sha-A"))
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
