package pipeline

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func TestReadAdvisorResultStrictVerdicts(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name     string
		content  string
		verdict  string
		findings string
		wantErr  bool
	}{
		{name: "approved", content: "PLAN_APPROVED\nReady.", verdict: AdvisorVerdictApproved, findings: "Ready."},
		{name: "revise", content: "PLAN_REVISE\nMissing rollback coverage.\nAdd a timeout test.", verdict: AdvisorVerdictRevise, findings: "Missing rollback coverage.\nAdd a timeout test."},
		{name: "leading blank", content: "\nPLAN_APPROVED", wantErr: true},
		{name: "leading prose", content: "Review complete\nPLAN_APPROVED", wantErr: true},
		{name: "marker suffix", content: "PLAN_APPROVED because it is good", wantErr: true},
		{name: "revise without findings", content: "PLAN_REVISE\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, AdvisorReviewFile), []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := ReadAdvisorResult(dir)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ReadAdvisorResult() = %+v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadAdvisorResult: %v", err)
			}
			if got.Verdict != tt.verdict || got.Findings != tt.findings {
				t.Fatalf("result = %+v, want verdict=%q findings=%q", got, tt.verdict, tt.findings)
			}
		})
	}
}

func TestReadAdvisorResultMissingFailsClosed(t *testing.T) {
	if _, err := ReadAdvisorResult(t.TempDir()); err == nil {
		t.Fatal("missing Advisor artifact must not be implicit approval")
	}
}

func TestAdvisorWorkspaceAllowsOnlyReviewArtifact(t *testing.T) {
	dir := advisorGitWorktree(t)
	before, err := CaptureAdvisorWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, AdvisorReviewFile), []byte("PLAN_APPROVED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if reason, err := ValidateAdvisorWorkspace(dir, before); err != nil {
		t.Fatalf("review artifact should be allowed, reason=%q err=%v", reason, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source.go"), []byte("package source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if reason, err := ValidateAdvisorWorkspace(dir, before); err == nil || reason != "advisor_worktree_mutation" {
		t.Fatalf("source edit reason=%q err=%v, want advisor_worktree_mutation", reason, err)
	}
}

func TestAdvisorWorkspaceRejectsCanonicalMutationAndCommit(t *testing.T) {
	dir := advisorGitWorktree(t)
	before, err := CaptureAdvisorWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, PlanFile), []byte("changed plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if reason, err := ValidateAdvisorWorkspace(dir, before); err == nil || reason != "canonical_artifact_mutated" {
		t.Fatalf("canonical mutation reason=%q err=%v", reason, err)
	}

	gitRun(t, dir, "checkout", "--", PlanFile)
	if err := os.WriteFile(filepath.Join(dir, AdvisorReviewFile), []byte("PLAN_APPROVED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", AdvisorReviewFile)
	gitRun(t, dir, "commit", "-m", "advisor should not commit")
	if reason, err := ValidateAdvisorWorkspace(dir, before); err == nil || reason != "advisor_commit_detected" {
		t.Fatalf("commit reason=%q err=%v", reason, err)
	}
}

func TestAdvisorPromptCarriesMandatoryPacket(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, PlanFile), []byte("step one"), 0o644)
	os.WriteFile(filepath.Join(dir, ValidationFile), []byte("assertion one"), 0o644)
	cfg := &config.Config{Repo: "owner/repo", Pipeline: config.PipelineConfig{Advisor: config.RoleConfig{Enabled: true}}}
	sess := &state.Session{PlanVersion: 3, AdvisorReviewRound: 2, AdvisorFindingsLedger: "Plan v2: cover retries"}
	prompt, err := AdvisorPrompt(cfg, github.Issue{Number: 9, Title: "Gate plans", Body: "Issue body"}, dir, "feat/gate", sess)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#9: Gate plans", "Issue body", "Plan version: 3", "Review round: 2", "Plan v2: cover retries", "step one", "assertion one", AdvisorVerdictApproved, AdvisorVerdictRevise, AdvisorReviewFile} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("Advisor prompt missing %q:\n%s", want, prompt)
		}
	}
}

func advisorGitWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "advisor@example.invalid")
	gitRun(t, dir, "config", "user.name", "Advisor Test")
	if err := os.WriteFile(filepath.Join(dir, PlanFile), []byte("plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ValidationFile), []byte("validation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", PlanFile, ValidationFile)
	gitRun(t, dir, "commit", "-m", "plan")
	return dir
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", dir}, args...)
	if out, err := exec.Command("git", cmdArgs...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
