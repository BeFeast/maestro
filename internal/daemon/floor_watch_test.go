package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/state"
)

func TestEvaluateFloorBreach_AlertsOncePerEpisodeAtPriorityFive(t *testing.T) {
	var mu sync.Mutex
	var priorities []string
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		priorities = append(priorities, r.Header.Get("Priority"))
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	saveFleetRunningState(t, dir, 1)
	store := &fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 5, MaxLiveWorkers: 10}}
	limiter := newFleetSpawnLimiter(store)
	limiter.RegisterStateDir(dir)

	d := &Daemon{
		spawnLimiter:  limiter,
		floorNotifier: (&notify.Notifier{}).WithNtfy(srv.URL, "fleet", ""),
	}
	var breach floorBreachState
	d.evaluateFloorBreach(&breach)
	if breach.streak != 1 {
		t.Fatalf("streak after first sample = %d, want 1", breach.streak)
	}
	d.evaluateFloorBreach(&breach)
	d.evaluateFloorBreach(&breach)
	mu.Lock()
	if len(priorities) != 1 || priorities[0] != "5" {
		mu.Unlock()
		t.Fatalf("first breach priorities = %v, want one priority 5 post", priorities)
	}
	firstBody := bodies[0]
	mu.Unlock()
	if !strings.Contains(firstBody, "live=1 min=5 max=10") {
		t.Fatalf("first alert body = %q", firstBody)
	}

	// Recovery resets the local debounce. A later identical floor condition is
	// a new episode and must bypass notify.Alert's prior body dedup.
	saveFleetRunningState(t, dir, 5)
	d.evaluateFloorBreach(&breach)
	if breach.streak != 0 || breach.notified {
		t.Fatalf("state after recovery = %+v, want reset", breach)
	}
	limiter.UnregisterStateDir(dir)
	rebreachDir := t.TempDir()
	saveFleetRunningState(t, rebreachDir, 1)
	limiter.RegisterStateDir(rebreachDir)
	d.evaluateFloorBreach(&breach)
	d.evaluateFloorBreach(&breach)
	mu.Lock()
	defer mu.Unlock()
	if len(priorities) != 2 || priorities[1] != "5" {
		t.Fatalf("posts after recovery and rebreach = %v, want two priority 5 posts", priorities)
	}
}

func TestFloorStatus_BelowMinExcludesPendingReservations(t *testing.T) {
	dir := t.TempDir()
	if err := state.Save(dir, state.NewState()); err != nil {
		t.Fatal(err)
	}
	store := &fleetConcurrencyTestStore{settings: config.FleetConcurrencySettings{MinLiveWorkers: 1, MaxLiveWorkers: 1}}
	limiter := newFleetSpawnLimiter(store)
	limiter.RegisterStateDir(dir)
	commit, release, ok := limiter.Reserve(dir)
	if !ok {
		t.Fatal("reservation failed")
	}
	defer release()
	commit("slot-pending")

	live, min, max, below, err := limiter.FloorStatus()
	if err != nil {
		t.Fatal(err)
	}
	if live != 0 || min != 1 || max != 1 || !below {
		t.Fatalf("FloorStatus = live=%d min=%d max=%d below=%v", live, min, max, below)
	}
	if !limiter.CeilingReached() {
		t.Fatal("pending reservation stopped protecting the hard ceiling")
	}
}

func TestFleetFloorNotifierForSkipsTelegramOnlyConfig(t *testing.T) {
	n := fleetFloorNotifierFor([]*config.Config{
		{Telegram: config.TelegramConfig{Target: "telegram-only"}},
		{Notify: config.NotifyConfig{Ntfy: config.NtfyConfig{BaseURL: "https://ntfy.example", Topic: "fleet"}}},
	})
	if n == nil || !n.NtfyConfigured() || n.NtfyTopic != "fleet" {
		t.Fatalf("floor notifier = %+v, want configured ntfy route", n)
	}
}
