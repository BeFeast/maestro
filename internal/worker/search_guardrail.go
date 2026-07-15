package worker

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var searchGuardedCommands = []string{"rg", "find", "grep"}

// workerCredentialEnvKeys are the provider credential env vars the worker
// harness needs to reach CLIProxyAPI / upstream APIs. The daemon inherits them
// from its own private credential boundary (an operator-managed systemd
// EnvironmentFile / MAESTRO_WORKER_CREDENTIALS_FILE), but `tmux new-session`
// does not propagate them into a fresh worker session by default (#822). They
// MUST NOT be inlined into the runner script or copied into a per-worker state
// file (#888). Instead an internal exec helper reads one authoritative private
// (0600) service file and injects its allow-listed values only into the worker
// process at exec time. A plain `cat` of `*-run.sh` exposes names/references only.
var workerCredentialEnvKeys = []string{
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"CLIPROXY_API_KEY",
	"GEMINI_API_KEY",
	"OPENAI_BASE_URL",
	"OPENAI_API_KEY",
}

// workerCredentialSecretKeys is the subset of workerCredentialEnvKeys whose
// values are secret (tokens/keys, not the plainly-public base URLs). Only these
// are treated as canary/redaction targets when scrubbing legacy artifacts.
var workerCredentialSecretKeys = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"CLIPROXY_API_KEY",
	"GEMINI_API_KEY",
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
	// workerCredentialsDirMode is the required owner-only directory mode used by
	// tests/runbook examples for the operator-managed boundary.
	workerCredentialsDirMode = 0o700

	// workerCredentialsFileEnvVar lets an operator point the worker at an
	// externally-owned private credential boundary (e.g. the systemd
	// EnvironmentFile that replaces literal Environment= secrets). When it is set
	// and passes validation, maestro references it and writes no
	// credential file of its own.
	workerCredentialsFileEnvVar = "MAESTRO_WORKER_CREDENTIALS_FILE"

	// workerCredentialsDirName / workerCredentialsFileName identify the legacy
	// project-local boundary created by an earlier #888 implementation attempt.
	// Startup reconciliation removes this copy. New workers require the explicit
	// service-level boundary named by workerCredentialsFileEnvVar and never write
	// credentials below a project state directory.
	workerCredentialsDirName  = "credentials"
	workerCredentialsFileName = "worker-proxy.env"

	// workerCredentialFileMaxBytes bounds the private service credential file.
	// Provider environment files are tiny; rejecting an unexpectedly large file
	// avoids turning worker startup or output filtering into an unbounded read.
	workerCredentialFileMaxBytes = 1 << 20
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

func buildWorkerRunnerScript(args []string, stdinFile, logFile, worktree, guardDir, credsFile, maestroBin string, split *streamSplit) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("export MAESTRO_WORKTREE=" + shellQuote(worktree) + "\n")
	b.WriteString("export MAESTRO_SEARCH_GUARDRAIL_DIR=" + shellQuote(guardDir) + "\n")
	b.WriteString("export MAESTRO_ORIGINAL_PATH=\"${PATH:-}\"\n")
	b.WriteString("export PATH=\"$MAESTRO_SEARCH_GUARDRAIL_DIR:$MAESTRO_ORIGINAL_PATH\"\n")
	// A long-lived tmux server can retain stale provider values. Remove every
	// known credential from the runner shell and its logging pipeline. The
	// internal _worker-exec helper opens and validates the single private file at
	// exec time, then injects only the allow-listed values into the worker process.
	// This script therefore contains only names and a private path reference.
	for _, key := range workerCredentialEnvKeys {
		b.WriteString("unset " + key + "\n")
	}
	b.WriteString("unset " + workerCredentialsFileEnvVar + "\n")
	b.WriteString("cd \"$MAESTRO_WORKTREE\" || exit 1\n")
	b.WriteString("printf '[maestro] worker worktree: %s\\n' \"$MAESTRO_WORKTREE\" | tee -a " + shellQuote(logFile) + "\n")
	workerExec := []string{maestroBin, "_worker-exec"}
	if credsFile != "" {
		workerExec = append(workerExec, "--credentials-file", credsFile)
	}
	workerExec = append(workerExec, "--")
	workerExec = append(workerExec, args...)
	pipeline := logPipeline(split, logFile)
	if stdinFile != "" {
		b.WriteString(fmt.Sprintf("exec %s < %s %s\n", shellJoin(workerExec), shellQuote(stdinFile), pipeline))
	} else {
		b.WriteString(fmt.Sprintf("exec %s %s\n", shellJoin(workerExec), pipeline))
	}
	return b.String()
}

