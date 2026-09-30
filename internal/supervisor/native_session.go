package supervisor

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/config"
	"github.com/google/uuid"
)

type NativeSessionRegistrationReceipt struct {
	Request         admissioncontrol.RegistrationRequest `json:"request"`
	Acknowledgement *admissioncontrol.Acknowledgement    `json:"acknowledgement,omitempty"`
	AuthorityPin    string                               `json:"authority_pin,omitempty"`
	OutcomeIntent   *admissioncontrol.SealRequest        `json:"outcome_intent,omitempty"`
	Outcome         *admissioncontrol.NativeOutcome      `json:"outcome,omitempty"`
}

func registrationClient(cfg *config.Config) (admissioncontrol.Client, error) {
	r := cfg.Supervisor.NativeSessionRegistration
	if r == nil || !filepath.IsAbs(r.ControlSocket) || r.AuthorityUID == nil || r.ExpectedPolicyVersion <= 0 ||
		!admissioncontrol.Identifier(r.FleetID) || !admissioncontrol.Identifier(r.GatewayScope) ||
		!admissioncontrol.Identifier(r.BudgetRunID) || !admissioncontrol.Identifier(cfg.ProjectID) || r.TTLSeconds <= 0 || r.TTLSeconds > math.MaxInt64-time.Now().Unix() {
		return admissioncontrol.Client{}, &ConsultationHold{Code: "registration_configuration_invalid"}
	}
	return admissioncontrol.Client{SocketPath: r.ControlSocket, AuthorityUID: *r.AuthorityUID, Timeout: 7 * time.Second}, nil
}

func prepareNativeSession(cfg *config.Config, identity ConsultationIdentity, invocation *InvocationReceipt, cmd *exec.Cmd) error {
	if _, err := registrationClient(cfg); err != nil {
		return err
	}
	// Wrapper/generic commands cannot prove that these argv reach Claude.
	if invocation.HarnessKind != config.BackendKindClaude || filepath.Base(cmd.Path) != "claude" {
		return &ConsultationHold{Code: "native_session_harness_unsupported"}
	}
	if rejectsSessionFlags(cmd.Args[1:]) {
		return &ConsultationHold{Code: "native_session_argument_conflict"}
	}
	r := cfg.Supervisor.NativeSessionRegistration
	invocation.NativeSession = &NativeSessionRegistrationReceipt{AuthorityPin: nativeAuthorityPin(cfg), Request: admissioncontrol.RegistrationRequest{
		Binding: admissioncontrol.Binding{GatewayScope: r.GatewayScope, NativeSessionID: invocation.ID, FleetID: r.FleetID,
			ProjectID: identity.ProjectID, RunID: r.BudgetRunID, Role: identity.Role, ExpiresAt: time.Now().Unix() + r.TTLSeconds},
		ExpectedVersion: r.ExpectedPolicyVersion,
	}}
	cmd.Args = append(cmd.Args, "--session-id", invocation.ID)
	return nil
}

// Conservative parsing also refuses short clusters containing -r/-c and an
// end-of-options marker; no ambiguity can override the owned session argument.
func rejectsSessionFlags(args []string) bool {
	for _, arg := range args {
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "--session-id", "--resume", "--continue", "--fork-session", "--no-session-persistence", "--":
			return true
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.ContainsAny(arg[1:], "rc") {
			return true
		}
	}
	return false
}

func registerNativeSession(cfg *config.Config, registration *NativeSessionRegistrationReceipt, timeout time.Duration) error {
	client, err := registrationClient(cfg)
	if err != nil {
		return err
	}
	if timeout <= 0 {
		return &ConsultationHold{Code: "registration_launch_deadline"}
	}
	if timeout < client.Timeout {
		client.Timeout = timeout
	}
	ack, err := client.Register(registration.Request)
	if err != nil {
		var hold *admissioncontrol.Hold
		if errors.As(err, &hold) {
			code := hold.Code
			if !strings.HasPrefix(code, "registration_") {
				code = "registration_" + code
			}
			return &ConsultationHold{Code: code}
		}
		return &ConsultationHold{Code: "registration_authority_unavailable"}
	}
	registration.Acknowledgement = &ack
	return nil
}

