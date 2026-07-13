package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #877: a self-deploy `systemctl restart maestro.service` must not kill
// in-flight tmux workers before the daemon's in-process drain completes. The
// root cause was the shipped unit relying on the systemd default
// KillMode=control-group, which SIGTERM/SIGKILLs the WHOLE cgroup — including
// worker tmux servers spawned as daemon descendants. These tests pin the
// production unit topology so a future edit cannot silently reintroduce the
// cgroup-sweep hazard.
func readServiceUnit(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "maestro.service")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(data)
}

func TestServiceUnit_KillModeIsProcessNotControlGroup(t *testing.T) {
	unit := readServiceUnit(t)

	// The unit must declare KillMode=process explicitly: the stop/restart kill
	// is scoped to the daemon's main process, so worker tmux servers are never
	// swept out of the cgroup by a self-deploy restart.
	if !unitHasDirective(unit, "KillMode", "process") {
		t.Fatalf("maestro.service must declare KillMode=process (explicit kill ownership, #877)")
	}

	// The default control-group kill is exactly the hazard #877 fixed; it must
	// not be reintroduced.
	if unitHasDirective(unit, "KillMode", "control-group") {
		t.Fatalf("maestro.service must NOT use KillMode=control-group — it sweeps in-flight workers (#877)")
	}
}

func TestServiceUnit_DrainTopologyIntact(t *testing.T) {
	unit := readServiceUnit(t)

	// The in-process drain needs room before systemd escalates to SIGKILL, and
	// there is no per-unit ExecStop=maestro drain in the single-service world.
	// Scan directives, not comments (the rationale comment mentions ExecStop).
	if !unitHasKey(unit, "TimeoutStopSec") {
		t.Fatalf("maestro.service must set TimeoutStopSec to give the in-process drain room before SIGKILL")
	}
	if unitHasKey(unit, "ExecStop") {
		t.Fatalf("maestro.service must not declare ExecStop — the single-service daemon drains in-process on SIGTERM (#761)")
	}
}

func TestServiceUnit_DocumentsCgroupOwnership(t *testing.T) {
	unit := readServiceUnit(t)

	// Acceptance criterion: KillMode / scope ownership is explicit and
	// documented. The unit must reference the issue and the out-of-cgroup scope
	// mechanism so the topology is self-explaining to an operator.
	if !strings.Contains(unit, "#877") {
		t.Fatalf("maestro.service should document the cgroup/kill ownership rationale (#877)")
	}
	if !strings.Contains(unit, "systemd-run") && !strings.Contains(unit, "scope") {
		t.Fatalf("maestro.service should document that worker tmux servers run in a scope outside the unit cgroup")
	}
}

// unitHasDirective reports whether a systemd unit sets key=value on a
// non-comment line (comment lines start with '#').
func unitHasDirective(unit, key, value string) bool {
	want := key + "=" + value
	for _, line := range strings.Split(unit, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == want {
			return true
		}
	}
	return false
}

// unitHasKey reports whether a systemd unit assigns key= on any non-comment
// line (a directive), ignoring comments that merely mention the key.
func unitHasKey(unit, key string) bool {
	for _, line := range strings.Split(unit, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, key+"=") {
			return true
		}
	}
	return false
}
