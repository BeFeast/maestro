package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/approvalstore"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/configstore"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/statestore"
	"github.com/befeast/maestro/internal/tmpfshygiene"
)

type selectionStore struct {
	*fakeWatchStore
	muCalls sync.Mutex
	loads   []string
	all     int
}

func (s *selectionStore) Load(ctx context.Context, name string) (*config.Config, error) {
	s.muCalls.Lock()
	s.loads = append(s.loads, name)
	s.muCalls.Unlock()
	if strings.HasPrefix(name, "poison") {
		return nil, fmt.Errorf("unselected row %s must never load", name)
	}
	return s.fakeWatchStore.Load(ctx, name)
}

func (s *selectionStore) LoadAll(ctx context.Context) ([]*config.Config, error) {
	s.muCalls.Lock()
	s.all++
	s.muCalls.Unlock()
	return s.fakeWatchStore.LoadAll(ctx)
}

func (s *selectionStore) calls() ([]string, int) {
	s.muCalls.Lock()
	defer s.muCalls.Unlock()
	return append([]string(nil), s.loads...), s.all
}

func TestProjectSelectionLoadsOnlyNamedRowsWithOrWithoutWatch(t *testing.T) {
	for _, watch := range []bool{false, true} {
		t.Run(fmt.Sprint(watch), func(t *testing.T) {
			store := &selectionStore{fakeWatchStore: newFakeWatchStore()}
			store.Set("alpha-row", testConfig(t, "owner/display-name"))
			store.Set("poison-unrelated", nil)
			names := []string{"alpha-row"}
			d := New(store, Options{ProjectNames: names, WatchStore: watch})
			names[0] = "poison-unrelated" // caller's slice is not a mutable scope.
			got, err := d.loadNamedConfigs(t.Context())
			if err != nil || len(got) != 1 || got[0].name != "alpha-row" {
				t.Fatalf("selected configs = %+v, err=%v", got, err)
			}
			loads, all := store.calls()
			if !reflect.DeepEqual(loads, []string{"alpha-row"}) || all != 0 {
				t.Fatalf("loads=%v LoadAll=%d", loads, all)
			}
			if !watch && d.projectStore != nil {
				t.Fatal("selection implicitly enabled store watching")
			}
		})
	}
}

func TestProjectSelectionInvalidFailsBeforeLoadAndSideEffects(t *testing.T) {
	for _, names := range [][]string{{""}, {" "}, {"alpha", "alpha"}, {"alpha", "missing"}, {"alpha "}} {
		t.Run(fmt.Sprintf("%q", names), func(t *testing.T) {
			store := &selectionStore{fakeWatchStore: newFakeWatchStore()}
			store.Set("alpha", testConfig(t, "owner/alpha"))
			db := filepath.Join(t.TempDir(), "must-not-open.db")
			d := New(store, Options{ProjectNames: names, WatchStore: true, StateStore: "sqlite", StateDBPath: db})
			if err := d.Run(t.Context()); err == nil {
				t.Fatal("invalid selection accepted")
			}
			loads, all := store.calls()
			if len(loads) != 0 || all != 0 || d.Fleet() != nil {
				t.Fatalf("side effects before rejection: loads=%v all=%d fleet=%v", loads, all, d.Fleet())
			}
			if _, err := os.Stat(db); !os.IsNotExist(err) {
				t.Fatalf("state DB was touched: %v", err)
			}
		})
	}
	if _, err := New(fakeLoader{}, Options{ProjectNames: []string{"alpha"}}).loadNamedConfigs(t.Context()); err == nil {
		t.Fatal("plain loader silently widened to LoadAll")
	}
	store := newFakeWatchStore()
	store.Set("alpha", nil)
	if _, err := New(store, Options{ProjectNames: []string{"alpha"}}).loadNamedConfigs(t.Context()); err == nil {
		t.Fatal("nil selected config accepted")
	}
}

func TestProjectSelectionOmittedPreservesWholeFleet(t *testing.T) {
	for _, watch := range []bool{false, true} {
		store := &selectionStore{fakeWatchStore: newFakeWatchStore()}
		store.Set("alpha", testConfig(t, "owner/alpha"))
		store.Set("beta", testConfig(t, "owner/beta"))
		got, err := New(store, Options{WatchStore: watch}).loadNamedConfigs(t.Context())
		if err != nil || len(got) != 2 {
			t.Fatalf("watch=%v configs=%+v err=%v", watch, got, err)
		}
		loads, all := store.calls()
		if (watch && (len(loads) != 2 || all != 0)) || (!watch && (len(loads) != 0 || all != 1)) {
			t.Fatalf("legacy loading changed: watch=%v loads=%v all=%d", watch, loads, all)
		}
	}
}

func isolatedSelectionDaemon(store ConfigLoader, opts Options) *Daemon {
	d := New(store, opts)
	d.runLoop = func(ctx context.Context, _ *config.Config, _ Options, _ <-chan *config.Config) { <-ctx.Done() }
	d.superviseLoop = func(ctx context.Context, _ string, _ func() *config.Config, _ Options, _ <-chan struct{}) {
		<-ctx.Done()
	}
	d.watchdogLoop = func(context.Context, string, string, time.Duration, chan<- struct{}) {}
	d.materialProgressLoop = func(context.Context, string, func() *config.Config) {}
	return d
}

func startSelectionDaemon(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	waitForFleet(t, d)
}

