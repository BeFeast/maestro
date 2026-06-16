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
