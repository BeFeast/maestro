package orchestrator

import (
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
)

func makeIssue(number int, title string, labels ...string) github.Issue {
	issue := github.Issue{Number: number, Title: title}
	for _, l := range labels {
		issue.Labels = append(issue.Labels, struct {
			Name string `json:"name"`
		}{Name: l})
	}
	return issue
}

func TestSelectPrompt_BugLabel(t *testing.T) {
	o := &Orchestrator{
		cfg:                   &config.Config{Repo: "owner/repo"},
		promptBase:            "default prompt",
		bugPromptBase:         "bug prompt",
		enhancementPromptBase: "enhancement prompt",
	}
	got := o.selectPrompt(makeIssue(1, "Fix crash", "bug"))
	if got != "bug prompt" {
		t.Errorf("selectPrompt() = %q, want %q", got, "bug prompt")
	}
}

func TestSelectPrompt_EnhancementLabel(t *testing.T) {
	o := &Orchestrator{
		cfg:                   &config.Config{Repo: "owner/repo"},
		promptBase:            "default prompt",
		bugPromptBase:         "bug prompt",
		enhancementPromptBase: "enhancement prompt",
	}
	got := o.selectPrompt(makeIssue(2, "Add feature", "enhancement"))
	if got != "enhancement prompt" {
		t.Errorf("selectPrompt() = %q, want %q", got, "enhancement prompt")
	}
}

func TestSelectPrompt_FallbackToDefault(t *testing.T) {
	o := &Orchestrator{
		cfg:                   &config.Config{Repo: "owner/repo"},
		promptBase:            "default prompt",
		bugPromptBase:         "bug prompt",
		enhancementPromptBase: "enhancement prompt",
	}
	got := o.selectPrompt(makeIssue(3, "Update docs", "documentation"))
	if got != "default prompt" {
		t.Errorf("selectPrompt() = %q, want %q", got, "default prompt")
	}
}

func TestSelectPrompt_BugTakesPrecedenceOverEnhancement(t *testing.T) {
	o := &Orchestrator{
		cfg:                   &config.Config{Repo: "owner/repo"},
		promptBase:            "default prompt",
		bugPromptBase:         "bug prompt",
		enhancementPromptBase: "enhancement prompt",
	}
	got := o.selectPrompt(makeIssue(4, "Bug and enhancement", "bug", "enhancement"))
	if got != "bug prompt" {
		t.Errorf("selectPrompt() = %q, want %q (bug should take precedence)", got, "bug prompt")
	}
}

func TestSelectPrompt_NoBugPromptConfigured(t *testing.T) {
	o := &Orchestrator{
		cfg:                   &config.Config{Repo: "owner/repo"},
		promptBase:            "default prompt",
		bugPromptBase:         "",
		enhancementPromptBase: "enhancement prompt",
	}
	got := o.selectPrompt(makeIssue(5, "Fix crash", "bug"))
	if got != "default prompt" {
		t.Errorf("selectPrompt() = %q, want %q (should fall back when bug_prompt not set)", got, "default prompt")
	}
}

func TestSelectPrompt_NoEnhancementPromptConfigured(t *testing.T) {
	o := &Orchestrator{
		cfg:                   &config.Config{Repo: "owner/repo"},
		promptBase:            "default prompt",
		bugPromptBase:         "bug prompt",
		enhancementPromptBase: "",
	}
	got := o.selectPrompt(makeIssue(6, "Add feature", "enhancement"))
	if got != "default prompt" {
		t.Errorf("selectPrompt() = %q, want %q (should fall back when enhancement_prompt not set)", got, "default prompt")
	}
}

func TestSelectPrompt_NoLabels(t *testing.T) {
	o := &Orchestrator{
		cfg:                   &config.Config{Repo: "owner/repo"},
		promptBase:            "default prompt",
		bugPromptBase:         "bug prompt",
		enhancementPromptBase: "enhancement prompt",
	}
	got := o.selectPrompt(makeIssue(7, "Something"))
	if got != "default prompt" {
		t.Errorf("selectPrompt() = %q, want %q", got, "default prompt")
	}
}

func TestSelectPrompt_CaseInsensitiveLabel(t *testing.T) {
	o := &Orchestrator{
		cfg:                   &config.Config{Repo: "owner/repo"},
		promptBase:            "default prompt",
		bugPromptBase:         "bug prompt",
		enhancementPromptBase: "enhancement prompt",
	}
	got := o.selectPrompt(makeIssue(8, "Fix crash", "Bug"))
	if got != "bug prompt" {
		t.Errorf("selectPrompt() = %q, want %q (label matching should be case-insensitive)", got, "bug prompt")
	}
}

func TestRunDeployCmd_Success(t *testing.T) {
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:      "owner/repo",
			LocalPath: "/tmp",
			DeployCmd: "echo deploy-ok",
		},
		notifier: &notify.Notifier{},
	}
	if err := o.runDeployCmd(42); err != nil {
		t.Errorf("runDeployCmd() unexpected error: %v", err)
	}
}

func TestRunDeployCmd_Failure(t *testing.T) {
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:      "owner/repo",
			LocalPath: "/tmp",
			DeployCmd: "exit 1",
		},
		notifier: &notify.Notifier{},
	}
	err := o.runDeployCmd(42)
	if err == nil {
		t.Fatal("runDeployCmd() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "deploy command failed") {
		t.Errorf("error = %q, want it to contain 'deploy command failed'", err.Error())
	}
}

func TestRunDeployCmd_CapturesOutput(t *testing.T) {
	o := &Orchestrator{
		cfg: &config.Config{
			Repo:      "owner/repo",
			LocalPath: "/tmp",
			DeployCmd: "echo hello-deploy && exit 1",
		},
		notifier: &notify.Notifier{},
	}
	err := o.runDeployCmd(42)
	if err == nil {
		t.Fatal("runDeployCmd() expected error, got nil")
	}
	if !strings.Contains(err.Error(), "hello-deploy") {
		t.Errorf("error = %q, want it to contain command output 'hello-deploy'", err.Error())
	}
}
