package review

import (
	"context"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/google/uuid"
)

// NativeClaudeLens is explicit transport selection. It retains the existing
// requested model and per-head durable attempt claim; no fallback is inferred.
// The native receipt runner is supplied by the configured controller adapter.
type NativeClaudeLens struct {
	Stream      string
	Model       string
	complete    func(context.Context, string, string) (supervisor.NativeReviewResult, error)
	policy      aiexecution.Policy
	projectID   string
	budgetRunID string
}

// NewNativeClaudeLens installs the supported runner. There is no exported
// callback injection that could label an arbitrary HTTP client a native leaf.
func NewNativeClaudeLens(stream, model string, cfg *config.Config) *NativeClaudeLens {
	var policy aiexecution.Policy
	if cfg != nil {
		policy = cfg.AIExecution
	}
	l := &NativeClaudeLens{Stream: stream, Model: model, policy: policy, complete: func(ctx context.Context, prompt, claimID string) (supervisor.NativeReviewResult, error) {
		return supervisor.CompleteNativeReview(ctx, cfg, model, claimID, prompt)
	}}
	if cfg != nil && cfg.Supervisor.NativeSessionRegistration != nil {
		l.projectID = cfg.ProjectID
		l.budgetRunID = cfg.Supervisor.NativeSessionRegistration.BudgetRunID
	}
	return l
}

func (l *NativeClaudeLens) Name() string { return l.Stream }
func (l *NativeClaudeLens) Available() error {
	if l.Model == "" || l.complete == nil {
		return aiexecution.Held("native_reviewer_unconfigured")
	}
	return nil
}
func (l *NativeClaudeLens) Run(context.Context, string) (string, error) {
	return "", aiexecution.Held("native_reviewer_claim_required")
}

// RunClaimed returns the verdict text plus whether the authority accounting
// of its single physical attempt is still pending (#1239). A pending result is
// a complete verdict; only the durable attempt reason and the status note
// differ, so the operator can see which reviews await settlement.
func (l *NativeClaudeLens) RunClaimed(ctx context.Context, prompt, claimID string) (supervisor.NativeReviewResult, error) {
	if err := l.Available(); err != nil {
		return supervisor.NativeReviewResult{}, err
	}
	if uuid.Validate(claimID) != nil {
		return supervisor.NativeReviewResult{}, aiexecution.Held("native_reviewer_claim_required")
	}
	return l.complete(ctx, prompt, claimID)
}
func managedLens(l Lens) bool {
	switch l.(type) {
	case *ChatLens, *NativeClaudeLens:
		return true
	}
	return false
}
