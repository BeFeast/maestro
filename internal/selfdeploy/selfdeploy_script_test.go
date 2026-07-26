package selfdeploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// selfDeployScriptPath returns the repo's scripts/self-deploy.sh relative to
// this test package (internal/selfdeploy → repo root is two levels up).
func selfDeployScriptPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "scripts", "self-deploy.sh"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("scripts/self-deploy.sh not found at %s: %v", p, err)
	}
	return p
}

// buildLineRE matches the `go build ... ./cmd/maestro/` invocation in the deploy
// script. The build is the step that silently failed in #807.
var buildLineRE = regexp.MustCompile(`(?m)^.*go build .*\./cmd/maestro/.*$`)

// #807 regression guard: the deploy build MUST pass -buildvcs=false (otherwise
// Go's default -buildvcs=auto shells out to git in the detached transient-unit
// worktree, trips dubious-ownership, exits 128, and the build — hence the whole
// deploy — silently fails). It must also keep -trimpath and the -X main.version
// stamp, so the fix doesn't regress version stamping (#682).
func TestSelfDeployScriptBuildFlags(t *testing.T) {
	data, err := os.ReadFile(selfDeployScriptPath(t))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	line := buildLineRE.Find(data)
	if line == nil {
		t.Fatal("could not find the `go build ... ./cmd/maestro/` line in self-deploy.sh")
	}
	build := string(line)
	for _, want := range []string{
		"-buildvcs=false",        // #807: build must not depend on git VCS status
		"-trimpath",              // reproducible paths
		"-X main.version=$STAMP", // #682: version stamping preserved
	} {
		if !strings.Contains(build, want) {
			t.Errorf("build line missing %q:\n\t%s", want, build)
		}
	}
}

