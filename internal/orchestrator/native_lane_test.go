package orchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/pipeline"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/befeast/maestro/internal/worker"
)

type fakeLaneReadiness struct {
	err   error
	calls int
}

func (f *fakeLaneReadiness) ObserveLaneReadiness(aiexecution.Policy) error {
	f.calls++
	return f.err
}

func deferredLaneHold(code string) error {
	return &worker.NativeRegistrationHold{Code: code, Deferred: true}
}

func sessionSnapshotJSON(t *testing.T, sess *state.Session) string {
	t.Helper()
	b, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A managed lane that is not ready pauses fresh dispatch before the GitHub
// list, routing or any claim, spends no retry budget and resumes next cycle.
func TestStartNewWorkers_NativeLanePausesBeforeListingAndSelfClears(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.WorkerNativeSessionRegistration = &config.NativeSessionRegistrationConfig{}
	lane := &fakeLaneReadiness{err: aiexecution.Held("binding_route_unserved")}
	cfg.RuntimeNativeLaneReadiness = lane
	issues := []github.Issue{makeIssue(1250, "ready issue")}
	o, started, labels := newStartWorkersOrchestrator(cfg, issues)
	listed := 0
	o.listOpenIssuesFn = func([]string) ([]github.Issue, error) { listed++; return issues, nil }

	s := state.NewState()
	o.startNewWorkers(s, 5)
	if listed != 0 || len(*started) != 0 || len(*labels) != 0 || len(s.Sessions) != 0 || s.FailedAttemptsForIssue(1250) != 0 || lane.calls != 1 {
		t.Fatalf("paused cycle listed=%d started=%v labels=%v sessions=%d probes=%d", listed, *started, *labels, len(s.Sessions), lane.calls)
	}
	lane.err = nil
	o.startNewWorkers(s, 5)
	if len(*started) != 1 || (*started)[0] != 1250 {
		t.Fatalf("started = %v, want the paused issue dispatched once the lane is ready", *started)
	}
}

// The lane check is the worker's last step before registration. When it
// pauses a fresh dispatch, the durable startup lease is superseded (no claim
// lingers without a session) and the reserved slot is reused next cycle.
func TestStartNewWorkers_DeferredLaneSupersedesFreshClaimAndRetries(t *testing.T) {
	dir := t.TempDir()
	cfg := cfgWithBackends("claude", "claude")
	cfg.StateDir, cfg.WorktreeBase, cfg.SessionPrefix = dir, filepath.Join(dir, "worktrees"), "fixture"
	cfg.WorkerNativeSessionRegistration = &config.NativeSessionRegistrationConfig{}
	if err := state.Save(dir, state.NewState()); err != nil {
		t.Fatal(err)
	}
	issue := makeIssue(1251, "deferred startup", "maestro-ready")
	o, _, _ := newStartWorkersOrchestrator(cfg, []github.Issue{issue})
	o.workerStartFn = nil
	permits, releases, commits := 0, 0, 0
	o.SetFleetSpawnReserve(func() (func(string), func(), bool) {
		permits++
		return func(string) { commits++ }, func() { releases++ }, true
	})
	var slots []string
	o.workerStartClaimedFn = func(_ *config.Config, s *state.State, _ string, got github.Issue, _ string, backend, slot string) (string, error) {
		slots = append(slots, slot)
		if len(slots) == 1 {
			return "", &worker.NativeRegistrationHold{Code: "binding_credential_unverified", Deferred: true, Slot: slot}
		}
		s.Sessions[slot] = &state.Session{IssueNumber: got.Number, Status: state.StatusRunning, PID: 4242, Backend: backend, StartedAt: time.Now().UTC()}
		return slot, nil
	}
	cycle, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	o.startNewWorkers(cycle, 1)
	if err := state.Save(dir, cycle); err != nil {
		t.Fatal(err)
	}
	loaded, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	claim := loaded.FreshDispatchClaims[1251]
	if claim == nil || claim.Status != state.FreshDispatchClaimStatusSuperseded || claim.TerminalReason != "native_lane_deferred" {
		t.Fatalf("deferred claim = %+v", claim)
	}
	if len(loaded.Sessions) != 0 || loaded.FailedAttemptsForIssue(1251) != 0 || permits != 1 || releases != 1 || commits != 0 {
		t.Fatalf("deferred dispatch sessions=%v permits=%d releases=%d commits=%d", loaded.Sessions, permits, releases, commits)
	}
	o.startNewWorkers(loaded, 1)
	if len(slots) != 2 || slots[1] != slots[0] || loaded.Sessions[slots[0]] == nil || commits != 1 {
		t.Fatalf("retry slots=%v sessions=%v commits=%d", slots, loaded.Sessions, commits)
	}
}

// A review-repair dispatch claims a bounded (pr, head) attempt before start; a
// deferred lane pause must give it back, like a failed start (#874).
func TestStartNewWorkers_DeferredLaneReleasesReviewRepairAttempt(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.WorkerNativeSessionRegistration = &config.NativeSessionRegistrationConfig{}
	cfg.Supervisor.ReviewRepair.MaxRetries = 1
	issues := []github.Issue{makeIssue(1252, "review repair", "maestro-ready")}
	o, started, _ := newStartWorkersOrchestrator(cfg, issues)
	start := o.workerStartFn
	calls := 0
	o.workerStartFn = func(c *config.Config, s *state.State, repo string, issue github.Issue, prompt, backend string) (string, error) {
		calls++
		if calls == 1 {
			return "", deferredLaneHold("binding_route_unserved")
		}
		return start(c, s, repo, issue, prompt, backend)
	}
	s := state.NewState()
	now := time.Now().UTC()
	decision := state.SupervisorDecision{
		ID: "repair-1252", CreatedAt: now, PolicyRule: supervisor.PolicyRuleReviewRepair,
		RecommendedAction: supervisor.ActionSpawnReviewRepair, Risk: supervisor.RiskMutating, RequiresApproval: true,
		Target: &state.SupervisorTarget{Issue: 1252, PR: 1300, HeadSHA: "4484a21c50b4"},
		ReviewRepair: &state.SupervisorReviewRepairPayload{HeadSHA: "4484a21c50b4", MaxRetries: 1, Backend: "claude",
			Findings: []state.SupervisorReviewFinding{{Path: "internal/fixture.go", Line: 1, Body: "P1: fixture", Severity: "P1"}}},
	}
	s.RecordSupervisorDecision(decision, state.DefaultSupervisorDecisionLimit)
	approval := s.RecordPendingApprovalForDecision(decision, now)
	approval.Status = state.ApprovalStatusAwaitingDispatch

	o.startNewWorkers(s, 1)
	if track, _ := s.LookupReviewRepairTrack(1300, "4484a21c50b4"); calls != 1 || track.Attempts != 0 || track.Exhausted {
		t.Fatalf("deferred review repair calls=%d track=%+v", calls, track)
	}
	o.startNewWorkers(s, 1)
	if calls != 2 || len(*started) != 1 || (*started)[0] != 1252 {
		t.Fatalf("review repair not dispatched after the lane recovered: calls=%d started=%v", calls, *started)
	}
}

// Retry respawns pause before any permit or session change while the lane is
// not ready; the probe runs only when a retry is actually due.
func TestRespawnDueRetries_NativeLanePauseLeavesSessionIdentical(t *testing.T) {
	cfg := &config.Config{Repo: "fixture/repo", StateDir: t.TempDir(), MaxRetryBackoffMs: 300000, MaxRuntimeMinutes: 999, WorkerNativeSessionRegistration: &config.NativeSessionRegistrationConfig{}}
	lane := &fakeLaneReadiness{err: aiexecution.Held("binding_route_unserved")}
	cfg.RuntimeNativeLaneReadiness = lane
	calls := 0
	o := &Orchestrator{cfg: cfg, notifier: &notify.Notifier{}, promptBase: "fixture task", getIssueFn: func(n int) (github.Issue, error) { return makeIssue(n, "fixture issue"), nil },
		respawnWorkerFn: func(_ *config.Config, _ string, sess *state.Session, _ string, _ github.Issue, _ string, _ string) error {
			calls++
			sess.Status = state.StatusRunning
			return nil
		}}
	future := time.Now().UTC().Add(time.Hour)
	s := state.NewState()
	sess := &state.Session{IssueNumber: 1253, Status: state.StatusDead, RetryCount: 2, NextRetryAt: &future, WorkerGeneration: 4, Branch: "fixture"}
	s.Sessions["fixture-1"] = sess
	o.respawnDueRetries(s, 10)
	if lane.calls != 0 {
		t.Fatal("lane probed without a due retry")
	}
	past := time.Now().UTC().Add(-time.Second)
	sess.NextRetryAt = &past
	before := sessionSnapshotJSON(t, sess)
	o.respawnDueRetries(s, 10)
	if calls != 0 || lane.calls != 1 || sessionSnapshotJSON(t, sess) != before {
		t.Fatalf("paused retry calls=%d probes=%d session=%+v", calls, lane.calls, sess)
	}
	lane.err = nil
	o.respawnDueRetries(s, 10)
	if calls != 1 || sess.Status != state.StatusRunning {
		t.Fatalf("retry not resumed: calls=%d session=%+v", calls, sess)
	}
}

// The worker's own pre-registration check is the backstop for a lane that
// changes after the cycle-level probe: the retry is restored exactly, no hold
// is persisted, no retry is spent, and the next cycle respawns.
func TestRespawnDueRetries_DeferredLaneHoldRestoresSnapshotAndRetries(t *testing.T) {
	cfg := &config.Config{Repo: "fixture/repo", StateDir: t.TempDir(), MaxRetryBackoffMs: 300000, MaxRuntimeMinutes: 999, WorkerNativeSessionRegistration: &config.NativeSessionRegistrationConfig{}}
	calls := 0
	o := &Orchestrator{cfg: cfg, notifier: &notify.Notifier{}, promptBase: "fixture task", getIssueFn: func(n int) (github.Issue, error) { return makeIssue(n, "fixture issue"), nil },
		respawnWorkerFn: func(_ *config.Config, _ string, sess *state.Session, _ string, _ github.Issue, _ string, _ string) error {
			calls++
			if calls == 1 {
				sess.WorkerGeneration++ // any partial change must be undone
				return deferredLaneHold("binding_credential_unverified")
			}
			sess.Status = state.StatusRunning
			return nil
		}}
	past := time.Now().UTC().Add(-time.Second)
	s := state.NewState()
	sess := &state.Session{IssueNumber: 1254, Status: state.StatusDead, RetryCount: 2, NextRetryAt: &past, WorkerGeneration: 4, Branch: "fixture", CIFailureOutput: "failure detail"}
	s.Sessions["fixture-1"] = sess
	before := sessionSnapshotJSON(t, sess)
	o.respawnDueRetries(s, 10)
	if calls != 1 || sessionSnapshotJSON(t, sess) != before || sess.NativeRegistrationHold != "" {
		t.Fatalf("deferred retry changed the session: %+v", sess)
	}
	o.respawnDueRetries(s, 10)
	if calls != 2 || sess.Status != state.StatusRunning || sess.NativeRegistrationHold != "" {
		t.Fatalf("deferred retry was not retried: calls=%d session=%+v", calls, sess)
	}
}

// A phase transition is not processed while the lane is not ready: Advisor
// artifacts and counters stay untouched and the transition runs next cycle. A
// deferral raised by the worker itself restores the pre-transition session.
func TestAdvancePipeline_NativeLanePauseAndDeferredBackstop(t *testing.T) {
	setup := func(t *testing.T) (*Orchestrator, *state.Session, *int, *fakeLaneReadiness) {
		cfg := pipelineConfig()
		cfg.StateDir = t.TempDir()
		cfg.WorkerNativeSessionRegistration = &config.NativeSessionRegistrationConfig{}
		lane := &fakeLaneReadiness{}
		cfg.RuntimeNativeLaneReadiness = lane
		o := pipelineOrchestrator(cfg)
		o.getIssueFn = func(n int) (github.Issue, error) { return makeIssue(n, "fixture issue"), nil }
		dir := t.TempDir()
		for _, name := range []string{pipeline.PlanFile, pipeline.ValidationFile} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		calls := 0
		sess := &state.Session{IssueNumber: 1255, Status: state.StatusRunning, Phase: state.PhasePlan, PlanVersion: 1, WorkerGeneration: 3, Worktree: dir}
		o.workerStartPhaseFn = func(_ *config.Config, s *state.Session, _ string, _ string, _ string) error {
			calls++
			if calls == 1 && lane.err == nil {
				return deferredLaneHold("binding_route_unserved")
			}
			s.WorkerGeneration++
			return nil
		}
		return o, sess, &calls, lane
	}
	t.Run("paused before processing", func(t *testing.T) {
		o, sess, calls, lane := setup(t)
		lane.err = aiexecution.Held("binding_route_unserved")
		before := sessionSnapshotJSON(t, sess)
		if !o.advancePipeline(state.NewState(), "fixture-1", sess) {
			t.Fatal("not handled")
		}
		if *calls != 0 || sessionSnapshotJSON(t, sess) != before {
			t.Fatalf("paused transition calls=%d session=%+v", *calls, sess)
		}
	})
	t.Run("worker deferral restores the transition", func(t *testing.T) {
		o, sess, calls, _ := setup(t)
		before := sessionSnapshotJSON(t, sess)
		if !o.advancePipeline(state.NewState(), "fixture-1", sess) {
			t.Fatal("not handled")
		}
		if *calls != 1 || sessionSnapshotJSON(t, sess) != before || sess.NativeRegistrationHold != "" {
			t.Fatalf("deferred transition calls=%d session=%+v", *calls, sess)
		}
		o.advancePipeline(state.NewState(), "fixture-1", sess)
		if *calls != 2 || sess.Phase != state.PhaseImplement || sess.WorkerGeneration != 4 || sess.NativeRegistrationHold != "" {
			t.Fatalf("transition not retried: calls=%d session=%+v", *calls, sess)
		}
	})
}

// A repair dispatch deferred by the lane keeps its approval and the exact
// session untouched, and is dispatched once the lane is ready.
func TestDispatchSpawnRepair_DeferredLaneRetainsApprovalAndRetries(t *testing.T) {
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
	o.respawnInPlaceFn = func(_ *config.Config, _ string, s *state.Session, _ string, _ github.Issue, _, _ string) error {
		calls++
		if calls == 1 {
			s.Status = state.StatusRunning // any partial change must be undone
			return deferredLaneHold("binding_credential_unverified")
		}
		s.Status = state.StatusRunning
		return nil
	}
	st := repairGateTestState(time.Now().UTC(), head)
	st.Sessions["slot-7"].NativeRoleRunID = "00000000-0000-4000-8000-000000000001"
	before := sessionSnapshotJSON(t, st.Sessions["slot-7"])
	o.startNewWorkers(st, 1)
	if calls != 1 || len(*fresh) != 0 || sessionSnapshotJSON(t, st.Sessions["slot-7"]) != before {
		t.Fatalf("deferred repair calls=%d session=%+v", calls, st.Sessions["slot-7"])
	}
	if got := approvalStatus(t, st, "repair-7"); got != state.ApprovalStatusAwaitingDispatch {
		t.Fatalf("deferred repair consumed approval=%s", got)
	}
	o.startNewWorkers(st, 1)
	if calls != 2 || st.Sessions["slot-7"].Status != state.StatusRunning {
		t.Fatalf("deferred repair not retried: calls=%d", calls)
	}
}

// The explicit prelaunch recovery is one-shot; a lane that is not ready must
// refuse it before any reservation or recovery call.
func TestNativePrelaunchRecoveryRefusesWhileLaneNotReady(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.ProjectID, cfg.MaxLiveWorkers, cfg.StateDir = "project", 1, t.TempDir()
	cfg.WorkerNativeSessionRegistration = &config.NativeSessionRegistrationConfig{}
	cfg.RuntimeNativeLaneReadiness = &fakeLaneReadiness{err: aiexecution.Held("binding_route_unserved")}
	issue := makeIssue(1, "held work", "maestro-ready")
	issue.State = "open"
	o := New(cfg)
	o.getIssueFn = func(int) (github.Issue, error) { return issue, nil }
	o.listOpenPRsFn = func() ([]github.PR, error) { return nil, nil }
	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{Status: state.StatusFailed, NativeRegistrationHold: "setup_failed", IssueNumber: 1, Backend: "claude", Branch: "canonical"}
	reserved, calls := 0, 0
	o.SetFleetSpawnReserve(func() (func(string), func(), bool) { reserved++; return func(string) {}, func() {}, true })
	o.nativePrelaunchRecoverFn = func(*config.Config, *state.State, string, github.Issue, string, string, string) (string, error) {
		calls++
		return "slot-1", nil
	}
	before := sessionSnapshotJSON(t, s.Sessions["slot-1"])
	if err := o.recoverNativePrelaunchWorker(s, NativePrelaunchRecovery{ProjectID: "project", Slot: "slot-1", NativeSessionID: "exact-native-id"}); err == nil {
		t.Fatal("recovery ran while the lane was not ready")
	}
	if calls != 0 || reserved != 0 || sessionSnapshotJSON(t, s.Sessions["slot-1"]) != before {
		t.Fatalf("refused recovery calls=%d reserved=%d", calls, reserved)
	}
}

// retainNativeWorkerHold never persists a deferred code; a real hold keeps the
// existing sticky semantics.
func TestRetainNativeWorkerHoldKeepsDeferredOutOfState(t *testing.T) {
	before := &state.Session{IssueNumber: 1, Status: state.StatusDead, RetryCount: 1}
	sess := &state.Session{IssueNumber: 1, Status: state.StatusRunning, RetryCount: 2}
	o := &Orchestrator{}
	if !o.retainNativeWorkerHold("slot-1", sess, deferredLaneHold("binding_route_unserved")) || sess.NativeRegistrationHold != "" {
		t.Fatalf("deferred hold persisted: %+v", sess)
	}
	restoreNativeHeldSession(sess, before)
	if sess.Status != state.StatusDead || sess.RetryCount != 1 || sess.NativeRegistrationHold != "" {
		t.Fatalf("deferred restore = %+v", sess)
	}
	if b, _ := json.Marshal(sess); string(b) != sessionSnapshotJSON(t, before) {
		t.Fatalf("deferral marker serialized: %s", b)
	}
	if nativeSessionSnapshot(&config.Config{WorkerNativeSessionRegistration: &config.NativeSessionRegistrationConfig{}}, sess); sess.NativeLaneDeferred != "" {
		t.Fatal("a new operation inherited an earlier deferral")
	}
	sess = &state.Session{IssueNumber: 1, Status: state.StatusRunning}
	if !o.retainNativeWorkerHold("slot-1", sess, &worker.NativeRegistrationHold{Code: "authority_unavailable"}) {
		t.Fatal("real hold not retained")
	}
	restoreNativeHeldSession(sess, before)
	if sess.NativeRegistrationHold != "authority_unavailable" || sess.Status != state.StatusDead {
		t.Fatalf("real hold restore = %+v", sess)
	}
}

// Once the lane is observed ready, a first spawn that a binding hold stopped at
// the registered stage is resumed through the explicit recovery path; while
// the lane is not ready nothing is attempted.
func TestStartNewWorkers_ResumesLaneHeldRegisteredFirstSpawnWhenReady(t *testing.T) {
	cfg := cfgWithBackends("claude", "claude")
	cfg.ProjectID, cfg.MaxLiveWorkers, cfg.StateDir = "project", 2, t.TempDir()
	cfg.WorkerNativeSessionRegistration = &config.NativeSessionRegistrationConfig{}
	lane := &fakeLaneReadiness{err: aiexecution.Held("binding_route_unserved")}
	cfg.RuntimeNativeLaneReadiness = lane
	issue := makeIssue(1, "held work", "maestro-ready")
	issue.State = "open"
	o, _, _ := newStartWorkersOrchestrator(cfg, nil)
	o.getIssueFn = func(int) (github.Issue, error) { return issue, nil }
	o.listOpenPRsFn = func() ([]github.PR, error) { return nil, nil }
	o.SetFleetSpawnReserve(func() (func(string), func(), bool) { return func(string) {}, func() {}, true })
	s := state.NewState()
	s.Sessions["slot-1"] = &state.Session{Status: state.StatusFailed, NativeRegistrationHold: "binding_route_unserved", IssueNumber: 1, Backend: "claude", Branch: "canonical"}
	s.Sessions["slot-2"] = &state.Session{Status: state.StatusFailed, NativeRegistrationHold: "setup_failed", IssueNumber: 2, Backend: "claude", Branch: "other"}
	o.nativeLaneHeldPrelaunchFn = func(_ *config.Config, slot string, sess *state.Session) (string, bool) {
		return "exact-native-id", worker.NativeLaneHoldCode(sess.NativeRegistrationHold)
	}
	var resumed []string
	o.nativePrelaunchRecoverFn = func(_ *config.Config, st *state.State, _ string, _ github.Issue, _, slot, nativeID string) (string, error) {
		resumed = append(resumed, slot+"="+nativeID)
		st.Sessions[slot].Status, st.Sessions[slot].NativeRegistrationHold = state.StatusRunning, ""
		return slot, nil
	}
	o.startNewWorkers(s, 1)
	if len(resumed) != 0 {
		t.Fatalf("resumed under a paused lane: %v", resumed)
	}
	lane.err = nil
	o.startNewWorkers(s, 1)
	o.startNewWorkers(s, 1)
	if len(resumed) != 1 || resumed[0] != "slot-1=exact-native-id" || s.Sessions["slot-2"].NativeRegistrationHold != "setup_failed" {
		t.Fatalf("resumed=%v", resumed)
	}
}

// An operator pause or drain returns before the managed-lane probe: such a
// cycle observes no lane readiness and journals only its own reason, never a
// "native lane not ready" line that would read as the cause of the stall.
func TestStartNewWorkers_PauseAndDrainReturnBeforeLaneProbe(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(*state.State)
		want string
	}{
		{"paused", func(s *state.State) { s.SetPaused(time.Now().UTC()) }, ""},
		{"drained", func(s *state.State) { s.SetSpawnDrain(time.Now().UTC()) }, "drain active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureOrchestratorLog(t)
			cfg := cfgWithBackends("claude", "claude")
			cfg.WorkerNativeSessionRegistration = &config.NativeSessionRegistrationConfig{}
			lane := &fakeLaneReadiness{err: aiexecution.Held("binding_route_unserved")}
			cfg.RuntimeNativeLaneReadiness = lane
			issues := []github.Issue{makeIssue(1256, "ready issue")}
			o, started, _ := newStartWorkersOrchestrator(cfg, issues)
			listed := 0
			o.listOpenIssuesFn = func([]string) ([]github.Issue, error) { listed++; return issues, nil }
			s := state.NewState()
			tc.stop(s)

			o.startNewWorkers(s, 5)
			out := buf.String()
			if lane.calls != 0 || listed != 0 || len(*started) != 0 {
				t.Fatalf("%s cycle probed the lane %d time(s), listed=%d started=%v", tc.name, lane.calls, listed, *started)
			}
			if strings.Contains(out, "native lane not ready") {
				t.Fatalf("%s cycle journaled a lane reason:\n%s", tc.name, out)
			}
			if tc.want != "" && !strings.Contains(out, tc.want) {
				t.Fatalf("%s cycle did not journal its own reason %q:\n%s", tc.name, tc.want, out)
			}
		})
	}
}