func writeWorkerRunnerScript(stateDir, runnerPath string, args []string, stdinFile, logFile, worktree string, split *streamSplit) error {
	guardDir, err := ensureSearchGuardrailWrappers(stateDir)
	if err != nil {
		return err
	}
	// Resolve the single authoritative private credential boundary the runner
	// reads through _worker-exec at exec time. Never a per-worker path (#888): every slot references
	// the same service-level file, so a secret is never duplicated per worker.
	credsFile, err := resolveWorkerCredentialsFile(stateDir)
	if err != nil {
		return err
	}
	maestroBin, ok := maestroExecutablePath()
	if !ok {
		return fmt.Errorf("resolve maestro executable for credential-safe worker exec")
	}
	runnerContent := buildWorkerRunnerScript(args, stdinFile, logFile, worktree, guardDir, credsFile, maestroBin, split)
	if err := writeFileAtomicMode(filepath.Dir(runnerPath), runnerPath, runnerContent, workerRunnerScriptMode); err != nil {
		return fmt.Errorf("write runner script: %w", err)
	}
	return nil
}

// resolveWorkerCredentialsFile returns the single authoritative private path the
// worker runner should read at exec time, or "" when no provider credential is
// available. It never returns a per-worker path (#888):
//
//   - If MAESTRO_WORKER_CREDENTIALS_FILE is set, that operator-owned file is the
//     authoritative boundary (e.g. the systemd EnvironmentFile that replaced the
//     literal Environment= secrets). It is validated — regular file, no
//     group/other access, owned by us, private parent dir — and referenced
//     as-is; maestro writes nothing. A misconfigured (world/group readable,
//     symlinked, or foreign-owned) target is a hard error so a spawn fails
//     closed rather than reading an insecure secret file.
//   - Otherwise no raw credential may be ambient. CLI-native auth can proceed
//     with an empty reference; any known provider value without the explicit
//     service-file boundary fails the spawn closed. Maestro never manufactures
//     a project-local or per-worker secret copy.
func resolveWorkerCredentialsFile(stateDir string) (string, error) {
	if strings.TrimSpace(stateDir) == "" {
		return "", fmt.Errorf("empty state dir")
	}
	if operator := strings.TrimSpace(os.Getenv(workerCredentialsFileEnvVar)); operator != "" {
		if _, err := readWorkerCredentialsFile(operator); err != nil {
			// Do not echo the path value; the env-var name is enough to diagnose.
			return "", fmt.Errorf("%s is not a private credential boundary: %w", workerCredentialsFileEnvVar, err)
		}
		return operator, nil
	}
	for _, key := range workerCredentialEnvKeys {
		if os.Getenv(key) != "" {
			return "", fmt.Errorf("%s must name the authoritative private service credential file when provider environment variables are set", workerCredentialsFileEnvVar)
		}
	}
	return "", nil
}

// openOwnedDirNoFollow opens the final directory component without following a
// symlink and verifies it is owned by the maestro uid and not group/other
// writable. The returned descriptor anchors later openat/renameat operations so
// a path swap cannot redirect a credential write after validation.
func openOwnedDirNoFollow(dir string) (*os.File, error) {
	f, err := openDirectoryPathNoFollow(dir)
	if err != nil {
		return nil, fmt.Errorf("open directory without following links: %w", err)
	}
	fail := func(err error) (*os.File, error) {
		f.Close()
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return fail(fmt.Errorf("stat directory: %w", err))
	}
	if !info.IsDir() {
		return fail(fmt.Errorf("%s is not a directory", filepath.Base(dir)))
	}
	if err := checkOwnedByUs(dir, info); err != nil {
		return fail(err)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fail(fmt.Errorf("directory %s is group/other writable", filepath.Base(dir)))
	}
	return f, nil
}

