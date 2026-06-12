package orchestrator

import (
	"os"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/selfdeploy"
	"github.com/befeast/maestro/internal/state"
)

// #698 AC3: flag default OFF — no trigger without self_deploy.enabled.
func TestMaybeSelfDeployAfterMerge_DefaultOff(t *testing.T) {
	called := false
	o := &Orchestrator{
		cfg:               &config.Config{Repo: "owner/repo"},
		notifier:          &notify.Notifier{},
		selfDeployStartFn: func(prNumber int) error { called = true; return nil },
	}

	o.maybeSelfDeployAfterMerge(698)
	if called {
		t.Fatal("self-deploy triggered with the flag off")
	}
}

func TestMaybeSelfDeployAfterMerge_Enabled(t *testing.T) {
	gotPR := 0
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:       "owner/repo",
			SelfDeploy: config.SelfDeployConfig{Enabled: true},
		},
		notifier:          &notify.Notifier{},
		selfDeployStartFn: func(prNumber int) error { gotPR = prNumber; return nil },
	}

	o.maybeSelfDeployAfterMerge(698)
	if gotPR != 698 {
		t.Fatalf("trigger called with PR %d, want 698", gotPR)
	}
}

// #698 AC5: a finished deploy surfaces as a supervisor finding and the
// result file is consumed exactly once.
func TestConsumeSelfDeployResult_Deployed(t *testing.T) {
	stateDir := t.TempDir()
	payload := `{"status":"deployed","version":"1.4.2+gabc1234","expected_sha":"abc1234","pr":698}`
	if err := os.WriteFile(selfdeploy.ResultPath(stateDir), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", StateDir: stateDir},
		notifier: &notify.Notifier{},
	}
	s := &state.State{}

	if !o.consumeSelfDeployResult(s) {
		t.Fatal("consumeSelfDeployResult = false, want true")
	}
	if len(s.SupervisorDecisions) != 1 {
		t.Fatalf("decisions recorded = %d, want 1", len(s.SupervisorDecisions))
	}
	d := s.SupervisorDecisions[0]
	if !strings.Contains(d.Summary, "deployed maestro v1.4.2+gabc1234") {
		t.Errorf("Summary = %q", d.Summary)
	}
	if d.Status != "succeeded" {
		t.Errorf("Status = %q", d.Status)
	}
	if _, err := os.Stat(selfdeploy.ResultPath(stateDir)); !os.IsNotExist(err) {
		t.Error("result file not cleared after consume")
	}
	// Second cycle: nothing left to consume.
	if o.consumeSelfDeployResult(s) {
		t.Error("second consume = true, want false")
	}
	if len(s.SupervisorDecisions) != 1 {
		t.Errorf("decisions after second consume = %d, want 1", len(s.SupervisorDecisions))
	}
}

// #698 AC2: a rollback emits a finding carrying the rollback reason.
func TestConsumeSelfDeployResult_RolledBack(t *testing.T) {
	stateDir := t.TempDir()
	payload := `{"status":"rolled_back","prev_version":"1.4.1","pr":698,"reason":"health check timed out"}`
	if err := os.WriteFile(selfdeploy.ResultPath(stateDir), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", StateDir: stateDir},
		notifier: &notify.Notifier{},
	}
	s := &state.State{}

	if !o.consumeSelfDeployResult(s) {
		t.Fatal("consumeSelfDeployResult = false, want true")
	}
	d := s.SupervisorDecisions[0]
	if !strings.Contains(d.Summary, "rolled back") || !strings.Contains(d.Summary, "health check timed out") {
		t.Errorf("Summary = %q", d.Summary)
	}
	if d.Status != "failed" {
		t.Errorf("Status = %q", d.Status)
	}
}

func TestConsumeSelfDeployResult_NoFile(t *testing.T) {
	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", StateDir: t.TempDir()},
		notifier: &notify.Notifier{},
	}
	s := &state.State{}
	if o.consumeSelfDeployResult(s) {
		t.Fatal("consumeSelfDeployResult = true with no result file")
	}
	if len(s.SupervisorDecisions) != 0 {
		t.Fatalf("decisions = %d, want 0", len(s.SupervisorDecisions))
	}
}

// A malformed result file is cleared (not re-logged forever) and records nothing.
func TestConsumeSelfDeployResult_Malformed(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(selfdeploy.ResultPath(stateDir), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}

	o := &Orchestrator{
		cfg:      &config.Config{Repo: "owner/repo", StateDir: stateDir},
		notifier: &notify.Notifier{},
	}
	if o.consumeSelfDeployResult(&state.State{}) {
		t.Fatal("consumeSelfDeployResult = true for malformed file")
	}
	if _, err := os.Stat(selfdeploy.ResultPath(stateDir)); !os.IsNotExist(err) {
		t.Error("malformed result file not cleared")
	}
}
