package github

import "testing"

func TestCICheckRollupFingerprint_IsOrderStableAndSemantic(t *testing.T) {
	checks := []greptileCheckRun{
		{Name: "lint", Status: "completed", Conclusion: "success"},
		{Name: "test", Status: "in_progress"},
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
	if len(first) != 16 {
		t.Fatalf("fingerprint length = %d, want bounded digest", len(first))
	}
}
