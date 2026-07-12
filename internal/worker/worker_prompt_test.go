package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

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
		"Never use closing keywords such as Closes/Fixes/Resolves in PR bodies for Maestro-managed work.",
		`gh pr create --repo BeFeast/ok-gobot --title "security hardening" --body "Refs #157"`,
	}
	for _, want := range required {
		if !strings.Contains(prompt, want) {
			t.Fatalf("assemblePrompt() missing %q\nprompt:\n%s", want, prompt)
		}
	}
}

func TestAssemblePromptIncludesSearchSafetyGuardrails(t *testing.T) {
	cfg := &config.Config{Repo: "BeFeast/maestro"}
	issue := github.Issue{Number: 319, Title: "worker safety", Body: "Constrain broad searches."}

	prompt := assemblePrompt("base prompt", issue, "/tmp/worktree", "feat/safety", cfg)

	for _, want := range []string{
		"## Worker Search Safety",
		"The assigned worktree is `/tmp/worktree`",
		"Do NOT run `rg`, `find`, or `grep` from broad filesystem roots such as `/`, `/mnt`, or `/home`.",
		"MAESTRO_ALLOW_BROAD_SEARCH=1",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("assemblePrompt() missing %q\nprompt:\n%s", want, prompt)
		}
	}
}

func TestAssemblePromptIncludesWorkDisciplineSection(t *testing.T) {
	cfg := &config.Config{Repo: "BeFeast/maestro"}
	issue := github.Issue{Number: 834, Title: "work discipline", Body: "Sequence the work."}

	want := []string{
		"## Work Discipline",
		"Plan before your first change.",
		"read-only orientation first",
		"a checkpoint, not an essay",
		"Make it durable before you declare done.",
		"A durable partial result beats a perfect plan lost to a dead session.",
		"Surface conflicts, don't silently switch.",
	}

	// Both assemblePrompt branches must carry the section: the template path
	// (base contains {{ISSUE_NUMBER}}) and the legacy append path (it does not).
	for _, tc := range []struct {
		name string
		base string
	}{
		{name: "template", base: "Task {{ISSUE_NUMBER}} in {{REPO}}"},
		{name: "legacy", base: "base prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt := assemblePrompt(tc.base, issue, "/tmp/worktree", "feat/discipline", cfg)
			for _, w := range want {
				if !strings.Contains(prompt, w) {
					t.Fatalf("assemblePrompt() [%s] missing %q\nprompt:\n%s", tc.name, w, prompt)
				}
			}
		})
	}
}

func TestGoWorkerPromptUsesStampedMaestroBuild(t *testing.T) {
	promptPath := filepath.Join("..", "..", "worker-prompt-go.md")
	data, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read %s: %v", promptPath, err)
	}
	prompt := string(data)

	required := []string{
		`VERSION="$(sed -nE 's/^version[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' VERSION)"`,
		`go build -trimpath -ldflags "-X main.version=${VERSION}" ./cmd/maestro/`,
		`./maestro version`,
	}
	for _, want := range required {
		if !strings.Contains(prompt, want) {
			t.Fatalf("worker-prompt-go.md missing %q", want)
		}
	}
	unstampedBuild := "\ngo build " + "./cmd/maestro/"
	if strings.Contains(prompt, unstampedBuild) {
		t.Fatalf("worker-prompt-go.md still contains an unstamped maestro build")
	}
}

func TestAssemblePromptIncludesRepoRulesFromAgentsFile(t *testing.T) {
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "AGENTS.md"), []byte("## Rules\n- Run go test ./...\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Repo: "owner/repo"}
	issue := github.Issue{Number: 642, Title: "repo rules", Body: "body"}

	prompt := assemblePrompt("base prompt", issue, worktree, "feat/rules", cfg)

	for _, want := range []string{
		"## Repository Rules / Conventions",
		"Source: `AGENTS.md` in the target repo worktree.",
		"Run go test ./...",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("assemblePrompt() missing %q\nprompt:\n%s", want, prompt)
		}
	}
}

