package mission

import (
	"fmt"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// makeIssue creates a github.Issue with the given labels (works around json struct tags).
func makeIssue(number int, title, body string, labelNames ...string) github.Issue {
	issue := github.Issue{Number: number, Title: title, Body: body}
	for _, name := range labelNames {
		issue.Labels = append(issue.Labels, struct {
			Name string `json:"name"`
		}{Name: name})
	}
	return issue
}

func TestDecomposeTasks_TaskSection(t *testing.T) {
	body := `## Summary
Some epic description.

## Tasks

- Build the auth module
- Implement the API endpoints
- Write integration tests

## Notes
Some extra info.
`
	tasks := DecomposeTasks(body)
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d: %v", len(tasks), tasks)
	}
	want := []string{"Build the auth module", "Implement the API endpoints", "Write integration tests"}
	for i, task := range tasks {
		if task.Title != want[i] {
			t.Errorf("task %d: got %q, want %q", i, task.Title, want[i])
		}
	}
}

func TestDecomposeTasks_CheckboxFallback(t *testing.T) {
	body := `## Epic: Build feature X

- [ ] Design the schema
- [x] Create the migration
- [ ] Implement the handler
`
	tasks := DecomposeTasks(body)
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d: %v", len(tasks), tasks)
	}
	if tasks[0].Title != "Design the schema" {
		t.Errorf("task 0: got %q, want %q", tasks[0].Title, "Design the schema")
	}
}

func TestDecomposeTasks_NumberedList(t *testing.T) {
	body := `## Plan

1. First step
2. Second step
3. Third step
`
	tasks := DecomposeTasks(body)
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d: %v", len(tasks), tasks)
	}
	if tasks[0].Title != "First step" {
		t.Errorf("task 0: got %q, want %q", tasks[0].Title, "First step")
	}
}

