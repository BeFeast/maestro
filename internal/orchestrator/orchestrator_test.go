package orchestrator

import "testing"

func TestHasLabel(t *testing.T) {
	labels := []string{"enhancement", "long-running", "bug"}

	if !hasLabel(labels, "long-running") {
		t.Error("expected hasLabel to find 'long-running'")
	}
	if !hasLabel(labels, "Long-Running") {
		t.Error("expected hasLabel to be case-insensitive")
	}
	if hasLabel(labels, "blocked") {
		t.Error("expected hasLabel to return false for 'blocked'")
	}
	if hasLabel(nil, "long-running") {
		t.Error("expected hasLabel to return false for nil labels")
	}
}
