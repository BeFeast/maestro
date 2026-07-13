package selfdeploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoFile reads a file relative to the repo root (internal/selfdeploy -> ../..).
func repoFile(t *testing.T, rel ...string) string {
	t.Helper()
	parts := append([]string{"..", ".."}, rel...)
	p, err := filepath.Abs(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("resolve %v: %v", rel, err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(data)
}

// #877: kill ownership must be explicit and safe. KillMode=mixed sends the stop
// SIGTERM to the daemon's main process only (so the in-process drain genuinely
// waits for live workers), while the final SIGKILL still reaps the whole control
// group (no orphans). KillMode=process was rejected in review precisely because
// it would leak service children out of the systemd lifecycle.
func TestMaestroServiceKillOwnershipExplicit(t *testing.T) {
	unit := repoFile(t, "maestro.service")

	// Inspect the ACTIVE KillMode directive(s) only — comments may legitimately
	// reference control-group/process while explaining why they are not used.
	var active []string
	for _, line := range strings.Split(unit, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "KillMode=") {
			active = append(active, strings.TrimPrefix(trimmed, "KillMode="))
		}
	}
	if len(active) != 1 || active[0] != "mixed" {
		t.Errorf("active KillMode directives = %v, want exactly [mixed] — the drain must wait for live workers, not race a cgroup-wide SIGTERM, and must never use process (leaks children outside the lifecycle, rejected in #877 review)", active)
	}
	// Ownership is documented, not just set.
	if !strings.Contains(unit, "#877") || !strings.Contains(unit, "cgroup") {
		t.Error("maestro.service must document the kill/cgroup ownership rationale (#877)")
	}
}

// #877: a fix that requires a unit change (KillMode) lives in the unit file, not
// the binary, so the self-deploy script must ship the unit alongside the binary
// — install it over the live FragmentPath, daemon-reload, and roll it back on
// failure — instead of replacing only the binary (P1 #877 review).
func TestSelfDeployScriptShipsUnitChanges(t *testing.T) {
	script := repoFile(t, "scripts", "self-deploy.sh")

	for _, want := range []string{
		"apply_units",     // the install step
		"rollback_units",  // the rollback step
		"daemon-reload",   // reload so the restart adopts the new unit
		"FragmentPath",    // authoritative live install destination
		"install -m 0644", // installs the repo unit file
	} {
		if !strings.Contains(script, want) {
			t.Errorf("self-deploy.sh must ship unit changes — missing %q", want)
		}
	}

	// apply_units must run before the restart (so the restart adopts the new
	// unit), and rollback() must restore units before restarting on failure.
	applyIdx := strings.Index(script, "\napply_units\n")
	restartIdx := strings.Index(script, "restart_units \"$RESTART_BUDGET\"")
	if applyIdx < 0 || restartIdx < 0 || applyIdx > restartIdx {
		t.Error("apply_units must run before the unit restart so the restart adopts the shipped unit change (#877)")
	}
	if !strings.Contains(script, "rollback_units\n  if [[ ! -f \"$BIN.prev\" ]]") {
		t.Error("rollback() must restore units (rollback_units) before the binary rollback path (#877)")
	}
}
