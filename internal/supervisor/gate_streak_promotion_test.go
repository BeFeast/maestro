package supervisor

import (
	"testing"

	"github.com/befeast/maestro/internal/github"
)

func TestIsGateFailStreakIssue(t *testing.T) {
	minted := github.Issue{
		Number: 277,
		Title:  "gate failure streak: healthcheck_command failing 2 consecutive scheduled runs",
		Body:   "<!-- maestro:gate-fail-streak 658f0089bedb2248 -->\n\nScheduled gate `healthcheck_command` failed 2 consecutive runs.",
	}
	if !isGateFailStreakIssue(minted) {
		t.Fatal("a Maestro-minted streak issue was not recognised by its marker")
	}

	human := github.Issue{
		Number: 628,
		Title:  "P0 Linux: screenshot in fullscreen leaves stale oversized native video",
		Body:   "## Summary\nThe fullscreen screenshot path leaves the native surface at the wrong size.",
	}
	if isGateFailStreakIssue(human) {
		t.Fatal("a human-written issue must not be mistaken for a minted streak report")
	}

	// An issue that merely mentions the phrase in prose is not a minted report.
	prose := github.Issue{
		Number: 900,
		Body:   "We keep seeing a gate failure streak on Linux; see the maestro docs.",
	}
	if isGateFailStreakIssue(prose) {
		t.Fatal("prose mentioning the phrase must not match — only the hidden marker counts")
	}
}
