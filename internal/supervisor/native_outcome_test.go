package supervisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/google/uuid"
)

func nativeOutcomeFixture(request admissioncontrol.SealRequest, held bool) admissioncontrol.NativeOutcome {
	result := admissioncontrol.NativeOutcome{Binding: request.Binding, RegistrationVersion: request.RegistrationVersion, Sealed: true, Outcome: "settled", NextGenerationAllowed: true, PhysicalAttempts: 1, TerminalAttempts: 1, AttemptsDigest: strings.Repeat("a", 64)}
	prefix := "native-outcome-v1:"
	if request.AdmissionBasis == "requests" {
		result.SchemaVersion, result.AdmissionBasis, result.MoneyStatus = 2, "requests", "unknown"
		result.Outcome, prefix = "request_accounted", "native-outcome-v2:"
	}
	if held {
		code := "outcome_unknown"
		result.Outcome = "held"
		result.NextGenerationAllowed = false
		result.HoldCode = &code
		result.TerminalAttempts = 0
		result.UnresolvedAttempts = 1
	}
	body, _ := json.Marshal(result)
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var object map[string]any
	_ = decoder.Decode(&object)
	delete(object, "snapshot_digest")
	delete(object, "evidence_id")
	var canonical bytes.Buffer
	encoder := json.NewEncoder(&canonical)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(object)
	hash := sha256.Sum256(bytes.TrimSuffix(canonical.Bytes(), []byte("\n")))
	result.SnapshotDigest = hex.EncodeToString(hash[:])
	result.EvidenceID = prefix + result.SnapshotDigest
	return result
}

func TestRequestAuxiliaryRecoveryRetainsUnknownMoneyAndDoesNotRepeatInference(t *testing.T) {
	for _, role := range []string{"supervisor", "reviewer"} {
		t.Run(role, func(t *testing.T) {
			cfg, count, aux := outcomeTestConfig(t)
			cfg.Supervisor.NativeSessionRegistration.AdmissionBasis = "requests"
			calls := 0
			var saved admissioncontrol.SealRequest
			sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
				calls++
				if request.AdmissionBasis != "requests" || request.Role != role {
					t.Fatal("unbound request basis/role")
				}
				if calls == 1 {
					saved = request
					return admissioncontrol.NativeOutcome{}, errors.New("lost reply")
				}
				if request != saved {
					t.Fatal("recovery changed request identity")
				}
				return nativeOutcomeFixture(request, calls == 2), nil
			}
			id := newConsultationIdentity(cfg, "request-cycle")
			id.Role = role
			client := NewBackendLLMClient(cfg).(*backendLLMClient)
			client.role = role
			_, err := client.CompleteConsultation(id, "same prompt")
			if err == nil || aux.released.Load() != 0 {
				t.Fatal("lost reply released")
			}
			result, err := ReconcileNativeConsultation(cfg, id, "same prompt")
			if err == nil || result.Output != "" || aux.released.Load() != 0 {
				t.Fatal("partial request outcome released")
			}
			result, err = ReconcileNativeConsultation(cfg, id, "same prompt")
			if err != nil || result.Output != "done" || aux.released.Load() != 1 || nativeCalls(t, count) != 1 {
				t.Fatal("request outcome failed recovery", err)
			}
			receipt := loadReceipt(t, cfg)
			outcome := receipt.Invocations[0].NativeSession.Outcome
			if outcome == nil || outcome.Outcome != "request_accounted" || outcome.MoneyStatus != "unknown" {
				t.Fatal("saved outcome fabricated money")
			}
			if _, err := ReconcileNativeConsultation(cfg, id, "same prompt"); err != nil || calls != 3 || nativeCalls(t, count) != 1 {
				t.Fatal("saved request proof regenerated inference", err)
			}
		})
	}
}

