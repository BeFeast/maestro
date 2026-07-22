package worker

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
	"github.com/befeast/maestro/internal/workerlease"
)

func TestLaunchWorkerProcessLeaseUsesOneServiceForProcessAndScratch(t *testing.T) {
	cfg := isolatedRuntimeConfig(t, "project-service")
	cfg.WorkerRuntime.Scope = config.WorkerRuntimeScopeUser

	binDir := t.TempDir()
	systemctl := filepath.Join(binDir, "systemctl")
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	originalSpawn := runTmuxNewSession
	originalRead := readTmuxPaneIdentity
	originalConfirm := confirmWorkerProcessLease
	originalCurrentUser := workerRuntimeCurrentUser
	t.Cleanup(func() {
		runTmuxNewSession = originalSpawn
		readTmuxPaneIdentity = originalRead
		confirmWorkerProcessLease = originalConfirm
		workerRuntimeCurrentUser = originalCurrentUser
	})
	workerRuntimeCurrentUser = func() (*user.User, error) {
		return &user.User{HomeDir: "/home/worker", Username: "worker"}, nil
	}
	var launched tmuxsession.ProcessLease
	runTmuxNewSession = func(_, _, _ string, lease tmuxsession.ProcessLease) ([]byte, error) {
		launched = lease
		return nil, nil
	}
	readTmuxPaneIdentity = func(string) (int, string, error) {
		return 4242, "/work/sup-927", nil
	}
	confirmWorkerProcessLease = func(lease tmuxsession.ProcessLease, pid int, _ time.Duration) (bool, error) {
		return lease.Unit == launched.Unit && pid == 4242, nil
	}

	pid, receipt, err := launchWorkerProcessLease(
		cfg, "sup-927", "maestro-sup-927", "/work/sup-927", "/state/sup-927-run.sh", 2, 0, "test",
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if receipt.Runtime != nil {
			_ = workerlease.CleanupManifest(receipt.Runtime.ManifestPath, receipt.Runtime.ScratchID)
		}
	})
	if pid != 4242 || receipt.Unit != launched.Unit || !strings.HasSuffix(receipt.Unit, ".service") || receipt.Runtime == nil {
		t.Fatalf("launch pid=%d receipt=%+v launched=%+v", pid, receipt, launched)
	}
	manifest, err := workerlease.ValidateManifest(receipt.Runtime.ManifestPath, receipt.Runtime.ScratchID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Unit != receipt.Unit || manifest.Scope != receipt.Manager {
		t.Fatalf("scratch manifest unit=%q/%q, process lease=%q/%q", manifest.Unit, manifest.Scope, receipt.Unit, receipt.Manager)
	}
	sess := &state.Session{}
	setSessionProcessLease(sess, receipt)
	if sess.ProcessLeaseUnit != sess.WorkerLeaseUnit || sess.ProcessLeaseManager != sess.WorkerLeaseScope || sess.WorkerLeaseID == "" {
		t.Fatalf("session persisted parallel ownership instead of one lease: %+v", sess)
	}
}

func TestLaunchWorkerProcessLeaseKeepsScratchWhenFailedStartTeardownIsUncertain(t *testing.T) {
	cfg := isolatedRuntimeConfig(t, "project-uncertain")
	cfg.WorkerRuntime.Scope = config.WorkerRuntimeScopeUser
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	originalSpawn := runTmuxNewSession
	originalRead := readTmuxPaneIdentity
	originalTerminate := terminateWorkerProcessLease
	originalCurrentUser := workerRuntimeCurrentUser
	originalCleanup := workerScratchCleanup
	t.Cleanup(func() {
		runTmuxNewSession = originalSpawn
		readTmuxPaneIdentity = originalRead
		terminateWorkerProcessLease = originalTerminate
		workerRuntimeCurrentUser = originalCurrentUser
		workerScratchCleanup = originalCleanup
	})
	workerRuntimeCurrentUser = func() (*user.User, error) {
		return &user.User{HomeDir: "/home/worker", Username: "worker"}, nil
	}
	runTmuxNewSession = func(_, _, _ string, _ tmuxsession.ProcessLease) ([]byte, error) {
		return nil, errors.New("service launch failed")
	}
	readTmuxPaneIdentity = func(string) (int, string, error) {
		return 0, "", errors.New("no pane")
	}
	terminateWorkerProcessLease = func(tmuxsession.ProcessLease) error {
		return errors.New("manager unavailable")
	}
	cleanupCalls := 0
	workerScratchCleanup = func(string, string) error {
		cleanupCalls++
		return nil
	}

	_, receipt, err := launchWorkerProcessLease(
		cfg, "sup-927", "maestro-sup-927", "/work/sup-927", "/state/sup-927-run.sh", 4, 0, "test",
	)
	if err == nil || receipt.Runtime == nil {
		t.Fatalf("uncertain teardown receipt=%+v err=%v", receipt, err)
	}
	if cleanupCalls != 0 {
		t.Fatalf("scratch cleanup ran %d times while the process lease might still be active", cleanupCalls)
	}
	if _, statErr := os.Stat(receipt.Runtime.ManifestPath); statErr != nil {
		t.Fatalf("scratch receipt was not retained for reconciliation: %v", statErr)
	}
	if cleanupErr := workerlease.CleanupManifest(receipt.Runtime.ManifestPath, receipt.Runtime.ScratchID); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
}

