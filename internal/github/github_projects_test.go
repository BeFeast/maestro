package github

import (
	"reflect"
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

// parseNonDoneProjectItemsResponse must drop already-terminal items even when
// the board's terminal column is "Completed" or "Closed" rather than "Done".
// Without this, every finished item leaks back into reconcileProjectBoard and
// generates one redundant Done sync per RunOnce cycle (regression from #405
// review).
func TestParseNonDoneProjectItemsResponse_FiltersTerminalOptionID(t *testing.T) {
	body := []byte(`{
		"data": {
			"node": {
				"items": {
					"nodes": [
						{"fieldValueByName": {"optionId": "opt-progress"}, "content": {"number": 10, "state": "OPEN"}},
						{"fieldValueByName": {"optionId": "opt-completed"}, "content": {"number": 11, "state": "CLOSED"}},
						{"fieldValueByName": null, "content": {"number": 12, "state": "OPEN"}},
						{"fieldValueByName": {"optionId": "opt-progress"}, "content": null}
					]
				}
			}
		}
	}`)

	items, err := parseNonDoneProjectItemsResponse(body, "opt-completed")
	if err != nil {
		t.Fatalf("parseNonDoneProjectItemsResponse: %v", err)
	}

	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (terminal-column item must be filtered, dangling node skipped); got %+v", len(items), items)
	}
	if items[0].IssueNumber != 10 || !items[0].HasStatus || items[0].IssueClosed {
		t.Fatalf("items[0] = %+v, want issue 10 open with status", items[0])
	}
	if items[1].IssueNumber != 12 || items[1].HasStatus || items[1].IssueClosed {
		t.Fatalf("items[1] = %+v, want issue 12 open without status", items[1])
	}
}

func TestParseNonDoneProjectItemsResponse_NoDoneOptionKeepsTerminalItems(t *testing.T) {
	body := []byte(`{
		"data": {
			"node": {
				"items": {
					"nodes": [
						{"fieldValueByName": {"optionId": "opt-closed"}, "content": {"number": 7, "state": "CLOSED"}}
					]
				}
			}
		}
	}`)

	items, err := parseNonDoneProjectItemsResponse(body, "")
	if err != nil {
		t.Fatalf("parseNonDoneProjectItemsResponse: %v", err)
	}
	if len(items) != 1 || items[0].IssueNumber != 7 || !items[0].IssueClosed {
		t.Fatalf("items = %+v, want one closed item when terminal optionID is empty", items)
	}
}

func TestParseNonDoneProjectItemsResponse_GraphQLErrors(t *testing.T) {
	body := []byte(`{"errors":[{"message":"boom"}]}`)
	if _, err := parseNonDoneProjectItemsResponse(body, ""); err == nil {
		t.Fatal("parseNonDoneProjectItemsResponse should surface graphql errors")
	}
}

