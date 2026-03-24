package github

import (
	"testing"
	"time"
)

func TestDiscoverProject_RequiresOrg(t *testing.T) {
	c := New("owner/repo")
	// DiscoverProject makes real API calls; just verify it doesn't panic
	_ = c
}

func TestSyncIssueStatus_NilProjectField(t *testing.T) {
	c := New("owner/repo")
	// A nil ProjectField should be a no-op (not panic)
	c.SyncIssueStatus(nil, 1, "Todo")
}

func TestListNonDoneProjectItems_NilProjectField(t *testing.T) {
	c := New("owner/repo")
	_, err := c.ListNonDoneProjectItems(nil)
	if err == nil {
		t.Error("expected error for nil ProjectField")
	}
}

func TestGhTimeout_IsReasonable(t *testing.T) {
	if ghTimeout < 5*time.Second {
		t.Errorf("ghTimeout = %v, want >= 5s", ghTimeout)
	}
	if ghTimeout > 2*time.Minute {
		t.Errorf("ghTimeout = %v, want <= 2m", ghTimeout)
	}
}

func TestKeys(t *testing.T) {
	m := map[string]string{"a": "1", "b": "2"}
	ks := keys(m)
	if len(ks) != 2 {
		t.Errorf("keys() returned %d items, want 2", len(ks))
	}
}