// openDirectoryPathNoFollow walks an absolute directory path one component at
// a time with openat(O_NOFOLLOW). Unlike opening only the final path, this also
// rejects symlinks in intermediate components.
func openDirectoryPathNoFollow(dir string) (*os.File, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	rootFD, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(rootFD), string(filepath.Separator))
	components := strings.Split(strings.TrimPrefix(filepath.Clean(abs), string(filepath.Separator)), string(filepath.Separator))
	for _, component := range components {
		if component == "" || component == "." {
			continue
		}
		fd, err := unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			current.Close()
			return nil, err
		}
		next := os.NewFile(uintptr(fd), component)
		current.Close()
		current = next
	}
	return current, nil
}

func openFilePathNoFollow(path string, flags int) (*os.File, error) {
	path = filepath.Clean(path)
	parent, err := openDirectoryPathNoFollow(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), filepath.Base(path), flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Base(path)), nil
}

// randomAtomicName returns an unpredictable basename for an O_EXCL temp file.
func randomAtomicName() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return ".maestro-atomic-" + hex.EncodeToString(raw[:]) + ".tmp", nil
}

// writeFileAtomicMode writes content relative to a no-follow directory
// descriptor via O_EXCL|O_NOFOLLOW, fsyncs it, verifies exact owner/mode, and
// atomically renameat-replaces target. A pre-planted target symlink is replaced
// as a directory entry; it is never followed.
func writeFileAtomicMode(dir, target, content string, mode os.FileMode) error {
	dirFile, err := openOwnedDirNoFollow(dir)
	if err != nil {
		return err
	}
	defer dirFile.Close()
	dirFD := int(dirFile.Fd())
	targetName := filepath.Base(target)
	if targetName == "." || targetName == string(filepath.Separator) || filepath.Dir(target) != filepath.Clean(dir) {
		return fmt.Errorf("target is not directly inside validated directory")
	}

	var tmpName string
	var fd int
	for attempt := 0; attempt < 8; attempt++ {
		tmpName, err = randomAtomicName()
		if err != nil {
			return fmt.Errorf("create random temp name: %w", err)
		}
		fd, err = unix.Openat(dirFD, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return fmt.Errorf("create exclusive temp file: %w", err)
		}
	}
	if fd < 0 || err != nil {
		return fmt.Errorf("create exclusive temp file after retries: %w", err)
	}
	tmp := os.NewFile(uintptr(fd), tmpName)
	cleanup := func() {
		tmp.Close()
		_ = unix.Unlinkat(dirFD, tmpName, 0)
	}
	if err := tmp.Chmod(mode.Perm()); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := io.WriteString(tmp, content); err != nil {
		cleanup()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp file: %w", err)
	}
	info, err := tmp.Stat()
	if err != nil {
		cleanup()
		return fmt.Errorf("stat temp file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != mode.Perm() {
		cleanup()
		return fmt.Errorf("temp file type/mode verification failed")
	}
	if err := checkOwnedByUs(tmpName, info); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = unix.Unlinkat(dirFD, tmpName, 0)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := unix.Renameat(dirFD, tmpName, dirFD, targetName); err != nil {
		_ = unix.Unlinkat(dirFD, tmpName, 0)
		return fmt.Errorf("rename temp file: %w", err)
	}
	if err := unix.Fsync(dirFD); err != nil {
		return fmt.Errorf("sync target directory: %w", err)
	}
	return nil
}

// openPrivateCredentialFile opens the final file and parent components with
// O_NOFOLLOW, then validates the opened descriptor. The descriptor — not a path
// checked earlier — is what callers read, closing the check/use race.
func openPrivateCredentialFile(path string) (*os.File, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == string(filepath.Separator) {
		return nil, fmt.Errorf("empty credential file path")
	}
	parentPath := filepath.Dir(path)
	parent, err := openDirectoryPathNoFollow(parentPath)
	if err != nil {
		return nil, fmt.Errorf("open credential parent without following links: %w", err)
	}
	defer parent.Close()
	parentInfo, err := parent.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat credential parent: %w", err)
	}
	if err := checkOwnedByUs(parentPath, parentInfo); err != nil {
		return nil, fmt.Errorf("credential parent: %w", err)
	}
	if parentInfo.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("credential parent is group/other accessible (mode %o)", parentInfo.Mode().Perm())
	}
	fd, err := unix.Openat(int(parent.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open credential file without following links: %w", err)
	}
	f := os.NewFile(uintptr(fd), filepath.Base(path))
	fail := func(err error) (*os.File, error) {
		f.Close()
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return fail(fmt.Errorf("stat credential file: %w", err))
	}
	if !info.Mode().IsRegular() {
		return fail(fmt.Errorf("is not a regular file"))
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fail(fmt.Errorf("is group/other accessible (mode %o)", info.Mode().Perm()))
	}
	if err := checkOwnedByUs(path, info); err != nil {
		return fail(err)
	}
	return f, nil
}

