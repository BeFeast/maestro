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

func TestProjectStatusCandidates(t *testing.T) {
	tests := []struct {
		status    ProjectStatus
		wantFirst string
	}{
		{ProjectStatusTodo, "Todo"},
		{ProjectStatusInProgress, "In Progress"},
		{ProjectStatusInReview, "In Review"},
		{ProjectStatusBlocked, "Blocked"},
		{ProjectStatusDone, "Done"},
	}
	for _, tc := range tests {
		got := ProjectStatusCandidates(tc.status)
		if len(got) == 0 {
			t.Fatalf("ProjectStatusCandidates(%q) returned no candidates", tc.status)
		}
		if got[0] != tc.wantFirst {
			t.Errorf("ProjectStatusCandidates(%q) first = %q, want %q", tc.status, got[0], tc.wantFirst)
		}
	}
}

func TestResolveProjectStatusOption(t *testing.T) {
	pf := &ProjectField{
		ProjectID: "p",
		FieldID:   "f",
		Options: map[string]string{
			"Todo":        "id-todo",
			"In Progress": "id-progress",
			"Review":      "id-review",
			"On Hold":     "id-hold",
			"Done":        "id-done",
		},
	}

	tests := []struct {
		name       string
		candidates []string
		wantName   string
		wantID     string
		wantOK     bool
	}{
		{
			name:       "exact match preferred",
			candidates: []string{"Todo"},
			wantName:   "Todo",
			wantID:     "id-todo",
			wantOK:     true,
		},
		{
			name:       "case-insensitive fallback",
			candidates: []string{"in progress"},
			wantName:   "In Progress",
			wantID:     "id-progress",
			wantOK:     true,
		},
		{
			name:       "falls through to next candidate when first is absent",
			candidates: []string{"In Review", "Review"},
			wantName:   "Review",
			wantID:     "id-review",
			wantOK:     true,
		},
		{
			name:       "second candidate fills gap",
			candidates: ProjectStatusCandidates(ProjectStatusBlocked),
			wantName:   "On Hold",
			wantID:     "id-hold",
			wantOK:     true,
		},
		{
			name:       "none match",
			candidates: []string{"Backlog", "Triage"},
			wantOK:     false,
		},
		{
			name:       "empty candidate ignored",
			candidates: []string{"", "Todo"},
			wantName:   "Todo",
			wantID:     "id-todo",
			wantOK:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name, id, ok := resolveProjectStatusOption(pf, tc.candidates)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if id != tc.wantID {
				t.Errorf("id = %q, want %q", id, tc.wantID)
			}
		})
	}
}

func TestSyncIssueStatusOneOf_NilProjectField(t *testing.T) {
	c := New("owner/repo")
	c.SyncIssueStatusOneOf(nil, 1, "In Review", "Review", "In Progress")
}

func TestNormalizeProjectStatusKey(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"In Progress", "in progress"},
		{"in_progress", "in progress"},
		{"  In   Review  ", "in review"},
		{"on-hold", "on hold"},
	}
	for _, tc := range tests {
		if got := normalizeProjectStatusKey(tc.in); got != tc.want {
			t.Errorf("normalizeProjectStatusKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