func TestAssemblePromptOmitsRepoRulesWhenNoConventionFileExists(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo"}
	issue := github.Issue{Number: 642, Title: "repo rules", Body: "body"}

	prompt := assemblePrompt("base prompt", issue, t.TempDir(), "feat/rules", cfg)

	if strings.Contains(prompt, "Repository Rules / Conventions") {
		t.Fatalf("repo rules section should be omitted when no convention file exists\nprompt:\n%s", prompt)
	}
}

func TestAssemblePromptRepoRulesPrecedence(t *testing.T) {
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "CONTRIBUTING.md"), []byte("contributing rules\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "CLAUDE.md"), []byte("claude rules\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "AGENTS.md"), []byte("agents rules\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Repo: "owner/repo"}
	issue := github.Issue{Number: 642, Title: "repo rules", Body: "body"}

	prompt := assemblePrompt("base prompt", issue, worktree, "feat/rules", cfg)

	if !strings.Contains(prompt, "Source: `AGENTS.md`") || !strings.Contains(prompt, "agents rules") {
		t.Fatalf("expected AGENTS.md rules in prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "claude rules") || strings.Contains(prompt, "contributing rules") {
		t.Fatalf("expected only the primary repo rules file in prompt:\n%s", prompt)
	}
}

func TestAssemblePromptRepoRulesFallbackToClaudeThenContributing(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo"}
	issue := github.Issue{Number: 642, Title: "repo rules", Body: "body"}

	t.Run("claude before contributing", func(t *testing.T) {
		worktree := t.TempDir()
		if err := os.WriteFile(filepath.Join(worktree, "CLAUDE.md"), []byte("claude rules\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(worktree, "CONTRIBUTING.md"), []byte("contributing rules\n"), 0644); err != nil {
			t.Fatal(err)
		}

		prompt := assemblePrompt("base prompt", issue, worktree, "feat/rules", cfg)

		if !strings.Contains(prompt, "Source: `CLAUDE.md`") || !strings.Contains(prompt, "claude rules") {
			t.Fatalf("expected CLAUDE.md rules in prompt:\n%s", prompt)
		}
		if strings.Contains(prompt, "contributing rules") {
			t.Fatalf("expected only CLAUDE.md rules in prompt:\n%s", prompt)
		}
	})

	t.Run("contributing when primary files missing", func(t *testing.T) {
		worktree := t.TempDir()
		if err := os.WriteFile(filepath.Join(worktree, "CONTRIBUTING.md"), []byte("contributing rules\n"), 0644); err != nil {
			t.Fatal(err)
		}

		prompt := assemblePrompt("base prompt", issue, worktree, "feat/rules", cfg)

		if !strings.Contains(prompt, "Source: `CONTRIBUTING.md`") || !strings.Contains(prompt, "contributing rules") {
			t.Fatalf("expected CONTRIBUTING.md rules in prompt:\n%s", prompt)
		}
	})
}

func TestAssemblePromptRepoRulesTruncatesOversizedFile(t *testing.T) {
	worktree := t.TempDir()
	content := strings.Repeat("a", repoRulesPromptMaxBytes) + "tail"
	if err := os.WriteFile(filepath.Join(worktree, "AGENTS.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Repo: "owner/repo"}
	issue := github.Issue{Number: 642, Title: "repo rules", Body: "body"}

	prompt := assemblePrompt("base prompt", issue, worktree, "feat/rules", cfg)

	if !strings.Contains(prompt, "Showing the first 32768 bytes; read the file for the rest.") {
		t.Fatalf("expected truncation notice in prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "tail") {
		t.Fatalf("expected oversized content to be truncated")
	}
}

func TestAssemblePromptRepoRulesSanitizesInvalidUTF8(t *testing.T) {
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "AGENTS.md"), []byte{'r', 'u', 'l', 'e', ':', ' ', 0xff}, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Repo: "owner/repo"}
	issue := github.Issue{Number: 642, Title: "repo rules", Body: "body"}

	prompt := assemblePrompt("base prompt", issue, worktree, "feat/rules", cfg)

	if !utf8.ValidString(prompt) {
		t.Fatalf("assembled prompt is not valid UTF-8: %q", prompt)
	}
	if !strings.Contains(prompt, "rule: \uFFFD") {
		t.Fatalf("expected invalid byte to be replaced in repo rules section:\n%q", prompt)
	}
}

func TestAssemblePromptIncludesVisualEvidenceSectionWhenActive(t *testing.T) {
	cfg := &config.Config{
		Repo: "BeFeast/panoptikon",
		Verify: config.VerifyConfig{Visual: config.VerifyVisualConfig{
			Enabled: true,
			Command: "./scripts/capture-screenshots.sh",
			Paths:   []string{"**/*.jsx", "web/**"},
		}},
	}
	issue := github.Issue{Number: 705, Title: "ui change", Body: "body"}

	for name, base := range map[string]string{
		"template": "base prompt {{ISSUE_NUMBER}}",
		"legacy":   "base prompt",
	} {
		t.Run(name, func(t *testing.T) {
			prompt := assemblePrompt(base, issue, t.TempDir(), "feat/ui", cfg)
			for _, want := range []string{
				"## Visual Evidence (UI changes)",
				"`**/*.jsx`, `web/**`",
				"`./scripts/capture-screenshots.sh`",
				"`.maestro/screenshots`",
				"git diff --name-only origin/main...HEAD",
				"do NOT block the PR on it",
			} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("assemblePrompt() missing %q\nprompt:\n%s", want, prompt)
				}
			}
		})
	}
}

func TestAssemblePromptOmitsVisualEvidenceSectionWhenInactive(t *testing.T) {
	for name, cfg := range map[string]*config.Config{
		"disabled": {Repo: "owner/repo"},
		"enabled but unconfigured": {
			Repo:   "owner/repo",
			Verify: config.VerifyConfig{Visual: config.VerifyVisualConfig{Enabled: true}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			issue := github.Issue{Number: 1, Title: "t", Body: "b"}
			prompt := assemblePrompt("base prompt {{ISSUE_NUMBER}}", issue, t.TempDir(), "feat/x", cfg)
			if strings.Contains(prompt, "Visual Evidence (UI changes)") {
				t.Fatalf("visual evidence section should be omitted\nprompt:\n%s", prompt)
			}
		})
	}
}

func TestSubagentHintPromptSectionRendersWhenSet(t *testing.T) {
	hint := "Use cheaper sub-agent models (opus/sonnet) for delegated subtasks; reserve the main model for orchestration and final review."
	section := subagentHintPromptSection(hint)
	for _, want := range []string{
		"## Sub-agent Model Policy",
		hint,
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("subagentHintPromptSection() missing %q\nsection:\n%s", want, section)
		}
	}
}

