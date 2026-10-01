package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
	"github.com/befeast/maestro/internal/workerlease"
)

var observeNativeWorkerLaunch = aiexecution.ObserveNativeProcessLaunch
var verifyNativeWorkerTermination = aiexecution.VerifyNativeProcessTermination
var verifyNativeWorkerPrelaunchAbsence = aiexecution.VerifyNativePrelaunchAbsence

// Recover only the lost local termination projection of a normally settled
// generation. Authority settlement alone never proves that its process ended.
// This preserves the sealed outcome, session status, retry history and PR;
// deciding to repair or retry remains a separate operation.
func reconcileSealedNativeWorkerTermination(cfg *config.Config, dir string, r *NativeWorkerReceipt, sess *state.Session) error {
	if !cfg.AIExecution.RequireVerifiedRoute || r.Status != "launched" || r.Acknowledgement == nil ||
		r.ProjectID != cfg.ProjectID || r.Generation != sess.WorkerGeneration || r.RoleRunID != sess.NativeRoleRunID ||
		r.Request.NativeSessionID != sess.NativeSessionID || r.IssueNumber != sess.IssueNumber ||
		r.Worktree != sess.Worktree || r.Branch != sess.Branch || sess.NativeReceiptDir != dir ||
		sess.ProcessLeaseUnit != "" || sess.ProcessLeaseManager != "" ||
		persistedNativeGenerationOutcome(cfg, r) != nil || r.Outcome.OperatorRetirement != nil {
		return &NativeRegistrationHold{Code: "native_identity_conflict", LaunchUncertain: true}
	}
	proof, err := observeNativeWorkerTerminationLocked(cfg, r)
	if err != nil {
		return err
	}
	r.NativeProcessEvidence = proof
	if err := persistNativeWorkerReceipt(dir, r); err != nil {
		return err
	}
	// Use the exact verified receipt lease for the marker without restoring a
	// live lease to the session or weakening markNativeWorkerTerminated's fence.
	terminal := *sess
	setSessionProcessLease(&terminal, tmuxsession.ProcessLease{Unit: r.ProcessLeaseUnit, Manager: r.ProcessLeaseManager})
	return markNativeWorkerTerminated(&terminal)
}

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
		// The projected generation is already sealed. An unresolved_launch hold
		// on top of it means the successor launch intent was persisted but the
		// session projection was restored to this snapshot (#1235); inspect
		// that successor receipt instead of re-reporting the sealed parent.
		if sess.NativeRegistrationHold == "unresolved_launch" {
			return reconcileAbandonedNativeLaunch(cfg, s, slot, sess, dir, r)
		}
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

// NativeLaunchAbandonmentRecord is the durable sidecar written next to an
// archived successor receipt whose host launch never produced a process. It
// keeps the exact authority seal so the abandonment can be audited without the
// original receipt name.
type NativeLaunchAbandonmentRecord struct {
	SchemaVersion        int                            `json:"schema_version"`
	AbandonedAt          time.Time                      `json:"abandoned_at"`
	Slot                 string                         `json:"slot"`
	Generation           uint64                         `json:"generation"`
	RoleRunID            string                         `json:"role_run_id"`
	ParentRoleRunID      string                         `json:"parent_role_run_id"`
	NativeSessionID      string                         `json:"native_session_id"`
	ProcessLeaseUnit     string                         `json:"process_lease_unit"`
	PreviousStatus       string                         `json:"previous_status"`
	ReceiptSHA256        string                         `json:"receipt_sha256"`
	ExecutionProofSHA256 string                         `json:"execution_proof_sha256"`
	OutcomeIntent        admissioncontrol.SealRequest   `json:"outcome_intent"`
	Outcome              admissioncontrol.NativeOutcome `json:"outcome"`
	WorkerExecHold       string                         `json:"worker_exec_hold,omitempty"`
}

func nativeLaunchAbandonmentPrefix(generation uint64, index int) string {
	return fmt.Sprintf("launch-abandoned-g%d-%d", generation, index)
}

