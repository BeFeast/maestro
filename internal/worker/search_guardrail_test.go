package worker

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/state"
)

type searchGuardrailHarness struct {
	guardDir string
	fakeDir  string
	worktree string
}

func newSearchGuardrailHarness(t *testing.T) searchGuardrailHarness {
	t.Helper()

	baseDir := t.TempDir()
	worktree := filepath.Join(baseDir, "worktree")
	fakeDir := filepath.Join(baseDir, "fake-bin")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	if err := os.MkdirAll(fakeDir, 0755); err != nil {
		t.Fatalf("create fake bin: %v", err)
	}

	guardDir, err := ensureSearchGuardrailWrappers(filepath.Join(baseDir, "state"))
	if err != nil {
		t.Fatalf("ensureSearchGuardrailWrappers: %v", err)
	}

	fakeScript := `#!/bin/sh
printf 'real:%s:%s\n' "$0" "$PWD"
printf 'args:'
for arg in "$@"; do
  printf '<%s>' "$arg"
done
printf '\n'
`
	for _, name := range searchGuardedCommands {
		if err := os.WriteFile(filepath.Join(fakeDir, name), []byte(fakeScript), 0755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}

	return searchGuardrailHarness{
		guardDir: guardDir,
		fakeDir:  fakeDir,
		worktree: worktree,
	}
}

func (h searchGuardrailHarness) run(t *testing.T, name, cwd string, allowBroad bool, args ...string) (string, int) {
	t.Helper()

	cmd := exec.Command(filepath.Join(h.guardDir, name), args...)
	cmd.Dir = cwd
	cmd.Env = []string{
		"MAESTRO_WORKTREE=" + h.worktree,
		"MAESTRO_ORIGINAL_PATH=" + h.fakeDir,
		"PATH=" + h.guardDir + string(os.PathListSeparator) + h.fakeDir,
	}
	if allowBroad {
		cmd.Env = append(cmd.Env, "MAESTRO_ALLOW_BROAD_SEARCH=1")
	} else {
		cmd.Env = append(cmd.Env, "MAESTRO_ALLOW_BROAD_SEARCH=")
	}

	output, err := cmd.CombinedOutput()
	if err == nil {
		return string(output), 0
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("run guarded %s: %v", name, err)
	}
	return string(output), exitErr.ExitCode()
}

func TestSearchGuardrailWrapperAllowsBroadLookingPatterns(t *testing.T) {
	h := newSearchGuardrailHarness(t)
	tests := []struct {
		name    string
		command string
		args    []string
	}{
		{
			name:    "rg positional pattern",
			command: "rg",
			args:    []string{"/home/", "."},
		},
		{
			name:    "rg regexp option pattern",
			command: "rg",
			args:    []string{"--regexp", "/tmp/", "."},
		},
		{
			name:    "grep positional pattern",
			command: "grep",
			args:    []string{"/tmp/", "file.txt"},
		},
		{
			name:    "grep regexp option pattern",
			command: "grep",
			args:    []string{"-e", "/home/", "file.txt"},
		},
		{
			name:    "find expression pattern",
			command: "find",
			args:    []string{".", "-name", "/tmp/*"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, code := h.run(t, tt.command, h.worktree, false, tt.args...)
			if code != 0 {
				t.Fatalf("guarded %s exited %d, output:\n%s", tt.command, code, output)
			}
			if !strings.Contains(output, "real:") {
				t.Fatalf("guarded %s did not run real command, output:\n%s", tt.command, output)
			}
		})
	}
}

func TestSearchGuardrailWrapperRejectsBroadSearchScopes(t *testing.T) {
	h := newSearchGuardrailHarness(t)
	tests := []struct {
		name    string
		command string
		args    []string
	}{
		{
			name:    "rg path operand",
			command: "rg",
			args:    []string{"needle", "/"},
		},
		{
			name:    "rg files mode path operand",
			command: "rg",
			args:    []string{"--files", "/tmp"},
		},
		{
			name:    "grep file operand",
			command: "grep",
			args:    []string{"needle", "/tmp"},
		},
		{
			name:    "find path operand",
			command: "find",
			args:    []string{"/tmp", "-name", "needle"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, code := h.run(t, tt.command, h.worktree, false, tt.args...)
			if code != 2 {
				t.Fatalf("guarded %s exited %d, want 2, output:\n%s", tt.command, code, output)
			}
			if !strings.Contains(output, "broad filesystem path") || !strings.Contains(output, h.worktree) {
				t.Fatalf("guarded %s did not point back to worktree, output:\n%s", tt.command, output)
			}
			if strings.Contains(output, "real:") {
				t.Fatalf("guarded %s ran real command after rejection, output:\n%s", tt.command, output)
			}
		})
	}
}

func TestSearchGuardrailWrapperRejectsBroadCWD(t *testing.T) {
	h := newSearchGuardrailHarness(t)
	output, code := h.run(t, "rg", "/tmp", false, "needle")
	if code != 2 {
		t.Fatalf("guarded rg exited %d, want 2, output:\n%s", code, output)
	}
	if !strings.Contains(output, "broad filesystem root") || !strings.Contains(output, h.worktree) {
		t.Fatalf("guarded rg did not point back to worktree, output:\n%s", output)
	}
	if strings.Contains(output, "real:") {
		t.Fatalf("guarded rg ran real command after rejection, output:\n%s", output)
	}
}

func TestSearchGuardrailWrapperAllowsExplicitBroadSearch(t *testing.T) {
	h := newSearchGuardrailHarness(t)
	output, code := h.run(t, "rg", "/tmp", true, "needle", "/")
	if code != 0 {
		t.Fatalf("guarded rg exited %d, output:\n%s", code, output)
	}
	if !strings.Contains(output, "real:") {
		t.Fatalf("guarded rg did not run real command, output:\n%s", output)
	}
}

func TestBuildWorkerRunnerScriptIncludesSearchGuardrails(t *testing.T) {
	script := buildWorkerRunnerScript(
		[]string{"codex", "exec", "-"},
		"/tmp/prompt.md",
		"/tmp/worker.log",
		"/tmp/worktree",
		"/tmp/state/search-guardrails",
		"",
		"/usr/local/bin/maestro",
		nil,
	)

	for _, want := range []string{
		"export MAESTRO_WORKTREE='/tmp/worktree'",
		"export MAESTRO_SEARCH_GUARDRAIL_DIR='/tmp/state/search-guardrails'",
		"export PATH=\"$MAESTRO_SEARCH_GUARDRAIL_DIR:$MAESTRO_ORIGINAL_PATH\"",
		"cd \"$MAESTRO_WORKTREE\" || exit 1",
		"[maestro] worker worktree:",
		"exec /usr/local/bin/maestro _worker-exec -- codex exec - < '/tmp/prompt.md' 2>&1 | tee -a '/tmp/worker.log'",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("runner script missing %q\nscript:\n%s", want, script)
		}
	}
}

