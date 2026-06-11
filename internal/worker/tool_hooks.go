package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/befeast/maestro/internal/config"
)

const (
	defaultPreToolMatcher  = "*"
	defaultPostEditMatcher = "Edit|MultiEdit|Write"
)

type workerToolHookSetup struct {
	RunnerPath string
	Claude     bool
}

// setupWorkerToolHooks prepares the hook runner. backendKind is the resolved
// exec-path kind from resolveBackendKind (not the raw config key), so a claude
// CLI under a custom backend name still gets the automatic hook installer (#684).
func setupWorkerToolHooks(stateDir, worktree, backendKind string, hooks config.HooksConfig) (workerToolHookSetup, error) {
	if !toolHookConfigured(hooks.PreTool) && !toolHookConfigured(hooks.PostEdit) {
		return workerToolHookSetup{}, nil
	}
	if strings.TrimSpace(stateDir) == "" {
		return workerToolHookSetup{}, fmt.Errorf("empty state dir")
	}
	if strings.TrimSpace(worktree) == "" {
		return workerToolHookSetup{}, fmt.Errorf("empty worktree")
	}

	runnerPath, err := writeWorkerToolHookRunner(stateDir, hooks)
	if err != nil {
		return workerToolHookSetup{}, err
	}

	setup := workerToolHookSetup{RunnerPath: runnerPath}
	if backendKind == config.BackendKindClaude {
		if err := writeClaudeToolHookSettings(worktree, runnerPath, hooks); err != nil {
			return setup, err
		}
		if err := excludeClaudeLocalSettings(worktree); err != nil {
			return setup, err
		}
		setup.Claude = true
	}
	return setup, nil
}

func toolHookConfigured(hook config.ToolHookConfig) bool {
	return strings.TrimSpace(hook.Command) != ""
}

func writeWorkerToolHookRunner(stateDir string, hooks config.HooksConfig) (string, error) {
	hookDir := filepath.Join(stateDir, "worker-tool-hooks")
	if err := os.MkdirAll(hookDir, 0755); err != nil {
		return "", fmt.Errorf("create worker tool hook dir: %w", err)
	}

	payload, err := json.Marshal(struct {
		PreTool  config.ToolHookConfig `json:"pre_tool"`
		PostEdit config.ToolHookConfig `json:"post_edit"`
		Timeout  int                   `json:"timeout_ms"`
	}{
		PreTool:  hooks.PreTool,
		PostEdit: hooks.PostEdit,
		Timeout:  hooks.TimeoutMs,
	})
	if err != nil {
		return "", fmt.Errorf("marshal worker tool hook config: %w", err)
	}
	sum := sha256.Sum256(payload)
	runnerPath := filepath.Join(hookDir, "maestro-tool-hook-"+hex.EncodeToString(sum[:8])+".py")
	if err := os.WriteFile(runnerPath, []byte(workerToolHookRunnerPython), 0755); err != nil {
		return "", fmt.Errorf("write worker tool hook runner: %w", err)
	}
	configPath := runnerPath + ".json"
	if err := os.WriteFile(configPath, payload, 0600); err != nil {
		return "", fmt.Errorf("write worker tool hook config: %w", err)
	}
	return runnerPath, nil
}

func writeClaudeToolHookSettings(worktree, runnerPath string, hooks config.HooksConfig) error {
	claudeDir := filepath.Join(worktree, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		return fmt.Errorf("create .claude dir: %w", err)
	}

	settingsPath := filepath.Join(claudeDir, "settings.local.json")
	var settings map[string]any
	if data, err := os.ReadFile(settingsPath); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("parse %s: %w", settingsPath, err)
		}
	} else {
		settings = make(map[string]any)
	}

	hookMap, _ := settings["hooks"].(map[string]any)
	if hookMap == nil {
		hookMap = make(map[string]any)
	}
	if toolHookConfigured(hooks.PreTool) {
		matcher := strings.TrimSpace(hooks.PreTool.Matcher)
		if matcher == "" {
			matcher = defaultPreToolMatcher
		}
		hookMap["PreToolUse"] = appendClaudeHook(hookMap["PreToolUse"], matcher, runnerPath, "pre_tool")
	}
	if toolHookConfigured(hooks.PostEdit) {
		matcher := strings.TrimSpace(hooks.PostEdit.Matcher)
		if matcher == "" {
			matcher = defaultPostEditMatcher
		}
		hookMap["PostToolUse"] = appendClaudeHook(hookMap["PostToolUse"], matcher, runnerPath, "post_edit")
	}
	settings["hooks"] = hookMap

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", settingsPath, err)
	}
	if err := os.WriteFile(settingsPath, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("write %s: %w", settingsPath, err)
	}
	return nil
}

