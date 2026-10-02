package orchestrator

import (
	"encoding/json"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
	"log"
)

func nativeWorkerConfig(cfg *config.Config, role, parent string) *config.Config {
	if cfg.WorkerNativeSessionRegistration == nil {
		return cfg
	}
	copy := *cfg
	copy.WorkerLaunchContext = &config.WorkerLaunchContext{Role: role, ParentRoleRunID: parent}
	return &copy
}
func retainNativeWorkerHold(sess *state.Session, err error) bool {
	hold, ok := worker.NativeHold(err)
	if !ok {
		return false
	}
	sess.NativeRegistrationHold = hold.Code
	log.Printf("[orch] native worker generation held: %s%s", hold.Code, nativeWorkerExecHoldSuffix(sess))
	return true
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
//   - any other hold on a failed first-generation start goes to the expired
//     pre-launch reconciliation (worker.ReconcileExpiredNativePrelaunch), which
//     retires a registration that expired or was revoked before launch and
//     releases the issue for redispatch. Releasing re-enables dispatch, so it
//     only runs while the project is not paused, drained or emergency-stopped.
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
			hold := sess.NativeRegistrationHold
			if _, err := expiryFn(o.cfg, s, slot); err != nil {
				log.Printf("[orch] expired native prelaunch reconciliation held for %s (hold %s): %v", slot, hold, err)
			}
		}
	}
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
	b, _ := json.Marshal(sess)
	var snapshot state.Session
	_ = json.Unmarshal(b, &snapshot)
	return &snapshot
}
func restoreNativeHeldSession(sess, before *state.Session) {
	if before == nil || sess.NativeRegistrationHold == "" {
		return
	}
	code := sess.NativeRegistrationHold
	*sess = *before
	sess.NativeRegistrationHold = code
}
