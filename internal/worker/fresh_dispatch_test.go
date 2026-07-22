package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
)

func TestStartReserved_AdoptsExactLiveWorkerWithoutDiscardingWorktree(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	worktreeBase := filepath.Join(root, "worktrees")
	stateDir := filepath.Join(root, "state")
	for _, args := range [][]string{
		{"init", "-b", "main", repo},
		{"-C", repo, "config", "user.email", "test@example.com"},
		{"-C", repo, "config", "user.name", "Test"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", repo, "add", "base.txt"}, {"-C", repo, "commit", "-m", "base"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	issue := github.Issue{Number: 394, Title: "atomic fresh dispatch"}
	slot := "ok-player-1"
	branch := BranchName(slot, issue)
	worktree := filepath.Join(worktreeBase, slot)
	if err := os.MkdirAll(worktreeBase, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-b", branch, worktree, "main").CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	partial := filepath.Join(worktree, "partial.txt")
	if err := os.WriteFile(partial, []byte("preserve me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	originalExists := tmuxSessionExists
	originalRead := readTmuxPaneIdentity
	originalLeaseActive := workerProcessLeaseActive
	originalLeaseAnchored := workerProcessLeaseAnchored
	t.Cleanup(func() {
		tmuxSessionExists = originalExists
		readTmuxPaneIdentity = originalRead
		workerProcessLeaseActive = originalLeaseActive
		workerProcessLeaseAnchored = originalLeaseAnchored
	})
	tmuxSessionExists = func(name string) bool { return name == TmuxSessionName(slot) }
	readTmuxPaneIdentity = func(name string) (int, string, error) { return 4242, worktree, nil }
	var inspectedLease tmuxsession.ProcessLease
	workerProcessLeaseActive = func(lease tmuxsession.ProcessLease) (bool, error) {
		inspectedLease = lease
		return true, nil
	}
	workerProcessLeaseAnchored = func(lease tmuxsession.ProcessLease, pid int) (bool, error) {
		return lease == inspectedLease && pid == 4242, nil
	}

	cfg := &config.Config{
		Repo:          "owner/repo",
		LocalPath:     repo,
		WorktreeBase:  worktreeBase,
		StateDir:      stateDir,
		SessionPrefix: "ok-player",
		Model: config.ModelConfig{
			Default: "test",
			Backends: map[string]config.BackendDef{
				"test": {Cmd: "/bin/true"},
			},
		},
	}
	s := state.NewState()
	got, err := StartReserved(cfg, s, cfg.Repo, issue, "prompt", "test", slot)
	if err != nil {
		t.Fatalf("StartReserved: %v", err)
	}
	if got != slot {
		t.Fatalf("slot = %q, want %q", got, slot)
	}
	if data, err := os.ReadFile(partial); err != nil || string(data) != "preserve me\n" {
		t.Fatalf("partial work changed: data=%q err=%v", data, err)
	}
	sess := s.Sessions[slot]
	if sess == nil || sess.IssueNumber != issue.Number || sess.Branch != branch || sess.Worktree != worktree || sess.PID != 4242 {
		t.Fatalf("adopted session = %+v", sess)
	}
	wantLease, err := workerProcessLease(cfg, slot, 1)
	if err != nil {
		t.Fatal(err)
	}
	if inspectedLease != wantLease || sess.ProcessLeaseUnit != wantLease.Unit || sess.ProcessLeaseManager != wantLease.Manager {
		t.Fatalf("adopted process lease inspected=%+v session=%+v, want %+v", inspectedLease, sess, wantLease)
	}
}
