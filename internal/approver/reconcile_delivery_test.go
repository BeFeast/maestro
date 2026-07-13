package approver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func seedInterruptedDelivery(t *testing.T, s *approvalstore.Store, id, sha string, delivery config.DeliveryConfig) *state.Approval {
	t.Helper()
	if delivery.LocalPath == "" {
		delivery.LocalPath = "/srv/app"
	}
	seedApproved(t, s, id, sha, delivery)
	claimed, err := s.ClaimDeliveryExecuting(context.Background(), deliveryStateDir, id, delivery.ApprovalDigest(), time.Now().UTC(), "daemon", "claim")
	if err != nil {
		t.Fatalf("claim interrupted delivery: %v", err)
	}
	return claimed
}

func newReconciler(s *approvalstore.Store, delivery config.DeliveryConfig, runner CommandRunner, checkout CheckoutPreparer) *DeliveryReconciler {
	if delivery.LocalPath == "" {
		delivery.LocalPath = "/srv/app"
	}
	return &DeliveryReconciler{
		Store:    s,
		StateDir: deliveryStateDir,
		Repo:     "owner/app",
		Delivery: delivery,
		Runner:   runner,
		Checkout: checkout,
	}
}

func TestReconcileDeliveryVerifiedRunsOnlyPristineVerifier(t *testing.T) {
	s := openDeliveryStore(t)
	sha := strings.Repeat("a", 40)
	delivery := config.DeliveryConfig{
		Mode: config.DeliveryModeApprovalRequired, Command: "./deploy.sh", VerifyCommand: "./verify.sh", LocalPath: "/srv/app",
	}
	seedInterruptedDelivery(t, s, "reconcile-happy", sha, delivery)
	runner := &recordingRunner{out: map[string]string{"./verify.sh": "token=must-never-persist"}}
	r := newReconciler(s, delivery, runner, fixedCheckout(t, sha))

	res, err := r.Reconcile(context.Background(), DeliveryReconcileRequest{
		ID: "reconcile-happy", Outcome: "verified", ObservedRevision: sha, RunnerGone: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != state.ApprovalStatusExecuted || res.Approval == nil || res.Approval.Delivery == nil ||
		!res.Approval.Delivery.Verified || res.Approval.Delivery.CompletionSource != state.DeliveryCompletionSourceOperatorReconcile ||
		res.Approval.Delivery.ReconcileOutcome != state.DeliveryReconcileOutcomeVerified ||
		res.Approval.Delivery.DeployExitCode != nil {
		t.Fatalf("verified reconcile result = %+v", res)
	}
	if runner.count() != 1 || len(runner.runs) != 1 || runner.runs[0] != "./verify.sh" {
		t.Fatalf("commands run = %v, want only verifier", runner.runs)
	}
	blob, err := json.Marshal(res.Approval)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "must-never-persist") || strings.Contains(string(blob), "deploy.sh") || strings.Contains(string(blob), "verify.sh") {
		t.Fatalf("command/output leaked into authoritative record: %s", blob)
	}
}

func TestReconcileDeliveryMissingAssertionsLeaveLease(t *testing.T) {
	sha := strings.Repeat("a", 40)
	delivery := config.DeliveryConfig{Mode: config.DeliveryModeApprovalRequired, Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
	tests := []struct {
		name string
		req  DeliveryReconcileRequest
		want error
	}{
		{name: "verified runner", req: DeliveryReconcileRequest{Outcome: "verified", ObservedRevision: sha}, want: ErrDeliveryReconcileRunnerGoneRequired},
		{name: "not applied runner", req: DeliveryReconcileRequest{Outcome: "not-applied", TargetSafe: true}, want: ErrDeliveryReconcileRunnerGoneRequired},
		{name: "not applied target", req: DeliveryReconcileRequest{Outcome: "not-applied", RunnerGone: true}, want: ErrDeliveryReconcileTargetSafeRequired},
		{name: "remediated target", req: DeliveryReconcileRequest{Outcome: "remediated-failed", RunnerGone: true}, want: ErrDeliveryReconcileTargetSafeRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openDeliveryStore(t)
			id := "reconcile-assert-" + strings.ReplaceAll(tt.name, " ", "-")
			seedInterruptedDelivery(t, s, id, sha, delivery)
			tt.req.ID = id
			r := newReconciler(s, delivery, &recordingRunner{}, fixedCheckout(t, sha))
			if _, err := r.Reconcile(context.Background(), tt.req); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			got, err := s.Get(context.Background(), deliveryStateDir, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != state.ApprovalStatusExecuting {
				t.Fatalf("status = %q, want executing", got.Status)
			}
		})
	}
}

func TestReconcileDeliveryWrongRevisionConfigDriftAndFailedVerifierLeaveLease(t *testing.T) {
	sha := strings.Repeat("a", 40)
	delivery := config.DeliveryConfig{Mode: config.DeliveryModeApprovalRequired, Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
	tests := []struct {
		name       string
		observed   string
		mutate     func(config.DeliveryConfig) config.DeliveryConfig
		runnerFail bool
		want       error
		wantRuns   int
	}{
		{name: "wrong revision", observed: strings.Repeat("b", 40), want: ErrDeliveryReconcileRevisionMismatch},
		{name: "config drift", observed: sha, mutate: func(d config.DeliveryConfig) config.DeliveryConfig { d.VerifyCommand = "./verify-v2.sh"; return d }, want: ErrDeliveryConfigMismatch},
		{name: "failed verifier", observed: sha, runnerFail: true, want: ErrDeliveryVerificationFailed, wantRuns: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openDeliveryStore(t)
			id := "reconcile-fence-" + strings.ReplaceAll(tt.name, " ", "-")
			seedInterruptedDelivery(t, s, id, sha, delivery)
			live := delivery
			if tt.mutate != nil {
				live = tt.mutate(live)
			}
			runner := &recordingRunner{}
			if tt.runnerFail {
				runner.fail = map[string]error{"./verify.sh": errors.New("secret-bearing verifier failure")}
			}
			r := newReconciler(s, live, runner, fixedCheckout(t, sha))
			_, err := r.Reconcile(context.Background(), DeliveryReconcileRequest{
				ID: id, Outcome: "verified", ObservedRevision: tt.observed, RunnerGone: true,
			})
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if runner.count() != tt.wantRuns {
				t.Fatalf("runner count = %d, want %d", runner.count(), tt.wantRuns)
			}
			got, err := s.Get(context.Background(), deliveryStateDir, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != state.ApprovalStatusExecuting || got.Delivery.CompletionSource != "" {
				t.Fatalf("failed recovery changed lease: %+v", got)
			}
		})
	}
}

func TestReconcileDeliveryNegativeOutcomesDoNotCheckoutOrRun(t *testing.T) {
	for _, outcome := range []string{"not-applied", "remediated-failed"} {
		t.Run(outcome, func(t *testing.T) {
			s := openDeliveryStore(t)
			sha := strings.Repeat("a", 40)
			delivery := config.DeliveryConfig{Mode: config.DeliveryModeApprovalRequired, Command: "./deploy.sh", VerifyCommand: "./verify.sh"}
			id := "reconcile-negative-" + outcome
			seedInterruptedDelivery(t, s, id, sha, delivery)
			runner := &recordingRunner{}
			var checkouts atomic.Int64
			checkout := CheckoutPreparerFunc(func(context.Context, string, string) (*PreparedCheckout, error) {
				checkouts.Add(1)
				return nil, errors.New("must not materialize")
			})
			r := newReconciler(s, delivery, runner, checkout)
			res, err := r.Reconcile(context.Background(), DeliveryReconcileRequest{
				ID: id, Outcome: outcome, RunnerGone: true, TargetSafe: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != state.ApprovalStatusExecutionFailed || res.Approval.Delivery.ReconcileOutcome == "" ||
				res.Approval.Delivery.CompletionSource != state.DeliveryCompletionSourceOperatorReconcile {
				t.Fatalf("negative result = %+v", res)
			}
			if runner.count() != 0 || checkouts.Load() != 0 {
				t.Fatalf("negative reconciliation ran runner=%d checkout=%d", runner.count(), checkouts.Load())
			}
		})
	}
}

func TestParseDeliveryReconcileOutcomeRejectsUnknown(t *testing.T) {
	for _, raw := range []string{"", "unknown", "not_applied", "verified-now"} {
		if _, err := ParseDeliveryReconcileOutcome(raw); !errors.Is(err, approvalstore.ErrDeliveryReconcileOutcome) {
			t.Fatalf("ParseDeliveryReconcileOutcome(%q) err = %v", raw, err)
		}
	}
}
