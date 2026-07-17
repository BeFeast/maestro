package tmuxsession

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestScopeLaunchArgsUsesSiblingSystemdScopeAndPrivateSocket(t *testing.T) {
	got := scopeLaunchArgs("maestro-workers-1-2", 1000, "/usr/local/bin:/usr/bin:/bin", []string{
		"new-session", "-d", "-s", "maestro-ok-player-1", "-c", "/tmp/worktree", bashPath, "/tmp/run.sh",
	})
	want := []string{
		"-n", resolvedPath(systemdRunName), "--scope", "--collect", "--uid=1000", "--unit=maestro-workers-1-2",
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
	out, err := StartDetached(session, t.TempDir(), runner)
	if err != nil {
		t.Fatalf("StartDetached: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = KillSession(session) })

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
