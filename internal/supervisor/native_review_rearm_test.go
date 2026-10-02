package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/aiexecution"
)

func TestNativeReviewRearmProofRequiresExactNormalTerminalEvidence(t *testing.T) {
	for _, mode := range []string{"valid", "wrong_run", "wrong_project", "missing_process", "invalid_process", "unknown_outcome"} {
		t.Run(mode, func(t *testing.T) {
			cfg, _, _ := outcomeTestConfig(t)
			cfg.Supervisor.NativeSessionRegistration.AdmissionBasis = "requests"
			def := cfg.Model.Backends["primary"]
			def.Cmd += " fail"
			cfg.Model.Backends["primary"] = def
			sealNativeConsultation = func(_ admissioncontrol.Client, r admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
				return nativeOutcomeFixture(r, false), nil
			}
			id := newConsultationIdentity(cfg, "")
			id.Role = "reviewer"
			id.CycleID = id.ID
			client := NewBackendLLMClient(cfg).(*backendLLMClient)
			client.role = "reviewer"
			_, _ = client.CompleteConsultation(id, "same diff")
			r := loadReceipt(t, cfg)
			if len(r.Invocations) != 1 {
				t.Fatal("fixture not launched")
			}
			inv := &r.Invocations[0]
			unit := "maestro-native-" + strings.ReplaceAll(inv.ID, "-", "") + ".service"
			pin := aiexecution.FileProof{Path: "/test/reviewer.json", SHA256: strings.Repeat("a", 64)}
			proof := &aiexecution.NativeProcessTermination{Version: 1, Profile: pin, NativeSessionID: inv.ID, Unit: unit, Cgroup: "/test/" + unit, BootID: "00000000-0000-4000-8000-000000000001", InvocationID: strings.Repeat("b", 32), StartedAt: inv.StartedAt, EndedAt: inv.EndedAt, LocalStatus: "failed", ExitCode: 1}
			b, _ := json.Marshal(proof)
			sum := sha256.Sum256(b)
			proof.Digest = hex.EncodeToString(sum[:])
			inv.ProcessLease = &NativeInvocationProcessLease{Unit: unit, Manager: "system", Profile: pin}
			inv.ProcessTerminationVerified = true
			inv.ProcessTermination = proof
			inv.ProcessTerminationDigest = proof.Digest
			run := cfg.Supervisor.NativeSessionRegistration.BudgetRunID
			project := cfg.ProjectID
			switch mode {
			case "wrong_run":
				run = "another-run"
			case "wrong_project":
				project = "another-project"
			case "missing_process":
				inv.ProcessLease = nil
			case "invalid_process":
				proof.Unit = "wrong.service"
			case "unknown_outcome":
				inv.NativeSession.Outcome = nil
			}
			if err := (&consultationStore{dir: filepath.Join(cfg.StateDir, "supervisor-consultations")}).save(&r); err != nil {
				t.Fatal(err)
			}
			got, err := NativeReviewRearmProof(cfg.StateDir, id.ID, project, run)
			if mode == "valid" {
				if err != nil || len(got) != 64 {
					t.Fatal("normal terminal proof rejected", err)
				}
			} else if err == nil {
				t.Fatal("unsafe prior accepted")
			}
		})
	}
}
