package worker

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

func TestWorkerExecutionWorktreeDefaultsLocal(t *testing.T) {
	local := "/control/worktrees/repo/rep-1"
	if got := workerExecutionWorktree(&config.Config{}, "rep-1", local); got != local {
		t.Fatalf("workerExecutionWorktree = %q, want %q", got, local)
	}
	cfg := &config.Config{RemoteRunner: config.RemoteRunnerConfig{Enabled: true, WorktreeBase: "/runner/worktrees/repo"}}
	if got := workerExecutionWorktree(cfg, "rep-1", local); got != "/runner/worktrees/repo/rep-1" {
		t.Fatalf("remote workerExecutionWorktree = %q", got)
	}
}

func TestRemoteRunnerRejectsIssueOptedPhasePipeline(t *testing.T) {
	cfg := &config.Config{
		RemoteRunner: config.RemoteRunnerConfig{Enabled: true},
		Pipeline:     config.PipelineConfig{Enabled: true},
	}
	_, err := StartReserved(cfg, &state.State{}, "owner/repo", github.Issue{Number: 1058, Title: "remote"}, "prompt", "codex", "rep-1")
	if err == nil || !strings.Contains(err.Error(), "phase-pipeline") {
		t.Fatalf("StartReserved error = %v", err)
	}
}

func TestRemotePromptUsesRunnerPathAndLocalRuleSource(t *testing.T) {
	sourceWorktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceWorktree, "CLAUDE.md"), []byte("REMOTE-RULE-CANARY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	remoteWorktree := "/srv/maestro/worktrees/repo/rep-1"
	cfg := &config.Config{
		Repo: "owner/repo",
		RemoteRunner: config.RemoteRunnerConfig{
			Enabled: true,
		},
		ManagementHome: config.ManagementHomeConfig{
			Kind:      config.ManagementHomeKindObsidian,
			Path:      "/control/private/management-home",
			Vault:     "Vault",
			VaultPath: "Dev/Areas/repo",
		},
	}
	prompt := assemblePromptWithSource("base prompt", github.Issue{Number: 1058, Title: "remote", Body: "body"}, remoteWorktree, sourceWorktree, "feat/rep-1-1058-remote", cfg)
	for _, want := range []string{remoteWorktree, "REMOTE-RULE-CANARY", "Dev/Areas/repo"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("remote prompt missing %q", want)
		}
	}
	for _, forbidden := range []string{sourceWorktree, "/control/private/management-home"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("remote prompt leaked control-plane path %q", forbidden)
		}
	}
}

