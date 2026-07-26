package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/tmpfshygiene"
)

func pressureDaemon(t *testing.T, ntfyURL string, sample func() tmpfshygiene.PressureSnapshot) *Daemon {
	t.Helper()
	d := &Daemon{tmpfsPressure: &tmpfsPressureRuntime{sample: sample}}
	if ntfyURL != "" {
		d.floorNotifier = (&notify.Notifier{}).WithNtfy(ntfyURL, "fleet", "")
	}
	return d
}

func lowSpaceSnapshot(availableBytes int64) tmpfshygiene.PressureSnapshot {
	const total = int64(16) << 30
	return tmpfshygiene.PressureSnapshot{
		Root:               "/tmp",
		Tmpfs:              true,
		TotalBytes:         total,
		AvailableBytes:     availableBytes,
		UsePct:             int((total - availableBytes) * 100 / total),
		PressureFloorBytes: tmpfshygiene.DefaultPressureFreeBytes,
		SpawnFloorBytes:    tmpfshygiene.DefaultSpawnFreeBytes,
		Pressure:           tmpfshygiene.BelowFloor(availableBytes, total, tmpfshygiene.DefaultPressureFreeBytes),
		SpawnHold:          tmpfshygiene.BelowFloor(availableBytes, total, tmpfshygiene.DefaultSpawnFreeBytes),
	}
}

// The whole point of #1128: the sweeper reclaims nothing on this host, so the
// alert must come from the standalone capacity sample and reach ntfy at the
// same CRITICAL priority as a floor breach.
func TestEvaluateTmpfsPressure_AlertsOncePerEpisodeAtPriorityFive(t *testing.T) {
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

	available := int64(6) << 30 // below the 8GiB page floor, above the 4GiB spawn floor
	d := pressureDaemon(t, srv.URL, func() tmpfshygiene.PressureSnapshot { return lowSpaceSnapshot(available) })

	d.evaluateTmpfsPressure()
	mu.Lock()
	pending := len(priorities)
	mu.Unlock()
	if pending != 0 {
		t.Fatalf("posts after one sample = %d, want the confirm debounce to hold", pending)
	}
	d.evaluateTmpfsPressure()
	d.evaluateTmpfsPressure()
	mu.Lock()
	if len(priorities) != 1 || priorities[0] != "5" {
		mu.Unlock()
		t.Fatalf("priorities = %v, want exactly one priority 5 post", priorities)
	}
	firstBody := bodies[0]
	mu.Unlock()
	if !strings.Contains(firstBody, "free=6.0GiB") || !strings.Contains(firstBody, "floor=8.0GiB") {
		t.Fatalf("alert body = %q, want the absolute free-byte budget", firstBody)
	}

	// Recovery resets the debounce; a later recurrence is a new episode and must
	// not be swallowed by notify.Alert's identical-body dedup.
	available = 12 << 30
	d.evaluateTmpfsPressure()
	if d.tmpfsPressure.streak != 0 || d.tmpfsPressure.notified {
		t.Fatalf("runtime after recovery = %+v, want the debounce reset", d.tmpfsPressure)
	}
	available = 6 << 30
	d.evaluateTmpfsPressure()
	d.evaluateTmpfsPressure()
	mu.Lock()
	defer mu.Unlock()
	if len(priorities) != 2 || priorities[1] != "5" {
		t.Fatalf("priorities after recurrence = %v, want a second priority 5 post", priorities)
	}
}

// A pressure episode that never crosses the spawn floor must not pause
// dispatch: paging and holding are independent budgets.
func TestTmpfsSpawnHold_HoldsOnlyBelowTheSpawnFloorAndSelfClears(t *testing.T) {
	available := int64(6) << 30
	d := pressureDaemon(t, "", func() tmpfshygiene.PressureSnapshot { return lowSpaceSnapshot(available) })

	d.evaluateTmpfsPressure()
	if hold, reason := d.tmpfsSpawnHold(); hold {
		t.Fatalf("hold above the spawn floor = %v (%s), want dispatch to continue", hold, reason)
	}

	available = 2 << 30
	d.evaluateTmpfsPressure()
	hold, reason := d.tmpfsSpawnHold()
	if !hold {
		t.Fatal("dispatch was not held below the spawn floor")
	}
	if !strings.Contains(reason, "free=2.0GiB") || !strings.Contains(reason, "resumes automatically") {
		t.Fatalf("hold reason = %q, want a visible, self-clearing reason", reason)
	}

	// Telemetry: every held dispatch is counted and surfaced on the snapshot, so
	// a throughput pause is distinguishable from a fleet that quietly stopped.
	if hold, _ := d.tmpfsSpawnHold(); !hold {
		t.Fatal("second dispatch was not held")
	}
	snapshot, ok := d.tmpfsPressureSnapshot()
	if !ok || snapshot.HeldSpawns != 2 || !snapshot.SpawnHold {
		t.Fatalf("snapshot = %+v ok=%v, want held_spawns=2", snapshot, ok)
	}

	// No operator action, no approval, no restart: the next sample clears it.
	available = 12 << 30
	d.evaluateTmpfsPressure()
	if hold, reason := d.tmpfsSpawnHold(); hold {
		t.Fatalf("hold after recovery = %v (%s), want it to clear itself", hold, reason)
	}
}

// A daemon that has not sampled yet, or whose sample failed, must let dispatch
// through — a measurement gap must never park the fleet.
func TestTmpfsSpawnHold_FailsOpen(t *testing.T) {
	d := pressureDaemon(t, "", func() tmpfshygiene.PressureSnapshot {
		return tmpfshygiene.PressureSnapshot{Root: "/tmp", Error: "statfs boom"}
	})
	if hold, _ := d.tmpfsSpawnHold(); hold {
		t.Fatal("dispatch was held before any sample was taken")
	}
	d.evaluateTmpfsPressure()
	if hold, _ := d.tmpfsSpawnHold(); hold {
		t.Fatal("dispatch was held on a failed sample")
	}
	if snapshot, ok := d.tmpfsPressureSnapshot(); !ok || snapshot.HeldSpawns != 0 {
		t.Fatalf("snapshot = %+v ok=%v, want no held spawns", snapshot, ok)
	}
}