func outcomeTestConfig(t *testing.T) (*config.Config, string, *auxiliaryFixture) {
	cfg, count, _ := nativeConfig(t)
	def := cfg.Model.Backends["primary"]
	def.Cmd = strings.TrimSuffix(def.Cmd, " fail")
	cfg.Model.Backends["primary"] = def
	cfg.Model.FallbackBackends = nil
	aux := &auxiliaryFixture{}
	cfg.RuntimeAuxiliaryLimiter = aux
	old := sealNativeConsultation
	t.Cleanup(func() { sealNativeConsultation = old })
	return cfg, count, aux
}

func TestNativeOutcomeSuccessfulAuxiliaryReleasesOnlyAfterExactDurableSeal(t *testing.T) {
	cfg, count, aux := outcomeTestConfig(t)
	calls := 0
	sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		calls++
		receipt := loadReceipt(t, cfg)
		inv := receipt.Invocations[len(receipt.Invocations)-1]
		if inv.NativeSession.OutcomeIntent == nil || *inv.NativeSession.OutcomeIntent != request || inv.OutputCheckpoint == nil || aux.released.Load() != 0 {
			t.Fatal("seal preceded durable output/intent or permit released")
		}
		return nativeOutcomeFixture(request, false), nil
	}
	id := newConsultationIdentity(cfg, "fixture-cycle")
	result, err := NewBackendLLMClient(cfg).(*backendLLMClient).CompleteConsultation(id, "same prompt")
	if err != nil || result.Output != "done" || calls != 1 || aux.acquired.Load() != 1 || aux.released.Load() != 1 || nativeCalls(t, count) != 1 {
		t.Fatalf("result=%+v err=%v calls=%d release=%d", result, err, calls, aux.released.Load())
	}
	if err := NativeAuxiliaryOutcomeComplete(cfg.StateDir, id.ID); err != nil {
		t.Fatal(err)
	}
	// A new client after restart returns the same checkpoint without invoking a
	// backend or taking another permit, even though current is completed.
	replay, err := NewBackendLLMClient(cfg).(*backendLLMClient).CompleteConsultation(id, "same prompt")
	if err != nil || replay.Output != result.Output || nativeCalls(t, count) != 1 || calls != 1 || aux.acquired.Load() != 1 {
		t.Fatal("replay generated inference", err)
	}
	if _, err := NewBackendLLMClient(cfg).(*backendLLMClient).CompleteConsultation(id, "changed prompt"); err == nil {
		t.Fatal("different prompt reused output")
	}
}

func TestNativeOutcomeLostReplyHeldLateSettlementRestoresOutputWithoutNewInference(t *testing.T) {
	cfg, count, aux := outcomeTestConfig(t)
	calls := 0
	var first admissioncontrol.SealRequest
	sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		calls++
		if calls == 1 {
			first = request
			return admissioncontrol.NativeOutcome{}, errors.New("lost reply")
		}
		if request != first {
			t.Fatal("recovery changed native identity")
		}
		return nativeOutcomeFixture(request, calls <= 3), nil
	}
	id := newConsultationIdentity(cfg, "fixture-cycle")
	_, err := NewBackendLLMClient(cfg).(*backendLLMClient).CompleteConsultation(id, "same prompt")
	if err == nil || aux.released.Load() != 0 {
		t.Fatal("lost reply released")
	}
	if _, err := NewBackendLLMClient(cfg).(*backendLLMClient).CompleteConsultation(newConsultationIdentity(cfg, "other-cycle"), "different"); err == nil {
		t.Fatal("fresh identity bypassed")
	}
	result, err := ReconcileNativeConsultation(cfg, id, "same prompt")
	if err == nil || result.Output != "" || aux.released.Load() != 0 {
		t.Fatal("held outcome released")
	}
	result, err = ReconcileNativeConsultation(cfg, id, "same prompt")
	if err != nil || result.Output != "done" || nativeCalls(t, count) != 1 || calls != 4 || aux.acquired.Load() != 1 || aux.released.Load() != 1 {
		t.Fatal("recovery didn't restore exact saved result", err)
	}
}

