package worker

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestKillCgroupProcsPreservesListedPIDs(t *testing.T) {
	dir := t.TempDir()
	procs := filepath.Join(dir, "cgroup.procs")
	if err := os.WriteFile(procs, []byte("111111\n222222\n333333\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	preserve := map[int]struct{}{222222: {}}
	killed := killCgroupProcs(procs, preserve)
	if killed != 2 {
		t.Fatalf("killed=%d want 2 (preserved 222222)", killed)
	}
}

func TestIsMaestroDaemonPIDMatchesDaemonCmdline(t *testing.T) {
	if isMaestroDaemonPID(os.Getpid()) {
		t.Fatal("test process incorrectly detected as maestro daemon")
	}
}

func TestPreservePIDsAlwaysKeepsSelf(t *testing.T) {
	dir := t.TempDir()
	procs := filepath.Join(dir, "cgroup.procs")
	self := os.Getpid()
	content := strconv.Itoa(self) + "\n999999\n"
	if err := os.WriteFile(procs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := preservePIDs(0, procs)
	if _, ok := got[self]; !ok {
		t.Fatalf("preserve map missing self pid %d: %#v", self, got)
	}
}

func TestResolveMaestroServiceCgroupProcsSkipsAmbientWithoutInvocation(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	// Without a live maestro.service cgroup and without INVOCATION_ID, resolve
	// must return empty — never an ambient session cgroup.
	if got := resolveMaestroServiceCgroupProcs(); got != "" && got != maestroServiceCgroup {
		// If maestro.service happens to exist on the test host, that is fine;
		// any other path would be the dangerous ambient fallback.
		if got != maestroServiceCgroup {
			t.Fatalf("resolve returned ambient path %q", got)
		}
	}
}
