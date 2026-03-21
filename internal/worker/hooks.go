package worker

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
)

// HookEnv holds environment variables passed to lifecycle hooks.
type HookEnv struct {
	IssueID       string // "owner/repo#number"
	IssueNumber   int
	WorkspacePath string
}

// RunHook executes a lifecycle hook script with the given environment.
// Returns an error if the hook fails. The caller decides whether to treat it as fatal.
func RunHook(cfg *config.Config, hookName, script string, env HookEnv) error {
	if strings.TrimSpace(script) == "" {
		return nil
	}

	timeout := time.Duration(cfg.Hooks.TimeoutMs) * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.Dir = env.WorkspacePath
	cmd.Env = append(cmd.Environ(),
		fmt.Sprintf("ISSUE_ID=%s", env.IssueID),
		fmt.Sprintf("ISSUE_NUMBER=%d", env.IssueNumber),
		fmt.Sprintf("WORKSPACE_PATH=%s", env.WorkspacePath),
	)

	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		log.Printf("[hooks] %s output:\n%s", hookName, strings.TrimRight(string(out), "\n"))
	}

	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("hook %s timed out after %dms", hookName, cfg.Hooks.TimeoutMs)
	}
	if err != nil {
		return fmt.Errorf("hook %s failed: %w", hookName, err)
	}

	log.Printf("[hooks] %s completed successfully", hookName)
	return nil
}
