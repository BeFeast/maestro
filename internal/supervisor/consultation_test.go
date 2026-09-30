package supervisor

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/google/uuid"
)

func receiptConfig(t *testing.T) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	count := filepath.Join(dir, "launches")
	script := filepath.Join(dir, "carrier")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf 'call\\n' >> '"+count+"'\ncase \"$1\" in\n fail) exit 2;;\n slow) sleep 5;;\nesac\nprintf 'done'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ProjectID: uuid.NewString(), StateDir: filepath.Join(dir, "state"), LocalPath: dir,
		Supervisor: config.SupervisorConfig{Model: "requested-model", TempDir: filepath.Join(dir, "tmp"), AttemptTimeoutSeconds: 1},
		Model: config.ModelConfig{Default: "primary", FallbackBackends: []string{"secondary"}, Backends: map[string]config.BackendDef{
			"primary":   {Cmd: script + " fail", Provider: "claude", Model: "configured-primary"},
			"secondary": {Cmd: script, Provider: "claude", Model: "configured-secondary"},
		}}}
	return cfg, count
}

func loadReceipt(t *testing.T, cfg *config.Config) ConsultationReceipt {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cfg.StateDir, "supervisor-consultations", "current.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r ConsultationReceipt
	if err = json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func assertHold(t *testing.T, err error, code string) {
	t.Helper()
	var hold *ConsultationHold
	if !errors.As(err, &hold) || hold.Code != code {
		t.Fatalf("error=%v, want hold %s", err, code)
	}
}

func TestConsultationReceiptsFallbackAndAncestry(t *testing.T) {
	cfg, count := receiptConfig(t)
	c := NewBackendLLMClient(cfg).(*backendLLMClient)
	id := newConsultationIdentity(cfg, "cycle-1")
	id.ParentRoleRunID = uuid.NewString()
	result, err := c.CompleteConsultation(id, "secret prompt --model=do-not-record")
	if err != nil || result.Output != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	r := loadReceipt(t, cfg)
	if r.Identity != id || r.RequestedModel != "requested-model" || r.Capability.Ready() || r.CatalogRevision != nil || r.PolicyDigest == "" {
		t.Fatalf("receipt=%+v", r)
	}
	if len(r.Invocations) != 2 || r.Invocations[0].Status != "failed" || r.Invocations[1].Status != "succeeded" {
		t.Fatalf("invocations=%+v", r.Invocations)
	}
	for i, inv := range r.Invocations {
		if inv.Number != i+1 || uuid.Validate(inv.ID) != nil || inv.EffectiveCLIModel == nil || *inv.EffectiveCLIModel != "requested-model" || inv.UpstreamActualModel != nil || inv.AccountAlias != nil {
			t.Fatalf("invocation=%+v", inv)
		}
	}
	if r.Invocations[0].ID == r.Invocations[1].ID || r.Invocations[1].FallbackReason != "previous_candidate_failed" {
		t.Fatal("fallback ancestry lost")
	}
	b, _ := json.Marshal(r)
	for _, secret := range []string{"secret prompt", "do-not-record", cfg.LocalPath} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("receipt contains %q", secret)
		}
	}
	if _, err := c.Complete("second consult"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "supervisor-consultations", id.ID+".json")); err != nil {
		t.Fatal("previous receipt not archived", err)
	}
	calls, _ := os.ReadFile(count)
	if strings.Count(string(calls), "call") != 4 {
		t.Fatalf("calls=%s", calls)
	}
}

func TestConsultationReceiptPersistenceBlocksLaunchAndRetry(t *testing.T) {
	for _, failAt := range []int{1, 2, 3} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			cfg, count := receiptConfig(t)
			c := NewBackendLLMClient(cfg).(*backendLLMClient)
			saves := 0
			c.receiptSave = func(r *ConsultationReceipt) error {
				saves++
				if saves == failAt {
					return errors.New("disk failed")
				}
				return (&consultationStore{dir: filepath.Join(cfg.StateDir, "supervisor-consultations")}).save(r)
			}
			_, err := c.Complete("prompt")
			assertHold(t, err, "receipt_persistence_failed")
			calls, _ := os.ReadFile(count)
			want := 0
			if failAt == 3 {
				want = 1
			}
			if got := strings.Count(string(calls), "call"); got != want {
				t.Fatalf("launches=%d want=%d", got, want)
			}
			if failAt == 3 {
				// Fresh client models restart; the pending launch survives and denies
				// retry even though the previous process actually finished.
				_, err = NewBackendLLMClient(cfg).Complete("retry")
				assertHold(t, err, "unresolved_launch_intent")
				calls, _ = os.ReadFile(count)
				if strings.Count(string(calls), "call") != 1 {
					t.Fatal("blind retry")
				}
			}
		})
	}
}