func validatePrivateCredentialFile(path string) error {
	f, err := openPrivateCredentialFile(path)
	if err != nil {
		return err
	}
	return f.Close()
}

// readWorkerCredentialsFile safely reads and parses the private service
// boundary. It accepts the simple KEY=value / export KEY=value syntax shared by
// systemd EnvironmentFile syntax while allow-listing only provider keys used by
// workers.
func readWorkerCredentialsFile(path string) (map[string]string, error) {
	if strings.TrimSpace(path) == "" {
		return map[string]string{}, nil
	}
	f, err := openPrivateCredentialFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, workerCredentialFileMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read credential file: %w", err)
	}
	if len(data) > workerCredentialFileMaxBytes {
		return nil, fmt.Errorf("credential file exceeds %d bytes", workerCredentialFileMaxBytes)
	}
	return parseWorkerCredentialAssignments(string(data))
}

func parseWorkerCredentialAssignments(content string) (map[string]string, error) {
	allowed := make(map[string]struct{}, len(workerCredentialEnvKeys))
	for _, key := range workerCredentialEnvKeys {
		allowed[key] = struct{}{}
	}
	values := make(map[string]string)
	for lineNo, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("credential file line %d is not KEY=value", lineNo+1)
		}
		key = strings.TrimSpace(key)
		if _, ok := allowed[key]; !ok {
			continue
		}
		value, err := decodeCredentialValue(raw)
		if err != nil {
			return nil, fmt.Errorf("credential file line %d: %w", lineNo+1, err)
		}
		values[key] = value
	}
	return values, nil
}

// decodeCredentialValue decodes a single shell/systemd-style assignment word
// without expansion or command execution. Adjacent quoted fragments are
// supported so maestro's shellQuote output round-trips; '$', backticks, and
// other characters remain literal.
func decodeCredentialValue(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	var out strings.Builder
	var quote byte
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if quote == '\'' {
			if c == '\'' {
				quote = 0
			} else {
				out.WriteByte(c)
			}
			continue
		}
		if quote == '"' {
			if c == '"' {
				quote = 0
				continue
			}
			if c == '\\' && i+1 < len(raw) {
				i++
				out.WriteByte(raw[i])
				continue
			}
			out.WriteByte(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '\\':
			if i+1 >= len(raw) {
				return "", fmt.Errorf("trailing escape")
			}
			i++
			out.WriteByte(raw[i])
		default:
			out.WriteByte(c)
		}
	}
	if quote != 0 {
		return "", fmt.Errorf("unterminated quote")
	}
	return out.String(), nil
}

func workerExecEnvironment(base []string, credentials map[string]string) []string {
	blocked := make(map[string]struct{}, len(workerCredentialEnvKeys)+1)
	for _, key := range workerCredentialEnvKeys {
		blocked[key] = struct{}{}
	}
	blocked[workerCredentialsFileEnvVar] = struct{}{}
	out := make([]string, 0, len(base)+len(credentials))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if _, drop := blocked[key]; ok && drop {
			continue
		}
		out = append(out, entry)
	}
	keys := make([]string, 0, len(credentials))
	for key := range credentials {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(out, key+"="+credentials[key])
	}
	return out
}

// RunWorkerWithCredentials is the internal runner boundary: validate and read
// the single private service file once immediately before starting the worker,
// clear any stale tmux copies, inject the allow-listed values only into that
// process environment, and redact its combined output from the exact same
// in-memory snapshot before returning bytes to JSONL/tee. A rotation therefore
// cannot race separate injector/filter reads, and no value enters argv or disk.
func RunWorkerWithCredentials(credentialsFile string, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return fmt.Errorf("worker command is empty")
	}
	credentials, err := readWorkerCredentialsFile(credentialsFile)
	if err != nil {
		return fmt.Errorf("load private worker credentials: %w", err)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = workerExecEnvironment(os.Environ(), credentials)
	cmd.Stdin = stdin
	reader, writer := io.Pipe()
	cmd.Stdout = writer
	cmd.Stderr = writer
	if err := cmd.Start(); err != nil {
		reader.Close()
		writer.Close()
		return fmt.Errorf("start worker executable: %w", err)
	}
	waited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		_ = writer.Close()
		waited <- err
	}()
	filterErr := redactCredentialStream(reader, stdout, credentialSecretValues(credentials))
	if filterErr != nil {
		_ = reader.CloseWithError(filterErr)
	}
	waitErr := <-waited
	if filterErr != nil {
		return fmt.Errorf("redact worker output: %w", filterErr)
	}
	if waitErr != nil {
		return fmt.Errorf("worker executable: %w", waitErr)
	}
	return nil
}

