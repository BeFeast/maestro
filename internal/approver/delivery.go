package approver

// Delivery executor (#872): the execution stage of the approval-gated
// post-merge delivery. Given an APPROVED deploy_project approval, it:
//
//  1. takes the durable approved→executing claim in the SQLite store BEFORE any
//     side effect, so a daemon and a CLI approve contend on one transition and
//     exactly one runs the delivery (and a restart observing executing never
//     replays it);
//  2. pins the exact merged revision — it verifies the checkout at LocalPath is
//     the payload's MergedSHA and refuses to deploy whatever else happens to be
//     there;
//  3. runs the project's delivery command once, with the configured timeout,
//     capturing bounded + sanitized output;
//  4. runs the configured live/deployment verifier;
//  5. records the terminal result (executed / execution_failed) with the
//     sanitized run record folded onto the approval payload.
//
// The delivery command and verifier are read from config at execute time, NOT
// from the approval payload — so a secret inlined into a command never has to
// live in the durable approval record (only a sanitized preview does).

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// CommandRunner runs a shell command in dir under ctx and returns combined
// stdout+stderr plus any error. The default runner uses `bash -c`. Tests inject
// a fake to exercise success/failure/timeout without touching a real service.
type CommandRunner interface {
	Run(ctx context.Context, dir, command string) (output string, err error)
}

// RevisionChecker returns the checked-out revision (HEAD commit SHA) at dir.
// The default uses `git -C dir rev-parse HEAD`.
type RevisionChecker interface {
	HeadSHA(dir string) (string, error)
}

// CommandRunnerFunc adapts a plain function to CommandRunner.
type CommandRunnerFunc func(ctx context.Context, dir, command string) (string, error)

func (f CommandRunnerFunc) Run(ctx context.Context, dir, command string) (string, error) {
	return f(ctx, dir, command)
}

// RevisionCheckerFunc adapts a plain function to RevisionChecker.
type RevisionCheckerFunc func(dir string) (string, error)

func (f RevisionCheckerFunc) HeadSHA(dir string) (string, error) { return f(dir) }

// bashRunner is the default CommandRunner: `bash -c <command>` in dir, output
// bounded by ctx timeout. A context deadline surfaces as a distinct error so
// the caller can label a timeout.
type bashRunner struct{}

func (bashRunner) Run(ctx context.Context, dir, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("timed out: %w", context.DeadlineExceeded)
	}
	return string(out), err
}

// gitRevision is the default RevisionChecker.
type gitRevision struct{}

