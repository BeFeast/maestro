package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// spawnSleeper starts a private child process the test owns, so signal-path
// tests never fire at arbitrary host PIDs. The child is reaped on cleanup.
func spawnSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func TestKillCgroupProcsPreservesListedPIDs(t *testing.T) {
	victimA := spawnSleeper(t)
	preserved := spawnSleeper(t)
	victimB := spawnSleeper(t)

	dir := t.TempDir()
	procs := filepath.Join(dir, "cgroup.procs")
	content := strconv.Itoa(victimA.Process.Pid) + "\n" +
		strconv.Itoa(preserved.Process.Pid) + "\n" +
		strconv.Itoa(victimB.Process.Pid) + "\n"
	if err := os.WriteFile(procs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	killed := killCgroupProcs(procs, map[int]struct{}{preserved.Process.Pid: {}})
	if killed != 2 {
		t.Fatalf("killed=%d want 2 (preserved %d)", killed, preserved.Process.Pid)
	}
	if !IsAlive(preserved.Process.Pid) {
		t.Fatalf("preserved pid %d was killed", preserved.Process.Pid)
	}
}

func TestKillMaestroWorkerScopeChildrenSweepsNestedSlices(t *testing.T) {
	// Mirror the production layout verified on the runtime host: the workers
	// slice nests under maestro.slice, and the isolated slice nests inside the
	// workers slice (systemd dash hierarchy).
	root := t.TempDir()
	workers := filepath.Join(root, "maestro.slice", "maestro-workers.slice")
	scope := filepath.Join(workers, "maestro-worker-0123456789abcdef0123456789abcdef-g1.scope")
	isoSvc := filepath.Join(workers, "maestro-workers-isolated.slice",
		"maestro-worker-fedcba9876543210fedcba9876543210-g2.service")
	for _, dir := range []string{scope, isoSvc} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	inScope := spawnSleeper(t)
	inIso := spawnSleeper(t)
	if err := os.WriteFile(filepath.Join(scope, "cgroup.procs"),
		[]byte(strconv.Itoa(inScope.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(isoSvc, "cgroup.procs"),
		[]byte(strconv.Itoa(inIso.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := workerSliceCgroupRoots
	workerSliceCgroupRoots = func() []string { return []string{workers} }
	t.Cleanup(func() { workerSliceCgroupRoots = orig })

	if killed := KillMaestroWorkerScopeChildren(); killed != 2 {
		t.Fatalf("killed=%d want 2 (scope + nested isolated service)", killed)
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
	// must return empty — never an ambient session cgroup. If maestro.service
	// happens to exist on the test host, returning its path is fine.
	if got := resolveMaestroServiceCgroupProcs(); got != "" && got != maestroServiceCgroup {
		t.Fatalf("resolve returned ambient path %q", got)
	}
}
