package outcome

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultCheckTimeout       = 15 * time.Second
	maxCheckDetailBytes       = 4000
	maxCheckOutputBytes       = 64 * 1024
	maxStructuredHealthChecks = 16
)

var errCheckOutputTooLarge = errors.New("outcome check output exceeded safe limit")

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
	checks, projectedDetail, projectedSummary, declaredHealthy := projectStructuredHealth(output)
	if state, summary := blockingStructuredHealthState(checks); state != "" {
		if summary != "" {
			summary = fmt.Sprintf("%s: %s", signal, summary)
		} else if state == HealthPending {
			summary = fmt.Sprintf("%s pending", signal)
		} else {
			summary = fmt.Sprintf("%s reported unhealthy", signal)
		}
		result := c.result(start, signal, state, summary, projectedDetail, exitCode)
		result.Checks = checks
		return result
	}
	if err != nil {
		summary := fmt.Sprintf("%s failed", signal)
		if errors.Is(err, errCheckOutputTooLarge) {
			summary = fmt.Sprintf("%s output exceeded the %d-byte safety limit", signal, maxCheckOutputBytes)
		} else if ctx.Err() == context.DeadlineExceeded {
			summary = fmt.Sprintf("%s timed out after %s", signal, c.timeout().String())
		} else if exitCode >= 0 {
			summary = fmt.Sprintf("%s failed with exit code %d", signal, exitCode)
		}
		if projectedSummary != "" {
			summary = fmt.Sprintf("%s failed: %s", signal, projectedSummary)
		}
		result := c.result(start, signal, HealthFailing, summary, projectedDetail, exitCode)
		result.Checks = checks
		return result
	}
	if declaredHealthy != nil && !*declaredHealthy {
		summary := fmt.Sprintf("%s reported unhealthy", signal)
		if projectedSummary != "" {
			summary += ": " + projectedSummary
		}
		result := c.result(start, signal, HealthFailing, summary, projectedDetail, exitCode)
		result.Checks = checks
		return result
	}
	result := c.result(start, signal, HealthHealthy, fmt.Sprintf("%s passed", signal), "", exitCode)
	result.Checks = checks
	return result
}

func (c Checker) runCommand(ctx context.Context, command, dir string) ([]byte, int, error) {
	if c.RunCommand != nil {
		return c.RunCommand(ctx, command, dir)
	}
	cmd := exec.CommandContext(ctx, "sh", "-lc", command)
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	capture := &boundedOutputBuffer{limit: maxCheckOutputBytes}
	cmd.Stdout = capture
	cmd.Stderr = capture
	err := cmd.Run()
	output := capture.Bytes()
	if capture.Truncated() {
		if err != nil {
			err = errors.Join(err, errCheckOutputTooLarge)
		} else {
			err = errCheckOutputTooLarge
		}
	}
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

type structuredHealthEnvelope struct {
	Healthy    *bool                   `json:"healthy"`
	DeadlineAt string                  `json:"deadline_at"`
	Deadline   string                  `json:"deadline"`
	Checks     []structuredHealthCheck `json:"checks"`
}

type structuredHealthCheck struct {
	Name       string `json:"name"`
	Blocking   bool   `json:"blocking"`
	Status     string `json:"status"`
	DeadlineAt string `json:"deadline_at"`
	Deadline   string `json:"deadline"`
}

var safeHealthCheckName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

// projectStructuredHealth keeps only an allow-listed envelope from checker
// JSON. Raw details and unknown fields are discarded so quota/token/credential
// material cannot accidentally enter durable state or Fleet.
func projectStructuredHealth(output []byte) (checks []HealthCheckItem, detail, summary string, healthy *bool) {
	var envelope structuredHealthEnvelope
	if err := json.Unmarshal(output, &envelope); err != nil || (envelope.Healthy == nil && len(envelope.Checks) == 0) {
		return nil, "", "", nil
	}
	envelopeDeadline := projectHealthCheckDeadline(firstNonEmpty(envelope.DeadlineAt, envelope.Deadline))
	projected := make([]HealthCheckItem, 0, len(envelope.Checks))
	for _, raw := range envelope.Checks {
		item := HealthCheckItem{
			Name:       projectHealthCheckName(raw.Name),
			Blocking:   raw.Blocking,
			Status:     projectHealthCheckStatus(raw.Status),
			DeadlineAt: projectHealthCheckDeadline(firstNonEmpty(raw.DeadlineAt, raw.Deadline)),
		}
		if item.DeadlineAt == "" && item.Status != "pass" && item.Status != "healthy" {
			item.DeadlineAt = envelopeDeadline
		}
		projected = append(projected, item)
	}
	// Keep the state bounded while prioritizing the blocking failure/pending
	// checks an operator actually needs. A long list of passing checks cannot
	// crowd the failing check out of Fleet.
	for priority := 0; priority <= 4 && len(checks) < maxStructuredHealthChecks; priority++ {
		for _, item := range projected {
			if structuredHealthPriority(item) != priority {
				continue
			}
			checks = append(checks, item)
			if len(checks) == maxStructuredHealthChecks {
				break
			}
		}
	}
	for _, item := range checks {
		if summary == "" && (item.Status == "fail" || item.Status == "failing" || item.Status == "error") {
			summary = item.Name + " reported " + item.Status
		}
	}
	if len(checks) > 0 {
		projected, _ := json.Marshal(struct {
			Checks []HealthCheckItem `json:"checks"`
		}{Checks: checks})
		detail = string(projected)
	}
	return checks, detail, summary, envelope.Healthy
}

func projectHealthCheckName(value string) string {
	value = strings.TrimSpace(value)
	if !safeHealthCheckName.MatchString(value) {
		return "structured-check"
	}
	return value
}

func projectHealthCheckStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "pass", "fail", "warning", "unknown", "error", "healthy", "failing", "pending", "in_progress", "queued":
		return strings.ToLower(strings.TrimSpace(value))
	case "in-progress":
		return "in_progress"
	default:
		return "unknown"
	}
}

func projectHealthCheckDeadline(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > len(time.RFC3339Nano)+16 {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return ""
	}
	return parsed.UTC().Format(time.RFC3339Nano)
}

func structuredHealthPriority(item HealthCheckItem) int {
	switch item.Status {
	case "fail", "failing", "error":
		if item.Blocking {
			return 0
		}
		return 3
	case "pending", "in_progress", "queued":
		if item.Blocking {
			return 1
		}
		return 3
	case "pass", "healthy":
		return 4
	default:
		if item.Blocking {
			return 2
		}
		return 3
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type boundedOutputBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedOutputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || len(p) > 0
		return written, nil
	}
	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		return written, nil
	}
	_, _ = b.buf.Write(p)
	return written, nil
}

func (b *boundedOutputBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func (b *boundedOutputBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

func blockingStructuredHealthState(checks []HealthCheckItem) (state, summary string) {
	for _, check := range checks {
		if !check.Blocking {
			continue
		}
		switch check.Status {
		case "fail", "failing", "error":
			return HealthFailing, check.Name + " reported " + check.Status
		}
	}
	for _, check := range checks {
		if !check.Blocking {
			continue
		}
		switch check.Status {
		case "pending", "in_progress", "queued":
			return HealthPending, check.Name + " reported " + check.Status
		}
	}
	return "", ""
}