func TestDecomposeTasks_MilestoneHeading(t *testing.T) {
	body := `## Milestones

- Phase 1: Foundation
- Phase 2: Features
`
	tasks := DecomposeTasks(body)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestDecomposeTasks_StepsHeading(t *testing.T) {
	body := `### Steps

* Step A
* Step B
`
	tasks := DecomposeTasks(body)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	if tasks[0].Title != "Step A" {
		t.Errorf("task 0: got %q, want %q", tasks[0].Title, "Step A")
	}
}

func TestDecomposeTasks_EmptyBody(t *testing.T) {
	tasks := DecomposeTasks("")
	if len(tasks) != 0 {
		t.Fatalf("expected 0 tasks, got %d", len(tasks))
	}
}

func TestDecomposeTasks_NoTaskSection(t *testing.T) {
	body := `## Summary
Just a plain description with no task items.
`
	tasks := DecomposeTasks(body)
	if len(tasks) != 0 {
		t.Fatalf("expected 0 tasks from body without lists, got %d", len(tasks))
	}
}

func TestIsMissionIssue(t *testing.T) {
	tests := []struct {
		name  string
		issue github.Issue
		want  bool
	}{
		{
			name:  "mission label",
			issue: makeIssue(1, "test", "", "mission"),
			want:  true,
		},
		{
			name:  "epic label",
			issue: makeIssue(2, "test", "", "Epic"),
			want:  true,
		},
		{
			name:  "no mission label",
			issue: makeIssue(3, "test", "", "bug"),
			want:  false,
		},
		{
			name:  "empty labels",
			issue: makeIssue(4, "test", ""),
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsMissionIssue(tt.issue, []string{"mission", "epic"})
			if got != tt.want {
				t.Errorf("IsMissionIssue() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFormatChildBody(t *testing.T) {
	task := ChildTask{Title: "Build auth", Body: "Implement OAuth2 flow"}
	body := formatChildBody(42, task, 10)

	if !strings.Contains(body, "Part of mission #42") {
		t.Errorf("body missing parent ref: %s", body)
	}
	if !strings.Contains(body, "Blocked by #10") {
		t.Errorf("body missing blocker ref: %s", body)
	}
	if !strings.Contains(body, "Implement OAuth2 flow") {
		t.Errorf("body missing task body: %s", body)
	}
}

func TestFormatChildBody_NoBlocker(t *testing.T) {
	task := ChildTask{Title: "First task"}
	body := formatChildBody(1, task, 0)
	if strings.Contains(body, "Blocked by") {
		t.Errorf("body should not have blocker for first task: %s", body)
	}
}

func TestProcessMissions_DecomposeAndTrack(t *testing.T) {
	cfg := &config.Config{
		Repo:        "owner/repo",
		IssueLabels: []string{"maestro"},
		Missions: config.MissionsConfig{
			Enabled:     true,
			MaxChildren: 10,
			Labels:      []string{"mission", "epic"},
		},
	}

	gh := github.New("owner/repo")
	proc := NewProcessor(cfg, gh)

	childCounter := 100
	proc.CreateIssueFn = func(title, body string, labels []string) (int, error) {
		childCounter++
		return childCounter, nil
	}
	proc.EditIssueBodyFn = func(number int, body string) error {
		return nil
	}
	proc.IsIssueClosedFn = func(number int) (bool, error) {
		return false, nil
	}
	proc.GetIssueFn = func(number int) (github.Issue, error) {
		return github.Issue{Number: number, Title: fmt.Sprintf("Child #%d", number)}, nil
	}

	s := state.NewState()

	issues := []github.Issue{
		makeIssue(1, "Epic: Build the platform", `## Summary
Build everything.

## Tasks

- Design the schema
- Build the API
- Build the frontend
`, "mission"),
	}

	proc.ProcessMissions(s, issues)

	if len(s.Missions) != 1 {
		t.Fatalf("expected 1 mission, got %d", len(s.Missions))
	}

	m, ok := s.Missions[1]
	if !ok {
		t.Fatal("expected mission for issue #1")
	}
	if len(m.ChildIssues) != 3 {
		t.Fatalf("expected 3 child issues, got %d", len(m.ChildIssues))
	}
	if m.Status != "active" {
		t.Errorf("expected status 'active', got %q", m.Status)
	}
	for i, num := range m.ChildIssues {
		if num != 101+i {
			t.Errorf("child %d: expected #%d, got #%d", i, 101+i, num)
		}
	}
}

func TestProcessMissions_SkipAlreadyTracked(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Missions: config.MissionsConfig{
			Enabled:     true,
			MaxChildren: 10,
			Labels:      []string{"mission"},
		},
	}

	gh := github.New("owner/repo")
	proc := NewProcessor(cfg, gh)

	createCalled := false
	proc.CreateIssueFn = func(title, body string, labels []string) (int, error) {
		createCalled = true
		return 0, fmt.Errorf("should not be called")
	}
	proc.IsIssueClosedFn = func(number int) (bool, error) {
		return false, nil
	}
	proc.GetIssueFn = func(number int) (github.Issue, error) {
		return github.Issue{Number: number, Title: "child"}, nil
	}
	proc.EditIssueBodyFn = func(number int, body string) error {
		return nil
	}

	s := state.NewState()
	s.Missions[1] = &state.Mission{
		ParentIssue: 1,
		ChildIssues: []int{10, 11},
		Status:      "active",
	}

	issues := []github.Issue{makeIssue(1, "Already tracked mission", "", "mission")}

	proc.ProcessMissions(s, issues)

	if createCalled {
		t.Error("CreateIssue should not be called for already-tracked mission")
	}
}

func TestProcessMissions_MaxChildrenCap(t *testing.T) {
	cfg := &config.Config{
		Repo:        "owner/repo",
		IssueLabels: []string{"maestro"},
		Missions: config.MissionsConfig{
			Enabled:     true,
			MaxChildren: 2,
			Labels:      []string{"mission"},
		},
	}

	gh := github.New("owner/repo")
	proc := NewProcessor(cfg, gh)

	childCounter := 200
	proc.CreateIssueFn = func(title, body string, labels []string) (int, error) {
		childCounter++
		return childCounter, nil
	}
	proc.EditIssueBodyFn = func(number int, body string) error {
		return nil
	}
	proc.IsIssueClosedFn = func(number int) (bool, error) {
		return false, nil
	}
	proc.GetIssueFn = func(number int) (github.Issue, error) {
		return github.Issue{Number: number, Title: "child"}, nil
	}

	s := state.NewState()

	issues := []github.Issue{
		makeIssue(5, "Big epic", `## Tasks
- Task 1
- Task 2
- Task 3
- Task 4
`, "mission"),
	}

	proc.ProcessMissions(s, issues)

	m := s.Missions[5]
	if m == nil {
		t.Fatal("expected mission for issue #5")
	}
	if len(m.ChildIssues) != 2 {
		t.Fatalf("expected 2 child issues (capped), got %d", len(m.ChildIssues))
	}
}

func TestProcessMissions_AllChildrenClosed(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Missions: config.MissionsConfig{
			Enabled:     true,
			MaxChildren: 10,
			Labels:      []string{"mission"},
		},
	}

	gh := github.New("owner/repo")
	proc := NewProcessor(cfg, gh)

	proc.IsIssueClosedFn = func(number int) (bool, error) {
		return true, nil
	}
	proc.GetIssueFn = func(number int) (github.Issue, error) {
		return github.Issue{Number: number, Title: "done child"}, nil
	}
	proc.EditIssueBodyFn = func(number int, body string) error {
		return nil
	}

	s := state.NewState()
	s.Missions[1] = &state.Mission{
		ParentIssue: 1,
		ChildIssues: []int{10, 11, 12},
		Status:      "active",
	}

	proc.ProcessMissions(s, nil)

	if s.Missions[1].Status != "done" {
		t.Errorf("expected mission status 'done', got %q", s.Missions[1].Status)
	}
}

func TestProcessMissions_Disabled(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/repo",
		Missions: config.MissionsConfig{
			Enabled: false,
		},
	}

	gh := github.New("owner/repo")
	proc := NewProcessor(cfg, gh)

	s := state.NewState()
	issues := []github.Issue{makeIssue(1, "test", "", "mission")}

	proc.ProcessMissions(s, issues)

	if len(s.Missions) != 0 {
		t.Error("missions should not be processed when disabled")
	}
}

func TestParseListItem(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"- Simple item", "Simple item"},
		{"* Star item", "Star item"},
		{"- [ ] Unchecked", "Unchecked"},
		{"- [x] Checked", "Checked"},
		{"1. Numbered", "Numbered"},
		{"12. Multi-digit", "Multi-digit"},
		{"Not a list item", ""},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseListItem(tt.input)
			if got != tt.want {
				t.Errorf("parseListItem(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestContainsTaskKeyword(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"## tasks", true},
		{"## subtasks", true},
		{"## implementation plan", true},
		{"## milestones", true},
		{"## steps", true},
		{"## breakdown", true},
		{"## summary", false},
		{"## notes", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := containsTaskKeyword(tt.input)
			if got != tt.want {
				t.Errorf("containsTaskKeyword(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatProgressUpdate(t *testing.T) {
	statuses := []ChildStatus{
		{Number: 10, Title: "Design schema", Closed: true},
		{Number: 11, Title: "Build API", Closed: false},
		{Number: 12, Title: "Build frontend", Closed: false},
	}

	body := formatProgressUpdate(1, statuses)

	if !strings.Contains(body, "[x] #10") {
		t.Error("expected closed child to have [x]")
	}
	if !strings.Contains(body, "[ ] #11") {
		t.Error("expected open child to have [ ]")
	}
	if !strings.Contains(body, "1/3 complete") {
		t.Errorf("expected progress count, got: %s", body)
	}
}