func TestLaunchWorkerProcessLeaseFailedStartTearsDownPreparedScope(t *testing.T) {
	t.Setenv("INVOCATION_ID", "maestro-test")
	originalSpawn := runTmuxNewSession
	originalRead := readTmuxPaneIdentity
	originalTerminate := terminateWorkerProcessLease
	originalKillTmux := killWorkerTmuxSession
	t.Cleanup(func() {
		runTmuxNewSession = originalSpawn
		readTmuxPaneIdentity = originalRead
		terminateWorkerProcessLease = originalTerminate
		killWorkerTmuxSession = originalKillTmux
	})

	spawnCalls := 0
	runTmuxNewSession = func(_, _, _ string, _ tmuxsession.ProcessLease) ([]byte, error) {
		spawnCalls++
		return nil, errors.New("scope launch failed")
	}
	readTmuxPaneIdentity = func(string) (int, string, error) {
		return 0, "", errors.New("no pane")
	}
	var terminated tmuxsession.ProcessLease
	terminateWorkerProcessLease = func(lease tmuxsession.ProcessLease) error {
		terminated = lease
		return nil
	}
	killedTmux := ""
	killWorkerTmuxSession = func(name string) ([]byte, error) {
		killedTmux = name
		return nil, nil
	}

	cfg := &config.Config{Repo: "BeFeast/maestro"}
	_, _, err := launchWorkerProcessLease(cfg, "sup-920", "maestro-sup-920", "/work/sup-920", "/state/sup-920-run.sh", 4, 0, "test")
	if err == nil {
		t.Fatal("failed start unexpectedly succeeded")
	}
	want, leaseErr := tmuxsession.WorkerProcessLease(cfg.Repo, "sup-920", 4)
	if leaseErr != nil {
		t.Fatal(leaseErr)
	}
	if spawnCalls != 1 || terminated != want || killedTmux != "" {
		t.Fatalf("failed-start cleanup spawn=%d terminated=%+v tmux=%q, want one spawn lease=%+v and no unowned tmux kill", spawnCalls, terminated, killedTmux, want)
	}
}

func TestLaunchWorkerProcessLeaseReturnsReceiptWhenCleanupIsUncertain(t *testing.T) {
	t.Setenv("INVOCATION_ID", "maestro-test")
	originalSpawn := runTmuxNewSession
	originalRead := readTmuxPaneIdentity
	originalTerminate := terminateWorkerProcessLease
	t.Cleanup(func() {
		runTmuxNewSession = originalSpawn
		readTmuxPaneIdentity = originalRead
		terminateWorkerProcessLease = originalTerminate
	})
	runTmuxNewSession = func(_, _, _ string, _ tmuxsession.ProcessLease) ([]byte, error) {
		return nil, errors.New("scope launch failed")
	}
	readTmuxPaneIdentity = func(string) (int, string, error) {
		return 0, "", errors.New("no pane")
	}
	terminateWorkerProcessLease = func(tmuxsession.ProcessLease) error {
		return errors.New("manager unavailable")
	}

	cfg := &config.Config{Repo: "BeFeast/maestro"}
	_, got, err := launchWorkerProcessLease(cfg, "sup-920", "maestro-sup-920", "/work/sup-920", "/state/sup-920-run.sh", 5, 0, "test")
	if err == nil || got.Unit == "" || got.Manager != tmuxsession.ProcessLeaseManagerSystem {
		t.Fatalf("uncertain cleanup result lease=%+v err=%v, want durable system lease receipt", got, err)
	}
}

