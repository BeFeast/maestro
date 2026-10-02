package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/pipeline"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

func TestNativeWorkerLaunchContextUsesPerCallConfig(t *testing.T) {
	cfg := &config.Config{WorkerNativeSessionRegistration: &config.NativeSessionRegistrationConfig{}}
	planner := nativeWorkerConfig(cfg, "planner", "")
	repair := nativeWorkerConfig(cfg, "repair", "parent")
	if cfg.WorkerLaunchContext != nil || planner == repair || planner == cfg || planner.WorkerLaunchContext.Role != "planner" || repair.WorkerLaunchContext.Role != "repair" || repair.WorkerLaunchContext.ParentRoleRunID != "parent" {
		t.Fatal("shared launch context was mutated")
	}
}

func TestNativeWorkerRetryHoldPreservesGenerationBudgetAndFeedback(t *testing.T) {
	cfg := &config.Config{Repo: "fixture/repo", StateDir: t.TempDir(), MaxRetryBackoffMs: 300000, MaxRuntimeMinutes: 999, WorkerNativeSessionRegistration: &config.NativeSessionRegistrationConfig{}}
	calls := 0
	o := &Orchestrator{cfg: cfg, notifier: &notify.Notifier{}, promptBase: "fixture task", getIssueFn: func(n int) (github.Issue, error) { return makeIssue(n, "fixture issue"), nil },
		respawnWorkerFn: func(_ *config.Config, _ string, _ *state.Session, _ string, _ github.Issue, _ string, _ string) error {
			calls++
			return &worker.NativeRegistrationHold{Code: "authority_unavailable"}
		}}
	past := time.Now().UTC().Add(-time.Second)
	s := state.NewState()
	sess := &state.Session{IssueNumber: 1207, Status: state.StatusDead, RetryCount: 2, NextRetryAt: &past, WorkerGeneration: 4, Branch: "fixture", CIFailureOutput: "failure detail", PreviousAttemptFeedback: "review detail", PreviousAttemptFeedbackKind: "review"}
	s.Sessions["fixture-1"] = sess
	o.respawnDueRetries(s, 10)
	if calls != 1 || sess.NativeRegistrationHold != "authority_unavailable" || sess.RetryCount != 2 || sess.WorkerGeneration != 4 || sess.NextRetryAt == nil || !sess.NextRetryAt.Equal(past) || sess.CIFailureOutput != "failure detail" || sess.PreviousAttemptFeedback != "review detail" || sess.Status != state.StatusDead {
		t.Fatalf("hold consumed retry: %+v", sess)
	}
	o.respawnDueRetries(s, 10)
	if calls != 1 || !s.IssueInProgress(1207) {
		t.Fatal("held generation retried or lost issue claim")
	}
}

func TestNativeWorkerPhaseHoldPreservesPriorPhaseAndCounters(t *testing.T) {
	cfg := pipelineConfig()
	cfg.StateDir = t.TempDir()
	cfg.WorkerNativeSessionRegistration = &config.NativeSessionRegistrationConfig{}
	o := pipelineOrchestrator(cfg)
	o.getIssueFn = func(n int) (github.Issue, error) { return makeIssue(n, "fixture issue"), nil }
	dir := t.TempDir()
	for _, name := range []string{pipeline.PlanFile, pipeline.ValidationFile} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	o.workerStartPhaseFn = func(_ *config.Config, s *state.Session, _ string, _ string, _ string) error {
		calls++
		if s.Phase != state.PhaseImplement {
			t.Fatal("wrong trusted role")
		}
		return &worker.NativeRegistrationHold{Code: "registration_conflict"}
	}
	sess := &state.Session{IssueNumber: 1207, Status: state.StatusRunning, Phase: state.PhasePlan, PlanVersion: 1, WorkerGeneration: 3, Worktree: dir}
	if !o.advancePipeline(state.NewState(), "fixture-1", sess) {
		t.Fatal("not handled")
	}
	if sess.Phase != state.PhasePlan || sess.PlanVersion != 1 || sess.WorkerGeneration != 3 || sess.NativeRegistrationHold != "registration_conflict" {
		t.Fatalf("phase hold consumed generation: %+v", sess)
	}
	o.advancePipeline(state.NewState(), "fixture-1", sess)
	if calls != 1 {
		t.Fatal("hold repeated phase")
	}
}

