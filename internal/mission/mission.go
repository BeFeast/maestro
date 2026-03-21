package mission

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// Decomposer handles breaking down mission/epic issues into child issues.
type Decomposer struct {
	gh  *github.Client
	cfg *config.Config
}

// NewDecomposer creates a new mission Decomposer.
func NewDecomposer(gh *github.Client, cfg *config.Config) *Decomposer {
	return &Decomposer{gh: gh, cfg: cfg}
}

// ChildSpec describes a child issue to create from a mission decomposition.
type ChildSpec struct {
	Title  string
	Body   string
	Labels []string
}

// IsMissionIssue returns true if the issue has any of the configured mission labels.
func IsMissionIssue(issue github.Issue, missionLabels []string) bool {
	return github.HasLabel(issue, missionLabels)
}

// ParseChildSpecs extracts child issue specifications from a mission issue body.
// It looks for a "## Tasks" or "## Child Issues" section with a markdown task list.
// Each task item becomes a child issue. Lines starting with "- [ ]" are parsed.
//
// Format:
//
//	## Tasks
//	- [ ] Title of first task
//	  Description of first task
//	- [ ] Title of second task (depends on #1)
//	  Description of second task
func ParseChildSpecs(body string) []ChildSpec {
	lines := strings.Split(body, "\n")
	var specs []ChildSpec
	inTaskSection := false
	var current *ChildSpec

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Detect task section header
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "## tasks") || strings.HasPrefix(lower, "## child issues") || strings.HasPrefix(lower, "## subtasks") {
			inTaskSection = true
			continue
		}

		// End of task section on next heading
		if inTaskSection && strings.HasPrefix(trimmed, "## ") {
			inTaskSection = false
			continue
		}

		if !inTaskSection {
			continue
		}

		// Parse task list items: "- [ ] Title"
		if strings.HasPrefix(trimmed, "- [ ] ") || strings.HasPrefix(trimmed, "- [x] ") {
			// Save previous spec
			if current != nil {
				specs = append(specs, *current)
			}
			title := trimmed[6:] // strip "- [ ] " or "- [x] "
			current = &ChildSpec{
				Title: strings.TrimSpace(title),
			}
			continue
		}

		// Continuation lines become body of current spec
		if current != nil && trimmed != "" {
			if current.Body != "" {
				current.Body += "\n"
			}
			current.Body += trimmed
		}
	}

	// Don't forget the last spec
	if current != nil {
		specs = append(specs, *current)
	}

	return specs
}

// DecomposeMission creates child issues for a mission and registers the mission in state.
// If the mission is already in decomposing state (partial failure recovery), it resumes
// creating only the remaining children. Returns the full list of child issue numbers.
func (d *Decomposer) DecomposeMission(s *state.State, issue github.Issue) ([]int, error) {
	specs := ParseChildSpecs(issue.Body)
	if len(specs) == 0 {
		return nil, fmt.Errorf("mission issue #%d has no parseable tasks in body", issue.Number)
	}

	// Enforce max children
	if len(specs) > d.cfg.Missions.MaxChildren {
		log.Printf("[mission] issue #%d has %d tasks, capping at %d",
			issue.Number, len(specs), d.cfg.Missions.MaxChildren)
		specs = specs[:d.cfg.Missions.MaxChildren]
	}

	// Check for resume: if mission already exists in decomposing state, pick up where we left off
	var childNumbers []int
	startIdx := 0
	if existing, ok := s.Missions[issue.Number]; ok && existing.Status == state.MissionStatusDecomposing {
		childNumbers = existing.ChildIssues
		startIdx = len(childNumbers)
		if startIdx >= len(specs) {
			// All children already created; advance to active
			existing.Status = state.MissionStatusActive
			return childNumbers, nil
		}
		log.Printf("[mission] resuming decomposition for #%d from child %d/%d",
			issue.Number, startIdx+1, len(specs))
	}

	// Collect labels from parent (excluding mission labels themselves)
	var parentLabels []string
	for _, l := range issue.Labels {
		isMissionLabel := false
		for _, ml := range d.cfg.Missions.Labels {
			if strings.EqualFold(l.Name, ml) {
				isMissionLabel = true
				break
			}
		}
		if !isMissionLabel {
			parentLabels = append(parentLabels, l.Name)
		}
	}

	// Use parent labels for children. Only add a single config issue label if the
	// parent has none of the configured labels (so children are picked up by the orchestrator).
	childLabels := parentLabels
	if len(d.cfg.IssueLabels) > 0 {
		hasConfigLabel := false
		for _, cl := range childLabels {
			for _, il := range d.cfg.IssueLabels {
				if strings.EqualFold(cl, il) {
					hasConfigLabel = true
					break
				}
			}
			if hasConfigLabel {
				break
			}
		}
		if !hasConfigLabel {
			// Add the first configured label so children appear in orchestrator queries
			childLabels = append(childLabels, d.cfg.IssueLabels[0])
		}
	}

	// Create child issues (starting from startIdx for resume)
	for i := startIdx; i < len(specs); i++ {
		spec := specs[i]
		body := spec.Body

		// Add blocker reference to parent
		if body != "" {
			body += "\n\n"
		}
		body += fmt.Sprintf("Part of mission #%d", issue.Number)

		// Add dependency on previous child if sequential ordering
		if i > 0 {
			body += fmt.Sprintf("\nBlocked by #%d", childNumbers[i-1])
		}

		labels := make([]string, len(childLabels))
		copy(labels, childLabels)
		labels = append(labels, spec.Labels...)

		childNum, err := d.gh.CreateIssue(spec.Title, body, labels)
		if err != nil {
			// Register partial mission to prevent re-decomposition on next poll
			if len(childNumbers) > 0 {
				now := time.Now()
				s.Missions[issue.Number] = &state.Mission{
					ParentIssue: issue.Number,
					ParentTitle: issue.Title,
					ChildIssues: childNumbers,
					Status:      state.MissionStatusDecomposing,
					CreatedAt:   now,
				}
			}
			return childNumbers, fmt.Errorf("create child issue %d/%d for mission #%d: %w",
				i+1, len(specs), issue.Number, err)
		}

		log.Printf("[mission] created child issue #%d: %s (for mission #%d)",
			childNum, spec.Title, issue.Number)
		childNumbers = append(childNumbers, childNum)
	}

	// Register mission in state
	now := time.Now()
	s.Missions[issue.Number] = &state.Mission{
		ParentIssue: issue.Number,
		ParentTitle: issue.Title,
		ChildIssues: childNumbers,
		Status:      state.MissionStatusActive,
		CreatedAt:   now,
	}

	// Update parent issue body with child issue checklist
	if err := d.updateParentChecklist(issue, childNumbers, specs); err != nil {
		log.Printf("[mission] warn: could not update parent issue #%d body: %v", issue.Number, err)
	}

	return childNumbers, nil
}

