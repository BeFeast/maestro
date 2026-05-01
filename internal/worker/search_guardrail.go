package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type searchGuardrailReason string

const (
	searchGuardrailNone     searchGuardrailReason = "none"
	searchGuardrailBroadCWD searchGuardrailReason = "broad_cwd"
	searchGuardrailBroadArg searchGuardrailReason = "broad_arg"
	searchGuardrailAllowed  searchGuardrailReason = "allowed"
)

type searchGuardrailDecision struct {
	Warn   bool
	Reason searchGuardrailReason
}

var searchGuardedCommands = []string{"rg", "find", "grep"}

func classifySearchGuardrail(command, cwd, worktree string, args []string, allowBroad bool) searchGuardrailDecision {
	if !isGuardedSearchCommand(command) {
		return searchGuardrailDecision{Reason: searchGuardrailNone}
	}
	if allowBroad {
		return searchGuardrailDecision{Reason: searchGuardrailAllowed}
	}
	if isBroadSearchPath(cwd, worktree) {
		return searchGuardrailDecision{Warn: true, Reason: searchGuardrailBroadCWD}
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if isBroadSearchPath(arg, worktree) {
			return searchGuardrailDecision{Warn: true, Reason: searchGuardrailBroadArg}
		}
	}
	return searchGuardrailDecision{Reason: searchGuardrailNone}
}

func isGuardedSearchCommand(command string) bool {
	name := filepath.Base(strings.TrimSpace(command))
	for _, guarded := range searchGuardedCommands {
		if name == guarded {
			return true
		}
	}
	return false
}

func isBroadSearchPath(path, worktree string) bool {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	path = filepath.Clean(path)
	if isWithinPath(path, worktree) {
		return false
	}

	switch path {
	case "/", "/mnt", "/home", "/Users", "/tmp", "/var", "/opt", "/usr", "/etc", "/proc", "/sys", "/dev":
		return true
	}
	for _, prefix := range []string{"/mnt/", "/home/", "/Users/", "/tmp/", "/var/", "/opt/", "/usr/", "/etc/", "/proc/", "/sys/", "/dev/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func isWithinPath(path, root string) bool {
	root = strings.TrimSpace(root)
	if root == "" {
		return false
	}
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	return true
}

func ensureSearchGuardrailWrappers(stateDir string) (string, error) {
	if strings.TrimSpace(stateDir) == "" {
		return "", fmt.Errorf("empty state dir")
	}
	guardDir := filepath.Join(stateDir, "search-guardrails")
	if err := os.MkdirAll(guardDir, 0755); err != nil {
		return "", fmt.Errorf("create search guardrail dir: %w", err)
	}
	for _, name := range searchGuardedCommands {
		path := filepath.Join(guardDir, name)
		if err := os.WriteFile(path, []byte(searchGuardrailWrapperScript), 0755); err != nil {
			return "", fmt.Errorf("write search guardrail wrapper %s: %w", name, err)
		}
	}
	return guardDir, nil
}

const searchGuardrailWrapperScript = `#!/bin/sh
cmd=${0##*/}
real_path=$(PATH="${MAESTRO_ORIGINAL_PATH:-$PATH}" command -v "$cmd" 2>/dev/null)
if [ -z "$real_path" ]; then
  echo "[maestro] search guardrail: unable to locate real $cmd" >&2
  exit 127
fi

maestro_inside_worktree() {
  [ -n "${MAESTRO_WORKTREE:-}" ] || return 1
  case "$PWD" in
    "$MAESTRO_WORKTREE"|"$MAESTRO_WORKTREE"/*) return 0 ;;
  esac
  return 1
}

maestro_path_inside_worktree() {
  [ -n "${MAESTRO_WORKTREE:-}" ] || return 1
  case "$1" in
    "$MAESTRO_WORKTREE"|"$MAESTRO_WORKTREE"/*) return 0 ;;
  esac
  return 1
}

maestro_broad_path() {
  case "$1" in
    /|/mnt|/mnt/*|/home|/home/*|/Users|/Users/*|/tmp|/tmp/*|/var|/var/*|/opt|/opt/*|/usr|/usr/*|/etc|/etc/*|/proc|/proc/*|/sys|/sys/*|/dev|/dev/*) return 0 ;;
  esac
  return 1
}

if [ -z "${MAESTRO_ALLOW_BROAD_SEARCH:-}" ]; then
  for arg in "$@"; do
    case "$arg" in
      -*) continue ;;
    esac
    if maestro_broad_path "$arg" && ! maestro_path_inside_worktree "$arg"; then
      echo "[maestro] search guardrail: $cmd was given a broad filesystem path; search the assigned worktree instead: $MAESTRO_WORKTREE" >&2
      exit 2
    fi
  done

  if ! maestro_inside_worktree && maestro_broad_path "$PWD"; then
    echo "[maestro] search guardrail: $cmd was launched from a broad filesystem root; rerunning from worktree: $MAESTRO_WORKTREE" >&2
    cd "$MAESTRO_WORKTREE" || exit 1
  fi
fi

exec "$real_path" "$@"
`

func buildWorkerRunnerScript(args []string, stdinFile, logFile, worktree, guardDir string) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("export MAESTRO_WORKTREE=" + shellQuote(worktree) + "\n")
	b.WriteString("export MAESTRO_SEARCH_GUARDRAIL_DIR=" + shellQuote(guardDir) + "\n")
	b.WriteString("export MAESTRO_ORIGINAL_PATH=\"${PATH:-}\"\n")
	b.WriteString("export PATH=\"$MAESTRO_SEARCH_GUARDRAIL_DIR:$MAESTRO_ORIGINAL_PATH\"\n")
	b.WriteString("cd \"$MAESTRO_WORKTREE\" || exit 1\n")
	b.WriteString("printf '[maestro] worker worktree: %s\\n' \"$MAESTRO_WORKTREE\" | tee -a " + shellQuote(logFile) + "\n")
	if stdinFile != "" {
		b.WriteString(fmt.Sprintf("exec %s < %s 2>&1 | tee -a %s\n", shellJoin(args), shellQuote(stdinFile), shellQuote(logFile)))
	} else {
		b.WriteString(fmt.Sprintf("exec %s 2>&1 | tee -a %s\n", shellJoin(args), shellQuote(logFile)))
	}
	return b.String()
}

func writeWorkerRunnerScript(stateDir, runnerPath string, args []string, stdinFile, logFile, worktree string) error {
	guardDir, err := ensureSearchGuardrailWrappers(stateDir)
	if err != nil {
		return err
	}
	runnerContent := buildWorkerRunnerScript(args, stdinFile, logFile, worktree, guardDir)
	if err := os.WriteFile(runnerPath, []byte(runnerContent), 0755); err != nil {
		return fmt.Errorf("write runner script: %w", err)
	}
	return nil
}

func workerSearchSafetyPromptSection(worktreePath string) string {
	return fmt.Sprintf("\n\n---\n\n## Worker Search Safety\n\n"+
		"- The assigned worktree is `%s`; use it as the current directory before running code search commands.\n"+
		"- Do NOT run `rg`, `find`, or `grep` from broad filesystem roots such as `/`, `/mnt`, or `/home`.\n"+
		"- If you intentionally need a broad host search, set `MAESTRO_ALLOW_BROAD_SEARCH=1` for that single command.\n",
		worktreePath)
}
