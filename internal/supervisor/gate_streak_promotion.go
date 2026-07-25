package supervisor

import (
	"strings"

	"github.com/befeast/maestro/internal/github"
)

// gateFailStreakBodyMarker is the hidden marker Maestro writes into every
// gate-fail-streak issue it mints (see gateFailStreakIssueBody). It is the only
// reliable way to tell one of Maestro's own reports from a human-written issue:
// the issues carry no distinguishing label, because they are created unlabelled.
const gateFailStreakBodyMarker = "<!-- maestro:gate-fail-streak"

// isGateFailStreakIssue reports whether the issue is one Maestro minted from a
// scheduled-gate failure streak.
func isGateFailStreakIssue(issue github.Issue) bool {
	return strings.Contains(issue.Body, gateFailStreakBodyMarker)
}

// operatorTriagedGateStreak reports whether an operator has explicitly admitted
// a minted streak report into the queue by labelling it.
//
// This deliberately does NOT reuse matchesRequiredLabels: with no issue_labels
// and no supervisor.ready_label configured — a supported setup where every open
// issue is eligible — that predicate is vacuously true, so a freshly minted
// report would count as triaged the moment it appeared and the wave would spawn
// against it. In that configuration there is no label for an operator to apply,
// so a streak report is never auto-eligible; the operator closes it, or converts
// it into a real issue.
func (e *Engine) operatorTriagedGateStreak(issue github.Issue) bool {
	required := e.requiredIssueLabels()
	if len(required) == 0 {
		return false
	}
	return matchesRequiredLabels(issue, required)
}