// #737: with a stream-split config the worker command is piped through
// `maestro stream-split` (raw NDJSON -> slot.jsonl) before tee, keeping
// slot.log human-readable while capturing usage on the side channel.
func TestBuildWorkerRunnerScriptStreamSplitPipeline(t *testing.T) {
	script := buildWorkerRunnerScript(
		[]string{"claude", "--dangerously-skip-permissions", "-p", "--output-format", "stream-json", "--verbose"},
		"/tmp/prompt.md",
		"/tmp/worker.log",
		"/tmp/worktree",
		"/tmp/state/search-guardrails",
		"",
		"/usr/local/bin/maestro",
		&streamSplit{MaestroBin: "/usr/local/bin/maestro", Backend: "claude", JSONLPath: "/tmp/worker.jsonl"},
	)

	want := "2>&1 | /usr/local/bin/maestro stream-split --backend claude --jsonl /tmp/worker.jsonl | tee -a '/tmp/worker.log'"
	if !strings.Contains(script, want) {
		t.Fatalf("runner script missing stream-split pipeline %q\nscript:\n%s", want, script)
	}
}

// #888: the generated runner must reference the single authoritative credential
// boundary, never inline credential values and never a per-worker copy. Two
// slots must both reference the *same* shared file. Synthetic canary values are
// built by concatenation so no contiguous credential-shaped literal exists in
// the source (agent-lint).
func TestWriteWorkerRunnerScriptUsesSingleSharedCredentialBoundary(t *testing.T) {
	tokenCanary := "CANARY-" + "auth-token-" + "d0n0t-persist-" + "0001"
	keyCanary := "CANARY-" + "cliproxy-key-" + "d0n0t-persist-" + "0002"

	// Deterministic credential environment: no operator override, clear every
	// key, then set only the two the test asserts on.
	for _, key := range workerCredentialEnvKeys {
		t.Setenv(key, "")
	}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", tokenCanary)
	t.Setenv("CLIPROXY_API_KEY", keyCanary)
	credentialDir := t.TempDir()
	if err := os.Chmod(credentialDir, workerCredentialsDirMode); err != nil {
		t.Fatalf("chmod service credential dir: %v", err)
	}
	sharedCreds := filepath.Join(credentialDir, workerCredentialsFileName)
	if err := os.WriteFile(sharedCreds, []byte(
		"ANTHROPIC_AUTH_TOKEN="+shellQuote(tokenCanary)+"\n"+
			"CLIPROXY_API_KEY="+shellQuote(keyCanary)+"\n",
	), workerCredentialsFileMode); err != nil {
		t.Fatalf("write service credential boundary: %v", err)
	}
	t.Setenv(workerCredentialsFileEnvVar, sharedCreds)

	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatalf("chmod private state dir: %v", err)
	}
	runners := make([]string, 0, 2)
	for _, slot := range []string{"slot-a", "slot-b"} {
		runnerPath := filepath.Join(stateDir, slot+"-run.sh")
		if err := writeWorkerRunnerScript(stateDir, runnerPath,
			[]string{"claude", "-p"}, "",
			filepath.Join(stateDir, slot+".log"),
			filepath.Join(stateDir, "worktree"), nil); err != nil {
			t.Fatalf("writeWorkerRunnerScript(%s): %v", slot, err)
		}
		runners = append(runners, runnerPath)
	}

	for _, runnerPath := range runners {
		scriptBytes, err := os.ReadFile(runnerPath)
		if err != nil {
			t.Fatalf("read runner: %v", err)
		}
		script := string(scriptBytes)
		for _, canary := range []string{tokenCanary, keyCanary} {
			if strings.Contains(script, canary) {
				t.Fatalf("runner script leaked a credential value")
			}
		}
		if strings.Contains(script, "ANTHROPIC_AUTH_TOKEN=") || strings.Contains(script, "CLIPROXY_API_KEY=") {
			t.Fatalf("runner script inlined a credential export")
		}
		sharedRef := shellJoin([]string{sharedCreds})
		if !strings.Contains(script, "_worker-exec --credentials-file "+sharedRef+" --") {
			t.Fatalf("runner does not exec through the shared credential boundary\n%s", script)
		}
		if strings.Contains(script, ". "+shellQuote(sharedCreds)) {
			t.Fatalf("runner shell-sources the credential file instead of exec-time injection\n%s", script)
		}
		for _, key := range workerCredentialEnvKeys {
			if !strings.Contains(script, "unset "+key) {
				t.Fatalf("runner does not clear stale tmux variable %s", key)
			}
		}
		if info, err := os.Stat(runnerPath); err != nil {
			t.Fatalf("stat runner: %v", err)
		} else if info.Mode().Perm() != workerRunnerScriptMode {
			t.Fatalf("runner perm = %o, want %o", info.Mode().Perm(), workerRunnerScriptMode)
		}
	}

	// No per-worker credential copies exist — the greptile P1 / review point-1
	// regression: secrets must never be duplicated per slot.
	if perSlot, _ := filepath.Glob(filepath.Join(stateDir, "*-run.env")); len(perSlot) != 0 {
		t.Fatalf("per-worker credential files were created: %v", perSlot)
	}
	if _, err := os.Stat(filepath.Join(stateDir, workerCredentialsDirName)); !os.IsNotExist(err) {
		t.Fatalf("project-local credential directory was created: %v", err)
	}

	// The single boundary carries the values at 0600 inside a 0700 dir.
	if info, err := os.Stat(sharedCreds); err != nil {
		t.Fatalf("shared creds missing: %v", err)
	} else if info.Mode().Perm() != workerCredentialsFileMode {
		t.Fatalf("shared creds perm = %o, want %o", info.Mode().Perm(), workerCredentialsFileMode)
	}
	if info, err := os.Stat(filepath.Dir(sharedCreds)); err != nil {
		t.Fatalf("creds dir missing: %v", err)
	} else if info.Mode().Perm() != workerCredentialsDirMode {
		t.Fatalf("creds dir perm = %o, want %o", info.Mode().Perm(), workerCredentialsDirMode)
	}
	credsBytes, err := os.ReadFile(sharedCreds)
	if err != nil {
		t.Fatalf("read shared creds: %v", err)
	}
	creds := string(credsBytes)
	for _, want := range []string{
		"ANTHROPIC_AUTH_TOKEN=" + shellQuote(tokenCanary),
		"CLIPROXY_API_KEY=" + shellQuote(keyCanary),
	} {
		if !strings.Contains(creds, want) {
			t.Fatalf("shared creds missing an expected export")
		}
	}
	if strings.Contains(creds, "OPENAI_API_KEY") {
		t.Fatalf("shared creds wrote a key that was unset")
	}
}

