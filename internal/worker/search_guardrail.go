package worker

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/befeast/maestro/internal/state"
)

var searchGuardedCommands = []string{"rg", "find", "grep"}

// workerCredentialEnvKeys are the provider credential env vars the worker
// harness needs to reach CLIProxyAPI / upstream APIs. The daemon inherits them
// from its own private credential boundary (an operator-managed systemd
// EnvironmentFile / MAESTRO_WORKER_CREDENTIALS_FILE), but `tmux new-session`
// does not propagate them into a fresh worker session by default (#822). They
// MUST NOT be inlined as literal values into the world-readable runner script,
// nor copied into a per-worker state file (#888). Instead the runner sources a
// single authoritative private (0600) env file *by reference* at exec time, so a
// plain `cat` of a worker `*-run.sh` never discloses a secret and no secret is
// duplicated per slot.
var workerCredentialEnvKeys = []string{
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_AUTH_TOKEN",
	"CLIPROXY_API_KEY",
	"OPENAI_BASE_URL",
	"OPENAI_API_KEY",
}

// workerCredentialSecretKeys is the subset of workerCredentialEnvKeys whose
// values are secret (tokens/keys, not the plainly-public base URLs). Only these
// are treated as canary/redaction targets when scrubbing legacy artifacts.
var workerCredentialSecretKeys = []string{
	"ANTHROPIC_AUTH_TOKEN",
	"CLIPROXY_API_KEY",
	"OPENAI_API_KEY",
}

const (
	// workerRunnerScriptMode keeps the generated runner owner-only (no
	// group/other read). The runner references the private creds file by path,
	// but tightening it anyway keeps the state dir's blast radius small (#888).
	workerRunnerScriptMode = 0o700
	// workerCredentialsFileMode is the private env-file boundary: owner
	// read/write only (#888 acceptance: mode 0600 or stronger).
	workerCredentialsFileMode = 0o600
	// workerCredentialsDirMode keeps the fallback credential directory
	// owner-only so no other local account can enumerate or read it.
	workerCredentialsDirMode = 0o700

	// workerCredentialsFileEnvVar lets an operator point the worker at an
	// externally-owned private credential boundary (e.g. the systemd
	// EnvironmentFile that replaces literal Environment= secrets). When it is set
	// and passes validation, maestro sources it by reference and writes no
	// credential file of its own.
	workerCredentialsFileEnvVar = "MAESTRO_WORKER_CREDENTIALS_FILE"

	// workerCredentialsDirName / workerCredentialsFileName locate the single
	// authoritative fallback boundary maestro maintains under the state dir when
	// no operator-provided file is configured. Exactly one file per daemon/state
	// dir — never one per worker slot (#888).
	workerCredentialsDirName  = "credentials"
	workerCredentialsFileName = "worker-proxy.env"

	// credentialRedactionPlaceholder replaces a secret value scrubbed out of a
	// write-once artifact (e.g. a prompt file). It never encodes the value.
	credentialRedactionPlaceholder = "«maestro-redacted-credential»"
)

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

// streamSplit configures the worker runner to route a backend's structured
// NDJSON stream through `maestro stream-split` before tee: the raw frames are
// appended to JSONLPath (parsed for usage) while the rendered human-readable
// text flows to slot.log (#737). A nil *streamSplit keeps the plain `tee`
// pipeline — also the degradation path when the maestro binary is unresolvable.
type streamSplit struct {
	MaestroBin string // path to the maestro binary providing the stream-split subcommand
	Backend    string // backend kind for rendering (e.g. "claude")
	JSONLPath  string // side-channel file for raw NDJSON frames (slot.jsonl)
}

// logPipeline builds the trailing `| ... | tee -a LOG` stage. When split is
// set it inserts `| maestro stream-split --backend B --jsonl J` ahead of tee
// so the raw stream lands in slot.jsonl and slot.log stays human-readable.
func logPipeline(split *streamSplit, logFile string) string {
	tee := "tee -a " + shellQuote(logFile)
	if split == nil {
		return "2>&1 | " + tee
	}
	splitter := shellJoin([]string{
		split.MaestroBin, "stream-split",
		"--backend", split.Backend,
		"--jsonl", split.JSONLPath,
	})
	return "2>&1 | " + splitter + " | " + tee
}

