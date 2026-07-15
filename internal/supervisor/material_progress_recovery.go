package supervisor

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/progress"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

const materialProgressRecoveryLeaseDuration = 2 * time.Minute

type exactWorkerLeaseState string

const (
	exactWorkerLeaseAbsent    exactWorkerLeaseState = "absent"
	exactWorkerLeaseExact     exactWorkerLeaseState = "exact"
	exactWorkerLeaseReplaced  exactWorkerLeaseState = "replaced"
	exactWorkerLeaseUncertain exactWorkerLeaseState = "uncertain"
)

type materialProgressRecoveryRuntime struct {
	inspect func(progress.Target) exactWorkerLeaseState
	stop    func(progress.Target) exactWorkerLeaseState
}

var defaultMaterialProgressRecoveryRuntime = materialProgressRecoveryRuntime{
	inspect: inspectExactWorkerLease,
	stop:    stopExactWorkerLease,
}

// reconcileMaterialProgressRecoveries claims and executes safe worker-only
// recovery recommendations. Delivery/gate recommendations remain reporting
// surfaces: this actuator has no authority beyond a pre-delivery worker lease.
func reconcileMaterialProgressRecoveries(cfg *config.Config, now time.Time, runtime materialProgressRecoveryRuntime) error {
	if cfg == nil || strings.TrimSpace(cfg.StateDir) == "" || !cfg.StalledProgressWatchdog.IsActive() {
		return nil
	}
	st, err := state.Load(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("load recovery state: %w", err)
	}
	if st.MaterialProgress == nil {
		return nil
	}

	keys := make([]string, 0, len(st.MaterialProgress.Targets))
	for key, target := range st.MaterialProgress.Targets {
		if !materialProgressRecoveryActionable(target) {
			continue
		}
		keys = append(keys, key)
	}
	// State target keys are digests, so lexical ordering is stable and reveals no
	// private path/session material.
	sort.Strings(keys)
	for _, key := range keys {
		if err := reconcileMaterialProgressRecovery(cfg, key, now.UTC(), runtime); err != nil {
			return err
		}
	}
	return nil
}

func reconcileMaterialProgressRecovery(cfg *config.Config, targetKey string, now time.Time, runtime materialProgressRecoveryRuntime) error {
	leaseID, err := newMaterialProgressRecoveryLeaseID()
	if err != nil {
		return fmt.Errorf("create recovery lease: %w", err)
	}
	var (
		claimed bool
		target  progress.Target
		recID   string
	)
	err = state.Update(cfg.StateDir, func(st *state.State) error {
		if st.MaterialProgress == nil {
			return state.ErrNoStateChange
		}
		record := st.MaterialProgress.Targets[targetKey]
		if !materialProgressRecoveryActionable(record) {
			return state.ErrNoStateChange
		}
		target = record.Target
		recID = record.LastRecommendation.RecommendationID
		var claimErr error
		claimed, claimErr = st.ClaimMaterialRecovery(targetKey, recID, leaseID, materialProgressRecoveryLeaseDuration, now)
		if claimErr == nil && !claimed {
			return state.ErrNoStateChange
		}
		return claimErr
	})
	if err != nil {
		return fmt.Errorf("claim recovery %s: %w", targetKey, err)
	}
	if !claimed {
		return nil
	}

	leaseState := runtime.inspect(target)
	switch leaseState {
	case exactWorkerLeaseExact:
		leaseState = runtime.stop(target)
	case exactWorkerLeaseAbsent:
		// A prior owner may have stopped the worker before crashing. The durable
		// claim lets this owner finish the state transition exactly once.
	case exactWorkerLeaseReplaced:
		return completeMaterialRecoveryWithoutMutation(cfg, targetKey, recID, leaseID, target,
			progress.RecoverySucceeded, progress.RecoveryStageReconciled, progress.RecoveryReasonTargetReplaced, now)
	default:
		return completeMaterialRecoveryWithoutMutation(cfg, targetKey, recID, leaseID, target,
			progress.RecoveryFailed, progress.RecoveryStageOperatorNeeded, progress.RecoveryReasonIdentityUncertain, now)
	}

	switch leaseState {
	case exactWorkerLeaseAbsent:
		return scheduleMaterialProgressRetry(cfg, targetKey, recID, leaseID, target, now)
	case exactWorkerLeaseReplaced:
		return completeMaterialRecoveryWithoutMutation(cfg, targetKey, recID, leaseID, target,
			progress.RecoverySucceeded, progress.RecoveryStageReconciled, progress.RecoveryReasonTargetReplaced, now)
	case exactWorkerLeaseExact:
		return completeMaterialRecoveryWithoutMutation(cfg, targetKey, recID, leaseID, target,
			progress.RecoveryFailed, progress.RecoveryStageOperatorNeeded, progress.RecoveryReasonStopFailed, now)
	default:
		return completeMaterialRecoveryWithoutMutation(cfg, targetKey, recID, leaseID, target,
			progress.RecoveryFailed, progress.RecoveryStageOperatorNeeded, progress.RecoveryReasonIdentityUncertain, now)
	}
}

