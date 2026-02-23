package worker

import (
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// ImportResult describes the outcome of importing a single worktree.
type ImportResult struct {
	SlotName    string
	IssueNumber int
	Branch      string
	Status      state.SessionStatus
	Skipped     bool
	SkipReason  string
}

// Import scans for existing worktrees under cfg.WorktreeBase, detects issue
// numbers from branch names, checks tmux session liveness, and seeds state
// entries. Useful for migrating from other tools or recovering from state loss.
func Import(cfg *config.Config, s *state.State) ([]ImportResult, error) {
	if cfg.WorktreeBase == "" {
		return nil, fmt.Errorf("worktree_base not set in config")
	}
	if cfg.LocalPath == "" {
		return nil, fmt.Errorf("local_path not set in config")
	}

	out, err := exec.Command("git", "-C", cfg.LocalPath, "worktree", "list").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w\n%s", err, out)
	}

	var results []ImportResult

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}

		wtPath, branch, ok := parseWorktreeLine(line)
		if !ok {
			continue
		}

		// Only consider worktrees under WorktreeBase
		absBase, _ := filepath.Abs(cfg.WorktreeBase)
		absWT, _ := filepath.Abs(wtPath)
		if !strings.HasPrefix(absWT, absBase+"/") {
			continue
		}

		// Derive slot name from directory name
		slotName := filepath.Base(wtPath)

		// Parse issue number from branch
		issueNum, err := ParseIssueFromBranch(branch)
		if err != nil {
			results = append(results, ImportResult{
				SlotName:   slotName,
				Branch:     branch,
				Skipped:    true,
				SkipReason: fmt.Sprintf("could not parse issue number: %v", err),
			})
			continue
		}

		// Check if already in state
		if _, exists := s.Sessions[slotName]; exists {
			results = append(results, ImportResult{
				SlotName:    slotName,
				IssueNumber: issueNum,
				Branch:      branch,
				Skipped:     true,
				SkipReason:  "already in state",
			})
			continue
		}

		// Check if tmux session is alive
		tmuxName := TmuxSessionName(slotName)
		tmuxAlive := exec.Command("tmux", "has-session", "-t", tmuxName).Run() == nil

		status := state.StatusDead
		pid := 0
		if tmuxAlive {
			status = state.StatusRunning
			// Try to get PID from tmux pane
			if pidOut, err := exec.Command("tmux", "list-panes", "-t", tmuxName, "-F", "#{pane_pid}").Output(); err == nil {
				if p, err := strconv.Atoi(strings.TrimSpace(string(pidOut))); err == nil {
					pid = p
				}
			}
		}

		// Check for existing log file
		logFile := filepath.Join(state.LogDir(cfg.StateDir), slotName+".log")

		s.Sessions[slotName] = &state.Session{
			IssueNumber: issueNum,
			Worktree:    wtPath,
			Branch:      branch,
			PID:         pid,
			LogFile:     logFile,
			StartedAt:   time.Now().UTC(),
			Status:      status,
		}

		// Advance NextSlot past any imported slot numbers to avoid collisions
		if num := parseSlotNumber(slotName); num >= s.NextSlot {
			s.NextSlot = num + 1
		}

		log.Printf("[import] %s → issue #%d [%s]", slotName, issueNum, status)

		results = append(results, ImportResult{
			SlotName:    slotName,
			IssueNumber: issueNum,
			Branch:      branch,
			Status:      status,
		})
	}

	return results, nil
}

// parseWorktreeLine parses a line from `git worktree list` output.
// Format: /path/to/worktree  abc1234 [branch-name]
func parseWorktreeLine(line string) (path, branch string, ok bool) {
	bracketStart := strings.Index(line, "[")
	bracketEnd := strings.Index(line, "]")
	if bracketStart < 0 || bracketEnd < 0 || bracketEnd <= bracketStart {
		return "", "", false
	}

	branch = line[bracketStart+1 : bracketEnd]

	fields := strings.Fields(line[:bracketStart])
	if len(fields) < 1 {
		return "", "", false
	}
	path = fields[0]

	return path, branch, true
}

// ParseIssueFromBranch extracts an issue number from a branch name.
// Supports formats:
//   - feat/{prefix}-{N}-{issue}-{title} (maestro: feat/pan-1-42-title)
//   - feat/issue-{N}-{title}
//   - {anything}/issue-{N}-{anything}
func ParseIssueFromBranch(branch string) (int, error) {
	// Maestro-style: feat/{letters}-{digits}-{issueNum}-...
	reMaestro := regexp.MustCompile(`^feat/[a-zA-Z]+-\d+-(\d+)`)
	if m := reMaestro.FindStringSubmatch(branch); len(m) >= 2 {
		return strconv.Atoi(m[1])
	}

	// issue-N or issue/N pattern
	reIssue := regexp.MustCompile(`issue[/-](\d+)`)
	if m := reIssue.FindStringSubmatch(branch); len(m) >= 2 {
		return strconv.Atoi(m[1])
	}

	return 0, fmt.Errorf("no issue number found in branch %q", branch)
}

// parseSlotNumber extracts the numeric suffix from a slot name (e.g. "pan-1" → 1).
func parseSlotNumber(slotName string) int {
	parts := strings.Split(slotName, "-")
	if len(parts) < 2 {
		return 0
	}
	num, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return 0
	}
	return num
}