// updateParentChecklist updates the parent issue body with a checklist of child issues.
func (d *Decomposer) updateParentChecklist(parent github.Issue, childNumbers []int, specs []ChildSpec) error {
	var checklist strings.Builder
	checklist.WriteString("\n\n---\n## Mission Progress\n\n")
	for i, num := range childNumbers {
		title := specs[i].Title
		checklist.WriteString(fmt.Sprintf("- [ ] #%d — %s\n", num, title))
	}

	newBody := parent.Body + checklist.String()
	return d.gh.UpdateIssueBody(parent.Number, newBody)
}

// SyncMissionProgress checks child issue statuses and updates mission state.
// Returns true if the mission is now complete (all children closed), along with
// a map of child issue number to closed status (for use in progress reporting).
func SyncMissionProgress(gh *github.Client, s *state.State, parentNumber int) (bool, map[int]bool, error) {
	m, ok := s.Missions[parentNumber]
	if !ok {
		return false, nil, fmt.Errorf("no mission found for parent #%d", parentNumber)
	}

	if m.Status == state.MissionStatusDone {
		return true, nil, nil
	}

	if len(m.ChildIssues) == 0 {
		return false, nil, fmt.Errorf("mission #%d has no child issues", parentNumber)
	}

	allClosed := true
	childStatuses := make(map[int]bool, len(m.ChildIssues))
	for _, childNum := range m.ChildIssues {
		closed, err := gh.IsIssueClosed(childNum)
		if err != nil {
			log.Printf("[mission] warn: could not check child #%d status: %v", childNum, err)
			allClosed = false
			continue
		}
		childStatuses[childNum] = closed
		if !closed {
			allClosed = false
		}
	}

	if allClosed {
		now := time.Now()
		m.Status = state.MissionStatusDone
		m.CompletedAt = &now
		log.Printf("[mission] mission #%d complete — all %d children closed", parentNumber, len(m.ChildIssues))
		return true, childStatuses, nil
	}

	return false, childStatuses, nil
}

// BuildProgressComment builds a summary comment for the parent issue.
// childStatuses maps child issue numbers to their closed status. If nil,
// all children are assumed closed (used on mission completion).
func BuildProgressComment(m *state.Mission, childStatuses map[int]bool) string {
	var sb strings.Builder
	sb.WriteString("## Mission Progress Update\n\n")

	closed := 0
	for _, childNum := range m.ChildIssues {
		isClosed, known := childStatuses[childNum]
		if !known {
			sb.WriteString(fmt.Sprintf("- ❓ #%d — status unknown\n", childNum))
			continue
		}
		if isClosed {
			sb.WriteString(fmt.Sprintf("- ✅ #%d — done\n", childNum))
			closed++
		} else {
			sb.WriteString(fmt.Sprintf("- ⏳ #%d — in progress\n", childNum))
		}
	}

	sb.WriteString(fmt.Sprintf("\n**Progress: %d/%d complete**\n", closed, len(m.ChildIssues)))
	return sb.String()
}