func TestSubagentHintPromptSectionOmittedWhenUnset(t *testing.T) {
	for name, hint := range map[string]string{
		"empty":      "",
		"whitespace": "   \n\t ",
	} {
		t.Run(name, func(t *testing.T) {
			if got := subagentHintPromptSection(hint); got != "" {
				t.Fatalf("expected empty section for %s hint, got %q", name, got)
			}
		})
	}
}

// TestWorkerPromptSubagentHintGolden is the acceptance golden check (#706):
// with subagent_hint set the rendered worker prompt gains the policy section;
// without it the prompt is byte-for-byte unchanged.
func TestWorkerPromptSubagentHintGolden(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo"}
	issue := github.Issue{Number: 706, Title: "subagent hint", Body: "body"}
	worktree := t.TempDir()

	base := assemblePrompt("base prompt", issue, worktree, "feat/subagent-hint", cfg)

	// Unset hint: the rendered prompt is identical to the base prompt.
	if got := base + subagentHintPromptSection(""); got != base {
		t.Fatalf("unset subagent_hint must not change the prompt; unexpected delta:\n%s", strings.TrimPrefix(got, base))
	}

	// Hint set: the prompt is the base with the policy section appended.
	hint := config.DefaultSubagentHint
	withHint := base + subagentHintPromptSection(hint)
	if !strings.HasPrefix(withHint, base) {
		t.Fatal("subagent_hint section must append to, not rewrite, the base prompt")
	}
	for _, want := range []string{"## Sub-agent Model Policy", hint} {
		if !strings.Contains(withHint, want) {
			t.Fatalf("rendered prompt with subagent_hint missing %q\nprompt:\n%s", want, withHint)
		}
	}
	if strings.Contains(base, "Sub-agent Model Policy") {
		t.Fatal("base prompt (no hint) must not contain the sub-agent policy section")
	}
}

