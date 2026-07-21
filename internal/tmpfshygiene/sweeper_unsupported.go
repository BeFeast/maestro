//go:build !linux

package tmpfshygiene

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

var errUnsupportedPlatform = errors.New("tmpfs hygiene is only supported on Linux")

// Sweep fails closed on platforms without Linux tmpfs and *at syscall support.
func Sweep(_ context.Context, opts Options) (Summary, error) {
	if strings.TrimSpace(opts.Root) == "" {
		opts.Root = "/tmp"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	summary := Summary{
		Timestamp:   opts.Now().UTC(),
		Mode:        opts.Mode,
		Root:        filepath.Clean(opts.Root),
		Categories:  make(map[string]CategoryStats),
		ProtectHits: make(map[string]int),
		Error:       errUnsupportedPlatform.Error(),
	}
	return summary, errUnsupportedPlatform
}

// InspectLinuxMount is unavailable outside Linux.
func InspectLinuxMount(string) (MountUsage, error) {
	return MountUsage{}, errUnsupportedPlatform
}