func TestConsultationSkipsAndStartFailureAreNotInvocations(t *testing.T) {
	cfg, count := receiptConfig(t)
	c := NewBackendLLMClient(cfg).(*backendLLMClient)
	c.backendHealth = map[string]state.BackendHealth{"primary": {State: state.BackendHealthCooldown}}
	c.memory.skipUntil["secondary"] = time.Now().Add(time.Hour)
	_, err := c.Complete("prompt")
	if err == nil {
		t.Fatal("expected no available candidates")
	}
	r := loadReceipt(t, cfg)
	if len(r.Invocations) != 0 || len(r.Candidates) != 2 {
		t.Fatalf("receipt=%+v", r)
	}
	if _, err := os.Stat(count); !os.IsNotExist(err) {
		t.Fatal("skipped candidate launched")
	}
	cfg.Model.FallbackBackends = nil
	d := cfg.Model.Backends["primary"]
	d.Cmd = filepath.Join(t.TempDir(), "absent")
	cfg.Model.Backends["primary"] = d
	_, err = NewBackendLLMClient(cfg).Complete("prompt")
	if err == nil {
		t.Fatal("expected start error")
	}
	r = loadReceipt(t, cfg)
	if len(r.Invocations) != 0 || r.Candidates[0].Status != "start_failed" {
		t.Fatalf("receipt=%+v", r)
	}
}

func TestConsultationTimeoutIsDistinctInvocation(t *testing.T) {
	cfg, _ := receiptConfig(t)
	d := cfg.Model.Backends["primary"]
	d.Cmd = strings.TrimSuffix(d.Cmd, " fail") + " slow"
	cfg.Model.Backends["primary"] = d
	if _, err := NewBackendLLMClient(cfg).Complete("prompt"); err != nil {
		t.Fatal(err)
	}
	r := loadReceipt(t, cfg)
	if len(r.Invocations) != 2 || r.Invocations[0].Status != "timed_out" || r.Invocations[1].Status != "succeeded" {
		t.Fatalf("invocations=%+v", r.Invocations)
	}
}

func TestConsultationStrictHoldAndNoAIShortCircuit(t *testing.T) {
	cfg, count := receiptConfig(t)
	cfg.Supervisor.RequireAccountingReady = true
	_, err := NewBackendLLMClient(cfg).Complete("prompt")
	assertHold(t, err, "accounting_route_unsupported")
	if _, err := os.Stat(count); !os.IsNotExist(err) {
		t.Fatal("strict path launched")
	}
	engineCfg := testConfig(t)
	engineCfg.Supervisor.RequireAccountingReady = true
	llm := idleLLM()
	decision, err := testLLMEngine(engineCfg, &fakeReader{}, llm).Decide(state.NewState())
	if err != nil || llm.calls != 0 || decision.ConsultationID != "" {
		t.Fatalf("deterministic result=%+v err=%v calls=%d", decision, err, llm.calls)
	}
	engineCfg.Supervisor.AlwaysConsultLLM = true
	decision, err = testLLMEngine(engineCfg, &fakeReader{}, llm).Decide(state.NewState())
	if err != nil || llm.calls != 0 || decision.ErrorClass != "accounting_route_unsupported" {
		t.Fatalf("strict result=%+v err=%v calls=%d", decision, err, llm.calls)
	}
	if _, err := os.Stat(filepath.Join(engineCfg.StateDir, "supervisor-consultations")); !os.IsNotExist(err) {
		t.Fatal("no-AI path fabricated receipts")
	}
}

func TestConsultationModelEvidenceDoesNotGuessPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		args       []string
		want       *string
		evidence   string
	}{
		{"single", "claude", []string{"-p", "--model=selected"}, stringPointer("selected"), "single_explicit_cli_model_argument"},
		{"repeated", "claude", []string{"--model", "pinned", "--model", "requested"}, nil, "unknown_ambiguous_model_arguments"},
		{"settings", "codex", []string{"--model", "requested", "-c", "model=elsewhere"}, nil, "unknown_ambiguous_model_arguments"},
		{"prompt", "gemini", []string{"-p", "--model=prompt-secret"}, nil, "unknown_cli_default"},
		{"generic", "generic", []string{"--model", "looks-known"}, nil, "unknown_generic_command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := invocationForCommand(supervisorBackendCandidate{name: tc.kind}, exec.Command(tc.kind, tc.args...), "id", 1, "")
			if r.ModelEvidence != tc.evidence || (r.EffectiveCLIModel == nil) != (tc.want == nil) || (tc.want != nil && *tc.want != *r.EffectiveCLIModel) {
				t.Fatalf("receipt=%+v", r)
			}
		})
	}
}

