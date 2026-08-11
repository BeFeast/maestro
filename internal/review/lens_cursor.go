package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// CursorLens runs a review through the cursor-agent CLI. Cursor is the one
// sanctioned exception to the CLIProxy invariant: CLIProxy does not proxy
// Cursor, so the agent authenticates with its own key.
//
// The subprocess is sandboxed the way the bash glue live-verified (#1161):
// an EMPTY working directory with the prompt+diff on stdin, so the only
// thing the agent can read is what we feed it — it cannot roam a checkout.
// `--mode ask` keeps it read-only (no write/shell tools) and `--trust`
// suppresses the workspace-trust prompt WITHOUT the command auto-approve
// that -f/--yolo would grant. stdin also sidesteps ARG_MAX on large diffs.
// stdout (the review text) is captured separately from stderr so a stray
// progress/telemetry line can never read as a spurious finding.
type CursorLens struct {
	// Stream is the status context / stream name, e.g. "llm-review-cursor".
	Stream string
	// Model is the cursor model, e.g. "composer-2.5" (the included-usage
	// pool default — cost-safe).
	Model string
	// APIKey is passed to the subprocess as CURSOR_API_KEY.
	APIKey string
	// Binary is the agent executable. Empty means "cursor-agent" on PATH.
	Binary string
	// Timeout bounds one run. Zero means 10 minutes.
	Timeout time.Duration
}

func (l *CursorLens) Name() string { return l.Stream }

// Available mirrors the bash prepare check: the key is the gating credential
// (a missing binary is a run failure, not a creds skip).
func (l *CursorLens) Available() error {
	if strings.TrimSpace(l.APIKey) == "" {
		return fmt.Errorf("cursor lens %s: CURSOR_API_KEY not configured", l.Stream)
	}
	return nil
}

func (l *CursorLens) binary() string {
	if l.Binary != "" {
		return l.Binary
	}
	return "cursor-agent"
}

func (l *CursorLens) timeout() time.Duration {
	if l.Timeout > 0 {
		return l.Timeout
	}
	return defaultLensTimeout
}

// Run executes cursor-agent over the prompt and returns its stdout.
func (l *CursorLens) Run(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, l.timeout())
	defer cancel()

	// The empty scratch dir is the read sandbox; remove it with the run.
	dir, err := os.MkdirTemp("", "llm-review-cursor-")
	if err != nil {
		return "", fmt.Errorf("scratch dir: %w", err)
	}
	defer os.RemoveAll(dir)

	cmd := exec.CommandContext(ctx, l.binary(),
		"-p", "--output-format", "text", "--mode", "ask", "--trust",
		"--model", l.Model)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "CURSOR_API_KEY="+l.APIKey)
	// Kill the whole process group at the deadline and bound the post-kill
	// wait (the ghCommandContext pattern): the agent spawns helpers, and a
	// surviving child holding the inherited output pipe would keep Run
	// blocked past the deadline — an unbounded producer hang the Timeout
	// field exists to prevent.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 512 {
			detail = detail[:512]
		}
		return "", fmt.Errorf("cursor-agent: %w: %s", err, detail)
	}
	return stdout.String(), nil
}
