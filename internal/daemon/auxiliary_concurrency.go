package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/google/uuid"
)

// AuxiliaryReceiptIndex is the durable index of every project state dir that
// ever reserved auxiliary capacity (maestro.db auxiliary_receipt_roots). It
// outlives project removal so an abandoned launch keeps counting (#1232).
type AuxiliaryReceiptIndex interface {
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
	var durable []supervisor.AuxiliaryOccupant
	for dir := range l.auxiliaryStateDirs {
		if _, err := os.Stat(dir); err != nil {
			return nil, aiexecution.Held("auxiliary_receipt_root_unavailable")
		}
		occupants, err := supervisor.PendingAuxiliaryOccupancy(dir)
		if err != nil {
			return nil, err
		}
		for _, o := range occupants {
			occupied[o.Key()] = struct{}{}
			durable = append(durable, o)
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
		// #1232: name what holds the ceiling so an abandoned durable marker is
		// diagnosable from the journal instead of looking like a live run.
		log.Printf("[daemon] auxiliary capacity exhausted for %s (%d/%d occupied): %s", projectDir, len(occupied), settings.MaxAuxiliaryRuns, describeAuxiliaryOccupancy(durable, l.auxiliaryReservations, time.Now()))
		return nil, aiexecution.Held("auxiliary_capacity_exhausted")
	}
	l.auxiliaryReservations[key] = struct{}{}
	return func() { l.mu.Lock(); delete(l.auxiliaryReservations, key); l.mu.Unlock() }, nil
}

// ReconcileAuxiliary releases an existing in-memory reservation only after the
// durable role receipt proves every native invocation terminal and the launch
// marker has been removed. It does not reserve another run or trust caller flags.
func (l *fleetSpawnLimiter) ReconcileAuxiliary(stateDir, roleRunID string) error {
	if l == nil {
		return aiexecution.Held("auxiliary_controller_unavailable")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := supervisor.NativeAuxiliaryOutcomeComplete(stateDir, roleRunID); err != nil {
		return err
	}
	delete(l.auxiliaryReservations, filepath.Clean(stateDir)+"\x00"+roleRunID)
	return nil
}

func describeAuxiliaryOccupancy(durable []supervisor.AuxiliaryOccupant, reserved map[string]struct{}, now time.Time) string {
	parts := make([]string, 0, len(durable)+len(reserved))
	for _, o := range durable {
		age := "age=unknown"
		if !o.Since.IsZero() {
			age = "age=" + now.Sub(o.Since).Truncate(time.Second).String()
		}
		intent := ""
		if o.Intent != "" {
			intent = " intent=" + o.Intent
		}
		parts = append(parts, fmt.Sprintf("durable{root=%s identity=%s source=%s%s %s}", o.Root, o.Identity, o.Source, intent, age))
	}
	for key := range reserved {
		root, id, _ := strings.Cut(key, "\x00")
		parts = append(parts, fmt.Sprintf("reserved{root=%s identity=%s}", root, id))
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return "(no occupants recorded)"
	}
	return strings.Join(parts, "; ")
}
