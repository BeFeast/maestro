package worker

import (
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
)

func TestAssemblePromptIncludesSecretSafetyGuardrails(t *testing.T) {
	cfg := &config.Config{Repo: "BeFeast/ok-gobot"}
	issue := github.Issue{
		Number: 157,
		Title:  "security hardening",
		Body:   "Fix secret handling.",
	}

	prompt := assemblePrompt("base prompt", issue, "/tmp/worktree", "codex/security", cfg)

	required := []string{
		"Do NOT commit or mention API keys",
		"Do NOT commit temp/debug artifacts such as tmp/, _tmp/, *.log, *.logs, *.test, or *.test.json",
		"Do NOT paste logs, doctor output, env dumps, or secret-bearing snippets into the PR body or comments.",
		`gh pr create --repo BeFeast/ok-gobot --title "security hardening" --body "Closes #157"`,
	}
	for _, want := range required {
		if !strings.Contains(prompt, want) {
			t.Fatalf("assemblePrompt() missing %q\nprompt:\n%s", want, prompt)
		}
	}
}
