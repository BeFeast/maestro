package orchestrator

import (
	"fmt"
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
// native launch the caller is about to start for slotName/issueNumber. It must
// be called only at the point where a launch would otherwise happen, before
// any session mutation, so a deferral leaves the launch intent exactly as it
// was. Each deferral is recorded for the single per-cycle journal line;
// callers must not log their own per-launch pause line.
func (o *Orchestrator) pauseDefersNativeLaunch(s *state.State, slotName string, issueNumber int) bool {
	if !s.PauseActive() {
		return false
	}
	o.notePauseDeferredLaunch(slotName, issueNumber)
	return true
}

// notePauseDeferredLaunch records one held launch intent for this cycle. The
// key is the issue when known: a project runs at most one worker per issue,
// so a failover parked as a due retry and the retry queue deferring that same
// retry later in the cycle are one launch, not two.
func (o *Orchestrator) notePauseDeferredLaunch(slotName string, issueNumber int) {
	key := "slot:" + slotName
	if issueNumber > 0 {
		key = fmt.Sprintf("issue:%d", issueNumber)
	}
	if o.pauseDeferredLaunches == nil {
		o.pauseDeferredLaunches = make(map[string]struct{})
	}
	o.pauseDeferredLaunches[key] = struct{}{}
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

// deadSessionAwaitsRelaunch reports whether a dead session still carries an
// unconsumed relaunch intent: a scheduled retry (NextRetryAt) or a restart
// resume marker (RestartCheckpointAt). Both relaunch the same slot, branch and
// worktree in place, so the session still owns its worktree and the 1h
// terminal worktree GC must leave it alone. Without the worktree, the retry
// path falls back to worker.Respawn, which deletes the branch and starts over
// from the default branch, and a restart resume drops its marker. An operator
// pause holds these intents for as long as the pause lasts, which is routinely
// longer than the GC grace.
func deadSessionAwaitsRelaunch(sess *state.Session) bool {
	if sess == nil || sess.Status != state.StatusDead {
		return false
	}
	return sess.NextRetryAt != nil || sess.RestartCheckpointAt != nil
}

// notePausedLaunchIntents records the launch intents that a paused cycle
// holds without reaching their launch point: supervisor-selected repair and
// review-repair dispatches (startNewWorkers returns before them while paused,
// and RunOnce skips it entirely when no slot is free) and due retries that
// respawnDueRetries never reached for lack of a slot. It reuses
// supervisorSelectedRepairSpawn so repair counting matches dispatch
// eligibility exactly, and it reads state only (no forge calls while paused).
func (o *Orchestrator) notePausedLaunchIntents(s *state.State, now time.Time) {
	if s == nil {
		return
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
	for issue := range candidates {
		if o.supervisorSelectedRepairSpawn(s, issue) {
			o.notePauseDeferredLaunch("", issue)
		}
	}
	for slotName, sess := range s.Sessions {
		if sess == nil || sess.NativeRegistrationHold != "" || sess.Status != state.StatusDead || sess.NextRetryAt == nil {
			continue
		}
		if now.Before(*sess.NextRetryAt) {
			continue
		}
		o.notePauseDeferredLaunch(slotName, sess.IssueNumber)
	}
}

// journalPauseDeferrals writes the one journal line a paused cycle produces
// and resets the per-cycle record. Fresh issue selection is skipped before any
// listing while paused, so its candidates are not part of the count.
func (o *Orchestrator) journalPauseDeferrals(s *state.State) {
	if !s.PauseActive() {
		o.pauseDeferredLaunches = nil
		return
	}
	o.notePausedLaunchIntents(s, time.Now().UTC())
	deferred := len(o.pauseDeferredLaunches)
	o.pauseDeferredLaunches = nil
	log.Printf("[orch] project paused: deferring %d native launches (since %s; issue selection skipped; running=%d)",
		deferred, s.PausedAt.Format(time.RFC3339), s.RunningSessionCount())
}
