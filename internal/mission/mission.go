package mission

import (
	"fmt"
	"log"
	"strings"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// ChildTask represents a parsed subtask from a mission issue body.
type ChildTask struct {
	Title string
	Body  string
}

// ChildStatus tracks a child issue's progress for parent checklist updates.
type ChildStatus struct {
	Number int
	Title  string
	Closed bool
}

// Processor handles mission decomposition and progress tracking.
type Processor struct {
	cfg *config.Config
	gh  *github.Client

	// Testing hooks
	CreateIssueFn   func(title, body string, labels []string) (int, error)
	EditIssueBodyFn func(number int, body string) error
	IsIssueClosedFn func(number int) (bool, error)
	GetIssueFn      func(number int) (github.Issue, error)
}

// NewProcessor creates a new mission processor.
func NewProcessor(cfg *config.Config, gh *github.Client) *Processor {
	return &Processor{cfg: cfg, gh: gh}
}

func (p *Processor) createIssue(title, body string, labels []string) (int, error) {
	if p.CreateIssueFn != nil {
		return p.CreateIssueFn(title, body, labels)
	}
	return p.gh.CreateIssue(title, body, labels)
}

func (p *Processor) editIssueBody(number int, body string) error {
	if p.EditIssueBodyFn != nil {
		return p.EditIssueBodyFn(number, body)
	}
	return p.gh.EditIssueBody(number, body)
}

func (p *Processor) isIssueClosed(number int) (bool, error) {
	if p.IsIssueClosedFn != nil {
		return p.IsIssueClosedFn(number)
	}
	return p.gh.IsIssueClosed(number)
}

func (p *Processor) getIssue(number int) (github.Issue, error) {
	if p.GetIssueFn != nil {
		return p.GetIssueFn(number)
	}
	return p.gh.GetIssue(number)
}

// IsMissionIssue returns true if the issue has a mission/epic label.
func IsMissionIssue(issue github.Issue, missionLabels []string) bool {
	return github.HasLabel(issue, missionLabels)
}

// DecomposeTasks parses the issue body for a task list.
// It looks for markdown list items (- [ ] or - or * or numbered) under
// headings containing "tasks", "subtasks", "plan", "milestones", or "steps".
// Falls back to any top-level list items if no matching section is found.
func DecomposeTasks(body string) []ChildTask {
	lines := strings.Split(body, "\n")

	var tasks []ChildTask
	inTaskSection := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Detect headings that indicate a task section
		if isHeading(trimmed) {
			lower := strings.ToLower(trimmed)
			inTaskSection = containsTaskKeyword(lower)
			continue
		}

		if !inTaskSection {
			continue
		}

		// Parse list items
		title := parseListItem(trimmed)
		if title == "" {
			continue
		}

		tasks = append(tasks, ChildTask{Title: title})
	}

	// Fallback: if no task section found, scan entire body for checkbox items
	if len(tasks) == 0 {
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if title := parseCheckboxItem(trimmed); title != "" {
				tasks = append(tasks, ChildTask{Title: title})
			}
		}
	}

	return tasks
}

// ProcessMissions scans open issues for missions, decomposes new ones,
// and updates progress on active missions.
func (p *Processor) ProcessMissions(s *state.State, issues []github.Issue) {
	if !p.cfg.Missions.Enabled {
		return
	}

	// Ensure missions map is initialized
	if s.Missions == nil {
		s.Missions = make(map[int]*state.Mission)
	}

	// Phase 1: Detect and decompose new mission issues
	for _, issue := range issues {
		if !IsMissionIssue(issue, p.cfg.Missions.Labels) {
			continue
		}

		// Skip if already tracked
		if _, ok := s.Missions[issue.Number]; ok {
			continue
		}

		log.Printf("[mission] detected new mission issue #%d: %s", issue.Number, issue.Title)
		p.decomposeMission(s, issue)
	}

	// Phase 2: Update progress on active missions
	for parentNum, m := range s.Missions {
		if m.Status == "done" {
			continue
		}
		p.updateMissionProgress(s, parentNum, m)
	}
}

// decomposeMission parses the issue body, creates child issues, and records the mission.
func (p *Processor) decomposeMission(s *state.State, issue github.Issue) {
	tasks := DecomposeTasks(issue.Body)
	if len(tasks) == 0 {
		log.Printf("[mission] no tasks found in issue #%d body, skipping decomposition", issue.Number)
		return
	}

	// Enforce max_children
	if len(tasks) > p.cfg.Missions.MaxChildren {
		log.Printf("[mission] issue #%d has %d tasks, capping at %d", issue.Number, len(tasks), p.cfg.Missions.MaxChildren)
		tasks = tasks[:p.cfg.Missions.MaxChildren]
	}

	// Determine labels for child issues: use the configured issue_labels
	// so maestro picks them up for dispatch
	childLabels := make([]string, len(p.cfg.IssueLabels))
	copy(childLabels, p.cfg.IssueLabels)

	var childNumbers []int
	var prevChildNum int

	for i, task := range tasks {
		// Build child issue body with parent reference and blocker
		body := formatChildBody(issue.Number, task, prevChildNum)

		title := fmt.Sprintf("%s (#%d/%d)", task.Title, i+1, len(tasks))
		childNum, err := p.createIssue(title, body, childLabels)
		if err != nil {
			log.Printf("[mission] failed to create child issue %d/%d for #%d: %v",
				i+1, len(tasks), issue.Number, err)
			continue
		}

		log.Printf("[mission] created child issue #%d: %s", childNum, title)
		childNumbers = append(childNumbers, childNum)

		// Sync to GitHub Projects if enabled
		if p.cfg.GitHubProjects.Enabled && p.cfg.GitHubProjects.ProjectNumber > 0 {
			p.gh.SyncIssueToProject(childNum, p.cfg.GitHubProjects.ProjectNumber, github.ProjectStatusTodo)
		}

		prevChildNum = childNum
	}

	if len(childNumbers) == 0 {
		log.Printf("[mission] no child issues created for #%d", issue.Number)
		return
	}

	// Record mission in state
	s.Missions[issue.Number] = &state.Mission{
		ParentIssue: issue.Number,
		ChildIssues: childNumbers,
		Status:      "active",
	}

	// Update parent issue with checklist
	checklist := formatParentChecklist(childNumbers, issue.Body)
	if err := p.editIssueBody(issue.Number, checklist); err != nil {
		log.Printf("[mission] failed to update parent issue #%d body: %v", issue.Number, err)
	}

	log.Printf("[mission] decomposed issue #%d into %d child issues: %v", issue.Number, len(childNumbers), childNumbers)
}

