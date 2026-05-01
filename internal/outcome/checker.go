package outcome

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	defaultCheckTimeout = 15 * time.Second
	maxCheckDetailBytes = 4000
)

// Checker executes configured read-only outcome signals and returns a compact
// result that can be persisted in Maestro state.
type Checker struct {
	HTTPClient     *http.Client
	CommandTimeout time.Duration
	Now            func() time.Time
	RunCommand     func(ctx context.Context, command, dir string) ([]byte, int, error)
}

func (c Checker) Check(ctx context.Context, brief Brief) HealthCheckResult {
	brief = brief.Normalized()
	start := c.now()
	result := HealthCheckResult{
		CheckedAt: start,
		State:     HealthUnknown,
	}
	if !brief.Configured() {
		result.State = HealthNotConfigured
		result.Summary = "No outcome brief is configured."
		return result
	}
	if !brief.HasHealthSignal() {
		result.State = HealthUnmonitored
		result.Summary = "No outcome health signal is configured."
		return result
	}
	if strings.TrimSpace(brief.HealthcheckURL) != "" {
		result = c.checkURL(ctx, brief.HealthcheckURL)
	} else if strings.TrimSpace(brief.HealthcheckCommand) != "" {
		result = c.checkCommand(ctx, brief.HealthcheckCommand, brief.SourceRepoPath, "healthcheck_command")
	} else {
		result = c.checkCommand(ctx, brief.DeploymentStatusCommand, brief.SourceRepoPath, "deployment_status_command")
	}
	if result.CheckedAt.IsZero() {
		result.CheckedAt = start
	}
	if result.DurationMillis == 0 {
		result.DurationMillis = int64(c.now().Sub(start) / time.Millisecond)
	}
	result.Detail = compactDetail(result.Detail)
	return result
}

func (c Checker) checkURL(ctx context.Context, rawURL string) HealthCheckResult {
	start := c.now()
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return c.result(start, "healthcheck_url", HealthFailing, fmt.Sprintf("Invalid healthcheck URL: %v", err), "", 0)
	}
	req.Header.Set("User-Agent", "maestro-outcome-check/1")

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return c.result(start, "healthcheck_url", HealthFailing, fmt.Sprintf("GET %s failed: %v", rawURL, err), "", 0)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxCheckDetailBytes+1))
	state := HealthHealthy
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		state = HealthFailing
	}
	detail := ""
	if state == HealthFailing {
		detail = string(body)
	}
	return c.result(start, "healthcheck_url", state, fmt.Sprintf("GET %s returned %s", rawURL, resp.Status), detail, 0)
}

func (c Checker) checkCommand(ctx context.Context, command, dir, signal string) HealthCheckResult {
	start := c.now()
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	output, exitCode, err := c.runCommand(ctx, command, dir)
	if err != nil {
		summary := fmt.Sprintf("%s failed", signal)
		if ctx.Err() == context.DeadlineExceeded {
			summary = fmt.Sprintf("%s timed out after %s", signal, c.timeout().String())
		} else if strings.TrimSpace(err.Error()) != "" {
			summary = fmt.Sprintf("%s failed: %v", signal, err)
		}
		return c.result(start, signal, HealthFailing, summary, string(output), exitCode)
	}
	return c.result(start, signal, HealthHealthy, fmt.Sprintf("%s passed", signal), "", exitCode)
}

func (c Checker) runCommand(ctx context.Context, command, dir string) ([]byte, int, error) {
	if c.RunCommand != nil {
		return c.RunCommand(ctx, command, dir)
	}
	cmd := exec.CommandContext(ctx, "sh", "-lc", command)
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	return output, exitCode(err), err
}

func (c Checker) result(start time.Time, signal, state, summary, detail string, exitCode int) HealthCheckResult {
	return HealthCheckResult{
		CheckedAt:      start,
		Signal:         signal,
		State:          state,
		Summary:        strings.TrimSpace(summary),
		Detail:         compactDetail(detail),
		ExitCode:       exitCode,
		DurationMillis: int64(c.now().Sub(start) / time.Millisecond),
	}
}

func (c Checker) timeout() time.Duration {
	if c.CommandTimeout > 0 {
		return c.CommandTimeout
	}
	return defaultCheckTimeout
}

func (c Checker) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return -1
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
		return status.ExitStatus()
	}
	return -1
}

func compactDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return ""
	}
	data := []byte(detail)
	if len(data) <= maxCheckDetailBytes {
		return detail
	}
	var buf bytes.Buffer
	buf.Write(data[:maxCheckDetailBytes])
	buf.WriteString("\n... truncated ...")
	return buf.String()
}
