package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/tmuxsession"
)

type nativeFixture struct {
	cfg                           *config.Config
	issue                         github.Issue
	st                            *state.State
	slot                          string
	registered                    []admissioncontrol.RegistrationRequest
	spawned, stopped              int
	live, unobservable, lostReply bool
	statuses                      []string
}

func nativeTestFixture(t *testing.T) *nativeFixture {
	t.Helper()
	t.Setenv(workerCredentialsFileEnvVar, "")
	for _, key := range workerCredentialEnvKeys {
		t.Setenv(key, "")
	}
	root := t.TempDir()
	repo := newBranchTestRepo(t)
	runBranchGit(t, repo, "remote", "add", "origin", repo)
	runBranchGit(t, repo, "fetch", "origin")
	uid := uint32(os.Getuid())
	f := &nativeFixture{issue: github.Issue{Number: 1207, Title: "native worker fixture"}, slot: "fixture-1", st: state.NewState()}
	f.cfg = &config.Config{ProjectID: "fixture-project", Repo: "fixture/repo", LocalPath: repo, StateDir: filepath.Join(root, "state"), WorktreeBase: filepath.Join(root, "worktrees"), SessionPrefix: "fixture",
		Model:                           config.ModelConfig{Default: "claude", Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}}},
		WorkerNativeSessionRegistration: &config.NativeSessionRegistrationConfig{ControlSocket: filepath.Join(root, "authority.sock"), AuthorityUID: &uid, ExpectedPolicyVersion: 1, FleetID: "fixture-fleet", GatewayScope: "fixture-gateway", BudgetRunID: "fixture-budget", TTLSeconds: 3600},
		WorkerLaunchContext:             &config.WorkerLaunchContext{Role: "planner"}}
	if err := os.MkdirAll(f.cfg.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(f.cfg.WorktreeBase, f.slot)
	runBranchGit(t, repo, "worktree", "add", "-b", BranchName(f.slot, f.issue), worktree, "main")
	oldRegister, oldPersist := registerNativeWorker, persistNativeWorkerReceipt
	oldSpawn, oldRead, oldConfirm := runTmuxNewSession, readTmuxPaneIdentity, confirmWorkerProcessLease
	oldActive, oldAnchored, oldTerminate := workerProcessLeaseActive, workerProcessLeaseAnchored, terminateWorkerProcessLease
	oldExists, oldWorkerExists := tmuxSessionExists, workerTmuxSessionExists
	t.Cleanup(func() {
		registerNativeWorker = oldRegister
		persistNativeWorkerReceipt = oldPersist
		runTmuxNewSession = oldSpawn
		readTmuxPaneIdentity = oldRead
		confirmWorkerProcessLease = oldConfirm
		workerProcessLeaseActive = oldActive
		workerProcessLeaseAnchored = oldAnchored
		terminateWorkerProcessLease = oldTerminate
		tmuxSessionExists = oldExists
		workerTmuxSessionExists = oldWorkerExists
	})
	registerNativeWorker = func(_ admissioncontrol.Client, r admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
		saved, err := readNativeWorkerReceipt(nativeReceiptDir(f.cfg.StateDir, f.slot), uint64(len(f.registered)+1))
		if err != nil {
			t.Fatal(err)
		}
		if saved.Status != "registration_intent" || saved.Request != r {
			t.Fatalf("registration preceded durable intent: %+v", saved)
		}
		f.registered = append(f.registered, r)
		return admissioncontrol.Acknowledgement{Binding: r.Binding, RegistrationVersion: 1}, nil
	}
	persistNativeWorkerReceipt = func(dir string, r *NativeWorkerReceipt) error {
		f.statuses = append(f.statuses, r.Status)
		return writeNativeWorkerReceipt(dir, r)
	}
	tmuxSessionExists = func(string) bool { return f.live }
	workerTmuxSessionExists = func(string) bool { return f.live }
	runTmuxNewSession = func(_, wt, runner string, lease tmuxsession.ProcessLease) ([]byte, error) {
		f.spawned++
		f.live = true
		r, err := readNativeWorkerReceipt(nativeReceiptDir(f.cfg.StateDir, f.slot), uint64(f.spawned))
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != "launch_intent" || r.Acknowledgement == nil || r.ProcessLeaseUnit != lease.Unit {
			t.Fatalf("runner lacked durable authorization: %+v", r)
		}
		b, err := os.ReadFile(runner)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), r.Request.NativeSessionID) || !strings.Contains(string(b), "--session-id") {
			t.Fatalf("runner lacks native identity: %s", b)
		}
		if r.Worktree != wt {
			t.Fatal("wrong worktree")
		}
		if f.lostReply {
			return nil, errors.New("lost launch response")
		}
		return nil, nil
	}
	readTmuxPaneIdentity = func(string) (int, string, error) {
		if f.unobservable || !f.live {
			return 0, "", errors.New("unknown pane")
		}
		return 4242, worktree, nil
	}
	confirmWorkerProcessLease = func(tmuxsession.ProcessLease, int, time.Duration) (bool, error) { return f.live, nil }
	workerProcessLeaseActive = func(tmuxsession.ProcessLease) (bool, error) { return f.live, nil }
	workerProcessLeaseAnchored = func(_ tmuxsession.ProcessLease, pid int) (bool, error) { return f.live && pid == 4242, nil }
	terminateWorkerProcessLease = func(tmuxsession.ProcessLease) error { f.stopped++; f.live = false; return nil }
	return f
}
func (f *nativeFixture) start() (string, error) {
	return StartReserved(f.cfg, f.st, f.cfg.Repo, f.issue, "fixture task", "claude", f.slot)
}
func expectNativeHold(t *testing.T, err error, code string, uncertain bool) {
	t.Helper()
	h, ok := NativeHold(err)
	if !ok || h.Code != code || h.LaunchUncertain != uncertain {
		t.Fatalf("hold=%+v err=%v, want %s uncertain=%v", h, err, code, uncertain)
	}
}