// reconcileAbandonedNativeLaunch resolves the launch/state gap left by an
// in-place respawn whose host runner refused before any unit, claim or pane
// existed (#1235). The successor receipt (generation+1, child of the projected
// role run) is still launch_intent while the session snapshot was restored to
// the sealed parent. With every absence proof present the exact registration
// is sealed at the authority (zero physical attempts), the receipt and its
// execution proof are archived, the orphaned scratch lease is released and the
// hold is cleared so the ordinary retry path can mint a fresh generation. No
// process is launched or signalled; retry counters and receipts are preserved.
func reconcileAbandonedNativeLaunch(cfg *config.Config, s *state.State, slot string, sess *state.Session, dir string, prior *NativeWorkerReceipt) error {
	sealed := &NativeRegistrationHold{Code: "native_generation_sealed", LaunchUncertain: true}
	if prior.Generation == math.MaxUint64 || prior.Status != "launched" || prior.RoleRunID != sess.NativeRoleRunID || prior.Request.NativeSessionID != sess.NativeSessionID {
		return sealed
	}
	next := prior.Generation + 1
	r, err := readNativeWorkerReceipt(dir, next)
	if errors.Is(err, os.ErrNotExist) {
		// A crash between archiving and saving the cleared projection leaves
		// only the archive; replay the projection from that exact record.
		recorded, err := nativeLaunchAbandonmentRecorded(dir, next, sess.NativeRoleRunID)
		if err != nil {
			return err
		}
		if !recorded {
			return sealed
		}
		sess.NativeRegistrationHold = ""
		return state.Save(cfg.StateDir, s)
	}
	if err != nil {
		return err
	}
	if r.ProjectID != cfg.ProjectID || r.Slot != slot || r.IssueNumber != sess.IssueNumber || r.Worktree != sess.Worktree || r.Branch != sess.Branch ||
		r.ParentRoleRunID != sess.NativeRoleRunID || r.Acknowledgement == nil {
		return &NativeRegistrationHold{Code: "native_identity_conflict", LaunchUncertain: true}
	}
	if r.Status != "launch_intent" || r.PID != 0 || r.OutcomeIntent != nil || r.Outcome != nil || r.NativeProcessEvidence != nil || r.OperatorRecovery != nil ||
		r.LogFile != filepath.Join(state.LogDir(cfg.StateDir), slot+".log") {
		return &NativeRegistrationHold{Code: "native_launch_abandonment_unproven", LaunchUncertain: true}
	}
	// Any terminal marker, including a malformed one, contradicts "never launched".
	if _, err := os.Lstat(filepath.Join(dir, nativeReceiptName(next)+".terminated")); !errors.Is(err, os.ErrNotExist) {
		return &NativeRegistrationHold{Code: "native_recovery_terminal_conflict", LaunchUncertain: true}
	}
	// Absence proofs: exact execution proof for this receipt, no surviving
	// monitor claim, inactive OS lease, absent pane and absent host runner.
	proofPath := filepath.Join(cfg.StateDir, slot+"-run.sh.execution.json")
	proofBytes, err := readOwnedRegularNoFollow(proofPath, 128<<10)
	if err != nil {
		return &NativeRegistrationHold{Code: "native_runtime_proof_invalid", LaunchUncertain: true}
	}
	pin, err := nativeProfileFromReceipt(cfg, r)
	if err != nil {
		return err
	}
	if err := verifyNativeWorkerPrelaunchAbsence(pin, r.ProjectID, r.Request.NativeSessionID, r.ProcessLeaseUnit); err != nil {
		return err
	}
	lease := tmuxsession.ProcessLease{Unit: r.ProcessLeaseUnit, Manager: r.ProcessLeaseManager}
	if active, err := workerProcessLeaseActive(lease); err != nil || active {
		return &NativeRegistrationHold{Code: "native_process_unknown", LaunchUncertain: true}
	}
	if absent, err := nativeWorkerPaneAbsent(TmuxSessionName(slot)); err != nil || !absent {
		return &NativeRegistrationHold{Code: "native_host_runtime_unknown", LaunchUncertain: true}
	}
	if err := verifyHostRunnerAbsent(filepath.Join(cfg.StateDir, slot+"-run.sh")); err != nil {
		return err
	}
	// Retire the registration at the authority. Sealing is idempotent and must
	// confirm zero physical attempts: any recorded request contradicts the local
	// absence evidence and keeps the receipt held for operator inspection.
	client, err := nativeClient(cfg)
	if err != nil {
		return err
	}
	seal := admissioncontrol.SealRequest{Binding: r.Request.Binding, RegistrationVersion: r.Acknowledgement.RegistrationVersion}
	outcome, err := sealNativeWorker(client, seal)
	if err != nil {
		return &NativeRegistrationHold{Code: "outcome_authority_unavailable", LaunchUncertain: true}
	}
	if admissioncontrol.ValidateNativeOutcome(outcome, seal) != nil {
		return &NativeRegistrationHold{Code: "outcome_response_invalid", LaunchUncertain: true}
	}
	if !outcome.NextGenerationAllowed || outcome.PhysicalAttempts != 0 || outcome.OperatorRetirement != nil {
		return &NativeRegistrationHold{Code: "native_launch_abandonment_contradicted", LaunchUncertain: true}
	}
	// Archive before any destructive step. The receipt itself is renamed, not
	// deleted, after its sidecars are durable.
	before, err := readOwnedRegularNoFollow(filepath.Join(dir, nativeReceiptName(next)), 64<<10)
	if err != nil {
		return &NativeRegistrationHold{Code: "receipt_invalid", LaunchUncertain: true}
	}
	index := 1
	for {
		if _, err := os.Lstat(filepath.Join(dir, nativeLaunchAbandonmentPrefix(next, index)+".receipt.json")); errors.Is(err, os.ErrNotExist) {
			break
		}
		index++
		if index > 64 {
			return &NativeRegistrationHold{Code: "receipt_persistence_failed", LaunchUncertain: true}
		}
	}
	prefix := nativeLaunchAbandonmentPrefix(next, index)
	receiptSum, proofSum := sha256.Sum256(before), sha256.Sum256(proofBytes)
	record := NativeLaunchAbandonmentRecord{SchemaVersion: 1, AbandonedAt: time.Now().UTC(), Slot: slot, Generation: next,
		RoleRunID: r.RoleRunID, ParentRoleRunID: r.ParentRoleRunID, NativeSessionID: r.Request.NativeSessionID, ProcessLeaseUnit: r.ProcessLeaseUnit,
		PreviousStatus: r.Status, ReceiptSHA256: hex.EncodeToString(receiptSum[:]), ExecutionProofSHA256: hex.EncodeToString(proofSum[:]),
		OutcomeIntent: seal, Outcome: outcome, WorkerExecHold: NativeWorkerExecHold(r.LogFile)}
	recordBytes, err := json.Marshal(record)
	if err != nil {
		return &NativeRegistrationHold{Code: "receipt_persistence_failed", LaunchUncertain: true}
	}
	if writeFileAtomicMode(dir, filepath.Join(dir, prefix+".execution.json"), string(proofBytes), 0600) != nil ||
		writeFileAtomicMode(dir, filepath.Join(dir, prefix+".outcome.json"), string(recordBytes), 0600) != nil ||
		syncNativeDir(dir) != nil {
		return &NativeRegistrationHold{Code: "receipt_persistence_failed", LaunchUncertain: true}
	}
	// Release only the exact scratch manifest of this never-launched lease. A
	// missing manifest means it was already released; anything ambiguous stays
	// for the exact lease reconciler, which no longer sees a native hold here.
	if err := releaseAbandonedWorkerScratch(cfg, slot, lease); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(dir, nativeReceiptName(next)), filepath.Join(dir, prefix+".receipt.json")); err != nil {
		return &NativeRegistrationHold{Code: "receipt_persistence_failed", LaunchUncertain: true}
	}
	if err := syncNativeDir(dir); err != nil {
		return &NativeRegistrationHold{Code: "receipt_persistence_failed", LaunchUncertain: true}
	}
	sess.NativeRegistrationHold = ""
	return state.Save(cfg.StateDir, s)
}