// #888: synthetic private-state scan. The canary may exist only in the single
// approved service credential boundary; runner scripts, prompts, logs, state,
// audit JSONL, and per-worker files must contain zero copies.
func TestSyntheticCredentialCanaryExistsOnlyInApprovedBoundary(t *testing.T) {
	for _, key := range workerCredentialEnvKeys {
		t.Setenv(key, "")
	}
	canary := "CANARY-" + "private-state-zero-copy-" + "0888"
	t.Setenv("OPENAI_API_KEY", canary)
	credentialDir := t.TempDir()
	if err := os.Chmod(credentialDir, 0o700); err != nil {
		t.Fatalf("chmod service credential dir: %v", err)
	}
	approved := filepath.Join(credentialDir, workerCredentialsFileName)
	if err := os.WriteFile(approved, []byte("OPENAI_API_KEY="+shellQuote(canary)+"\n"), 0o600); err != nil {
		t.Fatalf("write service credential boundary: %v", err)
	}
	t.Setenv(workerCredentialsFileEnvVar, approved)

	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatalf("chmod state dir: %v", err)
	}
	runner := filepath.Join(stateDir, "slot-a-run.sh")
	if err := writeWorkerRunnerScript(
		stateDir,
		runner,
		[]string{"sh", "-c", "printf safe-output"},
		filepath.Join(stateDir, "slot-a-prompt.md"),
		filepath.Join(stateDir, "logs", "slot-a.log"),
		filepath.Join(stateDir, "worktree"),
		nil,
	); err != nil {
		t.Fatalf("write runner: %v", err)
	}
	for path, content := range map[string]string{
		filepath.Join(stateDir, "slot-a-prompt.md"): "names only: OPENAI_API_KEY\n",
		filepath.Join(stateDir, "state.json"):       "{\"session\":\"slot-a\"}\n",
		filepath.Join(stateDir, "audit-log.jsonl"):  "{\"action\":\"spawn\"}\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write synthetic surface %s: %v", filepath.Base(path), err)
		}
	}
	logDir := state.LogDir(stateDir)
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "slot-a.log"), []byte("safe-output\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	foundApproved := false
	for _, root := range []string{stateDir, credentialDir} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return walkErr
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if path == approved {
				foundApproved = bytes.Contains(data, []byte(canary))
				return nil
			}
			if bytes.Contains(data, []byte(canary)) {
				t.Fatalf("synthetic credential copy found outside approved boundary: %s", path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("private-state scan: %v", err)
		}
	}
	if !foundApproved {
		t.Fatal("synthetic canary missing from the one approved service boundary")
	}
	if perWorker, _ := filepath.Glob(filepath.Join(stateDir, "*-run.env")); len(perWorker) != 0 {
		t.Fatalf("per-worker credential copies exist: %v", perWorker)
	}
}

