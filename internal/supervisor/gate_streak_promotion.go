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
