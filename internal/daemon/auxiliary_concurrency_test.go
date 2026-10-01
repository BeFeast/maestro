package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/google/uuid"
)

type auxiliaryTestStore struct {
	fleetConcurrencyTestStore
	dirs map[string]bool
}

func TestAuxiliaryNativeOutcomeRecoveryRetainsUnknownPermitAndReleasesOnlyVerifiedSnapshot(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	store := &auxiliaryTestStore{fleetConcurrencyTestStore: fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MaxAuxiliaryRuns: 1}}}
	l := newFleetSpawnLimiter(store)
	l.RegisterStateDir(dir)
	id, nativeID := uuid.NewString(), uuid.NewString()
	if _, err := l.ReserveAuxiliary(dir, id); err != nil {
		t.Fatal(err)
	}
	if err := l.ReconcileAuxiliary(dir, id); err == nil {
		t.Fatal("missing proof released memory permit")
	}
	if _, err := l.ReserveAuxiliary(dir, uuid.NewString()); err == nil {
		t.Fatal("unknown role lost capacity")
	}
	now := time.Now().UTC()
	request := admissioncontrol.RegistrationRequest{Binding: admissioncontrol.Binding{GatewayScope: "gateway", NativeSessionID: nativeID, FleetID: "fleet", ProjectID: "project", RunID: "budget", Role: "supervisor", ExpiresAt: now.Unix() + 60}, ExpectedVersion: 1}
	seal := admissioncontrol.SealRequest{Binding: request.Binding, RegistrationVersion: 1}
	outcome := admissioncontrol.NativeOutcome{Binding: request.Binding, RegistrationVersion: 1, Sealed: true, Outcome: "settled", NextGenerationAllowed: true, PhysicalAttempts: 1, TerminalAttempts: 1, AttemptsDigest: strings.Repeat("a", 64)}
	body, _ := json.Marshal(outcome)
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
	digest := sha256.Sum256(bytes.TrimSuffix(canonical.Bytes(), []byte("\n")))
	outcome.SnapshotDigest = hex.EncodeToString(digest[:])
	outcome.EvidenceID = "native-outcome-v1:" + outcome.SnapshotDigest
	data := []byte("saved output")
	hash := sha256.Sum256(data)
	sha := hex.EncodeToString(hash[:])
	filename := nativeID + ".output.json"
	// This is the public on-disk checkpoint protocol, independent from process state.
	record := struct {
		Version         int    `json:"version"`
		RoleRunID       string `json:"role_run_id"`
		InvocationID    string `json:"invocation_id"`
		NativeSessionID string `json:"native_session_id"`
		Status          string `json:"local_status"`
		SHA256          string `json:"sha256"`
		Complete        bool   `json:"complete"`
		Truncated       bool   `json:"truncated"`
		Data            []byte `json:"data"`
	}{1, id, nativeID, nativeID, "succeeded", sha, true, false, data}
	body, _ = json.Marshal(record)
	root := filepath.Join(dir, "supervisor-consultations")
	_ = os.MkdirAll(root, 0700)
	_ = os.WriteFile(filepath.Join(root, filename), body, 0600)
	receipt := supervisor.ConsultationReceipt{SchemaVersion: 1, Identity: supervisor.ConsultationIdentity{ID: id, ProjectID: "project", CycleID: id, Role: "supervisor"}, StartedAt: now, EndedAt: &now, Status: "succeeded", InputDigest: strings.Repeat("b", 64), NativeOutcomeComplete: true,
		Invocations: []supervisor.InvocationReceipt{{ID: nativeID, Number: 1, StartedAt: now, EndedAt: now, Status: "succeeded", OutputCheckpoint: &supervisor.NativeOutputCheckpoint{Filename: filename, SHA256: sha, Bytes: len(data), Complete: true}, NativeSession: &supervisor.NativeSessionRegistrationReceipt{Request: request, Acknowledgement: &admissioncontrol.Acknowledgement{Binding: request.Binding, RegistrationVersion: 1}, AuthorityPin: strings.Repeat("c", 64), OutcomeIntent: &seal, Outcome: &outcome}}}}
	body, _ = json.Marshal(receipt)
	_ = os.WriteFile(filepath.Join(root, "current.json"), body, 0600)
	auxLaunch(t, dir, id)
	if err := l.ReconcileAuxiliary(dir, id); err == nil {
		t.Fatal("live marker released")
	}
	_ = os.Remove(filepath.Join(root, "launch.json"))
	_ = os.Chmod(filepath.Join(root, filename), 0644)
	if err := l.ReconcileAuxiliary(dir, id); err == nil {
		t.Fatal("untrusted checkpoint released")
	}
	_ = os.Chmod(filepath.Join(root, filename), 0600)
	if err := l.ReconcileAuxiliary(dir, id); err != nil {
		t.Fatal(err)
	}
	release, err := l.ReserveAuxiliary(dir, uuid.NewString())
	if err != nil {
		t.Fatal("settled role did not release capacity", err)
	}
	release()
	restarted := newFleetSpawnLimiter(store)
	restarted.RegisterStateDir(dir)
	release, err = restarted.ReserveAuxiliary(dir, uuid.NewString())
	if err != nil {
		t.Fatal("settled receipt consumed restart capacity", err)
	}
	release()
}