func TestNativeOutcomeNewCycleReconcilesPriorWithoutConsumingStaleOutput(t *testing.T) {
	cfg, count, _ := outcomeTestConfig(t)
	held := true
	sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		return nativeOutcomeFixture(request, held), nil
	}
	_, _ = NewBackendLLMClient(cfg).(*backendLLMClient).CompleteConsultation(newConsultationIdentity(cfg, "old-cycle"), "old prompt")
	held = false
	result, err := NewBackendLLMClient(cfg).(*backendLLMClient).CompleteConsultation(newConsultationIdentity(cfg, "new-cycle"), "new prompt")
	var hold *aiexecution.Hold
	if !errors.As(err, &hold) || hold.Code != "native_prior_outcome_reconciled" || result.Output != "" || nativeCalls(t, count) != 1 {
		t.Fatal("new cycle consumed stale output or launched", err)
	}
	result, err = NewBackendLLMClient(cfg).(*backendLLMClient).CompleteConsultation(newConsultationIdentity(cfg, "next-cycle"), "current prompt")
	if err != nil || result.Output != "done" || nativeCalls(t, count) != 2 {
		t.Fatal("settled prior prevented future progress", err)
	}
}

func TestNativeOutcomePersistenceFailureKeepsMarkerPermitAndCheckpoint(t *testing.T) {
	for _, failIntent := range []bool{true, false} {
		t.Run(map[bool]string{true: "intent", false: "outcome"}[failIntent], func(t *testing.T) {
			cfg, count, aux := outcomeTestConfig(t)
			calls := 0
			sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
				calls++
				return nativeOutcomeFixture(request, false), nil
			}
			client := NewBackendLLMClient(cfg).(*backendLLMClient)
			client.receiptSave = func(r *ConsultationReceipt) error {
				for _, inv := range r.Invocations {
					if inv.NativeSession != nil && inv.NativeSession.OutcomeIntent != nil && (failIntent || inv.NativeSession.Outcome != nil) {
						return errors.New("fsync failure")
					}
				}
				return (&consultationStore{dir: filepath.Join(cfg.StateDir, "supervisor-consultations")}).save(r)
			}
			id := newConsultationIdentity(cfg, "fixture-cycle")
			result, err := client.CompleteConsultation(id, "prompt")
			if err == nil || result.Output != "" || aux.released.Load() != 0 || nativeCalls(t, count) != 1 {
				t.Fatal("failed fsync released", err)
			}
			if failIntent && calls != 0 || !failIntent && calls != 1 {
				t.Fatal("call crossed failed intent")
			}
			if _, err := os.Stat(filepath.Join(cfg.StateDir, "supervisor-consultations", "launch.json")); err != nil {
				t.Fatal(err)
			}
			result, err = ReconcileNativeConsultation(cfg, id, "prompt")
			if err != nil || result.Output != "done" || nativeCalls(t, count) != 1 {
				t.Fatal("recovery lost output", err)
			}
		})
	}
}

func TestNativeOutcomeCorruptForeignAndTruncatedCheckpointRetainPermit(t *testing.T) {
	for _, kind := range []string{"bytes", "symlink", "foreign", "permission", "pointer", "model", "outcome"} {
		t.Run(kind, func(t *testing.T) {
			cfg, count, aux := outcomeTestConfig(t)
			sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
				return nativeOutcomeFixture(request, true), nil
			}
			id := newConsultationIdentity(cfg, "fixture-cycle")
			_, _ = NewBackendLLMClient(cfg).(*backendLLMClient).CompleteConsultation(id, "prompt")
			receipt := loadReceipt(t, cfg)
			inv := &receipt.Invocations[0]
			path := filepath.Join(cfg.StateDir, "supervisor-consultations", inv.OutputCheckpoint.Filename)
			switch kind {
			case "bytes":
				_ = os.WriteFile(path, []byte("{}"), 0600)
			case "symlink":
				_ = os.Remove(path)
				_ = os.Symlink("/dev/null", path)
			case "foreign":
				body, _ := os.ReadFile(path)
				var output nativeOutputRecord
				_ = json.Unmarshal(body, &output)
				output.RoleRunID = uuid.NewString()
				body, _ = json.Marshal(output)
				_ = os.WriteFile(path, body, 0600)
			case "permission":
				_ = os.Chmod(path, 0644)
			case "pointer":
				inv.OutputCheckpoint.Bytes++
			case "model":
				cfg.Supervisor.Model = "different-model"
			case "outcome":
				inv.NativeSession.Outcome.SnapshotDigest = strings.Repeat("f", 64)
			}
			if kind == "pointer" || kind == "outcome" {
				_ = (&consultationStore{dir: filepath.Join(cfg.StateDir, "supervisor-consultations")}).save(&receipt)
			}
			result, err := ReconcileNativeConsultation(cfg, id, "prompt")
			if err == nil || result.Output != "" || aux.released.Load() != 0 || nativeCalls(t, count) != 1 {
				t.Fatal("corrupt evidence released")
			}
		})
	}
}