func materialProgressRecoveryActionable(target *state.MaterialProgressTarget) bool {
	if target == nil || target.LastRecommendation == nil ||
		target.LastRecommendation.Action != progress.ActionStopAndRetry {
		return false
	}
	return target.Active && target.LastDecision != nil &&
		target.LastDecision.Action == progress.ActionStopAndRetry &&
		target.LastDecision.RecommendationID == target.LastRecommendation.RecommendationID
}

func scheduleMaterialProgressRetry(cfg *config.Config, targetKey, recID, leaseID string, target progress.Target, now time.Time) error {
	return state.Update(cfg.StateDir, func(st *state.State) error {
		sess := st.Sessions[target.Slot]
		switch {
		case materialProgressSessionMatchesTarget(target.Slot, sess, target):
			sess.PID = 0
			sess.TmuxSession = ""
			sess.FinishedAt = timePointer(now)
			state.MarkWorkerEnded(sess, now)
			if materialProgressRetryAvailable(cfg, st, sess) {
				sess.RetryCount++
				sess.RetryReason = state.RetryReasonStalledProgress
				sess.Status = state.StatusDead
				sess.NextRetryAt = timePointer(now)
				return st.CompleteMaterialRecovery(targetKey, recID, leaseID, progress.RecoverySucceeded,
					progress.RecoveryStageRetryScheduled, progress.RecoveryReasonRetryScheduled, now)
			}
			sess.Status = state.StatusRetryExhausted
			sess.NextRetryAt = nil
			return st.CompleteMaterialRecovery(targetKey, recID, leaseID, progress.RecoveryFailed,
				progress.RecoveryStageRetryExhausted, progress.RecoveryReasonRetryExhausted, now)
		case sess == nil || materialProgressSessionCompleted(sess):
			return st.CompleteMaterialRecovery(targetKey, recID, leaseID, progress.RecoverySucceeded,
				progress.RecoveryStageReconciled, progress.RecoveryReasonTargetCompleted, now)
		case sess.Status == state.StatusDead && sess.NextRetryAt != nil:
			return st.CompleteMaterialRecovery(targetKey, recID, leaseID, progress.RecoverySucceeded,
				progress.RecoveryStageRetryScheduled, progress.RecoveryReasonRetryScheduled, now)
		case sess.Status == state.StatusRunning:
			return st.CompleteMaterialRecovery(targetKey, recID, leaseID, progress.RecoverySucceeded,
				progress.RecoveryStageReconciled, progress.RecoveryReasonTargetReplaced, now)
		default:
			return st.CompleteMaterialRecovery(targetKey, recID, leaseID, progress.RecoveryFailed,
				progress.RecoveryStageOperatorNeeded, progress.RecoveryReasonIdentityUncertain, now)
		}
	})
}

