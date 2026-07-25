package github

import "testing"

func TestCICheckRollupFingerprint_IsOrderStableAndSemantic(t *testing.T) {
	checks := []greptileCheckRun{
		{ID: 10, Name: "lint", Status: "completed", Conclusion: "success"},
		{ID: 20, Name: "test", Status: "in_progress"},
	}
	combined := combinedStatusResponse{State: "pending"}
	combined.Statuses = append(combined.Statuses, struct {
		Context     string `json:"context"`
		State       string `json:"state"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url"`
	}{Context: "legacy", State: "pending", Description: "private detail", TargetURL: "https://private.invalid"})

	first := ciCheckRollupFingerprint(checks, combined)
	reordered := ciCheckRollupFingerprint([]greptileCheckRun{checks[1], checks[0]}, combined)
	if first == "" || first != reordered {
		t.Fatalf("rollup fingerprint is not stable: first=%q reordered=%q", first, reordered)
	}
	checks[1].Status = "completed"
	checks[1].Conclusion = "success"
	green := ciCheckRollupFingerprint(checks, combined)
	if green == first {
		t.Fatalf("individual check transition did not change rollup fingerprint: %q", green)
	}
	checks[1] = greptileCheckRun{ID: 21, Name: "test", Status: "in_progress"}
	rerun := ciCheckRollupFingerprint(checks, combined)
	if rerun == first {
		t.Fatalf("same-head check rerun did not change rollup fingerprint: %q", rerun)
	}
	if len(first) != 16 {
		t.Fatalf("fingerprint length = %d, want bounded digest", len(first))
	}
}

func TestCICheckRollupFingerprintIgnoresSupersededAttemptHistory(t *testing.T) {
	current := greptileCheckRun{ID: 20, Name: "agent-lint", Status: "completed", Conclusion: "success", StartedAt: "2026-07-18T06:51:30Z"}
	stale := greptileCheckRun{ID: 10, Name: "agent-lint", Status: "completed", Conclusion: "failure", StartedAt: "2026-07-18T06:49:50Z"}

	want := ciCheckRollupFingerprint([]greptileCheckRun{current}, combinedStatusResponse{})
	got := ciCheckRollupFingerprint([]greptileCheckRun{stale, current}, combinedStatusResponse{})
	if got != want {
		t.Fatalf("superseded attempt changed fingerprint: got=%q want=%q", got, want)
	}
}

func TestCICheckRollupSignals_CarriesNonSecretCheckIdentity(t *testing.T) {
	checks := []greptileCheckRun{{ID: 10, Name: "Android SDK license acceptance gate", Status: "completed", Conclusion: "failure"}}
	combined := combinedStatusResponse{State: "failure"}
	combined.Statuses = append(combined.Statuses, struct {
		Context     string `json:"context"`
		State       string `json:"state"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url"`
	}{Context: "legacy/legal-gate", State: "pending", Description: "private detail", TargetURL: "https://private.invalid"})

	signals := ciCheckRollupSignals(checks, combined)
	if len(signals) != 2 {
		t.Fatalf("signals = %#v, want two", signals)
	}
	for _, signal := range signals {
		if signal.Name == "Android SDK license acceptance gate" && signal.Source == "check_run" && signal.Conclusion == "failure" {
			continue
		}
		if signal.Name == "legacy/legal-gate" && signal.Source == "commit_status" && signal.Status == "pending" {
			continue
		}
		t.Fatalf("unexpected signal: %#v", signal)
	}
}
