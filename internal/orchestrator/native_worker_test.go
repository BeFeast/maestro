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

	"github.com/befeast/maestro/internal/aiexecution"
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

// nativeHoldRoutingState holds one slot per routing shape at cycle start.
func nativeHoldRoutingState() *state.State {
	s := state.NewState()
	s.Sessions["fixture-1"] = &state.Session{IssueNumber: 21, Status: state.StatusFailed, NativeRegistrationHold: "binding_inventory_incomplete", Worktree: "/worktrees/fixture-1", Branch: "feat/fixture-1-21"}
	s.Sessions["fixture-2"] = &state.Session{IssueNumber: 22, Status: state.StatusDead, NativeRegistrationHold: "native_process_identity_missing", WorkerGeneration: 3, NativeRoleRunID: "00000000-0000-4000-8000-000000000003"}
	s.Sessions["fixture-3"] = &state.Session{IssueNumber: 23, Status: state.StatusFailed, NativeRegistrationHold: "unresolved_launch"}
	s.Sessions["fixture-4"] = &state.Session{IssueNumber: 24, Status: state.StatusRunning}
	s.Sessions["fixture-5"] = &state.Session{IssueNumber: 25, Status: state.StatusFailed, NativeRegistrationHold: "setup_failed", ReleasedForRedispatch: true}
	s.Sessions["fixture-6"] = &state.Session{IssueNumber: 26, Status: state.StatusPROpen, NativeRegistrationHold: "setup_failed", WorkerGeneration: 2, NativeRoleRunID: "00000000-0000-4000-8000-000000000006"}
	// A failed first-generation start under a deterministic local hold is an
	// operator decision, not an expiry candidate.
	s.Sessions["fixture-7"] = &state.Session{IssueNumber: 27, Status: state.StatusFailed, NativeRegistrationHold: "setup_failed", Worktree: "/worktrees/fixture-7", Branch: "feat/fixture-7-27"}
	return s
}

func nativeHoldRoutingOrchestrator(t *testing.T, runtime, expiry map[string]int) *Orchestrator {
	t.Helper()
	cfg := &config.Config{StateDir: t.TempDir(), AIExecution: aiexecution.Policy{RequireVerifiedRoute: true}}
	return &Orchestrator{cfg: cfg, notifier: &notify.Notifier{},
		nativeRuntimeReconcileFn: func(_ *config.Config, _ *state.State, slot string) error {
			runtime[slot]++
			return &worker.NativeRegistrationHold{Code: "native_launch_abandonment_unproven"}
		},
		nativePrelaunchExpiryFn: func(_ *config.Config, s *state.State, slot string) (bool, error) {
			expiry[slot]++
			sess := s.Sessions[slot]
			sess.NativeRegistrationHold = ""
			sess.ReleasedForRedispatch = true
			sess.WorkerOutcome = state.WorkerOutcomeNativePrelaunchAbandoned
			return true, nil
		}}
}

// Each held slot reaches exactly one reconciler per cycle: wedge/launch-gap
// holds the exact runtime reconciliation, a failed first-generation hold the
// expired pre-launch reconciliation; everything else is left alone. A released
// slot is not routed again on the next cycle (idempotent).
func TestNativeHoldsAtCycleStartRouteEachSlotOnce(t *testing.T) {
	runtime, expiry := map[string]int{}, map[string]int{}
	o := nativeHoldRoutingOrchestrator(t, runtime, expiry)
	s := nativeHoldRoutingState()
	if !s.IssueHasNonFreshClaim(21) {
		t.Fatal("fixture: the held failed slot must claim its issue")
	}
	o.reconcileNativeHoldsAtCycleStart(s)
	wantRuntime := map[string]int{"fixture-2": 1, "fixture-3": 1}
	wantExpiry := map[string]int{"fixture-1": 1}
	if len(runtime) != len(wantRuntime) || runtime["fixture-2"] != 1 || runtime["fixture-3"] != 1 {
		t.Fatalf("runtime routing=%v want %v", runtime, wantRuntime)
	}
	if len(expiry) != len(wantExpiry) || expiry["fixture-1"] != 1 {
		t.Fatalf("expiry routing=%v want %v", expiry, wantExpiry)
	}
	if s.IssueHasNonFreshClaim(21) {
		t.Fatal("released slot still claims its issue")
	}
	o.reconcileNativeHoldsAtCycleStart(s)
	if expiry["fixture-1"] != 1 || len(expiry) != 1 || runtime["fixture-2"] != 2 || runtime["fixture-3"] != 2 {
		t.Fatalf("second cycle: runtime=%v expiry=%v", runtime, expiry)
	}
}

