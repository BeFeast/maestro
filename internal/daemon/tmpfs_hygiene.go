package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/befeast/maestro/internal/tmpfshygiene"
)

type tmpfsHygieneRuntime struct {
	mu      sync.RWMutex
	summary tmpfshygiene.Summary
	set     bool

	options      tmpfshygiene.Options
	sweep        func(context.Context) (tmpfshygiene.Summary, error)
	metricWriter io.Writer
}

func newTmpfsHygieneRuntime(d *Daemon) *tmpfsHygieneRuntime {
	runtime := &tmpfsHygieneRuntime{
		options: tmpfshygiene.Options{
			Root:     "/tmp",
			ProcRoot: "/proc",
			Mode:     tmpfshygiene.ModeApply,
		},
		metricWriter: os.Stdout,
	}
	runtime.sweep = d.sweepTmpfsHygiene
	return runtime
}

func (d *Daemon) tmpfsHygieneSummary() (tmpfshygiene.Summary, bool) {
	if d == nil || d.tmpfsHygiene == nil {
		return tmpfshygiene.Summary{}, false
	}
	d.tmpfsHygiene.mu.RLock()
	defer d.tmpfsHygiene.mu.RUnlock()
	return d.tmpfsHygiene.summary, d.tmpfsHygiene.set
}

func (d *Daemon) publishTmpfsHygiene(summary tmpfshygiene.Summary) {
	if d == nil || d.tmpfsHygiene == nil {
		return
	}
	d.tmpfsHygiene.mu.Lock()
	d.tmpfsHygiene.summary = summary
	d.tmpfsHygiene.set = true
	d.tmpfsHygiene.mu.Unlock()
}

func (d *Daemon) tmpfsHygieneLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultTmpfsHygieneInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.runTmpfsHygieneTick(ctx)
		}
	}
}

func (d *Daemon) runTmpfsHygieneTick(ctx context.Context) {
	if d == nil || d.tmpfsHygiene == nil || d.tmpfsHygiene.sweep == nil {
		return
	}
	summary, err := d.tmpfsHygiene.sweep(ctx)
	d.publishTmpfsHygiene(summary)
	if writer := d.tmpfsHygiene.metricWriter; writer != nil {
		encoder := json.NewEncoder(writer)
		encoder.SetEscapeHTML(false)
		if encodeErr := encoder.Encode(summary); encodeErr != nil {
			log.Printf("[tmpfs-hygiene] emit JSONL summary failed: %v", encodeErr)
		}
	}
	if err != nil {
		log.Printf("[tmpfs-hygiene] sweep refused or failed: %v", err)
	}
	if summary.SweepIneffective {
		// Loud on purpose: a reaper that reclaims nothing looks identical to a
		// clean /tmp in the metric, so it has to announce itself (#1125).
		log.Printf("[tmpfs-hygiene] SUSPICIOUS: protected every matched entry and reclaimed nothing (matched=%d protected=%d reclaimable_bytes=0 protect_hits=%v proc_scan_errors=%d proc_unresolved_processes=%d) — protection may have stopped discriminating",
			summary.MatchedEntries, summary.ProtectedEntries, summary.ProtectHits, summary.ProcScanErrors, summary.ProcUnresolvedProcesses)
	}
}

func (d *Daemon) sweepTmpfsHygiene(ctx context.Context) (tmpfshygiene.Summary, error) {
	opts := d.tmpfsHygiene.options
	opts.Mode = tmpfshygiene.ModeApply
	cfgs, err := d.store.LoadAll(ctx)
	if err != nil {
		now := time.Now
		if opts.Now != nil {
			now = opts.Now
		}
		summary := tmpfshygiene.Summary{
			Timestamp:   now().UTC(),
			Mode:        tmpfshygiene.ModeApply,
			Root:        firstNonBlankTmpfsRoot(opts.Root),
			Categories:  map[string]tmpfshygiene.CategoryStats{},
			ProtectHits: map[string]int{},
			Error:       "load configured protection paths: " + err.Error(),
		}
		return summary, err
	}
	protected := append([]string(nil), opts.ProtectedPaths...)
	seen := make(map[string]bool, len(protected)+len(cfgs)*2)
	for _, path := range protected {
		seen[path] = true
	}
	for _, cfg := range cfgs {
		if cfg == nil {
			continue
		}
		for _, path := range []string{cfg.LocalPath, cfg.WorktreeBase} {
			path = strings.TrimSpace(path)
			if path != "" && !seen[path] {
				protected = append(protected, path)
				seen[path] = true
			}
		}
	}
	opts.ProtectedPaths = protected
	return tmpfshygiene.Sweep(ctx, opts)
}

func firstNonBlankTmpfsRoot(root string) string {
	if strings.TrimSpace(root) == "" {
		return "/tmp"
	}
	return root
}
