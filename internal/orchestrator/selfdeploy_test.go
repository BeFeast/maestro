package orchestrator

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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

	o.maybeSelfDeployAfterMerge(nil, 698)
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

	o.maybeSelfDeployAfterMerge(nil, 698)
	if gotPR != 698 {
		t.Fatalf("trigger called with PR %d, want 698", gotPR)
	}
}

// #742: a trigger failure (the detached unit never launched) is surfaced as a
// supervisor finding so a silently-undeployed merge shows as fleet attention
// instead of just a log line — and does not record a trigger marker, so the
// next merge retries.
func TestMaybeSelfDeployAfterMerge_TriggerFailureSurfacesFinding(t *testing.T) {
	stateDir := t.TempDir()
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:       "owner/repo",
			StateDir:   stateDir,
			SelfDeploy: config.SelfDeployConfig{Enabled: true, MinIntervalMinutes: 30},
		},
		notifier:          &notify.Notifier{},
		selfDeployStartFn: func(prNumber int) error { return errors.New("systemd-run: exit status 1") },
	}
	s := &state.State{}

	o.maybeSelfDeployAfterMerge(s, 742)

	if len(s.SupervisorDecisions) != 1 {
		t.Fatalf("decisions recorded = %d, want 1", len(s.SupervisorDecisions))
	}
	d := s.SupervisorDecisions[0]
	if d.Status != "failed" {
		t.Errorf("Status = %q, want failed", d.Status)
	}
	if !strings.Contains(d.Summary, "trigger failed") || !strings.Contains(d.Summary, "PR #742") {
		t.Errorf("Summary = %q", d.Summary)
	}
	// A pure trigger failure must not suppress the next attempt: no marker.
	if _, _, ok := selfdeploy.LastTrigger(stateDir); ok {
		t.Error("trigger marker recorded despite trigger failure — next merge would be debounced")
	}
}

// #722: a deploy triggered within the debounce window is skipped, so a burst of
// merges (or a run-loop restarted by its own deploy) cannot stack overlapping
// deploys — the self-trigger cascade.
func TestMaybeSelfDeployAfterMerge_DebouncesRecentTrigger(t *testing.T) {
	stateDir := t.TempDir()
	// A trigger fired moments ago for PR 698.
	if err := selfdeploy.RecordTrigger(stateDir, 698, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	calls := 0
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:       "owner/repo",
			StateDir:   stateDir,
			SelfDeploy: config.SelfDeployConfig{Enabled: true, MinIntervalMinutes: 30},
		},
		notifier:          &notify.Notifier{},
		selfDeployStartFn: func(prNumber int) error { calls++; return nil },
	}

	o.maybeSelfDeployAfterMerge(nil, 699)
	if calls != 0 {
		t.Fatalf("trigger fired %d times within the debounce window, want 0", calls)
	}
}

// Outside the debounce window the next merge deploys again and refreshes the
// trigger marker.
func TestMaybeSelfDeployAfterMerge_FiresAfterWindow(t *testing.T) {
	stateDir := t.TempDir()
	// Last trigger is well past the (1 minute) window.
	if err := selfdeploy.RecordTrigger(stateDir, 698, time.Now().UTC().Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}

	gotPR := 0
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:       "owner/repo",
			StateDir:   stateDir,
			SelfDeploy: config.SelfDeployConfig{Enabled: true, MinIntervalMinutes: 1},
		},
		notifier:          &notify.Notifier{},
		selfDeployStartFn: func(prNumber int) error { gotPR = prNumber; return nil },
	}

	o.maybeSelfDeployAfterMerge(nil, 720)
	if gotPR != 720 {
		t.Fatalf("trigger called with PR %d, want 720", gotPR)
	}
	// The marker advanced to the new trigger.
	if _, pr, ok := selfdeploy.LastTrigger(stateDir); !ok || pr != 720 {
		t.Fatalf("marker after fire = (pr %d, ok %v), want (720, true)", pr, ok)
	}
}

// #751: a PR merged outside the orchestrator's own merge path advances
// origin/main past the running binary; the next cycle observes the drift and
// fires exactly one debounced self-deploy. A second cycle within the window
// (e.g. reconcile still seeing the same already-merged PRs) does not re-fire.
func TestMaybeSelfDeployOnMainAdvance_FiresOnceThenDebounces(t *testing.T) {
	stateDir := t.TempDir()
	calls := 0
	gotPR := -1
	headLookups := 0
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:       "owner/repo",
			StateDir:   stateDir,
			SelfDeploy: config.SelfDeployConfig{Enabled: true, MinIntervalMinutes: 30},
		},
		notifier:      &notify.Notifier{},
		binaryVersion: "1.4.2+gabc1234", // built from abc1234...
		// origin/main moved to a different commit (externally-merged PR).
		mainHeadSHAFn:     func() (string, error) { headLookups++; return "fed9876543210fed9876543210fed9876543210f", nil },
		selfDeployStartFn: func(prNumber int) error { calls++; gotPR = prNumber; return nil },
	}

	o.maybeSelfDeployOnMainAdvance(nil)
	if calls != 1 {
		t.Fatalf("first cycle fired %d deploys, want 1", calls)
	}
	if gotPR != 0 {
		t.Errorf("observe-merge deploy PR = %d, want 0 (not a specific orchestrator merge)", gotPR)
	}
	if _, _, ok := selfdeploy.LastTrigger(stateDir); !ok {
		t.Error("trigger marker not recorded after firing")
	}

	// Second cycle within the debounce window: still drifted, but must not
	// re-fire — and must not even spend the head-SHA lookup.
	headLookups = 0
	o.maybeSelfDeployOnMainAdvance(nil)
	if calls != 1 {
		t.Fatalf("second cycle fired %d deploys total, want exactly 1 (debounced)", calls)
	}
	if headLookups != 0 {
		t.Errorf("debounced cycle still made %d head-SHA lookups, want 0", headLookups)
	}
}

