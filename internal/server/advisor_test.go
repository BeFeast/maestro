package server

import (
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func TestSessionInfoSurfacesAdvisorGateState(t *testing.T) {
	now := time.Now().UTC()
	sess := &state.Session{
		IssueNumber:               928,
		IssueTitle:                "Advisor gate",
		Status:                    state.StatusFailed,
		Phase:                     state.PhaseAdvisor,
		StartedAt:                 now.Add(-time.Minute),
		FinishedAt:                &now,
		PlanVersion:               2,
		AdvisorReviewRound:        2,
		AdvisorMaxReviewRounds:    2,
		AdvisorBackend:            "advisor",
		AdvisorModel:              "review-model",
		AdvisorVerdict:            "PLAN_REVISE",
		AdvisorUnresolvedFindings: "exact unresolved finding",
		AdvisorTerminalReason:     "review_rounds_exhausted",
		AdvisorReviews: []state.AdvisorReview{{
			PlanVersion: 2, ReviewRound: 2, Backend: "advisor", Model: "review-model", Verdict: "PLAN_REVISE", Findings: "exact unresolved finding", ReviewedAt: now,
		}},
	}
	info := makeSessionInfo("owner/repo", "slot-advisor", sess)
	if info.Phase != "advisor" || info.PlanVersion != 2 || info.AdvisorReviewRound != 2 || info.AdvisorBackend != "advisor" || info.AdvisorModel != "review-model" || len(info.AdvisorReviews) != 1 {
		t.Fatalf("session info = %+v", info)
	}
	if !info.NeedsAttention || !strings.Contains(info.StatusReason, "exact unresolved finding") {
		t.Fatalf("attention = %v %q", info.NeedsAttention, info.StatusReason)
	}

	project := fleetProjectState{Name: "maestro", Repo: "owner/repo"}
	worker := makeFleetWorkerState(project, info)
	if worker.AdvisorTerminalReason != "review_rounds_exhausted" || worker.AdvisorUnresolvedFindings != "exact unresolved finding" || len(worker.AdvisorReviews) != 1 {
		t.Fatalf("fleet worker = %+v", worker)
	}
}

func TestFleetEffectiveConfigSurfacesAdvisorPolicy(t *testing.T) {
	cfg := &config.Config{Pipeline: config.PipelineConfig{
		Advisor:             config.RoleConfig{Enabled: true, Backend: "advisor", Effort: "high"},
		AdvisorReviewRounds: 4,
		AdvisorBestEffort:   true,
	}}
	effective := buildFleetEffectiveConfig(cfg)
	if !effective.Pipeline.Advisor.Enabled || effective.Pipeline.Advisor.Backend != "advisor" || effective.Pipeline.Advisor.Effort != "high" || effective.Pipeline.AdvisorReviewRounds != 4 || !effective.Pipeline.AdvisorBestEffort {
		t.Fatalf("effective Advisor config = %+v", effective.Pipeline)
	}
}