func TestWorkerPromptTemplateExplainsVisualEvidenceAttachment(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "worker-prompt-template.md"))
	if err != nil {
		t.Fatalf("read worker-prompt-template.md: %v", err)
	}
	prompt := string(data)
	for _, want := range []string{
		"Visual Evidence",
		"verify.visual",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("worker-prompt-template.md missing %q", want)
		}
	}
}

// --- Tests from main: validation contract placeholder ---

func TestAssemblePrompt_ValidationContractFromFile(t *testing.T) {
	worktree := t.TempDir()
	validationContent := `## Validation Contract
- [ ] Build passes
- [ ] Tests pass
- [ ] Feature X works end-to-end`
	if err := os.WriteFile(filepath.Join(worktree, "VALIDATION.md"), []byte(validationContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Repo: "BeFeast/maestro"}
	issue := github.Issue{Number: 42, Title: "test feature", Body: "Add feature X."}
	base := "Template with {{ISSUE_NUMBER}} and {{VALIDATION_CONTRACT}} here."

	prompt := assemblePrompt(base, issue, worktree, "feat/test-42", cfg)

	if !strings.Contains(prompt, validationContent) {
		t.Fatalf("expected VALIDATION.md content in prompt, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "{{VALIDATION_CONTRACT}}") {
		t.Fatal("placeholder {{VALIDATION_CONTRACT}} was not replaced")
	}
}

func TestAssemblePrompt_ValidationContractMissingFile(t *testing.T) {
	worktree := t.TempDir() // no VALIDATION.md

	cfg := &config.Config{Repo: "BeFeast/maestro"}
	issue := github.Issue{Number: 42, Title: "test feature", Body: "Do stuff."}
	base := "Template with {{ISSUE_NUMBER}} and {{VALIDATION_CONTRACT}} end."

	prompt := assemblePrompt(base, issue, worktree, "feat/test-42", cfg)

	if strings.Contains(prompt, "{{VALIDATION_CONTRACT}}") {
		t.Fatal("placeholder {{VALIDATION_CONTRACT}} was not replaced when file is missing")
	}
	// Should contain a fallback message about no contract
	if !strings.Contains(prompt, "No VALIDATION.md found") {
		t.Fatalf("expected fallback message when VALIDATION.md missing, got:\n%s", prompt)
	}
}

func TestAssemblePrompt_NoValidationPlaceholder(t *testing.T) {
	// Templates without {{VALIDATION_CONTRACT}} and no VALIDATION.md should work unchanged
	cfg := &config.Config{Repo: "BeFeast/maestro"}
	issue := github.Issue{Number: 10, Title: "basic", Body: "Simple issue."}
	base := "Template with {{ISSUE_NUMBER}} only."

	prompt := assemblePrompt(base, issue, t.TempDir(), "feat/basic-10", cfg)

	if !strings.Contains(prompt, "10") {
		t.Fatal("expected issue number in prompt")
	}
	if strings.Contains(prompt, "VALIDATION") {
		t.Fatal("should not inject validation content when placeholder is absent and no file exists")
	}
}

func TestReadValidationContract(t *testing.T) {
	t.Run("file exists", func(t *testing.T) {
		dir := t.TempDir()
		content := "# Validation\n- build passes\n"
		if err := os.WriteFile(filepath.Join(dir, "VALIDATION.md"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		got := readValidationContract(dir)
		if got != content {
			t.Fatalf("expected %q, got %q", content, got)
		}
	})

	t.Run("file missing", func(t *testing.T) {
		got := readValidationContract(t.TempDir())
		if got != "" {
			t.Fatalf("expected empty string for missing file, got %q", got)
		}
	})
}

// --- Tests for prompt sections ---

func TestAssemblePrompt_IncludesPromptSections(t *testing.T) {
	dir := t.TempDir()

	// Write two section files
	section1 := filepath.Join(dir, "tdd.md")
	section2 := filepath.Join(dir, "style.md")
	os.WriteFile(section1, []byte("## TDD Section\nWrite tests first."), 0644)
	os.WriteFile(section2, []byte("## Style Section\nFollow coding standards."), 0644)

	cfg := &config.Config{
		Repo:           "owner/repo",
		PromptSections: []string{section1, section2},
	}
	issue := github.Issue{Number: 1, Title: "test", Body: "body"}

	base := "Base prompt with {{ISSUE_NUMBER}}"
	prompt := assemblePrompt(base, issue, "/tmp/wt", "feat/branch", cfg)

	if !strings.Contains(prompt, "## TDD Section") {
		t.Error("prompt should include TDD section content")
	}
	if !strings.Contains(prompt, "## Style Section") {
		t.Error("prompt should include style section content")
	}
}

func TestAssemblePrompt_SkipsMissingSections(t *testing.T) {
	cfg := &config.Config{
		Repo:           "owner/repo",
		PromptSections: []string{"/nonexistent/path.md"},
	}
	issue := github.Issue{Number: 1, Title: "test", Body: "body"}

	base := "Base prompt with {{ISSUE_NUMBER}}"
	prompt := assemblePrompt(base, issue, "/tmp/wt", "feat/branch", cfg)

	// Should still produce a valid prompt without crashing
	if !strings.Contains(prompt, "Base prompt with 1") {
		t.Error("prompt should still contain base content when sections are missing")
	}
}

func TestAssemblePrompt_IncludesValidationContractAutoAppend(t *testing.T) {
	dir := t.TempDir()

	// Write a VALIDATION.md in the worktree
	validationContent := "## Validation Contract\n- [ ] Build passes\n- [ ] Tests pass"
	os.WriteFile(filepath.Join(dir, "VALIDATION.md"), []byte(validationContent), 0644)

	cfg := &config.Config{Repo: "owner/repo"}
	issue := github.Issue{Number: 5, Title: "test", Body: "body"}

	// Template WITHOUT {{VALIDATION_CONTRACT}} — contract should be auto-appended
	base := "Base prompt with {{ISSUE_NUMBER}}"
	prompt := assemblePrompt(base, issue, dir, "feat/branch", cfg)

	if !strings.Contains(prompt, "Validation Contract") {
		t.Error("prompt should auto-append VALIDATION.md content when placeholder is absent")
	}
	if !strings.Contains(prompt, "Build passes") {
		t.Error("prompt should include quality gates from VALIDATION.md")
	}
}

func TestAssemblePrompt_NoValidationFileIsOK(t *testing.T) {
	dir := t.TempDir() // empty dir, no VALIDATION.md

	cfg := &config.Config{Repo: "owner/repo"}
	issue := github.Issue{Number: 1, Title: "test", Body: "body"}

	base := "Base prompt with {{ISSUE_NUMBER}}"
	prompt := assemblePrompt(base, issue, dir, "feat/branch", cfg)

	// Should still produce a valid prompt
	if !strings.Contains(prompt, "Base prompt with 1") {
		t.Error("prompt should work without VALIDATION.md")
	}
}

func TestAssemblePrompt_SanitizesInjectedFileContext(t *testing.T) {
	dir := t.TempDir()
	section := filepath.Join(dir, "section.md")
	if err := os.WriteFile(section, []byte{'#', ' ', 'S', 'e', 'c', 't', 'i', 'o', 'n', '\n', 0xff}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "VALIDATION.md"), []byte{'V', 'a', 'l', 'i', 'd', 'a', 't', 'i', 'o', 'n', ':', ' ', 0xfe}, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Repo: "owner/repo", PromptSections: []string{section}}
	issue := github.Issue{Number: 7, Title: "utf8", Body: "body"}
	prompt := assemblePrompt("Base prompt with {{ISSUE_NUMBER}} and {{VALIDATION_CONTRACT}}", issue, dir, "feat/utf8", cfg)

	if !utf8.ValidString(prompt) {
		t.Fatalf("assembled prompt is not valid UTF-8: %q", prompt)
	}
	if got := strings.Count(prompt, "\uFFFD"); got != 2 {
		t.Fatalf("replacement count = %d, want 2; prompt=%q", got, prompt)
	}
}

func managementHomeCfg(repo string) *config.Config {
	return &config.Config{
		Repo:      repo,
		ProjectID: "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		ManagementHome: config.ManagementHomeConfig{
			Kind:      config.ManagementHomeKindObsidian,
			Path:      "/home/god/vault/Dev/Areas/maestro",
			Vault:     "god",
			VaultPath: "Dev/Areas/maestro",
		},
	}
}

func TestAssemblePromptIncludesManagementHomeBoundary(t *testing.T) {
	cfg := managementHomeCfg("BeFeast/maestro")
	issue := github.Issue{Number: 870, Title: "management home", Body: "surface it"}

	prompt := assemblePrompt("base prompt", issue, "/tmp/worktree", "feat/mh", cfg)

	for _, want := range []string{
		"## Management Home (private PM context)",
		config.ManagementHomeBoundary,
		"Project id: `3f2504e0-4f89-41d3-9a0c-0305e82c3301`",
		"Management Home (vault-relative): `Dev/Areas/maestro`",
		"/home/god/vault/Dev/Areas/maestro",
		"never post/copy this into GitHub or repo files",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("assemblePrompt() missing %q\nprompt:\n%s", want, prompt)
		}
	}
}

// TestAssemblePromptIncludesManagementHomeBoundaryTemplatePath exercises the
// {{ISSUE_NUMBER}} template branch of assemblePrompt as well as the legacy path.
func TestAssemblePromptIncludesManagementHomeBoundaryTemplatePath(t *testing.T) {
	cfg := managementHomeCfg("BeFeast/maestro")
	issue := github.Issue{Number: 870, Title: "management home", Body: "surface it"}

	prompt := assemblePrompt("base prompt with {{ISSUE_NUMBER}}", issue, "/tmp/worktree", "feat/mh", cfg)

	if !strings.Contains(prompt, "## Management Home (private PM context)") {
		t.Fatalf("template-path prompt missing Management Home section\nprompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, config.ManagementHomeBoundary) {
		t.Fatalf("template-path prompt missing boundary statement")
	}
}

func TestAssemblePromptOmitsManagementHomeWhenUnconfigured(t *testing.T) {
	cfg := &config.Config{Repo: "BeFeast/maestro"}
	issue := github.Issue{Number: 1, Title: "no home", Body: "body"}

	prompt := assemblePrompt("base prompt", issue, "/tmp/worktree", "feat/nohome", cfg)

	if strings.Contains(prompt, "Management Home") {
		t.Fatalf("unconfigured project should render no Management Home section\nprompt:\n%s", prompt)
	}
}
