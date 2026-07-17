package selfdeploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// extractShellFunc pulls a `name() { ... }` block (closing brace in column 0)
// out of the script so the two unit helpers can be exercised in isolation.
func extractShellFunc(t *testing.T, script, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + `\(\) \{.*?^\}`)
	body := re.FindString(script)
	if body == "" {
		t.Fatalf("could not extract shell function %q from self-deploy.sh", name)
	}
	return body
}

// runUnitHarness runs apply_units/rollback_units from the real script against a
// temp "system" dir with priv/installed_unit_path/daemon_reload/log/fail stubbed,
// then runs `checks` (bash asserting the filesystem outcome).
func runUnitHarness(t *testing.T, dest string, destExists bool, checks string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script := repoFile(t, "scripts", "self-deploy.sh")
	build := t.TempDir()
	if err := os.WriteFile(filepath.Join(build, "myunit.service"), []byte("NEW UNIT v2\n"), 0644); err != nil {
		t.Fatalf("write repo unit: %v", err)
	}
	if destExists {
		if err := os.WriteFile(dest, []byte("OLD UNIT v1\n"), 0644); err != nil {
			t.Fatalf("seed existing unit: %v", err)
		}
	}

	harness := strings.Join([]string{
		"set -euo pipefail",
		`priv() { "$@"; }`,
		"installed_unit_path() { echo " + shellQuote(dest) + "; }",
		"daemon_reload() { return 0; }",
		"log() { :; }",
		`fail() { echo "FAIL: $*" >&2; exit 1; }`,
		"UNIT_LIST=(myunit.service)",
		"BUILD_DIR=" + shellQuote(build),
		"APPLIED_UNITS=()",
		extractShellFunc(t, script, "apply_units"),
		extractShellFunc(t, script, "rollback_units"),
		checks,
	}, "\n")

	cmd := exec.Command("bash", "-c", harness)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("unit harness failed: %v\n%s", err, out)
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// #877 review comment 4: when apply_units installs a unit that did NOT exist
// before (no <dest>.prev backup), a later rollback must REMOVE the newly-
// installed unit — otherwise rollback reverts only the binary and leaves the new
// unit + previous-revision binary from different revisions live.
func TestApplyUnitsRollbackRemovesNewlyInstalledUnit(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "myunit.service") // does NOT exist yet
	runUnitHarness(t, dest, false, strings.Join([]string{
		"apply_units",
		`[[ -f ` + shellQuote(dest) + ` ]] || { echo "apply did not install the new unit"; exit 1; }`,
		`[[ -f ` + shellQuote(dest+".prev.absent") + ` ]] || { echo "apply did not record the absent-marker"; exit 1; }`,
		`[[ ! -f ` + shellQuote(dest+".prev") + ` ]] || { echo "apply must not fabricate a .prev for a new unit"; exit 1; }`,
		"rollback_units",
		`[[ ! -f ` + shellQuote(dest) + ` ]] || { echo "rollback did NOT remove the newly-installed unit"; exit 1; }`,
		`[[ ! -f ` + shellQuote(dest+".prev.absent") + ` ]] || { echo "rollback left the absent-marker behind"; exit 1; }`,
	}, "\n"))
}

// The existing-unit path is unchanged: apply_units backs the prior file up as
// <dest>.prev and rollback restores it.
func TestApplyUnitsRollbackRestoresExistingUnit(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "myunit.service")
	runUnitHarness(t, dest, true, strings.Join([]string{
		"apply_units",
		`[[ "$(cat ` + shellQuote(dest) + `)" == *"NEW UNIT v2"* ]] || { echo "apply did not install the new revision"; exit 1; }`,
		`[[ -f ` + shellQuote(dest+".prev") + ` ]] || { echo "apply did not back up the existing unit"; exit 1; }`,
		`[[ ! -f ` + shellQuote(dest+".prev.absent") + ` ]] || { echo "apply wrongly created an absent-marker for an existing unit"; exit 1; }`,
		"rollback_units",
		`[[ "$(cat ` + shellQuote(dest) + `)" == *"OLD UNIT v1"* ]] || { echo "rollback did not restore the prior unit"; exit 1; }`,
	}, "\n"))
}
