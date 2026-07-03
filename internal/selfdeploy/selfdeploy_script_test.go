package selfdeploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