// #888: ambient raw provider values without the explicit service-file
// reference fail closed. Maestro must never manufacture a project-local copy.
func TestResolveWorkerCredentialsFileRejectsAmbientCredentialWithoutBoundary(t *testing.T) {
	t.Setenv(workerCredentialsFileEnvVar, "")
	for _, key := range workerCredentialEnvKeys {
		t.Setenv(key, "")
	}
	canary := "CANARY-" + "rotate-me-" + "0003"
	t.Setenv("ANTHROPIC_AUTH_TOKEN", canary)

	stateDir := t.TempDir()
	if got, err := resolveWorkerCredentialsFile(stateDir); err == nil || got != "" {
		t.Fatalf("ambient credential accepted without private boundary: path=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, workerCredentialsDirName)); !os.IsNotExist(err) {
		t.Fatalf("project-local credential copy was created: %v", err)
	}
}

func TestResolveWorkerCredentialsFileAllowsCLINativeAuthWithoutBoundary(t *testing.T) {
	t.Setenv(workerCredentialsFileEnvVar, "")
	for _, key := range workerCredentialEnvKeys {
		t.Setenv(key, "")
	}

	if got, err := resolveWorkerCredentialsFile(t.TempDir()); err != nil || got != "" {
		t.Fatalf("CLI-native auth without ambient credentials was rejected: path=%q err=%v", got, err)
	}
}

// #888: an operator-provided private credential file is the authoritative
// boundary; maestro references it and writes no fallback of its own.
func TestResolveWorkerCredentialsFilePrefersOperatorFile(t *testing.T) {
	for _, key := range workerCredentialEnvKeys {
		t.Setenv(key, "")
	}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "CANARY-"+"env-side-"+"0004")

	opDir := t.TempDir()
	if err := os.Chmod(opDir, 0o700); err != nil {
		t.Fatalf("chmod operator dir: %v", err)
	}
	opFile := filepath.Join(opDir, "proxy.env")
	if err := os.WriteFile(opFile, []byte("export ANTHROPIC_AUTH_TOKEN='x'\n"), 0o600); err != nil {
		t.Fatalf("write operator file: %v", err)
	}
	if err := os.Chmod(opFile, 0o600); err != nil {
		t.Fatalf("chmod operator file: %v", err)
	}
	t.Setenv(workerCredentialsFileEnvVar, opFile)

	stateDir := t.TempDir()
	got, err := resolveWorkerCredentialsFile(stateDir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != opFile {
		t.Fatalf("resolve returned %q, want operator file %q", got, opFile)
	}
	if _, err := os.Stat(filepath.Join(stateDir, workerCredentialsDirName)); !os.IsNotExist(err) {
		t.Fatalf("fallback credential dir created despite operator file: err=%v", err)
	}
}

// #888: a misconfigured (group/other-readable) operator file fails closed.
func TestResolveWorkerCredentialsFileRejectsInsecureOperatorFile(t *testing.T) {
	for _, key := range workerCredentialEnvKeys {
		t.Setenv(key, "")
	}
	opFile := filepath.Join(t.TempDir(), "loose.env")
	if err := os.WriteFile(opFile, []byte("export ANTHROPIC_AUTH_TOKEN='x'\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(opFile, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Setenv(workerCredentialsFileEnvVar, opFile)
	if _, err := resolveWorkerCredentialsFile(t.TempDir()); err == nil {
		t.Fatalf("expected error for group/other-readable operator file")
	}
}

// #888: the private-file validator rejects symlinks, loose modes, and missing
// files so maestro never sources an attacker-substitutable secret file.
func TestValidatePrivateCredentialFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod credential dir: %v", err)
	}

	good := filepath.Join(dir, "good.env")
	if err := os.WriteFile(good, []byte("x"), 0o600); err != nil {
		t.Fatalf("write good: %v", err)
	}
	if err := os.Chmod(good, 0o600); err != nil {
		t.Fatalf("chmod good: %v", err)
	}
	if err := validatePrivateCredentialFile(good); err != nil {
		t.Fatalf("valid file rejected: %v", err)
	}
	if err := os.Chmod(dir, 0o710); err != nil {
		t.Fatalf("chmod accessible credential dir: %v", err)
	}
	if err := validatePrivateCredentialFile(good); err == nil {
		t.Fatal("credential file below a group-accessible parent was accepted")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("restore credential dir mode: %v", err)
	}

	link := filepath.Join(dir, "link.env")
	if err := os.Symlink(good, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := validatePrivateCredentialFile(link); err == nil {
		t.Fatalf("symlink accepted")
	}
	linkedParent := filepath.Join(t.TempDir(), "linked-parent")
	if err := os.Symlink(dir, linkedParent); err != nil {
		t.Fatalf("symlink parent: %v", err)
	}
	if err := validatePrivateCredentialFile(filepath.Join(linkedParent, "good.env")); err == nil {
		t.Fatalf("credential file below a symlinked parent accepted")
	}

	loose := filepath.Join(dir, "loose.env")
	if err := os.WriteFile(loose, []byte("x"), 0o600); err != nil {
		t.Fatalf("write loose: %v", err)
	}
	if err := os.Chmod(loose, 0o640); err != nil {
		t.Fatalf("chmod loose: %v", err)
	}
	if err := validatePrivateCredentialFile(loose); err == nil {
		t.Fatalf("group-readable file accepted")
	}

	if err := validatePrivateCredentialFile(filepath.Join(dir, "nope.env")); err == nil {
		t.Fatalf("missing file accepted")
	}
}

func TestResolveWorkerCredentialsFileRejectsSymlinkedPrivateDirectory(t *testing.T) {
	for _, key := range workerCredentialEnvKeys {
		t.Setenv(key, "")
	}

	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatalf("chmod state dir: %v", err)
	}
	outside := t.TempDir()
	if err := os.Chmod(outside, 0o700); err != nil {
		t.Fatalf("chmod outside dir: %v", err)
	}
	outsideFile := filepath.Join(outside, workerCredentialsFileName)
	outsideBody := "ANTHROPIC_AUTH_TOKEN='synthetic-only'\n"
	if err := os.WriteFile(outsideFile, []byte(outsideBody), 0o600); err != nil {
		t.Fatalf("write outside credential file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(stateDir, workerCredentialsDirName)); err != nil {
		t.Fatalf("symlink credential dir: %v", err)
	}
	t.Setenv(workerCredentialsFileEnvVar, filepath.Join(stateDir, workerCredentialsDirName, workerCredentialsFileName))

	if _, err := resolveWorkerCredentialsFile(stateDir); err == nil {
		t.Fatal("symlinked credential directory was accepted")
	}
	if data, err := os.ReadFile(outsideFile); err != nil || string(data) != outsideBody {
		t.Fatalf("rejected external credential file changed: data=%q err=%v", data, err)
	}
}

func TestWriteWorkerRunnerScriptAtomicallyReplacesTargetSymlink(t *testing.T) {
	for _, key := range workerCredentialEnvKeys {
		t.Setenv(key, "")
	}
	t.Setenv(workerCredentialsFileEnvVar, "")

	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatalf("chmod state dir: %v", err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	target := filepath.Join(stateDir, "slot-a-run.sh")
	if err := os.Symlink(victim, target); err != nil {
		t.Fatalf("symlink target: %v", err)
	}

	if err := writeWorkerRunnerScript(
		stateDir,
		target,
		[]string{"claude", "-p"},
		"",
		filepath.Join(stateDir, "slot-a.log"),
		filepath.Join(stateDir, "worktree"),
		nil,
	); err != nil {
		t.Fatalf("write runner: %v", err)
	}
	if info, err := os.Lstat(target); err != nil {
		t.Fatalf("lstat target: %v", err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != workerRunnerScriptMode {
		t.Fatalf("target was not atomically replaced with an owner-only regular runner: %v", info.Mode())
	}
	if data, err := os.ReadFile(victim); err != nil || string(data) != "untouched" {
		t.Fatalf("victim changed through target symlink: data=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(target); err != nil || !bytes.Contains(data, []byte("_worker-exec")) {
		t.Fatalf("replacement runner missing exec boundary: err=%v", err)
	}
	if temps, _ := filepath.Glob(filepath.Join(stateDir, ".maestro-atomic-*.tmp")); len(temps) != 0 {
		t.Fatalf("atomic temp files survived replacement: %v", temps)
	}
}

func TestParseWorkerCredentialAssignmentsSupportsSystemdAndShellQuote(t *testing.T) {
	canary := "CANARY-" + "quote-'-$-" + "0888"
	values, err := parseWorkerCredentialAssignments(
		"# private service boundary\n" +
			"ANTHROPIC_AUTH_TOKEN=" + shellQuote(canary) + "\n" +
			"export CLIPROXY_API_KEY=\"CANARY-double-quoted-0888\"\n" +
			"UNRELATED=value\n",
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if values["ANTHROPIC_AUTH_TOKEN"] != canary {
		t.Fatal("shell-quoted service credential did not round-trip")
	}
	if values["CLIPROXY_API_KEY"] != "CANARY-double-quoted-0888" {
		t.Fatal("plain systemd KEY=value assignment was not loaded")
	}
	if _, ok := values["UNRELATED"]; ok {
		t.Fatal("non-allow-listed environment key was accepted")
	}
}

func TestWorkerExecEnvironmentClearsStaleTmuxValues(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_AUTH_TOKEN=stale-value",
		"CLIPROXY_API_KEY=stale-key",
		workerCredentialsFileEnvVar + "=/stale/reference",
	}
	current := "CANARY-" + "exec-only-" + "0888"
	got := workerExecEnvironment(base, map[string]string{"ANTHROPIC_AUTH_TOKEN": current})
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "stale-value") || strings.Contains(joined, "stale-key") || strings.Contains(joined, "/stale/reference") {
		t.Fatalf("stale tmux credential material survived exec env construction")
	}
	if !strings.Contains(joined, "ANTHROPIC_AUTH_TOKEN="+current) {
		t.Fatalf("current credential was not injected at exec boundary")
	}
	if !strings.Contains(joined, "PATH=/usr/bin") {
		t.Fatalf("unrelated environment was not preserved")
	}
}

func TestRunWorkerWithCredentialsInjectsOnlyAtExecTime(t *testing.T) {
	current := "CANARY-" + "exec-current-" + "0888"
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod credential dir: %v", err)
	}
	credentialFile := filepath.Join(dir, "worker.env")
	if err := os.WriteFile(credentialFile, []byte("OPENAI_API_KEY="+shellQuote(current)+"\n"), 0o600); err != nil {
		t.Fatalf("write credential file: %v", err)
	}
	t.Setenv("OPENAI_API_KEY", "stale-openai-value")
	t.Setenv("CLIPROXY_API_KEY", "stale-cliproxy-value")
	var output bytes.Buffer
	// The shell program contains variable names only. Length proves the current
	// file value was injected; "unset" proves stale tmux state was removed.
	err := RunWorkerWithCredentials(credentialFile, []string{
		"/bin/sh", "-c", `printf '%s|%s' "${#OPENAI_API_KEY}" "${CLIPROXY_API_KEY-unset}"`,
	}, strings.NewReader(""), &output)
	if err != nil {
		t.Fatalf("exec-time helper subprocess: %v (%s)", err, output.String())
	}
	if output.String() != fmt.Sprintf("%d|unset", len(current)) {
		t.Fatalf("exec-time environment proof = %q, want current length and no stale key", output.String())
	}
}

func TestCredentialArgvHelperProcess(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_CREDENTIAL_ARGV_HELPER") != "1" {
		return
	}
	secret := os.Getenv("OPENAI_API_KEY")
	if secret == "" {
		os.Exit(92)
	}
	for _, arg := range os.Args {
		if strings.Contains(arg, secret) {
			os.Exit(93)
		}
	}
	_, _ = fmt.Fprint(os.Stdout, "argv-clean")
	os.Exit(0)
}

func TestRunWorkerWithCredentialsKeepsCredentialOutOfProcessArguments(t *testing.T) {
	canary := "CANARY-" + "child-argv-zero-copy-" + "0888"
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod credential dir: %v", err)
	}
	credentialFile := filepath.Join(dir, "worker.env")
	if err := os.WriteFile(credentialFile, []byte("OPENAI_API_KEY="+shellQuote(canary)+"\n"), 0o600); err != nil {
		t.Fatalf("write credential file: %v", err)
	}
	t.Setenv("MAESTRO_TEST_CREDENTIAL_ARGV_HELPER", "1")
	var output bytes.Buffer
	err := RunWorkerWithCredentials(credentialFile, []string{
		os.Args[0], "-test.run=^TestCredentialArgvHelperProcess$",
	}, strings.NewReader(""), &output)
	if err != nil {
		t.Fatalf("run argv helper: %v (%q)", err, output.String())
	}
	if output.String() != "argv-clean" {
		t.Fatalf("argv helper output = %q", output.String())
	}
}

func TestShippedSystemdUnitContainsNoLiteralWorkerCredential(t *testing.T) {
	canary := "CANARY-" + "systemd-unit-zero-copy-" + "0888"
	t.Setenv("OPENAI_API_KEY", canary)
	unit, err := os.ReadFile(filepath.Join("..", "..", "maestro.service"))
	if err != nil {
		t.Fatalf("read shipped maestro.service: %v", err)
	}
	text := string(unit)
	if strings.Contains(text, canary) {
		t.Fatal("shipped systemd unit contains the synthetic credential value")
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Environment=") {
			continue
		}
		assignment := strings.Trim(strings.TrimPrefix(line, "Environment="), `"'`)
		for _, key := range workerCredentialSecretKeys {
			if strings.HasPrefix(assignment, key+"=") {
				t.Fatalf("shipped systemd unit has a literal %s assignment", key)
			}
		}
	}
}

func TestRedactCredentialStreamRedactsAcrossReadBoundary(t *testing.T) {
	canary := "CANARY-" + strings.Repeat("cross-chunk-", 6) + "0888"
	// Position the value across the filter's internal 32 KiB read boundary.
	input := strings.Repeat("a", 32*1024-7) + canary + "\ntail\n"
	var output bytes.Buffer
	if err := redactCredentialStream(strings.NewReader(input), &output, []string{canary}); err != nil {
		t.Fatalf("filter: %v", err)
	}
	if strings.Contains(output.String(), canary) {
		t.Fatal("credential value crossed the read boundary unredacted")
	}
	if output.Len() != len(input) {
		t.Fatalf("filter changed stream length: got %d want %d", output.Len(), len(input))
	}
	if !strings.HasSuffix(output.String(), "\ntail\n") || !strings.Contains(output.String(), "[REDACTED]") {
		t.Fatalf("filter did not preserve non-secret output or emit a mask")
	}
}

func TestRunWorkerWithCredentialsRedactsBeforeReturningOutput(t *testing.T) {
	canary := "CANARY-" + "worker-output-" + "0888"
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod credential dir: %v", err)
	}
	credentialFile := filepath.Join(dir, "worker.env")
	if err := os.WriteFile(credentialFile, []byte("ANTHROPIC_AUTH_TOKEN="+shellQuote(canary)+"\n"), 0o600); err != nil {
		t.Fatalf("write credential file: %v", err)
	}
	var output bytes.Buffer
	if err := RunWorkerWithCredentials(credentialFile, []string{
		"/bin/sh", "-c", `printf 'before:%s:after' "$ANTHROPIC_AUTH_TOKEN"`,
	}, strings.NewReader(""), &output); err != nil {
		t.Fatalf("run worker: %v", err)
	}
	if strings.Contains(output.String(), canary) {
		t.Fatal("worker output returned the injected credential value")
	}
	if !strings.Contains(output.String(), "before:[REDACTED]") || !strings.HasSuffix(output.String(), ":after") {
		t.Fatalf("worker output was not safely redacted: %q", output.String())
	}
}