func TestProjectSelectionBoundsHotMembershipAndHostCleanup(t *testing.T) {
	store := &selectionStore{fakeWatchStore: newFakeWatchStore()}
	cfg := testConfig(t, "owner/alpha")
	store.Set("alpha-row", cfg)
	store.Set("poison-unrelated", nil)
	d := isolatedSelectionDaemon(store, Options{ProjectNames: []string{"alpha-row"}, WatchStore: true, WatchStoreInterval: time.Hour, TmpfsHygieneInterval: time.Millisecond})
	var sweeps atomic.Int64
	d.tmpfsHygiene.sweep = func(context.Context) (tmpfshygiene.Summary, error) { sweeps.Add(1); return tmpfshygiene.Summary{}, nil }
	startSelectionDaemon(t, d)
	waitForNames(t, d, "alpha")
	reconcile := func() {
		fp, err := store.ProjectsFingerprint(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		d.reconcileStore(t.Context(), fp, map[string]bool{})
	}
	store.Set("poison-new", nil)
	reconcile()
	waitForNames(t, d, "alpha")
	store.Delete("alpha-row")
	reconcile()
	waitForNames(t, d)
	// Empty selected membership cannot broaden to either poisonous row.
	reconcile()
	store.Set("alpha-row", cfg)
	reconcile()
	waitForNames(t, d, "alpha")
	loads, all := store.calls()
	if !reflect.DeepEqual(loads, []string{"alpha-row", "alpha-row"}) || all != 0 {
		t.Fatalf("scope escaped during hot reconciliation: loads=%v all=%d", loads, all)
	}
	if _, err := d.sweepTmpfsHygiene(t.Context()); err == nil {
		t.Fatal("host sweep permitted for a partial fleet")
	}
	if got := sweeps.Load(); got != 0 {
		t.Fatalf("host-wide sweeps=%d", got)
	}
	waitFor(t, func() bool { _, ok := d.tmpfsPressureSnapshot(); return ok })
}

func TestProjectSelectionPreservesUnrelatedSQLiteStateAndApprovals(t *testing.T) {
	state.SetSaveHook(nil)
	t.Cleanup(func() { state.SetSaveHook(nil) })
	ctx := t.Context()
	db := filepath.Join(t.TempDir(), "canonical.db")
	store, err := configstore.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	selected, unrelated := testConfig(t, "owner/selected"), testConfig(t, "owner/unrelated")
	for name, cfg := range map[string]*config.Config{"selected-row": selected, "unrelated-row": unrelated} {
		yaml := fmt.Sprintf("repo: %s\nlocal_path: %s\nstate_dir: %s\n", cfg.Repo, t.TempDir(), cfg.StateDir)
		if err := store.UpsertProject(ctx, name, yaml); err != nil {
			t.Fatal(err)
		}
	}
	st := state.NewState()
	st.Sessions["historic"] = &state.Session{IssueNumber: 77, Status: state.StatusDone}
	if err := state.Save(unrelated.StateDir, st); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(unrelated.StateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	states, err := statestore.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer states.Close()
	if err := states.ImportState(ctx, statestore.RowBinding{Project: unrelated.Repo, Repo: unrelated.Repo, StateDir: unrelated.StateDir}, st); err != nil {
		t.Fatal(err)
	}
	approvals, err := approvalstore.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer approvals.Close()
	now := time.Now().UTC()
	for _, status := range []state.ApprovalStatus{state.ApprovalStatusPending, state.ApprovalStatusApproved} {
		a := &state.Approval{ID: "historic-" + string(status), Action: config.SupervisorActionMergePR, Status: status, CreatedAt: now, UpdatedAt: now}
		if _, err := approvals.Put(ctx, a, approvalstore.RowBinding{Project: unrelated.Repo, Repo: unrelated.Repo, StateDir: unrelated.StateDir}); err != nil {
			t.Fatal(err)
		}
	}
	wantApprovals, err := approvals.List(ctx, unrelated.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	d := isolatedSelectionDaemon(store, Options{ProjectNames: []string{"selected-row"}, WatchStore: true, WatchStoreInterval: time.Hour, StateStore: "sqlite", StateDBPath: db, ApprovalsStore: "sqlite", ApprovalsDBPath: db})
	startSelectionDaemon(t, d)
	waitForNames(t, d, "selected")
	// Unselected project endpoints never reach its existing approved action.
	rec := httptest.NewRecorder()
	d.Fleet().HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/fleet/approvals/historic-approved/approve?project=unrelated", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unselected approval route = %d", rec.Code)
	}
	// The dashboard cannot delete an unrelated canonical row either.
	rec = httptest.NewRecorder()
	d.Fleet().HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/fleet/projects", strings.NewReader(`{"name":"unrelated-row"}`)))
	if rec.Code < 400 {
		t.Fatalf("unselected project mutation = %d", rec.Code)
	}
	if _, err := store.Load(ctx, "unrelated-row"); err != nil {
		t.Fatal(err)
	}
	// Removing the selected row leaves the partial fleet empty; all historical
	// state and approval IDs survive both startup and the removal reconciliation.
	if err := store.DeleteProject(ctx, "selected-row"); err != nil {
		t.Fatal(err)
	}
	fp, err := store.ProjectsFingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	d.reconcileStore(ctx, fp, map[string]bool{})
	waitForNames(t, d)
	gotSessions, err := states.Sessions(ctx, unrelated.StateDir)
	if err != nil || !reflect.DeepEqual(gotSessions, st.Sessions) {
		t.Fatalf("unrelated sessions=%v err=%v", gotSessions, err)
	}
	gotApprovals, err := approvals.List(ctx, unrelated.StateDir)
	if err != nil || !reflect.DeepEqual(gotApprovals, wantApprovals) {
		t.Fatalf("unrelated approvals changed: %v err=%v", gotApprovals, err)
	}
	after, err := os.ReadFile(filepath.Join(unrelated.StateDir, "state.json"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("unrelated state.json changed: %v", err)
	}
}
