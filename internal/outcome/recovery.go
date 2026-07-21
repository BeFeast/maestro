package outcome

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// RecoveryExecution is a command-text/output-free receipt. The configured
// entrypoint may handle credentials, so neither stdout nor stderr is retained.
type RecoveryExecution struct {
	StartedAt      time.Time
	FinishedAt     time.Time
	ExitCode       int
	TimedOut       bool
	DurationMillis int64
}

type RecoveryRunner struct {
	Now        func() time.Time
	RunCommand func(ctx context.Context, command, dir string) (int, error)
}

func (r RecoveryRunner) Run(ctx context.Context, brief Brief) RecoveryExecution {
	brief = brief.Normalized()
	start := r.now()
	execution := RecoveryExecution{StartedAt: start, ExitCode: -1}
	ctx, cancel := context.WithTimeout(ctx, brief.EffectiveRecoveryTimeout())
	defer cancel()

	exitCode, _ := r.runCommand(ctx, brief.RecoveryCommand, brief.SourceRepoPath)
	execution.ExitCode = exitCode
	execution.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	execution.FinishedAt = r.now()
	execution.DurationMillis = execution.FinishedAt.Sub(start).Milliseconds()
	return execution
}

func (r RecoveryRunner) runCommand(ctx context.Context, command, dir string) (int, error) {
	if r.RunCommand != nil {
		return r.RunCommand(ctx, command, dir)
	}
	cmd := exec.CommandContext(ctx, "sh", "-lc", command)
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	// A bounded recovery must not leave a helper alive after the shell exits.
	// Give the shell and every descendant one process group, then make context
	// cancellation kill that entire group before a later cooldown retry can run.
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
	// Do not attach buffers: output may contain provider credentials or private
	// quota state. The durable receipt records only exit code and timestamps.
	err := cmd.Run()
	return recoveryExitCode(err), err
}

// OwnsActiveFailure reports whether this automatic-recovery lease/receipt is
// still the sole actuator for the failing project outcome at time now.
//
// Ownership holds while the recovery is:
//   - executing (a bounded command is running under the durable lease), or
//   - verification-pending (the command finished exit 0 and the authoritative
//     health check has not yet confirmed the fix), or
//   - cooldown-bounded: a failed attempt whose retry cooldown has not yet
//     elapsed. The project-scoped actuator will re-lease at NextEligibleAt, so
//     the drift is still owned and an outcome-driven product repair must not
//     race in during the cooldown window.
//
// A verified receipt (the outcome recovered), an uncertain receipt (no durable
// receipt; health verification is authoritative and no cooldown fence exists),
// or a failed attempt whose cooldown already elapsed do NOT own the failure —
// genuine independent PR blockers remain repairable in those states.
func (r *RecoveryState) OwnsActiveFailure(now time.Time) bool {
	if r == nil {
		return false
	}
	switch r.Status {
	case RecoveryStatusExecuting, RecoveryStatusVerificationPending:
		return true
	case RecoveryStatusFailed:
		return !r.NextEligibleAt.IsZero() && now.UTC().Before(r.NextEligibleAt)
	default:
		return false
	}
}

func (r RecoveryRunner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func recoveryExitCode(err error) int {
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