func TestNativeWorkerAllEntrypointsRotateIdentityAndAncestry(t *testing.T) {
	f := nativeTestFixture(t)
	if _, err := f.start(); err != nil {
		t.Fatal(err)
	}
	sess := f.st.Sessions[f.slot]
	if sess.NativeRole != "planner" || sess.WorkerGeneration != 1 {
		t.Fatalf("fresh=%+v", sess)
	}
	f.cfg.WorkerLaunchContext = nil
	for i, step := range []struct {
		role  string
		phase state.Phase
		run   func() error
	}{
		{"advisor", state.PhaseAdvisor, func() error { return StartPhase(f.cfg, sess, f.slot, "advisor prompt", "claude") }},
		{"implementer", state.PhaseImplement, func() error { return RespawnInPlace(f.cfg, f.slot, sess, f.cfg.Repo, f.issue, "continue", "claude") }},
		{"validator", state.PhaseValidate, func() error { return Respawn(f.cfg, f.slot, sess, f.cfg.Repo, f.issue, "fresh", "claude") }},
	} {
		oldID, oldRun := sess.NativeSessionID, sess.NativeRoleRunID
		sess.Phase = step.phase
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.role, err)
		}
		if sess.NativeRole != step.role || sess.NativeSessionID == oldID || sess.NativeRoleRunID == oldRun || sess.NativeParentRoleRunID != oldRun || sess.WorkerGeneration != uint64(i+2) {
			t.Fatalf("%s identity=%+v", step.role, sess)
		}
	}
	if f.spawned != 4 || len(f.registered) != 4 || f.stopped != 3 {
		t.Fatalf("spawns/registers/stops=%d/%d/%d", f.spawned, len(f.registered), f.stopped)
	}
	slots, err := NativePendingSlots(f.cfg.StateDir, f.st.Sessions)
	if err != nil || len(slots) != 1 {
		t.Fatalf("occupancy slots=%v err=%v", slots, err)
	}
	if err := StopProcess(f.slot, sess); err != nil {
		t.Fatal(err)
	}
	slots, err = NativePendingSlots(f.cfg.StateDir, f.st.Sessions)
	if err != nil || len(slots) != 0 {
		t.Fatalf("terminated occupancy=%v err=%v", slots, err)
	}
}

func TestNativeWorkerLostLaunchResponseAdoptsExactlyOnce(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(map[bool]string{false: "observed", true: "unknown_after_dispatch"}[crash], func(t *testing.T) {
			f := nativeTestFixture(t)
			f.lostReply = true
			f.unobservable = crash
			_, err := f.start()
			if !crash {
				if err != nil {
					t.Fatal(err)
				}
				if f.spawned != 1 {
					t.Fatal("duplicate spawn")
				}
				return
			}
			expectNativeHold(t, err, "unresolved_launch", true)
			if f.stopped != 0 {
				t.Fatal("uncertain process was torn down")
			}
			r, err := readNativeWorkerReceipt(nativeReceiptDir(f.cfg.StateDir, f.slot), 1)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != "launch_intent" {
				t.Fatal(r.Status)
			}
			slots, err := NativePendingSlots(f.cfg.StateDir, state.NewState().Sessions)
			if err != nil || len(slots) != 1 {
				t.Fatalf("restart occupancy=%v %v", slots, err)
			}
			f.unobservable = false
			f.st, err = state.Load(f.cfg.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.start(); err != nil {
				t.Fatal(err)
			}
			if f.spawned != 1 || len(f.registered) != 1 || f.st.Sessions[f.slot].NativeSessionID != r.Request.NativeSessionID {
				t.Fatal("adoption regenerated or relaunched")
			}
		})
	}
}

