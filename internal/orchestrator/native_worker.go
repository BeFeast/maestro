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