func appendClaudeHook(existing any, matcher, runnerPath, event string) []any {
	items, _ := existing.([]any)
	command := fmt.Sprintf("MAESTRO_WORKER_HOOK_EVENT=%s %s", shellQuote(event), shellQuote(runnerPath))
	entry := map[string]any{
		"matcher": matcher,
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": command,
			},
		},
	}
	for i, item := range items {
		m, ok := item.(map[string]any)
		if !ok || m["matcher"] != matcher {
			continue
		}
		hookItems, _ := m["hooks"].([]any)
		for _, hookItem := range hookItems {
			h, ok := hookItem.(map[string]any)
			if ok && h["command"] == command {
				return items
			}
		}
		m["hooks"] = append(hookItems, entry["hooks"].([]any)...)
		items[i] = m
		return items
	}
	return append(items, entry)
}

func excludeClaudeLocalSettings(worktree string) error {
	out, err := exec.Command("git", "-C", worktree, "rev-parse", "--git-path", "info/exclude").CombinedOutput()
	if err != nil {
		return fmt.Errorf("resolve git exclude path: %w\n%s", err, out)
	}
	excludePath := strings.TrimSpace(string(out))
	if excludePath == "" {
		return fmt.Errorf("empty git exclude path")
	}
	excludePath = filepath.Clean(excludePath)
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(worktree, excludePath)
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0755); err != nil {
		return fmt.Errorf("create git info dir: %w", err)
	}
	existing, _ := os.ReadFile(excludePath)
	line := ".claude/settings.local.json"
	if strings.Contains("\n"+string(existing)+"\n", "\n"+line+"\n") {
		return nil
	}
	f, err := os.OpenFile(excludePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open git exclude: %w", err)
	}
	defer f.Close()
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		if _, err := f.WriteString("\n"); err != nil {
			return fmt.Errorf("write git exclude newline: %w", err)
		}
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		return fmt.Errorf("write git exclude entry: %w", err)
	}
	return nil
}