func TestNativeWorkerRepairUsesTrustedCopiedRoleAndRetainsApprovalOnHold(t *testing.T) {
	const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cfg := cfgWithBackends("claude", "claude")
	cfg.WorkerNativeSessionRegistration = &config.NativeSessionRegistrationConfig{}
	o, fresh, _ := newStartWorkersOrchestrator(cfg, []github.Issue{makeIssue(7, "repair exact PR", "maestro-ready")})
	o.hasOpenPRForIssueFn = func(int) (bool, error) { return true, nil }
	o.ghPRHeadSHAFn = func(int) (string, error) { return head, nil }
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		return github.PRCheckRollup{HeadSHA: head, Verdict: "failure", Complete: true}, nil
	}
	o.ghPRMergeStatusFn = func(int) (string, string, error) { return "MERGEABLE", "clean", nil }
	o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) {
		return github.ReviewGateVerdict{Passed: true}, nil
	}
	calls := 0
	o.respawnInPlaceFn = func(c *config.Config, _ string, s *state.Session, _ string, _ github.Issue, _, _ string) error {
		calls++
		if c == cfg || c.WorkerLaunchContext == nil || c.WorkerLaunchContext.Role != "repair" || c.WorkerLaunchContext.ParentRoleRunID != s.NativeRoleRunID {
			t.Fatal("repair lacked isolated trusted role")
		}
		return &worker.NativeRegistrationHold{Code: "unresolved_launch", LaunchUncertain: true}
	}
	st := repairGateTestState(time.Now().UTC(), head)
	st.Sessions["slot-7"].NativeRoleRunID = "00000000-0000-4000-8000-000000000001"
	o.startNewWorkers(st, 1)
	if calls != 1 || len(*fresh) != 0 || st.Sessions["slot-7"].NativeRegistrationHold != "unresolved_launch" || cfg.WorkerLaunchContext != nil {
		t.Fatal("repair hold lost identity")
	}
	if got := approvalStatus(t, st, "repair-7"); got != state.ApprovalStatusAwaitingDispatch {
		t.Fatalf("held repair consumed approval=%s", got)
	}
	o.startNewWorkers(st, 1)
	if calls != 1 {
		t.Fatal("held repair automatically retried")
	}
}

func TestNativeRuntimeReconcileHoldRoutesPrelaunchWedge(t *testing.T) {
	for code, want := range map[string]bool{
		"unresolved_launch":               true,
		"native_process_identity_missing": true,
		"previous_outcome_unknown":        true,
		"native_generation_sealed":        false,
		"setup_failed":                    false,
		"":                                false,
	} {
		if got := nativeRuntimeReconcileHold(code); got != want {
			t.Fatalf("nativeRuntimeReconcileHold(%q)=%v want %v", code, got, want)
		}
	}
}

// parkedHoldMessages captures operator notifications and returns the ones that
// report a parked native hold.
func parkedHoldMessages(t *testing.T) (*notify.Notifier, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var messages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var payload struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &payload)
		mu.Lock()
		messages = append(messages, payload.Message)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return notify.New(server.URL, "fixture-target"), func() []string {
		mu.Lock()
		defer mu.Unlock()
		var parked []string
		for _, m := range messages {
			if strings.Contains(m, "parked on native hold") {
				parked = append(parked, m)
			}
		}
		return parked
	}
}

