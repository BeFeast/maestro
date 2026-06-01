package supervisor

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// ActionUnblockIssue is the supervisor recommended action for the
// dependency-unblock controller: every configured dependency has closed (or
// its linked PR is merged), so the supervisor removes the blocked label and
// adds the ready label for the next wave. See issue #442.
const ActionUnblockIssue = "unblock_issue"

// PolicyRuleDependencyUnblock identifies the supervisor policy rule that
// generated a dependency-unblock decision in audit/state.
const PolicyRuleDependencyUnblock = "supervisor.dependency_unblock"

// ProjectEnroller adds a blocked wave member to the configured GitHub
// Project board so the operator can see the upcoming wave even before the
// supervisor unblocks it. Implementations are best-effort: errors are
// logged, not surfaced as decision failures, because enrollment must not
// block the supervisor cycle.
type ProjectEnroller interface {
	EnrollBlockedIssue(issueNumber int) error
}

// enrollBlockedWaveMembers walks every blocked wave member and asks the
// configured enroller to add it to the GitHub Project. Returns the issues
// that were attempted (regardless of success) so the decision Reasons can
// list them. Safe to call with a nil enroller; this is just a no-op then.
func (e *Engine) enrollBlockedWaveMembers(blocked []github.Issue) []int {
	cfg := e.cfg
	if cfg == nil || !cfg.GitHubProjects.Enabled {
		return nil
	}
	unblock := cfg.Supervisor.DynamicWave.DependencyUnblock
	if !unblock.Active() || !unblock.EnrollInProjectEnabled() {
		return nil
	}
	if e.enroller == nil {
		return nil
	}
	enrolled := make([]int, 0, len(blocked))
	for _, issue := range blocked {
		if err := e.enroller.EnrollBlockedIssue(issue.Number); err != nil {
			// Best-effort: keep the supervisor cycle moving. The
			// fake reader in tests can capture this through its own
			// counter; production wiring logs via the gh client.
			continue
		}
		enrolled = append(enrolled, issue.Number)
	}
	sort.Ints(enrolled)
	return enrolled
}

// evaluateDependencyUnblock walks every open issue, finds those carrying the
// configured blocked label, parses their `Depends on: #N` / `## Dependencies`
// references, and recommends a label/comment mutation set when all
// dependencies are resolved (closed or PR-merged). Returns nil when the
// controller is inactive, when there are no blocked wave members, or when no
// member is ready to unblock (e.g. still has open deps, capacity exhausted,
// or another supervisor decision already ran the same mutation this cycle).
func (e *Engine) evaluateDependencyUnblock(st *state.State, issues []github.Issue, baseReasons []string, projectState state.SupervisorProjectState, now time.Time, cache *resolutionCache) *state.SupervisorDecision {
	if e == nil || e.cfg == nil {
		return nil
	}
	cfg := e.cfg
	unblockCfg := cfg.Supervisor.DynamicWave.DependencyUnblock
	if !unblockCfg.Active() {
		return nil
	}
	blockedLabel := e.blockedLabel()
	readyLabel := e.readyLabel()
	if blockedLabel == "" || readyLabel == "" {
		return nil
	}

	// Respect max_runnable: we count current ready-labeled open issues so
	// that supervising the wave never grows the runnable pool past the
	// operator cap. Ordered/dynamic wave concurrency lives elsewhere; this
	// cap is specific to the dependency-unblock controller.
	if cap := unblockCfg.MaxRunnable; cap > 0 {
		if currentlyReady := countOpenIssuesWithLabel(issues, readyLabel); currentlyReady >= cap {
			return nil
		}
	}

	candidates := blockedWaveMembers(issues, blockedLabel)
	if len(candidates) == 0 {
		return nil
	}

	// Project enrollment side-effect: surface blocked wave members on the
	// configured GitHub Project so operators see the upcoming wave even
	// before any one item is unblocked. Errors are best-effort.
	enrolled := e.enrollBlockedWaveMembers(candidates)
	if len(enrolled) > 0 {
		baseReasons = appendReasons(baseReasons,
			fmt.Sprintf("Enrolled %d blocked wave member(s) onto GitHub Project: %s", len(enrolled), dependencyRefs(enrolled)),
		)
	}

	for _, issue := range candidates {
		deps := github.FindDependencies(issue.Body)
		if len(deps) == 0 {
			// A blocked issue without parseable dependencies is operator-
			// gated: we don't guess when it's safe to unblock. Skip silently
			// so existing handoff items without the format don't appear as
			// supervisor work.
			continue
		}
		resolved, unresolved, evidence, err := e.resolveDependencies(deps, cache)
		if err != nil {
			// Read errors should not crash the whole supervisor cycle —
			// just skip this candidate this cycle.
			continue
		}
		if len(unresolved) > 0 {
			continue
		}

		// Idempotency: do not re-emit if a previous decision already
		// removed the blocked label or added the ready label for this
		// exact issue/label pair. Prevents duplicate comments/labels.
		removeBlocked := github.HasLabel(issue, []string{blockedLabel}) &&
			!supervisorMutationSucceeded(st, issue.Number, MutationRemoveBlockedLabel, blockedLabel)
		addReady := !github.HasLabel(issue, []string{readyLabel}) &&
			!supervisorMutationSucceeded(st, issue.Number, MutationAddReadyLabel, readyLabel)
		if !removeBlocked && !addReady {
			continue
		}

		// Policy gate: the operator must explicitly allow the mutations we
		// plan to execute. Without the safe-action grant we still surface
		// the recommendation but mark it as mutating (approval-gated).
		mutations := plannedDependencyUnblockMutations(cfg, issue.Number, readyLabel, blockedLabel, removeBlocked, addReady)
		risk := RiskMutating
		if len(mutations) > 0 && allMutationsAllowed(cfg, mutations) {
			risk = RiskSafe
		}

		// Append the comment plan (dependency evidence) when the operator
		// asked for it AND the comment safe-action is granted. Without the
		// grant, the label mutations still go through and the audit trail
		// lives in supervisor state instead of GitHub.
		if unblockCfg.AnnounceWithCommentEnabled() && safeActionAllowed(cfg, config.SupervisorActionAddIssueComment) && risk == RiskSafe {
			mutations = append(mutations, state.SupervisorMutation{
				Type:   MutationIssueComment,
				Issue:  issue.Number,
				Body:   dependencyUnblockComment(issue, resolved, evidence),
				Status: MutationStatusPlanned,
			})
		}

		reasons := appendReasons(baseReasons,
			fmt.Sprintf("Issue #%d is the next blocked wave member with all dependencies resolved", issue.Number),
			fmt.Sprintf("Dependency evidence: %s", strings.Join(evidence, "; ")),
			fmt.Sprintf("Plan: remove `%s`, add `%s`", blockedLabel, readyLabel),
		)
		summary := fmt.Sprintf("Unblock issue #%d: all dependencies (%s) are complete; add `%s` and remove `%s`.", issue.Number, dependencyRefs(deps), readyLabel, blockedLabel)
		decision := e.decision(st, now, projectState, ActionUnblockIssue, summary, risk, 0.9,
			&state.SupervisorTarget{Issue: issue.Number}, PolicyRuleDependencyUnblock, reasons)
		decision.Mutations = mutations
		return &decision
	}

	return nil
}

