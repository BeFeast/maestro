package tmuxsession

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestScopeLaunchArgsUsesSiblingSystemdScopeAndPrivateSocket(t *testing.T) {
	got := scopeLaunchArgs("maestro-workers-1-2", 1000, "/usr/local/bin:/usr/bin:/bin", []string{
		"new-session", "-d", "-s", "maestro-ok-player-1", "-c", "/tmp/worktree", bashPath, "/tmp/run.sh",
	})
	want := []string{
		"-n", resolvedPath(systemdRunName), "--scope", "--collect", "--uid=1000", "--unit=maestro-workers-1-2",
		"--slice=" + workerSlice,
		"--setenv=PATH=/usr/local/bin:/usr/bin:/bin",
		resolvedPath(tmuxName), "-L", privateSocket,
		"new-session", "-d", "-s", "maestro-ok-player-1", "-c", "/tmp/worktree", bashPath, "/tmp/run.sh",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scope argv = %#v, want %#v", got, want)
	}
}

func TestStartDetachedUsesSiblingSystemdScope(t *testing.T) {
	if os.Getenv("MAESTRO_SYSTEMD_INTEGRATION") != "1" {
		t.Skip("set MAESTRO_SYSTEMD_INTEGRATION=1 on a systemd host")
	}

	session := fmt.Sprintf("maestro-scope-it-%d", time.Now().UnixNano())
	runner := t.TempDir() + "/run.sh"
	if err := os.WriteFile(runner, []byte("#!/usr/bin/bash\nexec /usr/bin/sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INVOCATION_ID", "maestro-tmux-integration")
	lease, err := WorkerProcessLease("BeFeast/maestro", session, 1)
	if err != nil {
		t.Fatal(err)
	}
	out, err := StartDetached(session, t.TempDir(), runner, lease)
	if err != nil {
		t.Fatalf("StartDetached: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = TerminateProcessLease(lease)
		_, _ = KillSession(session)
	})

	pidBytes, err := PanePID(session)
	if err != nil {
		t.Fatalf("PanePID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse pane pid %q: %v", pidBytes, err)
	}
	cgroup, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		t.Fatalf("read pane cgroup: %v", err)
	}
	if !strings.Contains(string(cgroup), "/maestro-workers-") || strings.Contains(string(cgroup), "/maestro.service") {
		t.Fatalf("pane cgroup = %q, want sibling maestro-workers scope", cgroup)
	}
}

func TestExactTargetCannotPrefixMatchAnotherWorker(t *testing.T) {
	if got, want := exactTarget("maestro-ok-player-26"), "=maestro-ok-player-26:"; got != want {
		t.Fatalf("exactTarget = %q, want %q", got, want)
	}
}