// TestSelfDeployBuildIsVCSIndependent proves the smoke check that would have
// caught #807: with Go's default VCS stamping, a build in a worktree where
// `git status` fails aborts with "error obtaining VCS status: exit status 128";
// with -buildvcs=false (what the deploy script now uses) the same build
// succeeds. Reproduced here by corrupting a throwaway repo's HEAD — a
// uid-independent proxy for the transient unit's dubious-ownership failure.
func TestSelfDeployBuildIsVCSIndependent(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not in PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module vcsSmoke\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")

	// git identity + a clean HOME so no user-global config leaks into the repo.
	gitEnv := append(os.Environ(),
		"HOME="+dir,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	runIn(t, dir, gitEnv, "git", "init", "-q")
	runIn(t, dir, gitEnv, "git", "add", "-A")
	runIn(t, dir, gitEnv, "git", "commit", "-qm", "init")

	// Corrupt HEAD so `git status` exits 128 — the failure class from #807.
	writeFile(t, filepath.Join(dir, ".git", "HEAD"), "garbage\n")

	// Neutralize any ambient -buildvcs override so the default really is auto.
	buildEnv := append(os.Environ(), "GOFLAGS=")

	// Default build: expected to fail on VCS status. If some environment can't
	// reproduce the failure (e.g. git behaves differently), skip this half rather
	// than flake — the -buildvcs=false success below is the load-bearing guard.
	def := exec.Command(goBin, "build", "-o", filepath.Join(dir, "out-default"), ".")
	def.Dir = dir
	def.Env = buildEnv
	if out, err := def.CombinedOutput(); err == nil {
		t.Logf("default build unexpectedly succeeded in this environment; skipping the failure assertion")
	} else if !strings.Contains(string(out), "VCS") && !strings.Contains(string(out), "buildvcs") {
		t.Fatalf("default build failed but not on VCS status: %v\n%s", err, out)
	}

	// -buildvcs=false (the deploy script's build): must succeed regardless of git.
	fixed := exec.Command(goBin, "build", "-buildvcs=false",
		"-trimpath", "-ldflags", "-s -w -X main.version=1.0.0+gdeadbee",
		"-o", filepath.Join(dir, "out-fixed"), ".")
	fixed.Dir = dir
	fixed.Env = buildEnv
	if out, err := fixed.CombinedOutput(); err != nil {
		t.Fatalf("-buildvcs=false build failed (the deploy build must be VCS-independent): %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "out-fixed")); err != nil {
		t.Fatalf("-buildvcs=false build produced no binary: %v", err)
	}
}

// TestSelfDeployScriptSmokeGateWiring (#842) is the static guard that the
// behavioral smoke gate is wired into the deploy path correctly: it runs the
// freshly-installed binary's `selfcheck`, does so AFTER the version/health
// verify and BEFORE the deploy is finalized (write_result deployed), and routes
// a gate failure through fail() — the same function the version-mismatch path
// uses to roll back to .prev. A refactor that reorders these, or drops the gate
// past the finalize point, would let a merely-booting-but-worse binary through.
func TestSelfDeployScriptSmokeGateWiring(t *testing.T) {
	data, err := os.ReadFile(selfDeployScriptPath(t))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	script := string(data)

	// The gate must invoke selfcheck on the installed binary and route failure
	// through fail() (which rolls back when INSTALLED).
	for _, want := range []string{
		`"$BIN" selfcheck`,   // the gate exercises the new binary's behavior
		"smoke_gate || fail", // gate failure → fail() → rollback
	} {
		if !strings.Contains(script, want) {
			t.Errorf("self-deploy.sh missing smoke-gate wiring %q", want)
		}
	}

	// Ordering: the gate call must sit AFTER the verify call and BEFORE the
	// deploy is finalized, so a booting-but-broken binary is caught pre-finalize.
	verifyIdx := strings.Index(script, "verify || fail")
	gateIdx := strings.Index(script, "smoke_gate || fail")
	// Anchor on the finalize-specific form `write_result deployed ""`; a bare
	// `write_result deployed` also appears earlier in the "already at this
	// version" no-op path, which precedes verify.
	finalizeIdx := strings.Index(script, `write_result deployed ""`)
	if verifyIdx < 0 || gateIdx < 0 || finalizeIdx < 0 {
		t.Fatalf("missing anchor(s): verify=%d gate=%d finalize=%d", verifyIdx, gateIdx, finalizeIdx)
	}
	if !(verifyIdx < gateIdx && gateIdx < finalizeIdx) {
		t.Errorf("smoke gate is misordered: want verify(%d) < gate(%d) < finalize(%d)", verifyIdx, gateIdx, finalizeIdx)
	}
}

// --- #1129: the deploy must not build in the RAM-backed /tmp ----------------
//
// The three tests below execute the deploy script's OWN shell, extracted from
// marker-delimited regions, instead of asserting on strings that drift away
// from behaviour. scripts/self-deploy.sh carries the matching markers.

// scriptRegion returns the shell between "# >>> <name>" and "# <<< <name>" in
// the script, so a test can run the real code path in bash.
func scriptRegion(t *testing.T, script, name string) string {
	t.Helper()
	openMark, closeMark := "# >>> "+name, "# <<< "+name
	i := strings.Index(script, openMark)
	if i < 0 {
		t.Fatalf("self-deploy.sh has no %q marker (the region the test executes was renamed or removed)", openMark)
	}
	nl := strings.IndexByte(script[i:], '\n')
	if nl < 0 {
		t.Fatalf("marker %q is not followed by a newline", openMark)
	}
	start := i + nl + 1
	j := strings.Index(script[start:], closeMark)
	if j < 0 {
		t.Fatalf("self-deploy.sh has no closing %q marker", closeMark)
	}
	region := script[start : start+j]
	if strings.TrimSpace(region) == "" {
		t.Fatalf("region %q is empty", name)
	}
	return region
}

func readSelfDeployScript(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(selfDeployScriptPath(t))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	return string(data)
}

func bashPath(t *testing.T) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not in PATH")
	}
	return bash
}

