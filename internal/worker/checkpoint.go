package worker

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const checkpointFile = "CHECKPOINT.md"

// SaveCheckpoint captures the current state of a worker's worktree into a CHECKPOINT.md file.
// It commits any staged/unstaged changes and writes a progress summary.
func SaveCheckpoint(worktreePath string, issueNumber int, tokensUsed int) error {
	if worktreePath == "" {
		return fmt.Errorf("empty worktree path")
	}

	// Stage and commit any uncommitted work before capturing stats
	if out, err := exec.Command("git", "-C", worktreePath, "add", "-A").CombinedOutput(); err != nil {
		log.Printf("[worker] checkpoint: git add failed: %v\n%s", err, out)
	}
	// Only commit if there are staged changes
	if out, err := exec.Command("git", "-C", worktreePath, "diff", "--cached", "--quiet").CombinedOutput(); err != nil {
		// There are staged changes — commit them
		msg := fmt.Sprintf("wip: checkpoint before token-threshold respawn (issue #%d)", issueNumber)
		if commitOut, commitErr := exec.Command("git", "-C", worktreePath, "commit", "-m", msg).CombinedOutput(); commitErr != nil {
			log.Printf("[worker] checkpoint: git commit failed: %v\n%s", commitErr, commitOut)
		}
		_ = out
	}

	// Gather stats after staging/committing so the WIP commit is included
	diffStat := "(no changes)"
	if out, err := exec.Command("git", "-C", worktreePath, "diff", "--stat", "origin/main..HEAD").CombinedOutput(); err == nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed != "" {
			diffStat = trimmed
		}
	}

	commitLog := "(no commits)"
	if out, err := exec.Command("git", "-C", worktreePath, "log", "--oneline", "origin/main..HEAD").CombinedOutput(); err == nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed != "" {
			commitLog = trimmed
		}
	}

	// Push the branch so the new worker can pick it up
	if out, err := exec.Command("git", "-C", worktreePath, "push", "-u", "origin", "HEAD").CombinedOutput(); err != nil {
		return fmt.Errorf("checkpoint push failed: %w\n%s", err, out)
	}

	content := fmt.Sprintf(`# Checkpoint — Issue #%d

**Saved at:** %s
**Tokens used:** %d

## Changes so far

%s

## Commits

%s

## Instructions for continuation

This checkpoint was created because the worker approached its token budget.
Pick up where the previous worker left off. Review the commits and code changes
above, then continue implementing the remaining work for the issue.
Do NOT redo work that is already committed.
`,
		issueNumber,
		time.Now().UTC().Format(time.RFC3339),
		tokensUsed,
		"```\n"+diffStat+"\n```",
		"```\n"+commitLog+"\n```",
	)

	checkpointPath := filepath.Join(worktreePath, checkpointFile)
	if err := os.WriteFile(checkpointPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}

	log.Printf("[worker] checkpoint saved at %s", checkpointPath)
	return nil
}

// LoadCheckpoint reads CHECKPOINT.md from a worktree path if it exists.
// Returns empty string if no checkpoint is found.
func LoadCheckpoint(worktreePath string) string {
	checkpointPath := filepath.Join(worktreePath, checkpointFile)
	data, err := os.ReadFile(checkpointPath)
	if err != nil {
		return ""
	}
	return string(data)
}