func TestRunWorkerWithCredentialsRedactsShortSecret(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod credential dir: %v", err)
	}
	credentialFile := filepath.Join(dir, "worker.env")
	if err := os.WriteFile(credentialFile, []byte("OPENAI_API_KEY=short\n"), 0o600); err != nil {
		t.Fatalf("write credential file: %v", err)
	}
	var output bytes.Buffer
	if err := RunWorkerWithCredentials(credentialFile, []string{
		"/bin/sh", "-c", `printf '<%s>' "$OPENAI_API_KEY"`,
	}, strings.NewReader(""), &output); err != nil {
		t.Fatalf("run worker: %v", err)
	}
	if strings.Contains(output.String(), "short") || output.String() != "<*****>" {
		t.Fatalf("short worker credential was not fully redacted: %q", output.String())
	}
}

// #888: the startup migration inventories and scrubs legacy credential material
// left by a pre-fix daemon — removing every per-worker `*-run.env` (including
// earlier stopped slots: the greptile P1 case), stripping inlined exports and
// stale env sourcing from `*-run.sh`, and redacting prompts/logs in place — all
// without touching a worker process.
func TestScrubLegacyRunArtifacts(t *testing.T) {
	for _, key := range workerCredentialEnvKeys {
		t.Setenv(key, "")
	}
	t.Setenv(workerCredentialsFileEnvVar, "")
	canary := "CANARY-" + "legacy-secret-" + "d0n0t-persist-" + "0005"

	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatalf("chmod state dir: %v", err)
	}
	logDir := state.LogDir(stateDir)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir logdir: %v", err)
	}

	// Legacy runner: inlined credential export + stale per-worker env sourcing.
	legacyRunner := filepath.Join(stateDir, "slot-a-run.sh")
	legacyEnvRef := filepath.Join(stateDir, "slot-a-run.env")
	runnerBody := "#!/bin/bash\n" +
		"export MAESTRO_WORKTREE='/w'\n" +
		"export ANTHROPIC_AUTH_TOKEN=" + shellQuote(canary) + "\n" +
		"[ -r " + shellQuote(legacyEnvRef) + " ] && . " + shellQuote(legacyEnvRef) + "\n" +
		"exec claude -p\n"
	if err := os.WriteFile(legacyRunner, []byte(runnerBody), 0o755); err != nil {
		t.Fatalf("write legacy runner: %v", err)
	}
	if err := os.Chmod(legacyRunner, 0o755); err != nil {
		t.Fatalf("chmod runner: %v", err)
	}

	// Per-worker env copies: the current slot AND an earlier stopped slot.
	for _, p := range []string{legacyEnvRef, filepath.Join(stateDir, "slot-earlier-run.env")} {
		if err := os.WriteFile(p, []byte("export ANTHROPIC_AUTH_TOKEN="+shellQuote(canary)+"\n"), 0o600); err != nil {
			t.Fatalf("write env copy: %v", err)
		}
	}

	// Earlier #888 attempt: one project-local service copy. It is still a raw
	// duplicate of the real service boundary and must disappear on startup.
	legacyServiceDir := filepath.Join(stateDir, workerCredentialsDirName)
	if err := os.Mkdir(legacyServiceDir, workerCredentialsDirMode); err != nil {
		t.Fatalf("mkdir legacy service credential dir: %v", err)
	}
	legacyServiceFile := filepath.Join(legacyServiceDir, workerCredentialsFileName)
	if err := os.WriteFile(legacyServiceFile, []byte("ANTHROPIC_AUTH_TOKEN="+shellQuote(canary)+"\n"), workerCredentialsFileMode); err != nil {
		t.Fatalf("write legacy service credential copy: %v", err)
	}

	// Write-once prompt with a leaked secret value.
	prompt := filepath.Join(stateDir, "slot-a-prompt.md")
	if err := os.WriteFile(prompt, []byte("context\n"+canary+"\nmore\n"), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	stateFile := filepath.Join(stateDir, "state.json")
	if err := os.WriteFile(stateFile, []byte("{\"synthetic\":\""+canary+"\"}\n"), 0o644); err != nil {
		t.Fatalf("write synthetic state: %v", err)
	}
	auditFile := filepath.Join(stateDir, "audit-log.jsonl")
	if err := os.WriteFile(auditFile, []byte("{\"detail\":\""+canary+"\"}\n"), 0o644); err != nil {
		t.Fatalf("write synthetic audit state: %v", err)
	}

	// Live log holding the secret. Keep an O_APPEND descriptor open across the
	// scrub to prove the migration preserves the inode/active writer.
	logFile := filepath.Join(logDir, "slot-a.log")
	logBody := "line\n" + canary + "\n"
	if err := os.WriteFile(logFile, []byte(logBody), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	activeLog, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open active log descriptor: %v", err)
	}
	defer activeLog.Close()

	ScrubLegacyRunArtifacts(stateDir)

	// Every per-worker env copy removed.
	if perSlot, _ := filepath.Glob(filepath.Join(stateDir, "*-run.env")); len(perSlot) != 0 {
		t.Fatalf("stale per-worker env files survived: %v", perSlot)
	}
	if _, err := os.Lstat(legacyServiceFile); !os.IsNotExist(err) {
		t.Fatalf("legacy project-local service credential copy survived: %v", err)
	}

	// Runner scrubbed of the value, the export, and the stale source; unrelated
	// content preserved; mode tightened.
	scrubbed, err := os.ReadFile(legacyRunner)
	if err != nil {
		t.Fatalf("read scrubbed runner: %v", err)
	}
	got := string(scrubbed)
	if strings.Contains(got, canary) {
		t.Fatalf("runner still holds the credential value")
	}
	if strings.Contains(got, "export ANTHROPIC_AUTH_TOKEN=") {
		t.Fatalf("runner still holds the credential export")
	}
	if strings.Contains(got, "-run.env") {
		t.Fatalf("runner still sources a stale per-worker env file")
	}
	if !strings.Contains(got, "export MAESTRO_WORKTREE='/w'") || !strings.Contains(got, "exec claude -p") {
		t.Fatalf("scrub removed unrelated runner content:\n%s", got)
	}
	if info, _ := os.Stat(legacyRunner); info.Mode().Perm() != workerRunnerScriptMode {
		t.Fatalf("runner perm = %o, want %o", info.Mode().Perm(), workerRunnerScriptMode)
	}

	// Prompt redacted in place.
	promptData, err := os.ReadFile(prompt)
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	if strings.Contains(string(promptData), canary) {
		t.Fatalf("prompt still holds the credential value")
	}
	if !strings.Contains(string(promptData), "[REDACTED]") {
		t.Fatalf("prompt was not redacted with the fixed-length mask")
	}
	for _, path := range []string{stateFile, auditFile} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read scrubbed state artifact: %v", err)
		}
		if strings.Contains(string(data), canary) {
			t.Fatalf("state artifact %s still holds the legacy value", filepath.Base(path))
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("state artifact %s permissions not repaired: info=%v err=%v", filepath.Base(path), info, err)
		}
	}

	if _, err := activeLog.WriteString("after-scrub\n"); err != nil {
		t.Fatalf("active log append after scrub: %v", err)
	}
	// The historical value is redacted in the same inode and the active writer
	// keeps appending to the visible path.
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(logData), canary) {
		t.Fatalf("live log still holds the credential value")
	}
	if !strings.Contains(string(logData), "after-scrub") {
		t.Fatalf("active log descriptor no longer appends to visible log")
	}
}