// diskBackedTempDir returns a scratch directory that is NOT under /tmp. These
// tests are specifically about tmpfs, so t.TempDir() (which honours TMPDIR and
// otherwise lands in /tmp) is exactly the wrong tool.
func diskBackedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "maestro-selfdeploy-test.")
	if err != nil {
		t.Skipf("no writable disk-backed /var/tmp on this host: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func underTmpfs(p string) bool {
	return p == "/tmp" || strings.HasPrefix(p, "/tmp/")
}

// evalBuildRoot runs the script's build-root selection region in bash with
// mktemp stubbed to echo the template it was handed (so the test observes the
// chosen location without creating anything). It returns the resolved
// BUILD_ROOT and the script's own DEFAULT_BUILD_TMPDIR. tmpdir==nil leaves
// TMPDIR unset.
func evalBuildRoot(t *testing.T, script string, tmpdir *string) (buildRoot, defaultTmpdir string) {
	t.Helper()
	bash := bashPath(t)
	region := scriptRegion(t, script, "build-root-selection (#1129)")
	if !strings.Contains(region, "mktemp") {
		t.Fatalf("the build-root selection region no longer calls mktemp:\n%s", region)
	}

	binDir := filepath.Join(t.TempDir(), "bin")
	writeExec(t, filepath.Join(binDir, "mktemp"),
		"#!/bin/sh\nfor a in \"$@\"; do last=$a; done\nprintf '%s\\n' \"$last\"\n")

	env := []string{"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH")}
	if tmpdir != nil {
		env = append(env, "TMPDIR="+*tmpdir)
	}

	cmd := exec.Command(bash, "-c", "set -euo pipefail\n"+region+
		"\nprintf '%s\\n%s\\n' \"$BUILD_ROOT\" \"$DEFAULT_BUILD_TMPDIR\"")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("evaluating the build-root selection failed: %v\n%s\nregion:\n%s", err, out, region)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		t.Fatalf("unexpected build-root selection output: %q", out)
	}
	return strings.TrimSpace(lines[len(lines)-2]), strings.TrimSpace(lines[len(lines)-1])
}

// #1129 regression guard (acceptance criterion 1, half one): the build root —
// a full detached git worktree plus the ~25 MB binary — must never be placed in
// /tmp, which on the fleet host is a RAM-backed tmpfs on a swapless box. The
// old code hardcoded the /tmp template, which also made mktemp ignore $TMPDIR,
// so there was no environment-level lever either. This runs the script's own
// selection logic under a range of TMPDIR values, including the degenerate
// TMPDIR=/ that would otherwise collapse to the template
// "/maestro-self-deploy.XXXXXX" and abort the deploy at mktemp.
func TestSelfDeployBuildRootIsDiskBacked(t *testing.T) {
	script := readSelfDeployScript(t)
	disk := diskBackedTempDir(t)
	str := func(s string) *string { return &s }

	// The script's documented default must itself be a real, disk-backed dir.
	_, def := evalBuildRoot(t, script, nil)
	if !filepath.IsAbs(def) {
		t.Fatalf("DEFAULT_BUILD_TMPDIR is not absolute: %q", def)
	}
	if underTmpfs(def) {
		t.Fatalf("DEFAULT_BUILD_TMPDIR is in the RAM-backed tmpfs: %q", def)
	}
	if info, err := os.Stat(def); err != nil || !info.IsDir() {
		t.Fatalf("DEFAULT_BUILD_TMPDIR %q is not an existing directory: %v", def, err)
	}

	cases := []struct {
		name   string
		tmpdir *string
		want   string
	}{
		{"unset TMPDIR falls back to the disk-backed default", nil, def},
		{"empty TMPDIR falls back", str(""), def},
		// Concern 4: "/" must not collapse to an empty prefix.
		{"TMPDIR=/ is refused", str("/"), def},
		{"TMPDIR=/// is refused", str("///"), def},
		// The whole point of #1129: never the tmpfs, even if asked.
		{"TMPDIR=/tmp is refused", str("/tmp"), def},
		{"TMPDIR=/tmp/ is refused", str("/tmp/"), def},
		{"TMPDIR under /tmp is refused", str("/tmp/somewhere"), def},
		{"nonexistent TMPDIR falls back", str(filepath.Join(disk, "does-not-exist")), def},
		// A usable disk-backed TMPDIR is still honoured: the operator keeps a
		// lever to move the build root without editing the script.
		{"usable TMPDIR is honoured", str(disk), disk},
		{"usable TMPDIR with trailing slash is honoured", str(disk + "/"), disk},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := evalBuildRoot(t, script, tc.tmpdir)
			if !filepath.IsAbs(root) {
				t.Fatalf("build root is not an absolute path: %q", root)
			}
			if underTmpfs(root) {
				t.Fatalf("build root landed in the RAM-backed tmpfs: %q", root)
			}
			if got := filepath.Dir(root); got != tc.want {
				t.Errorf("build root parent = %q, want %q (full: %q)", got, tc.want, root)
			}
			if base := filepath.Base(root); !strings.HasPrefix(base, "maestro-self-deploy.") {
				t.Errorf("build root basename %q lost the maestro-self-deploy. prefix the reaper matches on", base)
			}
		})
	}

	// The rest of the deploy must still hang off BUILD_ROOT: worktree, build
	// output, install source, cleanup. A build root nothing builds into or
	// cleans up would trade a tmpfs spike for a disk leak.
	for _, want := range []string{
		`BUILD_DIR="$BUILD_ROOT/src"`,       // the git worktree lives under it
		`-o "$BUILD_ROOT/maestro"`,          // the build writes into it
		`"$BUILD_ROOT/maestro" "$BIN.next"`, // the install stages from it
		`rm -rf "$BUILD_ROOT"`,              // cleanup_build reclaims it
	} {
		if !strings.Contains(script, want) {
			t.Errorf("self-deploy.sh no longer wires the build root through %q", want)
		}
	}
}

