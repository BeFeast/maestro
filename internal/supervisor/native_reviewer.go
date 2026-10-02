package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/google/uuid"
)

// NativeReviewResult is the reviewer lane's completion. AccountingPending
// marks a complete, durable verdict whose single physical attempt the
// authority has sealed but not yet accounted (#1239); the receipt stays held
// for the regular reconcile and no second attempt may be spent on it.
type NativeReviewResult struct {
	Output            string
	AccountingPending bool
}

// CompleteNativeReview reuses the bounded native receipt/registration runner,
// with an explicit reviewer role, exact requested model and one candidate.
// It never silently adopts the supervisor fallback chain or review transport.
func CompleteNativeReview(ctx context.Context, cfg *config.Config, model, claimID, prompt string) (NativeReviewResult, error) {
	var result NativeReviewResult
	if cfg == nil || cfg.Supervisor.NativeSessionRegistration == nil || model == "" || uuid.Validate(claimID) != nil {
		return result, aiexecution.Held("native_reviewer_unconfigured")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	projectDigest, err := config.AIExecutionConfigDigest(cfg)
	if err != nil {
		return result, aiexecution.Held("project_config_unobservable")
	}
	local := *cfg
	local.Supervisor = cfg.Supervisor
	local.Model = cfg.Model
	local.Model.FallbackBackends = nil
	local.Supervisor.Model = model
	local.Supervisor.TotalTimeoutSeconds = 600
	local.Supervisor.AttemptTimeoutSeconds = 600
	if deadline, ok := ctx.Deadline(); ok {
		seconds := int(time.Until(deadline).Seconds())
		if seconds <= 0 {
			return result, context.DeadlineExceeded
		}
		if seconds < 600 {
			local.Supervisor.TotalTimeoutSeconds = seconds
			local.Supervisor.AttemptTimeoutSeconds = seconds
		}
	}
	base, def, err := supervisorBackend(&local)
	if err != nil {
		return result, aiexecution.Held("native_reviewer_backend_unavailable")
	}
	if config.ResolveBackendKind(base, def.Provider, def.Cmd) != config.BackendKindClaude {
		return result, aiexecution.Held("native_reviewer_harness_unsupported")
	}
	// Force one route and its requested model. Backend-specific deadline cannot
	// silently widen this bounded one-shot reviewer request.
	def.SupervisorAttemptTimeoutSeconds = local.Supervisor.AttemptTimeoutSeconds
	// The reviewer accepts plain text from stdin and cannot execute a tool or
	// load project/user hooks. Preserve configured effort through Supervisor;
	// arbitrary extra CLI arguments are unsupported on this explicit lane.
	if len(def.ExtraArgs) != 0 {
		return result, aiexecution.Held("native_reviewer_arguments_unsupported")
	}
	def.ExtraArgs = []string{"--bare", "--tools", "", "--max-turns", "1", "--permission-mode", "dontAsk", "--permission-prompts", "none"}
	local.Model.Backends = map[string]config.BackendDef{base: def}
	local.Supervisor.Backend = base
	local.StateDir = filepath.Join(cfg.StateDir, "native-reviews")
	if err := os.MkdirAll(local.StateDir, 0700); err != nil {
		return result, aiexecution.Held("review_receipt_store_unavailable")
	}
	scratch, err := os.MkdirTemp("", "maestro-native-review-")
	if err != nil {
		return result, aiexecution.Held("review_workspace_unavailable")
	}
	defer os.RemoveAll(scratch)
	local.LocalPath = scratch
	// A private cwd plus --bare suppresses repository/user config discovery.
	// Actual credential/egress containment remains an independent strict proof.
	if strings.TrimSpace(local.Supervisor.Model) != model {
		return result, aiexecution.Held("native_reviewer_model_invalid")
	}
	client := &backendLLMClient{cfg: &local, role: "reviewer", executionContext: ctx, projectConfigSHA256: projectDigest, memory: newSupervisorBackendMemory(), acceptPendingOutcome: true}
	consultation, err := client.CompleteConsultation(ConsultationIdentity{ID: claimID, ProjectID: cfg.ProjectID, CycleID: claimID, Role: "reviewer"}, prompt)
	if err != nil {
		return result, err
	}
	result.Output, result.AccountingPending = consultation.Output, consultation.AccountingPending
	return result, nil
}
