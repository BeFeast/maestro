package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func TestForgeLinksSurviveFleetProjection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		forge      config.ForgeConfig
		base, pull string
	}{
		{"github", config.ForgeConfig{}, "https://github.com/BeFeast/hedroom", "pull"},
		{"forgejo", config.ForgeConfig{Kind: config.ForgeKindForgejo, BaseURL: "https://git.oklabs.uk/"}, "https://git.oklabs.uk/BeFeast/hedroom", "pulls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			cfg := &config.Config{Repo: "BeFeast/hedroom", Forge: tc.forge, StateDir: t.TempDir(), MaxParallel: 1}
			st := state.NewState()
			st.Sessions["ret-hedroom-1"] = &state.Session{IssueNumber: 382, PRNumber: 400, Status: state.StatusDone, StartedAt: now.Add(-time.Minute), FinishedAt: &now}
			if err := state.Save(cfg.StateDir, st); err != nil {
				t.Fatal(err)
			}
			response := NewFleet([]FleetProject{NewFleetProject("hedroom", "", "", cfg)}, "127.0.0.1", 8786, true).snapshot()
			if len(response.Workers) != 1 || len(response.Projects) != 1 {
				t.Fatalf("unexpected projection: %+v", response)
			}
			wantIssue, wantPR := tc.base+"/issues/382", tc.base+"/"+tc.pull+"/400"
			worker := response.Workers[0]
			if worker.IssueURL != wantIssue || worker.PRURL != wantPR {
				t.Fatalf("worker links = %q %q", worker.IssueURL, worker.PRURL)
			}
			project := response.Projects[0]
			if project.ForgeBaseURL != tc.forge.BaseURL {
				t.Fatalf("forge base lost: %+v", project)
			}
			candidates := fleetCloseCandidates(project, st)
			if len(candidates) != 1 || candidates[0].IssueURL != wantIssue || candidates[0].PRURL != wantPR {
				t.Fatalf("close candidates: %+v", candidates)
			}
			target := &state.SupervisorTarget{Issue: 382, PR: 400}
			operator := applyFleetOperatorTarget(project, fleetOperatorState{}, target)
			if operator.IssueURL != wantIssue || operator.PRURL != wantPR {
				t.Fatalf("operator links: %+v", operator)
			}
			decision := makeSupervisorDecisionInfo(cfg, st, state.SupervisorDecision{Target: target})
			approval := makeFleetApprovalState(project, st, state.Approval{Target: target}, now)
			for _, links := range [][]targetLinkInfo{decision.TargetLinks, approval.TargetLinks} {
				if len(links) < 2 || links[0].URL != wantIssue || links[1].URL != wantPR {
					t.Fatalf("target links: %+v", links)
				}
			}
			if got := fleetProjectRepoURL(project); got != tc.base {
				t.Fatalf("repo = %q", got)
			}
			// JSON is the UI boundary; no Forgejo link may silently return to GitHub.
			wire, err := json.Marshal([]any{worker, candidates, operator, decision, approval})
			if err != nil {
				t.Fatal(err)
			}
			if tc.forge.IsForgejo() && strings.Contains(string(wire), "https://github.com/BeFeast/hedroom") {
				t.Fatalf("GitHub link leaked: %s", wire)
			}
		})
	}
}
