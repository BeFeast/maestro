package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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
	wedge := nativePrelaunchWedgeHold(sess.NativeRegistrationHold)
	if wedge && r.Status == "launched" && sess.ProcessLeaseUnit == "" && sess.ProcessLeaseManager == "" &&
		(r.OutcomeIntent == nil || previousNativeGenerationOutcome(cfg, r) != nil) {
		// A wedge hold on a lease-less projected generation whose seal has not
		// completed: the termination fence held before it could prove, seal and
		// record this generation (lock contention, authority unavailable, an
		// observation that did not verify yet), or the seal attempt persisted
		// its intent and lost the reply. Nothing else seals an unsealed
		// generation under a hold, so the hold would be sticky; repeat the
		// fence's own sequence here, under the lock, every cycle. Unproven
		// termination keeps the hold with nothing persisted.
		sealed, err := sealNativeGenerationTerminationLocked(cfg, dir, slot, sess, r)
		if err != nil {
			return err
		}
		return reconcileAbandonedNativeLaunch(cfg, s, slot, sess, dir, sealed)
	}
	if r.OutcomeIntent != nil {
		// The projected generation is already sealed. An unresolved_launch hold
		// on top of it means the successor launch intent was persisted but the
		// session projection was restored to this snapshot (#1235); a pre-launch
		// wedge hold means the successor was registered and the respawn then
		// held on this generation's termination. Inspect that successor receipt
		// instead of re-reporting the sealed parent.
		if sess.NativeRegistrationHold == "unresolved_launch" || wedge {
			return reconcileAbandonedNativeLaunch(cfg, s, slot, sess, dir, r)
		}
		return &NativeRegistrationHold{Code: "native_generation_sealed", LaunchUncertain: true}
	}
	if wedge && r.Status == "launched" {
		// The session still owns the exact lease of the unsealed projected
		// generation: StopProcess and the lease termination path prove and mark
		// that termination, and the seal follows it. Observing a live launch
		// here could re-project an already terminal or pr_open session.
		return &NativeRegistrationHold{Code: "previous_outcome_unknown"}
	}
	// A launch_intent projected generation under a wedge hold never had its
	// launch adopted by the session, so no lease path observes it and the hold
	// would be sticky. It takes the ordinary launch/state gap recovery below,
	// which adopts the exact live launch or records its verified termination
	// and clears the hold either way.
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
	ClearedHold          string                         `json:"cleared_hold,omitempty"`
}

func nativeLaunchAbandonmentPrefix(generation uint64, index int) string {
	return fmt.Sprintf("launch-abandoned-g%d-%d", generation, index)
}

// nativePrelaunchWedgeHold reports the holds under which a retained slot may
// carry a never-launched successor registration, or none at all, on top of a
// projected generation whose termination still has to be proven and sealed:
//
//   - native_process_identity_missing: the termination fence held before any
//     successor existed, or (before the fence) StopProcess held on the
//     missing terminal marker after generation+1 was registered.
//   - previous_outcome_unknown: raised before any successor exists by
//     prepareNativeWorker and by the fence when the projected generation's
//     settlement is unavailable or denies a next generation; raised after a
//     successor was registered by Stop's destructive-outcome check in the
//     fallover respawn. Without a successor the reconciliation reduces to the
//     same clearance the daemon's own previous_outcome_unknown branch performs
//     (sealed with next_generation_allowed and terminal), plus sealing a
//     lease-less unsealed generation once its termination is proven; with a
//     registered successor it is the pre-launch wedge.
//
// Both are resolved only from a sealed, terminal projected generation; every
// successor abandonment additionally needs the full absence proof and an
// authority seal confirming zero physical attempts.
func nativePrelaunchWedgeHold(code string) bool {
	return code == "native_process_identity_missing" || code == "previous_outcome_unknown"
}

