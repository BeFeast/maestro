package selfdeploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
)

func TestReadResultAbsent(t *testing.T) {
	res, err := ReadResult(t.TempDir())
	if err != nil {
		t.Fatalf("ReadResult: %v", err)
	}
	if res != nil {
		t.Fatalf("ReadResult = %+v, want nil", res)
	}
}

func TestReadAndClearResult(t *testing.T) {
	dir := t.TempDir()
	payload := `{"status":"deployed","version":"1.4.2+gabc1234","expected_sha":"abc1234","pr":698,"finished_at":"2026-06-12T10:00:00Z"}`
	if err := os.WriteFile(ResultPath(dir), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ReadResult(dir)
	if err != nil {
		t.Fatalf("ReadResult: %v", err)
	}
	if res == nil || res.Status != StatusDeployed || res.Version != "1.4.2+gabc1234" || res.PR != 698 {
		t.Fatalf("ReadResult = %+v", res)
	}

	if err := ClearResult(dir); err != nil {
		t.Fatalf("ClearResult: %v", err)
	}
	if res, err = ReadResult(dir); err != nil || res != nil {
		t.Fatalf("after clear: res=%+v err=%v, want nil/nil", res, err)
	}
	// Clearing twice is fine (consume-once semantics, no error on absent).
	if err := ClearResult(dir); err != nil {
		t.Fatalf("ClearResult twice: %v", err)
	}
}

func TestReadResultMalformed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(ResultPath(dir), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadResult(dir); err == nil {
		t.Fatal("ReadResult on malformed JSON: want error")
	}
}

// #722: the trigger marker round-trips through the state dir so a restarted
// orchestrator can debounce re-triggers.
func TestRecordAndReadLastTrigger(t *testing.T) {
	dir := t.TempDir()

	// No marker yet → not ok.
	if _, _, ok := LastTrigger(dir); ok {
		t.Fatal("LastTrigger ok=true with no marker")
	}

	when := time.Date(2026, 6, 16, 15, 1, 39, 0, time.UTC)
	if err := RecordTrigger(dir, 722, when); err != nil {
		t.Fatalf("RecordTrigger: %v", err)
	}
	got, pr, ok := LastTrigger(dir)
	if !ok {
		t.Fatal("LastTrigger ok=false after RecordTrigger")
	}
	if !got.Equal(when) {
		t.Errorf("LastTrigger time = %s, want %s", got, when)
	}
	if pr != 722 {
		t.Errorf("LastTrigger pr = %d, want 722", pr)
	}
}

// A blank state dir is a no-op (no marker written, nothing read) so callers
// without a state dir don't scribble in the working directory.
func TestTriggerMarkerBlankStateDir(t *testing.T) {
	if err := RecordTrigger("", 1, time.Now().UTC()); err != nil {
		t.Fatalf("RecordTrigger(\"\"): %v", err)
	}
	if _, _, ok := LastTrigger(""); ok {
		t.Fatal("LastTrigger(\"\") ok=true, want false")
	}
}

// A malformed marker fails open (ok=false) so a corrupt file never wedges
// deploys forever.
func TestLastTriggerMalformed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(triggerMarkerPath(dir), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := LastTrigger(dir); ok {
		t.Fatal("LastTrigger ok=true for malformed marker")
	}
}

func triggerTestConfig(t *testing.T) *config.Config {
	t.Helper()
	local := t.TempDir()
	if err := os.MkdirAll(filepath.Join(local, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(local, "scripts", "self-deploy.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		Repo:      "owner/repo",
		LocalPath: local,
		StateDir:  t.TempDir(),
		Server:    config.ServerConfig{Port: 8788},
		SelfDeploy: config.SelfDeployConfig{
			Enabled: true,
			BinPath: "/usr/local/bin/maestro",
		},
	}
}

func TestTriggerCommand(t *testing.T) {
	cfg := triggerTestConfig(t)
	now := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)

	name, args, err := TriggerCommand(cfg, 698, now)
	if err != nil {
		t.Fatalf("TriggerCommand: %v", err)
	}
	if name != "systemd-run" {
		t.Errorf("name = %q", name)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--user",
		"--collect",
		"--unit=maestro-self-deploy-pr698-",
		"--property=WorkingDirectory=" + cfg.LocalPath,
		"/bin/bash " + filepath.Join(cfg.LocalPath, "scripts", "self-deploy.sh"),
		"--bin /usr/local/bin/maestro",
		"--units maestro.service",
		"--result-file " + ResultPath(cfg.StateDir),
		"--timeout-seconds 1800",
		"--restart-timeout-seconds 270",
		"--pr 698",
		"--scope user",
		"--health-url http://127.0.0.1:8788/api/v1/state",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q in:\n%s", want, joined)
		}
	}
}

// #716: scope flows from the self_deploy block to a --scope flag; default user.
func TestTriggerCommandScope(t *testing.T) {
	cfg := triggerTestConfig(t)
	now := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)

	// Default (no scope set) → --scope user.
	_, args, err := TriggerCommand(cfg, 716, now)
	if err != nil {
		t.Fatalf("TriggerCommand: %v", err)
	}
	if !flagValue(args, "--scope", "user") {
		t.Errorf("default scope: want --scope user, got %v", args)
	}

	// Opt-in system scope is forwarded verbatim.
	cfg.SelfDeploy.Scope = "system"
	_, args, err = TriggerCommand(cfg, 716, now)
	if err != nil {
		t.Fatalf("TriggerCommand: %v", err)
	}
	if !flagValue(args, "--scope", "system") {
		t.Errorf("system scope: want --scope system, got %v", args)
	}
}