func TestLaunchWorkerProcessLeaseRefusesPaneOutsidePreparedScope(t *testing.T) {
	t.Setenv("INVOCATION_ID", "maestro-test")
	originalSpawn := runTmuxNewSession
	originalRead := readTmuxPaneIdentity
	originalConfirm := confirmWorkerProcessLease
	originalTerminate := terminateWorkerProcessLease
	t.Cleanup(func() {
		runTmuxNewSession = originalSpawn
		readTmuxPaneIdentity = originalRead
		confirmWorkerProcessLease = originalConfirm
		terminateWorkerProcessLease = originalTerminate
	})

	runTmuxNewSession = func(_, _, _ string, _ tmuxsession.ProcessLease) ([]byte, error) {
		return nil, nil
	}
	readTmuxPaneIdentity = func(string) (int, string, error) {
		return 4242, "/work/sup-920", nil
	}
	var confirmed tmuxsession.ProcessLease
	confirmWorkerProcessLease = func(lease tmuxsession.ProcessLease, pid int, timeout time.Duration) (bool, error) {
		confirmed = lease
		if pid != 4242 || timeout != processLeaseStartWait {
			t.Fatalf("confirm pane pid/timeout = %d/%s, want 4242/%s", pid, timeout, processLeaseStartWait)
		}
		return false, nil
	}
	var terminated tmuxsession.ProcessLease
	terminateWorkerProcessLease = func(lease tmuxsession.ProcessLease) error {
		terminated = lease
		return nil
	}

	cfg := &config.Config{Repo: "BeFeast/maestro"}
	pid, receipt, err := launchWorkerProcessLease(cfg, "sup-920", "maestro-sup-920", "/work/sup-920", "/state/sup-920-run.sh", 6, 0, "test")
	if err == nil {
		t.Fatal("pane outside exact worker scope was accepted")
	}
	if pid != 0 || receipt.Unit != "" || confirmed.Unit == "" || terminated != confirmed {
		t.Fatalf("unowned launch pid=%d receipt=%+v confirmed=%+v terminated=%+v", pid, receipt, confirmed, terminated)
	}
}

func TestWaitWorkerProcessLeaseReadyRetriesUntilScopeOwnsPane(t *testing.T) {
	originalActive := workerProcessLeaseActive
	originalAnchored := workerProcessLeaseAnchored
	t.Cleanup(func() {
		workerProcessLeaseActive = originalActive
		workerProcessLeaseAnchored = originalAnchored
	})

	checks := 0
	workerProcessLeaseActive = func(tmuxsession.ProcessLease) (bool, error) {
		checks++
		return checks >= 2, nil
	}
	workerProcessLeaseAnchored = func(tmuxsession.ProcessLease, int) (bool, error) {
		return true, nil
	}
	lease := tmuxsession.ProcessLease{Unit: "maestro-worker-0123456789abcdef0123456789abcdef-g8.scope", Manager: tmuxsession.ProcessLeaseManagerSystem}
	ready, err := waitWorkerProcessLeaseReady(lease, 4242, 100*time.Millisecond)
	if err != nil || !ready || checks < 2 {
		t.Fatalf("ready=%v checks=%d err=%v, want retry then ready", ready, checks, err)
	}
}

func TestProcessLeaseProjectIdentitySeparatesSameRepoProjects(t *testing.T) {
	first := processLeaseProjectIdentity(&config.Config{Repo: "owner/repo", StateDir: "/state/project-a"})
	second := processLeaseProjectIdentity(&config.Config{Repo: "owner/repo", StateDir: "/state/project-b"})
	if first == second {
		t.Fatalf("same-repo projects share process lease identity %q", first)
	}
	projectID := "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	if got := processLeaseProjectIdentity(&config.Config{Repo: "owner/repo", StateDir: "/state/project-a", ProjectID: projectID}); got != projectID {
		t.Fatalf("project_id did not take precedence: %q", got)
	}
}

