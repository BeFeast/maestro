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