// reconcileAbandonedNativeLaunch resolves the launch/state gap left by an
// in-place respawn whose successor never produced a process. Two shapes:
//
//   - #1235: the host runner refused before any unit, claim or pane existed.
//     The successor receipt (generation+1, child of the projected role run) is
//     still launch_intent while the session snapshot was restored to the
//     sealed parent under an unresolved_launch hold.
//   - pre-launch wedge: the respawn registered the successor and then
//     StopProcess held on the projected generation's missing terminal marker.
//     The successor receipt is still registered (no log file, no execution
//     proof, no lease, no claim) under a native_process_identity_missing or
//     previous_outcome_unknown hold; the projected generation has since been
//     sealed and proven terminal by the ordinary hold path.
//
// With every absence proof present the exact registration is sealed at the
// authority (zero physical attempts, no_dispatch), the receipt and, when it
// exists, its execution proof are archived, the orphaned scratch lease is
// released and the hold is cleared so the ordinary retry path can mint a fresh
// generation. A wedge hold with no successor registration at all is cleared
// from the sealed, terminal projected generation alone. No process is launched
// or signalled; retry counters and receipts are preserved.
func reconcileAbandonedNativeLaunch(cfg *config.Config, s *state.State, slot string, sess *state.Session, dir string, prior *NativeWorkerReceipt) error {
	sealed := &NativeRegistrationHold{Code: "native_generation_sealed", LaunchUncertain: true}
	if prior.Generation == math.MaxUint64 || prior.Status != "launched" || prior.RoleRunID != sess.NativeRoleRunID || prior.Request.NativeSessionID != sess.NativeSessionID {
		return sealed
	}
	hold := sess.NativeRegistrationHold
	wedge := nativePrelaunchWedgeHold(hold)
	if wedge {
		// The wedge is resolved only from a settled, terminal projected
		// generation: the seal proves accounting, the marker proves the exact
		// OS termination recorded by the sealed-termination reconciliation.
		// Until both exist the hold is re-reported unchanged.
		if previousNativeGenerationOutcome(cfg, prior) != nil || prior.Outcome.OperatorRetirement != nil {
			return &NativeRegistrationHold{Code: "previous_outcome_unknown"}
		}
		if terminal, err := nativeWorkerTerminated(dir, prior); err != nil || !terminal {
			return &NativeRegistrationHold{Code: "native_process_identity_missing", LaunchUncertain: true}
		}
	}
	next := prior.Generation + 1
	r, err := readNativeWorkerReceipt(dir, next)
	if errors.Is(err, os.ErrNotExist) {
		if wedge {
			// No successor identity exists: the hold was raised before any
			// registration (termination fence) or its registration was already
			// archived. The projected generation is proven settled and
			// terminal, so the ordinary retry path may mint the next one.
			log.Printf("[worker] native hold %s cleared for %s: generation %d is sealed and terminal and no successor registration exists", hold, slot, prior.Generation)
			sess.NativeRegistrationHold = ""
			return state.Save(cfg.StateDir, s)
		}
		// A crash between archiving and saving the cleared projection leaves
		// only the archive; replay the projection from that exact record. The
		// successor is identified by the slot's execution proof, which still
		// names its native session, not by any archive sharing the parent.
		recorded, err := nativeLaunchAbandonmentRecorded(cfg, dir, slot, next, sess)
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
	// launch_intent proves the runner was invoked but no process was created
	// (#1235); registered proves setup stopped before any launch intent. The
	// wedge holds accept both; unresolved_launch only ever sees launch_intent.
	launchIntent := r.Status == "launch_intent" && (hold == "unresolved_launch" || wedge)
	registered := r.Status == "registered" && wedge
	if (!launchIntent && !registered) || r.PID != 0 || r.OutcomeIntent != nil || r.Outcome != nil || r.NativeProcessEvidence != nil || r.OperatorRecovery != nil ||
		(launchIntent && r.LogFile != filepath.Join(state.LogDir(cfg.StateDir), slot+".log")) || (registered && r.LogFile != "") {
		return &NativeRegistrationHold{Code: "native_launch_abandonment_unproven", LaunchUncertain: true}
	}
	// Any terminal marker, including a malformed one, contradicts "never launched".
	if _, err := os.Lstat(filepath.Join(dir, nativeReceiptName(next)+".terminated")); !errors.Is(err, os.ErrNotExist) {
		return &NativeRegistrationHold{Code: "native_recovery_terminal_conflict", LaunchUncertain: true}
	}
	// Absence proofs: no surviving monitor claim, inactive OS lease, absent
	// pane and absent host runner. A launch_intent receipt is additionally
	// pinned by its exact execution proof; a registered receipt never reached
	// that proof (the slot's proof, if any, still describes the projected
	// generation), so its containment profile is pinned from the live policy
	// exactly as its launch would have pinned it.
	proofPath := filepath.Join(cfg.StateDir, slot+"-run.sh.execution.json")
	var proofBytes []byte
	var pin aiexecution.FileProof
	if launchIntent {
		proofBytes, err = readOwnedRegularNoFollow(proofPath, 128<<10)
		if err != nil {
			return &NativeRegistrationHold{Code: "native_runtime_proof_invalid", LaunchUncertain: true}
		}
		pin, err = nativeProfileFromReceipt(cfg, r)
	} else {
		pin, err = aiexecution.ContainmentProfilePin(cfg.AIExecution, slot, r.Request.Role)
	}
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
	before, err := readOwnedRegularNoFollow(filepath.Join(dir, nativeReceiptName(next)), 64<<10)
	if err != nil {
		return &NativeRegistrationHold{Code: "receipt_invalid", LaunchUncertain: true}
	}
	receiptSum := sha256.Sum256(before)
	proofSHA := ""
	if proofBytes != nil {
		proofSum := sha256.Sum256(proofBytes)
		proofSHA = hex.EncodeToString(proofSum[:])
	}
	// Persist the exact seal request before contacting the authority, as the
	// ordinary outcome path does through the receipt's outcome_intent: a crash
	// after the request was sent must leave durable evidence of which binding
	// was retired, so the retry re-seals the same registration idempotently.
	prefix, intent, err := nativeLaunchAbandonmentIntent(dir, slot, next, r, hex.EncodeToString(receiptSum[:]))
	if err != nil {
		return err
	}
	seal := intent.OutcomeIntent
	outcome, err := sealNativeWorker(client, seal)
	if err != nil {
		return &NativeRegistrationHold{Code: "outcome_authority_unavailable", LaunchUncertain: true}
	}
	if admissioncontrol.ValidateNativeOutcome(outcome, seal) != nil {
		return &NativeRegistrationHold{Code: "outcome_response_invalid", LaunchUncertain: true}
	}
	if !outcome.NextGenerationAllowed || outcome.PhysicalAttempts != 0 || outcome.Outcome != "no_dispatch" || outcome.OperatorRetirement != nil {
		return &NativeRegistrationHold{Code: "native_launch_abandonment_contradicted", LaunchUncertain: true}
	}
	// Archive before any destructive step. The receipt itself is renamed, not
	// deleted, after its sidecars are durable.
	record := NativeLaunchAbandonmentRecord{SchemaVersion: 1, AbandonedAt: time.Now().UTC(), Slot: slot, Generation: next,
		RoleRunID: r.RoleRunID, ParentRoleRunID: r.ParentRoleRunID, NativeSessionID: r.Request.NativeSessionID, ProcessLeaseUnit: r.ProcessLeaseUnit,
		PreviousStatus: r.Status, ReceiptSHA256: hex.EncodeToString(receiptSum[:]), ExecutionProofSHA256: proofSHA,
		OutcomeIntent: seal, Outcome: outcome, WorkerExecHold: NativeWorkerExecHold(r.LogFile), ClearedHold: hold}
	recordBytes, err := json.Marshal(record)
	if err != nil {
		return &NativeRegistrationHold{Code: "receipt_persistence_failed", LaunchUncertain: true}
	}
	if proofBytes != nil && writeFileAtomicMode(dir, filepath.Join(dir, prefix+".execution.json"), string(proofBytes), 0600) != nil {
		return &NativeRegistrationHold{Code: "receipt_persistence_failed", LaunchUncertain: true}
	}
	if writeFileAtomicMode(dir, filepath.Join(dir, prefix+".outcome.json"), string(recordBytes), 0600) != nil ||
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
	log.Printf("[worker] native launch abandoned for %s: generation %d (%s) sealed with zero attempts and archived as %s; hold %s cleared", slot, next, r.Status, prefix, hold)
	sess.NativeRegistrationHold = ""
	return state.Save(cfg.StateDir, s)
}

// ensureNativeGenerationTerminalBeforeSuccessor fences every path that mints
// generation+1 in a retained slot (in-place respawn, fallover respawn, phase
// transition) against the pre-launch wedge seen on 2026-10-01/02: the scratch
// lease reconciler releases a cleanly exited worker's exact lease without
// writing the native terminal marker, so the session projects a launched
// generation with no lease and no marker. prepareNativeWorker then seals that
// generation and registers generation+1, and StopProcess holds on the missing
// marker with the successor already registered. The exact OS termination is
// therefore proven and recorded here, before any successor identity exists:
// observe the original pinned runtime (never signalling), seal the exact
// binding (what prepareNativeWorker does next anyway), then persist the
// termination evidence and marker, all under the receipt lock. The session
// projection itself is never modified here: status, PID, pane and lease are
// left to the successor launch (or to StopProcess), so a phase transition of a
// running session cannot flip it to dead.
//
// Anything that stops short of a recorded termination returns the same
// native_process_identity_missing hold StopProcess would return, with nothing
// minted. That covers genuinely unproven termination (active lease, surviving
// pane or host runner, no verified termination proof, no verified route) as
// well as transient failures before the proof is recorded (receipt lock
// contention, authority unavailable, persistence errors): the daemon routes
// this hold to ReconcileNativeWorkerRuntime, which repeats the same sequence
// every cycle and clears the hold once the generation is sealed and terminal,
// so a transient failure is never sticky. Only an identity conflict and an
// authority settlement that denies a next generation are reported as such.
// An absent projected receipt (projected_receipt_missing), or a receipt or
// terminal marker that cannot be read or decoded (projected_receipt_undecodable),
// is likewise a hold, never a plain error: the callers' error path records a
// failed respawn, which this gap is not.
func ensureNativeGenerationTerminalBeforeSuccessor(cfg *config.Config, slot string, sess *state.Session) error {
	if cfg == nil || sess == nil || sess.NativeRoleRunID == "" || cfg.WorkerNativeSessionRegistration == nil || sess.WorkerGeneration == 0 {
		return nil
	}
	if _, hasLease, err := sessionProcessLease(sess); err != nil || hasLease {
		// An exact lease is terminated and marked by StopProcess itself; a
		// malformed one is reported there as well.
		return nil
	}
	terminal, err := NativeSessionProcessTerminal(cfg.StateDir, slot, sess)
	if err != nil {
		if hold, ok := NativeHold(err); ok {
			return &NativeRegistrationHold{Code: hold.Code, LaunchUncertain: hold.LaunchUncertain, Slot: slot, cause: err}
		}
		// The projected generation's receipt is absent, or the receipt or its
		// terminal marker cannot be read or decoded. No successor exists yet,
		// so this is not a failure of the successor launch but an unproven
		// projected generation: hold the slot, so the daemon retains the
		// session and re-inspects the receipt directory every cycle, instead
		// of returning an error the callers record as a failed respawn.
		code := "projected_receipt_undecodable"
		if errors.Is(err, os.ErrNotExist) {
			code = "projected_receipt_missing"
		}
		log.Printf("[worker] native generation %d of %s has no readable projected receipt before its successor: %v", sess.WorkerGeneration, slot, err)
		return &NativeRegistrationHold{Code: code, LaunchUncertain: true, Slot: slot, cause: err}
	}
	if terminal {
		return nil
	}
	missing := func(cause error) error {
		log.Printf("[worker] native generation %d of %s is not recorded terminal before its successor: %v", sess.WorkerGeneration, slot, cause)
		return &NativeRegistrationHold{Code: "native_process_identity_missing", LaunchUncertain: true, Slot: slot, cause: cause}
	}
	dir, unlock, err := lockNativeWorker(cfg, slot)
	if err != nil {
		return missing(err)
	}
	defer unlock()
	r, err := readNativeWorkerReceipt(dir, sess.WorkerGeneration)
	if err != nil {
		return missing(err)
	}
	if _, err := sealNativeGenerationTerminationLocked(cfg, dir, slot, sess, r); err != nil {
		if hold, ok := NativeHold(err); ok && (hold.Code == "native_identity_conflict" || hold.Code == "previous_outcome_unknown") {
			return &NativeRegistrationHold{Code: hold.Code, LaunchUncertain: hold.LaunchUncertain, Slot: slot, cause: err}
		}
		return missing(err)
	}
	return nil
}

// sealNativeGenerationTerminationLocked proves, seals and records the exact OS
// termination of the session's projected launched generation r, in that order
// (verified terminal process state precedes the seal, docs/strict-ai-execution.md):
//
//  1. observe the original pinned runtime without persisting or signalling
//     anything: exact lease inactive, pane and host runner absent, verified
//     termination proof. Only the strict verified route can observe that. A
//     generation that already carries the terminal marker was proven earlier
//     and is not re-observed.
//  2. seal the exact binding at the authority (idempotent; a persisted seal
//     intent from an earlier attempt is reused) and require a settlement that
//     allows a next generation.
//  3. persist the termination evidence on the receipt and write the terminal
//     marker, exactly as the sealed-termination reconciliation does.
//
// The caller holds the receipt lock. The session projection is not modified;
// only the receipt directory is. Every failure leaves the receipt in a state
// the same call reconciles on the next attempt.
func sealNativeGenerationTerminationLocked(cfg *config.Config, dir, slot string, sess *state.Session, r *NativeWorkerReceipt) (*NativeWorkerReceipt, error) {
	if sess == nil || sess.NativeReceiptDir != dir || sess.ProcessLeaseUnit != "" || sess.ProcessLeaseManager != "" ||
		r == nil || r.ProjectID != cfg.ProjectID || r.Slot != slot || r.Generation != sess.WorkerGeneration || r.Status != "launched" || r.Acknowledgement == nil ||
		r.RoleRunID != sess.NativeRoleRunID || r.Request.NativeSessionID != sess.NativeSessionID ||
		r.IssueNumber != sess.IssueNumber || r.Worktree != sess.Worktree || r.Branch != sess.Branch {
		return nil, &NativeRegistrationHold{Code: "native_identity_conflict", LaunchUncertain: true, Slot: slot}
	}
	terminal, err := nativeWorkerTerminated(dir, r)
	if err != nil {
		return nil, &NativeRegistrationHold{Code: "native_process_identity_missing", LaunchUncertain: true, Slot: slot, cause: err}
	}
	var proof *aiexecution.NativeProcessTermination
	if !terminal {
		if !cfg.AIExecution.RequireVerifiedRoute {
			return nil, &NativeRegistrationHold{Code: "native_process_identity_missing", LaunchUncertain: true, Slot: slot,
				cause: &NativeRegistrationHold{Code: "native_process_unknown", LaunchUncertain: true}}
		}
		proof, err = observeNativeWorkerTerminationLocked(cfg, r)
		if err != nil {
			return nil, &NativeRegistrationHold{Code: "native_process_identity_missing", LaunchUncertain: true, Slot: slot, cause: err}
		}
	}
	sealed, err := reconcileNativeWorkerOutcomeLocked(cfg, dir, r)
	if err != nil {
		return nil, err
	}
	if sealed.RoleRunID != sess.NativeRoleRunID || sealed.Request.NativeSessionID != sess.NativeSessionID {
		return nil, &NativeRegistrationHold{Code: "native_identity_conflict", LaunchUncertain: true, Slot: slot}
	}
	if previousNativeGenerationOutcome(cfg, sealed) != nil || sealed.Outcome.OperatorRetirement != nil {
		return nil, &NativeRegistrationHold{Code: "previous_outcome_unknown"}
	}
	if terminal {
		return sealed, nil
	}
	sealed.NativeProcessEvidence = proof
	if err := persistNativeWorkerReceipt(dir, sealed); err != nil {
		return nil, &NativeRegistrationHold{Code: "receipt_persistence_failed", LaunchUncertain: true, Slot: slot}
	}
	// Use the exact verified receipt lease for the marker without restoring a
	// live lease to the session or weakening markNativeWorkerTerminated's fence.
	marked := *sess
	setSessionProcessLease(&marked, tmuxsession.ProcessLease{Unit: sealed.ProcessLeaseUnit, Manager: sealed.ProcessLeaseManager})
	if err := markNativeWorkerTerminated(&marked); err != nil {
		return nil, err
	}
	if terminal, err := nativeWorkerTerminated(dir, sealed); err != nil || !terminal {
		return nil, &NativeRegistrationHold{Code: "native_process_identity_missing", LaunchUncertain: true, Slot: slot, cause: err}
	}
	return sealed, nil
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

// NativeLaunchAbandonmentIntent is the durable sidecar persisted before the
// authority seal of an abandoned launch. It pins the exact receipt and seal
// request so a crash after the request leaves auditable evidence and the retry
// re-seals the same binding.
type NativeLaunchAbandonmentIntent struct {
	SchemaVersion   int                          `json:"schema_version"`
	IntendedAt      time.Time                    `json:"intended_at"`
	Slot            string                       `json:"slot"`
	Generation      uint64                       `json:"generation"`
	RoleRunID       string                       `json:"role_run_id"`
	ParentRoleRunID string                       `json:"parent_role_run_id"`
	NativeSessionID string                       `json:"native_session_id"`
	ReceiptSHA256   string                       `json:"receipt_sha256"`
	OutcomeIntent   admissioncontrol.SealRequest `json:"outcome_intent"`
}

func (i *NativeLaunchAbandonmentIntent) matches(slot string, generation uint64, r *NativeWorkerReceipt, receiptSHA string) bool {
	return i.SchemaVersion == 1 && i.Slot == slot && i.Generation == generation && i.RoleRunID == r.RoleRunID && i.ParentRoleRunID == r.ParentRoleRunID &&
		i.NativeSessionID == r.Request.NativeSessionID && i.ReceiptSHA256 == receiptSHA &&
		i.OutcomeIntent == admissioncontrol.SealRequest{Binding: r.Request.Binding, RegistrationVersion: r.Acknowledgement.RegistrationVersion}
}

// nativeLaunchAbandonmentIntent selects the archive prefix for receipt r and
// makes its seal intent durable. An intent already persisted for this exact
// receipt (a previous attempt crashed or was held after writing it) is reused,
// so the same prefix and seal request carry over; an intent for a different
// receipt is never overwritten.
func nativeLaunchAbandonmentIntent(dir, slot string, generation uint64, r *NativeWorkerReceipt, receiptSHA string) (string, *NativeLaunchAbandonmentIntent, error) {
	for index := 1; index <= 64; index++ {
		prefix := nativeLaunchAbandonmentPrefix(generation, index)
		if _, err := os.Lstat(filepath.Join(dir, prefix+".receipt.json")); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		path := filepath.Join(dir, prefix+".intent.json")
		if _, err := os.Lstat(path); err == nil {
			b, err := readOwnedRegularNoFollow(path, 64<<10)
			if err != nil {
				return "", nil, &NativeRegistrationHold{Code: "receipt_invalid", LaunchUncertain: true}
			}
			var existing NativeLaunchAbandonmentIntent
			if aiexecution.DecodeStrict(b, &existing) != nil {
				return "", nil, &NativeRegistrationHold{Code: "receipt_invalid", LaunchUncertain: true}
			}
			if existing.matches(slot, generation, r, receiptSHA) {
				return prefix, &existing, nil
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", nil, &NativeRegistrationHold{Code: "receipt_invalid", LaunchUncertain: true}
		}
		intent := &NativeLaunchAbandonmentIntent{SchemaVersion: 1, IntendedAt: time.Now().UTC(), Slot: slot, Generation: generation,
			RoleRunID: r.RoleRunID, ParentRoleRunID: r.ParentRoleRunID, NativeSessionID: r.Request.NativeSessionID, ReceiptSHA256: receiptSHA,
			OutcomeIntent: admissioncontrol.SealRequest{Binding: r.Request.Binding, RegistrationVersion: r.Acknowledgement.RegistrationVersion}}
		b, err := json.Marshal(intent)
		if err != nil {
			return "", nil, &NativeRegistrationHold{Code: "receipt_persistence_failed", LaunchUncertain: true}
		}
		if writeFileAtomicMode(dir, path, string(b), 0600) != nil || syncNativeDir(dir) != nil {
			return "", nil, &NativeRegistrationHold{Code: "receipt_persistence_failed", LaunchUncertain: true}
		}
		return prefix, intent, nil
	}
	return "", nil, &NativeRegistrationHold{Code: "receipt_persistence_failed", LaunchUncertain: true}
}

// nativeLaunchAbandonmentRecorded reports whether the successor of the session's
// projected role run was already archived as an abandoned launch. The successor
// receipt is gone, so its identity comes from the slot's execution proof, which
// the abandonment left in place: the archive must carry that proof's digest and
// native session, and its archived receipt must be the record's exact bytes with
// the expected role run, native session, parent, slot and project. An archive
// that merely shares the parent never replays.
func nativeLaunchAbandonmentRecorded(cfg *config.Config, dir, slot string, generation uint64, sess *state.Session) (bool, error) {
	proofBytes, err := readOwnedRegularNoFollow(filepath.Join(cfg.StateDir, slot+"-run.sh.execution.json"), 128<<10)
	if err != nil {
		return false, nil
	}
	var proof workerExecutionProof
	if aiexecution.DecodeStrict(proofBytes, &proof) != nil || proof.Spec.Registration == nil || proof.RuntimeKey != slot || proof.Spec.ProjectID != cfg.ProjectID {
		return false, nil
	}
	nativeID := proof.Spec.Registration.Binding.NativeSessionID
	proofSum := sha256.Sum256(proofBytes)
	proofSHA := hex.EncodeToString(proofSum[:])
	invalid := &NativeRegistrationHold{Code: "receipt_invalid", LaunchUncertain: true}
	for index := 1; index <= 64; index++ {
		prefix := nativeLaunchAbandonmentPrefix(generation, index)
		if _, err := os.Lstat(filepath.Join(dir, prefix+".receipt.json")); errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		b, err := readOwnedRegularNoFollow(filepath.Join(dir, prefix+".outcome.json"), 64<<10)
		if err != nil {
			return false, invalid
		}
		var record NativeLaunchAbandonmentRecord
		if aiexecution.DecodeStrict(b, &record) != nil || record.SchemaVersion != 1 || record.Generation != generation ||
			admissioncontrol.ValidateNativeOutcome(record.Outcome, record.OutcomeIntent) != nil || record.Outcome.PhysicalAttempts != 0 {
			return false, invalid
		}
		if record.NativeSessionID != nativeID || record.ExecutionProofSHA256 != proofSHA || record.ParentRoleRunID != sess.NativeRoleRunID || record.Slot != slot {
			continue
		}
		archived, err := readOwnedRegularNoFollow(filepath.Join(dir, prefix+".receipt.json"), 64<<10)
		if err != nil {
			return false, invalid
		}
		archivedSum := sha256.Sum256(archived)
		var receipt NativeWorkerReceipt
		if hex.EncodeToString(archivedSum[:]) != record.ReceiptSHA256 || aiexecution.DecodeStrict(archived, &receipt) != nil ||
			receipt.RoleRunID != record.RoleRunID || receipt.Request.NativeSessionID != nativeID || receipt.ParentRoleRunID != sess.NativeRoleRunID ||
			receipt.Slot != slot || receipt.ProjectID != cfg.ProjectID || receipt.Generation != generation ||
			record.OutcomeIntent.Binding != receipt.Request.Binding {
			return false, invalid
		}
		return true, nil
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
