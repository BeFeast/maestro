package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var searchGuardedCommands = []string{"rg", "find", "grep"}

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

maestro_reject_broad_arg() {
  if maestro_broad_path "$1" && ! maestro_path_inside_worktree "$1"; then
    echo "[maestro] search guardrail: $cmd was given a broad filesystem path; search the assigned worktree instead: $MAESTRO_WORKTREE" >&2
    return 2
  fi
  return 0
}

maestro_check_rg_args() {
  rg_after_options=0
  rg_paths_only=0
  rg_saw_pattern=0
  rg_skip_next=0

  for arg in "$@"; do
    if [ "$rg_skip_next" = "1" ]; then
      rg_skip_next=0
      continue
    fi

    if [ "$rg_after_options" = "0" ]; then
      case "$arg" in
        --)
          rg_after_options=1
          continue
          ;;
        --files)
          rg_paths_only=1
          continue
          ;;
        --regexp|--file)
          rg_saw_pattern=1
          rg_skip_next=1
          continue
          ;;
        --regexp=*|--file=*)
          rg_saw_pattern=1
          continue
          ;;
        --after-context|--before-context|--color|--colors|--context|--context-separator|--dfa-size-limit|--encoding|--engine|--field-context-separator|--field-match-separator|--glob|--hostname-bin|--hyperlink-format|--iglob|--ignore-file|--max-columns|--max-count|--max-depth|--max-filesize|--path-separator|--pre|--pre-glob|--regex-size-limit|--replace|--sort|--sortr|--threads|--type|--type-add|--type-clear|--type-not)
          rg_skip_next=1
          continue
          ;;
        --*=*)
          continue
          ;;
        -e|-f)
          rg_saw_pattern=1
          rg_skip_next=1
          continue
          ;;
        -e?*|-f?*)
          rg_saw_pattern=1
          continue
          ;;
        -A|-B|-C|-E|-g|-j|-m|-M|-r|-t|-T)
          rg_skip_next=1
          continue
          ;;
        -A?*|-B?*|-C?*|-E?*|-g?*|-j?*|-m?*|-M?*|-r?*|-t?*|-T?*)
          continue
          ;;
        -*)
          continue
          ;;
      esac
    fi

    if [ "$rg_paths_only" = "1" ]; then
      maestro_reject_broad_arg "$arg" || return $?
      continue
    fi
    if [ "$rg_saw_pattern" = "0" ]; then
      rg_saw_pattern=1
      continue
    fi
    maestro_reject_broad_arg "$arg" || return $?
  done
  return 0
}

maestro_check_grep_args() {
  grep_after_options=0
  grep_saw_pattern=0
  grep_skip_next=0

  for arg in "$@"; do
    if [ "$grep_skip_next" = "1" ]; then
      grep_skip_next=0
      continue
    fi

    if [ "$grep_after_options" = "0" ]; then
      case "$arg" in
        --)
          grep_after_options=1
          continue
          ;;
        --regexp|--file)
          grep_saw_pattern=1
          grep_skip_next=1
          continue
          ;;
        --regexp=*|--file=*)
          grep_saw_pattern=1
          continue
          ;;
        --after-context|--before-context|--binary-files|--context|--devices|--directories|--exclude|--exclude-dir|--exclude-from|--group-separator|--include|--label|--max-count)
          grep_skip_next=1
          continue
          ;;
        --*=*)
          continue
          ;;
        -e|-f)
          grep_saw_pattern=1
          grep_skip_next=1
          continue
          ;;
        -e?*|-f?*)
          grep_saw_pattern=1
          continue
          ;;
        -A|-B|-C|-D|-d|-m)
          grep_skip_next=1
          continue
          ;;
        -A?*|-B?*|-C?*|-D?*|-d?*|-m?*)
          continue
          ;;
        -*)
          continue
          ;;
      esac
    fi

    if [ "$grep_saw_pattern" = "0" ]; then
      grep_saw_pattern=1
      continue
    fi
    maestro_reject_broad_arg "$arg" || return $?
  done
  return 0
}

maestro_check_find_args() {
  find_skip_next=0

  for arg in "$@"; do
    if [ "$find_skip_next" = "1" ]; then
      find_skip_next=0
      continue
    fi

    case "$arg" in
      --|-H|-L|-P)
        continue
        ;;
      -D|-O)
        find_skip_next=1
        continue
        ;;
      -D?*|-O?*)
        continue
        ;;
      -*)
        return 0
        ;;
      '!'|'('|')'|',')
        return 0
        ;;
    esac

    maestro_reject_broad_arg "$arg" || return $?
  done
  return 0
}

if [ -z "${MAESTRO_ALLOW_BROAD_SEARCH:-}" ]; then
  case "$cmd" in
    rg) maestro_check_rg_args "$@" || exit $? ;;
    grep) maestro_check_grep_args "$@" || exit $? ;;
    find) maestro_check_find_args "$@" || exit $? ;;
  esac

  if ! maestro_inside_worktree && maestro_broad_path "$PWD"; then
    echo "[maestro] search guardrail: $cmd was launched from a broad filesystem root; run it from the assigned worktree instead: $MAESTRO_WORKTREE" >&2
    exit 2
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