func buildWorkerRunnerScript(args []string, stdinFile, logFile, worktree, guardDir, credsFile string, split *streamSplit) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("export MAESTRO_WORKTREE=" + shellQuote(worktree) + "\n")
	b.WriteString("export MAESTRO_SEARCH_GUARDRAIL_DIR=" + shellQuote(guardDir) + "\n")
	b.WriteString("export MAESTRO_ORIGINAL_PATH=\"${PATH:-}\"\n")
	b.WriteString("export PATH=\"$MAESTRO_SEARCH_GUARDRAIL_DIR:$MAESTRO_ORIGINAL_PATH\"\n")
	// Provider credentials are sourced at exec time from the single authoritative
	// private (0600) credential file so this script holds only a reference, never
	// secret values, and no secret is copied per worker slot (#888). The daemon
	// inherits the credentials from its own private boundary; tmux new-session
	// does not propagate them by default (#822), so sourcing the shared file
	// bridges them into the worker process without persisting them here.
	if credsFile != "" {
		b.WriteString("[ -r " + shellQuote(credsFile) + " ] && . " + shellQuote(credsFile) + "\n")
	}
	b.WriteString("cd \"$MAESTRO_WORKTREE\" || exit 1\n")
	b.WriteString("printf '[maestro] worker worktree: %s\\n' \"$MAESTRO_WORKTREE\" | tee -a " + shellQuote(logFile) + "\n")
	pipeline := logPipeline(split, logFile)
	if stdinFile != "" {
		b.WriteString(fmt.Sprintf("exec %s < %s %s\n", shellJoin(args), shellQuote(stdinFile), pipeline))
	} else {
		b.WriteString(fmt.Sprintf("exec %s %s\n", shellJoin(args), pipeline))
	}
	return b.String()
}

func writeWorkerRunnerScript(stateDir, runnerPath string, args []string, stdinFile, logFile, worktree string, split *streamSplit) error {
	guardDir, err := ensureSearchGuardrailWrappers(stateDir)
	if err != nil {
		return err
	}
	// Resolve the single authoritative private credential boundary the runner
	// sources at exec time. Never a per-worker path (#888): every slot references
	// the same service-level file, so a secret is never duplicated per worker.
	credsFile, err := resolveWorkerCredentialsFile(stateDir)
	if err != nil {
		return err
	}
	runnerContent := buildWorkerRunnerScript(args, stdinFile, logFile, worktree, guardDir, credsFile, split)
	if err := os.WriteFile(runnerPath, []byte(runnerContent), workerRunnerScriptMode); err != nil {
		return fmt.Errorf("write runner script: %w", err)
	}
	// os.WriteFile only applies the mode when it creates the file; repair the
	// permissions on an existing (possibly pre-#888 world-readable) script so an
	// in-place upgrade drops group/other read.
	if err := os.Chmod(runnerPath, workerRunnerScriptMode); err != nil {
		return fmt.Errorf("chmod runner script: %w", err)
	}
	return nil
}

// resolveWorkerCredentialsFile returns the single authoritative private path the
// worker runner should source at exec time, or "" when no provider credential is
// available. It never returns a per-worker path (#888):
//
//   - If MAESTRO_WORKER_CREDENTIALS_FILE is set, that operator-owned file is the
//     authoritative boundary (e.g. the systemd EnvironmentFile that replaced the
//     literal Environment= secrets). It is validated — regular file, no
//     group/other access, owned by us, private parent dir — and referenced
//     as-is; maestro writes nothing. A misconfigured (world/group readable,
//     symlinked, or foreign-owned) target is a hard error so a spawn fails
//     closed rather than sourcing an insecure secret file.
//   - Otherwise the daemon's own environment is the source. maestro materializes
//     a single authoritative file at <stateDir>/credentials/worker-proxy.env
//     (atomically, mode 0600, inside a validated owner-only dir) and returns it,
//     or removes any stale copy and returns "" when no credential is present.
func resolveWorkerCredentialsFile(stateDir string) (string, error) {
	if operator := strings.TrimSpace(os.Getenv(workerCredentialsFileEnvVar)); operator != "" {
		if err := validatePrivateCredentialFile(operator); err != nil {
			// Do not echo the path value; the env-var name is enough to diagnose.
			return "", fmt.Errorf("%s is not a private credential boundary: %w", workerCredentialsFileEnvVar, err)
		}
		return operator, nil
	}
	return maintainAuthoritativeCredentialsFile(stateDir)
}

