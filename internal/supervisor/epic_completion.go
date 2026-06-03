package supervisor

import (
	"fmt"
	"sort"
	"strings"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
)

// ActionCloseEpic is the supervisor decision tag for the epic-completion
// branch. The underlying executor verb is config.SupervisorActionCloseIssue;
// ActionCloseEpic exists so the journal / dashboard can distinguish "epic
// complete" from a routine done-issue close when filtering decisions.
const ActionCloseEpic = "close_epic"

// PolicyRuleEpicCompletion identifies decisions emitted by the epic-
// completion aggregate in audit/state.
const PolicyRuleEpicCompletion = "supervisor.epic_completion"

// computeEpicProgress builds the aggregate for every open issue that
// looks like a handoff/epic parent. childResolver receives a child issue
// number and returns (merged, closed) verdicts; supervisor wires this to
// the resolution cache so each underlying lookup happens at most once
// per cycle. outcomeStatus controls the Complete=true gate.
//
// Returns a stable, number-sorted slice. Returns nil when no candidate
// epics exist OR no candidate epic has any parseable children — a
// label-only "epic" with an empty body should not surface as a fake
// "0/0 complete" entry.
func computeEpicProgress(epics []github.Issue, childResolver func(int) (merged, closed bool), outcomeStatus outcome.Status) []state.EpicProgress {
	if len(epics) == 0 || childResolver == nil {
		return nil
	}
	sorted := append([]github.Issue(nil), epics...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })

	out := make([]state.EpicProgress, 0, len(sorted))
	for _, epic := range sorted {
		children := github.FindChildIssuesExcluding(epic.Body, epic.Number)
		if len(children) == 0 {
			continue
		}
		progress := state.EpicProgress{
			Number:        epic.Number,
			Title:         strings.TrimSpace(epic.Title),
			Children:      children,
			TotalChildren: len(children),
			OutcomeHealth: outcomeStatus.HealthState,
		}
		for _, child := range children {
			merged, closed := childResolver(child)
			if merged || closed {
				progress.MergedChildren = append(progress.MergedChildren, child)
				progress.MergedCount++
			} else {
				progress.OpenChildren = append(progress.OpenChildren, child)
				progress.OpenCount++
			}
		}
		progress.AllChildrenDone = progress.OpenCount == 0 && progress.TotalChildren > 0
		progress.OutcomeHealthy = outcomeStatus.HealthState == outcome.HealthHealthy
		progress.Complete = progress.AllChildrenDone && progress.OutcomeHealthy
		progress.Summary = epicSummary(progress)
		progress.Evidence = epicEvidence(progress)
		out = append(out, progress)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func epicSummary(p state.EpicProgress) string {
	switch {
	case p.Complete:
		return fmt.Sprintf("Epic #%d is complete: %d/%d children merged AND outcome health is healthy.", p.Number, p.MergedCount, p.TotalChildren)
	case p.AllChildrenDone && !p.OutcomeHealthy:
		return fmt.Sprintf("Epic #%d: all %d children merged, but outcome health is %s — verify runtime before close.", p.Number, p.TotalChildren, healthLabel(p.OutcomeHealth))
	case p.MergedCount == 0:
		return fmt.Sprintf("Epic #%d: 0/%d children merged.", p.Number, p.TotalChildren)
	default:
		return fmt.Sprintf("Epic #%d: %d/%d children merged, %d still open.", p.Number, p.MergedCount, p.TotalChildren, p.OpenCount)
	}
}

func epicEvidence(p state.EpicProgress) []string {
	if p.TotalChildren == 0 {
		return nil
	}
	evidence := []string{
		fmt.Sprintf("children_total=%d", p.TotalChildren),
		fmt.Sprintf("children_merged=%d", p.MergedCount),
		fmt.Sprintf("children_open=%d", p.OpenCount),
		fmt.Sprintf("outcome_health=%s", healthLabel(p.OutcomeHealth)),
	}
	if len(p.MergedChildren) > 0 {
		evidence = append(evidence, fmt.Sprintf("merged=%s", joinIssueNums(p.MergedChildren)))
	}
	if len(p.OpenChildren) > 0 {
		evidence = append(evidence, fmt.Sprintf("open=%s", joinIssueNums(p.OpenChildren)))
	}
	return evidence
}

func joinIssueNums(nums []int) string {
	parts := make([]string, 0, len(nums))
	for _, n := range nums {
		parts = append(parts, fmt.Sprintf("#%d", n))
	}
	return strings.Join(parts, ",")
}

func healthLabel(state string) string {
	state = strings.TrimSpace(state)
	if state == "" {
		return "unknown"
	}
	return state
}

// openEpicCandidates returns the open issues that look like handoff/epic
// parents under the configured handoff_planner labels OR an `epic:` title
// prefix. Used by both the fleet snapshot and the close-epic decision
// branch. Order is preserved from the caller-provided issue list; callers
// that need stable ordering should sort themselves.
func (e *Engine) openEpicCandidates(issues []github.Issue) []github.Issue {
	if e == nil || e.cfg == nil {
		return nil
	}
	labels := e.cfg.Supervisor.HandoffPlanner.EffectiveSourceLabels()
	out := make([]github.Issue, 0)
	for _, issue := range issues {
		if isHandoffSource(issue, labels) {
			out = append(out, issue)
		}
	}
	return out
}

// epicProgressForIssues returns the EpicProgress aggregate for every open
// epic in issues that has parseable children. Reads through the supplied
// resolution cache so each child lookup happens at most once per cycle.
func (e *Engine) epicProgressForIssues(issues []github.Issue, cache *resolutionCache, outcomeStatus outcome.Status) []state.EpicProgress {
	epics := e.openEpicCandidates(issues)
	if len(epics) == 0 {
		return nil
	}
	if cache == nil {
		cache = newResolutionCache(e.reader)
	}
	resolver := func(child int) (bool, bool) {
		merged := cache.hasMergedPRForIssue(child)
		closed := cache.isIssueClosed(child)
		return merged, closed
	}
	return computeEpicProgress(epics, resolver, outcomeStatus)
}

// firstCompletedEpic returns the lowest-numbered epic in progresses whose
// children are all merged AND the configured outcome health is healthy.
// Used by the supervisor to mint a single approval-gated close_epic
// recommendation per cycle. Returns nil when no epic is fully complete.
func firstCompletedEpic(progresses []state.EpicProgress) *state.EpicProgress {
	for i := range progresses {
		p := progresses[i]
		if p.Complete {
			return &p
		}
	}
	return nil
}
