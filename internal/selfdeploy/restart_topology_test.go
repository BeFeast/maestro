package selfdeploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
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

// activeUnitDirectives returns the active key/value directives in section.
// It joins backslash-continuation lines so callers inspect the effective
// ExecStart rather than matching comments or disconnected fragments.
func activeUnitDirectives(unit, section string) map[string][]string {
	directives := make(map[string][]string)
	lines := strings.Split(unit, "\n")
	activeSection := ""
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			activeSection = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			continue
		}
		if activeSection != section || line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		logical := line
		for strings.HasSuffix(logical, `\`) && i+1 < len(lines) {
			logical = strings.TrimSpace(strings.TrimSuffix(logical, `\`))
			i++
			next := strings.TrimSpace(lines[i])
			if next == "" || strings.HasPrefix(next, "#") {
				continue
			}
			logical += " " + next
		}

		key, value, ok := strings.Cut(logical, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		directives[key] = append(directives[key], strings.TrimSpace(value))
	}
	return directives
}

func requireSingleUnitDirective(t *testing.T, directives map[string][]string, key, want string) {
	t.Helper()
	got := directives[key]
	if len(got) != 1 || got[0] != want {
		t.Errorf("active %s directives = %q, want exactly [%q]", key, got, want)
	}
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

// #953: maestro.service is a system-scoped unit. Without an explicit User and
// home/store path, systemd expands %h for root and self-deploy boots an empty
// fleet from /root/.maestro instead of the operator's live config store.
func TestMaestroServiceUsesDeterministicRuntimeIdentity(t *testing.T) {
	unit := repoFile(t, "maestro.service")
	service := activeUnitDirectives(unit, "Service")
	requireSingleUnitDirective(t, service, "User", "god")
	requireSingleUnitDirective(t, service, "WorkingDirectory", "/home/god")

	const wantPATH = "PATH=/home/god/.local/bin:/home/god/.bun/bin:/home/god/.npm-global/bin:/usr/local/bin:/usr/bin:/bin"
	var paths []string
	for _, environment := range service["Environment"] {
		for _, assignment := range strings.Fields(environment) {
			if strings.HasPrefix(assignment, "PATH=") {
				paths = append(paths, assignment)
			}
		}
	}
	if len(paths) != 1 || paths[0] != wantPATH {
		t.Errorf("active PATH assignments = %q, want exactly [%q]", paths, wantPATH)
	}

	execStarts := service["ExecStart"]
	if len(execStarts) != 1 {
		t.Fatalf("active ExecStart directives = %q, want exactly one", execStarts)
	}
	var stores []string
	args := strings.Fields(execStarts[0])
	for i, arg := range args {
		if arg == "--store" && i+1 < len(args) {
			stores = append(stores, args[i+1])
		}
	}
	if len(stores) != 1 || stores[0] != "/home/god/.maestro/maestro.db" {
		t.Errorf("active ExecStart --store values = %q, want exactly [/home/god/.maestro/maestro.db]", stores)
	}

	for key, values := range service {
		for _, value := range values {
			if strings.Contains(value, "%h") || strings.Contains(value, "/root/.maestro") {
				t.Errorf("active %s directive must not derive runtime paths from root or manager HOME: %q", key, value)
			}
		}
	}

	install := activeUnitDirectives(unit, "Install")
	requireSingleUnitDirective(t, install, "WantedBy", "multi-user.target")
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

// #966: the blocking systemctl restart step has its own drain-sized budget. A
// wedged restart must return a specific Fleet-unavailable diagnosis instead of
// hiding under the much larger overall deploy timeout.
func TestRestartUnitsReportsBoundedDrainBudgetOverrun(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	stubDir := t.TempDir()
	writeExec(t, filepath.Join(stubDir, "systemctl"), "#!/bin/bash\nsleep 30\n")

	script := repoFile(t, "scripts", "self-deploy.sh")
	harness := strings.Join([]string{
		"set -euo pipefail",
		"PATH=" + shellQuote(stubDir) + ":$PATH",
		`log() { :; }`,
		`SCOPE="user"`,
		`UNIT_LIST=(maestro.service)`,
		`RESTART_FAIL_DETAIL=""`,
		`RESTART_TIMED_OUT=0`,
		extractShellFunc(t, script, "restart_units"),
		`if restart_units 1; then echo "restart unexpectedly succeeded" >&2; exit 1; fi`,
		`(( RESTART_TIMED_OUT == 1 )) || { echo "restart timeout flag not set" >&2; exit 1; }`,
		`[[ "$RESTART_FAIL_DETAIL" == *"bounded drain/restart budget"* ]] || { echo "missing bounded-budget diagnosis: $RESTART_FAIL_DETAIL" >&2; exit 1; }`,
		`[[ "$RESTART_FAIL_DETAIL" == *"Fleet may be unavailable"* ]] || { echo "missing Fleet impact: $RESTART_FAIL_DETAIL" >&2; exit 1; }`,
	}, "\n")

	started := time.Now()
	out, err := exec.Command("bash", "-c", harness).CombinedOutput()
	if err != nil {
		t.Fatalf("restart timeout harness failed: %v\n%s", err, out)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("restart helper waited %v, want the 1s restart budget rather than the deploy timeout", elapsed)
	}
	if !strings.Contains(script, `if (( RESTART_TIMED_OUT )); then`) || !strings.Contains(script, `fail_restart_timeout "$RESTART_FAIL_DETAIL"`) {
		t.Fatal("timed-out restart must report immediately without entering the 10-minute rollback restart path")
	}
}

func TestRestartUnitsSharesOneBudgetAcrossConfiguredUnits(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	stubDir := t.TempDir()
	writeExec(t, filepath.Join(stubDir, "systemctl"), `#!/bin/bash
shift 2
for unit in "$@"; do
  sleep 0.75
done
`)

	script := repoFile(t, "scripts", "self-deploy.sh")
	harness := strings.Join([]string{
		"set -euo pipefail",
		"PATH=" + shellQuote(stubDir) + ":$PATH",
		`log() { :; }`,
		`SCOPE="user"`,
		`UNIT_LIST=(maestro.service maestro-fleet.service)`,
		`RESTART_FAIL_DETAIL=""`,
		`RESTART_TIMED_OUT=0`,
		extractShellFunc(t, script, "restart_units"),
		`if restart_units 1; then echo "restart unexpectedly received a fresh budget per unit" >&2; exit 1; fi`,
		`(( RESTART_TIMED_OUT == 1 )) || { echo "shared restart timeout flag not set" >&2; exit 1; }`,
	}, "\n")

	started := time.Now()
	out, err := exec.Command("bash", "-c", harness).CombinedOutput()
	if err != nil {
		t.Fatalf("multi-unit restart timeout harness failed: %v\n%s", err, out)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("multi-unit restart used more than one shared 1s budget: %v", elapsed)
	}
}