// maintainAuthoritativeCredentialsFile keeps the single fallback credential
// boundary in sync with the daemon environment and returns its path, or "" when
// no credential is set. The file is written atomically at mode 0600 inside an
// owner-only directory; a rotation that clears the daemon env removes the file
// so no on-disk copy survives (lifecycle cleanup).
func maintainAuthoritativeCredentialsFile(stateDir string) (string, error) {
	if strings.TrimSpace(stateDir) == "" {
		return "", fmt.Errorf("empty state dir")
	}
	dir := filepath.Join(stateDir, workerCredentialsDirName)
	target := filepath.Join(dir, workerCredentialsFileName)

	var b strings.Builder
	present := false
	for _, key := range workerCredentialEnvKeys {
		if v := os.Getenv(key); v != "" {
			b.WriteString(fmt.Sprintf("export %s=%s\n", key, shellQuote(v)))
			present = true
		}
	}
	if !present {
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("remove stale worker credentials file: %w", err)
		}
		return "", nil
	}
	if err := ensurePrivateDir(dir); err != nil {
		return "", err
	}
	if err := writeFileAtomicPrivate(dir, target, b.String()); err != nil {
		return "", err
	}
	if err := validatePrivateCredentialFile(target); err != nil {
		return "", fmt.Errorf("worker credentials file failed post-write validation: %w", err)
	}
	return target, nil
}

// ensurePrivateDir creates dir owner-only and rejects a symlink, a
// foreign-owned dir, or one with any group/other bit so an unprivileged local
// account cannot pre-plant a link or a loose-mode directory under our path.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, workerCredentialsDirMode); err != nil {
		return fmt.Errorf("create private dir: %w", err)
	}
	// MkdirAll honors the umask and is a no-op on an existing dir; force the mode
	// then verify the on-disk object is really our private directory.
	if err := os.Chmod(dir, workerCredentialsDirMode); err != nil {
		return fmt.Errorf("chmod private dir: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("lstat private dir: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("private dir %s is not a regular directory", filepath.Base(dir))
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("private dir %s is group/other accessible", filepath.Base(dir))
	}
	return checkOwnedByUs(dir, info)
}

// writeFileAtomicPrivate writes content to target via a fresh O_EXCL temp file
// in the same (already-validated) directory, then renames it into place. The
// random exclusive temp name defeats a pre-planted symlink at the temp path, and
// rename operates on the name so a symlink planted at target is replaced rather
// than followed.
func writeFileAtomicPrivate(dir, target, content string) error {
	tmp, err := os.CreateTemp(dir, ".worker-proxy-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp credentials file: %w", err)
	}
	tmpPath := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Chmod(workerCredentialsFileMode); err != nil {
		return fail(fmt.Errorf("chmod temp credentials file: %w", err))
	}
	if _, err := tmp.WriteString(content); err != nil {
		return fail(fmt.Errorf("write temp credentials file: %w", err))
	}
	if err := tmp.Sync(); err != nil {
		return fail(fmt.Errorf("sync temp credentials file: %w", err))
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp credentials file: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename credentials file: %w", err)
	}
	return nil
}

// validatePrivateCredentialFile asserts path is a real owner-only file suitable
// to hold secrets: a regular file (not a symlink), no group/other permission
// bits, owned by the running user, in a parent dir that is not group/other
// writable.
func validatePrivateCredentialFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("is a symlink")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("is group/other accessible (mode %o)", info.Mode().Perm())
	}
	if err := checkOwnedByUs(path, info); err != nil {
		return err
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("stat parent: %w", err)
	}
	if parent.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("parent dir is group/other writable")
	}
	return nil
}

// checkOwnedByUs verifies the object is owned by the running uid.
func checkOwnedByUs(name string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot determine ownership", filepath.Base(name))
	}
	if int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("%s: owned by uid %d, not the maestro user", filepath.Base(name), stat.Uid)
	}
	return nil
}

// ScrubLegacyRunArtifacts inventories and neutralizes provider-credential
// material a pre-#888 (or attempt-0) daemon left in the state dir, then repairs
// permissions. It performs only file-content and mode operations — it never
// reads, kills, or signals a worker process, so it preserves active workers and
// cannot repeat the #877 control-group kill (an already-exec'd runner does not
// re-read its script). Best-effort: a missing dir/file is a no-op and a per-file
// error is logged and skipped so one bad file cannot abort the pass.
//
//   - Legacy per-worker `*-run.env` files (raw credential copies, including the
//     stale earlier-slot copies attempt-0 left behind) are removed outright.
//   - Legacy `*-run.sh` scripts that inlined `export <CRED>=...` values or
//     sourced a per-worker `*-run.env` are rewritten with those lines dropped,
//     scrubbing the on-disk value, then chmod'd owner-only. Remaining scripts
//     are just chmod'd owner-only.
//   - `*-prompt.md` files (written once, never appended) are redacted of any
//     currently-known secret value in place.
//   - `*.log` files (appended by live workers; unsafe to rewrite) are only
//     inventoried — a count of files still holding a current secret value is
//     logged so an operator can scrub them under retention rules, never the
//     values themselves.
func ScrubLegacyRunArtifacts(stateDir string) {
	if strings.TrimSpace(stateDir) == "" {
		return
	}

	if matches, err := filepath.Glob(filepath.Join(stateDir, "*-run.env")); err == nil {
		for _, path := range matches {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				log.Printf("[worker] scrub stale creds %s: %v", filepath.Base(path), err)
			}
		}
	}

	if matches, err := filepath.Glob(filepath.Join(stateDir, "*-run.sh")); err == nil {
		for _, path := range matches {
			scrubRunnerScript(path)
		}
	}

	secrets := currentCredentialSecretValues()
	if len(secrets) == 0 {
		return
	}
	if matches, err := filepath.Glob(filepath.Join(stateDir, "*-prompt.md")); err == nil {
		for _, path := range matches {
			redactSecretsInFile(path, secrets)
		}
	}
	inventorySecretsInLogs(state.LogDir(stateDir), secrets)
}

