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

	cleanupHelper := filepath.Join(filepath.Dir(root), "lease-cleanup.sh")
	cleanupScript := fmt.Sprintf(`#!/bin/bash
manifest=""
lease=""
shift
while [ "$#" -gt 0 ]; do
  case "$1" in
    --manifest) manifest="$2"; shift 2 ;;
    --lease) lease="$2"; shift 2 ;;
    *) exit 2 ;;
  esac
done
dir="${manifest%%/lease.json}"
case "$dir" in
  %s/mw-*) /usr/bin/rm -rf -- "$dir" ;;
  *) exit 3 ;;
esac
`, root)
	if err := os.WriteFile(cleanupHelper, []byte(cleanupScript), 0o700); err != nil {
		t.Fatal(err)
	}

	childPIDFile := filepath.Join(target.ScratchDir, "reparented.pid")
	targetPayload := filepath.Join(target.ScratchDir, "payload.sh")
	targetScript := fmt.Sprintf(`#!/bin/bash
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

	start := func(lease Lease, payload string) *exec.Cmd {
		binary, args, err := BuildLaunchCommand(LaunchSpec{
			Lease: lease, UID: os.Getuid(), PATH: os.Getenv("PATH"),
			PayloadPath: payload, MaestroBin: cleanupHelper,
		})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, args...)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start %s: %v", lease.Unit, err)
		}
		return cmd
	}
	targetCmd := start(target, targetPayload)
	neighborCmd := start(neighbor, neighborPayload)
	t.Cleanup(func() {
		_ = Stop(ScopeSystem, target.Unit)
		_ = Stop(ScopeSystem, neighbor.Unit)
		_ = targetCmd.Wait()
		_ = neighborCmd.Wait()
	})

	waitForLeaseCondition(t, 15*time.Second, func() bool {
		active, _ := Active(ScopeSystem, target.Unit)
		neighborActive, _ := Active(ScopeSystem, neighbor.Unit)
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
		ppid, err := procPPID(childPID)
		return err == nil && ppid == 1
	}, "descendant reparented to pid 1")

	binary, args, err := systemctlCommand(ScopeSystem, "kill", "--kill-whom=all", "--signal=SIGKILL", target.Unit)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(binary, args...).CombinedOutput(); err != nil {
		t.Fatalf("force-kill target lease: %v\n%s", err, out)
	}
	waitForLeaseCondition(t, 15*time.Second, func() bool {
		active, _ := Active(ScopeSystem, target.Unit)
		return !active && !processExists(childPID)
	}, "target lease and reparented descendant gone")
	waitForLeaseCondition(t, 15*time.Second, func() bool {
		_, err := os.Stat(target.ScratchDir)
		return os.IsNotExist(err)
	}, "target scratch cleaned by ExecStopPost")

	neighborActive, err := Active(ScopeSystem, neighbor.Unit)
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