func completeMaterialRecoveryWithoutMutation(cfg *config.Config, targetKey, recID, leaseID string, target progress.Target, outcome progress.RecoveryOutcome, stage progress.RecoveryStage, reason progress.RecoveryReason, now time.Time) error {
	return state.Update(cfg.StateDir, func(st *state.State) error {
		// A replacement is success only when durable state also moved away from
		// the claimed identity. If state still names the old worker, an OS-level
		// mismatch is ambiguous and must remain operator-owned.
		if reason == progress.RecoveryReasonTargetReplaced && materialProgressSessionMatchesTarget(target.Slot, st.Sessions[target.Slot], target) {
			outcome = progress.RecoveryFailed
			stage = progress.RecoveryStageOperatorNeeded
			reason = progress.RecoveryReasonIdentityUncertain
		}
		return st.CompleteMaterialRecovery(targetKey, recID, leaseID, outcome, stage, reason, now)
	})
}

func materialProgressRetryAvailable(cfg *config.Config, st *state.State, sess *state.Session) bool {
	if cfg.MaxRetriesPerIssue <= 0 {
		return true
	}
	return st.FailedAttemptsForIssue(sess.IssueNumber)+sess.RetryCount < cfg.MaxRetriesPerIssue
}

func materialProgressSessionMatchesTarget(slot string, sess *state.Session, target progress.Target) bool {
	if sess == nil || sess.Status != state.StatusRunning || sess.IssueNumber != target.IssueNumber ||
		sess.PID != target.ProcessID || strings.TrimSpace(sess.TmuxSession) != strings.TrimSpace(target.TmuxSession) {
		return false
	}
	sessionID := materialProgressSessionID(slot, sess)
	return sessionID == target.SessionID && target.LeaseID == "spawn:"+sessionID
}

func materialProgressSessionCompleted(sess *state.Session) bool {
	if sess == nil {
		return true
	}
	switch sess.Status {
	case state.StatusQueued, state.StatusPROpen, state.StatusCodeLanded, state.StatusDone:
		return true
	default:
		return false
	}
}

func inspectExactWorkerLease(target progress.Target) exactWorkerLeaseState {
	if target.Kind != progress.TargetWorker || strings.TrimSpace(target.TmuxSession) == "" || target.ProcessID <= 0 {
		return exactWorkerLeaseUncertain
	}
	out, err := exec.Command("tmux", "list-panes", "-t", exactTmuxSessionTarget(target.TmuxSession), "-F", "#{pane_pid}").Output()
	if err != nil {
		if worker.IsAlive(target.ProcessID) {
			return exactWorkerLeaseUncertain
		}
		return exactWorkerLeaseAbsent
	}
	lines := strings.Fields(string(out))
	if len(lines) != 1 {
		return exactWorkerLeaseUncertain
	}
	pid, err := strconv.Atoi(lines[0])
	if err != nil || pid <= 0 {
		return exactWorkerLeaseUncertain
	}
	if pid != target.ProcessID {
		return exactWorkerLeaseReplaced
	}
	return exactWorkerLeaseExact
}

func stopExactWorkerLease(target progress.Target) exactWorkerLeaseState {
	observed := inspectExactWorkerLease(target)
	if observed != exactWorkerLeaseExact {
		return observed
	}
	worker.KillProcessTree(target.ProcessID)
	_ = exec.Command("tmux", "kill-session", "-t", exactTmuxSessionTarget(target.TmuxSession)).Run()
	return inspectExactWorkerLease(target)
}

func exactTmuxSessionTarget(session string) string {
	return "=" + strings.TrimSpace(session)
}

func newMaterialProgressRecoveryLeaseID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "recovery:" + hex.EncodeToString(raw[:]), nil
}

func timePointer(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}
