package configwatch

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/befeast/maestro/internal/config"
)

// Watch polls the config file at path for modifications and sends a newly
// parsed *config.Config on the returned channel whenever the file changes.
// Invalid YAML is logged and skipped (last good config is retained by caller).
func Watch(ctx context.Context, path string, pollInterval time.Duration) <-chan *config.Config {
	ch := make(chan *config.Config, 1)
	go func() {
		defer close(ch)

		watchedPaths := watchedConfigPaths(path, nil)
		if cfg, err := config.LoadFrom(path); err == nil {
			watchedPaths = watchedConfigPaths(path, cfg)
		}
		lastModTimes := statModTimes(watchedPaths)

		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()

		log.Printf("[configwatch] watching %s (poll every %s)", path, pollInterval)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !hasConfigChange(watchedPaths, lastModTimes) {
					continue
				}
				// Debounce: wait briefly for writes to settle
				time.Sleep(500 * time.Millisecond)
				lastModTimes = statModTimes(watchedPaths)

				cfg, err := config.LoadFrom(path)
				if err != nil {
					log.Printf("[configwatch] reload failed (keeping previous config): %v", err)
					continue
				}
				watchedPaths = watchedConfigPaths(path, cfg)
				lastModTimes = statModTimes(watchedPaths)

				select {
				case ch <- cfg:
				default:
					// Drop if consumer hasn't drained yet
				}
			}
		}
	}()
	return ch
}

// ProjectStore is the subset of *configstore.Store that WatchStore polls: a
// single project's config and the fleet-wide fingerprint (name → updated_at)
// used to detect when that project changed. It is declared here — not imported
// from configstore — so configwatch stays free of a store dependency and the
// daemon can reuse the same interface for its diff-loop (#757).
type ProjectStore interface {
	Load(ctx context.Context, name string) (*config.Config, error)
	ProjectsFingerprint(ctx context.Context) (map[string]time.Time, error)
}

// WatchStore polls the config store for the project named name and sends a
// newly loaded *config.Config on the returned channel whenever that project's
// updated_at advances. It is the DB-backed sibling of Watch (#757): the daemon
// wires it into each flow's orchestrator via SetConfigReloadCh, so an operator
// editing a project row in the store hot-reloads the running flow without a
// restart.
//
// Semantics mirror Watch: a load/fingerprint error is logged and skipped (the
// caller keeps the last good config), and a project that has disappeared from
// the store emits nothing — the daemon's diff-loop owns add/remove, so WatchStore
// must not push a stale config or close on removal. The channel is buffered with
// depth 1 and drops a new config if the consumer has not drained the previous
// one, matching Watch. The goroutine exits and closes the channel on ctx.Done.
func WatchStore(ctx context.Context, store ProjectStore, name string, pollInterval time.Duration) <-chan *config.Config {
	ch := make(chan *config.Config, 1)
	go func() {
		defer close(ch)

		// Seed last with the project's current timestamp so an unchanged project
		// never triggers a spurious reload on the first tick. A project missing at
		// seed time leaves last zero; once it appears its timestamp is After(zero)
		// and the first real config is emitted.
		last, _ := storeProjectUpdatedAt(ctx, store, name)

		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()

		log.Printf("[configwatch] watching store project %q (poll every %s)", name, pollInterval)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ts, ok := storeProjectUpdatedAt(ctx, store, name)
				if !ok || !ts.After(last) {
					// Missing (the diff-loop will stop the flow), fingerprint
					// errored, or unchanged — nothing to reload.
					continue
				}
				cfg, err := store.Load(ctx, name)
				if err != nil {
					log.Printf("[configwatch] store reload of %q failed (keeping previous config): %v", name, err)
					continue
				}
				last = ts
				select {
				case ch <- cfg:
				default:
					// Drop if consumer hasn't drained yet
				}
			}
		}
	}()
	return ch
}

// storeProjectUpdatedAt returns project name's updated_at and whether it is
// present in the store. A fingerprint error is logged and reported as absent so
// the watcher treats it as "no change this tick" rather than reloading.
func storeProjectUpdatedAt(ctx context.Context, store ProjectStore, name string) (time.Time, bool) {
	fp, err := store.ProjectsFingerprint(ctx)
	if err != nil {
		log.Printf("[configwatch] fingerprint store for project %q failed: %v", name, err)
		return time.Time{}, false
	}
	ts, ok := fp[name]
	return ts, ok
}

func watchedConfigPaths(path string, cfg *config.Config) []string {
	paths := []string{path}
	paths = append(paths, config.SupervisorPolicyCandidatePaths(path, cfg)...)
	return uniquePaths(paths)
}

func statModTimes(paths []string) map[string]time.Time {
	modTimes := make(map[string]time.Time, len(paths))
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil {
			modTimes[filepath.Clean(path)] = info.ModTime()
		}
	}
	return modTimes
}

func hasConfigChange(paths []string, last map[string]time.Time) bool {
	for _, path := range paths {
		clean := filepath.Clean(path)
		info, err := os.Stat(path)
		if err != nil {
			if _, existed := last[clean]; existed {
				return true
			}
			continue
		}
		if prev, ok := last[clean]; !ok || info.ModTime().After(prev) {
			return true
		}
	}
	return false
}

func uniquePaths(paths []string) []string {
	unique := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(path)
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		unique = append(unique, path)
	}
	return unique
}