func workerToolHookPromptSection(hooks config.HooksConfig, backendName string, setup workerToolHookSetup) string {
	if !toolHookConfigured(hooks.PreTool) && !toolHookConfigured(hooks.PostEdit) {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\n---\n\n## Worker Tool Hooks\n\n")
	if setup.Claude {
		b.WriteString("- Maestro installed local Claude Code hooks for this worker session; hook results are fed back into the session automatically.\n")
	} else {
		b.WriteString("- Maestro tool hooks are configured for this project, but this backend does not expose a supported automatic hook installer. Run the matching hook command yourself at the required point and use its output before continuing.\n")
	}
	if toolHookConfigured(hooks.PreTool) {
		b.WriteString(fmt.Sprintf("- Before matching tool calls (`%s`), run: `%s`.\n", effectiveHookMatcher(hooks.PreTool, defaultPreToolMatcher), strings.TrimSpace(hooks.PreTool.Command)))
		if hooks.PreTool.BlockOnFailure {
			b.WriteString("- A non-zero pre-tool hook result blocks the tool call; correct the reported problem before retrying.\n")
		}
	}
	if toolHookConfigured(hooks.PostEdit) {
		b.WriteString(fmt.Sprintf("- After file edit tools (`%s`), run: `%s`.\n", effectiveHookMatcher(hooks.PostEdit, defaultPostEditMatcher), strings.TrimSpace(hooks.PostEdit.Command)))
		if hooks.PostEdit.BlockOnFailure {
			b.WriteString("- A non-zero post-edit hook result must be corrected before making more changes.\n")
		}
	}
	if setup.RunnerPath != "" {
		b.WriteString(fmt.Sprintf("- Hook runner: `%s`.\n", setup.RunnerPath))
	}
	return b.String()
}

func effectiveHookMatcher(hook config.ToolHookConfig, fallback string) string {
	if strings.TrimSpace(hook.Matcher) != "" {
		return strings.TrimSpace(hook.Matcher)
	}
	return fallback
}

const workerToolHookRunnerPython = `#!/usr/bin/env python3
import json
import os
import subprocess
import sys

MAX_CONTEXT = 9000

def load_config():
    with open(sys.argv[0] + ".json", "r", encoding="utf-8") as f:
        return json.load(f)

def trim(text):
    if len(text) <= MAX_CONTEXT:
        return text
    return text[:MAX_CONTEXT] + "\n... [maestro hook output truncated]"

def event_name():
    return os.environ.get("MAESTRO_WORKER_HOOK_EVENT", "").strip()

def hook_for(cfg, event):
    if event == "pre_tool":
        return cfg.get("pre_tool") or {}
    if event == "post_edit":
        return cfg.get("post_edit") or {}
    return {}

def claude_event(event):
    if event == "pre_tool":
        return "PreToolUse"
    if event == "post_edit":
        return "PostToolUse"
    return "PostToolUse"

def emit_context(event, message, block=False):
    payload = {
        "hookSpecificOutput": {
            "hookEventName": claude_event(event),
            "additionalContext": trim(message),
        }
    }
    if block:
        if event == "pre_tool":
            payload["hookSpecificOutput"]["permissionDecision"] = "deny"
            payload["hookSpecificOutput"]["permissionDecisionReason"] = trim(message)
        else:
            payload["decision"] = "block"
            payload["reason"] = trim(message)
    print(json.dumps(payload))

def main():
    cfg = load_config()
    event = event_name()
    hook = hook_for(cfg, event)
    command = (hook.get("command") or "").strip()
    if not command:
        return 0

    timeout = int(cfg.get("timeout_ms") or 60000) / 1000
    raw_input = sys.stdin.read()
    env = os.environ.copy()
    env["MAESTRO_HOOK_EVENT"] = event
    env["MAESTRO_HOOK_INPUT"] = raw_input
    try:
        input_data = json.loads(raw_input or "{}")
        env["MAESTRO_HOOK_TOOL_NAME"] = str(input_data.get("tool_name") or "")
        tool_input = input_data.get("tool_input") or {}
        if isinstance(tool_input, dict):
            file_path = tool_input.get("file_path") or tool_input.get("path") or ""
            if file_path:
                env["MAESTRO_HOOK_FILE_PATH"] = str(file_path)
    except Exception:
        pass

    try:
        proc = subprocess.run(
            ["bash", "-c", command],
            text=True,
            capture_output=True,
            timeout=timeout,
            env=env,
        )
        output = ""
        if proc.stdout:
            output += "stdout:\n" + proc.stdout.rstrip() + "\n"
        if proc.stderr:
            output += "stderr:\n" + proc.stderr.rstrip() + "\n"
        if not output:
            output = "(no stdout/stderr)"
        status = "passed" if proc.returncode == 0 else "failed"
        message = f"Maestro {event} hook {status} (exit {proc.returncode}): {command}\n\n{output}".rstrip()
        emit_context(event, message, block=proc.returncode != 0 and bool(hook.get("block_on_failure")))
        return 0
    except subprocess.TimeoutExpired as exc:
        output = ""
        if exc.stdout:
            output += "stdout:\n" + exc.stdout.rstrip() + "\n"
        if exc.stderr:
            output += "stderr:\n" + exc.stderr.rstrip() + "\n"
        message = f"Maestro {event} hook timed out after {timeout:.0f}s: {command}\n\n{output}".rstrip()
        emit_context(event, message, block=bool(hook.get("block_on_failure")))
        return 0

if __name__ == "__main__":
    raise SystemExit(main())
`
