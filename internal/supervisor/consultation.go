package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/google/uuid"
)

// ConsultationIdentity is local execution ancestry, not a gateway request ID.
// CycleID identifies the deterministic decision that requested synthesis.
type ConsultationIdentity struct {
	ID              string `json:"id"`
	ProjectID       string `json:"project_id"`
	CycleID         string `json:"cycle_id"`
	Role            string `json:"role"`
	ParentRoleRunID string `json:"parent_role_run_id,omitempty"`
}

// AccountingCapability describes evidence required of a future shared-admission
// adapter. Local process receipts establish neither property. It is intentionally
// not configurable: a user-supplied boolean cannot prove physical call coverage.
type AccountingCapability struct {
	AttributionVerified              bool `json:"attribution_verified"`
	PhysicalRequestAdmissionVerified bool `json:"physical_request_admission_verified"`
}

func (c AccountingCapability) Ready() bool {
	return c.AttributionVerified && c.PhysicalRequestAdmissionVerified
}

// ConsultationClient is the optional richer interface; legacy test/remote LLM
// clients can retain Complete. Strict policy is checked before either interface.
type ConsultationClient interface {
	CompleteConsultation(ConsultationIdentity, string) (ConsultationResult, error)
}

type ConsultationResult struct {
	Output  string
	Receipt *ConsultationReceipt
}

type ConsultationHold struct{ Code string }

func (h *ConsultationHold) Error() string { return "supervisor consultation held: " + h.Code }

type ConsultationReceipt struct {
	SchemaVersion         int                  `json:"schema_version"`
	Identity              ConsultationIdentity `json:"identity"`
	StartedAt             time.Time            `json:"started_at"`
	EndedAt               *time.Time           `json:"ended_at,omitempty"`
	Status                string               `json:"status"`
	RequestedBackend      string               `json:"requested_backend"`
	RequestedModel        string               `json:"requested_model"`
	RequestedModelSource  string               `json:"requested_model_source"`
	PolicyVersion         string               `json:"policy_version"`
	PolicyDigest          string               `json:"policy_digest"`
	CatalogRevision       *string              `json:"catalog_revision"`
	Capability            AccountingCapability `json:"accounting_capability"`
	InputDigest           string               `json:"input_digest,omitempty"`
	NativeOutcomeComplete bool                 `json:"native_outcome_complete,omitempty"`
	// PlannedInvocation is an intent snapshot, not evidence that Start succeeded.
	PlannedInvocation *InvocationReceipt  `json:"planned_invocation,omitempty"`
	Candidates        []CandidateReceipt  `json:"candidates"`
	Invocations       []InvocationReceipt `json:"invocations"`
}

// Candidates record consideration and launch intent. An intent surviving a crash
// means launch is uncertain; it must never be replayed as if no request occurred.
type CandidateReceipt struct {
	Backend  string `json:"backend"`
	Status   string `json:"status"`
	IntentID string `json:"intent_id,omitempty"`
}

// InvocationReceipt represents a successfully started local process only. It is
// not a token record, gateway logical request, or physical upstream attempt.
type InvocationReceipt struct {
	ProcessLease               *NativeInvocationProcessLease         `json:"process_lease,omitempty"`
	ProcessTerminationVerified bool                                  `json:"process_termination_verified,omitempty"`
	ProcessTerminationDigest   string                                `json:"process_termination_digest,omitempty"`
	ProcessTermination         *aiexecution.NativeProcessTermination `json:"process_termination,omitempty"`
	OutputCheckpoint           *NativeOutputCheckpoint               `json:"output_checkpoint,omitempty"`
	NativeSession              *NativeSessionRegistrationReceipt     `json:"native_session,omitempty"`
	ID                         string                                `json:"id"`
	Number                     int                                   `json:"number"`
	SelectedBackend            string                                `json:"selected_backend"`
	HarnessKind                string                                `json:"harness_kind"`
	ConfiguredModel            string                                `json:"configured_model"`
	EffectiveCLIModel          *string                               `json:"effective_cli_model"`
	ModelArguments             []string                              `json:"model_arguments"`
	ModelEvidence              string                                `json:"model_evidence"`
	UpstreamActualModel        *string                               `json:"upstream_actual_model"`
	AccountAlias               *string                               `json:"account_alias"`
	FallbackReason             string                                `json:"fallback_reason,omitempty"`
	RoutePolicyDecision        string                                `json:"route_policy_decision"`
	StartedAt                  time.Time                             `json:"started_at"`
	EndedAt                    time.Time                             `json:"ended_at"`
	Status                     string                                `json:"status"`
}

type NativeInvocationProcessLease struct {
	Unit    string                `json:"unit"`
	Manager string                `json:"manager"`
	Profile aiexecution.FileProof `json:"profile"`
}

