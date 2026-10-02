package orchestrator

import (
	"encoding/json"
	"github.com/befeast/maestro/internal/aiexecution"
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
	if hold.Deferred {
		// A pre-registration lane pause: the caller restores its snapshot
		// (restoreNativeHeldSession) and nothing is persisted.
		sess.NativeLaneDeferred = hold.Code
		log.Printf("[orch] native lane not ready: %s — retrying next cycle", hold.Code)
		return true
	}
	sess.NativeRegistrationHold = hold.Code
	log.Printf("[orch] native worker generation held: %s%s", hold.Code, nativeWorkerExecHoldSuffix(sess))
	return true
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
