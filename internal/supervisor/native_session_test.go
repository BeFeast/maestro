package supervisor

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/config"
)

type registrationFixture struct {
	mu       sync.Mutex
	requests []admissioncontrol.RegistrationRequest
	mode     string
	problems []string
	cfg      *config.Config
}

func nativeConfig(t *testing.T) (*config.Config, string, *registrationFixture) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux authenticated control RPC")
	}
	cfg, count := receiptConfig(t)
	old := strings.TrimSuffix(cfg.Model.Backends["primary"].Cmd, " fail")
	claude := filepath.Join(cfg.LocalPath, "claude")
	if err := os.Rename(old, claude); err != nil {
		t.Fatal(err)
	}
	for name, def := range cfg.Model.Backends {
		def.Cmd = strings.Replace(def.Cmd, old, claude, 1)
		cfg.Model.Backends[name] = def
	}
	dir, err := os.MkdirTemp("", "sr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "control.sock")
	uid := uint32(os.Geteuid())
	cfg.Supervisor.NativeSessionRegistration = &config.NativeSessionRegistrationConfig{ControlSocket: socket, AuthorityUID: &uid, ExpectedPolicyVersion: 3, FleetID: "test-fleet", GatewayScope: "maestro-test", BudgetRunID: "preprovisioned-pilot", TTLSeconds: 600}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &registrationFixture{cfg: cfg}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			fixture.serve(conn)
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done })
	return cfg, count, fixture
}

func (f *registrationFixture) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	var prefix [4]byte
	if _, err := io.ReadFull(conn, prefix[:]); err != nil {
		return
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if n > admissioncontrol.MaxFrame {
		return
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(conn, body); err != nil {
		return
	}
	var message struct {
		Version int                                  `json:"version"`
		ID      string                               `json:"id"`
		Op      string                               `json:"op"`
		Args    admissioncontrol.RegistrationRequest `json:"args"`
	}
	if json.Unmarshal(body, &message) != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, message.Args)
	current, err := os.ReadFile(filepath.Join(f.cfg.StateDir, "supervisor-consultations", "current.json"))
	var receipt ConsultationReceipt
	if err != nil || json.Unmarshal(current, &receipt) != nil || receipt.PlannedInvocation == nil || receipt.PlannedInvocation.NativeSession == nil || receipt.PlannedInvocation.NativeSession.Request != message.Args {
		f.problems = append(f.problems, "registration sent before durable receipt")
	}
	if _, err := os.Stat(filepath.Join(f.cfg.StateDir, "supervisor-consultations", "registration.json")); err != nil {
		f.problems = append(f.problems, "registration marker missing")
	}
	if _, err := os.Stat(filepath.Join(f.cfg.StateDir, "supervisor-consultations", "launch.json")); !os.IsNotExist(err) {
		f.problems = append(f.problems, "launch marker present at registration")
	}
	if f.mode == "lost" {
		return
	}
	response := map[string]any{"version": 1, "id": message.ID, "ok": true, "result": admissioncontrol.Acknowledgement{Binding: message.Args.Binding, RegistrationVersion: message.Args.ExpectedVersion}}
	if f.mode == "hold" {
		delete(response, "result")
		response["ok"] = false
		response["hold"] = map[string]string{"code": "scope_missing"}
	}
	if f.mode == "conflict" {
		ack := response["result"].(admissioncontrol.Acknowledgement)
		ack.Binding.Role = "worker"
		response["result"] = ack
	}
	data, _ := json.Marshal(response)
	binary.BigEndian.PutUint32(prefix[:], uint32(len(data)))
	_, _ = conn.Write(prefix[:])
	_, _ = conn.Write(data)
}

func nativeCalls(t *testing.T, count string) int {
	t.Helper()
	b, _ := os.ReadFile(count)
	return strings.Count(string(b), "call")
}

