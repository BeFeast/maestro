package selfdeploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSelfDeployGateTriggersRollback is the AC9 end-to-end proof (#842): a
// freshly-built binary that BOOTS and reports the expected version (so the
// existing version/health check passes) but BEHAVES worse (its `selfcheck`
// fails) must be rolled back to <bin>.prev, and the result file must record a
// rolled_back outcome naming the failing check.
//
// It runs the real scripts/self-deploy.sh with git/go/systemctl stubbed on PATH
// so no systemd, network, or toolchain is needed — only bash + coreutils. The
// "new binary" the stubbed build installs reports v9.9.9+gdeadbee for `version`
// but exits non-zero for `selfcheck`; the "old binary" preserved as .prev
// reports the previous version and a passing selfcheck.
func TestSelfDeployGateTriggersRollback(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	script := selfDeployScriptPath(t)

	root := t.TempDir()
	stubDir := filepath.Join(root, "stubs")
	repoDir := filepath.Join(root, "repo")
	stateDir := filepath.Join(root, "state")
	binDir := filepath.Join(root, "bin")
	for _, d := range []string{stubDir, repoDir, stateDir, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(binDir, "maestro")
	resultFile := filepath.Join(stateDir, resultFileName)

	// The currently-installed binary → preserved as <bin>.prev on install, and
	// restored on rollback. Old version, passing selfcheck.
	writeExec(t, bin, `#!/bin/bash
case "$1" in
  version) echo "maestro v1.0.0+gcafef00" ;;
  selfcheck) echo "[selfcheck] OK (4 checks passed)"; exit 0 ;;
  *) exit 0 ;;
esac
`)

	// git stub: fetch is a no-op; rev-parse yields a fixed SHA whose short form
	// is "deadbee"; worktree add materializes the build dir with a VERSION file;
	// worktree remove tears it down.
	writeExec(t, filepath.Join(stubDir, "git"), `#!/bin/bash
if [[ "$1" == "-C" ]]; then shift 2; fi
sub="$1"; shift
case "$sub" in
  fetch) exit 0 ;;
  rev-parse) echo "deadbeefcafe00000000000000000000000000ab" ;;
  worktree)
    action="$1"; shift
    if [[ "$action" == "add" ]]; then
      dir=""
      for a in "$@"; do
        case "$a" in --*) continue ;; *) dir="$a"; break ;; esac
      done
      mkdir -p "$dir"
      printf 'version = "9.9.9"\n' > "$dir/VERSION"
    elif [[ "$action" == "remove" ]]; then
      dir="${!#}"
      rm -rf "$dir" 2>/dev/null || true
    fi
    ;;
  *) exit 0 ;;
esac
`)

	// go stub: `build -o OUT` writes the NEW binary — one that reports the
	// stamped version (so verify passes) but whose selfcheck FAILS (the exact
	// regression the gate exists to catch).
	writeExec(t, filepath.Join(stubDir, "go"), `#!/bin/bash
sub="$1"; shift
case "$sub" in
  build)
    out=""; prev=""
    for a in "$@"; do
      if [[ "$prev" == "-o" ]]; then out="$a"; fi
      prev="$a"
    done
    cat > "$out" <<'NEWBIN'
#!/bin/bash
case "$1" in
  version) echo "maestro v9.9.9+gdeadbee" ;;
  selfcheck)
    echo "[selfcheck] PASS config: ok"
    echo "[selfcheck] FAIL prompt: assemble supervisor prompt: boom"
    echo "[selfcheck] FAILED: prompt"
    exit 1 ;;
  *) exit 0 ;;
esac
NEWBIN
    chmod +x "$out"
    ;;
  version) echo "go version go1.25.0 stub" ;;
  *) exit 0 ;;
esac
`)

	// systemctl stub: restarts and liveness checks always succeed.
	writeExec(t, filepath.Join(stubDir, "systemctl"), "#!/bin/bash\nexit 0\n")

	cmd := exec.Command(bash, script,
		"--repo-dir", repoDir,
		"--bin", bin,
		"--units", "maestro-test.service",
		"--result-file", resultFile,
		"--timeout-seconds", "60",
		"--pr", "842",
		"--scope", "user",
	)
	cmd.Env = append(os.Environ(), "PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, runErr := cmd.CombinedOutput()

	// The script exits non-zero on any rollback (fail() ends with exit 1); the
	// authoritative signal is the result file, not the exit code.
	if runErr == nil {
		t.Fatalf("expected the deploy to fail (rollback) on a gate failure, but it exited 0\n%s", out)
	}

	res, err := ReadResult(stateDir)
	if err != nil {
		t.Fatalf("ReadResult: %v\nscript output:\n%s", err, out)
	}
	if res == nil {
		t.Fatalf("no result file written by the deploy\nscript output:\n%s", out)
	}
	if res.Status != StatusRolledBack {
		t.Fatalf("result status = %q, want %q\nreason: %s\nscript output:\n%s", res.Status, StatusRolledBack, res.Reason, out)
	}
	if !strings.Contains(res.Reason, "smoke gate") || !strings.Contains(res.Reason, "prompt") {
		t.Errorf("rollback reason should name the smoke gate and the failing check; got %q", res.Reason)
	}

	// Rollback must have restored the previous binary in place.
	restored, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read restored bin: %v", err)
	}
	if !strings.Contains(string(restored), "1.0.0+gcafef00") {
		t.Errorf("expected <bin> restored to the previous binary after rollback; content did not match old binary")
	}
	if _, err := os.Stat(bin + ".prev"); err != nil {
		t.Errorf("<bin>.prev should be kept after rollback for manual inspection: %v", err)
	}
}

// TestSelfDeployGatePassAllowsDeploy is the counterpart: when the freshly-built
// binary's selfcheck PASSES, the gate does not interfere and the deploy
// finalizes as `deployed`. This guards against the gate rejecting healthy
// builds (which would make every deploy roll back).
func TestSelfDeployGatePassAllowsDeploy(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	script := selfDeployScriptPath(t)

	root := t.TempDir()
	stubDir := filepath.Join(root, "stubs")
	repoDir := filepath.Join(root, "repo")
	stateDir := filepath.Join(root, "state")
	binDir := filepath.Join(root, "bin")
	for _, d := range []string{stubDir, repoDir, stateDir, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(binDir, "maestro")
	resultFile := filepath.Join(stateDir, resultFileName)

	writeExec(t, bin, `#!/bin/bash
case "$1" in
  version) echo "maestro v1.0.0+gcafef00" ;;
  *) exit 0 ;;
esac
`)
	writeExec(t, filepath.Join(stubDir, "git"), `#!/bin/bash
if [[ "$1" == "-C" ]]; then shift 2; fi
sub="$1"; shift
case "$sub" in
  fetch) exit 0 ;;
  rev-parse) echo "deadbeefcafe00000000000000000000000000ab" ;;
  worktree)
    action="$1"; shift
    if [[ "$action" == "add" ]]; then
      dir=""
      for a in "$@"; do case "$a" in --*) continue ;; *) dir="$a"; break ;; esac; done
      mkdir -p "$dir"; printf 'version = "9.9.9"\n' > "$dir/VERSION"
    elif [[ "$action" == "remove" ]]; then
      dir="${!#}"; rm -rf "$dir" 2>/dev/null || true
    fi
    ;;
  *) exit 0 ;;
esac
`)
	// The healthy new binary: correct version AND a passing selfcheck.
	writeExec(t, filepath.Join(stubDir, "go"), `#!/bin/bash
sub="$1"; shift
case "$sub" in
  build)
    out=""; prev=""
    for a in "$@"; do if [[ "$prev" == "-o" ]]; then out="$a"; fi; prev="$a"; done
    cat > "$out" <<'NEWBIN'
#!/bin/bash
case "$1" in
  version) echo "maestro v9.9.9+gdeadbee" ;;
  selfcheck) echo "[selfcheck] OK (4 checks passed)"; exit 0 ;;
  *) exit 0 ;;
esac
NEWBIN
    chmod +x "$out"
    ;;
  version) echo "go version go1.25.0 stub" ;;
  *) exit 0 ;;
esac
`)
	writeExec(t, filepath.Join(stubDir, "systemctl"), "#!/bin/bash\nexit 0\n")

	cmd := exec.Command(bash, script,
		"--repo-dir", repoDir,
		"--bin", bin,
		"--units", "maestro-test.service",
		"--result-file", resultFile,
		"--timeout-seconds", "60",
		"--pr", "842",
		"--scope", "user",
	)
	cmd.Env = append(os.Environ(), "PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("healthy deploy should exit 0, got %v\n%s", runErr, out)
	}

	res, err := ReadResult(stateDir)
	if err != nil || res == nil {
		t.Fatalf("ReadResult: res=%v err=%v\n%s", res, err, out)
	}
	if res.Status != StatusDeployed {
		t.Fatalf("result status = %q, want %q\nscript output:\n%s", res.Status, StatusDeployed, out)
	}
}

func writeExec(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