func TestStopProcessRetainsLeaseMetadataUntilTeardownConfirmed(t *testing.T) {
	originalTerminate := terminateWorkerProcessLease
	originalKillTmux := killWorkerTmuxSession
	originalTmuxExists := workerTmuxSessionExists
	t.Cleanup(func() {
		terminateWorkerProcessLease = originalTerminate
		killWorkerTmuxSession = originalKillTmux
		workerTmuxSessionExists = originalTmuxExists
	})

	sess := &state.Session{
		PID:                 999999,
		TmuxSession:         "maestro-sup-920",
		ProcessLeaseUnit:    "maestro-worker-0123456789abcdef0123456789abcdef-g2.scope",
		ProcessLeaseManager: tmuxsession.ProcessLeaseManagerSystem,
	}
	terminateCalls := 0
	terminateWorkerProcessLease = func(tmuxsession.ProcessLease) error {
		terminateCalls++
		if terminateCalls == 1 {
			return errors.New("manager unavailable")
		}
		return nil
	}
	tmuxKills := 0
	workerTmuxSessionExists = func(string) bool { return false }
	killWorkerTmuxSession = func(name string) ([]byte, error) {
		tmuxKills++
		if name != sess.TmuxSession {
			t.Fatalf("killed tmux %q, want %q", name, sess.TmuxSession)
		}
		return nil, nil
	}

	if err := StopProcess("sup-920", sess); err == nil {
		t.Fatal("uncertain lease teardown unexpectedly succeeded")
	}
	if sess.ProcessLeaseUnit == "" || tmuxKills != 0 {
		t.Fatalf("failed teardown lost receipt or removed tmux: lease=%q tmuxKills=%d", sess.ProcessLeaseUnit, tmuxKills)
	}
	if err := StopProcess("sup-920", sess); err != nil {
		t.Fatal(err)
	}
	if sess.ProcessLeaseUnit != "" || sess.ProcessLeaseManager != "" || tmuxKills != 0 {
		t.Fatalf("confirmed teardown did not clear exact receipt: session=%+v tmuxKills=%d", sess, tmuxKills)
	}
}

func TestStopProcessCleansScratchOnlyAfterExactProcessLeaseStops(t *testing.T) {
	cfg := isolatedRuntimeConfig(t, "project-stop")
	lease := prepareRuntimeLease(t, cfg, "sup-927")
	sess := sessionForRuntimeLease(lease, state.StatusRunning)
	sess.TmuxSession = "maestro-sup-927"

	originalTerminate := terminateWorkerProcessLease
	originalTmuxExists := workerTmuxSessionExists
	t.Cleanup(func() {
		terminateWorkerProcessLease = originalTerminate
		workerTmuxSessionExists = originalTmuxExists
	})
	terminated := false
	terminateWorkerProcessLease = func(got tmuxsession.ProcessLease) error {
		if got.Unit != lease.Unit || got.Manager != lease.Scope {
			t.Fatalf("terminated lease=%+v, want %s/%s", got, lease.Unit, lease.Scope)
		}
		terminated = true
		return nil
	}
	workerTmuxSessionExists = func(string) bool { return false }

	if err := StopProcess("sup-927", sess); err != nil {
		t.Fatal(err)
	}
	if !terminated || sess.ProcessLeaseUnit != "" || sess.WorkerLeaseID != "" {
		t.Fatalf("cleanup did not clear one process-and-scratch receipt: %+v", sess)
	}
	if _, err := os.Stat(lease.ScratchDir); !os.IsNotExist(err) {
		t.Fatalf("scratch still exists after exact lease stop: %v", err)
	}
}

func TestStopProcessRefusesToKillPersistentSameNameTmux(t *testing.T) {
	originalTerminate := terminateWorkerProcessLease
	originalTmuxExists := workerTmuxSessionExists
	originalKillTmux := killWorkerTmuxSession
	t.Cleanup(func() {
		terminateWorkerProcessLease = originalTerminate
		workerTmuxSessionExists = originalTmuxExists
		killWorkerTmuxSession = originalKillTmux
	})
	terminateWorkerProcessLease = func(tmuxsession.ProcessLease) error { return nil }
	workerTmuxSessionExists = func(string) bool { return true }
	killCalls := 0
	killWorkerTmuxSession = func(string) ([]byte, error) {
		killCalls++
		return nil, nil
	}
	sess := &state.Session{
		TmuxSession:         "maestro-sup-920",
		ProcessLeaseUnit:    "maestro-worker-0123456789abcdef0123456789abcdef-g3.scope",
		ProcessLeaseManager: tmuxsession.ProcessLeaseManagerSystem,
	}
	if err := StopProcess("sup-920", sess); err == nil {
		t.Fatal("persistent same-name tmux should fail closed")
	}
	if killCalls != 0 || sess.ProcessLeaseUnit == "" {
		t.Fatalf("unowned tmux was killed or cleanup receipt lost: kills=%d session=%+v", killCalls, sess)
	}
}
