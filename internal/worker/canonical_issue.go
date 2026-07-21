package worker

import (
	"fmt"
	"strings"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

const canonicalIssueMarkerFormat = "<!-- maestro:canonical-issue-number=%d -->"

const canonicalIssueReminderFormat = "Runtime identifier suffixes are not issue numbers. Continue only issue **#%d**."

// assertCanonicalIssue fails closed when the issue about to be rendered into a
// worker prompt does not match the session's persisted canonical issue_number.
//
// #959: an in-place respawn for an open-PR session composed the worker task from
// the numeric slot suffix (e.g. slot "ok-player-273") instead of the session's
// canonical issue_number (345), so every respawn ran `gh issue view 273` and
// burned tokens on the wrong issue. The slot suffix is a monotonic slot counter
// unrelated to the GitHub issue number; the two diverge whenever the slot index
// and the issue number differ. Every launch/respawn path already sources the
// issue via getIssue(sess.IssueNumber), so a mismatch here means a caller
// derived the number from the slot name — refuse to launch (and preserve the
// existing worktree/PR) rather than prompt a worker for the wrong issue.
func assertCanonicalIssue(slotName string, sess *state.Session, issue github.Issue) error {
	if sess == nil {
		return fmt.Errorf("refusing to launch worker %s: session record is missing", slotName)
	}
	if sess.IssueNumber <= 0 {
		return fmt.Errorf("refusing to launch worker %s: session canonical issue_number is invalid (%d)", slotName, sess.IssueNumber)
	}
	if issue.Number <= 0 {
		return fmt.Errorf("refusing to launch worker %s: rendered issue number is invalid (%d)", slotName, issue.Number)
	}
	if issue.Number != sess.IssueNumber {
		return fmt.Errorf(
			"refusing to launch worker %s: rendered issue #%d does not match session canonical issue #%d",
			slotName, issue.Number, sess.IssueNumber,
		)
	}
	return nil
}

// withCanonicalIssueBinding puts the persisted GitHub identity ahead of every
// other prompt section. Slot, worktree, and tmux names contain a monotonic
// numeric suffix that is unrelated to the GitHub issue number; spelling that
// out at the top prevents an agent from treating the runtime suffix as task
// identity when it decides which `gh issue` command to run.
func withCanonicalIssueBinding(prompt, repo string, issue github.Issue) string {
	repo = strings.TrimSpace(repo)
	identity := fmt.Sprintf("issue #%d", issue.Number)
	if repo != "" {
		identity = fmt.Sprintf("%s#%d", repo, issue.Number)
	}
	title := strings.TrimSpace(issue.Title)
	if title != "" {
		identity += ": " + title
	}

	return fmt.Sprintf(`%s
## Canonical GitHub Issue Binding

This worker launch is bound to **%s**. Use issue **#%d** for every GitHub issue lookup, issue command, issue comment, and PR-body issue reference.

Session, slot, branch, worktree, log, and tmux names are runtime identifiers. Their numeric suffixes are never GitHub issue numbers; do not derive an issue number from them.

---

%s

---

## Canonical Issue Reminder

%s`,
		fmt.Sprintf(canonicalIssueMarkerFormat, issue.Number),
		identity,
		issue.Number,
		prompt,
		fmt.Sprintf(canonicalIssueReminderFormat, issue.Number),
	)
}

// assertCanonicalPrompt verifies the final rendered prompt retained the exact
// canonical issue binding before any worker process is launched.
func assertCanonicalPrompt(slotName string, expectedIssue int, prompt string) error {
	if expectedIssue <= 0 {
		return fmt.Errorf("refusing to launch worker %s: expected canonical issue number is invalid (%d)", slotName, expectedIssue)
	}
	marker := fmt.Sprintf(canonicalIssueMarkerFormat, expectedIssue)
	if !strings.HasPrefix(prompt, marker+"\n") {
		return fmt.Errorf("refusing to launch worker %s: rendered prompt does not match canonical issue #%d", slotName, expectedIssue)
	}
	reminder := fmt.Sprintf(canonicalIssueReminderFormat, expectedIssue)
	if !strings.HasSuffix(prompt, reminder) {
		return fmt.Errorf("refusing to launch worker %s: rendered prompt lost its final canonical issue #%d reminder", slotName, expectedIssue)
	}
	// Continuation/checkpoint text is carried forward from a previous worker
	// and can contain the exact bad command that caused #959. Reject that
	// command when it targets the runtime slot suffix instead of the canonical
	// issue, even though the surrounding prompt has a correct binding.
	slotSuffix := parseSlotNumber(slotName)
	if slotSuffix > 0 && slotSuffix != expectedIssue {
		wrongView := fmt.Sprintf("gh issue view %d", slotSuffix)
		if strings.Contains(strings.ToLower(prompt), wrongView) {
			return fmt.Errorf("refusing to launch worker %s: rendered prompt contains a slot-suffix issue command instead of canonical issue #%d", slotName, expectedIssue)
		}
	}
	return nil
}
