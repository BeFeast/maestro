package daemon

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/tmpfshygiene"
)

// DefaultTmpfsPressureInterval is how often the daemon samples free bytes on
// the tmpfs root (#1128). It is deliberately decoupled from the 10m hygiene
// sweep: the sweeper refuses a non-tmpfs root outright and can reclaim nothing
// at all (#1125), yet the operator alert and the spawn precondition still need
// a current signal. One statfs at this cadence is negligible.
const DefaultTmpfsPressureInterval = 30 * time.Second

// tmpfsPressureConfirmSamples mirrors floorBreachConfirmSamples: ~1 minute of
// sustained pressure before paging, so a momentary spike from one worker that
// is about to exit does not wake the operator.
const tmpfsPressureConfirmSamples = 2

// tmpfsPressureRuntime owns the sweep-independent capacity sample plus the two
// consumers built on it: the CRITICAL ntfy and the spawn precondition.
type tmpfsPressureRuntime struct {
	mu       sync.RWMutex
	snapshot tmpfshygiene.PressureSnapshot
	set      bool
	// heldSpawns counts every dispatch the precondition paused. It is the
	// telemetry that separates a deliberate throughput pause from a fleet that
	// has quietly stopped working.
	heldSpawns uint64

	sample func() tmpfshygiene.PressureSnapshot

	// streak/notified/episode debounce the alert exactly like floorBreachState:
	// one page per pressure episode, reset on recovery, with the episode number
	// in the body so notify.Alert's identical-body dedup cannot swallow a
	// genuine recurrence. Only the watch goroutine touches them.
	streak   int
	notified bool
	episode  uint64
	// floorWarned keeps a floor-larger-than-the-mount misconfiguration to one
	// log line instead of one per tick.
	floorWarned bool
}

func newTmpfsPressureRuntime(root string, pressureFloorBytes, spawnFloorBytes int64) *tmpfsPressureRuntime {
	if pressureFloorBytes == 0 {
		pressureFloorBytes = tmpfshygiene.DefaultPressureFreeBytes
	}
	if spawnFloorBytes == 0 {
		spawnFloorBytes = tmpfshygiene.DefaultSpawnFreeBytes
	}
	opts := tmpfshygiene.SampleOptions{
		Root:               root,
		PressureFloorBytes: pressureFloorBytes,
		SpawnFloorBytes:    spawnFloorBytes,
	}
	return &tmpfsPressureRuntime{
		sample: func() tmpfshygiene.PressureSnapshot { return tmpfshygiene.Sample(opts) },
	}
}

// tmpfsPressureSnapshot is the Fleet-facing view. HeldSpawns is stamped on read
// so the API reports the live counter, not the value at sample time.
func (d *Daemon) tmpfsPressureSnapshot() (tmpfshygiene.PressureSnapshot, bool) {
	if d == nil || d.tmpfsPressure == nil {
		return tmpfshygiene.PressureSnapshot{}, false
	}
	r := d.tmpfsPressure
	r.mu.RLock()
	defer r.mu.RUnlock()
	snapshot := r.snapshot
	snapshot.HeldSpawns = r.heldSpawns
	return snapshot, r.set
}

func (d *Daemon) watchTmpfsPressureLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultTmpfsPressureInterval
	}
	// Sample immediately rather than after one interval: restarting into an
	// already-full /tmp must not leave the precondition failing open for the
	// whole first tick.
	d.evaluateTmpfsPressure()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.evaluateTmpfsPressure()
		}
	}
}