func credentialSecretValues(credentials map[string]string) []string {
	var values []string
	for _, key := range workerCredentialSecretKeys {
		// Live worker output must never expose even a deliberately short test or
		// development credential. Historical scrubbing applies its own minimum
		// length before scanning broad state surfaces to avoid destructive
		// over-matching.
		if value := credentials[key]; value != "" {
			values = append(values, value)
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return values
}

func credentialMask(length int) []byte {
	if length <= 0 {
		return nil
	}
	base := []byte("[REDACTED]")
	mask := bytes.Repeat([]byte{'*'}, length)
	if length >= len(base) {
		copy(mask, base)
	}
	return mask
}

func redactCredentialBytes(data []byte, secrets []string) []byte {
	out := append([]byte(nil), data...)
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		out = bytes.ReplaceAll(out, []byte(secret), credentialMask(len(secret)))
	}
	return out
}

// redactCredentialStream removes exact credential values before output reaches
// stream-split, JSONL, or tee. It retains a max-secret overlap across chunks so
// a value split by the reader boundary is never emitted partially raw.
func redactCredentialStream(in io.Reader, out io.Writer, secrets []string) error {
	if len(secrets) == 0 {
		_, err := io.Copy(out, in)
		return err
	}
	maxLen := 0
	for _, secret := range secrets {
		if len(secret) > maxLen {
			maxLen = len(secret)
		}
	}
	buf := make([]byte, 32*1024)
	pending := make([]byte, 0, len(buf)+maxLen)
	for {
		n, readErr := in.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			if emit := len(pending) - (maxLen - 1); emit > 0 {
				redacted := redactCredentialBytes(pending, secrets)
				if _, err := out.Write(redacted[:emit]); err != nil {
					return err
				}
				pending = append(pending[:0], redacted[emit:]...)
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return readErr
			}
			if len(pending) > 0 {
				_, err := out.Write(redactCredentialBytes(pending, secrets))
				return err
			}
			return nil
		}
	}
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
//   - Text state, prompt, postmortem, audit, JSONL, and log artifacts are scrubbed
//     in place with same-length masks. In-place writes preserve the inode and
//     length, so a live worker holding an O_APPEND log descriptor keeps running
//     and appending to the same file (#877 safety).
//   - Secret candidates are collected before deletion from the current private
//     boundary, daemon environment, legacy env copies, and legacy runner exports.
//     This lets an upgrade scrub an old value even if rotation already changed
//     the daemon environment.
func ScrubLegacyRunArtifacts(stateDir string) {
	if strings.TrimSpace(stateDir) == "" {
		return
	}

	legacyEnvFiles, _ := filepath.Glob(filepath.Join(stateDir, "*-run.env"))
	legacyRunners, _ := filepath.Glob(filepath.Join(stateDir, "*-run.sh"))
	legacyServiceFile := filepath.Join(stateDir, workerCredentialsDirName, workerCredentialsFileName)
	approvedBoundary := strings.TrimSpace(os.Getenv(workerCredentialsFileEnvVar))
	legacyCredentialFiles := append([]string(nil), legacyEnvFiles...)
	if !sameCleanPath(approvedBoundary, legacyServiceFile) {
		legacyCredentialFiles = append(legacyCredentialFiles, legacyServiceFile)
	}
	secrets := collectCredentialSecretsForScrub(approvedBoundary, legacyCredentialFiles, legacyRunners)

	if stateRoot, err := openOwnedDirNoFollow(stateDir); err != nil {
		log.Printf("[worker] open state dir for stale credential scrub: %v", err)
	} else {
		for _, path := range legacyEnvFiles {
			// Unlink relative to the validated directory descriptor; a malicious
			// final-component symlink is removed rather than followed.
			if err := unix.Unlinkat(int(stateRoot.Fd()), filepath.Base(path), 0); err != nil && err != unix.ENOENT {
				log.Printf("[worker] scrub stale creds %s: %v", filepath.Base(path), err)
			}
		}
		_ = unix.Fsync(int(stateRoot.Fd()))
		stateRoot.Close()
	}
	if !sameCleanPath(approvedBoundary, legacyServiceFile) {
		legacyDir := filepath.Dir(legacyServiceFile)
		if dir, err := openOwnedDirNoFollow(legacyDir); err == nil {
			if err := unix.Unlinkat(int(dir.Fd()), filepath.Base(legacyServiceFile), 0); err != nil && err != unix.ENOENT {
				log.Printf("[worker] scrub stale service credential copy: %v", err)
			}
			_ = unix.Fsync(int(dir.Fd()))
			dir.Close()
		}
	}

	for _, path := range legacyRunners {
		scrubRunnerScript(path)
	}

	if err := ensureWorkerLogDir(filepath.Join(stateDir, "logs")); err != nil && !os.IsNotExist(err) {
		log.Printf("[worker] repair worker log dir permissions: %v", err)
	}
	if approvedBoundary != "" {
		approvedBoundary, _ = filepath.Abs(approvedBoundary)
	}
	redacted := 0
	err := filepath.WalkDir(stateDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !credentialArtifactTextFile(path) {
			return nil
		}
		absolutePath, _ := filepath.Abs(path)
		if approvedBoundary != "" && filepath.Clean(absolutePath) == filepath.Clean(approvedBoundary) {
			return nil
		}
		changed, err := scrubSecretValuesInPlace(path, secrets)
		if err != nil {
			log.Printf("[worker] scrub artifact %s: %v", filepath.Base(path), err)
			return nil
		}
		if changed {
			redacted++
		}
		return nil
	})
	if err != nil {
		log.Printf("[worker] scrub credential artifacts: %v", err)
	}
	if redacted > 0 {
		log.Printf("[worker] redacted provider credential values from %d historical text artifact(s) (#888)", redacted)
	}
}