func releaseAbandonedWorkerScratch(cfg *config.Config, slot string, lease tmuxsession.ProcessLease) error {
	if !cfg.WorkerRuntime.IsolatedEnabled() {
		return nil
	}
	leases, _, err := workerlease.List(workerLeaseProjectRoot(cfg))
	if err != nil {
		return &NativeRegistrationHold{Code: "scratch_identity_unknown", LaunchUncertain: true}
	}
	var match *workerlease.Lease
	for i := range leases {
		candidate := leases[i]
		if candidate.ProjectKey != workerLeaseProjectKey(cfg) || candidate.Slot != slot || candidate.Unit != lease.Unit || candidate.Scope != lease.Manager {
			continue
		}
		if match != nil {
			return &NativeRegistrationHold{Code: "scratch_identity_unknown", LaunchUncertain: true}
		}
		match = &candidate
	}
	if match == nil {
		return nil
	}
	if err := terminateWorkerProcessLease(lease); err != nil {
		return &NativeRegistrationHold{Code: "native_process_unknown", LaunchUncertain: true}
	}
	if err := workerScratchCleanup(match.ManifestPath, match.ID); err != nil {
		return &NativeRegistrationHold{Code: "scratch_release_failed", LaunchUncertain: true}
	}
	return nil
}

// nativeLaunchAbandonmentRecorded reports whether generation was already
// archived as an abandoned launch of a child of parentRoleRunID.
func nativeLaunchAbandonmentRecorded(dir string, generation uint64, parentRoleRunID string) (bool, error) {
	for index := 1; index <= 64; index++ {
		prefix := nativeLaunchAbandonmentPrefix(generation, index)
		if _, err := os.Lstat(filepath.Join(dir, prefix+".receipt.json")); errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		b, err := readOwnedRegularNoFollow(filepath.Join(dir, prefix+".outcome.json"), 64<<10)
		if err != nil {
			return false, &NativeRegistrationHold{Code: "receipt_invalid", LaunchUncertain: true}
		}
		var record NativeLaunchAbandonmentRecord
		if aiexecution.DecodeStrict(b, &record) != nil || record.SchemaVersion != 1 || record.Generation != generation ||
			admissioncontrol.ValidateNativeOutcome(record.Outcome, record.OutcomeIntent) != nil || record.Outcome.PhysicalAttempts != 0 {
			return false, &NativeRegistrationHold{Code: "receipt_invalid", LaunchUncertain: true}
		}
		if record.ParentRoleRunID == parentRoleRunID {
			return true, nil
		}
	}
	return false, nil
}

var nativeWorkerExecHoldLine = regexp.MustCompile(`AI execution held: ([a-z0-9_]{1,64})\b`)

// NativeWorkerExecHold returns the last hold code the host runner wrote to the
// worker log ("AI execution held: <code>"), or "" when none is present. The log
// is worker-writable, so only a bounded hold-code token is ever surfaced.
func NativeWorkerExecHold(logFile string) string {
	if logFile == "" || !filepath.IsAbs(logFile) {
		return ""
	}
	f, err := os.OpenFile(logFile, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !nativeOwned(info) {
		return ""
	}
	const window = 64 << 10
	if info.Size() > window {
		if _, err := f.Seek(info.Size()-window, io.SeekStart); err != nil {
			return ""
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, window))
	if err != nil {
		return ""
	}
	matches := nativeWorkerExecHoldLine.FindAllSubmatch(b, -1)
	if len(matches) == 0 {
		return ""
	}
	return string(bytes.TrimSpace(matches[len(matches)-1][1]))
}