func (d *Daemon) evaluateTmpfsPressure() {
	if d == nil || d.tmpfsPressure == nil || d.tmpfsPressure.sample == nil {
		return
	}
	r := d.tmpfsPressure
	snapshot := r.sample()
	r.mu.Lock()
	r.snapshot = snapshot
	r.set = true
	r.mu.Unlock()

	if snapshot.Error != "" {
		// A broken measurement must not page anyone and must not pause anyone:
		// Sample already reported neither pressure nor hold, so just say so.
		log.Printf("[daemon] tmpfs pressure sample failed for %s: %s", snapshot.Root, snapshot.Error)
		return
	}
	if snapshot.TotalBytes > 0 && snapshot.PressureFloorBytes >= snapshot.TotalBytes && !r.floorWarned {
		r.floorWarned = true
		log.Printf("[daemon] tmpfs pressure floor %s is not smaller than %s (%s); the pressure alert stays off until it is lowered",
			formatTmpfsBytes(snapshot.PressureFloorBytes), snapshot.Root, formatTmpfsBytes(snapshot.TotalBytes))
	}
	if !snapshot.Pressure {
		if r.streak > 0 {
			log.Printf("[daemon] tmpfs pressure cleared: %s free=%s floor=%s",
				snapshot.Root, formatTmpfsBytes(snapshot.AvailableBytes), formatTmpfsBytes(snapshot.PressureFloorBytes))
		}
		r.streak = 0
		r.notified = false
		return
	}
	if r.streak == 0 {
		r.episode++
	}
	r.streak++
	if r.streak < tmpfsPressureConfirmSamples {
		log.Printf("[daemon] tmpfs pressure pending confirm: %s free=%s floor=%s sample=%d/%d",
			snapshot.Root, formatTmpfsBytes(snapshot.AvailableBytes), formatTmpfsBytes(snapshot.PressureFloorBytes),
			r.streak, tmpfsPressureConfirmSamples)
		return
	}
	if r.notified {
		return
	}
	body := fmt.Sprintf(
		"CRITICAL: %s free space below the absolute floor — free=%s floor=%s total=%s use=%d%% for %d consecutive samples "+
			"(pressure episode %d). This tmpfs is RAM-backed, so exhaustion is a host memory outage. "+
			"New worker dispatch pauses on its own below %s and resumes when space is reclaimed.",
		snapshot.Root, formatTmpfsBytes(snapshot.AvailableBytes), formatTmpfsBytes(snapshot.PressureFloorBytes),
		formatTmpfsBytes(snapshot.TotalBytes), snapshot.UsePct, r.streak, r.episode,
		formatTmpfsBytes(snapshot.SpawnFloorBytes),
	)
	log.Printf("[daemon] %s", body)
	n := d.floorNotifier
	if n == nil {
		r.notified = true
		return
	}
	if err := n.Alert(notify.AlertTmpfsPressure, "fleet:tmpfs_pressure", "maestro CRITICAL: tmpfs free space below floor", body); err != nil {
		log.Printf("[daemon] tmpfs pressure ntfy failed: %v", err)
		return
	}
	r.notified = true
}

// tmpfsSpawnHold is the host-resource precondition the orchestrator consults
// before claiming issues or spawning workers (#1128).
//
// It is a throughput pause, never a freeze. It persists no state, needs no
// approval, produces no ActionNone/human-gate decision, and spends none of the
// issue retry budget — the orchestrator simply returns for this cycle and
// re-evaluates on the next poll, so dispatch resumes by itself the moment bytes
// come back. It also fails OPEN: no sample yet, or a failed one, lets dispatch
// through, because a measurement gap must never park the fleet.
func (d *Daemon) tmpfsSpawnHold() (bool, string) {
	if d == nil || d.tmpfsPressure == nil {
		return false, ""
	}
	r := d.tmpfsPressure
	r.mu.RLock()
	snapshot := r.snapshot
	set := r.set
	r.mu.RUnlock()
	if !set || snapshot.Error != "" || !snapshot.SpawnHold {
		return false, ""
	}
	r.mu.Lock()
	r.heldSpawns++
	held := r.heldSpawns
	r.mu.Unlock()
	return true, fmt.Sprintf(
		"%s free=%s is below the spawn floor %s (use=%d%%, held_spawns=%d); dispatch resumes automatically once space is reclaimed",
		snapshot.Root, formatTmpfsBytes(snapshot.AvailableBytes), formatTmpfsBytes(snapshot.SpawnFloorBytes),
		snapshot.UsePct, held,
	)
}

// formatTmpfsBytes renders a byte budget the way the operator reasons about
// this mount — in binary units, since /tmp is sized in GiB.
func formatTmpfsBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}
	value := float64(bytes)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f%s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1fPiB", value/unit)
}
