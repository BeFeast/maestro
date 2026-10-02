package aiexecution

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostObservationPathExcludesEveryMountedTreeAndAliases(t *testing.T) {
	dir := t.TempDir()
	work, scratch, shared := filepath.Join(dir, "work"), filepath.Join(dir, "scratch"), filepath.Join(dir, "shared")
	for _, path := range []string{work, scratch, shared} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	p := NativeContainmentProfile{WorktreeRoot: work, ScratchRoot: scratch, ReadOnly: []NativeReadOnlyMount{{Source: shared, Target: "/usr"}}}
	if err := verifyHostObservationPath(p, filepath.Join(dir, "observer.json")); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{work, scratch, shared} {
		if err := verifyHostObservationPath(p, filepath.Join(root, "observer.json")); err == nil {
			t.Fatal("mounted credential accepted", root)
		}
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(work, alias); err != nil {
		t.Fatal(err)
	}
	if err := verifyHostObservationPath(p, filepath.Join(alias, "observer.json")); err == nil {
		t.Fatal("symlink credential accepted")
	}
	p.ReadOnly[0].Source = filepath.Join(dir, "missing")
	if err := verifyHostObservationPath(p, filepath.Join(dir, "observer.json")); err == nil {
		t.Fatal("unknown mount source accepted")
	}
}

func TestHostObservationEnvironmentIsNotForwardedOrGloballyChanged(t *testing.T) {
	t.Setenv("MAESTRO_TEST_OBSERVER", "synthetic-observer-secret")
	for _, env := range [][]string{nil, {"SAFE=value", "MAESTRO_TEST_OBSERVER=stale-copy"}} {
		cmd := exec.Command("/bin/true")
		cmd.Env = env
		stripHostObservationEnvironment(cmd, "MAESTRO_TEST_OBSERVER")
		if cmd.Env == nil || strings.Contains(strings.Join(cmd.Env, "\n"), "MAESTRO_TEST_OBSERVER=") {
			t.Fatal("host observer inherited")
		}
		if os.Getenv("MAESTRO_TEST_OBSERVER") != "synthetic-observer-secret" {
			t.Fatal("global environment changed")
		}
	}
}

func TestControllerRecoveryRequiresOldProcessToBeGone(t *testing.T) {
	lease, err := controllerIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.VerifyInactive(); err == nil {
		t.Fatal("live controller accepted")
	}
	lease.StartTicks = "previous-process"
	if err := lease.VerifyInactive(); err != nil {
		t.Fatal("reused PID not recognized", err)
	}
	lease.PID = 0
	if err := lease.VerifyInactive(); err == nil {
		t.Fatal("missing identity accepted")
	}
}
