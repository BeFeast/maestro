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
