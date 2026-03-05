package configwatch

import (
	"context"
	"log"
	"os"
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

		var lastModTime time.Time
		if info, err := os.Stat(path); err == nil {
			lastModTime = info.ModTime()
		}

		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()

		log.Printf("[configwatch] watching %s (poll every %s)", path, pollInterval)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				info, err := os.Stat(path)
				if err != nil {
					continue
				}
				if !info.ModTime().After(lastModTime) {
					continue
				}
				// Debounce: wait briefly for writes to settle
				time.Sleep(500 * time.Millisecond)
				if info2, err := os.Stat(path); err == nil {
					lastModTime = info2.ModTime()
				} else {
					lastModTime = info.ModTime()
				}

				cfg, err := config.LoadFrom(path)
				if err != nil {
					log.Printf("[configwatch] reload failed (keeping previous config): %v", err)
					continue
				}

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
