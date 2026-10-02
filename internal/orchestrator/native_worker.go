package orchestrator

import (
	"encoding/json"
	"fmt"
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
	"log"
	"time"
)

func nativeWorkerConfig(cfg *config.Config, role, parent string) *config.Config {
	if cfg.WorkerNativeSessionRegistration == nil {
		return cfg
	}
	copy := *cfg
	copy.WorkerLaunchContext = &config.WorkerLaunchContext{Role: role, ParentRoleRunID: parent}
	return &copy
}
func (o *Orchestrator) retainNativeWorkerHold(slot string, sess *state.Session, err error) bool {
	hold, ok := worker.NativeHold(err)
	if !ok {
		return false
	}
	if hold.Deferred {
		// A pre-registration lane pause: the caller restores its snapshot
		// (restoreNativeHeldSession) and nothing is persisted.
		sess.NativeLaneDeferred = hold.Code
		log.Printf("[orch] native lane not ready: %s — retrying next cycle", hold.Code)
		return true
	}
	sess.NativeRegistrationHold = hold.Code
	log.Printf("[orch] native worker generation held: %s%s", hold.Code, nativeWorkerExecHoldSuffix(sess))
	if hold.LaunchUncertain && nativeParkedHold(hold.Code) {
		o.notifyParkedNativeHold(slot, sess, hold.Code)
	}
	return true
}

// nativeParkedHold reports whether a launch-uncertain native hold parks its
// slot until an operator acts. No cycle-start reconciliation routes these codes
// (nativeRuntimeReconcileHold) and no reconciliation clears them, while every
// respawn, retry, phase and repair path skips a held session, so the slot keeps
// its capacity and issue claim with no further automatic attempt. The set is
// the worker's own list of codes the termination fence reports when the
// projected generation's receipt or terminal marker cannot be trusted
// (worker.NativeProjectedReceiptHoldCodes), not a copy of it.
func nativeParkedHold(code string) bool {
	return worker.NativeProjectedReceiptHold(code)
}

// notifyParkedNativeHold tells the operator once that a slot is parked on a
// native hold. Before these holds existed the same receipt faults failed the
// respawn with a "respawn failed" notification; the hold keeps the session but
// must not make the stall silent. The notification fires on the first
// retention of a hold for the slot's projected generation and stays silent on
// every later retention of the same hold; a held session is not re-attempted
// by later cycles, so a daemon restart does not repeat it either.
func (o *Orchestrator) notifyParkedNativeHold(slot string, sess *state.Session, code string) {
	if o == nil || sess == nil {
		return
	}
	identity := fmt.Sprintf("%s/%s/%d", code, sess.NativeRoleRunID, sess.WorkerGeneration)
	if o.parkedNativeHoldNotified[slot] == identity {
		return
	}
	if o.parkedNativeHoldNotified == nil {
		o.parkedNativeHoldNotified = make(map[string]string)
	}
	o.parkedNativeHoldNotified[slot] = identity
	if o.notifier == nil {
		return
	}
	o.notifier.Sendf("⚠️ maestro: worker %s (issue #%d: %s) is parked on native hold %s for generation %d: no respawn, retry, phase transition or repair runs and no release command exists yet; inspect the slot's native receipt directory (docs/worker-native-registration.md)",
		slot, sess.IssueNumber, sess.IssueTitle, code, sess.WorkerGeneration)
}

// nativeLaneHold is the managed-lane throughput pause (like the #1128 host
// resource hold): nothing is persisted, no retry budget is spent and the next
// poll re-checks. It only runs where native registration is configured.
func (o *Orchestrator) nativeLaneHold() (bool, string) {
	cfg := o.cfg
	if cfg == nil || cfg.WorkerNativeSessionRegistration == nil || cfg.RuntimeNativeLaneReadiness == nil {
		return false, ""
	}
	if err := cfg.RuntimeNativeLaneReadiness.ObserveLaneReadiness(cfg.AIExecution); err != nil {
		return true, aiexecution.LaneHoldCode(err)
	}
	return false, ""
}

// nativeRuntimeReconcileHold reports whether a native hold is resolved by the
// exact runtime reconciliation at cycle start (worker.ReconcileNativeWorkerRuntime):
// the successor-launch gap (#1235) and the pre-launch wedge in which an
// in-place respawn registered a successor and then held on the projected
// generation's missing terminal marker or unsettled outcome.
func nativeRuntimeReconcileHold(code string) bool {
	switch code {
	case "unresolved_launch", "native_process_identity_missing", "previous_outcome_unknown":
		return true
	}
	return false
}

