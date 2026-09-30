package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/google/uuid"
)

type auxiliaryReceiptIndex interface {
	RememberAuxiliaryStateDir(context.Context, string) error
	AuxiliaryStateDirs(context.Context) ([]string, error)
}

// ReserveAuxiliary shares the controller mutex and settings owner with worker
// capacity. It neither consumes a worker slot nor contributes to its floor.
func (l *fleetSpawnLimiter) ReserveAuxiliary(stateDir, roleRunID string) (func(), error) {
	if l == nil || stateDir == "" || uuid.Validate(roleRunID) != nil {
		return nil, aiexecution.Held("auxiliary_controller_unavailable")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Native reviews use a separate receipt scope under the registered project.
	projectDir := filepath.Clean(stateDir)
	if filepath.Base(projectDir) == "native-reviews" {
		projectDir = filepath.Dir(projectDir)
	}
	if _, ok := l.stateDirs[projectDir]; !ok {
		return nil, aiexecution.Held("auxiliary_project_unregistered")
	}
	settings, err := l.settingsLocked()
	if err != nil {
		return nil, aiexecution.Held("auxiliary_settings_unavailable")
	}
	if settings.MaxAuxiliaryRuns <= 0 {
		return nil, aiexecution.Held("auxiliary_cap_required")
	}
	if l.auxiliaryStore == nil {
		return nil, aiexecution.Held("auxiliary_receipt_index_unavailable")
	}
	if err := l.auxiliaryStore.RememberAuxiliaryStateDir(context.Background(), projectDir); err != nil {
		return nil, aiexecution.Held("auxiliary_receipt_index_unavailable")
	}
	dirs, err := l.auxiliaryStore.AuxiliaryStateDirs(context.Background())
	if err != nil {
		return nil, aiexecution.Held("auxiliary_receipt_index_unavailable")
	}
	for _, dir := range dirs {
		l.auxiliaryStateDirs[dir] = struct{}{}
	}
	occupied := map[string]struct{}{}
	for dir := range l.auxiliaryStateDirs {
		if _, err := os.Stat(dir); err != nil {
			return nil, aiexecution.Held("auxiliary_receipt_root_unavailable")
		}
		pending, err := supervisor.PendingAuxiliaryRuns(dir)
		if err != nil {
			return nil, err
		}
		for _, id := range pending {
			occupied[id] = struct{}{}
		}
	}
	for id := range l.auxiliaryReservations {
		occupied[id] = struct{}{}
	}
	key := filepath.Clean(stateDir) + "\x00" + strings.TrimSpace(roleRunID)
	if _, ok := occupied[key]; ok {
		return nil, aiexecution.Held("auxiliary_identity_in_use")
	}
	if len(occupied) >= settings.MaxAuxiliaryRuns {
		return nil, aiexecution.Held("auxiliary_capacity_exhausted")
	}
	l.auxiliaryReservations[key] = struct{}{}
	return func() { l.mu.Lock(); delete(l.auxiliaryReservations, key); l.mu.Unlock() }, nil
}
