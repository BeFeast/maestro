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
	Stream   string
	Model    string
	complete func(context.Context, string, string) (string, error)
	policy   aiexecution.Policy
}

// NewNativeClaudeLens installs the supported runner. There is no exported
// callback injection that could label an arbitrary HTTP client a native leaf.
func NewNativeClaudeLens(stream, model string, cfg *config.Config) *NativeClaudeLens {
	var policy aiexecution.Policy
	if cfg != nil {
		policy = cfg.AIExecution
	}
	return &NativeClaudeLens{Stream: stream, Model: model, policy: policy, complete: func(ctx context.Context, prompt, claimID string) (string, error) {
		return supervisor.CompleteNativeReview(ctx, cfg, model, claimID, prompt)
	}}
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
func (l *NativeClaudeLens) RunClaimed(ctx context.Context, prompt, claimID string) (string, error) {
	if err := l.Available(); err != nil {
		return "", err
	}
	if uuid.Validate(claimID) != nil {
		return "", aiexecution.Held("native_reviewer_claim_required")
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
