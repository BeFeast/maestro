package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
	"github.com/befeast/maestro/internal/workerlease"
)

var observeNativeWorkerLaunch = aiexecution.ObserveNativeProcessLaunch
var verifyNativeWorkerTermination = aiexecution.VerifyNativeProcessTermination

func nativeProfileFromReceipt(cfg *config.Config, r *NativeWorkerReceipt) (aiexecution.FileProof, error) {
	b, err := readOwnedRegularNoFollow(filepath.Join(cfg.StateDir, r.Slot+"-run.sh.execution.json"), 128<<10)
	var proof workerExecutionProof
	if err != nil || aiexecution.DecodeStrict(b, &proof) != nil || (proof.Version != 1 && proof.Version != 2) || !proof.Policy.RequireVerifiedRoute || proof.Spec.Registration == nil || *proof.Spec.Registration != *r.Acknowledgement || proof.Spec.ProjectID != r.ProjectID || proof.Spec.Role != r.Request.Role || proof.RuntimeKey != r.Slot || proof.ProcessLeaseUnit != r.ProcessLeaseUnit || proof.Worktree != r.Worktree {
		return aiexecution.FileProof{}, &NativeRegistrationHold{Code: "native_runtime_proof_invalid", LaunchUncertain: true}
	}
	return aiexecution.ContainmentProfilePin(proof.Policy, r.Slot, r.Request.Role)
}

// ReconcileNativeWorkerRuntime recovers the launch/state gap without launching
// or signalling any process. A monitor claim proves launch; exact live service
// evidence adopts it, and exact empty-cgroup evidence records terminal failure
// with local_output_unknown when the monitor was killed before saving output.
// Financial settlement remains a separate authority operation.
func ReconcileNativeWorkerRuntime(cfg *config.Config, s *state.State, slot string) error {
	if cfg == nil || s == nil || !cfg.AIExecution.RequireVerifiedRoute {
		return nil
	}
	sess := s.Sessions[slot]
	if sess == nil {
		return &NativeRegistrationHold{Code: "native_runtime_projection_missing", LaunchUncertain: true}
	}
	generation := sess.WorkerGeneration
	if generation == 0 {
		generation = 1
	}
	dir, unlock, err := lockNativeWorker(cfg, slot)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := readNativeWorkerReceipt(dir, generation)
	if err != nil {
		return err
	}
	if r.ProjectID != cfg.ProjectID || r.Slot != slot || r.IssueNumber != sess.IssueNumber || r.Worktree != sess.Worktree || r.Branch != sess.Branch || (r.Status != "launch_intent" && r.Status != "launched") || r.Acknowledgement == nil {
		return &NativeRegistrationHold{Code: "native_identity_conflict", LaunchUncertain: true}
	}
	if r.OutcomeIntent != nil {
		return &NativeRegistrationHold{Code: "native_generation_sealed", LaunchUncertain: true}
	}
	pin, err := nativeProfileFromReceipt(cfg, r)
	if err != nil {
		return err
	}
	lease := tmuxsession.ProcessLease{Unit: r.ProcessLeaseUnit, Manager: r.ProcessLeaseManager}
	proof, monitorPID, liveErr := observeNativeWorkerLaunch(pin, r.ProjectID, r.Request.NativeSessionID, r.ProcessLeaseUnit)
	if liveErr == nil {
		panePID, path, err := TmuxPaneIdentity(TmuxSessionName(slot))
		if err != nil || !sameCleanPath(path, r.Worktree) || validateExactWorktreeIdentity(cfg.LocalPath, r.Worktree, r.Branch) != nil {
			return &NativeRegistrationHold{Code: "native_host_runtime_unknown", LaunchUncertain: true}
		}
		r.PID, r.Status, r.NativeProcessEvidence = monitorPID, "launched", proof
		if err := persistNativeWorkerReceipt(dir, r); err != nil {
			return err
		}
		sess.PID, sess.TmuxSession, sess.Status = panePID, TmuxSessionName(slot), state.StatusRunning
		sess.WorkerGeneration = generation
		if sess.StartedAt.IsZero() {
			sess.StartedAt = proof.StartedAt
		}
		setSessionProcessLease(sess, lease)
		(&nativeWorkerLaunch{dir: dir, receipt: r}).stamp(sess)
		return state.Save(cfg.StateDir, s)
	}
	active, err := workerProcessLeaseActive(lease)
	if err != nil || active {
		return liveErr
	}
	absent, err := nativeWorkerPaneAbsent(TmuxSessionName(slot))
	if err != nil || !absent {
		return &NativeRegistrationHold{Code: "native_host_runtime_unknown", LaunchUncertain: true}
	}
	if err := verifyHostRunnerAbsent(filepath.Join(cfg.StateDir, slot+"-run.sh")); err != nil {
		return err
	}
	proof, err = verifyNativeWorkerTermination(pin, r.Request.NativeSessionID, r.ProcessLeaseUnit)
	if err != nil {
		return err
	}
	if err := aiexecution.ValidateNativeProcessTermination(*proof, pin, r.Request.NativeSessionID, r.ProcessLeaseUnit); err != nil {
		return err
	}
	r.Status, r.NativeProcessEvidence = "launched", proof
	if err := persistNativeWorkerReceipt(dir, r); err != nil {
		return err
	}
	sess.WorkerGeneration = generation
	setSessionProcessLease(sess, lease)
	(&nativeWorkerLaunch{dir: dir, receipt: r}).stamp(sess)
	if err := markNativeWorkerTerminated(sess); err != nil {
		return err
	}
	sess.PID, sess.TmuxSession, sess.Status = 0, "", state.StatusDead
	if sess.StartedAt.IsZero() {
		sess.StartedAt = proof.StartedAt
	}
	ended := proof.EndedAt
	sess.FinishedAt = &ended
	state.MarkWorkerEnded(sess, ended)
	return state.Save(cfg.StateDir, s)
}

