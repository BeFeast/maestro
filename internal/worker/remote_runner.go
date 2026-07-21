package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/befeast/maestro/internal/config"
)

const remoteRunnerScriptSuffix = "-remote.sh"

// workerExecutionWorktree returns the directory the agent CLI must see. Local
// workers use the control-plane worktree directly. An opted-in SSH worker uses
// the deterministic runner-side path while Maestro retains the local worktree
// as a lightweight lifecycle/identity shadow.
func workerExecutionWorktree(cfg *config.Config, slotName, localWorktree string) string {
	if cfg == nil || !cfg.RemoteRunner.Enabled {
		return localWorktree
	}
	return pathpkg.Join(cfg.RemoteRunner.WorktreeBase, slotName)
}

func writeConfiguredWorkerRunnerScript(cfg *config.Config, slotName, branch, promptFile, runnerPath string, args []string, stdinFile, logFile, localWorktree string, split *streamSplit) error {
	if cfg == nil {
		return fmt.Errorf("write worker runner: nil config")
	}
	if !cfg.RemoteRunner.Enabled {
		return writeWorkerRunnerScript(cfg.StateDir, runnerPath, args, stdinFile, logFile, localWorktree, split)
	}
	return writeRemoteWorkerRunnerScript(cfg, slotName, branch, promptFile, runnerPath, args, stdinFile, logFile, localWorktree, split)
}

func writeRemoteWorkerRunnerScript(cfg *config.Config, slotName, branch, promptFile, runnerPath string, args []string, stdinFile, logFile, localWorktree string, split *streamSplit) error {
	if cfg == nil || !cfg.RemoteRunner.Enabled {
		return fmt.Errorf("write remote worker runner: remote runner is disabled")
	}
	if slotName == "" || pathpkg.Base(slotName) != slotName || slotName == "." || slotName == ".." {
		return fmt.Errorf("write remote worker runner: invalid slot name")
	}
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return fmt.Errorf("write remote worker runner: worker command is empty")
	}
	prompt, err := os.ReadFile(promptFile)
	if err != nil {
		return fmt.Errorf("write remote worker runner: read prompt: %w", err)
	}
	sshPath, err := resolveRemoteExecutable(cfg.RemoteRunner.SSHCommand)
	if err != nil {
		return fmt.Errorf("write remote worker runner: %w", err)
	}
	envPath, err := resolveRemoteExecutable("env")
	if err != nil {
		return fmt.Errorf("write remote worker runner: %w", err)
	}
	remoteWorktree := workerExecutionWorktree(cfg, slotName, localWorktree)
	remotePrompt := pathpkg.Join(cfg.RemoteRunner.WorktreeBase, ".maestro-prompts", slotName+".md")
	remoteArgs := replaceExactArg(args, promptFile, remotePrompt)
	remoteScript := buildRemoteWorkerScript(cfg.RemoteRunner, branch, remoteWorktree, remotePrompt, remoteArgs, stdinFile != "", prompt)
	remoteScriptPath := filepath.Join(cfg.StateDir, slotName+remoteRunnerScriptSuffix)
	if err := writeFileAtomicMode(filepath.Dir(remoteScriptPath), remoteScriptPath, remoteScript, workerRunnerScriptMode); err != nil {
		return fmt.Errorf("write remote worker bootstrap: %w", err)
	}
	if split != nil && split.MaxTokens > 0 && split.MarkerPath != "" {
		if err := os.Remove(split.MarkerPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clear stale token-budget marker: %w", err)
		}
	}
	runner := buildRemoteControlPlaneRunner(cfg.RemoteRunner, envPath, sshPath, remoteScriptPath, logFile, localWorktree, remoteWorktree, split)
	if err := writeFileAtomicMode(filepath.Dir(runnerPath), runnerPath, runner, workerRunnerScriptMode); err != nil {
		return fmt.Errorf("write remote worker runner: %w", err)
	}
	return nil
}

func resolveRemoteExecutable(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("remote ssh command is empty")
	}
	resolved := name
	var err error
	if !filepath.IsAbs(name) {
		resolved, err = exec.LookPath(name)
		if err != nil {
			return "", fmt.Errorf("resolve remote ssh executable %q: %w", name, err)
		}
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("make remote ssh executable absolute: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("validate remote ssh executable: %w", err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("remote ssh executable is not executable")
	}
	return resolved, nil
}