func (s *auxiliaryTestStore) RememberAuxiliaryStateDir(_ context.Context, dir string) error {
	if s.dirs == nil {
		s.dirs = map[string]bool{}
	}
	s.dirs[dir] = true
	return nil
}
func (s *auxiliaryTestStore) AuxiliaryStateDirs(context.Context) ([]string, error) {
	var dirs []string
	for dir := range s.dirs {
		dirs = append(dirs, dir)
	}
	return dirs, nil
}

func auxLaunch(t *testing.T, dir, id string) {
	t.Helper()
	root := filepath.Join(dir, "supervisor-consultations")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"identity": supervisor.ConsultationIdentity{ID: id, Role: "supervisor"}, "intent_id": uuid.NewString()})
	if err := os.WriteFile(filepath.Join(root, "launch.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestAuxiliaryCeilingContentionRestartAndWorkerFloor(t *testing.T) {
	store := &auxiliaryTestStore{fleetConcurrencyTestStore: fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 2, MaxLiveWorkers: 3, MaxAuxiliaryRuns: 2}}}
	dirs := []string{t.TempDir(), t.TempDir()}
	for _, dir := range dirs {
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	newLimiter := func() *fleetSpawnLimiter {
		l := newFleetSpawnLimiter(store)
		for _, dir := range dirs {
			saveFleetRunningState(t, dir, 1)
			l.RegisterStateDir(dir)
		}
		return l
	}
	l := newLimiter()
	var wg sync.WaitGroup
	winners := make(chan struct {
		dir, id string
		release func()
	}, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := uuid.NewString()
			dir := dirs[i%2]
			release, err := l.ReserveAuxiliary(dir, id)
			if err == nil {
				winners <- struct {
					dir, id string
					release func()
				}{dir, id, release}
			}
		}(i)
	}
	wg.Wait()
	close(winners)
	if len(winners) != 2 {
		t.Fatalf("aux permits=%d, want2", len(winners))
	}
	live, min, max, below, err := l.FloorStatus()
	if err != nil || live != 2 || min != 2 || max != 3 || below {
		t.Fatalf("aux changed worker floor: %d %d %d %t %v", live, min, max, below, err)
	}
	// Simulate one known terminal completion and one unresolved launch. The
	// durable marker replaces the in-memory permit after controller restart.
	n := 0
	for w := range winners {
		if n == 0 {
			auxLaunch(t, w.dir, w.id)
		}
		w.release()
		n++
	}
	restarted := newLimiter()
	release, err := restarted.ReserveAuxiliary(dirs[0], uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.ReserveAuxiliary(dirs[1], uuid.NewString()); err == nil {
		t.Fatal("unresolved durable auxiliary launch lost after restart")
	}
	release()
}
func TestAuxiliaryReleaseAndReceiptFailureHold(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store := &auxiliaryTestStore{fleetConcurrencyTestStore: fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MaxAuxiliaryRuns: 1}}}
	l := newFleetSpawnLimiter(store)
	l.RegisterStateDir(dir)
	release, err := l.ReserveAuxiliary(dir, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	release()
	release, err = l.ReserveAuxiliary(dir, uuid.NewString())
	if err != nil {
		t.Fatal("failed prelaunch did not release", err)
	}
	release()
	root := filepath.Join(dir, "supervisor-consultations")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "current.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReserveAuxiliary(dir, uuid.NewString()); err == nil {
		t.Fatal("unreadable occupancy admitted new run")
	}
}

