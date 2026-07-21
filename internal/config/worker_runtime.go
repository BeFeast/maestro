package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	WorkerRuntimeModeLegacy   = "legacy"
	WorkerRuntimeModeIsolated = "isolated"

	WorkerRuntimeScopeSystem = "system"
	WorkerRuntimeScopeUser   = "user"
)

// WorkerRuntimeConfig controls the opt-in durable worker lease runtime.
// Legacy mode preserves the existing shared-tmux launch path as the rollback
// mechanism while isolated mode runs the worker payload in its own transient
// systemd service and private disk-backed scratch tree.
type WorkerRuntimeConfig struct {
	Mode        string `yaml:"mode,omitempty"`
	Scope       string `yaml:"scope,omitempty"`
	ScratchRoot string `yaml:"scratch_root,omitempty"`
	MemoryMaxMB int    `yaml:"memory_max_mb,omitempty"`
}

func (c WorkerRuntimeConfig) EffectiveMode() string {
	mode := strings.ToLower(strings.TrimSpace(c.Mode))
	if mode == "" {
		return WorkerRuntimeModeLegacy
	}
	return mode
}

func (c WorkerRuntimeConfig) IsolatedEnabled() bool {
	return c.EffectiveMode() == WorkerRuntimeModeIsolated
}

func (c WorkerRuntimeConfig) EffectiveScope() string {
	scope := strings.ToLower(strings.TrimSpace(c.Scope))
	if scope == "" {
		return WorkerRuntimeScopeSystem
	}
	return scope
}

// EffectiveScratchRoot returns the disk-backed base under which the worker
// package creates deterministic per-project reconciliation roots and unique
// per-attempt directories. /var/tmp is used deliberately instead of
// os.TempDir(): production /tmp is RAM-backed and is the resource this runtime
// is designed to keep worker build output out of.
func (c WorkerRuntimeConfig) EffectiveScratchRoot() string {
	if root := strings.TrimSpace(c.ScratchRoot); root != "" {
		return filepath.Clean(root)
	}
	return filepath.Join("/var/tmp", fmt.Sprintf("maestro-workers-%d", os.Getuid()))
}

func validateWorkerRuntime(c WorkerRuntimeConfig) error {
	switch c.EffectiveMode() {
	case WorkerRuntimeModeLegacy, WorkerRuntimeModeIsolated:
	default:
		return fmt.Errorf("config: worker_runtime.mode %q is invalid (want legacy or isolated)", c.Mode)
	}
	switch c.EffectiveScope() {
	case WorkerRuntimeScopeSystem, WorkerRuntimeScopeUser:
	default:
		return fmt.Errorf("config: worker_runtime.scope %q is invalid (want system or user)", c.Scope)
	}
	if c.MemoryMaxMB < 0 {
		return fmt.Errorf("config: worker_runtime.memory_max_mb must be >= 0")
	}
	if !c.IsolatedEnabled() {
		return nil
	}
	root := expandHome(c.EffectiveScratchRoot())
	if !filepath.IsAbs(root) {
		return fmt.Errorf("config: worker_runtime.scratch_root must be absolute in isolated mode")
	}
	clean := filepath.Clean(root)
	if clean == string(filepath.Separator) {
		return fmt.Errorf("config: worker_runtime.scratch_root must not be the filesystem root")
	}
	globalTmp := filepath.Clean("/tmp")
	if clean == globalTmp || strings.HasPrefix(clean, globalTmp+string(filepath.Separator)) {
		return fmt.Errorf("config: worker_runtime.scratch_root must be disk-backed and must not live under global %s", globalTmp)
	}
	return nil
}