// reconcileNativeHoldsAtCycleStart runs the exact native hold reconcilers once
// per cycle, before any lease, status or scheduling decision (and therefore
// also on the first cycle after a daemon restart). Each slot reaches at most
// one reconciler per cycle:
//
//   - the launch/state gap and pre-launch wedge holds go to the exact runtime
//     reconciliation (worker.ReconcileNativeWorkerRuntime);
//   - a managed-lane or admission-authority hold on a failed first-generation
//     start (worker.NativePrelaunchExpiryCandidate) goes to the expired
//     pre-launch reconciliation (worker.ReconcileExpiredNativePrelaunch), which
//     retires a registration that expired or was revoked before launch and
//     releases the issue for redispatch. Releasing re-enables dispatch, so it
//     only runs while the project is not paused, drained or emergency-stopped,
//     notifies the operator on every release and stops after
//     maxConsecutiveNativePrelaunchReleases releases of one issue without a
//     launch (releaseExpiredNativePrelaunch). Other first-generation holds stay
//     operator decisions.
//
// Only the verified route records the evidence these reconcilers need.
func (o *Orchestrator) reconcileNativeHoldsAtCycleStart(s *state.State) {
	if o.cfg == nil || s == nil || !o.cfg.AIExecution.RequireVerifiedRoute {
		return
	}
	runtimeFn := o.nativeRuntimeReconcileFn
	if runtimeFn == nil {
		runtimeFn = worker.ReconcileNativeWorkerRuntime
	}
	expiryFn := o.nativePrelaunchExpiryFn
	if expiryFn == nil {
		expiryFn = worker.ReconcileExpiredNativePrelaunch
	}
	releaseAllowed := !s.PauseActive() && !s.DrainActive() && (o.emergencyHaltFn == nil || !o.emergencyHaltFn())
	for _, slot := range sortedStateSessionNames(s) {
		sess := s.Sessions[slot]
		switch {
		case sess == nil:
		case nativeRuntimeReconcileHold(sess.NativeRegistrationHold):
			if err := runtimeFn(o.cfg, s, slot); err != nil {
				log.Printf("[orch] exact native runtime reconciliation held for %s: %v%s", slot, err, nativeWorkerExecHoldSuffix(sess))
			}
		case releaseAllowed && worker.NativePrelaunchExpiryCandidate(sess):
			o.releaseExpiredNativePrelaunch(s, slot, sess, expiryFn)
		}
	}
}

// maxConsecutiveNativePrelaunchReleases bounds the automatic re-queueing of
// one issue whose fresh starts keep stopping before launch. Each release
// starts the issue again under a fresh registration; if the managed lane or
// the authority keeps holding every new start past its registration TTL, the
// release -> redispatch -> hold -> expiry cycle would otherwise repeat forever.
const maxConsecutiveNativePrelaunchReleases = 2

// releaseExpiredNativePrelaunch runs the expired pre-launch reconciliation for
// one candidate slot and tells the operator about every release. An issue that
// was already released maxConsecutiveNativePrelaunchReleases times in a row
// without a launched worker in between is not released again: the slot stays
// held for the operator and one notification says so.
func (o *Orchestrator) releaseExpiredNativePrelaunch(s *state.State, slot string, sess *state.Session, expiryFn func(*config.Config, *state.State, string) (bool, error)) {
	hold := sess.NativeRegistrationHold
	streak := consecutiveNativePrelaunchReleases(s, sess.IssueNumber)
	if streak >= maxConsecutiveNativePrelaunchReleases {
		o.notifyNativePrelaunchReleaseStopped(slot, sess, streak)
		return
	}
	released, err := expiryFn(o.cfg, s, slot)
	if err != nil {
		log.Printf("[orch] expired native prelaunch reconciliation held for %s (hold %s): %v", slot, hold, err)
	}
	if !released {
		return
	}
	if o.notifier != nil {
		o.notifier.Sendf("♻️ maestro: issue #%d re-queued after an expired pre-launch hold on %s (hold %s; consecutive re-queue %d of %d without a launch)",
			sess.IssueNumber, slot, hold, streak+1, maxConsecutiveNativePrelaunchReleases)
	}
}

