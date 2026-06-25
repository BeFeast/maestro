package statestore

import (
	"context"
	"fmt"
	"strings"

	"github.com/befeast/maestro/internal/state"
)

// Mode selects whether the non-approval state is mirrored into the shared
// SQLite database. It mirrors approvalstore.Mode so the daemon plumbs both
// stores with one --state-store / --approvals-store flag pair.
type Mode string

const (
	// ModeJSON keeps per-project state.json as the only store (no SQLite
	// mirror). This is the safe default until the SQLite read path is flipped
	// on for the fleet.
	ModeJSON Mode = "json"
	// ModeSQLite write-through-mirrors every project's state into the shared
	// maestro.db (JSON remains the authoritative mint source during the
	// transition) so cross-project SQL queries are available.
	ModeSQLite Mode = "sqlite"
)

// ParseMode normalises the --state-store flag value. Empty defaults to json.
func ParseMode(s string) (Mode, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", string(ModeJSON):
		return ModeJSON, nil
	case string(ModeSQLite):
		return ModeSQLite, nil
	default:
		return "", fmt.Errorf("unknown state store %q (want json|sqlite)", s)
	}
}

// Binding is everything a write-through call-site needs to mirror one project's
// state into SQLite. StateDir is the JSON state directory (the mint source and
// the row scope); Handle is an optional already-open store shared by a
// long-lived process (the fleet server / daemon) so every in-process write
// serialises through its single pooled connection. When Handle is nil and
// ModeSQLite is selected the store is opened per call from DBPath.
type Binding struct {
	Mode     Mode
	DBPath   string
	StateDir string
	Repo     string
	Project  string
	Handle   *Store
}

// UseSQLite reports whether the SQLite mirror is selected.
func (b Binding) UseSQLite() bool { return b.Mode == ModeSQLite }

// RowBinding projects the per-project columns out of a Binding.
func (b Binding) RowBinding() RowBinding {
	return RowBinding{Project: b.Project, Repo: b.Repo, StateDir: b.StateDir}
}

// resolveStore returns the store to use plus a cleanup func. When Handle is set
// it is reused (cleanup is a no-op); otherwise a fresh store is opened from
// DBPath and the cleanup closes it.
func (b Binding) resolveStore() (*Store, func(), error) {
	if b.Handle != nil {
		return b.Handle, func() {}, nil
	}
	store, err := Open(b.DBPath)
	if err != nil {
		return nil, func() {}, err
	}
	return store, func() { store.Close() }, nil
}

// Mirror write-through-mirrors st into the SQLite store for one project. It is a
// no-op in ModeJSON, so a caller can unconditionally call it after state.Save
// during the transition and only pay the SQLite write when the mirror is
// enabled. The mirror is a full per-project snapshot replace (see
// Store.ImportState), so it is idempotent and converges even after a no-op Save.
func Mirror(b Binding, st *state.State) error {
	if !b.UseSQLite() {
		return nil
	}
	if strings.TrimSpace(b.StateDir) == "" {
		return nil
	}
	store, cleanup, err := b.resolveStore()
	if err != nil {
		return err
	}
	defer cleanup()
	return store.ImportState(context.Background(), b.RowBinding(), st)
}