// Releasing a held issue re-enables dispatch, so it waits while the project
// is paused, drained or emergency-stopped; runtime reconciliation of wedge
// holds (which never releases an issue) still runs.
func TestNativeHoldsAtCycleStartDeferExpiredPrelaunchWhileStopped(t *testing.T) {
	for _, mode := range []string{"paused", "drained", "emergency"} {
		t.Run(mode, func(t *testing.T) {
			runtime, expiry := map[string]int{}, map[string]int{}
			o := nativeHoldRoutingOrchestrator(t, runtime, expiry)
			s := nativeHoldRoutingState()
			switch mode {
			case "paused":
				s.Paused = true
			case "drained":
				s.SpawnDrain = true
			case "emergency":
				o.SetEmergencyHalt(func() bool { return true })
			}
			o.reconcileNativeHoldsAtCycleStart(s)
			if len(expiry) != 0 {
				t.Fatalf("expired prelaunch reconciled while %s: %v", mode, expiry)
			}
			if sess := s.Sessions["fixture-1"]; sess.NativeRegistrationHold != "binding_inventory_incomplete" || sess.ReleasedForRedispatch || !s.IssueHasNonFreshClaim(21) {
				t.Fatalf("held slot changed while %s: %+v", mode, sess)
			}
			if runtime["fixture-2"] != 1 || runtime["fixture-3"] != 1 {
				t.Fatalf("runtime reconciliation skipped while %s: %v", mode, runtime)
			}
		})
	}
}

func TestNativeHoldsAtCycleStartRequireVerifiedRoute(t *testing.T) {
	runtime, expiry := map[string]int{}, map[string]int{}
	o := nativeHoldRoutingOrchestrator(t, runtime, expiry)
	o.cfg.AIExecution = aiexecution.Policy{}
	o.reconcileNativeHoldsAtCycleStart(nativeHoldRoutingState())
	if len(runtime) != 0 || len(expiry) != 0 {
		t.Fatalf("reconciled without the verified route: runtime=%v expiry=%v", runtime, expiry)
	}
}

// A typed hold from the expired pre-launch reconciliation keeps the slot held
// and claimed; the next cycle tries again.
func TestNativeHoldsAtCycleStartKeepHoldOnExpiredPrelaunchFailure(t *testing.T) {
	runtime, expiry := map[string]int{}, map[string]int{}
	o := nativeHoldRoutingOrchestrator(t, runtime, expiry)
	o.nativePrelaunchExpiryFn = func(_ *config.Config, _ *state.State, slot string) (bool, error) {
		expiry[slot]++
		return false, &worker.NativeRegistrationHold{Code: "outcome_authority_unavailable", LaunchUncertain: true}
	}
	s := nativeHoldRoutingState()
	o.reconcileNativeHoldsAtCycleStart(s)
	o.reconcileNativeHoldsAtCycleStart(s)
	if expiry["fixture-1"] != 2 || s.Sessions["fixture-1"].NativeRegistrationHold != "binding_inventory_incomplete" || !s.IssueHasNonFreshClaim(21) {
		t.Fatalf("expiry=%v session=%+v", expiry, s.Sessions["fixture-1"])
	}
}

// capturedNotifications returns a notifier whose messages are recorded, and a
// reader for the messages containing substr.
func capturedNotifications(t *testing.T) (*notify.Notifier, func(substr string) []string) {
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
	return notify.New(server.URL, "fixture-target"), func(substr string) []string {
		mu.Lock()
		defer mu.Unlock()
		var matched []string
		for _, m := range messages {
			if strings.Contains(m, substr) {
				matched = append(matched, m)
			}
		}
		return matched
	}
}

// heldFirstStart is the failed first-generation projection a binding hold
// leaves when a fresh dispatch stops after registration.
func heldFirstStart(issue int, slot string) *state.Session {
	return &state.Session{IssueNumber: issue, Status: state.StatusFailed, NativeRegistrationHold: "binding_inventory_incomplete",
		Worktree: "/worktrees/" + slot, Branch: "feat/" + slot}
}