func newConsultationIdentity(cfg *config.Config, cycle string) ConsultationIdentity {
	id := uuid.NewString()
	if cycle == "" {
		cycle = id
	}
	return ConsultationIdentity{ID: id, ProjectID: cfg.ProjectID, CycleID: cycle, Role: "supervisor"}
}

// Only route identifiers are persisted; never store the prompt, complete argv,
// environment, executable/auth paths, or raw provider errors.
func routeIdentifier(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 160 || strings.HasPrefix(s, "/") || strings.Contains(s, "://") {
		return ""
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.:/()+", c)) {
			return ""
		}
	}
	return s
}

func newConsultationReceipt(cfg *config.Config, identity ConsultationIdentity) *ConsultationReceipt {
	primary, _, _ := supervisorBackend(cfg)
	// The digest identifies the routing configuration snapshot, not a store
	// revision or catalog revision. The snapshot itself is never persisted.
	snapshot := struct {
		Backend, Model, Effort                     string
		Fallbacks                                  []string
		Backends                                   map[string]config.BackendDef
		Strict, AllowMetered                       bool
		AttemptTimeoutSeconds, TotalTimeoutSeconds int
		Registration                               *config.NativeSessionRegistrationConfig
	}{
		Backend: primary, Model: cfg.Supervisor.Model, Effort: cfg.Supervisor.Effort,
		Fallbacks: cfg.Model.FallbackBackends, Backends: cfg.Model.Backends,
		Strict: cfg.Supervisor.RequireAccountingReady, AllowMetered: cfg.Supervisor.AllowMeteredBackend,
		AttemptTimeoutSeconds: cfg.Supervisor.AttemptTimeoutSeconds, TotalTimeoutSeconds: cfg.Supervisor.TotalTimeoutSeconds,
		Registration: cfg.Supervisor.NativeSessionRegistration,
	}
	b, _ := json.Marshal(snapshot)
	hash := sha256.Sum256(b)
	return &ConsultationReceipt{SchemaVersion: 1, Identity: identity, StartedAt: time.Now().UTC(), Status: "prepared",
		RequestedBackend: routeIdentifier(primary), RequestedModel: routeIdentifier(cfg.Supervisor.Model), RequestedModelSource: "supervisor.model",
		PolicyVersion: "legacy-configured-backend-chain/v1", PolicyDigest: hex.EncodeToString(hash[:]),
		Candidates: []CandidateReceipt{}, Invocations: []InvocationReceipt{}}
}

func invocationForCommand(candidate supervisorBackendCandidate, cmd *exec.Cmd, id string, number int, fallback string) InvocationReceipt {
	kind := config.ResolveBackendKind(candidate.name, candidate.def.Provider, candidate.def.Cmd)
	r := InvocationReceipt{ID: id, Number: number, SelectedBackend: routeIdentifier(candidate.name), HarnessKind: kind,
		ConfiguredModel: routeIdentifier(candidate.def.Model), ModelArguments: []string{}, ModelEvidence: "unknown_cli_default",
		FallbackReason: fallback, RoutePolicyDecision: "configured_primary"}
	if fallback != "" {
		r.RoutePolicyDecision = "configured_fallback_model_preservation_unverified"
	}
	// A generic command may interpret any flag differently. Never infer its
	// effective model from a spelling that merely resembles a model option.
	if kind == config.BackendKindGeneric {
		r.ModelEvidence = "unknown_generic_command"
		return r
	}
	uncertain := false
	for i := 1; i < len(cmd.Args); i++ {
		a := cmd.Args[i]
		// Skip prompt-bearing arguments, even when the prompt looks like a flag.
		if (kind == config.BackendKindGemini && a == "-p") || (kind == config.BackendKindCline && a == "-y") {
			i++
			continue
		}
		if a == "--" {
			break
		}
		if a == "-c" || a == "--config" || a == "--settings" || strings.HasPrefix(a, "--config=") || strings.HasPrefix(a, "--settings=") {
			uncertain = true
		}
		var model string
		if a == "--model" || a == "-m" {
			if i+1 >= len(cmd.Args) {
				uncertain = true
				continue
			}
			i++
			model = cmd.Args[i]
		} else if strings.HasPrefix(a, "--model=") {
			model = strings.TrimPrefix(a, "--model=")
		} else {
			continue
		}
		if safe := routeIdentifier(model); safe != "" {
			r.ModelArguments = append(r.ModelArguments, safe)
		} else {
			uncertain = true
		}
	}
	if uncertain || len(r.ModelArguments) > 1 {
		r.ModelEvidence = "unknown_ambiguous_model_arguments"
		return r
	}
	if len(r.ModelArguments) == 1 {
		value := r.ModelArguments[0]
		r.EffectiveCLIModel = &value
		r.ModelEvidence = "single_explicit_cli_model_argument"
	}
	return r
}