// #742: the OUTER launcher must match the unit scope. User scope launches
// `systemd-run --user`; system scope launches `sudo -n systemd-run --uid=<user>`
// (no --user) so it works from a system-service context lacking the user bus.
func TestTriggerCommandOuterLauncherScope(t *testing.T) {
	now := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)

	// --- user scope (default): systemd-run --user, no sudo ---
	cfg := triggerTestConfig(t)
	name, args, err := TriggerCommand(cfg, 742, now)
	if err != nil {
		t.Fatalf("TriggerCommand: %v", err)
	}
	if name != "systemd-run" {
		t.Errorf("user-scope launcher = %q, want systemd-run", name)
	}
	if len(args) == 0 || args[0] != "--user" {
		t.Errorf("user-scope first arg = %v, want --user first", args)
	}
	for _, banned := range []string{"-n", "sudo"} {
		if contains(args, banned) {
			t.Errorf("user-scope args must not contain %q: %v", banned, args)
		}
	}
	if hasPrefix(args, "--uid=") {
		t.Errorf("user-scope args must not set --uid: %v", args)
	}

	// --- system scope: sudo -n systemd-run --uid=<user>, no --user ---
	cfg = triggerTestConfig(t)
	cfg.SelfDeploy.Scope = "system"
	name, args, err = TriggerCommand(cfg, 742, now)
	if err != nil {
		t.Fatalf("TriggerCommand: %v", err)
	}
	if name != "sudo" {
		t.Errorf("system-scope launcher = %q, want sudo", name)
	}
	if len(args) < 3 || args[0] != "-n" || args[1] != "systemd-run" {
		t.Errorf("system-scope args = %v, want [-n systemd-run ...] prefix", args)
	}
	if contains(args, "--user") {
		t.Errorf("system-scope must NOT pass --user (root cause of #742): %v", args)
	}
	if !hasPrefix(args, "--uid=") {
		t.Errorf("system-scope must run the unit as the invoking user via --uid: %v", args)
	}
	// The shared unit properties still flow through regardless of scope.
	for _, want := range []string{"--collect", "/bin/bash"} {
		if !contains(args, want) {
			t.Errorf("system-scope args missing %q: %v", want, args)
		}
	}
}

// hasPrefix reports whether any arg starts with the given prefix.
func hasPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

