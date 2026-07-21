package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
)

func TestDroppedSupervisorRecommendationCannotDispatch(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	dropped := &state.RecommendationDisposition{
		Status: state.RecommendationDispositionDropped,
		Reason: state.RecommendationDispositionTTLExpired,
		At:     now,
	}

	t.Run("classic repair", func(t *testing.T) {
		s := state.NewState()
		s.RecordSupervisorDecision(state.SupervisorDecision{
			ID:                "sup-repair-expired",
			CreatedAt:         now,
			RecommendedAction: supervisor.ActionSpawnRepairWorker,
			Target:            &state.SupervisorTarget{Issue: 1022, PR: 1100, Session: "slot-1"},
			Disposition:       dropped,
		}, state.DefaultSupervisorDecisionLimit)
		o := &Orchestrator{cfg: &config.Config{}}
		if o.supervisorSelectedRepairSpawn(s, 1022) {
			t.Fatal("expired repair recommendation remained dispatchable")
		}
		if dispatch := o.resolveSpawnRepairDispatch(s, 1022); dispatch != nil {
			t.Fatalf("expired repair dispatch = %+v, want nil", dispatch)
		}
	})

	t.Run("review repair", func(t *testing.T) {
		s := state.NewState()
		s.RecordSupervisorDecision(state.SupervisorDecision{
			ID:                "sup-review-expired",
			CreatedAt:         now,
			RecommendedAction: supervisor.ActionSpawnReviewRepair,
			Target:            &state.SupervisorTarget{Issue: 1022, PR: 1100, HeadSHA: "abc"},
			ReviewRepair:      &state.SupervisorReviewRepairPayload{HeadSHA: "abc"},
			Disposition:       dropped,
		}, state.DefaultSupervisorDecisionLimit)
		o := &Orchestrator{cfg: &config.Config{}}
		if payload, target := o.supervisorSelectedReviewRepair(s, 1022); payload != nil || target != nil {
			t.Fatalf("expired review repair remained dispatchable: payload=%+v target=%+v", payload, target)
		}
	})
}

func TestWorkerStartMaterializesSupervisorRecommendation(t *testing.T) {
	s := state.NewState()
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	s.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-worker",
		CreatedAt:         now,
		RecommendedAction: supervisor.ActionSpawnWorker,
		Target:            &state.SupervisorTarget{Issue: 1022},
	}, state.DefaultSupervisorDecisionLimit)

	if !markSupervisorWorkerRecommendationMaterialized(s, 1022, now.Add(time.Minute)) {
		t.Fatal("worker start did not materialize the supervisor recommendation")
	}
	latest := s.LatestSupervisorDecision()
	if latest == nil || latest.Disposition == nil || latest.Disposition.Status != state.RecommendationDispositionMaterialized || latest.Disposition.Reason != state.RecommendationDispositionWorkerStarted {
		t.Fatalf("latest disposition = %+v, want materialized/worker_started", latest)
	}
}