func TestNativeRegistrationBeforeLaunchAndFallback(t *testing.T) {
	cfg, count, fixture := nativeConfig(t)
	// The executable independently checks that the echoed ack is on disk before
	// accepting the exact owned argv. No model/provider participates in this test.
	claude := filepath.Join(cfg.LocalPath, "claude")
	script := `#!/bin/sh
cat >/dev/null
prior=""
sid=""
for arg do
  if [ "$prior" = "--session-id" ]; then sid="$arg"; fi
  prior="$arg"
done
test -n "$sid" || exit 91
grep -F '"acknowledgement": {' '` + filepath.Join(cfg.StateDir, "supervisor-consultations/current.json") + `' >/dev/null || exit 92
grep -F "$sid" '` + filepath.Join(cfg.StateDir, "supervisor-consultations/current.json") + `' >/dev/null || exit 93
printf 'call\n' >> '` + count + `'
test "$1" != fail || exit 2
printf done
`
	if err := os.WriteFile(claude, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	identity := newConsultationIdentity(cfg, "fixture-cycle")
	result, err := NewBackendLLMClient(cfg).(*backendLLMClient).CompleteConsultation(identity, "synthetic prompt")
	if err != nil || result.Output != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	r := loadReceipt(t, cfg)
	if nativeCalls(t, count) != 2 || len(r.Invocations) != 2 || r.Capability.Ready() {
		t.Fatalf("receipt=%+v", r)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.requests) != 2 || len(fixture.problems) != 0 {
		t.Fatalf("requests=%+v problems=%v", fixture.requests, fixture.problems)
	}
	if fixture.requests[0].NativeSessionID == fixture.requests[1].NativeSessionID {
		t.Fatal("fallback reused native identity")
	}
	for i, request := range fixture.requests {
		if request.NativeSessionID != r.Invocations[i].ID || request.RunID != cfg.Supervisor.NativeSessionRegistration.BudgetRunID || request.RunID == identity.ID || r.Invocations[i].NativeSession.Acknowledgement == nil {
			t.Fatalf("request=%+v invocation=%+v", request, r.Invocations[i])
		}
	}
}

func TestNativeRegistrationLostReplyHoldsRestartAndReconcilesWithoutLaunch(t *testing.T) {
	cfg, count, fixture := nativeConfig(t)
	fixture.mode = "lost"
	_, err := NewBackendLLMClient(cfg).Complete("synthetic")
	assertHold(t, err, "registration_authority_unavailable")
	if nativeCalls(t, count) != 0 {
		t.Fatal("lost reply launched")
	}
	_, err = NewBackendLLMClient(cfg).Complete("restart")
	assertHold(t, err, "unresolved_registration_intent")
	fixture.mu.Lock()
	fixture.mode = ""
	fixture.mu.Unlock()
	reconciled, err := ReconcileNativeSupervisorRegistration(cfg)
	if err != nil || reconciled.Status != "registration_reconciled_not_launched" || nativeCalls(t, count) != 0 {
		t.Fatalf("receipt=%+v err=%v", reconciled, err)
	}
	fixture.mu.Lock()
	if len(fixture.requests) != 2 || fixture.requests[0] != fixture.requests[1] || len(fixture.problems) != 0 {
		t.Fatalf("requests=%+v problems=%v", fixture.requests, fixture.problems)
	}
	fixture.mu.Unlock()
	if _, err := NewBackendLLMClient(cfg).Complete("later distinct consultation"); err != nil {
		t.Fatal(err)
	}
	if nativeCalls(t, count) != 2 {
		t.Fatal("later consultation did not launch normal fallback")
	}
}

func TestNativeRegistrationFailuresNeverLaunch(t *testing.T) {
	for _, mode := range []string{"hold", "conflict", "ack_persistence", "intent_persistence", "authority_uid", "missing_scope", "non_claude", "session_flag", "prefix_flag", "strict"} {
		t.Run(mode, func(t *testing.T) {
			cfg, count, fixture := nativeConfig(t)
			client := NewBackendLLMClient(cfg).(*backendLLMClient)
			want := "registration_scope_missing"
			switch mode {
			case "hold":
				fixture.mode = "hold"
			case "conflict":
				fixture.mode = "conflict"
				want = "registration_invalid_response"
			case "ack_persistence", "intent_persistence":
				want = "receipt_persistence_failed"
				client.receiptSave = func(r *ConsultationReceipt) error {
					if r.PlannedInvocation != nil && r.PlannedInvocation.NativeSession != nil && (mode == "intent_persistence" || r.PlannedInvocation.NativeSession.Acknowledgement != nil) {
						return errors.New("synthetic sync failure")
					}
					return (&consultationStore{dir: filepath.Join(cfg.StateDir, "supervisor-consultations")}).save(r)
				}
			case "authority_uid":
				*cfg.Supervisor.NativeSessionRegistration.AuthorityUID++
				want = "registration_authority_peer_mismatch"
			case "missing_scope":
				cfg.Supervisor.NativeSessionRegistration.BudgetRunID = ""
				want = "registration_configuration_invalid"
			case "non_claude":
				def := cfg.Model.Backends["primary"]
				def.Provider = "codex"
				cfg.Model.Backends["primary"] = def
				want = "native_session_harness_unsupported"
			case "session_flag":
				def := cfg.Model.Backends["primary"]
				def.ExtraArgs = []string{"--session-id=unowned"}
				cfg.Model.Backends["primary"] = def
				want = "native_session_argument_conflict"
			case "prefix_flag":
				def := cfg.Model.Backends["primary"]
				def.Cmd += " -r unowned"
				cfg.Model.Backends["primary"] = def
				want = "native_session_argument_conflict"
			case "strict":
				cfg.Supervisor.RequireAccountingReady = true
				want = "accounting_route_unsupported"
			}
			_, err := client.Complete("synthetic")
			assertHold(t, err, want)
			if nativeCalls(t, count) != 0 {
				t.Fatal("held registration launched")
			}
			if mode == "ack_persistence" {
				_, err = NewBackendLLMClient(cfg).Complete("restart")
				assertHold(t, err, "unresolved_registration_intent")
				if _, err = ReconcileNativeSupervisorRegistration(cfg); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestNativeRegistrationResumeFlagsAndRecoveryLaunchFence(t *testing.T) {
	for _, arg := range []string{"--session-id", "--session-id=x", "--resume", "--resume=x", "-r", "-rx", "--continue", "--continue=true", "-c", "-cp", "--fork-session", "--no-session-persistence", "--"} {
		if !rejectsSessionFlags([]string{arg}) {
			t.Errorf("accepted %q", arg)
		}
	}
	if rejectsSessionFlags([]string{"-p", "--model", "claude-sonnet-4-6"}) {
		t.Fatal("rejected normal args")
	}
	cfg, count, _ := nativeConfig(t)
	client := NewBackendLLMClient(cfg).(*backendLLMClient)
	client.receiptSave = func(r *ConsultationReceipt) error {
		if len(r.Invocations) > 0 {
			return errors.New("synthetic outcome failure")
		}
		return (&consultationStore{dir: filepath.Join(cfg.StateDir, "supervisor-consultations")}).save(r)
	}
	_, err := client.Complete("synthetic")
	assertHold(t, err, "receipt_persistence_failed")
	_, err = ReconcileNativeSupervisorRegistration(cfg)
	assertHold(t, err, "unresolved_launch_intent")
	_, err = NewBackendLLMClient(cfg).Complete("restart")
	assertHold(t, err, "unresolved_launch_intent")
	if nativeCalls(t, count) != 1 {
		t.Fatal("uncertain launched request repeated")
	}
}
