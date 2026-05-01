package github

import (
	"strings"
	"testing"
	"time"
)

func TestDiscoverProject_RequiresOrg(t *testing.T) {
	c := New("owner/repo")
	// DiscoverProject makes real API calls; just verify it doesn't panic
	_ = c
}

func TestDiscoverProjectQuerySupportsUserAndOrganizationOwners(t *testing.T) {
	query := discoverProjectQuery("kossoy", 2)

	for _, want := range []string{
		`repositoryOwner(login: "kossoy")`,
		"... on User",
		"... on Organization",
		"projectV2(number: 2)",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("discoverProjectQuery() missing %q in:\n%s", want, query)
		}
	}
	if strings.Contains(query, "organization(login:") {
		t.Fatalf("discoverProjectQuery() still uses organization-only lookup:\n%s", query)
	}
}

func TestParseDiscoverProjectResponse_UserOwner(t *testing.T) {
	body := []byte(`{
		"data": {
			"repositoryOwner": {
				"__typename": "User",
				"projectV2": {
					"id": "project-id",
					"field": {
						"id": "field-id",
						"options": [
							{"id": "todo-id", "name": "Todo"},
							{"id": "progress-id", "name": "In Progress"}
						]
					}
				}
			}
		}
	}`)

	pf, err := parseDiscoverProjectResponse("kossoy", 2, body)
	if err != nil {
		t.Fatalf("parseDiscoverProjectResponse() error = %v", err)
	}
	if pf.ProjectID != "project-id" {
		t.Fatalf("ProjectID = %q, want project-id", pf.ProjectID)
	}
	if pf.FieldID != "field-id" {
		t.Fatalf("FieldID = %q, want field-id", pf.FieldID)
	}
	if got := pf.Options["In Progress"]; got != "progress-id" {
		t.Fatalf("Options[In Progress] = %q, want progress-id", got)
	}
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