func TestRemoteWorkerRunnerProvisionsWorktreeAndRunsAgent(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	remoteRepo := filepath.Join(root, "runner-repo")
	remoteWorktreeBase := filepath.Join(root, "runner-worktrees")
	stateDir := filepath.Join(root, "state")
	logDir := filepath.Join(stateDir, "logs")
	localWorktree := filepath.Join(root, "control-shadow")
	fakeBin := filepath.Join(root, "bin")
	for _, dir := range []string{stateDir, logDir, localWorktree, fakeBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	runRemoteRunnerTestCommand(t, root, "git", "init", "--bare", "--initial-branch=main", origin)
	runRemoteRunnerTestCommand(t, root, "git", "clone", origin, seed)
	runRemoteRunnerTestCommand(t, seed, "git", "config", "user.email", "runner-test@example.invalid")
	runRemoteRunnerTestCommand(t, seed, "git", "config", "user.name", "Runner Test")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runRemoteRunnerTestCommand(t, seed, "git", "add", "README.md")
	runRemoteRunnerTestCommand(t, seed, "git", "commit", "-m", "seed")
	runRemoteRunnerTestCommand(t, seed, "git", "push", "origin", "main")
	runRemoteRunnerTestCommand(t, root, "git", "clone", origin, remoteRepo)

	sshPath := writeRemoteRunnerTestExecutable(t, fakeBin, "ssh", `#!/bin/bash
exec /bin/bash -s
`)
	maestroPath := writeRemoteRunnerTestExecutable(t, fakeBin, "maestro", `#!/bin/bash
if [ "$1" = "_worker-exec" ]; then shift; fi
if [ "${1:-}" = "--credentials-file" ]; then shift 2; fi
if [ "${1:-}" = "--" ]; then shift; fi
exec "$@"
`)
	writeRemoteRunnerTestExecutable(t, fakeBin, "gh", `#!/bin/bash
if [ "${1:-}" = "auth" ] && [ "${2:-}" = "status" ]; then exit 0; fi
exit 2
`)
	agentPath := writeRemoteRunnerTestExecutable(t, fakeBin, "agent", `#!/bin/bash
cat > prompt.received
printf 'remote-agent-ok\n'
`)

	promptFile := filepath.Join(stateDir, "rep-1-prompt.md")
	prompt := "work only in /runner/worktrees/repo/rep-1\n"
	if err := os.WriteFile(promptFile, []byte(prompt), 0o600); err != nil {
		t.Fatal(err)
	}
	runnerPath := filepath.Join(stateDir, "rep-1-run.sh")
	logFile := filepath.Join(logDir, "rep-1.log")
	cfg := &config.Config{
		StateDir: stateDir,
		RemoteRunner: config.RemoteRunnerConfig{
			Enabled:        true,
			Target:         "runner.test",
			RepoPath:       remoteRepo,
			WorktreeBase:   remoteWorktreeBase,
			BaseBranch:     "main",
			SSHCommand:     sshPath,
			MaestroCommand: maestroPath,
		},
	}
	branch := "feat/rep-1-1058-remote-runner"
	t.Setenv("OPENAI_API_KEY", "control-plane-secret-must-not-cross")
	if err := writeRemoteWorkerRunnerScript(cfg, "rep-1", branch, promptFile, runnerPath, []string{agentPath}, promptFile, logFile, localWorktree, nil); err != nil {
		t.Fatalf("writeRemoteWorkerRunnerScript: %v", err)
	}

	cmd := exec.Command("/bin/bash", runnerPath)
	cmd.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run remote runner: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "remote-agent-ok") {
		t.Fatalf("runner output = %q", out)
	}
	remoteWorktree := filepath.Join(remoteWorktreeBase, "rep-1")
	gotPrompt, err := os.ReadFile(filepath.Join(remoteWorktree, "prompt.received"))
	if err != nil {
		t.Fatalf("read remote agent prompt: %v", err)
	}
	if string(gotPrompt) != prompt {
		t.Fatalf("remote prompt = %q, want %q", gotPrompt, prompt)
	}
	branchOut := runRemoteRunnerTestCommand(t, remoteWorktree, "git", "symbolic-ref", "--short", "HEAD")
	if strings.TrimSpace(branchOut) != branch {
		t.Fatalf("remote branch = %q, want %q", strings.TrimSpace(branchOut), branch)
	}

	remoteScript, err := os.ReadFile(filepath.Join(stateDir, "rep-1"+remoteRunnerScriptSuffix))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"control-plane-secret-must-not-cross", stateDir, localWorktree, ".db", "DATABASE_URL"} {
		if strings.Contains(string(remoteScript), forbidden) {
			t.Fatalf("remote bootstrap contains forbidden control-plane material %q", forbidden)
		}
	}
	localRunner, err := os.ReadFile(runnerPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(localRunner), "control-plane-secret-must-not-cross") {
		t.Fatal("control-plane credential value was written to runner")
	}
	for _, required := range []string{"unset OPENAI_API_KEY", "ForwardAgent=no", "ClearAllForwardings=yes", " -i HOME=\"${HOME:-}\""} {
		if !strings.Contains(string(localRunner), required) {
			t.Fatalf("local runner missing %q", required)
		}
	}
}

func TestRemoteWorkerScriptRewritesFilePromptArgument(t *testing.T) {
	remote := config.RemoteRunnerConfig{
		RepoPath:       "/srv/repos/repo",
		WorktreeBase:   "/srv/worktrees/repo",
		BaseBranch:     "main",
		MaestroCommand: "maestro",
	}
	localPrompt := "/control/state/prompt.md"
	remotePrompt := "/srv/worktrees/repo/.maestro-prompts/rep-1.md"
	args := replaceExactArg([]string{"custom-agent", "--prompt", localPrompt}, localPrompt, remotePrompt)
	script := buildRemoteWorkerScript(remote, "feat/rep-1-1058-spike", "/srv/worktrees/repo/rep-1", remotePrompt, args, false, []byte("prompt\n"))
	if strings.Contains(script, localPrompt) || !strings.Contains(script, remotePrompt) {
		t.Fatalf("remote script prompt path rewrite failed:\n%s", script)
	}
}

func writeRemoteRunnerTestExecutable(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func runRemoteRunnerTestCommand(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}