// ScheduleNativeWorkerRecovery is an explicit exact-generation recovery
// decision after OS termination and authority settlement. It queues the normal
// bounded in-place retry path; it never creates a registration or launches.
func ScheduleNativeWorkerRecovery(cfg *config.Config, s *state.State, slot, nativeID string) error {
	if cfg == nil || s == nil {
		return &NativeRegistrationHold{Code: "configuration_invalid"}
	}
	sess := s.Sessions[slot]
	if sess == nil || sess.Status != state.StatusDead || sess.NativeSessionID != nativeID || sess.WorkerGeneration == 0 {
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
	if r.ProjectID != cfg.ProjectID || r.Slot != slot || r.Request.NativeSessionID != nativeID || r.RoleRunID != sess.NativeRoleRunID || r.Outcome == nil || !r.Outcome.NextGenerationAllowed || r.NativeProcessEvidence == nil || r.NativeProcessEvidence.LocalStatus == "launch_intent" {
		return &NativeRegistrationHold{Code: "previous_outcome_unknown"}
	}
	if terminal, err := nativeWorkerTerminated(dir, r); err != nil || !terminal {
		return &NativeRegistrationHold{Code: "native_process_unknown", LaunchUncertain: true}
	}
	if sess.NextRetryAt != nil {
		return nil
	}
	others := *s
	others.Sessions = make(map[string]*state.Session, len(s.Sessions))
	for name, other := range s.Sessions {
		if name != slot {
			others.Sessions[name] = other
		}
	}
	if cfg.MaxRetriesPerIssue > 0 && others.FailedAttemptsForIssue(sess.IssueNumber)+sess.RetryCount >= cfg.MaxRetriesPerIssue {
		return &NativeRegistrationHold{Code: "native_recovery_retry_exhausted"}
	}
	now := time.Now().UTC()
	sess.RetryCount++
	sess.UnexpectedExitRetries++
	sess.RetryReason = state.RetryReasonStalledProgress
	sess.NextRetryAt = &now
	sess.NativeRegistrationHold = ""
	return state.Save(cfg.StateDir, s)
}

func nativeLeaseCleanupHold(cfg *config.Config, lease workerlease.Lease) (bool, error) {
	dir := nativeReceiptDir(cfg.StateDir, lease.Slot)
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	for _, file := range files {
		if !strings.HasPrefix(file.Name(), "generation-") || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		generation, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(file.Name(), "generation-"), ".json"), 10, 64)
		if err != nil || generation == 0 {
			return true, &NativeRegistrationHold{Code: "receipt_invalid", LaunchUncertain: true}
		}
		r, err := readNativeWorkerReceipt(dir, generation)
		if err != nil {
			return true, err
		}
		if r.ProcessLeaseUnit != lease.Unit || r.ProcessLeaseManager != lease.Scope {
			continue
		}
		if r.ProjectID != cfg.ProjectID || r.Slot != lease.Slot {
			return true, &NativeRegistrationHold{Code: "native_identity_conflict", LaunchUncertain: true}
		}
		if r.Status != "launch_intent" && r.Status != "launched" {
			continue
		}
		terminal, err := nativeWorkerTerminated(dir, r)
		if err != nil || !terminal {
			return true, err
		}
		if r.Outcome == nil || !r.Outcome.NextGenerationAllowed {
			return true, nil
		}
	}
	return false, nil
}

func waitNativeWorkerLaunch(cfg *config.Config, slot string, generation uint64, timeout time.Duration) error {
	r, err := readNativeWorkerReceipt(nativeReceiptDir(cfg.StateDir, slot), generation)
	if err != nil {
		return err
	}
	pin, err := nativeProfileFromReceipt(cfg, r)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for {
		_, _, err = observeNativeWorkerLaunch(pin, r.ProjectID, r.Request.NativeSessionID, r.ProcessLeaseUnit)
		if err == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}