func (gitRevision) HeadSHA(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD in %s: %w", dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// DeliveryExecutor drives one approved deploy_project approval to its delivery.
type DeliveryExecutor struct {
	// Store is the SQLite approvals store that arbitrates the durable
	// approved→executing claim and records the terminal result. Required.
	Store *approvalstore.Store
	// StateDir scopes the claim to one project's rows in the shared DB.
	StateDir string
	// Repo binds the executor to a project; a delivery approval stamped with a
	// different repo is refused (cross-project mutation guard).
	Repo string
	// Delivery is the resolved delivery config (command / verifier / timeout)
	// read at execute time. The raw command is never taken from the approval.
	Delivery config.DeliveryConfig

	// Runner / Revision default to bash + git when nil.
	Runner   CommandRunner
	Revision RevisionChecker
	// Actor labels audit entries (e.g. "daemon", operator name).
	Actor string
	// OutputLimit bounds sanitized output; 0 uses the state default.
	OutputLimit int
	// Now overrides the clock in tests. nil uses time.Now().UTC().
	Now func() time.Time
}

// DeliveryResult is the outcome of a Deliver call.
type DeliveryResult struct {
	Approval *state.Approval
	Status   state.ApprovalStatus
	Summary  string
	Err      error
	// Skipped is true when no side effect ran because the claim was lost, the
	// approval was not deliverable, or it was already in flight/terminal.
	Skipped bool
}

// ErrNotDeliverable is returned when an approval is not an executable delivery.
var ErrNotDeliverable = errors.New("approval is not an executable deploy_project delivery")

func (d *DeliveryExecutor) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

func (d *DeliveryExecutor) runner() CommandRunner {
	if d.Runner != nil {
		return d.Runner
	}
	return bashRunner{}
}

func (d *DeliveryExecutor) revision() RevisionChecker {
	if d.Revision != nil {
		return d.Revision
	}
	return gitRevision{}
}

func (d *DeliveryExecutor) actor() string {
	if strings.TrimSpace(d.Actor) != "" {
		return d.Actor
	}
	return "delivery-executor"
}

// Deliver executes the delivery for approval id. It is safe to call
// concurrently across processes and goroutines for the same id: the store's
// approved→executing claim admits exactly one runner; every other caller
// returns a Skipped result with no side effect.
func (d *DeliveryExecutor) Deliver(ctx context.Context, id string) DeliveryResult {
	if d.Store == nil {
		return DeliveryResult{Status: state.ApprovalStatusExecutionFailed, Err: errors.New("delivery executor has no store"), Skipped: true}
	}

	// Inspect before claiming so a non-delivery / cross-repo approval is
	// refused without consuming the claim.
	pre, err := d.Store.Get(ctx, d.StateDir, id)
	if err != nil {
		return DeliveryResult{Status: state.ApprovalStatusExecutionFailed, Err: err, Skipped: true}
	}
	if pre.Action != state.ApprovalActionDeployProject || pre.Delivery == nil {
		return DeliveryResult{Approval: pre, Status: state.ApprovalStatusExecutionFailed, Err: ErrNotDeliverable, Skipped: true}
	}
	if guardErr := d.repoGuard(pre); guardErr != nil {
		return DeliveryResult{Approval: pre, Status: state.ApprovalStatusExecutionFailed, Err: guardErr, Skipped: true}
	}

	// Durable claim: approved → executing. Exactly one winner.
	claimed, err := d.Store.ClaimExecuting(ctx, d.StateDir, id, d.now(), d.actor(), "delivery claim before side effect")
	if err != nil {
		// Lost the race, already executing, or already terminal — never a
		// side effect. A restart observing an executing/terminal row lands here.
		return DeliveryResult{
			Approval: claimed,
			Status:   statusOf(claimed),
			Summary:  fmt.Sprintf("delivery %s not claimable (%v) — no command run", id, err),
			Err:      err,
			Skipped:  true,
		}
	}

	payload := claimed.Delivery.Clone()
	payload.StartedAt = d.now()

	// Revision pin: never deploy whatever revision happens to be at LocalPath.
	head, err := d.revision().HeadSHA(payload.LocalPath)
	if err != nil {
		return d.fail(ctx, id, payload, fmt.Sprintf("could not read checkout revision at %s: %v", payload.LocalPath, err))
	}
	payload.ExecutedRevision = head
	if !revisionMatches(head, payload.MergedSHA) {
		return d.fail(ctx, id, payload, fmt.Sprintf(
			"checkout revision mismatch: %s is at %s but the approved delivery pins %s — refusing to deploy a different revision",
			payload.LocalPath, state.SanitizeDeliveryOutput(head, 64), state.SanitizeDeliveryOutput(payload.MergedSHA, 64)))
	}

	command := strings.TrimSpace(d.Delivery.Command)
	if command == "" {
		return d.fail(ctx, id, payload, "delivery.command is empty — nothing to run")
	}

	timeout := d.Delivery.EffectiveTimeout()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	out, runErr := d.runner().Run(runCtx, payload.LocalPath, command)
	cancel()
	payload.Output = state.SanitizeDeliveryOutput(out, d.OutputLimit)
	if runErr != nil {
		payload.ExitError = state.SanitizeDeliveryOutput(runErr.Error(), 512)
		return d.fail(ctx, id, payload, fmt.Sprintf("delivery command failed: %s", payload.ExitError))
	}

	// Live/deployment verifier.
	if verify := strings.TrimSpace(d.Delivery.VerifyCommand); verify != "" {
		vCtx, vCancel := context.WithTimeout(ctx, timeout)
		vOut, vErr := d.runner().Run(vCtx, payload.LocalPath, verify)
		vCancel()
		payload.VerifyOutput = state.SanitizeDeliveryOutput(vOut, d.OutputLimit)
		if vErr != nil {
			payload.ExitError = state.SanitizeDeliveryOutput(vErr.Error(), 512)
			return d.fail(ctx, id, payload, fmt.Sprintf("delivery verifier failed: %s", payload.ExitError))
		}
	}

	payload.Verified = true
	payload.FinishedAt = d.now()
	summary := fmt.Sprintf("delivered %s @ %s", strings.TrimSpace(payload.Project), shortRev(payload.MergedSHA))
	final, err := d.Store.FinishDelivery(ctx, d.StateDir, id, true, payload, d.now(), d.actor(), summary)
	if err != nil {
		return DeliveryResult{Approval: final, Status: statusOf(final), Err: err}
	}
	return DeliveryResult{Approval: final, Status: state.ApprovalStatusExecuted, Summary: summary}
}

// fail records a terminal execution_failed result for a claimed delivery.
func (d *DeliveryExecutor) fail(ctx context.Context, id string, payload *state.DeliveryPayload, reason string) DeliveryResult {
	payload.Verified = false
	payload.FinishedAt = d.now()
	final, err := d.Store.FinishDelivery(ctx, d.StateDir, id, false, payload, d.now(), d.actor(), reason)
	if err != nil {
		return DeliveryResult{Approval: final, Status: statusOf(final), Err: err}
	}
	return DeliveryResult{Approval: final, Status: state.ApprovalStatusExecutionFailed, Summary: reason, Err: errors.New(reason)}
}

func (d *DeliveryExecutor) repoGuard(a *state.Approval) error {
	stamped := strings.TrimSpace(a.Repo)
	cfgRepo := strings.TrimSpace(d.Repo)
	if stamped != "" && cfgRepo != "" && stamped != cfgRepo {
		return fmt.Errorf("delivery %s is bound to repo %q but executor repo is %q — refusing cross-project delivery", a.ID, stamped, cfgRepo)
	}
	return nil
}

func statusOf(a *state.Approval) state.ApprovalStatus {
	if a == nil {
		return state.ApprovalStatusExecutionFailed
	}
	return a.Status
}

// revisionMatches reports whether checkout head satisfies the pinned SHA. A
// short (abbreviated) pin matches when it is a prefix of the full head, so a
// payload pinned to an abbreviated SHA still verifies against the full checkout.
func revisionMatches(head, pinned string) bool {
	head = strings.ToLower(strings.TrimSpace(head))
	pinned = strings.ToLower(strings.TrimSpace(pinned))
	if head == "" || pinned == "" {
		return false
	}
	if head == pinned {
		return true
	}
	if len(pinned) >= 7 && strings.HasPrefix(head, pinned) {
		return true
	}
	return false
}

func shortRev(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