// ListNonDoneProjectItems must resolve the terminal-column option ID through
// ProjectStatusCandidates so a board whose Done column is named "Completed"
// or "Closed" still filters out finished items. The unit-test angle: ensure
// every Done candidate normalizes to a recognized board column name in
// resolveProjectStatusOption.
func TestProjectStatusCandidatesDoneResolvesCommonTerminalColumns(t *testing.T) {
	for _, columnName := range []string{"Done", "Completed", "Closed", "completed", "DONE"} {
		pf := &ProjectField{
			ProjectID: "p",
			FieldID:   "f",
			Options:   map[string]string{columnName: "opt-terminal"},
		}
		_, id, ok := resolveProjectStatusOption(pf, ProjectStatusCandidates(ProjectStatusDone))
		if !ok || id != "opt-terminal" {
			t.Fatalf("Done candidates failed to resolve board column %q (ok=%v, id=%q)", columnName, ok, id)
		}
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
		{ProjectStatusDeploying, "Deploying"},
		{ProjectStatusLiveVerify, "Live Verification"},
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

func TestProjectStatusCandidates_LifecycleStatusesDoNotCollapseToInProgress(t *testing.T) {
	for _, status := range []ProjectStatus{ProjectStatusDeploying, ProjectStatusLiveVerify} {
		for _, candidate := range ProjectStatusCandidates(status) {
			if strings.EqualFold(strings.TrimSpace(candidate), "In Progress") {
				t.Fatalf("ProjectStatusCandidates(%q) includes %q; lifecycle status must not silently collapse to In Progress", status, candidate)
			}
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

func TestTrySyncIssueStatusOneOf_NilProjectField(t *testing.T) {
	c := New("owner/repo")
	if c.TrySyncIssueStatusOneOf(nil, 1, "In Review") {
		t.Fatal("TrySyncIssueStatusOneOf should report false for nil project field")
	}
}

func TestParseIssueNodeIDUsesRESTNodeID(t *testing.T) {
	got, err := parseIssueNodeID(42, []byte(`{"node_id":"I_kwDOExample"}`))
	if err != nil {
		t.Fatalf("parseIssueNodeID() error = %v", err)
	}
	if got != "I_kwDOExample" {
		t.Fatalf("node id = %q, want I_kwDOExample", got)
	}

	if _, err := parseIssueNodeID(42, []byte(`{"id":123}`)); err == nil {
		t.Fatal("parseIssueNodeID() should reject missing REST node_id")
	}
}

func TestParseDiscoverProjectResponse_PreservesOwnerAndOptionOrder(t *testing.T) {
	body := []byte(`{
		"data": {
			"repositoryOwner": {
				"__typename": "Organization",
				"projectV2": {
					"id": "project-id",
					"field": {
						"id": "field-id",
						"options": [
							{"id": "todo-id", "name": "Todo"},
							{"id": "progress-id", "name": "In Progress"},
							{"id": "done-id", "name": "Done"}
						]
					}
				}
			}
		}
	}`)
	pf, err := parseDiscoverProjectResponse("befeast", 5, body)
	if err != nil {
		t.Fatalf("parseDiscoverProjectResponse: %v", err)
	}
	if pf.Owner != "befeast" || pf.OwnerType != "Organization" || pf.Number != 5 {
		t.Fatalf("owner metadata = %q/%q/%d, want befeast/Organization/5", pf.Owner, pf.OwnerType, pf.Number)
	}
	want := []string{"Todo", "In Progress", "Done"}
	if !reflect.DeepEqual(pf.OptionOrder, want) {
		t.Fatalf("OptionOrder = %v, want %v", pf.OptionOrder, want)
	}
}

func TestProjectBoardURL_OrgVsUser(t *testing.T) {
	org := &ProjectField{Owner: "befeast", OwnerType: "Organization", Number: 5}
	if got, want := ProjectBoardURL(org), "https://github.com/orgs/befeast/projects/5"; got != want {
		t.Errorf("org URL = %q, want %q", got, want)
	}
	user := &ProjectField{Owner: "kossoy", OwnerType: "User", Number: 2}
	if got, want := ProjectBoardURL(user), "https://github.com/users/kossoy/projects/2"; got != want {
		t.Errorf("user URL = %q, want %q", got, want)
	}
	missing := &ProjectField{Owner: "", OwnerType: "Organization", Number: 5}
	if got := ProjectBoardURL(missing); got != "" {
		t.Errorf("missing owner URL = %q, want empty", got)
	}
	if got := ProjectBoardURL(nil); got != "" {
		t.Errorf("nil URL = %q, want empty", got)
	}
}

func TestProjectBoardIssueFilterURL_AppendsFilterQuery(t *testing.T) {
	pf := &ProjectField{Owner: "befeast", OwnerType: "Organization", Number: 5}
	got := ProjectBoardIssueFilterURL(pf, 529)
	want := "https://github.com/orgs/befeast/projects/5?pane=info&filterQuery=%23529"
	if got != want {
		t.Errorf("ProjectBoardIssueFilterURL = %q, want %q", got, want)
	}
	if got := ProjectBoardIssueFilterURL(pf, 0); got != "https://github.com/orgs/befeast/projects/5" {
		t.Errorf("issue=0 URL = %q, want bare board URL", got)
	}
}

func TestParseProjectItemStatusCountsResponse_PreservesBoardOrder(t *testing.T) {
	pf := &ProjectField{
		Options: map[string]string{
			"Todo":        "opt-todo",
			"In Progress": "opt-progress",
			"Done":        "opt-done",
		},
		OptionOrder: []string{"Todo", "In Progress", "Done"},
	}
	body := []byte(`{
		"data": {
			"node": {
				"items": {
					"totalCount": 6,
					"nodes": [
						{"fieldValueByName": {"optionId": "opt-todo", "name": "Todo"}},
						{"fieldValueByName": {"optionId": "opt-todo", "name": "Todo"}},
						{"fieldValueByName": {"optionId": "opt-progress", "name": "In Progress"}},
						{"fieldValueByName": {"optionId": "opt-done", "name": "Done"}},
						{"fieldValueByName": null},
						{"fieldValueByName": {"optionId": "", "name": ""}}
					]
				}
			}
		}
	}`)
	columns, total, err := parseProjectItemStatusCountsResponse(body, pf)
	if err != nil {
		t.Fatalf("parseProjectItemStatusCountsResponse: %v", err)
	}
	if total != 6 {
		t.Errorf("total = %d, want 6", total)
	}
	want := []ProjectBoardColumn{
		{Name: "Todo", OptionID: "opt-todo", Count: 2},
		{Name: "In Progress", OptionID: "opt-progress", Count: 1},
		{Name: "Done", OptionID: "opt-done", Count: 1},
		{Name: "No Status", Count: 2},
	}
	if !reflect.DeepEqual(columns, want) {
		t.Fatalf("columns = %#v, want %#v", columns, want)
	}
}

func TestParseProjectItemStatusCountsResponse_SurfacesGraphQLErrors(t *testing.T) {
	body := []byte(`{"errors":[{"message":"forbidden"}]}`)
	if _, _, err := parseProjectItemStatusCountsResponse(body, &ProjectField{}); err == nil {
		t.Fatal("expected graphql error")
	}
}

func TestListProjectItemStatusCounts_NilProjectField(t *testing.T) {
	c := New("owner/repo")
	if _, _, err := c.ListProjectItemStatusCounts(nil); err == nil {
		t.Fatal("expected error for nil ProjectField")
	}
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