// A launch-uncertain hold that no reconciliation clears parks the slot: every
// later cycle skips the held session. Before these holds the same receipt
// faults failed the respawn with a notification, so the first retention must
// tell the operator, once, and neither the next cycle nor a repeated retention
// of the same hold may repeat it. This holds for every code the termination
// fence can report for an untrusted projected receipt, including the
// outcome, operator recovery and process evidence checks of the receipt read,
// so the codes come from the worker's list rather than a copy here.
func TestNativeWorkerParkedHoldNotifiesOperatorOnce(t *testing.T) {
	codes := worker.NativeProjectedReceiptHoldCodes()
	for _, want := range []string{"projected_receipt_missing", "projected_receipt_undecodable", "receipt_invalid", "outcome_receipt_invalid", "operator_recovery_receipt_invalid", "native_process_evidence_invalid", "native_identity_conflict"} {
		if !slices.Contains(codes, want) {
			t.Fatalf("termination fence hold %s missing from the parked codes %q", want, codes)
		}
	}
	for _, code := range codes {
		if nativeRuntimeReconcileHold(code) {
			t.Fatalf("parked code %s is routed to cycle-start reconciliation", code)
		}
		t.Run(code, func(t *testing.T) {
			notifier, parked := parkedHoldMessages(t)
			cfg := &config.Config{Repo: "fixture/repo", StateDir: t.TempDir(), MaxRetryBackoffMs: 300000, MaxRuntimeMinutes: 999, WorkerNativeSessionRegistration: &config.NativeSessionRegistrationConfig{}}
			hold := &worker.NativeRegistrationHold{Code: code, LaunchUncertain: true, Slot: "fixture-1"}
			calls := 0
			o := &Orchestrator{cfg: cfg, notifier: notifier, promptBase: "fixture task", getIssueFn: func(n int) (github.Issue, error) { return makeIssue(n, "fixture issue"), nil },
				respawnWorkerFn: func(_ *config.Config, _ string, _ *state.Session, _ string, _ github.Issue, _ string, _ string) error {
					calls++
					return hold
				}}
			past := time.Now().UTC().Add(-time.Second)
			s := state.NewState()
			sess := &state.Session{IssueNumber: 1249, IssueTitle: "fixture issue", Status: state.StatusDead, RetryCount: 1, NextRetryAt: &past, WorkerGeneration: 3, NativeRoleRunID: "00000000-0000-4000-8000-000000000003", Branch: "fixture"}
			s.Sessions["fixture-1"] = sess
			o.respawnDueRetries(s, 10)
			if calls != 1 || sess.NativeRegistrationHold != code || sess.Status != state.StatusDead || sess.RetryCount != 1 || sess.WorkerGeneration != 3 {
				t.Fatalf("parked hold not retained: calls=%d %+v", calls, sess)
			}
			got := parked()
			if len(got) != 1 || !strings.Contains(got[0], "fixture-1") || !strings.Contains(got[0], "#1249") || !strings.Contains(got[0], code) || !strings.Contains(got[0], "generation 3") {
				t.Fatalf("parked hold notifications = %q, want one naming slot, issue, code and generation", got)
			}
			// Inspecting or repairing the receipt does not release the slot;
			// the message must not suggest it does.
			if !strings.Contains(got[0], "no release command exists yet") || strings.Contains(got[0], "until an operator") {
				t.Fatalf("parked hold notification %q implies a release path", got[0])
			}
			o.respawnDueRetries(s, 10)
			if !o.retainNativeWorkerHold("fixture-1", sess, hold) {
				t.Fatal("hold not retained")
			}
			if calls != 1 || len(parked()) != 1 {
				t.Fatalf("parked hold re-attempted or re-notified: calls=%d notifications=%q", calls, parked())
			}
		})
	}
}

// Only a launch-uncertain parked code notifies, and only once per slot and
// projected generation: holds the cycle-start reconciliation resolves, or a
// non-uncertain hold, stay log-only, while a later generation parked on the
// same slot is a new condition.
func TestNativeWorkerParkedHoldNotificationScope(t *testing.T) {
	notifier, parked := parkedHoldMessages(t)
	o := &Orchestrator{cfg: &config.Config{}, notifier: notifier}
	sess := &state.Session{IssueNumber: 1249, WorkerGeneration: 3, NativeRoleRunID: "00000000-0000-4000-8000-000000000003"}
	for _, hold := range []*worker.NativeRegistrationHold{
		{Code: "native_process_identity_missing", LaunchUncertain: true},
		{Code: "previous_outcome_unknown"},
		{Code: "unresolved_launch", LaunchUncertain: true},
		{Code: "native_identity_conflict"},
	} {
		o.retainNativeWorkerHold("fixture-1", sess, hold)
	}
	if got := parked(); len(got) != 0 {
		t.Fatalf("non-parked holds notified: %q", got)
	}
	parkedHold := &worker.NativeRegistrationHold{Code: "projected_receipt_missing", LaunchUncertain: true}
	o.retainNativeWorkerHold("fixture-1", sess, parkedHold)
	o.retainNativeWorkerHold("fixture-1", sess, parkedHold)
	o.retainNativeWorkerHold("fixture-2", sess, parkedHold)
	sess.WorkerGeneration, sess.NativeRoleRunID = 4, "00000000-0000-4000-8000-000000000004"
	o.retainNativeWorkerHold("fixture-1", sess, parkedHold)
	if got := parked(); len(got) != 3 {
		t.Fatalf("parked hold notifications = %q, want one per slot and generation", got)
	}
}