func TestWorkerProcessLeaseIsUniquePerSlotAndGeneration(t *testing.T) {
	t.Setenv("INVOCATION_ID", "maestro-test")
	first, err := WorkerProcessLease("BeFeast/maestro", "sup-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	same, err := WorkerProcessLease("BeFeast/maestro", "sup-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	neighbor, err := WorkerProcessLease("BeFeast/maestro", "sup-2", 1)
	if err != nil {
		t.Fatal(err)
	}
	next, err := WorkerProcessLease("BeFeast/maestro", "sup-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if first != same {
		t.Fatalf("same worker generation produced different leases: %+v vs %+v", first, same)
	}
	if first.Unit == neighbor.Unit || first.Unit == next.Unit || neighbor.Unit == next.Unit {
		t.Fatalf("worker leases collided: first=%q neighbor=%q next=%q", first.Unit, neighbor.Unit, next.Unit)
	}
	if first.Manager != ProcessLeaseManagerSystem {
		t.Fatalf("production manager = %q, want system", first.Manager)
	}
}

func TestProcessLeaseLaunchCommandUsesExactSystemScope(t *testing.T) {
	lease := ProcessLease{Unit: "maestro-worker-0123456789abcdef0123456789abcdef-g7.scope", Manager: ProcessLeaseManagerSystem}
	got, err := processLeaseLaunchCommand(lease, "/state/sup-7-run.sh", 1000, "/usr/local/bin:/usr/bin:/bin")
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{resolvedPath(sudoName), "-n", resolvedPath(systemdRunName), "--uid=1000", "--scope", "--collect", "--quiet"}
	if len(got) < len(wantPrefix) || !reflect.DeepEqual(got[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("system lease argv prefix = %#v, want %#v", got, wantPrefix)
	}
	for _, want := range []string{
		"--unit=" + lease.Unit,
		"--slice=" + workerSlice,
		"--property=KillMode=control-group",
		"--setenv=PATH=/usr/local/bin:/usr/bin:/bin",
		bashPath,
		"/state/sup-7-run.sh",
	} {
		if !containsArg(got, want) {
			t.Errorf("system lease argv missing %q: %v", want, got)
		}
	}
	if containsArg(got, "--user") {
		t.Fatalf("production system scope must not use --user: %v", got)
	}
}

func TestTerminateProcessLeaseEscalatesOnlyExactUnit(t *testing.T) {
	lease := ProcessLease{Unit: "maestro-worker-0123456789abcdef0123456789abcdef-g2.scope", Manager: ProcessLeaseManagerUser}
	neighbor := "maestro-worker-fedcba9876543210fedcba9876543210-g2.scope"
	var calls [][]string
	forceKilled := false
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := append([]string{name}, args...)
		calls = append(calls, call)
		joined := strings.Join(call, " ")
		if strings.Contains(joined, neighbor) {
			t.Fatalf("teardown touched neighboring lease: %v", call)
		}
		if strings.Contains(joined, "--signal=SIGKILL") {
			forceKilled = true
			return nil, nil
		}
		if strings.Contains(joined, " show ") {
			if forceKilled {
				return []byte("inactive\n"), nil
			}
			return []byte("active\n"), nil
		}
		return nil, nil
	}
	if err := terminateProcessLease(context.Background(), lease, 3*time.Millisecond, 20*time.Millisecond, time.Millisecond, run); err != nil {
		t.Fatal(err)
	}
	joined := make([]string, 0, len(calls))
	for _, call := range calls {
		joined = append(joined, strings.Join(call, " "))
	}
	all := strings.Join(joined, "\n")
	termAt := strings.Index(all, "--signal=SIGTERM")
	killAt := strings.Index(all, "--signal=SIGKILL")
	if termAt < 0 || killAt < 0 || termAt > killAt {
		t.Fatalf("teardown calls did not TERM then KILL exact lease:\n%s", all)
	}
}

func TestTerminateProcessLeaseStopsAfterGracefulExit(t *testing.T) {
	lease := ProcessLease{Unit: "maestro-worker-0123456789abcdef0123456789abcdef-g4.scope", Manager: ProcessLeaseManagerUser}
	termSent := false
	forceSent := false
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		joined := strings.Join(append([]string{name}, args...), " ")
		if strings.Contains(joined, "--signal=SIGTERM") {
			termSent = true
			return nil, nil
		}
		if strings.Contains(joined, "--signal=SIGKILL") {
			forceSent = true
			return nil, nil
		}
		if strings.Contains(joined, " show ") {
			if termSent {
				return []byte("inactive\n"), nil
			}
			return []byte("active\n"), nil
		}
		return nil, nil
	}
	if err := terminateProcessLease(context.Background(), lease, time.Second, time.Second, time.Millisecond, run); err != nil {
		t.Fatal(err)
	}
	if !termSent || forceSent {
		t.Fatalf("graceful teardown term=%v kill=%v, want TERM only", termSent, forceSent)
	}
}

func TestTerminateProcessLeaseAlreadyCollectedIsNoop(t *testing.T) {
	lease := ProcessLease{Unit: "maestro-worker-0123456789abcdef0123456789abcdef-g3.scope", Manager: ProcessLeaseManagerSystem}
	var calls int
	run := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		calls++
		return []byte("inactive\n"), nil
	}
	if err := terminateProcessLease(context.Background(), lease, time.Second, time.Second, time.Millisecond, run); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("already-collected teardown made %d calls, want one liveness check", calls)
	}
}