func TestNativeOutcomeAllInvocationsMustSettleBeforeRolePermitRelease(t *testing.T) {
	cfg, count, aux := outcomeTestConfig(t)
	def := cfg.Model.Backends["primary"]
	def.Cmd += " fail"
	cfg.Model.Backends["primary"] = def
	cfg.Model.FallbackBackends = []string{"secondary"}
	calls := 0
	sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		calls++
		return nativeOutcomeFixture(request, calls == 2), nil
	}
	id := newConsultationIdentity(cfg, "fixture-cycle")
	result, err := NewBackendLLMClient(cfg).(*backendLLMClient).CompleteConsultation(id, "prompt")
	if err == nil || result.Output != "" || nativeCalls(t, count) != 2 || aux.released.Load() != 0 {
		t.Fatalf("partial role release: calls=%d native=%d err=%v", calls, nativeCalls(t, count), err)
	}
	receipt := loadReceipt(t, cfg)
	if len(receipt.Invocations) != 2 || !receipt.Invocations[0].NativeSession.Outcome.NextGenerationAllowed || receipt.Invocations[1].NativeSession.Outcome.NextGenerationAllowed {
		t.Fatal("bad multi-native fixture")
	}
	result, err = ReconcileNativeConsultation(cfg, id, "prompt")
	if err != nil || result.Output != "done" || calls != 3 || nativeCalls(t, count) != 2 || aux.released.Load() != 1 {
		t.Fatal("did not reconcile all saved identities", err)
	}
}