func TestNativeWorkerUncertainGenerationAdoptionPreservesPreviousReceipt(t *testing.T) {
	f := nativeTestFixture(t)
	if _, err := f.start(); err != nil {
		t.Fatal(err)
	}
	sess := f.st.Sessions[f.slot]
	f.cfg.WorkerLaunchContext = nil
	sess.Phase = state.PhaseAdvisor
	oldRun := sess.NativeRoleRunID
	f.unobservable = true
	err := StartPhase(f.cfg, sess, f.slot, "review", "claude")
	expectNativeHold(t, err, "unresolved_launch", true)
	if sess.WorkerGeneration != 1 || sess.NativeRoleRunID != oldRun {
		t.Fatal("uncertain attempt consumed generation")
	}
	f.unobservable = false
	if err := StartPhase(f.cfg, sess, f.slot, "review", "claude"); err != nil {
		t.Fatal(err)
	}
	if sess.WorkerGeneration != 2 || sess.NativeParentRoleRunID != oldRun || f.spawned != 2 || len(f.registered) != 2 {
		t.Fatal("wrong crash adoption")
	}
}

func TestNativeWorkerHoldsBeforeAnySetupOrOldProcessStop(t *testing.T) {
	for _, entry := range []string{"fresh", "respawn", "in_place", "phase"} {
		t.Run(entry, func(t *testing.T) {
			f := nativeTestFixture(t)
			if entry != "fresh" {
				if _, err := f.start(); err != nil {
					t.Fatal(err)
				}
				f.cfg.WorkerLaunchContext = nil
			}
			registerNativeWorker = func(admissioncontrol.Client, admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
				return admissioncontrol.Acknowledgement{}, &admissioncontrol.Hold{Code: "policy_version_conflict"}
			}
			before := f.spawned
			var err error
			sess := f.st.Sessions[f.slot]
			switch entry {
			case "fresh":
				_, err = f.start()
			case "respawn":
				err = Respawn(f.cfg, f.slot, sess, f.cfg.Repo, f.issue, "prompt", "claude")
			case "in_place":
				err = RespawnInPlace(f.cfg, f.slot, sess, f.cfg.Repo, f.issue, "prompt", "claude")
			case "phase":
				sess.Phase = state.PhaseAdvisor
				err = StartPhase(f.cfg, sess, f.slot, "prompt", "claude")
			}
			expectNativeHold(t, err, "policy_version_conflict", false)
			if f.spawned != before || f.stopped != 0 {
				t.Fatal("held registration changed runtime")
			}
			if sess != nil && sess.WorkerGeneration != 1 {
				t.Fatal("held registration consumed generation")
			}
		})
	}
}

func TestNativeWorkerRejectsCarrierRoleAndSessionOverrides(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*nativeFixture)
		code   string
	}{
		{"missing_role", func(f *nativeFixture) { f.cfg.WorkerLaunchContext = nil }, "role_context_missing"},
		{"remote", func(f *nativeFixture) { f.cfg.RemoteRunner.Enabled = true }, "harness_unsupported"},
		{"codex", func(f *nativeFixture) {
			f.cfg.Model.Backends["claude"] = config.BackendDef{Cmd: "codex", Provider: "openai"}
		}, "harness_unsupported"},
		{"missing_backend", func(f *nativeFixture) { delete(f.cfg.Model.Backends, "claude") }, "backend_unknown"},
		{"existing_foreign_process", func(f *nativeFixture) { f.live = true }, "unregistered_process_exists"},
	}
	for _, arg := range []string{"--session-id=foreign", "--resume", "--continue", "--fork-session", "-r", "-c", "--no-session-persistence", "--"} {
		arg := arg
		cases = append(cases, struct {
			name   string
			mutate func(*nativeFixture)
			code   string
		}{arg, func(f *nativeFixture) {
			b := f.cfg.Model.Backends["claude"]
			b.ExtraArgs = []string{arg}
			f.cfg.Model.Backends["claude"] = b
		}, "argument_conflict"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := nativeTestFixture(t)
			tc.mutate(f)
			_, err := f.start()
			expectNativeHold(t, err, tc.code, tc.name == "existing_foreign_process")
			if f.spawned != 0 || len(f.registered) != 0 {
				t.Fatal("invalid context reached authority/process")
			}
		})
	}
}