func TestTerminateProcessLeaseRejectsSharedOrArbitraryUnit(t *testing.T) {
	for _, unit := range []string{"maestro.service", workerSlice, "maestro-workers-1.scope", ""} {
		calls := 0
		run := func(context.Context, string, ...string) ([]byte, error) {
			calls++
			return nil, nil
		}
		err := terminateProcessLease(context.Background(), ProcessLease{Unit: unit, Manager: ProcessLeaseManagerSystem}, time.Second, time.Second, time.Millisecond, run)
		if err == nil {
			t.Errorf("unit %q passed exact worker lease validation", unit)
		}
		if calls != 0 {
			t.Errorf("unit %q reached systemctl before validation", unit)
		}
	}
}

func TestProcessLeaseIntegrationReparentedChildAndNeighborIsolation(t *testing.T) {
	if os.Getenv("MAESTRO_SYSTEMD_INTEGRATION") != "1" {
		t.Skip("set MAESTRO_SYSTEMD_INTEGRATION=1 on a systemd host")
	}
	t.Setenv("INVOCATION_ID", "maestro-worker-lease-integration")

	type runtime struct {
		session string
		lease   ProcessLease
		child   string
	}
	start := func(slot string) runtime {
		t.Helper()
		dir := t.TempDir()
		childPID := dir + "/child.pid"
		childScript := dir + "/child.sh"
		if err := os.WriteFile(childScript, []byte(fmt.Sprintf("#!/usr/bin/bash\ntrap '' HUP TERM\necho $BASHPID > %s\nwhile true; do /usr/bin/sleep 1; done\n", childPID)), 0o700); err != nil {
			t.Fatal(err)
		}
		runner := dir + "/run.sh"
		runnerBody := fmt.Sprintf("#!/usr/bin/bash\n( /usr/bin/setsid %s </dev/null >/dev/null 2>&1 & ) &\nwhile true; do /usr/bin/sleep 1; done\n", childScript)
		if err := os.WriteFile(runner, []byte(runnerBody), 0o700); err != nil {
			t.Fatal(err)
		}
		lease, err := WorkerProcessLease("BeFeast/maestro", slot, 1)
		if err != nil {
			t.Fatal(err)
		}
		session := "maestro-lease-it-" + slot + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		out, err := StartDetached(session, dir, runner, lease)
		if err != nil {
			t.Fatalf("StartDetached %s: %v\n%s", slot, err, out)
		}
		return runtime{session: session, lease: lease, child: childPID}
	}

	worker := start("worker")
	neighbor := start("neighbor")
	t.Cleanup(func() {
		_ = TerminateProcessLease(worker.lease)
		_ = TerminateProcessLease(neighbor.lease)
		_, _ = KillSession(worker.session)
		_, _ = KillSession(neighbor.session)
	})

	workerPID := waitPIDFile(t, worker.child)
	neighborPID := waitPIDFile(t, neighbor.child)
	assertPIDInLease(t, workerPID, worker.lease)
	assertPIDInLease(t, neighborPID, neighbor.lease)

	// Remove the pane first. The reparented setsid child must still be owned by
	// the cgroup lease even though tmux can no longer identify it by ancestry.
	if out, err := KillSession(worker.session); err != nil {
		t.Fatalf("kill worker tmux: %v: %s", err, out)
	}
	if !pidAlive(workerPID) {
		t.Fatal("reparented child died with tmux; integration did not exercise lease-only teardown")
	}
	if err := TerminateProcessLease(worker.lease); err != nil {
		t.Fatal(err)
	}
	waitPIDDead(t, workerPID)
	if !pidAlive(neighborPID) {
		t.Fatal("neighboring worker was killed by exact lease teardown")
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func waitPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pid file %s", path)
	return 0
}

func assertPIDInLease(t *testing.T, pid int, lease ProcessLease) {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), lease.Unit) {
		t.Fatalf("pid %d cgroup = %q, want exact lease %q", pid, data, lease.Unit)
	}
}

func waitPIDDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive", pid)
}

func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