func TestNativeOutcomeReviewerSameClaimRestoresExactModelOutput(t *testing.T) {
	cfg, count, fixture := nativeConfig(t)
	def := cfg.Model.Backends["primary"]
	def.Cmd = strings.TrimSuffix(def.Cmd, " fail")
	cfg.Model.Backends["primary"] = def
	aux := &auxiliaryFixture{}
	cfg.RuntimeAuxiliaryLimiter = aux
	receiptCfg := *cfg
	receiptCfg.StateDir = filepath.Join(cfg.StateDir, "native-reviews")
	fixture.cfg = &receiptCfg
	old := sealNativeConsultation
	t.Cleanup(func() { sealNativeConsultation = old })
	sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		return nativeOutcomeFixture(request, true), nil
	}
	claim := uuid.NewString()
	// #1239: the reviewer exited with a complete verdict while the authority
	// still reported its single attempt as claimed. The verdict is returned
	// with accounting pending; receipt, marker and permit stay held.
	output, err := CompleteNativeReview(context.Background(), cfg, "claude-opus-5", claim, "diff")
	if err != nil || output.Output != "done" || !output.AccountingPending || aux.released.Load() != 0 || nativeCalls(t, count) != 1 {
		t.Fatalf("pending verdict not surfaced: output=%+v err=%v released=%d", output, err, aux.released.Load())
	}
	receipt := loadReceipt(t, &receiptCfg)
	if receipt.NativeOutcomeComplete || receipt.EndedAt != nil || nativeInvocationsAllowed(&receipt) || receipt.Invocations[0].NativeSession.Outcome == nil || receipt.Invocations[0].NativeSession.Outcome.UnresolvedAttempts != 1 {
		t.Fatalf("pending receipt sealed as complete: %+v", receipt)
	}
	if _, err := os.Stat(filepath.Join(receiptCfg.StateDir, "supervisor-consultations", "launch.json")); err != nil {
		t.Fatal("launch marker dropped while accounting pending", err)
	}
	if pending, err := PendingAuxiliaryRuns(cfg.StateDir); err != nil || len(pending) != 1 {
		t.Fatalf("pending receipt released occupancy: %v %v", pending, err)
	}
	// A different claim cannot bypass the unsettled prior and launches nothing.
	_, err = CompleteNativeReview(context.Background(), cfg, "claude-opus-5", uuid.NewString(), "other diff")
	var hold *aiexecution.Hold
	if !errors.As(err, &hold) || hold.Code != "native_outcome_unverified" || nativeCalls(t, count) != 1 || aux.acquired.Load() != 1 {
		t.Fatal("fresh claim bypassed pending accounting", err)
	}
	sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		return nativeOutcomeFixture(request, false), nil
	}
	// Later reconcile of the same claim settles the receipt without inference.
	output, err = CompleteNativeReview(context.Background(), cfg, "claude-opus-5", claim, "diff")
	if err != nil || output.Output != "done" || output.AccountingPending || nativeCalls(t, count) != 1 || aux.acquired.Load() != 1 || aux.released.Load() != 1 {
		t.Fatal("review replay generated", err)
	}
	receipt = loadReceipt(t, &receiptCfg)
	if !receipt.NativeOutcomeComplete || receipt.Status != "succeeded" || !nativeInvocationsAllowed(&receipt) {
		t.Fatalf("settled receipt not completed: %+v", receipt)
	}
	if err := NativeAuxiliaryOutcomeComplete(receiptCfg.StateDir, claim); err != nil {
		t.Fatal(err)
	}
	_, err = CompleteNativeReview(context.Background(), cfg, "other-model", claim, "diff")
	if !errors.As(err, &hold) || hold.Code != "native_input_conflict" {
		t.Fatal("foreign model reused review", err)
	}
}

func TestNativeOutcomeLiveSealWaitsForLateSettlement(t *testing.T) {
	cfg, count, aux := outcomeTestConfig(t)
	nativeSettleWindow, nativeSettleBackoff = 2*time.Second, 5*time.Millisecond
	calls := 0
	var first admissioncontrol.SealRequest
	sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		calls++
		if calls == 1 {
			first = request
		} else if request != first {
			t.Fatal("settlement poll changed native identity")
		}
		if calls == 2 {
			return admissioncontrol.NativeOutcome{}, errors.New("transient reply loss")
		}
		return nativeOutcomeFixture(request, calls < 4), nil
	}
	id := newConsultationIdentity(cfg, "late-settlement")
	result, err := NewBackendLLMClient(cfg).(*backendLLMClient).CompleteConsultation(id, "same prompt")
	if err != nil || result.Output != "done" || result.AccountingPending || calls != 4 || aux.released.Load() != 1 || nativeCalls(t, count) != 1 {
		t.Fatalf("result=%+v err=%v calls=%d released=%d", result, err, calls, aux.released.Load())
	}
	receipt := loadReceipt(t, cfg)
	if !receipt.NativeOutcomeComplete || receipt.Invocations[0].NativeSession.Outcome.UnresolvedAttempts != 0 || !nativeInvocationsAllowed(&receipt) {
		t.Fatal("settled seal not persisted")
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "supervisor-consultations", "launch.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("launch marker retained after settled seal", err)
	}
}