// consecutiveNativePrelaunchReleases counts the automatic pre-launch releases
// of issue since its most recent launched worker: released sessions with the
// native_prelaunch_abandoned outcome that ended after the latest launch time of
// any session for the issue. A launched attempt carries a worker generation and
// a launch time; a pre-launch release never does, and its FinishedAt is the
// release (or failure) time. Derived from the retained session records, so it
// needs no separate counter and survives daemon restarts.
func consecutiveNativePrelaunchReleases(s *state.State, issue int) int {
	var lastLaunch time.Time
	for _, sess := range s.Sessions {
		if sess != nil && sess.IssueNumber == issue && sess.WorkerGeneration > 0 && sess.StartedAt.After(lastLaunch) {
			lastLaunch = sess.StartedAt
		}
	}
	releases := 0
	for _, sess := range s.Sessions {
		if sess == nil || sess.IssueNumber != issue || !sess.ReleasedForRedispatch || sess.WorkerOutcome != state.WorkerOutcomeNativePrelaunchAbandoned {
			continue
		}
		if sess.FinishedAt == nil || sess.FinishedAt.After(lastLaunch) {
			releases++
		}
	}
	return releases
}

// notifyNativePrelaunchReleaseStopped journals and notifies, once per slot and
// hold, that automatic re-queueing stopped for the slot's issue.
func (o *Orchestrator) notifyNativePrelaunchReleaseStopped(slot string, sess *state.Session, streak int) {
	identity := "prelaunch_release_stopped/" + sess.NativeRegistrationHold
	if o.parkedNativeHoldNotified[slot] == identity {
		return
	}
	if o.parkedNativeHoldNotified == nil {
		o.parkedNativeHoldNotified = make(map[string]string)
	}
	o.parkedNativeHoldNotified[slot] = identity
	log.Printf("[orch] expired native prelaunch hold on %s left for the operator: issue #%d was already re-queued %d times in a row without a launch (hold %s)",
		slot, sess.IssueNumber, streak, sess.NativeRegistrationHold)
	if o.notifier == nil {
		return
	}
	o.notifier.Sendf("⚠️ maestro: issue #%d (%s): automatic re-queue stopped after %d consecutive expired pre-launch holds without a launch; slot %s stays held on %s for the operator (docs/worker-native-registration.md)",
		sess.IssueNumber, sess.IssueTitle, streak, slot, sess.NativeRegistrationHold)
}

// nativeWorkerExecHoldSuffix appends the hold code the host runner wrote to the
// worker log, so an unresolved_launch journal line names the actual refusal
// (for example containment_forgejo_authorization_unverified) instead of only
// the generic launch uncertainty (#1235).
func nativeWorkerExecHoldSuffix(sess *state.Session) string {
	if sess == nil || sess.NativeRegistrationHold != "unresolved_launch" {
		return ""
	}
	code := worker.NativeWorkerExecHold(sess.LogFile)
	if code == "" {
		return ""
	}
	return " (worker exec held: " + code + ")"
}

// Snapshot only for the opt-in contract. A hold is not a consumed retry,
// provider failure, completed phase or exhausted Advisor round.
func nativeSessionSnapshot(cfg *config.Config, sess *state.Session) *state.Session {
	if sess == nil || cfg == nil || (cfg.WorkerNativeSessionRegistration == nil && sess.NativeRoleRunID == "") {
		return nil
	}
	sess.NativeLaneDeferred = "" // a new operation starts; an older deferral is settled
	b, _ := json.Marshal(sess)
	var snapshot state.Session
	_ = json.Unmarshal(b, &snapshot)
	return &snapshot
}
func restoreNativeHeldSession(sess, before *state.Session) {
	if sess == nil {
		return
	}
	if code := sess.NativeLaneDeferred; code != "" {
		// Deferred before registration: undo every change of this call and
		// persist no hold, so the same transition is retried next cycle. The
		// in-memory marker survives so an enclosing snapshot is restored too;
		// it is never serialized and the next snapshot clears it.
		if before != nil {
			*sess = *before
		}
		sess.NativeLaneDeferred = code
		return
	}
	if before == nil || sess.NativeRegistrationHold == "" {
		return
	}
	code := sess.NativeRegistrationHold
	*sess = *before
	sess.NativeRegistrationHold = code
}