func sameCleanPath(left, right string) bool {
	if strings.TrimSpace(left) == "" || strings.TrimSpace(right) == "" {
		return false
	}
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}

func credentialArtifactTextFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json", ".jsonl", ".log", ".md", ".txt", ".yaml", ".yml", ".toml", ".sh", ".env":
		return true
	default:
		return false
	}
}

// readOwnedRegularNoFollow reads a historical state artifact without following
// a final-component symlink. Legacy files may have loose modes (that is exactly
// what the migration repairs), but they must be regular and owned by maestro.
func readOwnedRegularNoFollow(path string, maxBytes int64) ([]byte, error) {
	f, err := openFilePathNoFollow(path, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if err := checkOwnedByUs(path, info); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file exceeds scrub read limit")
	}
	return data, nil
}

func collectCredentialSecretsForScrub(approvedBoundary string, legacyCredentialFiles, legacyRunners []string) []string {
	seen := map[string]struct{}{}
	add := func(value string) {
		if len(value) >= 8 {
			seen[value] = struct{}{}
		}
	}
	for _, value := range currentCredentialSecretValues() {
		add(value)
	}
	if credentials, err := readWorkerCredentialsFile(approvedBoundary); err == nil {
		for _, value := range credentialSecretValues(credentials) {
			add(value)
		}
	}
	for _, path := range legacyCredentialFiles {
		data, err := readOwnedRegularNoFollow(path, workerCredentialFileMaxBytes)
		if err != nil {
			continue
		}
		if credentials, err := parseWorkerCredentialAssignments(string(data)); err == nil {
			for _, value := range credentialSecretValues(credentials) {
				add(value)
			}
		}
	}
	for _, path := range legacyRunners {
		data, err := readOwnedRegularNoFollow(path, workerCredentialFileMaxBytes)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if !isCredentialExportLine(line) {
				continue
			}
			if credentials, err := parseWorkerCredentialAssignments(strings.TrimSpace(line)); err == nil {
				for _, value := range credentialSecretValues(credentials) {
					add(value)
				}
			}
		}
	}
	values := make([]string, 0, len(seen))
	for value := range seen {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return values
}