// ReconcileNativeSupervisorRegistration explicitly repeats only the exact
// persisted idempotent registration. It never starts/resumes a process, spends,
// mutates policy, clears a launch marker or extends the registration expiry.
// A caller must deliberately invoke this API; Complete never reconciles itself.
func ReconcileNativeSupervisorRegistration(cfg *config.Config) (*ConsultationReceipt, error) {
	if _, err := registrationClient(cfg); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		return nil, &ConsultationHold{Code: "receipt_store_unavailable"}
	}
	store, unlock, err := lockConsultationStore(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	data, err := os.ReadFile(filepath.Join(store.dir, "current.json"))
	if err != nil {
		return nil, &ConsultationHold{Code: "receipt_invalid"}
	}
	var receipt ConsultationReceipt
	if json.Unmarshal(data, &receipt) != nil || receipt.SchemaVersion != 1 || uuid.Validate(receipt.Identity.ID) != nil || receipt.Identity.ProjectID != cfg.ProjectID || receipt.Identity.Role != "supervisor" || receipt.PlannedInvocation == nil || receipt.PlannedInvocation.NativeSession == nil {
		return nil, &ConsultationHold{Code: "receipt_invalid"}
	}
	inv := receipt.PlannedInvocation
	registration := inv.NativeSession
	r := cfg.Supervisor.NativeSessionRegistration
	request := registration.Request
	if !request.Valid() || request.NativeSessionID != inv.ID || request.ProjectID != cfg.ProjectID || request.Role != "supervisor" || request.GatewayScope != r.GatewayScope || request.FleetID != r.FleetID || request.RunID != r.BudgetRunID || request.ExpectedVersion != r.ExpectedPolicyVersion {
		return nil, &ConsultationHold{Code: "registration_identity_conflict"}
	}
	index := -1
	for i, candidate := range receipt.Candidates {
		if candidate.IntentID == inv.ID && (candidate.Status == "registration_intent" || candidate.Status == "launch_intent" || candidate.Status == "registration_reconciled_not_launched") {
			index = i
		}
	}
	if index < 0 {
		return nil, &ConsultationHold{Code: "receipt_invalid"}
	}
	for _, previous := range receipt.Invocations {
		if previous.ID == inv.ID {
			return nil, &ConsultationHold{Code: "unresolved_launch_intent"}
		}
	}
	// A marker may be absent if the crash happened after receipt sync but before
	// marker creation. Recreate it from that same durable request, never new data.
	marker, err := os.ReadFile(filepath.Join(store.dir, "registration.json"))
	if err == nil {
		var pending struct {
			ConsultationID string                            `json:"consultation_id"`
			Registration   *NativeSessionRegistrationReceipt `json:"registration"`
		}
		if json.Unmarshal(marker, &pending) != nil || pending.ConsultationID != receipt.Identity.ID || pending.Registration == nil || pending.Registration.Request != request {
			return nil, &ConsultationHold{Code: "registration_identity_conflict"}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, &ConsultationHold{Code: "receipt_store_unavailable"}
	}
	if err := store.beginRegistration(receipt.Identity, registration); err != nil {
		return nil, &ConsultationHold{Code: "receipt_persistence_failed"}
	}
	if err := registerNativeSession(cfg, registration, 7*time.Second); err != nil {
		return &receipt, err
	}
	receipt.Candidates[index].Status = "registration_reconciled_not_launched"
	receipt.Status = "registration_reconciled_not_launched"
	now := time.Now().UTC()
	receipt.EndedAt = &now
	// Keep the planned record as evidence of an acknowledged session that was
	// never launched. Recovery cannot turn it into an invocation retroactively.
	if err := store.save(&receipt); err != nil {
		return &receipt, &ConsultationHold{Code: "receipt_persistence_failed"}
	}
	if err := store.finishRegistration(); err != nil {
		return &receipt, &ConsultationHold{Code: "receipt_persistence_failed"}
	}
	return &receipt, nil
}
