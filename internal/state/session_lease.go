package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
)

// WithSessionLease serializes external side effects for one canonical worker
// slot across processes. Respawn and destructive worktree cleanup must both
// hold this lease so cleanup cannot validate a stale snapshot and then race a
// replacement worker into (or out of) the same deterministic slot.
//
// A blank stateDir is accepted for small unit-test configurations that do not
// persist state; production configurations always provide one.
func WithSessionLease(stateDir, slot string, fn func() error) error {
	if fn == nil {
		return fmt.Errorf("session lease: nil callback")
	}
	if err := ValidateSlotID(slot); err != nil {
		return fmt.Errorf("session lease: %w", err)
	}
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return fn()
	}

	leaseDir := filepath.Join(stateDir, ".session-leases")
	if err := os.MkdirAll(leaseDir, 0o755); err != nil {
		return fmt.Errorf("create session lease dir: %w", err)
	}
	lease := flock.New(filepath.Join(leaseDir, slot+".lock"))
	if err := lease.Lock(); err != nil {
		return fmt.Errorf("lock session lease for %s: %w", slot, err)
	}
	defer func() { _ = lease.Unlock() }()
	return fn()
}