// #1260 loop: every automatic release of an expired pre-launch hold notifies
// the operator, and an issue whose fresh starts keep stopping before launch is
// re-queued at most twice in a row. The third consecutive held start stays
// held for the operator with exactly one notification, however many cycles
// pass. A launched worker for the issue in between resets the count.
func TestExpiredPrelaunchReleaseNotifiesAndStopsAfterTwoConsecutive(t *testing.T) {
	notifier, messages := capturedNotifications(t)
	expiry := map[string]int{}
	cfg := &config.Config{StateDir: t.TempDir(), AIExecution: aiexecution.Policy{RequireVerifiedRoute: true}}
	o := &Orchestrator{cfg: cfg, notifier: notifier,
		nativeRuntimeReconcileFn: func(*config.Config, *state.State, string) error { return nil },
		nativePrelaunchExpiryFn: func(_ *config.Config, s *state.State, slot string) (bool, error) {
			expiry[slot]++
			sess := s.Sessions[slot]
			now := time.Now().UTC()
			sess.NativeRegistrationHold, sess.ReleasedForRedispatch, sess.WorkerOutcome, sess.FinishedAt = "", true, state.WorkerOutcomeNativePrelaunchAbandoned, &now
			return true, nil
		}}
	s := state.NewState()
	// An earlier launched attempt does not count as "in between".
	launchedAt := time.Now().UTC().Add(-time.Hour)
	s.Sessions["fixture-1"] = &state.Session{IssueNumber: 31, Status: state.StatusDead, WorkerGeneration: 2, StartedAt: launchedAt, FinishedAt: &launchedAt}

	// Two consecutive held fresh starts are released, one notification each.
	for i, slot := range []string{"fixture-2", "fixture-3"} {
		s.Sessions[slot] = heldFirstStart(31, slot)
		o.reconcileNativeHoldsAtCycleStart(s)
		if expiry[slot] != 1 || !s.Sessions[slot].ReleasedForRedispatch {
			t.Fatalf("release %d of %s: expiry=%v session=%+v", i+1, slot, expiry, s.Sessions[slot])
		}
		want := "issue #31 re-queued after an expired pre-launch hold on " + slot
		if got := messages(want); len(got) != 1 {
			t.Fatalf("release notifications for %s = %q, want one containing %q", slot, got, want)
		}
	}

	// The third consecutive held start is not released, on any cycle, and the
	// operator is told once.
	s.Sessions["fixture-4"] = heldFirstStart(31, "fixture-4")
	for cycle := 0; cycle < 3; cycle++ {
		o.reconcileNativeHoldsAtCycleStart(s)
	}
	if expiry["fixture-4"] != 0 || s.Sessions["fixture-4"].NativeRegistrationHold != "binding_inventory_incomplete" || s.Sessions["fixture-4"].ReleasedForRedispatch {
		t.Fatalf("third consecutive held start was released: expiry=%v session=%+v", expiry, s.Sessions["fixture-4"])
	}
	if got := messages("automatic re-queue stopped"); len(got) != 1 || !strings.Contains(got[0], "issue #31") || !strings.Contains(got[0], "fixture-4") {
		t.Fatalf("stop notifications = %q, want exactly one naming issue #31 and fixture-4", got)
	}
	if got := messages("re-queued after an expired pre-launch hold on fixture-4"); len(got) != 0 {
		t.Fatalf("held slot reported as re-queued: %q", got)
	}

	// Another issue is unaffected by issue #31's count.
	s.Sessions["fixture-5"] = heldFirstStart(32, "fixture-5")
	o.reconcileNativeHoldsAtCycleStart(s)
	if expiry["fixture-5"] != 1 {
		t.Fatalf("issue #32 release blocked by another issue's count: expiry=%v", expiry)
	}

	// A launched worker for issue #31 after the releases resets the count.
	relaunched := time.Now().UTC().Add(time.Second)
	s.Sessions["fixture-4"].NativeRegistrationHold = ""
	s.Sessions["fixture-4"].Status = state.StatusDone
	s.Sessions["fixture-6"] = &state.Session{IssueNumber: 31, Status: state.StatusDone, WorkerGeneration: 1, StartedAt: relaunched, FinishedAt: &relaunched}
	s.Sessions["fixture-7"] = heldFirstStart(31, "fixture-7")
	o.reconcileNativeHoldsAtCycleStart(s)
	if expiry["fixture-7"] != 1 {
		t.Fatalf("held start after a launch was not released: expiry=%v", expiry)
	}
}
