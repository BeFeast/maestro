package worker

import (
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// #959: the numeric slot suffix (a monotonic slot counter) is unrelated to the
// canonical GitHub issue number. assertCanonicalIssue must fail closed when a
// caller hands the launch path an issue whose number was derived from the slot
// name instead of the session's persisted issue_number.
func TestAssertCanonicalIssue_SlotSuffixDiffersFromCanonical(t *testing.T) {
	// slot "ok-player-273", canonical issue #345 — the incident shape.
	sess := &state.Session{IssueNumber: 345}

	if err := assertCanonicalIssue("ok-player-273", sess, github.Issue{Number: 345}); err != nil {
		t.Fatalf("canonical issue #345 must launch: %v", err)
	}

	err := assertCanonicalIssue("ok-player-273", sess, github.Issue{Number: 273})
	if err == nil {
		t.Fatal("issue #273 derived from the slot suffix must be refused")
	}
	if !strings.Contains(err.Error(), "345") || !strings.Contains(err.Error(), "273") {
		t.Fatalf("error must name both the rendered and canonical issue: %q", err)
	}
}

func TestAssertCanonicalIssue_MissingRecordFailsClosed(t *testing.T) {
	if err := assertCanonicalIssue("slot-1", nil, github.Issue{Number: 5}); err == nil {
		t.Fatal("nil session must fail closed")
	}
	if err := assertCanonicalIssue("slot-1", &state.Session{IssueNumber: 0}, github.Issue{Number: 5}); err == nil {
		t.Fatal("unset canonical issue must fail closed")
	}
}

// Acceptance: respawning a fixture session named `project-273` with
// issue_number=345 produces a prompt for issue #345 only; no command or prompt
// references issue #273.
func TestRespawnPrompt_SourcesCanonicalIssueNotSlotSuffix(t *testing.T) {
	cfg := &config.Config{Repo: "BeFeast/ok-player"}
	// The canonical issue the session record points at — NOT the slot suffix.
	issue := github.Issue{
		Number: 345,
		Title:  "flatpak beta retry",
		Body:   "Retry the flatpak beta publish.",
	}

	prompt := assemblePromptWithCheckpoint(
		"Before editing, run `gh issue view {{ISSUE_NUMBER}} --repo {{REPO}}` and read {{ISSUE_TITLE}}.\n\n{{ISSUE_BODY}}",
		issue,
		"/tmp/worktrees/project-273",
		"feat/ok-player-345-flatpak-beta-retry",
		cfg,
		"checkpoint context",
		"CHECKPOINT.md",
	)
	prompt = withCanonicalIssueBinding(prompt, cfg.Repo, issue)

	for _, want := range []string{
		"<!-- maestro:canonical-issue-number=345 -->",
		"Use issue **#345** for every GitHub issue lookup",
		"Runtime identifier suffixes are not issue numbers. Continue only issue **#345**.",
		"gh issue view 345 --repo BeFeast/ok-player",
		"flatpak beta retry",
		"Retry the flatpak beta publish.",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt must contain canonical issue content %q\nprompt:\n%s", want, prompt)
		}
	}
	if err := assertCanonicalPrompt("project-273", 345, prompt); err != nil {
		t.Fatalf("canonical prompt validation failed: %v", err)
	}
	// The slot suffix 273 must never appear as an issue reference or a
	// `gh issue view 273` command.
	for _, forbidden := range []string{"#273", "issue 273", "view 273", "issue #273"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("prompt must not reference slot suffix via %q\nprompt:\n%s", forbidden, prompt)
		}
	}
}

func TestAssertCanonicalPromptRejectsDifferentRenderedIssue(t *testing.T) {
	prompt := withCanonicalIssueBinding("base", "BeFeast/ok-player", github.Issue{Number: 273, Title: "wrong"})
	if err := assertCanonicalPrompt("project-273", 345, prompt); err == nil {
		t.Fatal("prompt rendered for slot suffix issue #273 must not launch canonical issue #345")
	}
}

func TestAssertCanonicalPromptRejectsSlotSuffixIssueCommandFromCheckpoint(t *testing.T) {
	prompt := withCanonicalIssueBinding(
		"Previous output: I'll inspect the issue now.\n`gh issue view 273 --repo BeFeast/ok-player`",
		"BeFeast/ok-player",
		github.Issue{Number: 345, Title: "flatpak beta retry"},
	)
	if err := assertCanonicalPrompt("project-273", 345, prompt); err == nil {
		t.Fatal("slot-suffix issue command carried in checkpoint context must fail closed")
	}
}
