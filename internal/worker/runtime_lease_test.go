package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/workerlease"
)

type fakeWorkerLeaseOps struct {
	active      map[string]bool
	activeErr   map[string]error
	stopErr     map[string]error
	cleanupErr  map[string]error
	stopped     []string
	cleaned     []string
	listCalls   []string
	cleanupReal bool
}

func (f *fakeWorkerLeaseOps) List(root string) ([]workerlease.Lease, []workerlease.Attention, error) {
	f.listCalls = append(f.listCalls, root)
	return workerlease.List(root)
}

func (f *fakeWorkerLeaseOps) Active(_ string, unit string) (bool, error) {
	if err := f.activeErr[unit]; err != nil {
		return false, err
	}
	return f.active[unit], nil
}

func (f *fakeWorkerLeaseOps) Stop(_ string, unit string) error {
	f.stopped = append(f.stopped, unit)
	return f.stopErr[unit]
}

func (f *fakeWorkerLeaseOps) Cleanup(manifestPath, leaseID string) error {
	f.cleaned = append(f.cleaned, leaseID)
	if err := f.cleanupErr[leaseID]; err != nil {
		return err
	}
	if f.cleanupReal {
		return workerlease.CleanupManifest(manifestPath, leaseID)
	}
	return nil
}

func isolatedRuntimeConfig(t *testing.T, projectID string) *config.Config {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	scratchBase, err := os.MkdirTemp(cwd, ".worker-runtime-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratchBase) })
	return &config.Config{
		ProjectID: projectID,
		Repo:      "owner/repo",
		StateDir:  filepath.Join(t.TempDir(), "state"),
		WorkerRuntime: config.WorkerRuntimeConfig{
			Mode:        config.WorkerRuntimeModeIsolated,
			Scope:       config.WorkerRuntimeScopeSystem,
			ScratchRoot: filepath.Join(scratchBase, "scratch"),
		},
	}
}

func prepareRuntimeLease(t *testing.T, cfg *config.Config, slot string) workerlease.Lease {
	t.Helper()
	lease, err := workerlease.Prepare(workerlease.Spec{
		Root: workerLeaseProjectRoot(cfg), ProjectKey: workerLeaseProjectKey(cfg), Repo: cfg.Repo,
		Slot: slot, Attempt: "test", Scope: cfg.WorkerRuntime.EffectiveScope(),
		Now: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func sessionForRuntimeLease(lease workerlease.Lease, status state.SessionStatus) *state.Session {
	sess := &state.Session{Status: status, PID: 1234, StartedAt: lease.CreatedAt}
	assignWorkerLease(sess, &lease)
	return sess
}

func TestWorkerLeaseProjectRootSeparatesProjectsSharingScratchBase(t *testing.T) {
	a := isolatedRuntimeConfig(t, "project-a")
	b := *a
	b.ProjectID = "project-b"
	if workerLeaseProjectRoot(a) == workerLeaseProjectRoot(&b) {
		t.Fatal("different projects received the same reconciliation root")
	}
	if filepath.Dir(workerLeaseProjectRoot(a)) != filepath.Dir(workerLeaseProjectRoot(&b)) {
		t.Fatal("project roots should remain under the configured scratch base")
	}
}

func TestReconcileWorkerLeasesCleansOwnedOrphanAndKeepsNeighbor(t *testing.T) {
	cfg := isolatedRuntimeConfig(t, "project-a")
	orphan := prepareRuntimeLease(t, cfg, "slot-orphan")
	neighbor := prepareRuntimeLease(t, cfg, "slot-neighbor")
	st := state.NewState()
	st.Sessions[neighbor.Slot] = sessionForRuntimeLease(neighbor, state.StatusRunning)
	ops := &fakeWorkerLeaseOps{
		active: map[string]bool{orphan.Unit: true, neighbor.Unit: true}, cleanupReal: true,
		activeErr: map[string]error{}, stopErr: map[string]error{}, cleanupErr: map[string]error{},
	}

	result := reconcileWorkerLeasesWithOps(cfg, st, time.Now().UTC(), ops)
	if len(result.Cleaned) != 1 || result.Cleaned[0] != orphan.ID {
		t.Fatalf("cleaned = %v, want only orphan %s", result.Cleaned, orphan.ID)
	}
	if len(ops.stopped) != 1 || ops.stopped[0] != orphan.Unit {
		t.Fatalf("stopped = %v, want only orphan unit", ops.stopped)
	}
	if _, err := os.Stat(orphan.ScratchDir); !os.IsNotExist(err) {
		t.Fatalf("orphan scratch still exists: %v", err)
	}
	if _, err := os.Stat(neighbor.ManifestPath); err != nil {
		t.Fatalf("neighboring worker was touched: %v", err)
	}
	if st.Sessions[neighbor.Slot].WorkerLeaseID != neighbor.ID || st.Sessions[neighbor.Slot].WorkerLeaseAttention != "" {
		t.Fatalf("neighbor session changed unexpectedly: %+v", st.Sessions[neighbor.Slot])
	}
}

func TestReconcileWorkerLeasesCleansTerminalSessionExactly(t *testing.T) {
	cfg := isolatedRuntimeConfig(t, "project-a")
	lease := prepareRuntimeLease(t, cfg, "slot-done")
	st := state.NewState()
	st.Sessions[lease.Slot] = sessionForRuntimeLease(lease, state.StatusDone)
	ops := &fakeWorkerLeaseOps{
		active: map[string]bool{lease.Unit: true}, cleanupReal: true,
		activeErr: map[string]error{}, stopErr: map[string]error{}, cleanupErr: map[string]error{},
	}

	result := reconcileWorkerLeasesWithOps(cfg, st, time.Now().UTC(), ops)
	if len(result.Cleaned) != 1 || st.Sessions[lease.Slot].WorkerLeaseID != "" {
		t.Fatalf("terminal cleanup did not converge: result=%+v session=%+v", result, st.Sessions[lease.Slot])
	}
	if _, err := os.Stat(lease.ScratchDir); !os.IsNotExist(err) {
		t.Fatalf("terminal scratch still exists: %v", err)
	}
}

func TestReconcileWorkerLeasesMarksInactiveRunningLeaseEnded(t *testing.T) {
	cfg := isolatedRuntimeConfig(t, "project-a")
	lease := prepareRuntimeLease(t, cfg, "slot-ended")
	st := state.NewState()
	st.Sessions[lease.Slot] = sessionForRuntimeLease(lease, state.StatusRunning)
	st.Sessions[lease.Slot].TmuxSession = "maestro-slot-ended"
	ops := &fakeWorkerLeaseOps{
		active: map[string]bool{lease.Unit: false}, cleanupReal: true,
		activeErr: map[string]error{}, stopErr: map[string]error{}, cleanupErr: map[string]error{},
	}

	result := reconcileWorkerLeasesWithOps(cfg, st, time.Now().UTC(), ops)
	sess := st.Sessions[lease.Slot]
	if len(result.Cleaned) != 1 || sess.PID != 0 || sess.TmuxSession != "" || sess.WorkerEndedAt == nil {
		t.Fatalf("inactive lease did not converge to ended runtime: result=%+v session=%+v", result, sess)
	}
}

func TestReconcileWorkerLeasesLeavesAmbiguousSameSlotUntouched(t *testing.T) {
	cfg := isolatedRuntimeConfig(t, "project-a")
	claimed := prepareRuntimeLease(t, cfg, "slot-live")
	extra := prepareRuntimeLease(t, cfg, "slot-live")
	st := state.NewState()
	st.Sessions[claimed.Slot] = sessionForRuntimeLease(claimed, state.StatusRunning)
	ops := &fakeWorkerLeaseOps{
		active: map[string]bool{claimed.Unit: true, extra.Unit: true}, cleanupReal: true,
		activeErr: map[string]error{}, stopErr: map[string]error{}, cleanupErr: map[string]error{},
	}

	result := reconcileWorkerLeasesWithOps(cfg, st, time.Now().UTC(), ops)
	if len(result.Cleaned) != 0 || len(ops.stopped) != 0 {
		t.Fatalf("ambiguous slot was modified: result=%+v stopped=%v", result, ops.stopped)
	}
	if result.Attention == 0 || !strings.Contains(st.Sessions[claimed.Slot].WorkerLeaseAttention, "conflicts") {
		t.Fatalf("ambiguity was not surfaced: %+v", st.WorkerLeaseAttention)
	}
	if _, err := os.Stat(extra.ManifestPath); err != nil {
		t.Fatalf("ambiguous extra lease was deleted: %v", err)
	}
}

func TestReconcileWorkerLeasesPersistsInvalidOwnershipWithoutDeleting(t *testing.T) {
	cfg := isolatedRuntimeConfig(t, "project-a")
	root := workerLeaseProjectRoot(cfg)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ambiguous := filepath.Join(root, "unknown-build-tree")
	if err := os.Mkdir(ambiguous, 0o700); err != nil {
		t.Fatal(err)
	}
	st := state.NewState()
	ops := &fakeWorkerLeaseOps{
		active: map[string]bool{}, activeErr: map[string]error{}, stopErr: map[string]error{}, cleanupErr: map[string]error{},
	}

	result := reconcileWorkerLeasesWithOps(cfg, st, time.Now().UTC(), ops)
	if result.Attention != 1 || !strings.HasPrefix(st.WorkerLeaseAttention[0].Identity, "ambiguous-") {
		t.Fatalf("attention = %+v", st.WorkerLeaseAttention)
	}
	if strings.Contains(st.WorkerLeaseAttention[0].Identity, "unknown-build-tree") {
		t.Fatal("raw ambiguous entry leaked into durable attention identity")
	}
	if _, err := os.Stat(ambiguous); err != nil {
		t.Fatalf("ambiguous entry was deleted: %v", err)
	}
}

func TestReconcileWorkerLeasesFailsClosedWhenExactStopFails(t *testing.T) {
	cfg := isolatedRuntimeConfig(t, "project-a")
	lease := prepareRuntimeLease(t, cfg, "slot-orphan")
	st := state.NewState()
	ops := &fakeWorkerLeaseOps{
		active: map[string]bool{lease.Unit: true}, cleanupReal: true,
		activeErr: map[string]error{}, stopErr: map[string]error{lease.Unit: errors.New("denied")}, cleanupErr: map[string]error{},
	}

	result := reconcileWorkerLeasesWithOps(cfg, st, time.Now().UTC(), ops)
	if len(result.Cleaned) != 0 || result.Attention != 1 {
		t.Fatalf("result = %+v, want attention without cleanup", result)
	}
	if len(ops.cleaned) != 0 {
		t.Fatalf("scratch was removed before exact lease stop: %v", ops.cleaned)
	}
	if _, err := os.Stat(lease.ManifestPath); err != nil {
		t.Fatalf("owned scratch disappeared after failed stop: %v", err)
	}
}
