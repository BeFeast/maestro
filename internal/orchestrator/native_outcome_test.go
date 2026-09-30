package orchestrator

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
	"github.com/befeast/maestro/internal/worker"
	"github.com/google/uuid"
)

func TestNativeWorkerLostSealReplyStaleRunningProjectionAndLateOutcomeReconcile(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"tmux": "#!/bin/sh\nexit 0\n", "systemctl": "#!/bin/sh\nprintf 'inactive\\n'\n", "claude": "#!/bin/sh\nexit 99\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	socketDir, err := os.MkdirTemp("", "orch-seal-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socketPath := filepath.Join(socketDir, "c")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	uid := uint32(os.Getuid())
	cfg := &config.Config{ProjectID: "fixture-project", StateDir: dir, Model: config.ModelConfig{Default: "claude", Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}}},
		WorkerNativeSessionRegistration: &config.NativeSessionRegistrationConfig{ControlSocket: socketPath, AuthorityUID: &uid, ExpectedPolicyVersion: 1, FleetID: "fixture-fleet", GatewayScope: "fixture-gateway", BudgetRunID: "fixture-budget", TTLSeconds: 100}}
	slot := "fixture-1"
	receiptDir := filepath.Join(dir, "worker-native-sessions", slot)
	if err := os.MkdirAll(receiptDir, 0700); err != nil {
		t.Fatal(err)
	}
	request := admissioncontrol.RegistrationRequest{Binding: admissioncontrol.Binding{GatewayScope: "fixture-gateway", NativeSessionID: uuid.NewString(), FleetID: "fixture-fleet", ProjectID: cfg.ProjectID, RunID: "fixture-budget", Role: "planner", ExpiresAt: time.Now().Unix() + 100}, ExpectedVersion: 1}
	lease, err := tmuxsession.WorkerProcessLease(cfg.ProjectID, slot, 1)
	if err != nil {
		t.Fatal(err)
	}
	lease.Manager = tmuxsession.ProcessLeaseManagerUser
	configBody, _ := json.Marshal(struct {
		Registration           *config.NativeSessionRegistrationConfig
		Project, Backend, Role string
		Config                 worker.BackendConfig
	}{cfg.WorkerNativeSessionRegistration, cfg.ProjectID, "claude", "planner", worker.BackendConfig{Cmd: "claude"}})
	configHash := sha256.Sum256(configBody)
	receipt := worker.NativeWorkerReceipt{SchemaVersion: 1, ProjectID: cfg.ProjectID, Slot: slot, Generation: 1, IssueNumber: 1207, RoleRunID: uuid.NewString(), Status: "launched", LogFile: filepath.Join(dir, "fixture.log"), Worktree: filepath.Join(dir, "worktree"), Branch: "fixture", Backend: "claude", ConfigDigest: hex.EncodeToString(configHash[:]), Request: request, Acknowledgement: &admissioncontrol.Acknowledgement{Binding: request.Binding, RegistrationVersion: 1}, ProcessLeaseUnit: lease.Unit, ProcessLeaseManager: lease.Manager, PID: 4242}
	body, _ := json.Marshal(receipt)
	receiptPath := filepath.Join(receiptDir, "generation-1.json")
	if err := os.WriteFile(receiptPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	sess := &state.Session{IssueNumber: 1207, Status: state.StatusRunning, WorkerGeneration: 1, NativeSessionID: request.NativeSessionID, NativeRoleRunID: receipt.RoleRunID, NativeRole: "planner", NativeReceiptDir: receiptDir, Worktree: receipt.Worktree, Branch: receipt.Branch, Backend: "claude", ProcessLeaseUnit: lease.Unit, ProcessLeaseManager: lease.Manager, PID: 4242, Phase: state.PhasePlan, RetryCount: 2}
	st := state.NewState()
	st.Sessions[slot] = sess
	if err := state.Save(dir, st); err != nil {
		t.Fatal(err)
	} // Stale pre-hold projection.
	var calls atomic.Int32
	serverDone := make(chan error, 1)
	go func() {
		for n := 1; n <= 3; n++ {
			conn, err := listener.Accept()
			if err != nil {
				serverDone <- err
				return
			}
			var prefix [4]byte
			if _, err = io.ReadFull(conn, prefix[:]); err != nil {
				conn.Close()
				serverDone <- err
				return
			}
			data := make([]byte, binary.BigEndian.Uint32(prefix[:]))
			if _, err = io.ReadFull(conn, data); err != nil {
				conn.Close()
				serverDone <- err
				return
			}
			var wire struct {
				Version int
				ID, Op  string
				Args    admissioncontrol.SealRequest
			}
			if json.Unmarshal(data, &wire) != nil || wire.Op != "seal_native" || wire.Args.Binding != request.Binding || wire.Args.RegistrationVersion != 1 {
				conn.Close()
				serverDone <- io.ErrUnexpectedEOF
				return
			}
			calls.Add(1)
			if n == 1 {
				conn.Close()
				continue
			} // Seal committed; reply lost.
			result := admissioncontrol.NativeOutcome{Binding: wire.Args.Binding, RegistrationVersion: 1, Sealed: true, Outcome: "no_dispatch", NextGenerationAllowed: true, AttemptsDigest: strings.Repeat("a", 64)}
			if n == 2 {
				code := "outcome_unknown"
				result.HoldCode = &code
				result.Outcome = "held"
				result.NextGenerationAllowed = false
				result.PhysicalAttempts = 1
				result.UnresolvedAttempts = 1
			}
			// Late ledger settlement preserves the physical attempt count.
			if n == 3 {
				result.Outcome = "settled"
				result.PhysicalAttempts = 1
				result.TerminalAttempts = 1
			}
			encoded, _ := json.Marshal(result)
			decoder := json.NewDecoder(bytes.NewReader(encoded))
			decoder.UseNumber()
			var value map[string]any
			_ = decoder.Decode(&value)
			delete(value, "snapshot_digest")
			delete(value, "evidence_id")
			var canonical bytes.Buffer
			encoder := json.NewEncoder(&canonical)
			encoder.SetEscapeHTML(false)
			_ = encoder.Encode(value)
			hash := sha256.Sum256(bytes.TrimSuffix(canonical.Bytes(), []byte("\n")))
			result.SnapshotDigest = hex.EncodeToString(hash[:])
			result.EvidenceID = "native-outcome-v1:" + result.SnapshotDigest
			response, _ := json.Marshal(map[string]any{"version": 1, "id": wire.ID, "ok": true, "result": result})
			binary.BigEndian.PutUint32(prefix[:], uint32(len(response)))
			_, err = conn.Write(append(prefix[:], response...))
			conn.Close()
			if err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()
	newController := func() *Orchestrator {
		return &Orchestrator{cfg: cfg, listOpenPRsFn: func() ([]github.PR, error) { return nil, nil }}
	}
	sess.NativeRegistrationHold = "previous_outcome_unknown"
	newController().reconcileRunningSessions(st)
	if calls.Load() != 1 || sess.NativeRegistrationHold == "" {
		t.Fatal("lost reply released hold")
	}
	// Crash drops all memory including hold/dead state, but not fsynced intent.
	restored, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	current := restored.Sessions[slot]
	if current.Status != state.StatusRunning || current.NativeRegistrationHold != "" {
		t.Fatal("not stale pre-hold fixture")
	}
	newController().reconcileRunningSessions(restored)
	if calls.Load() != 2 || current.NativeRegistrationHold != "native_generation_sealed" || current.Status != state.StatusDead {
		t.Fatalf("sealed generation was not recovered: %+v calls=%d", current, calls.Load())
	}
	if err := state.Save(dir, restored); err != nil {
		t.Fatal(err)
	}
	restored, err = state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	current = restored.Sessions[slot]
	newController().reconcileRunningSessions(restored)
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || current.NativeRegistrationHold != "" || current.WorkerGeneration != 1 || current.NativeSessionID != request.NativeSessionID || current.RetryCount != 2 {
		t.Fatal("recovery dispatched or consumed retry")
	}
	savedBody, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(savedBody, &receipt) != nil || receipt.Outcome == nil || !receipt.Outcome.NextGenerationAllowed {
		t.Fatal("released before durable proof")
	}
	if _, err := os.Stat(filepath.Join(receiptDir, "generation-2.json")); !os.IsNotExist(err) {
		t.Fatal("reconcile minted next generation")
	}
}
