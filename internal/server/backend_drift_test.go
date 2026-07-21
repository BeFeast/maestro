package server

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func backendDriftTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Repo:     "owner/demo",
		StateDir: t.TempDir(),
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {
					Cmd:      "claude --model opus --effort high",
					Provider: "anthropic",
					Model:    "opus",
					Variant:  "opus[1m]",
					Effort:   "high",
				},
			},
		},
	}
}

func staleBackendSession(issue, pr int) *state.Session {
	started := time.Now().UTC().Add(-10 * time.Minute)
	return &state.Session{
		IssueNumber: issue,
		IssueTitle:  "backend drift",
		StartedAt:   started,
		Status:      state.StatusRunning,
		Backend:     "claude",
		PRNumber:    pr,
		Attribution: []state.BackendAttribution{{
			Backend:   "claude",
			Provider:  "anthropic",
			Model:     "opus",
			Variant:   "opus[1m]",
			Effort:    "xhigh",
			StartedAt: started,
			Reason:    "initial_spawn",
		}},
	}
}

func TestBackendDriftRestartTargetsSkipsOpenPRWorkers(t *testing.T) {
	cfg := backendDriftTestConfig(t)
	st := &state.State{Sessions: map[string]*state.Session{
		"sup-1": staleBackendSession(1, 0),
		"sup-2": staleBackendSession(2, 22),
	}}

	targets, skipped := backendDriftRestartTargets(cfg, st)
	if len(targets) != 1 || targets[0].Slot != "sup-1" {
		t.Fatalf("targets = %+v, want only PR-less sup-1", targets)
	}
	if len(skipped) != 1 || skipped[0].Slot != "sup-2" || skipped[0].PRNumber != 22 {
		t.Fatalf("skipped = %+v, want open-PR sup-2", skipped)
	}
}

func TestBackendDriftRestartActionEnqueuesOnlyPRLessWorkers(t *testing.T) {
	cfg := backendDriftTestConfig(t)
	st := &state.State{Sessions: map[string]*state.Session{
		"sup-1": staleBackendSession(1, 0),
		"sup-2": staleBackendSession(2, 22),
	}}
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	fs := &FleetServer{}
	res := fs.dispatchBackendDriftRestartAction(controlActionRequest{
		ActionID: config.SupervisorActionRestartStaleBackendWorkers,
		Project:  "demo",
		Reason:   "backend effort changed",
	}, cfg, nil, "tester")
	if res.status != 202 || res.err != nil {
		t.Fatalf("result status=%d err=%v body=%+v", res.status, res.err, res.body)
	}
	fresh, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if len(fresh.Approvals) != 1 {
		t.Fatalf("approvals = %+v, want one restart approval", fresh.Approvals)
	}
	approval := fresh.Approvals[0]
	if approval.Action != config.SupervisorActionRestartWorker || approval.Target == nil || approval.Target.Session != "sup-1" {
		t.Fatalf("approval = %+v, want restart_worker targeting sup-1", approval)
	}
	if approval.Target.Issue != 1 {
		t.Fatalf("approval target issue = %d, want 1", approval.Target.Issue)
	}

	res = fs.dispatchBackendDriftRestartAction(controlActionRequest{
		ActionID: config.SupervisorActionRestartStaleBackendWorkers,
		Project:  "demo",
		Reason:   "backend effort changed",
	}, cfg, nil, "tester")
	if res.status != 202 || res.err != nil {
		t.Fatalf("second result status=%d err=%v", res.status, res.err)
	}
	fresh, err = state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if len(fresh.Approvals) != 1 {
		t.Fatalf("dedup failed; approvals = %+v", fresh.Approvals)
	}
}

func TestApplyBackendDriftMarksMatchingWorkerFresh(t *testing.T) {
	cfg := backendDriftTestConfig(t)
	sess := staleBackendSession(1, 0)
	sess.Attribution[0].Effort = "high"
	info := makeSessionInfo(cfg.Repo, "sup-1", sess)

	applyBackendDrift(cfg, &info)
	if info.BackendDrift != nil {
		t.Fatalf("BackendDrift = %+v, want nil for matching attribution", info.BackendDrift)
	}
}

func TestBackendDriftRestartActionNoopsWhenNoDrift(t *testing.T) {
	cfg := backendDriftTestConfig(t)
	sess := staleBackendSession(1, 0)
	sess.Attribution[0].Effort = "high"
	if err := state.Save(cfg.StateDir, &state.State{Sessions: map[string]*state.Session{"sup-1": sess}}); err != nil {
		t.Fatalf("save state: %v", err)
	}
	res := (&FleetServer{}).dispatchBackendDriftRestartAction(controlActionRequest{ActionID: config.SupervisorActionRestartStaleBackendWorkers}, cfg, nil, "tester")
	if res.status != 200 || res.err != nil {
		t.Fatalf("result status=%d err=%v body=%+v", res.status, res.err, res.body)
	}
	fresh, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if len(fresh.Approvals) != 0 {
		t.Fatalf("approvals = %+v, want none", fresh.Approvals)
	}
}

func TestBackendDriftRestartActionRejectsNilConfig(t *testing.T) {
	res := (&FleetServer{}).dispatchBackendDriftRestartAction(controlActionRequest{}, nil, nil, "tester")
	if res.status != 500 || res.err == nil {
		t.Fatalf("result status=%d err=%v", res.status, res.err)
	}
}