// #1129 regression guard (acceptance criterion 1, half two): moving BUILD_ROOT
// is NOT enough. `go build -o <path>` only places the finished binary; every
// intermediate package archive and the linker's own exe/a.out go to Go's $WORK
// tree under $GOTMPDIR / $TMPDIR, which defaults to /tmp. On this host,
//
//	go build -x -buildvcs=false -trimpath -ldflags '...' -o /var/tmp/x ./cmd/maestro/
//
// prints "WORK=/tmp/go-build2329867455" and "link -o $WORK/b001/exe/a.out" —
// i.e. the link still happened in RAM. This test runs the script's real build
// invocation with `go` stubbed out and TMPDIR deliberately preset to /tmp (the
// value a host without an explicit TMPDIR effectively gives Go), then asserts
// on the environment the build actually ran under. Without the TMPDIR/GOTMPDIR
// pin on the build line it records TMPDIR=/tmp and GOTMPDIR unset, and fails.
func TestSelfDeployGoBuildScratchIsDiskBacked(t *testing.T) {
	script := readSelfDeployScript(t)
	bash := bashPath(t)
	region := scriptRegion(t, script, "build-invocation (#1129)")

	buildRoot := diskBackedTempDir(t)
	buildDir := filepath.Join(buildRoot, "src")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(buildRoot, "go-env.txt")

	binDir := filepath.Join(t.TempDir(), "bin")
	writeExec(t, filepath.Join(binDir, "go"), "#!/bin/sh\n"+
		"{\n"+
		"  printf 'TMPDIR=%s\\n' \"${TMPDIR-}\"\n"+
		"  printf 'GOTMPDIR=%s\\n' \"${GOTMPDIR-}\"\n"+
		"  printf 'PWD=%s\\n' \"$PWD\"\n"+
		"} >\"$GO_STUB_RECORD\"\n")

	cmd := exec.Command(bash, "-c", "set -euo pipefail\n"+region)
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"BUILD_ROOT=" + buildRoot,
		"BUILD_DIR=" + buildDir,
		"STAMP=1.2.3+gdeadbee",
		"GO_STUB_RECORD=" + record,
		// The pre-fix reality: nothing sets TMPDIR for the transient deploy
		// unit, so Go resolves $WORK against /tmp.
		"TMPDIR=/tmp",
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("running the build invocation failed: %v\n%s\nregion:\n%s", err, out, region)
	}

	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the build invocation never reached `go build`: %v", err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			got[k] = v
		}
	}

	if got["PWD"] != buildDir {
		t.Errorf("build ran in %q, want the checked-out worktree %q", got["PWD"], buildDir)
	}
	// Both matter: GOTMPDIR is what Go reads first, TMPDIR is the fallback and
	// is also what the toolchain's child processes use.
	for _, key := range []string{"GOTMPDIR", "TMPDIR"} {
		v := got[key]
		if v == "" {
			t.Errorf("%s is unset for the deploy build — Go's $WORK tree falls back to the RAM-backed /tmp", key)
			continue
		}
		if !filepath.IsAbs(v) {
			t.Errorf("%s=%q is not an absolute path", key, v)
			continue
		}
		if underTmpfs(v) {
			t.Errorf("%s=%q — the deploy build still writes its scratch tree into the RAM-backed tmpfs", key, v)
			continue
		}
		if info, err := os.Stat(v); err != nil || !info.IsDir() {
			t.Errorf("%s=%q is not an existing directory (the build would fail or Go would fall back): %v", key, v, err)
			continue
		}
		// Keeping it inside BUILD_ROOT is what makes cleanup_build and the
		// stale reaper reclaim the scratch tree along with everything else.
		if v != buildRoot && !strings.HasPrefix(v, buildRoot+string(os.PathSeparator)) {
			t.Errorf("%s=%q is outside BUILD_ROOT %q, so nothing reclaims it", key, v, buildRoot)
		}
	}
}

