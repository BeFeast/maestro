package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"path/filepath"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
)

// NativeOperatorRecoveryRecord authorizes one successor of the retired native
// generation. Session digests make a failed state save replayable without
// reusing the authorization after the normal launch path has consumed it.
type NativeOperatorRecoveryRecord struct {
	RetirementDigest    string    `json:"retirement_digest"`
	NextGeneration      uint64    `json:"next_generation"`
	ScheduledAt         time.Time `json:"scheduled_at"`
	BeforeSessionDigest string    `json:"before_session_digest"`
	QueuedSessionDigest string    `json:"queued_session_digest"`
}

func operatorRecoverySessionDigest(sess *state.Session) string {
	b, _ := json.Marshal(sess)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func validateNativeOperatorRecovery(r *NativeWorkerReceipt) error {
	p := r.OperatorRecovery
	if p == nil {
		return nil
	}
	invalid := &NativeRegistrationHold{Code: "operator_recovery_receipt_invalid", LaunchUncertain: true}
	if r.Generation == math.MaxUint64 || p.NextGeneration != r.Generation+1 || p.ScheduledAt.IsZero() || r.Outcome == nil ||
		r.Outcome.OperatorRetirement == nil || r.Outcome.Outcome != "operator_retired_unknown" ||
		p.RetirementDigest != r.Outcome.OperatorRetirement.RetirementDigest {
		return invalid
	}
	for _, value := range []string{p.BeforeSessionDigest, p.QueuedSessionDigest} {
		b, err := hex.DecodeString(value)
		if err != nil || len(b) != sha256.Size || hex.EncodeToString(b) != value {
			return invalid
		}
	}
	return nil
}

// ScheduleNativeOperatorRecovery consumes an explicit retired-unknown outcome
// into one normal in-place restart. The automatic retry budget, failure history,
// and unknown provider accounting remain unchanged. Ordinary reconciliation must
// never call this operator-only scheduling API automatically.
func ScheduleNativeOperatorRecovery(cfg *config.Config, s *state.State, slot, nativeID string) error {
	if cfg == nil || s == nil || !cfg.AIExecution.RequireVerifiedRoute {
		return &NativeRegistrationHold{Code: "configuration_invalid"}
	}
	sess := s.Sessions[slot]
	if sess == nil || (sess.Status != state.StatusDead && sess.Status != state.StatusFailed) || sess.NativeSessionID != nativeID || sess.WorkerGeneration == 0 || sess.PID != 0 || sess.TmuxSession != "" {
		return &NativeRegistrationHold{Code: "native_recovery_projection_conflict"}
	}
	dir, unlock, err := lockNativeWorker(cfg, slot)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := readNativeWorkerReceipt(dir, sess.WorkerGeneration)
	if err != nil {
		return err
	}
	if r.ProjectID != cfg.ProjectID || r.Slot != slot || r.Request.NativeSessionID != nativeID || r.RoleRunID != sess.NativeRoleRunID || r.IssueNumber != sess.IssueNumber || r.Worktree != sess.Worktree || r.Branch != sess.Branch ||
		r.Outcome == nil || r.OutcomeIntent == nil || r.Outcome.Outcome != "operator_retired_unknown" || r.Outcome.OperatorRetirement == nil ||
		admissioncontrol.ValidateNativeOutcome(*r.Outcome, *r.OutcomeIntent) != nil || r.NativeProcessEvidence == nil || r.NativeProcessEvidence.LocalStatus == "launch_intent" {
		return &NativeRegistrationHold{Code: "operator_retirement_unproven"}
	}
	proof := r.NativeProcessEvidence
	attestation := r.Outcome.OperatorRetirement.LocalTerminationAttestation
	want := admissioncontrol.LocalTerminationAttestation{SchemaVersion: 1, NativeSessionID: nativeID, Unit: proof.Unit, Cgroup: proof.Cgroup, BootID: proof.BootID, InvocationID: proof.InvocationID, ProofDigest: proof.Digest}
	if attestation != want || !attestation.Valid(nativeID) {
		return &NativeRegistrationHold{Code: "operator_retirement_termination_mismatch"}
	}
	if terminal, err := nativeWorkerTerminated(dir, r); err != nil || !terminal {
		return &NativeRegistrationHold{Code: "native_process_unknown", LaunchUncertain: true}
	}
	prior := *sess
	before := operatorRecoverySessionDigest(sess)
	if r.OperatorRecovery != nil {
		if before == r.OperatorRecovery.QueuedSessionDigest {
			return nil
		}
		if before != r.OperatorRecovery.BeforeSessionDigest {
			return &NativeRegistrationHold{Code: "operator_recovery_already_consumed"}
		}
	} else {
		if sess.NextRetryAt != nil || sess.RetryReason == state.RetryReasonOperatorRestart || r.Generation == math.MaxUint64 {
			return &NativeRegistrationHold{Code: "native_recovery_projection_conflict"}
		}
		r.OperatorRecovery = &NativeOperatorRecoveryRecord{RetirementDigest: r.Outcome.OperatorRetirement.RetirementDigest,
			NextGeneration: r.Generation + 1, ScheduledAt: time.Now().UTC(), BeforeSessionDigest: before}
	}
	queued := prior
	queued.Status = state.StatusDead
	queued.RetryReason = state.RetryReasonOperatorRestart
	queued.NextRetryAt = &r.OperatorRecovery.ScheduledAt
	queued.NativeRegistrationHold = ""
	digest := operatorRecoverySessionDigest(&queued)
	if r.OperatorRecovery.QueuedSessionDigest == "" {
		r.OperatorRecovery.QueuedSessionDigest = digest
		if err := persistNativeWorkerReceipt(dir, r); err != nil {
			return err
		}
	} else if r.OperatorRecovery.QueuedSessionDigest != digest {
		return &NativeRegistrationHold{Code: "native_recovery_projection_conflict"}
	}
	*sess = queued
	if err := state.Save(cfg.StateDir, s); err != nil {
		*sess = prior
		return err
	}
	return nil
}

// ObserveNativeWorkerTermination verifies the exact original process while its
// execution profile is still installed. It never alters the worker receipt or
// session, contacts the authority, or launches/signals a process. The verifier
// may persist a recovered OS termination claim when the monitor lost its reply.
func ObserveNativeWorkerTermination(cfg *config.Config, slot string, generation uint64, nativeID string) (*aiexecution.NativeProcessTermination, error) {
	if cfg == nil || !cfg.AIExecution.RequireVerifiedRoute || generation == 0 {
		return nil, &NativeRegistrationHold{Code: "configuration_invalid"}
	}
	dir, unlock, err := lockNativeWorker(cfg, slot)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := readNativeWorkerReceipt(dir, generation)
	if err != nil {
		return nil, err
	}
	if r.ProjectID != cfg.ProjectID || r.Slot != slot || r.Generation != generation || r.Request.NativeSessionID != nativeID || r.Acknowledgement == nil || (r.Status != "launched" && r.Status != "launch_intent") {
		return nil, &NativeRegistrationHold{Code: "native_identity_conflict", LaunchUncertain: true}
	}
	return observeNativeWorkerTerminationLocked(cfg, r)
}

// The caller holds the receipt lock and has checked the requested identity.
func observeNativeWorkerTerminationLocked(cfg *config.Config, r *NativeWorkerReceipt) (*aiexecution.NativeProcessTermination, error) {
	pin, err := nativeProfileFromReceipt(cfg, r)
	if err != nil {
		return nil, err
	}
	lease := tmuxsession.ProcessLease{Unit: r.ProcessLeaseUnit, Manager: r.ProcessLeaseManager}
	if active, err := workerProcessLeaseActive(lease); err != nil || active {
		return nil, &NativeRegistrationHold{Code: "native_process_unknown", LaunchUncertain: true}
	}
	if absent, err := nativeWorkerPaneAbsent(TmuxSessionName(r.Slot)); err != nil || !absent {
		return nil, &NativeRegistrationHold{Code: "native_host_runtime_unknown", LaunchUncertain: true}
	}
	if err := verifyHostRunnerAbsent(filepath.Join(cfg.StateDir, r.Slot+"-run.sh")); err != nil {
		return nil, err
	}
	proof, err := verifyNativeWorkerTermination(pin, r.Request.NativeSessionID, r.ProcessLeaseUnit)
	if err != nil {
		return nil, err
	}
	if proof == nil {
		return nil, &NativeRegistrationHold{Code: "native_process_unknown", LaunchUncertain: true}
	}
	if err := aiexecution.ValidateNativeProcessTermination(*proof, pin, r.Request.NativeSessionID, r.ProcessLeaseUnit); err != nil {
		return nil, err
	}
	return proof, nil
}