func TestNativeWorkerLostRegistrationIsExplicitReconciliationOnly(t *testing.T) {
	f := nativeTestFixture(t)
	var saved admissioncontrol.RegistrationRequest
	calls := 0
	registerNativeWorker = func(_ admissioncontrol.Client, r admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
		calls++
		saved = r
		return admissioncontrol.Acknowledgement{}, errors.New("reply lost")
	}
	_, err := f.start()
	expectNativeHold(t, err, "authority_unavailable", false)
	_, err = f.start()
	expectNativeHold(t, err, "unresolved_registration", false)
	if calls != 1 {
		t.Fatal("automatic registration retry")
	}
	registerNativeWorker = func(_ admissioncontrol.Client, r admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
		calls++
		if r != saved {
			t.Fatal("reconciliation changed immutable request")
		}
		return admissioncontrol.Acknowledgement{Binding: r.Binding, RegistrationVersion: 1}, nil
	}
	receipt, err := ReconcileNativeWorkerRegistration(f.cfg, f.slot, 1)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "registration_reconciled_not_launched" || f.spawned != 0 {
		t.Fatal("reconciliation launched")
	}
	_, err = f.start()
	expectNativeHold(t, err, "unresolved_registration", false)
	if calls != 2 || f.spawned != 0 {
		t.Fatal("reconciled identity became launch authority")
	}
}

func TestNativeWorkerPersistenceFailureNeverDispatchesWithoutDurableIntent(t *testing.T) {
	for _, phase := range []string{"registration_intent", "registered", "launch_intent", "launched"} {
		t.Run(phase, func(t *testing.T) {
			f := nativeTestFixture(t)
			persistNativeWorkerReceipt = func(dir string, r *NativeWorkerReceipt) error {
				if r.Status == phase {
					return errors.New("fsync failed")
				}
				return writeNativeWorkerReceipt(dir, r)
			}
			_, err := f.start()
			expectNativeHold(t, err, "receipt_persistence_failed", phase == "launch_intent" || phase == "launched")
			want := 0
			if phase == "launched" {
				want = 1
			}
			if f.spawned != want || f.stopped != 0 {
				t.Fatalf("spawn/stop=%d/%d", f.spawned, f.stopped)
			}
		})
	}
}

func TestNativeWorkerExactAckRequired(t *testing.T) {
	for _, kind := range []string{"wrong_binding", "revoked", "zero_version", "future_version", "expired"} {
		t.Run(kind, func(t *testing.T) {
			f := nativeTestFixture(t)
			registerNativeWorker = func(_ admissioncontrol.Client, r admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
				ack := admissioncontrol.Acknowledgement{Binding: r.Binding, RegistrationVersion: 1}
				switch kind {
				case "wrong_binding":
					ack.Binding.Role = "repair"
				case "revoked":
					ack.Revoked = true
				case "zero_version":
					ack.RegistrationVersion = 0
				case "future_version":
					ack.RegistrationVersion = 2
				case "expired":
					ack.Binding.ExpiresAt = time.Now().Unix() - 1
				}
				return ack, nil
			}
			_, err := f.start()
			expectNativeHold(t, err, "acknowledgement_invalid", false)
			if f.spawned != 0 {
				t.Fatal("invalid ack launched")
			}
		})
	}
}

func TestNativeWorkerConcurrentOwnerCannotRebind(t *testing.T) {
	f := nativeTestFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	registerNativeWorker = func(_ admissioncontrol.Client, r admissioncontrol.RegistrationRequest) (admissioncontrol.Acknowledgement, error) {
		close(entered)
		<-release
		return admissioncontrol.Acknowledgement{Binding: r.Binding, RegistrationVersion: 1}, nil
	}
	result := make(chan error, 1)
	go func() {
		n, err := prepareNativeWorker(f.cfg, nil, f.slot, "claude", workerBackendConfig(f.cfg.Model.Backends["claude"]), 1, f.issue.Number, filepath.Join(f.cfg.WorktreeBase, f.slot), BranchName(f.slot, f.issue))
		if n != nil {
			n.close()
		}
		result <- err
	}()
	<-entered
	_, err := prepareNativeWorker(f.cfg, nil, f.slot, "claude", workerBackendConfig(f.cfg.Model.Backends["claude"]), 1, f.issue.Number, filepath.Join(f.cfg.WorktreeBase, f.slot), BranchName(f.slot, f.issue))
	expectNativeHold(t, err, "generation_in_progress", false)
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestNativeWorkerGenerationNotChangedByRuntimeProjectionAdoption(t *testing.T) {
	f := nativeTestFixture(t)
	if _, err := f.start(); err != nil {
		t.Fatal(err)
	}
	sess := f.st.Sessions[f.slot]
	id, run := sess.NativeSessionID, sess.NativeRoleRunID
	AdoptLiveRuntime(f.cfg, sess, 4242, TmuxSessionName(f.slot), time.Now())
	if sess.WorkerGeneration != 1 || sess.NativeSessionID != id || sess.NativeRoleRunID != run {
		t.Fatal("runtime observation minted generation")
	}
}