func TestReserveAuxiliaryJournalsOccupancyWhenCeilingExhausted(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store := &auxiliaryTestStore{fleetConcurrencyTestStore: fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MaxAuxiliaryRuns: 1}}}
	l := newFleetSpawnLimiter(store)
	l.RegisterStateDir(dir)
	abandoned := uuid.NewString()
	auxLaunch(t, dir, abandoned)
	var journal bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&journal)
	t.Cleanup(func() { log.SetOutput(previous) })
	_, err := l.ReserveAuxiliary(dir, uuid.NewString())
	var hold *aiexecution.Hold
	if !errors.As(err, &hold) || hold.Code != "auxiliary_capacity_exhausted" {
		t.Fatalf("err=%v", err)
	}
	out := journal.String()
	for _, want := range []string{"auxiliary capacity exhausted", "(1/1 occupied)", "root=" + filepath.Clean(dir), "identity=" + abandoned, "source=launch_marker", "intent=", "age="} {
		if !strings.Contains(out, want) {
			t.Fatalf("journal lacks %q:\n%s", want, out)
		}
	}
}

func TestReconcileAbandonedAuxiliaryConsultationsCoversEveryRootWithReceipts(t *testing.T) {
	stateDir := t.TempDir()
	type call struct{ dir, role string }
	var calls []call
	oldReconcile := reconcileNativeConsultation
	t.Cleanup(func() { reconcileNativeConsultation = oldReconcile })
	reconcileNativeConsultation = func(cfg *config.Config, identity supervisor.ConsultationIdentity, prompt string) (supervisor.ConsultationResult, error) {
		if uuid.Validate(identity.ID) != nil || identity.ProjectID != "project" || prompt != "" {
			t.Fatalf("identity=%+v prompt=%q", identity, prompt)
		}
		calls = append(calls, call{cfg.StateDir, identity.Role})
		return supervisor.ConsultationResult{}, aiexecution.Held("native_prior_outcome_reconciled")
	}
	cfg := &config.Config{ProjectID: "project", StateDir: stateDir}
	cfg.Supervisor.NativeSessionRegistration = &config.NativeSessionRegistrationConfig{}
	// Paused projects and supervisor.enabled=false never reach Engine.decideWithLLM;
	// the startup reconcile must not depend on either.
	cfg.Supervisor.Enabled = false
	reconcileAbandonedAuxiliaryConsultations(cfg, "flow")
	if len(calls) != 0 {
		t.Fatal("roots without receipts were reconciled", calls)
	}
	if _, err := os.Lstat(filepath.Join(stateDir, "supervisor-consultations")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reconcile created a receipt directory for an unused root", err)
	}
	auxLaunch(t, stateDir, uuid.NewString())
	reviews := filepath.Join(stateDir, "native-reviews")
	if err := os.MkdirAll(filepath.Join(reviews, "supervisor-consultations"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reviews, "supervisor-consultations", "current.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	reconcileAbandonedAuxiliaryConsultations(cfg, "flow")
	want := []call{{stateDir, "supervisor"}, {reviews, "reviewer"}}
	if len(calls) != 2 || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("calls=%v want %v", calls, want)
	}
	cfg.Supervisor.NativeSessionRegistration = nil
	reconcileAbandonedAuxiliaryConsultations(cfg, "flow")
	if len(calls) != 2 {
		t.Fatal("non-native project reconciled native consultations")
	}
}