// flagValue reports whether args contains "<flag> <value>" as adjacent items.
func flagValue(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestTriggerCommandInstallViaSudo(t *testing.T) {
	cfg := triggerTestConfig(t)
	now := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)

	// Default off: the flag must not be passed.
	_, args, err := TriggerCommand(cfg, 711, now)
	if err != nil {
		t.Fatalf("TriggerCommand: %v", err)
	}
	if strings.Contains(strings.Join(args, " "), "--install-via-sudo") {
		t.Error("--install-via-sudo passed with install_via_sudo off")
	}

	// Opt-in: the flag is forwarded to the deploy script.
	cfg.SelfDeploy.InstallViaSudo = true
	_, args, err = TriggerCommand(cfg, 711, now)
	if err != nil {
		t.Fatalf("TriggerCommand: %v", err)
	}
	if !contains(args, "--install-via-sudo") {
		t.Errorf("--install-via-sudo missing when enabled: %v", args)
	}
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestTriggerCommandNoHealthURLWithoutServerPort(t *testing.T) {
	cfg := triggerTestConfig(t)
	cfg.Server = config.ServerConfig{}

	_, args, err := TriggerCommand(cfg, 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("TriggerCommand: %v", err)
	}
	if strings.Contains(strings.Join(args, " "), "--health-url") {
		t.Error("expected no --health-url when server port is unset")
	}
}

func TestTriggerCommandMissingScript(t *testing.T) {
	cfg := triggerTestConfig(t)
	cfg.SelfDeploy.Script = filepath.Join(cfg.LocalPath, "does-not-exist.sh")

	if _, _, err := TriggerCommand(cfg, 1, time.Now().UTC()); err == nil {
		t.Fatal("want error for missing script")
	}
}

func TestFindingDeployed(t *testing.T) {
	now := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
	res := &Result{Status: StatusDeployed, Version: "1.4.2+gabc1234", ExpectedSHA: "abc1234def", PR: 698, PrevVersion: "1.4.1"}

	d := Finding(res, "owner/repo", now)
	if d.Status != "succeeded" {
		t.Errorf("Status = %q", d.Status)
	}
	if !strings.Contains(d.Summary, "deployed maestro v1.4.2+gabc1234") || !strings.Contains(d.Summary, "PR #698") {
		t.Errorf("Summary = %q", d.Summary)
	}
	if d.RequiresApproval {
		t.Error("informational finding must not require approval")
	}
	joined := strings.Join(d.Reasons, "\n")
	for _, want := range []string{"1.4.2+gabc1234", "1.4.1", "abc1234def"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Reasons missing %q: %v", want, d.Reasons)
		}
	}
}

func TestFindingRolledBack(t *testing.T) {
	d := Finding(&Result{Status: StatusRolledBack, PR: 698, Reason: "health check timed out"}, "owner/repo", time.Now().UTC())
	if d.Status != "failed" {
		t.Errorf("Status = %q", d.Status)
	}
	if !strings.Contains(d.Summary, "rolled back") || !strings.Contains(d.Summary, "health check timed out") {
		t.Errorf("Summary = %q", d.Summary)
	}
}

func TestBuildSHA(t *testing.T) {
	cases := []struct {
		version string
		want    string
	}{
		{"1.4.2+gabc1234", "abc1234"},              // self-deploy / release stamp (#682)
		{"v1.4.2+gdeadbee", "deadbee"},             // leading v tolerated
		{"dev-abc123456789", "abc123456789"},       // local checkout VCS fallback
		{"dev-abc123456789-dirty", "abc123456789"}, // dirty working tree suffix stripped
		{"dev", ""},                       // bare sentinel — no SHA
		{"", ""},                          // empty
		{"1.4.2", ""},                     // plain semver, no stamp
		{"  1.4.2+gabc1234  ", "abc1234"}, // surrounding whitespace
	}
	for _, tc := range cases {
		if got := BuildSHA(tc.version); got != tc.want {
			t.Errorf("BuildSHA(%q) = %q, want %q", tc.version, got, tc.want)
		}
	}
}

func TestMainAdvanced(t *testing.T) {
	const head = "abc1234567890abcdef1234567890abcdef123456" // full 40-char origin/main SHA

	// Running binary built from the same commit as origin/main — up to date.
	if MainAdvanced("1.4.2+gabc1234", head) {
		t.Error("MainAdvanced = true when the binary's short SHA prefixes main head (no drift)")
	}
	// Case-insensitive prefix match still counts as up to date.
	if MainAdvanced("1.4.2+gABC1234", strings.ToUpper(head)) {
		t.Error("MainAdvanced = true for a case-only difference (no drift)")
	}
	// origin/main moved to a different commit — the binary lags.
	if !MainAdvanced("1.4.2+gabc1234", "fed9876543210fed9876543210fed9876543210f") {
		t.Error("MainAdvanced = false when main head differs from the running binary")
	}
	// Undeterminable running version (bare dev): never deploy on a guess.
	if MainAdvanced("dev", head) {
		t.Error("MainAdvanced = true for an unstamped binary")
	}
	// Missing head SHA (lookup failed): never deploy on a guess.
	if MainAdvanced("1.4.2+gabc1234", "") {
		t.Error("MainAdvanced = true with an empty main head SHA")
	}
}
