package workerlease

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/tmuxsession"
)

// TestSystemdLeaseForcedKillReapsReparentedDescendantAndKeepsNeighbor is a
// production-topology integration test. It is opt-in because it requires a
// systemd host and the same passwordless system-manager boundary used by the
// shipped maestro.service.
func TestSystemdLeaseForcedKillReapsReparentedDescendantAndKeepsNeighbor(t *testing.T) {
	if os.Getenv("MAESTRO_SYSTEMD_INTEGRATION") != "1" {
		t.Skip("set MAESTRO_SYSTEMD_INTEGRATION=1 on a systemd host")
	}
	root := filepath.Join(diskTestDir(t), "scratch")
	target := prepareTestLease(t, root, "forced-kill")
	neighbor := prepareTestLease(t, root, "neighbor")
	if err := EnsureWorkerSlice(ScopeSystem); err != nil {
		t.Fatal(err)
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// NOT t.TempDir(): that follows TMPDIR, which is /tmp by default, and every
	// lease binds its private scratch over /tmp for the unit. ExecStopPost then
	// runs in a namespace where the host /tmp is shadowed and systemd fails the
	// step with "Unable to locate executable", so the scratch is never cleaned
	// and the test blames the production path for its own setup. diskTestDir
	// puts the binary next to the repo, which the namespace can still see;
	// production is unaffected because maestroBin is /usr/local/bin/maestro.
	maestroBin := filepath.Join(diskTestDir(t), "maestro")
	build := exec.Command("go", "build", "-o", maestroBin, "./cmd/maestro/")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build integration cleanup binary: %v\n%s", err, out)
	}

	childPIDFile := filepath.Join(target.ScratchDir, "reparented.pid")
	runtimeEnvFile := filepath.Join(target.TempDir, "runtime-env")
	targetPayload := filepath.Join(target.ScratchDir, "payload.sh")
	targetScript := fmt.Sprintf(`#!/bin/bash
printf '%%s\n' "$TMPDIR|$TMP|$TEMP|$GOTMPDIR|$CARGO_TARGET_DIR" > /tmp/runtime-env
(
  (
    /usr/bin/setsid /usr/bin/bash -c 'echo $$ > %s; while :; do /usr/bin/sleep 1; done' </dev/null >/dev/null 2>&1 &
  ) &
) &
exec /usr/bin/sleep 300
`, shellQuote(childPIDFile))
	if err := os.WriteFile(targetPayload, []byte(targetScript), 0o700); err != nil {
		t.Fatal(err)
	}
	neighborPayload := filepath.Join(neighbor.ScratchDir, "payload.sh")
	if err := os.WriteFile(neighborPayload, []byte("#!/bin/bash\nexec /usr/bin/sleep 300\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("INVOCATION_ID", "maestro-worker-scratch-integration")
	start := func(lease Lease, payload, session string) tmuxsession.ProcessLease {
		processLease := tmuxsession.ProcessLease{
			Unit:    lease.Unit,
			Manager: lease.Scope,
			Runtime: &tmuxsession.ProcessLeaseRuntime{
				ScratchID: lease.ID, ScratchDir: lease.ScratchDir, TempDir: lease.TempDir,
				GoTempDir: lease.GoTempDir, CargoTarget: lease.CargoTarget, ManifestPath: lease.ManifestPath,
				CleanupExec: CleanupExec(maestroBin, lease.ManifestPath, lease.ID),
			},
		}
		out, err := tmuxsession.StartDetached(session, lease.ScratchDir, payload, processLease)
		if err != nil {
			t.Fatalf("start %s: %v\n%s", lease.Unit, err, out)
		}
		return processLease
	}
	targetSession := fmt.Sprintf("maestro-scratch-target-%d", time.Now().UnixNano())
	neighborSession := fmt.Sprintf("maestro-scratch-neighbor-%d", time.Now().UnixNano())
	targetProcessLease := start(target, targetPayload, targetSession)
	neighborProcessLease := start(neighbor, neighborPayload, neighborSession)
	t.Cleanup(func() {
		_ = tmuxsession.TerminateProcessLease(targetProcessLease)
		_ = tmuxsession.TerminateProcessLease(neighborProcessLease)
		_, _ = tmuxsession.KillSession(targetSession)
		_, _ = tmuxsession.KillSession(neighborSession)
	})

	waitForLeaseCondition(t, 15*time.Second, func() bool {
		active, _ := tmuxsession.ProcessLeaseActive(targetProcessLease)
		neighborActive, _ := tmuxsession.ProcessLeaseActive(neighborProcessLease)
		return active && neighborActive
	}, "both worker leases active")

	var childPID int
	waitForLeaseCondition(t, 10*time.Second, func() bool {
		data, err := os.ReadFile(childPIDFile)
		if err != nil {
			return false
		}
		childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && childPID > 0
	}, "reparented descendant pid")
	waitForLeaseCondition(t, 10*time.Second, func() bool {
		data, err := os.ReadFile(runtimeEnvFile)
		return err == nil && strings.TrimSpace(string(data)) == "/tmp|/tmp|/tmp|/tmp/go|/tmp/cargo-target"
	}, "private tmp mount and temp environment")
	waitForLeaseCondition(t, 10*time.Second, func() bool {
		ppid, err := procPPID(childPID)
		return err == nil && ppid == 1
	}, "descendant reparented to pid 1")

	if err := tmuxsession.TerminateProcessLease(targetProcessLease); err != nil {
		t.Fatalf("terminate target lease: %v", err)
	}
	waitForLeaseCondition(t, 15*time.Second, func() bool {
		active, _ := tmuxsession.ProcessLeaseActive(targetProcessLease)
		return !active && !processExists(childPID)
	}, "target lease and reparented descendant gone")
	waitForLeaseCondition(t, 15*time.Second, func() bool {
		_, err := os.Stat(target.ScratchDir)
		return os.IsNotExist(err)
	}, "target scratch cleaned by ExecStopPost")

	neighborActive, err := tmuxsession.ProcessLeaseActive(neighborProcessLease)
	if err != nil || !neighborActive {
		t.Fatalf("neighboring worker did not survive target force-kill: active=%v err=%v", neighborActive, err)
	}
	if _, err := os.Stat(neighbor.ManifestPath); err != nil {
		t.Fatalf("neighbor scratch was removed: %v", err)
	}
}

func waitForLeaseCondition(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid)))
	return err == nil
}

func procPPID(pid int) (int, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	text := string(data)
	end := strings.LastIndex(text, ")")
	if end < 0 {
		return 0, fmt.Errorf("malformed proc stat")
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed proc stat fields")
	}
	return strconv.Atoi(fields[1])
}
