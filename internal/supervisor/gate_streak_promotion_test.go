package supervisor

import (
	"testing"

	"github.com/befeast/maestro/internal/config"
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

// Codex review catch (P1): with neither issue_labels nor supervisor.ready_label
// configured — a supported setup where every open issue is eligible —
// matchesRequiredLabels is vacuously true, so a guard built on it would treat a
// freshly minted report as operator-triaged and spawn against it anyway.
func TestOperatorTriagedGateStreak_NoLabelsConfiguredNeverAutoEligible(t *testing.T) {
	e := &Engine{cfg: &config.Config{}} // no issue_labels, no ready_label
	minted := github.Issue{Number: 277, Body: gateFailStreakBodyMarker + " abc -->"}

	if e.operatorTriagedGateStreak(minted) {
		t.Fatal("with no labels configured a minted report must never count as triaged")
	}
}

// With a ready label configured, the operator applying it admits the report.
func TestOperatorTriagedGateStreak_ReadyLabelAdmitsReport(t *testing.T) {
	cfg := &config.Config{}
	cfg.Supervisor.ReadyLabel = "maestro-ready"
	e := &Engine{cfg: cfg}

	unlabelled := github.Issue{Number: 277, Body: gateFailStreakBodyMarker + " abc -->"}
	if e.operatorTriagedGateStreak(unlabelled) {
		t.Fatal("an unlabelled report must not be treated as triaged")
	}

	labelled := github.Issue{
		Number: 277,
		Body:   gateFailStreakBodyMarker + " abc -->",
	}
	labelled.Labels = append(labelled.Labels, struct {
		Name string `json:"name"`
	}{Name: "maestro-ready"})
	if !e.operatorTriagedGateStreak(labelled) {
		t.Fatal("an operator-labelled report must run normally")
	}
}