// resolveDependencies returns the dependency issues that are closed or whose
// linked PR is merged (resolved) and those that are still open (unresolved).
// Evidence is a human-readable list of "#N closed" / "#N PR merged" lines
// used in the unblock comment.
func (e *Engine) resolveDependencies(deps []int, cache *resolutionCache) (resolved []int, unresolved []int, evidence []string, err error) {
	if cache == nil {
		cache = newResolutionCache(e.reader)
	}
	for _, dep := range deps {
		closed, lookupErr := e.reader.IsIssueClosed(dep)
		if lookupErr != nil {
			return nil, nil, nil, fmt.Errorf("check dependency #%d closed: %w", dep, lookupErr)
		}
		if closed {
			resolved = append(resolved, dep)
			evidence = append(evidence, fmt.Sprintf("#%d closed", dep))
			continue
		}
		merged, lookupErr := e.reader.HasMergedPRForIssue(dep)
		if lookupErr != nil {
			return nil, nil, nil, fmt.Errorf("check dependency #%d merged PR: %w", dep, lookupErr)
		}
		if merged {
			resolved = append(resolved, dep)
			evidence = append(evidence, fmt.Sprintf("#%d PR merged", dep))
			continue
		}
		unresolved = append(unresolved, dep)
		evidence = append(evidence, fmt.Sprintf("#%d still open", dep))
	}
	return resolved, unresolved, evidence, nil
}

func blockedWaveMembers(issues []github.Issue, blockedLabel string) []github.Issue {
	if strings.TrimSpace(blockedLabel) == "" {
		return nil
	}
	out := make([]github.Issue, 0)
	for _, issue := range issues {
		if github.HasLabel(issue, []string{blockedLabel}) {
			out = append(out, issue)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

func countOpenIssuesWithLabel(issues []github.Issue, label string) int {
	if strings.TrimSpace(label) == "" {
		return 0
	}
	count := 0
	for _, issue := range issues {
		if github.HasLabel(issue, []string{label}) {
			count++
		}
	}
	return count
}

func plannedDependencyUnblockMutations(cfg *config.Config, issueNumber int, readyLabel, blockedLabel string, removeBlocked, addReady bool) []state.SupervisorMutation {
	var mutations []state.SupervisorMutation
	if removeBlocked {
		mutations = append(mutations, state.SupervisorMutation{
			Type:   MutationRemoveBlockedLabel,
			Issue:  issueNumber,
			Label:  blockedLabel,
			Status: MutationStatusPlanned,
		})
	}
	if addReady {
		mutations = append(mutations, state.SupervisorMutation{
			Type:   MutationAddReadyLabel,
			Issue:  issueNumber,
			Label:  readyLabel,
			Status: MutationStatusPlanned,
		})
	}
	return mutations
}

func allMutationsAllowed(cfg *config.Config, mutations []state.SupervisorMutation) bool {
	for _, m := range mutations {
		if !safeActionAllowed(cfg, m.Type) {
			return false
		}
	}
	return true
}

func dependencyUnblockComment(issue github.Issue, resolved []int, evidence []string) string {
	var b strings.Builder
	b.WriteString("Maestro supervisor unblocked this issue: all configured dependencies are resolved.\n\n")
	b.WriteString("Dependency evidence:\n")
	for _, line := range evidence {
		b.WriteString("- ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	if len(resolved) > 0 {
		b.WriteString(fmt.Sprintf("\nResolved dependencies: %s\n", dependencyRefs(resolved)))
	}
	return strings.TrimSpace(b.String())
}

func dependencyRefs(deps []int) string {
	if len(deps) == 0 {
		return ""
	}
	refs := make([]string, len(deps))
	for i, n := range deps {
		refs[i] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(refs, ", ")
}