// scrubRunnerScript drops any inlined credential export or stale per-worker env
// sourcing from a legacy runner script and repairs its mode. The rewrite touches
// only the file on disk; a worker that already exec'd this script is unaffected.
func scrubRunnerScript(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	kept := make([]string, 0, len(lines))
	changed := false
	for _, line := range lines {
		if isCredentialExportLine(line) || isStalePerWorkerEnvSourceLine(line) {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	if changed {
		if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), workerRunnerScriptMode); err != nil {
			log.Printf("[worker] scrub runner %s: %v", filepath.Base(path), err)
			return
		}
	}
	if err := os.Chmod(path, workerRunnerScriptMode); err != nil {
		log.Printf("[worker] repair perms %s: %v", filepath.Base(path), err)
	}
}

// isCredentialExportLine reports whether a script line inlines a provider
// credential value, e.g. `export ANTHROPIC_AUTH_TOKEN='...'`.
func isCredentialExportLine(line string) bool {
	t := strings.TrimSpace(line)
	for _, key := range workerCredentialEnvKeys {
		if strings.HasPrefix(t, "export "+key+"=") {
			return true
		}
	}
	return false
}

// isStalePerWorkerEnvSourceLine reports whether a script line sources a legacy
// per-worker `*-run.env` file (removed by this scrub). The go-forward runner
// sources `worker-proxy.env`, which does not match and is preserved.
func isStalePerWorkerEnvSourceLine(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.Contains(t, "-run.env") {
		return false
	}
	return strings.HasPrefix(t, ". ") || strings.Contains(t, "] && . ")
}

// currentCredentialSecretValues returns the non-trivial secret values currently
// present in the daemon environment, used to redact/inventory legacy artifacts.
// Base URLs are excluded (not secret); short values are ignored to avoid
// over-matching.
func currentCredentialSecretValues() []string {
	var out []string
	for _, key := range workerCredentialSecretKeys {
		if v := os.Getenv(key); len(v) >= 8 {
			out = append(out, v)
		}
	}
	return out
}

// redactSecretsInFile replaces exact secret-value occurrences in a write-once
// file with a fixed placeholder, preserving the file's mode. A no-op when the
// file holds no known secret.
func redactSecretsInFile(path string, secrets []string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	content := string(data)
	replaced := content
	for _, s := range secrets {
		replaced = strings.ReplaceAll(replaced, s, credentialRedactionPlaceholder)
	}
	if replaced == content {
		return
	}
	mode := os.FileMode(workerCredentialsFileMode)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(path, []byte(replaced), mode); err != nil {
		log.Printf("[worker] redact %s: %v", filepath.Base(path), err)
	}
}

// inventorySecretsInLogs counts (never prints) worker log files that still hold
// a current secret value and logs a single warning so an operator can scrub them
// under retention rules. Logs are appended by live workers, so rewriting them
// here would race the writer; inventory-only keeps the pass safe.
func inventorySecretsInLogs(logDir string, secrets []string) {
	matches, err := filepath.Glob(filepath.Join(logDir, "*.log"))
	if err != nil {
		return
	}
	count := 0
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := string(data)
		for _, s := range secrets {
			if strings.Contains(text, s) {
				count++
				break
			}
		}
	}
	if count > 0 {
		log.Printf("[worker] WARNING: %d worker log file(s) still contain a current provider credential value; scrub or rotate under retention rules (#888)", count)
	}
}

func workerSearchSafetyPromptSection(worktreePath string) string {
	return fmt.Sprintf("\n\n---\n\n## Worker Search Safety\n\n"+
		"- The assigned worktree is `%s`; use it as the current directory before running code search commands.\n"+
		"- Do NOT run `rg`, `find`, or `grep` from broad filesystem roots such as `/`, `/mnt`, or `/home`.\n"+
		"- If you intentionally need a broad host search, set `MAESTRO_ALLOW_BROAD_SEARCH=1` for that single command.\n",
		worktreePath)
}
