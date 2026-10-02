package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/google/uuid"
)

// DeriveAbandonedConsultationConfig builds the minimal configuration
// replayNativeConsultation needs for a receipt root whose project is no longer
// configured (#1232): project identity, the receipt root and the exact native
// registration pins recorded in the abandoned receipt. The control socket and
// authority UID are not stored in a receipt (only their digest, the authority
// pin), so they are taken from a supplied authority whose pin matches. Without a
// matching authority the root cannot be sealed and stays held. Nothing is
// written; no process is launched.
func DeriveAbandonedConsultationConfig(root string, authorities []config.NativeSessionRegistrationConfig, limiter aiexecution.AuxiliaryLimiter) (*config.Config, string, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if !filepath.IsAbs(root) {
		return nil, "", aiexecution.Held("auxiliary_receipt_root_invalid")
	}
	data, err := readAuxiliaryReceipt(root, "current.json")
	if errors.Is(err, os.ErrNotExist) {
		// A launch marker without a current receipt has nothing to replay.
		return nil, "", aiexecution.Held("native_receipt_missing")
	}
	if err != nil {
		return nil, "", err
	}
	var prior ConsultationReceipt
	if aiexecution.DecodeStrict(data, &prior) != nil || prior.SchemaVersion != 1 || uuid.Validate(prior.Identity.ID) != nil {
		return nil, "", aiexecution.Held("native_receipt_invalid")
	}
	role := prior.Identity.Role
	if (role != "supervisor" && role != "reviewer") || prior.Identity.ProjectID == "" {
		return nil, "", aiexecution.Held("native_receipt_invalid")
	}
	if !hasNativeSession(&prior) {
		return nil, "", aiexecution.Held("native_receipt_missing")
	}
	var native *NativeSessionRegistrationReceipt
	if prior.PlannedInvocation != nil {
		native = prior.PlannedInvocation.NativeSession
	} else {
		native = prior.Invocations[len(prior.Invocations)-1].NativeSession
	}
	if native == nil || !nativeDigest(native.AuthorityPin) || native.Request.ProjectID != prior.Identity.ProjectID || native.Request.Role != role {
		return nil, "", aiexecution.Held("native_receipt_invalid")
	}
	var authority *config.NativeSessionRegistrationConfig
	for i := range authorities {
		candidate := authorities[i]
		if candidate.AuthorityUID == nil || !filepath.IsAbs(candidate.ControlSocket) {
			continue
		}
		pin := nativeHash(struct {
			Socket string
			UID    uint32
		}{candidate.ControlSocket, *candidate.AuthorityUID})
		if pin == native.AuthorityPin {
			authority = &candidate
			break
		}
	}
	if authority == nil {
		return nil, "", aiexecution.Held("native_authority_unknown")
	}
	uid := *authority.AuthorityUID
	binding := native.Request.Binding
	cfg := &config.Config{ProjectID: prior.Identity.ProjectID, StateDir: root}
	// The removed project's ai_execution policy is unknown; its receipt shows
	// the posture it ran under. A verified-route project records a process
	// lease on every native invocation, so any lease in the receipt restores
	// the strict requirement and an invocation without one stays unsealed.
	cfg.AIExecution.RequireVerifiedRoute = receiptRecordsProcessLease(&prior)
	cfg.RuntimeAuxiliaryLimiter = limiter
	cfg.Supervisor.NativeSessionRegistration = &config.NativeSessionRegistrationConfig{
		AdmissionBasis:        binding.AdmissionBasis,
		ControlSocket:         authority.ControlSocket,
		AuthorityUID:          &uid,
		ExpectedPolicyVersion: native.Request.ExpectedVersion,
		FleetID:               binding.FleetID,
		GatewayScope:          binding.GatewayScope,
		BudgetRunID:           binding.RunID,
		// Sealing never registers; the TTL only has to be a valid duration.
		TTLSeconds: 1,
	}
	return cfg, role, nil
}

// ReconcileAbandonedRoot replays the abandoned consultation under root with a
// configuration derived from its own receipt (see
// DeriveAbandonedConsultationConfig). The result codes are those of
// ReconcileNativeConsultation: native_prior_outcome_reconciled means the
// marker was sealed and capacity released.
func ReconcileAbandonedRoot(root string, authorities []config.NativeSessionRegistrationConfig, limiter aiexecution.AuxiliaryLimiter) (string, error) {
	cfg, role, err := DeriveAbandonedConsultationConfig(root, authorities, limiter)
	if err != nil {
		return "", err
	}
	_, err = ReconcileNativeConsultation(cfg, ConsultationIdentity{ID: uuid.NewString(), ProjectID: cfg.ProjectID, Role: role}, "")
	return role, err
}

func receiptRecordsProcessLease(receipt *ConsultationReceipt) bool {
	if receipt.PlannedInvocation != nil && receipt.PlannedInvocation.ProcessLease != nil {
		return true
	}
	for _, inv := range receipt.Invocations {
		if inv.ProcessLease != nil {
			return true
		}
	}
	return false
}
