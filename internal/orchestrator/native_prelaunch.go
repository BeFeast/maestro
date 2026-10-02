package orchestrator

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/pipeline"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
	"github.com/google/uuid"
)

type NativePrelaunchRecovery struct {
	ProjectID       string
	Slot            string
	NativeSessionID string
}

func ParseNativePrelaunchRecoveries(values []string) ([]NativePrelaunchRecovery, error) {
	var requests []NativePrelaunchRecovery
	seen := make(map[string]bool)
	for _, value := range values {
		parts := strings.Split(value, ":")
		if len(parts) != 3 || uuid.Validate(parts[0]) != nil || state.ValidateSlotID(parts[1]) != nil || uuid.Validate(parts[2]) != nil {
			return nil, fmt.Errorf("native prelaunch recovery requires project-ID:slot:native-session-UUID")
		}
		key := parts[0] + ":" + parts[1]
		if seen[key] {
			return nil, fmt.Errorf("duplicate native prelaunch recovery slot")
		}
		seen[key] = true
		requests = append(requests, NativePrelaunchRecovery{parts[0], parts[1], parts[2]})
	}
	return requests, nil
}

func (o *Orchestrator) SetNativePrelaunchRecoveries(requests []NativePrelaunchRecovery) {
	o.nativePrelaunchRecoveries = append([]NativePrelaunchRecovery(nil), requests...)
}

func (o *Orchestrator) SetFleetNativeRecoveryReserve(fn func(slot, nativeID string) (func(string), func(), bool)) {
	o.fleetNativeRecoveryReserveFn = fn
}

func (o *Orchestrator) recoverNativePrelaunchWorkers(s *state.State) {
	requests := o.nativePrelaunchRecoveries
	o.nativePrelaunchRecoveries = nil // Explicit one-shot; a failure never auto-replays.
	for _, request := range requests {
		if err := o.recoverNativePrelaunchWorker(s, request); err != nil {
			log.Printf("[orch] explicit native prelaunch recovery held for %s: %v", request.Slot, err)
		}
	}
}

// resumeLaneHeldPrelaunchWorkers resumes first-generation registrations that
// a managed-lane binding hold stopped before launch intent, once the lane is
// observed ready again. It uses the explicit recovery path unchanged
// (capacity, issue and PR checks, fleet permit, exact receipt and absence
// proofs); a failure leaves the session as it was or re-holds it with the new
// code. Bounded per registration; launch-intent holds stay operator decisions.
func (o *Orchestrator) resumeLaneHeldPrelaunchWorkers(s *state.State) {
	if o.cfg == nil || o.cfg.RuntimeNativeLaneReadiness == nil || o.cfg.WorkerNativeSessionRegistration == nil || s == nil {
		return
	}
	for _, slot := range sortedStateSessionNames(s) {
		sess := s.Sessions[slot]
		selectFn := o.nativeLaneHeldPrelaunchFn
		if selectFn == nil {
			selectFn = worker.NativeLaneHeldPrelaunch
		}
		id, ok := selectFn(o.cfg, slot, sess)
		if !ok {
			continue
		}
		code := sess.NativeRegistrationHold
		if err := o.recoverNativePrelaunchWorker(s, NativePrelaunchRecovery{ProjectID: o.cfg.ProjectID, Slot: slot, NativeSessionID: id}); err != nil {
			log.Printf("[orch] native lane ready; resuming %s held by %s is deferred: %v", slot, code, err)
			continue
		}
		log.Printf("[orch] native lane ready; resumed %s held by %s from its registered pre-launch receipt", slot, code)
	}
}

func (o *Orchestrator) recoverNativePrelaunchWorker(s *state.State, request NativePrelaunchRecovery) error {
	if request.ProjectID != o.cfg.ProjectID {
		return fmt.Errorf("project identity mismatch")
	}
	if s.PauseActive() || s.DrainActive() || o.emergencyHaltFn != nil && o.emergencyHaltFn() {
		return fmt.Errorf("operator pause/drain/emergency active")
	}
	if availableSlots(o.cfg, s, len(s.ActiveSessions())) <= 0 || o.fleetNativeRecoveryReserveFn == nil && o.fleetSpawnCeilingFn != nil && o.fleetSpawnCeilingFn() {
		return fmt.Errorf("worker capacity unavailable")
	}
	if o.spawnResourceHoldFn != nil {
		if hold, _ := o.spawnResourceHoldFn(); hold {
			return fmt.Errorf("host resource hold")
		}
	}
	if hold, code := o.nativeLaneHold(); hold {
		return fmt.Errorf("native lane not ready: %s", code)
	}
	sess := s.Sessions[request.Slot]
	if sess == nil || sess.Status != state.StatusFailed || sess.NativeRegistrationHold == "" {
		return fmt.Errorf("slot is not a failed prelaunch hold")
	}
	issue, err := o.getIssue(sess.IssueNumber)
	if err != nil {
		return fmt.Errorf("issue unavailable")
	}
	if !strings.EqualFold(issue.State, "open") || github.HasLabel(issue, o.cfg.ExcludeLabels) || len(o.cfg.IssueLabels) > 0 && !github.HasLabel(issue, o.cfg.IssueLabels) {
		return fmt.Errorf("issue is no longer runnable")
	}
	if prs, err := o.listOpenPRs(); err != nil {
		return fmt.Errorf("open PR state unavailable")
	} else {
		for _, pr := range prs {
			if pr.HeadRefName == sess.Branch {
				return fmt.Errorf("canonical branch already has a PR")
			}
		}
	}
	var permit *fleetSpawnPermit
	var ok bool
	if o.fleetNativeRecoveryReserveFn != nil {
		commit, release, granted := o.fleetNativeRecoveryReserveFn(request.Slot, request.NativeSessionID)
		permit, ok = &fleetSpawnPermit{commitFn: commit, releaseFn: release}, granted
	} else {
		permit, ok = o.reserveFleetSpawn()
	}
	if !ok {
		return fmt.Errorf("fleet capacity unavailable")
	}
	defer permit.Release()
	workerCfg, _, _ := pipelineConfigForIssue(o.cfg, issue)
	phase := pipeline.InitialPhase(workerCfg)
	workerCfg = pipeline.ApplyPhaseEffort(workerCfg, sess.Backend, phase)
	workerCfg = nativeWorkerConfig(workerCfg, worker.NativeRoleForPhase(phase), "")
	recoverFn := o.nativePrelaunchRecoverFn
	if recoverFn == nil {
		recoverFn = worker.RecoverRegisteredWorkerStart
	}
	slot, err := recoverFn(workerCfg, s, o.repo, issue, o.selectPrompt(issue), request.Slot, request.NativeSessionID)
	if err != nil {
		if hold, ok := worker.NativeHold(err); ok && hold.LaunchUncertain {
			permit.Commit(request.Slot)
		}
		return err
	}
	permit.Commit(slot)
	markSupervisorWorkerRecommendationMaterialized(s, issue.Number, time.Now().UTC())
	log.Printf("[orch] explicit native prelaunch recovery started %s for issue #%d", slot, issue.Number)
	return nil
}