func replaceExactArg(args []string, oldValue, newValue string) []string {
	out := append([]string(nil), args...)
	for i := range out {
		if out[i] == oldValue {
			out[i] = newValue
		}
	}
	return out
}

func buildRemoteControlPlaneRunner(remote config.RemoteRunnerConfig, envPath, sshPath, remoteScriptPath, logFile, localWorktree, remoteWorktree string, split *streamSplit) string {
	sshArgs := []string{
		sshPath,
		"-T",
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "ForwardAgent=no",
	}
	sshArgs = append(sshArgs, remote.SSHArgs...)
	sshArgs = append(sshArgs, remote.Target, "/bin/bash", "-s")

	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("set -o pipefail\n")
	b.WriteString("export MAESTRO_WORKTREE=" + shellQuote(localWorktree) + "\n")
	for _, key := range workerCredentialEnvKeys {
		b.WriteString("unset " + key + "\n")
	}
	b.WriteString("unset " + workerCredentialsFileEnvVar + "\n")
	b.WriteString("cd \"$MAESTRO_WORKTREE\" || exit 1\n")
	b.WriteString("printf '[maestro] remote worker target: %s; worktree: %s\\n' " + shellQuote(remote.Target) + " " + shellQuote(remoteWorktree) + " | tee -a " + shellQuote(logFile) + "\n")
	transport := shellQuote(envPath) + " -i" +
		" HOME=\"${HOME:-}\"" +
		" USER=\"${USER:-}\"" +
		" LOGNAME=\"${LOGNAME:-}\"" +
		" PATH=\"${PATH:-}\"" +
		" SSH_AUTH_SOCK=\"${SSH_AUTH_SOCK:-}\" " +
		shellJoin(sshArgs)
	b.WriteString(transport + " < " + shellQuote(remoteScriptPath) + " " + logPipeline(split, logFile) + "\n")
	b.WriteString("exit $?\n")
	return b.String()
}