func TestScrubLegacyRunArtifactsPreservesApprovedBoundaryInsideStateDir(t *testing.T) {
	for _, key := range workerCredentialEnvKeys {
		t.Setenv(key, "")
	}
	canary := "CANARY-" + "approved-boundary-" + "0888"
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatalf("chmod state dir: %v", err)
	}
	boundary := filepath.Join(stateDir, "service-private.env")
	boundaryBody := "OPENAI_API_KEY=" + shellQuote(canary) + "\n"
	if err := os.WriteFile(boundary, []byte(boundaryBody), 0o600); err != nil {
		t.Fatalf("write boundary: %v", err)
	}
	t.Setenv(workerCredentialsFileEnvVar, boundary)
	artifact := filepath.Join(stateDir, "audit-log.jsonl")
	if err := os.WriteFile(artifact, []byte("{\"leak\":\""+canary+"\"}\n"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	ScrubLegacyRunArtifacts(stateDir)

	if data, err := os.ReadFile(boundary); err != nil || string(data) != boundaryBody {
		t.Fatalf("approved credential boundary was modified: data=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(artifact); err != nil || bytes.Contains(data, []byte(canary)) {
		t.Fatalf("historical artifact was not scrubbed: err=%v", err)
	}
}

// #888: a legacy runner without credential material and recognized worker text
// artifacts still have their permissions repaired.
func TestScrubLegacyRunArtifactsRepairsPlainRunnerPerms(t *testing.T) {
	for _, key := range workerCredentialEnvKeys {
		t.Setenv(key, "")
	}
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatalf("chmod state dir: %v", err)
	}
	runner := filepath.Join(stateDir, "slot-a-run.sh")
	if err := os.WriteFile(runner, []byte("#!/bin/bash\nexec claude -p\n"), 0o755); err != nil {
		t.Fatalf("write runner: %v", err)
	}
	if err := os.Chmod(runner, 0o755); err != nil {
		t.Fatalf("chmod runner: %v", err)
	}
	unrelated := filepath.Join(stateDir, "slot-a-prompt.md")
	if err := os.WriteFile(unrelated, []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write unrelated: %v", err)
	}
	if err := os.Chmod(unrelated, 0o644); err != nil {
		t.Fatalf("chmod unrelated: %v", err)
	}

	ScrubLegacyRunArtifacts(stateDir)

	if info, _ := os.Stat(runner); info.Mode().Perm() != workerRunnerScriptMode {
		t.Fatalf("plain runner perm = %o, want %o", info.Mode().Perm(), workerRunnerScriptMode)
	}
	if info, _ := os.Stat(unrelated); info.Mode().Perm() != 0o600 {
		t.Fatalf("worker text artifact perm = %o, want 600", info.Mode().Perm())
	}
	if info, err := os.Stat(filepath.Join(stateDir, "logs")); err != nil {
		t.Fatalf("stat repaired log dir: %v", err)
	} else if info.Mode().Perm() != workerLogDirMode {
		t.Fatalf("worker log dir perm = %o, want %o", info.Mode().Perm(), workerLogDirMode)
	}
}

func TestEnsureSearchGuardrailWrappers(t *testing.T) {
	guardDir, err := ensureSearchGuardrailWrappers(t.TempDir())
	if err != nil {
		t.Fatalf("ensureSearchGuardrailWrappers: %v", err)
	}

	for _, name := range searchGuardedCommands {
		path := filepath.Join(guardDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("missing wrapper %s: %v", name, err)
		}
		if info.Mode()&0111 == 0 {
			t.Fatalf("wrapper %s is not executable: %v", name, info.Mode())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read wrapper %s: %v", name, err)
		}
		content := string(data)
		for _, want := range []string{"MAESTRO_WORKTREE", "MAESTRO_ALLOW_BROAD_SEARCH", "broad filesystem"} {
			if !strings.Contains(content, want) {
				t.Fatalf("wrapper %s missing %q\n%s", name, want, content)
			}
		}
	}
}
