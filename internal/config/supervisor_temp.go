package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultSupervisorTempDir returns the disk-backed directory supervisor backend
// children use for temporary files when supervisor.temp_dir is unset. /var/tmp
// is used deliberately instead of os.TempDir(): production /tmp is RAM-backed,
// and the Bun-built CLIs the supervisor probes extract a multi-megabyte native
// library into $TMPDIR on every invocation without ever deleting it (#1127).
// The uid suffix keeps two users on one host out of each other's directory.
func DefaultSupervisorTempDir() string {
	return filepath.Join("/var/tmp", fmt.Sprintf("maestro-supervisor-%d", os.Getuid()))
}

// EffectiveTempDir resolves the TMPDIR handed to supervisor backend children.
func (c SupervisorConfig) EffectiveTempDir() string {
	if dir := strings.TrimSpace(c.TempDir); dir != "" {
		return filepath.Clean(dir)
	}
	return DefaultSupervisorTempDir()
}

// validateSupervisorTempDir applies the same containment rule as
// worker_runtime.scratch_root: an operator-supplied path must be absolute and
// must not live under global /tmp, which is the RAM-backed filesystem this
// setting exists to keep backend children out of.
func validateSupervisorTempDir(c SupervisorConfig) error {
	if strings.TrimSpace(c.TempDir) == "" {
		return nil
	}
	dir := expandHome(c.EffectiveTempDir())
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("config: supervisor.temp_dir must be absolute")
	}
	clean := filepath.Clean(dir)
	if clean == string(filepath.Separator) {
		return fmt.Errorf("config: supervisor.temp_dir must not be the filesystem root")
	}
	globalTmp := filepath.Clean("/tmp")
	if clean == globalTmp || strings.HasPrefix(clean, globalTmp+string(filepath.Separator)) {
		return fmt.Errorf("config: supervisor.temp_dir must be disk-backed and must not live under global %s", globalTmp)
	}
	return nil
}