// scrubSecretValuesInPlace masks exact values without renaming or resizing the
// file. That is safe for active O_APPEND logs: their open descriptor stays on
// the same inode and subsequent output remains visible.
func scrubSecretValuesInPlace(path string, secrets []string) (bool, error) {
	f, err := openFilePathNoFollow(path, unix.O_RDWR)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("not a regular file")
	}
	if err := checkOwnedByUs(path, info); err != nil {
		return false, err
	}

	maxLen := 0
	for _, secret := range secrets {
		if len(secret) > maxLen {
			maxLen = len(secret)
		}
	}
	repairedMode := os.FileMode(0o600)
	if info.Mode().Perm()&0o100 != 0 {
		repairedMode = 0o700
	}
	modeChanged := info.Mode().Perm() != repairedMode
	if maxLen == 0 || info.Size() == 0 {
		if modeChanged {
			if err := f.Chmod(repairedMode); err != nil {
				return false, err
			}
			if err := f.Sync(); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	const chunkSize = 64 * 1024
	buf := make([]byte, chunkSize)
	pending := make([]byte, 0, chunkSize+maxLen)
	var readOffset, pendingOffset int64
	changed := false
	for readOffset < info.Size() {
		want := int64(len(buf))
		if remaining := info.Size() - readOffset; remaining < want {
			want = remaining
		}
		n, readErr := f.ReadAt(buf[:want], readOffset)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			readOffset += int64(n)
			emit := len(pending) - (maxLen - 1)
			if readOffset == info.Size() {
				emit = len(pending)
			}
			if emit > 0 {
				redacted := redactCredentialBytes(pending, secrets)
				if !bytes.Equal(redacted[:emit], pending[:emit]) {
					if _, err := f.WriteAt(redacted[:emit], pendingOffset); err != nil {
						return false, err
					}
					changed = true
				}
				pendingOffset += int64(emit)
				pending = append(pending[:0], redacted[emit:]...)
			}
		}
		if readErr != nil && readErr != io.EOF {
			return false, readErr
		}
		if n == 0 {
			break
		}
	}
	if changed || modeChanged {
		if err := f.Chmod(repairedMode); err != nil {
			return false, err
		}
		if err := f.Sync(); err != nil {
			return false, err
		}
	}
	return changed, nil
}

// scrubRunnerScript drops any inlined credential export or stale per-worker env
// sourcing from a legacy runner script and repairs its mode. The rewrite touches
// only the file on disk; a worker that already exec'd this script is unaffected.
func scrubRunnerScript(path string) {
	f, err := openFilePathNoFollow(path, unix.O_RDWR)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || checkOwnedByUs(path, info) != nil {
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, workerCredentialFileMaxBytes+1))
	if err != nil || len(data) > workerCredentialFileMaxBytes {
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
		if err := f.Truncate(0); err != nil {
			log.Printf("[worker] scrub runner %s: %v", filepath.Base(path), err)
			return
		}
		if _, err := f.WriteAt([]byte(strings.Join(kept, "\n")), 0); err != nil {
			log.Printf("[worker] scrub runner %s: %v", filepath.Base(path), err)
			return
		}
	}
	if err := f.Chmod(workerRunnerScriptMode); err != nil {
		log.Printf("[worker] repair perms %s: %v", filepath.Base(path), err)
		return
	}
	if changed {
		if err := f.Sync(); err != nil {
			log.Printf("[worker] sync scrubbed runner %s: %v", filepath.Base(path), err)
		}
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
// invokes _worker-exec with a private path reference and never sources a file.
func isStalePerWorkerEnvSourceLine(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.Contains(t, "-run.env") {
		return false
	}
	return strings.HasPrefix(t, ". ") || strings.Contains(t, "] && . ")
}

// currentCredentialSecretValues returns the non-trivial secret values currently
// present in the daemon environment, used to redact legacy artifacts.
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

func workerSearchSafetyPromptSection(worktreePath string) string {
	return fmt.Sprintf("\n\n---\n\n## Worker Search Safety\n\n"+
		"- The assigned worktree is `%s`; use it as the current directory before running code search commands.\n"+
		"- Do NOT run `rg`, `find`, or `grep` from broad filesystem roots such as `/`, `/mnt`, or `/home`.\n"+
		"- If you intentionally need a broad host search, set `MAESTRO_ALLOW_BROAD_SEARCH=1` for that single command.\n",
		worktreePath)
}