func TestNativeOutcomeUnsettledWindowStaysHeldWithoutCompleteVerdict(t *testing.T) {
	t.Run("supervisor", func(t *testing.T) {
		cfg, count, aux := outcomeTestConfig(t)
		nativeSettleWindow, nativeSettleBackoff = 60*time.Millisecond, 5*time.Millisecond
		calls := 0
		sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
			calls++
			return nativeOutcomeFixture(request, true), nil
		}
		id := newConsultationIdentity(cfg, "never-settles")
		result, err := NewBackendLLMClient(cfg).(*backendLLMClient).CompleteConsultation(id, "same prompt")
		var hold *aiexecution.Hold
		if !errors.As(err, &hold) || hold.Code != "native_outcome_unverified" || result.Output != "" || result.AccountingPending || aux.released.Load() != 0 || calls < 2 || nativeCalls(t, count) != 1 {
			t.Fatalf("supervisor surfaced unsettled output: result=%+v err=%v calls=%d", result, err, calls)
		}
		receipt := loadReceipt(t, cfg)
		if receipt.NativeOutcomeComplete || receipt.Invocations[0].NativeSession.Outcome == nil || receipt.Invocations[0].NativeSession.Outcome.UnresolvedAttempts != 1 {
			t.Fatalf("held seal not persisted: %+v", receipt)
		}
		if _, err := os.Stat(filepath.Join(cfg.StateDir, "supervisor-consultations", "launch.json")); err != nil {
			t.Fatal("launch marker dropped on unsettled seal", err)
		}
	})
	t.Run("reviewer_local_failure", func(t *testing.T) {
		cfg, count, fixture := nativeConfig(t)
		aux := &auxiliaryFixture{}
		cfg.RuntimeAuxiliaryLimiter = aux
		receiptCfg := *cfg
		receiptCfg.StateDir = filepath.Join(cfg.StateDir, "native-reviews")
		fixture.cfg = &receiptCfg
		old := sealNativeConsultation
		t.Cleanup(func() { sealNativeConsultation = old })
		sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
			return nativeOutcomeFixture(request, true), nil
		}
		output, err := CompleteNativeReview(context.Background(), cfg, "claude-opus-5", uuid.NewString(), "diff")
		var hold *aiexecution.Hold
		if !errors.As(err, &hold) || hold.Code != "native_outcome_unverified" || output.Output != "" || output.AccountingPending || aux.released.Load() != 0 || nativeCalls(t, count) != 1 {
			t.Fatalf("failed reviewer output surfaced as pending verdict: output=%+v err=%v", output, err)
		}
	})
}

func TestNativeOutcomeMissingMarkerCannotBypassHeldReceiptAndUnverifiedOSCannotSeal(t *testing.T) {
	cfg, count, aux := outcomeTestConfig(t)
	calls := 0
	sealNativeConsultation = func(_ admissioncontrol.Client, request admissioncontrol.SealRequest) (admissioncontrol.NativeOutcome, error) {
		calls++
		return nativeOutcomeFixture(request, true), nil
	}
	id := newConsultationIdentity(cfg, "fixture-cycle")
	_, _ = NewBackendLLMClient(cfg).(*backendLLMClient).CompleteConsultation(id, "prompt")
	receipt := loadReceipt(t, cfg)
	inv := &receipt.Invocations[0]
	inv.ProcessLease = &NativeInvocationProcessLease{Unit: "maestro-native-" + strings.ReplaceAll(inv.ID, "-", "") + ".service", Manager: "system"}
	inv.ProcessTerminationVerified = false
	_ = (&consultationStore{dir: filepath.Join(cfg.StateDir, "supervisor-consultations")}).save(&receipt)
	if _, err := ReconcileNativeConsultation(cfg, id, "prompt"); err == nil || calls != 1 || aux.released.Load() != 0 {
		t.Fatal("unverified cgroup opened seal")
	}
	_ = os.Remove(filepath.Join(cfg.StateDir, "supervisor-consultations", "launch.json"))
	if _, err := NewBackendLLMClient(cfg).(*backendLLMClient).CompleteConsultation(newConsultationIdentity(cfg, "new-cycle"), "prompt"); err == nil || nativeCalls(t, count) != 1 {
		t.Fatal("missing marker erased native financial hold")
	}
}