func buildRemoteWorkerScript(remote config.RemoteRunnerConfig, branch, remoteWorktree, remotePrompt string, args []string, promptOnStdin bool, prompt []byte) string {
	delimiter := remotePromptDelimiter(prompt)
	credentialsArg := ""
	if remote.CredentialsFile != "" {
		credentialsArg = " --credentials-file " + shellQuote(remote.CredentialsFile)
	}

	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("set -euo pipefail\n")
	b.WriteString("umask 077\n")
	b.WriteString("REMOTE_REPO=" + shellQuote(remote.RepoPath) + "\n")
	b.WriteString("REMOTE_WORKTREE=" + shellQuote(remoteWorktree) + "\n")
	b.WriteString("REMOTE_BRANCH=" + shellQuote(branch) + "\n")
	b.WriteString("REMOTE_BASE=" + shellQuote(remote.BaseBranch) + "\n")
	b.WriteString("REMOTE_PROMPT=" + shellQuote(remotePrompt) + "\n")
	b.WriteString("REMOTE_MAESTRO=" + shellQuote(remote.MaestroCommand) + "\n")
	b.WriteString("maestro_require_command() {\n")
	b.WriteString("  case \"$1\" in\n")
	b.WriteString("    */*) [ -x \"$1\" ] ;;\n")
	b.WriteString("    *) command -v \"$1\" >/dev/null 2>&1 ;;\n")
	b.WriteString("  esac || { printf '[maestro] remote runner: missing required CLI: %s\\n' \"$1\" >&2; exit 127; }\n")
	b.WriteString("}\n")
	b.WriteString("maestro_require_command \"$REMOTE_MAESTRO\"\n")
	b.WriteString("maestro_require_command git\n")
	b.WriteString("maestro_require_command gh\n")
	b.WriteString("maestro_require_command " + shellQuote(args[0]) + "\n")
	b.WriteString("maestro_exec() {\n")
	b.WriteString("  \"$REMOTE_MAESTRO\" _worker-exec" + credentialsArg + " -- \"$@\"\n")
	b.WriteString("}\n")
	b.WriteString("if [ ! -d \"$REMOTE_REPO\" ]; then\n")
	b.WriteString("  printf '[maestro] remote runner: repo_path is not a directory\\n' >&2\n")
	b.WriteString("  exit 72\n")
	b.WriteString("fi\n")
	b.WriteString("maestro_exec git -C \"$REMOTE_REPO\" rev-parse --git-dir >/dev/null\n")
	b.WriteString("mkdir -p " + shellQuote(remote.WorktreeBase) + "\n")
	b.WriteString("maestro_exec git -C \"$REMOTE_REPO\" fetch --prune origin\n")
	b.WriteString("maestro_exec git -C \"$REMOTE_REPO\" worktree prune\n")
	b.WriteString("if [ -e \"$REMOTE_WORKTREE\" ]; then\n")
	b.WriteString("  if [ ! -d \"$REMOTE_WORKTREE\" ]; then\n")
	b.WriteString("    printf '[maestro] remote runner: worktree path exists but is not a directory\\n' >&2\n")
	b.WriteString("    exit 73\n")
	b.WriteString("  fi\n")
	b.WriteString("  current_branch=$(maestro_exec git -C \"$REMOTE_WORKTREE\" symbolic-ref --short HEAD)\n")
	b.WriteString("  if [ \"$current_branch\" != \"$REMOTE_BRANCH\" ]; then\n")
	b.WriteString("    printf '[maestro] remote runner: leftover worktree branch mismatch\\n' >&2\n")
	b.WriteString("    exit 74\n")
	b.WriteString("  fi\n")
	b.WriteString("elif maestro_exec git -C \"$REMOTE_REPO\" show-ref --verify --quiet \"refs/heads/$REMOTE_BRANCH\"; then\n")
	b.WriteString("  maestro_exec git -C \"$REMOTE_REPO\" worktree add \"$REMOTE_WORKTREE\" \"$REMOTE_BRANCH\"\n")
	b.WriteString("elif maestro_exec git -C \"$REMOTE_REPO\" show-ref --verify --quiet \"refs/remotes/origin/$REMOTE_BRANCH\"; then\n")
	b.WriteString("  maestro_exec git -C \"$REMOTE_REPO\" worktree add -b \"$REMOTE_BRANCH\" \"$REMOTE_WORKTREE\" \"origin/$REMOTE_BRANCH\"\n")
	b.WriteString("else\n")
	b.WriteString("  maestro_exec git -C \"$REMOTE_REPO\" worktree add -b \"$REMOTE_BRANCH\" \"$REMOTE_WORKTREE\" \"origin/$REMOTE_BASE\"\n")
	b.WriteString("fi\n")
	b.WriteString("mkdir -p " + shellQuote(pathpkg.Dir(remotePrompt)) + "\n")
	b.WriteString("chmod 700 " + shellQuote(pathpkg.Dir(remotePrompt)) + "\n")
	b.WriteString("trap 'rm -f \"$REMOTE_PROMPT\"' EXIT\n")
	b.WriteString("cat > \"$REMOTE_PROMPT\" <<'" + delimiter + "'\n")
	b.Write(prompt)
	if len(prompt) == 0 || prompt[len(prompt)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString(delimiter + "\n")
	b.WriteString("if ! maestro_exec gh auth status >/dev/null 2>&1; then\n")
	b.WriteString("  printf '[maestro] remote runner: GitHub authentication unavailable\\n' >&2\n")
	b.WriteString("  exit 77\n")
	b.WriteString("fi\n")
	b.WriteString("export MAESTRO_WORKTREE=\"$REMOTE_WORKTREE\"\n")
	b.WriteString("cd \"$REMOTE_WORKTREE\"\n")
	b.WriteString("printf '[maestro] remote worker worktree: %s\\n' \"$REMOTE_WORKTREE\"\n")
	command := "maestro_exec " + shellJoin(args)
	if promptOnStdin {
		command += " < \"$REMOTE_PROMPT\""
	}
	b.WriteString(command + "\n")
	return b.String()
}

func remotePromptDelimiter(prompt []byte) string {
	sum := sha256.Sum256(prompt)
	delimiter := "MAESTRO_REMOTE_PROMPT_" + strings.ToUpper(hex.EncodeToString(sum[:8]))
	for {
		collision := false
		for _, line := range strings.Split(string(prompt), "\n") {
			if line == delimiter {
				collision = true
				break
			}
		}
		if !collision {
			return delimiter
		}
		delimiter += "X"
	}
}
