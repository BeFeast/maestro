package orchestrator

import (
	"errors"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

func TestNativePrelaunchRecoveryUsesCapacityAndOneShotDecision(t *testing.T) {
	for _, kind := range []string{"start", "prelaunch_failure", "uncertain_failure", "emergency", "pause", "drain", "project_capacity", "fleet_capacity", "reservation_denied", "resources", "closed_issue", "pr_exists", "wrong_project"} {
		t.Run(kind, func(t *testing.T) {
			cfg := cfgWithBackends("claude", "claude")
			cfg.ProjectID, cfg.MaxLiveWorkers, cfg.StateDir = "project", 1, t.TempDir()
			cfg.WorkerNativeSessionRegistration = &config.NativeSessionRegistrationConfig{}
			issue := makeIssue(1, "held work", "maestro-ready")
			issue.State = "open"
			o := New(cfg)
			o.getIssueFn = func(int) (github.Issue, error) { return issue, nil }
			o.listOpenPRsFn = func() ([]github.PR, error) { return nil, nil }
			s := state.NewState()
			s.Sessions["slot-1"] = &state.Session{Status: state.StatusFailed, NativeRegistrationHold: "setup_failed", IssueNumber: 1, Backend: "claude", Branch: "canonical"}
			calls, commits, releases := 0, 0, 0
			o.SetFleetSpawnReserve(func() (func(string), func(), bool) {
				return func(slot string) { commits++ }, func() { releases++ }, kind != "reservation_denied"
			})
			o.nativePrelaunchRecoverFn = func(gotCfg *config.Config, gotState *state.State, repo string, gotIssue github.Issue, prompt, slot, nativeID string) (string, error) {
				calls++
				if gotCfg.WorkerLaunchContext.Role != "implementer" || gotState != s || slot != "slot-1" || nativeID != "exact-native-id" {
					t.Fatal("recovery context changed")
				}
				if kind == "prelaunch_failure" {
					return slot, errors.New("hold")
				}
				if kind == "uncertain_failure" {
					return slot, &worker.NativeRegistrationHold{Code: "unknown", LaunchUncertain: true, Slot: slot}
				}
				s.Sessions[slot].Status = state.StatusRunning
				return slot, nil
			}
			request := NativePrelaunchRecovery{ProjectID: cfg.ProjectID, Slot: "slot-1", NativeSessionID: "exact-native-id"}
			switch kind {
			case "emergency":
				o.SetEmergencyHalt(func() bool { return true })
			case "pause":
				s.SetPaused(time.Now())
			case "drain":
				s.SetSpawnDrain(time.Now())
			case "project_capacity":
				s.Sessions["other"] = &state.Session{Status: state.StatusRunning}
			case "fleet_capacity":
				o.SetFleetSpawnCeiling(func() bool { return true })
			case "resources":
				o.SetSpawnResourceHold(func() (bool, string) { return true, "full" })
			case "closed_issue":
				issue.State = "closed"
			case "pr_exists":
				o.listOpenPRsFn = func() ([]github.PR, error) { return []github.PR{{HeadRefName: "canonical"}}, nil }
			case "wrong_project":
				request.ProjectID = "foreign"
			}
			o.SetNativePrelaunchRecoveries([]NativePrelaunchRecovery{request})
			o.recoverNativePrelaunchWorkers(s)
			o.recoverNativePrelaunchWorkers(s)
			wantsCall := kind == "start" || kind == "prelaunch_failure" || kind == "uncertain_failure"
			if wantsCall && calls != 1 || !wantsCall && calls != 0 {
				t.Fatalf("recovery calls=%d", calls)
			}
			if (kind == "start" || kind == "uncertain_failure") && (commits != 1 || releases != 0) {
				t.Fatalf("lost occupied reservation: %d/%d", commits, releases)
			}
			if kind == "prelaunch_failure" && (commits != 0 || releases != 1) {
				t.Fatalf("leaked unused reservation: %d/%d", commits, releases)
			}
		})
	}
}

func TestParseNativePrelaunchRecoveriesRejectsAmbiguity(t *testing.T) {
	valid := "00000000-0000-4000-8000-000000000001:slot-1:00000000-0000-4000-8000-000000000002"
	if got, err := ParseNativePrelaunchRecoveries([]string{valid}); err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	for _, input := range [][]string{{"project:slot:uuid"}, {valid, valid}, {"00000000-0000-4000-8000-000000000001:../slot:00000000-0000-4000-8000-000000000002"}} {
		if _, err := ParseNativePrelaunchRecoveries(input); err == nil {
			t.Fatal("invalid recovery accepted")
		}
	}
}