// #1129 follow-up: moving the build root off tmpfs changes the failure mode of
// a SIGKILLed deploy (systemd's RuntimeMaxSec backstop — a documented scenario
// in docs/self-deploy-runbook.md). cleanup_build runs from an EXIT trap, which
// SIGKILL does not honour, so the ~26 MB leftover survives. In /tmp the next
// reboot took care of it; /var/tmp is persistent, and Maestro's own sweeper
// cannot help (internal/tmpfshygiene refuses non-tmpfs roots outright, and
// permanently protects anything containing .git — which a worktree always has).
// So the deploy reaps its own litter. This runs the real reaper function.
func TestSelfDeployReapsStaleBuildRoots(t *testing.T) {
	script := readSelfDeployScript(t)
	bash := bashPath(t)
	if _, err := exec.LookPath("find"); err != nil {
		t.Skip("find not in PATH")
	}
	region := scriptRegion(t, script, "stale-build-root-reap (#1129)")

	base := t.TempDir()
	old := time.Now().Add(-8 * time.Hour)

	mkdir := func(name string, aged bool) string {
		p := filepath.Join(base, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(p, "maestro"), "binary-ish\n")
		if aged {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}

	stale := mkdir("maestro-self-deploy.stale1", true)
	stale2 := mkdir("maestro-self-deploy.stale2", true)
	fresh := mkdir("maestro-self-deploy.fresh", false)
	foreign := mkdir("someone-elses-dir.stale", true)
	// A regular file that matches the glob must survive: the reaper is
	// -type d, and a stray file is not ours to delete.
	strayFile := filepath.Join(base, "maestro-self-deploy.notadir")
	writeFile(t, strayFile, "x\n")
	if err := os.Chtimes(strayFile, old, old); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bash, "-c", "set -euo pipefail\n"+
		"log() { printf '[self-deploy] %s\\n' \"$*\" >&2; }\n"+
		region+"\nreap_stale_build_roots \"$BASE\"\n")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"BASE=" + base,
		"STALE_BUILD_ROOT_MINUTES=360",
		// The reaper prunes dangling worktree registrations; point it at a
		// non-repo so the best-effort git call must not break the reap.
		"REPO_DIR=" + filepath.Join(base, "not-a-repo"),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the stale reaper failed: %v\n%s\nregion:\n%s", err, out, region)
	}

	for _, gone := range []string{stale, stale2} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("stale build root %q survived the reaper (err=%v)\n%s", gone, err, out)
		}
	}
	for _, kept := range []string{fresh, foreign, strayFile} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("the reaper removed %q, which it must not touch: %v", kept, err)
		}
	}

	// Static wiring: the reaper is useless unless the deploy actually calls it,
	// with the disk-backed roots AND the legacy /tmp location (so leftovers
	// from pre-#1129 deploys are collected too), before the build starts.
	call := `reap_stale_build_roots "$BUILD_TMPDIR" "$DEFAULT_BUILD_TMPDIR" /tmp`
	callIdx := strings.Index(script, call)
	if callIdx < 0 {
		t.Fatalf("self-deploy.sh never calls %s", call)
	}
	lockIdx := strings.Index(script, "acquired self-deploy lock")
	buildIdx := strings.Index(script, "# >>> build-invocation (#1129)")
	if lockIdx < 0 || buildIdx < 0 {
		t.Fatalf("missing anchors: lock=%d build=%d", lockIdx, buildIdx)
	}
	if !(lockIdx < callIdx && callIdx < buildIdx) {
		t.Errorf("reap call is misplaced: want single-flight lock(%d) < reap(%d) < build(%d)", lockIdx, callIdx, buildIdx)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runIn(t *testing.T, dir string, env []string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}
