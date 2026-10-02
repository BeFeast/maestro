package orchestrator

import (
	"log"
	"time"

	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
)

// Operator pause (#683) means "no new native worker launch of any kind" for
// the project: fresh issue dispatch, supervisor-selected repair and
// review-repair, scheduled/backoff retries, provider or backend failover
// respawns, restart resumes and soft-token checkpoint respawns. Workers that
// are already running keep running and finish normally. A deferred launch is
// never consumed: retries stay due, restart markers stay set, and repair
// approvals stay approved/awaiting_dispatch, so the first cycle after
// `maestro resume` launches each of them exactly once.
//
// EMERGENCY STOP (#840) is a separate, fleet-wide gate and is unchanged here.

// pauseDefersNativeLaunch reports whether the operator pause holds back the
// native launch the caller is about to start. It must be called only at the
// point where a launch would otherwise happen, before any session mutation,
// so a deferral leaves the launch intent exactly as it was. Each deferral is
// counted for the single per-cycle journal line; callers must not log their
// own per-launch pause line.
func (o *Orchestrator) pauseDefersNativeLaunch(s *state.State) bool {
	if !s.PauseActive() {
		return false
	}
	o.pauseDeferredLaunches++
	return true
}

// parkEndedWorkerForPause records a worker whose process has already ended as
// a due, budget-neutral scheduled retry instead of launching its failover
// replacement during an operator pause. The retry queue is the canonical
// container for a pending in-place respawn: respawnDueRetries keeps it due
// while paused and, after resume, re-resolves backend health (#805) so the
// respawn still lands on a healthy fallback. RetryCount and the unexpected
// exit budget are untouched because the failover path preserves them too.
func parkEndedWorkerForPause(sess *state.Session, now time.Time) {
	sess.Status = state.StatusDead
	sess.PID = 0
	sess.TmuxSession = ""
	sess.FinishedAt = &now
	state.MarkWorkerEnded(sess, now)
	retryAt := now
	sess.NextRetryAt = &retryAt
}

// pausedRepairDispatches counts the supervisor-selected repair and
// review-repair dispatches that startNewWorkers would have tried this cycle.
// It reuses supervisorSelectedRepairSpawn so the count matches dispatch
// eligibility exactly, and it reads state only (no forge calls while paused).
func (o *Orchestrator) pausedRepairDispatches(s *state.State) int {
	if s == nil {
		return 0
	}
	candidates := make(map[int]struct{})
	for i := range s.Approvals {
		approval := &s.Approvals[i]
		if approval.Target == nil || approval.Target.Issue <= 0 {
			continue
		}
		if approval.Action != supervisor.ActionSpawnRepairWorker && approval.Action != supervisor.ActionSpawnReviewRepair {
			continue
		}
		if approval.Status != state.ApprovalStatusApproved && approval.Status != state.ApprovalStatusAwaitingDispatch {
			continue
		}
		candidates[approval.Target.Issue] = struct{}{}
	}
	if decision := s.LatestSupervisorDecision(); decision != nil && decision.Target != nil && decision.Target.Issue > 0 {
		candidates[decision.Target.Issue] = struct{}{}
	}
	count := 0
	for issue := range candidates {
		if o.supervisorSelectedRepairSpawn(s, issue) {
			count++
		}
	}
	return count
}

// journalPauseDeferrals writes the one journal line a paused cycle produces
// and resets the per-cycle counter. Fresh issue selection is skipped before
// any listing while paused, so its candidates are not part of the count.
func (o *Orchestrator) journalPauseDeferrals(s *state.State) {
	deferred := o.pauseDeferredLaunches
	o.pauseDeferredLaunches = 0
	if !s.PauseActive() {
		return
	}
	log.Printf("[orch] project paused: deferring %d native launches (since %s; issue selection skipped; running=%d)",
		deferred, s.PausedAt.Format(time.RFC3339), s.RunningSessionCount())
}