// #751 AC3 (storm guard): reconcile that sees already-merged historical PRs but
// where origin/main matches the running binary must NOT deploy.
func TestMaybeSelfDeployOnMainAdvance_NoDriftNoTrigger(t *testing.T) {
	calls := 0
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:       "owner/repo",
			StateDir:   t.TempDir(),
			SelfDeploy: config.SelfDeployConfig{Enabled: true, MinIntervalMinutes: 30},
		},
		notifier:      &notify.Notifier{},
		binaryVersion: "1.4.2+gabc1234",
		// main head is the same commit the running binary was built from.
		mainHeadSHAFn:     func() (string, error) { return "abc1234567890abcdef1234567890abcdef123456", nil },
		selfDeployStartFn: func(prNumber int) error { calls++; return nil },
	}

	o.maybeSelfDeployOnMainAdvance(nil)
	if calls != 0 {
		t.Fatalf("deploy fired %d times with no drift, want 0", calls)
	}
}

// #751 AC2: a deploy the orchestrator just launched for its own merge (which
// also advanced main) must not be double-triggered by the drift-based path.
func TestMaybeSelfDeployOnMainAdvance_DebouncesOrchestratorOwnMerge(t *testing.T) {
	stateDir := t.TempDir()
	// The orchestrator merged PR 740 moments ago and recorded its trigger.
	if err := selfdeploy.RecordTrigger(stateDir, 740, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:       "owner/repo",
			StateDir:   stateDir,
			SelfDeploy: config.SelfDeployConfig{Enabled: true, MinIntervalMinutes: 30},
		},
		notifier:          &notify.Notifier{},
		binaryVersion:     "1.4.2+gabc1234",
		mainHeadSHAFn:     func() (string, error) { return "fed9876543210fed9876543210fed9876543210f", nil },
		selfDeployStartFn: func(prNumber int) error { calls++; return nil },
	}

	o.maybeSelfDeployOnMainAdvance(nil)
	if calls != 0 {
		t.Fatalf("drift-based deploy fired %d times within the orchestrator's own debounce window, want 0", calls)
	}
}

// Flag default OFF: no drift check, no GitHub round-trip.
func TestMaybeSelfDeployOnMainAdvance_DefaultOff(t *testing.T) {
	headLookups := 0
	calls := 0
	o := &Orchestrator{
		cfg:               &config.Config{Repo: "owner/repo", StateDir: t.TempDir()},
		notifier:          &notify.Notifier{},
		binaryVersion:     "1.4.2+gabc1234",
		mainHeadSHAFn:     func() (string, error) { headLookups++; return "fed9876543210fed9876543210fed9876543210f", nil },
		selfDeployStartFn: func(prNumber int) error { calls++; return nil },
	}

	o.maybeSelfDeployOnMainAdvance(nil)
	if calls != 0 || headLookups != 0 {
		t.Fatalf("flag off: calls=%d headLookups=%d, want 0/0", calls, headLookups)
	}
}

// An unstamped binary (bare "dev") cannot determine drift — never deploy, and
// never spend the head-SHA lookup.
func TestMaybeSelfDeployOnMainAdvance_UnstampedBinaryNoTrigger(t *testing.T) {
	headLookups := 0
	calls := 0
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:       "owner/repo",
			StateDir:   t.TempDir(),
			SelfDeploy: config.SelfDeployConfig{Enabled: true, MinIntervalMinutes: 30},
		},
		notifier:          &notify.Notifier{},
		binaryVersion:     "dev",
		mainHeadSHAFn:     func() (string, error) { headLookups++; return "fed9876543210fed9876543210fed9876543210f", nil },
		selfDeployStartFn: func(prNumber int) error { calls++; return nil },
	}

	o.maybeSelfDeployOnMainAdvance(nil)
	if calls != 0 || headLookups != 0 {
		t.Fatalf("unstamped binary: calls=%d headLookups=%d, want 0/0", calls, headLookups)
	}
}

// A trigger failure on the drift-based path surfaces a supervisor finding and
// records NO marker, so the next cycle retries.
func TestMaybeSelfDeployOnMainAdvance_TriggerFailureSurfacesFinding(t *testing.T) {
	stateDir := t.TempDir()
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:       "owner/repo",
			StateDir:   stateDir,
			SelfDeploy: config.SelfDeployConfig{Enabled: true, MinIntervalMinutes: 30},
		},
		notifier:          &notify.Notifier{},
		binaryVersion:     "1.4.2+gabc1234",
		mainHeadSHAFn:     func() (string, error) { return "fed9876543210fed9876543210fed9876543210f", nil },
		selfDeployStartFn: func(prNumber int) error { return errors.New("systemd-run: exit status 1") },
	}
	s := &state.State{}

	o.maybeSelfDeployOnMainAdvance(s)

	if len(s.SupervisorDecisions) != 1 {
		t.Fatalf("decisions recorded = %d, want 1", len(s.SupervisorDecisions))
	}
	if s.SupervisorDecisions[0].Status != "failed" {
		t.Errorf("Status = %q, want failed", s.SupervisorDecisions[0].Status)
	}
	// No marker on a pure trigger failure — the next cycle must retry.
	if _, _, ok := selfdeploy.LastTrigger(stateDir); ok {
		t.Error("trigger marker recorded despite trigger failure — next cycle would be debounced")
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
