package worker

import (
	"fmt"
	"os/exec"
	"strings"
)

// defaultBaseBranch is the branch worker worktrees are derived from. The
// orchestrator creates PRs against this branch (see orchestrator.createPR) and
// the whole pipeline assumes the local checkout tracks origin/<defaultBaseBranch>.
const defaultBaseBranch = "main"

// SyncBaseBranch fetches origin and fast-forwards the local base branch
// (typically "main") inside localPath to origin/<branch>, so that newly created
// worker worktrees branch from an up-to-date base.
//
// Maestro creates each worker worktree from the local checkout. When the local
// base branch lags origin (e.g. PRs merged on GitHub but never pulled locally),
// every worker branches from a stale base: it re-implements already-merged work,
// and its branch becomes a sibling rather than a descendant of origin/main.
// The fetch + fast-forward here prevents all of that (#734).
//
// If the local base branch has diverged from origin (cannot fast-forward), or
// the base branch is checked out with a dirty working tree, SyncBaseBranch
// returns an explicit error instead of silently leaving a stale base in place.
func SyncBaseBranch(localPath, branch string) error {
	localPath = strings.TrimSpace(localPath)
	if localPath == "" {
		return fmt.Errorf("sync base branch: empty local path")
	}
	if strings.TrimSpace(branch) == "" {
		branch = defaultBaseBranch
	}
	remoteRef := "origin/" + branch

	// Refresh remote-tracking refs before deriving or rebasing worker branches.
	if _, err := runGit(localPath, "fetch", "origin"); err != nil {
		return fmt.Errorf("sync base branch %q: %w", branch, err)
	}

	// origin must actually have the branch.
	if _, err := runGit(localPath, "rev-parse", "--verify", "--quiet", remoteRef); err != nil {
		return fmt.Errorf("sync base branch %q: remote ref %s not found after fetch", branch, remoteRef)
	}

	// If the local branch doesn't exist yet, create it at origin/<branch>.
	if _, err := runGit(localPath, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		if _, cErr := runGit(localPath, "branch", branch, remoteRef); cErr != nil {
			return fmt.Errorf("sync base branch %q: create local branch: %w", branch, cErr)
		}
		return nil
	}

	localSHA, err := runGit(localPath, "rev-parse", branch)
	if err != nil {
		return fmt.Errorf("sync base branch %q: %w", branch, err)
	}
	remoteSHA, err := runGit(localPath, "rev-parse", remoteRef)
	if err != nil {
		return fmt.Errorf("sync base branch %q: %w", branch, err)
	}
	if strings.TrimSpace(localSHA) == strings.TrimSpace(remoteSHA) {
		return nil // already up to date
	}

	// A fast-forward is only possible when the local branch is an ancestor of
	// the remote. Otherwise the branch has diverged — fail loudly rather than
	// branching workers from a base that conflicts with origin.
	if _, err := runGit(localPath, "merge-base", "--is-ancestor", branch, remoteRef); err != nil {
		return fmt.Errorf("local base branch %q has diverged from %s and cannot fast-forward; "+
			"reconcile %s manually before spawning workers", branch, remoteRef, localPath)
	}

	head, _ := runGit(localPath, "symbolic-ref", "--quiet", "--short", "HEAD")
	if strings.TrimSpace(head) == branch {
		// Base branch is checked out: refuse a dirty *tracked* tree, then
		// fast-forward merge. Untracked agent harness dirs (.claude/.codex/…)
		// must not block sync — merge --ff-only does not touch them, and
		// workers commonly leave those dirs in the shared base checkout (#1100).
		if dirty, err := baseCheckoutBlockingDirt(localPath); err != nil {
			return fmt.Errorf("sync base branch %q: %w", branch, err)
		} else if dirty != "" {
			return fmt.Errorf("base branch %q checkout at %s is dirty; cannot fast-forward to %s:\n%s",
				branch, localPath, remoteRef, dirty)
		}
		if _, err := runGit(localPath, "merge", "--ff-only", remoteRef); err != nil {
			return fmt.Errorf("fast-forward base branch %q to %s: %w", branch, remoteRef, err)
		}
		return nil
	}

	// Base branch is not the checked-out branch: advance the ref directly. The
	// ancestry check above guarantees this is a fast-forward.
	if _, err := runGit(localPath, "branch", "-f", branch, remoteRef); err != nil {
		return fmt.Errorf("fast-forward base branch %q to %s: %w", branch, remoteRef, err)
	}
	return nil
}

// ignorableBaseUntrackedPrefixes are untracked top-level paths workers and
// agent harnesses commonly leave in the shared local_path checkout. They do
// not block git merge --ff-only and must not freeze fleet spawn (#1100).
var ignorableBaseUntrackedPrefixes = []string{
	".claude/",
	".codex/",
	".cursor/",
	".entire/",
	".agents/",
	".windsurf/",
	".clinerules",
}

// baseCheckoutBlockingDirt returns porcelain lines that should block SyncBaseBranch.
// Tracked modifications always block. Untracked agent harness dirs are ignored.
func baseCheckoutBlockingDirt(localPath string) (string, error) {
	out, err := worktreeDirty(localPath)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", nil
	}
	var blocking []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "?? ") {
			path := strings.TrimPrefix(line, "?? ")
			if isIgnorableBaseUntracked(path) {
				continue
			}
		}
		blocking = append(blocking, line)
	}
	return strings.Join(blocking, "\n"), nil
}

func isIgnorableBaseUntracked(path string) bool {
	path = strings.TrimSpace(path)
	path = strings.ReplaceAll(path, "\\", "/")
	for _, prefix := range ignorableBaseUntrackedPrefixes {
		if path == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// addWorktreeFromBase creates worktreePath as a fresh checkout on branchName,
// rooted directly at origin/<defaultBaseBranch>. Branching from the remote ref
// (rather than the local checkout's HEAD) guarantees the worktree descends from
// the remote base regardless of which branch localPath currently has checked
// out, so the worktree's merge-base with origin/main is origin/main (#734).
func addWorktreeFromBase(localPath, worktreePath, branchName string) error {
	base := "origin/" + defaultBaseBranch
	out, err := exec.Command("git", "-C", localPath,
		"worktree", "add", "--no-track", "-b", branchName, worktreePath, base).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree add %s on %s from %s: %w\n%s", worktreePath, branchName, base, err, out)
	}
	return nil
}
