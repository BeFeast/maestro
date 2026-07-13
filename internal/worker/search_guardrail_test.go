package worker

import (
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
		nil,
	)

	for _, want := range []string{
		"export MAESTRO_WORKTREE='/tmp/worktree'",
		"export MAESTRO_SEARCH_GUARDRAIL_DIR='/tmp/state/search-guardrails'",
		"export PATH=\"$MAESTRO_SEARCH_GUARDRAIL_DIR:$MAESTRO_ORIGINAL_PATH\"",
		"cd \"$MAESTRO_WORKTREE\" || exit 1",
		"[maestro] worker worktree:",
		"exec codex exec - < '/tmp/prompt.md' 2>&1 | tee -a '/tmp/worker.log'",
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
		&streamSplit{MaestroBin: "/usr/local/bin/maestro", Backend: "claude", JSONLPath: "/tmp/worker.jsonl"},
	)

	want := "2>&1 | /usr/local/bin/maestro stream-split --backend claude --jsonl /tmp/worker.jsonl | tee -a '/tmp/worker.log'"
	if !strings.Contains(script, want) {
		t.Fatalf("runner script missing stream-split pipeline %q\nscript:\n%s", want, script)
	}
}

// #888: the generated runner must reference the single authoritative credential
// boundary, never inline credential values and never a per-worker copy. Two
// slots must both source the *same* shared file. Synthetic canary values are
// built by concatenation so no contiguous credential-shaped literal exists in
// the source (agent-lint).
func TestWriteWorkerRunnerScriptUsesSingleSharedCredentialBoundary(t *testing.T) {
	tokenCanary := "CANARY-" + "auth-token-" + "d0n0t-persist-" + "0001"
	keyCanary := "CANARY-" + "cliproxy-key-" + "d0n0t-persist-" + "0002"

	// Deterministic credential environment: no operator override, clear every
	// key, then set only the two the test asserts on.
	t.Setenv(workerCredentialsFileEnvVar, "")
	for _, key := range workerCredentialEnvKeys {
		t.Setenv(key, "")
	}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", tokenCanary)
	t.Setenv("CLIPROXY_API_KEY", keyCanary)

	stateDir := t.TempDir()
	sharedCreds := filepath.Join(stateDir, workerCredentialsDirName, workerCredentialsFileName)

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
		if !strings.Contains(script, ". "+shellQuote(sharedCreds)) {
			t.Fatalf("runner does not source the shared credential boundary\n%s", script)
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
		"export ANTHROPIC_AUTH_TOKEN=" + shellQuote(tokenCanary),
		"export CLIPROXY_API_KEY=" + shellQuote(keyCanary),
	} {
		if !strings.Contains(creds, want) {
			t.Fatalf("shared creds missing an expected export")
		}
	}
	if strings.Contains(creds, "OPENAI_API_KEY") {
		t.Fatalf("shared creds wrote a key that was unset")
	}
}

// #888: a rotation that clears the daemon credential env must also clear the
// on-disk authoritative copy.
func TestResolveWorkerCredentialsFileRemovesStaleOnClear(t *testing.T) {
	t.Setenv(workerCredentialsFileEnvVar, "")
	for _, key := range workerCredentialEnvKeys {
		t.Setenv(key, "")
	}
	canary := "CANARY-" + "rotate-me-" + "0003"
	t.Setenv("ANTHROPIC_AUTH_TOKEN", canary)

	stateDir := t.TempDir()
	got, err := resolveWorkerCredentialsFile(stateDir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got == "" {
		t.Fatalf("expected a credential path")
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("creds file not written: %v", err)
	}

	// Rotation clears the daemon credential env.
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	cleared, err := resolveWorkerCredentialsFile(stateDir)
	if err != nil {
		t.Fatalf("resolve (cleared): %v", err)
	}
	if cleared != "" {
		t.Fatalf("expected empty path after clear, got %q", cleared)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatalf("stale creds file not removed: err=%v", err)
	}
}

// #888: an operator-provided private credential file is the authoritative
// boundary; maestro references it and writes no fallback of its own.
func TestResolveWorkerCredentialsFilePrefersOperatorFile(t *testing.T) {
	for _, key := range workerCredentialEnvKeys {
		t.Setenv(key, "")
	}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "CANARY-"+"env-side-"+"0004")

	opFile := filepath.Join(t.TempDir(), "proxy.env")
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

	link := filepath.Join(dir, "link.env")
	if err := os.Symlink(good, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := validatePrivateCredentialFile(link); err == nil {
		t.Fatalf("symlink accepted")
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

// #888: the startup migration inventories and scrubs legacy credential material
// left by a pre-fix daemon — removing every per-worker `*-run.env` (including
// earlier stopped slots: the greptile P1 case), stripping inlined exports and
// stale env sourcing from `*-run.sh`, redacting write-once prompts, and leaving
// live logs intact (inventory-only) — all without touching a worker process.
func TestScrubLegacyRunArtifacts(t *testing.T) {
	for _, key := range workerCredentialEnvKeys {
		t.Setenv(key, "")
	}
	canary := "CANARY-" + "legacy-secret-" + "d0n0t-persist-" + "0005"
	t.Setenv("ANTHROPIC_AUTH_TOKEN", canary)

	stateDir := t.TempDir()
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

	// Write-once prompt with a leaked secret value.
	prompt := filepath.Join(stateDir, "slot-a-prompt.md")
	if err := os.WriteFile(prompt, []byte("context\n"+canary+"\nmore\n"), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	// Live log holding the secret (inventory-only; must not be rewritten).
	logFile := filepath.Join(logDir, "slot-a.log")
	logBody := "line\n" + canary + "\n"
	if err := os.WriteFile(logFile, []byte(logBody), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	ScrubLegacyRunArtifacts(stateDir)

	// Every per-worker env copy removed.
	if perSlot, _ := filepath.Glob(filepath.Join(stateDir, "*-run.env")); len(perSlot) != 0 {
		t.Fatalf("stale per-worker env files survived: %v", perSlot)
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
	if !strings.Contains(string(promptData), credentialRedactionPlaceholder) {
		t.Fatalf("prompt was not redacted with the placeholder")
	}

	// Live log left byte-for-byte intact (inventory-only; no racing rewrite).
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(logData) != logBody {
		t.Fatalf("live log was rewritten")
	}
}

// #888: a legacy runner without credential material still has its permissions
// repaired, while unrelated artifacts are untouched.
func TestScrubLegacyRunArtifactsRepairsPlainRunnerPerms(t *testing.T) {
	for _, key := range workerCredentialEnvKeys {
		t.Setenv(key, "")
	}
	stateDir := t.TempDir()
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
	if info, _ := os.Stat(unrelated); info.Mode().Perm() != 0o644 {
		t.Fatalf("unrelated file perm changed to %o", info.Mode().Perm())
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