// updateMissionProgress checks child issue status and updates the parent.
func (p *Processor) updateMissionProgress(s *state.State, parentNum int, m *state.Mission) {
	var statuses []ChildStatus
	allClosed := true

	for _, childNum := range m.ChildIssues {
		closed, err := p.isIssueClosed(childNum)
		if err != nil {
			log.Printf("[mission] could not check status of child issue #%d: %v", childNum, err)
			allClosed = false
			continue
		}

		childIssue, err := p.getIssue(childNum)
		title := fmt.Sprintf("#%d", childNum)
		if err == nil {
			title = childIssue.Title
		}

		statuses = append(statuses, ChildStatus{
			Number: childNum,
			Title:  title,
			Closed: closed,
		})

		if !closed {
			allClosed = false
		}
	}

	// Update parent issue body with current progress
	progressBody := formatProgressUpdate(parentNum, statuses)
	if err := p.editIssueBody(parentNum, progressBody); err != nil {
		log.Printf("[mission] failed to update parent #%d progress: %v", parentNum, err)
	}

	// If all children are done, mark mission as done
	if allClosed && len(m.ChildIssues) > 0 {
		m.Status = "done"
		log.Printf("[mission] all child issues closed for mission #%d — marking done", parentNum)

		// Sync parent to done in GitHub Projects
		if p.cfg.GitHubProjects.Enabled && p.cfg.GitHubProjects.ProjectNumber > 0 {
			p.gh.SyncIssueToProject(parentNum, p.cfg.GitHubProjects.ProjectNumber, github.ProjectStatusDone)
		}
	}
}

// formatChildBody builds the body for a child issue.
func formatChildBody(parentNumber int, task ChildTask, blockerNumber int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Part of mission #%d\n\n", parentNumber))

	if task.Body != "" {
		sb.WriteString(task.Body)
		sb.WriteString("\n\n")
	}

	if blockerNumber > 0 {
		sb.WriteString(fmt.Sprintf("Blocked by #%d\n", blockerNumber))
	}

	return sb.String()
}

// formatParentChecklist appends a progress checklist to the original issue body.
func formatParentChecklist(childNumbers []int, originalBody string) string {
	var sb strings.Builder
	sb.WriteString(originalBody)
	sb.WriteString("\n\n---\n\n## Mission Progress\n\n")

	for i, num := range childNumbers {
		sb.WriteString(fmt.Sprintf("- [ ] #%d (step %d/%d)\n", num, i+1, len(childNumbers)))
	}

	return sb.String()
}

// formatProgressUpdate generates a full progress checklist for the parent issue.
func formatProgressUpdate(parentNum int, statuses []ChildStatus) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Mission #%d Progress\n\n", parentNum))

	doneCount := 0
	for i, cs := range statuses {
		check := " "
		if cs.Closed {
			check = "x"
			doneCount++
		}
		sb.WriteString(fmt.Sprintf("- [%s] #%d — %s (step %d/%d)\n", check, cs.Number, cs.Title, i+1, len(statuses)))
	}

	sb.WriteString(fmt.Sprintf("\n**Progress: %d/%d complete**\n", doneCount, len(statuses)))

	return sb.String()
}

// isHeading returns true if the line is a markdown heading.
func isHeading(line string) bool {
	return strings.HasPrefix(line, "#")
}

// containsTaskKeyword returns true if the heading text suggests a task section.
func containsTaskKeyword(lower string) bool {
	keywords := []string{"task", "subtask", "plan", "milestone", "step", "breakdown", "work item", "child issue"}
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// parseListItem extracts the text from a markdown list item.
// Supports: "- text", "* text", "1. text", "- [ ] text", "- [x] text"
func parseListItem(line string) string {
	// Checkbox items
	if title := parseCheckboxItem(line); title != "" {
		return title
	}

	// Unordered list: - or *
	for _, prefix := range []string{"- ", "* "} {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}

	// Numbered list: "1. text"
	for i, ch := range line {
		if ch >= '0' && ch <= '9' {
			continue
		}
		if ch == '.' && i > 0 && i < len(line)-1 && line[i+1] == ' ' {
			return strings.TrimSpace(line[i+2:])
		}
		break
	}

	return ""
}

// parseCheckboxItem extracts text from "- [ ] text" or "- [x] text" patterns.
func parseCheckboxItem(line string) string {
	for _, prefix := range []string{"- [ ] ", "- [x] ", "- [X] ", "* [ ] ", "* [x] ", "* [X] "} {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}
