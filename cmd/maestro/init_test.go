package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
)

func TestBuildProjectStatusJSONIncludesOutcome(t *testing.T) {
	cfg := &config.Config{
		Repo:          "org/repo",
		SessionPrefix: "rep",
		Outcome: outcome.Brief{
			DesiredOutcome: "Repo is live",
			RuntimeTarget:  "https://repo.example.com",
			HealthcheckURL: "https://repo.example.com/healthz",
		},
	}
	st := state.NewState()
	st.Sessions["done"] = &state.Session{IssueNumber: 1, Status: state.StatusDone, PRNumber: 10}

	got := buildProjectStatusJSON(cfg, st)
	if got.Repo != "org/repo" || got.Prefix != "rep" {
		t.Fatalf("project metadata = %q/%q, want org/repo rep", got.Repo, got.Prefix)
	}
	if !got.Outcome.Configured || got.Outcome.Goal != "Repo is live" || got.Outcome.HealthState != outcome.HealthUnknown {
		t.Fatalf("outcome = %+v, want configured unknown health", got.Outcome)
	}
}

// The retired init redirect must point at the genesis flow and must NOT recommend
// the retired per-project topology (maestro run / per-project systemctl units).
func TestRunInitRedirect(t *testing.T) {
	var buf bytes.Buffer
	runInitRedirect(&buf)
	out := buf.String()

	mustContain := []string{
		"maestro init is retired",
		"maestro project plan",
		"maestro project apply",
		"--watch-store",
		"--confirm",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("redirect output missing %q, got:\n%s", want, out)
		}
	}

	// It must not steer operators back to the retired single-project run/service
	// topology.
	mustNotContain := []string{
		"maestro run",
		"systemctl --user enable",
		"maestro@",
	}
	for _, bad := range mustNotContain {
		if strings.Contains(out, bad) {
			t.Errorf("redirect output should not mention retired topology %q, got:\n%s", bad, out)
		}
	}
}

// The redirect writes nothing to disk — no maestro.yaml, no ~/.maestro, no unit
// files. It only prints guidance.
func TestRunInitRedirectWritesNoFiles(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	var buf bytes.Buffer
	runInitRedirect(&buf)

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("read temp home: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("redirect created files under HOME: %v", names)
	}
}
