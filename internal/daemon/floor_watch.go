package daemon

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/befeast/maestro/internal/notify"
)

// DefaultFloorWatchInterval is how often the daemon re-checks live workers
// against fleet.min_live_workers (#1106).
const DefaultFloorWatchInterval = 30 * time.Second

// floorBreachConfirmSamples is how many consecutive below-floor samples must
// fire before CRITICAL alert. At 30s interval this is ~1 minute — long enough
// to ignore a brief drain/restart, short enough that operators are not waiting
// on a status poll.
const floorBreachConfirmSamples = 2

// watchFloorBreachLoop emits CRITICAL ntfy when fleet live workers stay below
// fleet.min_live_workers. Dedup is owned by notify.Alert (body-stable).
func (d *Daemon) watchFloorBreachLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultFloorWatchInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	belowStreak := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.evaluateFloorBreach(&belowStreak)
		}
	}
}

func (d *Daemon) evaluateFloorBreach(belowStreak *int) {
	if d == nil || d.spawnLimiter == nil || belowStreak == nil {
		return
	}
	live, min, max, below, err := d.spawnLimiter.FloorStatus()
	if err != nil {
		log.Printf("[daemon] floor watch: %v", err)
		return
	}
	if !below {
		if *belowStreak > 0 {
			log.Printf("[daemon] floor recovered: live=%d min=%d max=%d", live, min, max)
		}
		*belowStreak = 0
		return
	}
	*belowStreak++
	if *belowStreak < floorBreachConfirmSamples {
		log.Printf("[daemon] floor breach pending confirm: live=%d min=%d sample=%d/%d",
			live, min, *belowStreak, floorBreachConfirmSamples)
		return
	}
	body := fmt.Sprintf(
		"CRITICAL: fleet live workers below floor — live=%d min=%d max=%d. Hands-off fill is failing; investigate spawn/supervisor freezes immediately.",
		live, min, max,
	)
	log.Printf("[daemon] %s", body)
	n := d.emergencyNotifier
	if n == nil {
		return
	}
	if err := n.Alert(notify.AlertFloorBreach, "fleet:floor_breach", "maestro CRITICAL: live below min", body); err != nil {
		log.Printf("[daemon] floor breach ntfy failed: %v", err)
	}
}