func stringPointer(s string) *string { return &s }

func TestConsultationMeteredFallbackRequiresExplicitPolicy(t *testing.T) {
	cfg, count := receiptConfig(t)
	d := cfg.Model.Backends["secondary"]
	d.PricingClass = config.PricingClassMetered
	cfg.Model.Backends["secondary"] = d
	if _, err := NewBackendLLMClient(cfg).Complete("prompt"); err == nil {
		t.Fatal("metered fallback ran")
	}
	r := loadReceipt(t, cfg)
	calls, _ := os.ReadFile(count)
	if len(r.Invocations) != 1 || strings.Count(string(calls), "call") != 1 || r.Candidates[1].Status != "metered_policy_denied" {
		t.Fatalf("receipt=%+v calls=%s", r, calls)
	}
}

func TestConsultationOutcomeSyncFailureKeepsLaunchMarker(t *testing.T) {
	cfg, count := receiptConfig(t)
	c := NewBackendLLMClient(cfg).(*backendLLMClient)
	c.receiptSave = func(r *ConsultationReceipt) error {
		if err := (&consultationStore{dir: filepath.Join(cfg.StateDir, "supervisor-consultations")}).save(r); err != nil {
			return err
		}
		if len(r.Invocations) > 0 {
			return errors.New("sync outcome uncertain after rename")
		}
		return nil
	}
	_, err := c.Complete("prompt")
	assertHold(t, err, "receipt_persistence_failed")
	// current.json can already look completed; launch.json is authoritative
	// until the writer has proved the outcome durable.
	if len(loadReceipt(t, cfg).Invocations) != 1 {
		t.Fatal("fixture lacks renamed outcome")
	}
	_, err = NewBackendLLMClient(cfg).Complete("restart")
	assertHold(t, err, "unresolved_launch_intent")
	b, _ := os.ReadFile(count)
	if strings.Count(string(b), "call") != 1 {
		t.Fatal("blind retry after failed sync")
	}
}

func TestConsultationFinalSaveFailureCannotRetrySuccessfulCall(t *testing.T) {
	cfg, count := receiptConfig(t)
	d := cfg.Model.Backends["primary"]
	d.Cmd = strings.TrimSuffix(d.Cmd, " fail")
	cfg.Model.Backends["primary"] = d
	c := NewBackendLLMClient(cfg).(*backendLLMClient)
	c.receiptSave = func(r *ConsultationReceipt) error {
		if r.EndedAt != nil {
			return errors.New("final write failed")
		}
		return (&consultationStore{dir: filepath.Join(cfg.StateDir, "supervisor-consultations")}).save(r)
	}
	output, err := c.Complete("prompt")
	assertHold(t, err, "receipt_persistence_failed")
	if output != "" {
		t.Fatal("failed final write returned output")
	}
	_, err = NewBackendLLMClient(cfg).Complete("restart")
	assertHold(t, err, "unresolved_launch_intent")
	b, _ := os.ReadFile(count)
	if strings.Count(string(b), "call") != 1 {
		t.Fatal("blind retry after final save failure")
	}
}

func TestConsultationSerializesClientsAndRejectsIdentityReuse(t *testing.T) {
	cfg, _ := receiptConfig(t)
	_, unlock, err := openConsultationStore(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewBackendLLMClient(cfg).Complete("concurrent")
	assertHold(t, err, "consultation_in_progress")
	unlock()
	c := NewBackendLLMClient(cfg).(*backendLLMClient)
	id := newConsultationIdentity(cfg, "cycle-1")
	if _, err = c.CompleteConsultation(id, "first"); err != nil {
		t.Fatal(err)
	}
	_, err = c.CompleteConsultation(id, "replay")
	assertHold(t, err, "consultation_identity_reused")
	for _, name := range []string{"current.json", id.ID + ".json"} {
		st, err := os.Stat(filepath.Join(cfg.StateDir, "supervisor-consultations", name))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0600 {
			t.Fatalf("receipt mode=%v", st.Mode())
		}
	}
}
