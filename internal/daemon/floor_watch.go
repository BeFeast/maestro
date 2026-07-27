package daemon

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/befeast/maestro/internal/config"
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

type floorBreachState struct {
	streak   int
	notified bool
	episode  uint64
}

// watchFloorBreachLoop emits one CRITICAL ntfy per sustained below-floor
// episode. Recovery resets the debounce and increments the next episode's
// identity so notify.Alert can deliver a later recurrence even at the same
// live/min/max values.
func (d *Daemon) watchFloorBreachLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultFloorWatchInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var breach floorBreachState
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.evaluateFloorBreach(&breach)
		}
	}
}

func (d *Daemon) evaluateFloorBreach(breach *floorBreachState) {
	if d == nil || d.spawnLimiter == nil || breach == nil {
		return
	}
	live, min, max, below, err := d.spawnLimiter.FloorStatus()
	if err != nil {
		log.Printf("[daemon] floor watch: %v", err)
		return
	}
	if !below {
		if breach.streak > 0 {
			log.Printf("[daemon] floor recovered: live=%d min=%d max=%d", live, min, max)
		}
		breach.streak = 0
		breach.notified = false
		return
	}
	if breach.streak == 0 {
		breach.episode++
	}
	breach.streak++
	if breach.streak < floorBreachConfirmSamples {
		log.Printf("[daemon] floor breach pending confirm: live=%d min=%d sample=%d/%d",
			live, min, breach.streak, floorBreachConfirmSamples)
		return
	}
	if breach.notified {
		return
	}
	body := fmt.Sprintf(
		"CRITICAL: fleet live workers below floor — live=%d min=%d max=%d for %d consecutive samples "+
			"(breach episode %d). Hands-off fill is failing; investigate spawn/supervisor freezes immediately.",
		live, min, max, breach.streak, breach.episode,
	)
	log.Printf("[daemon] %s", body)
	n := d.floorNotifier
	if n == nil {
		breach.notified = true
		return
	}
	if err := n.Alert(notify.AlertFloorBreach, "fleet:floor_breach", "maestro CRITICAL: live below min", body); err != nil {
		log.Printf("[daemon] floor breach ntfy failed: %v", err)
		return
	}
	breach.notified = true
}

// fleetFloorNotifierFor selects an ntfy-capable route directly. A Telegram-only
// project is skipped so a later project's configured ntfy topic cannot be
// shadowed by the emergency channel selection order.
func fleetFloorNotifierFor(cfgs []*config.Config) *notify.Notifier {
	for _, cfg := range cfgs {
		if cfg == nil || !cfg.Notify.Ntfy.Enabled() {
			continue
		}
		n := notify.NewWithToken(
			cfg.Telegram.Token(), cfg.Telegram.Target, cfg.Telegram.Mode, cfg.Telegram.OpenclawURL,
		)
		n.WithNtfy(cfg.Notify.Ntfy.BaseURL, cfg.Notify.Ntfy.Topic, cfg.Notify.Ntfy.Token())
		return n
	}
	return nil
}
