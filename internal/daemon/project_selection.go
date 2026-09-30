package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/server"
)

// ValidateProjectSelection rejects ambiguous selection before the command opens
// its store. Run repeats the check for embedded callers that bypass the CLI.
func ValidateProjectSelection(names []string) error {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if name == "" || strings.TrimSpace(name) != name {
			return fmt.Errorf("--project requires an exact non-empty store row name, got %q", name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate --project %q", name)
		}
		seen[name] = true
	}
	return nil
}

// selectedFingerprint filters before Load, so malformed unrelated rows cannot
// enter startup, config reload, or hot membership reconciliation. A missing
// selected row stays missing: an empty result never means 'run all'.
func (d *Daemon) selectedFingerprint(fp map[string]time.Time) map[string]time.Time {
	if len(d.opts.ProjectNames) == 0 {
		return fp
	}
	selected := make(map[string]time.Time, len(d.opts.ProjectNames))
	for _, name := range d.opts.ProjectNames {
		if stamp, ok := fp[name]; ok {
			selected[name] = stamp
		}
	}
	return selected
}

// selectedProjectWriter bounds dashboard mutations by the same row identities
// that own the daemon's flows. The underlying canonical store is unchanged.
type selectedProjectWriter struct {
	store server.FleetProjectStore
	names []string
}

func (s selectedProjectWriter) check(name string) error {
	if !slices.Contains(s.names, name) {
		return fmt.Errorf("project %q is outside this daemon's --project selection", name)
	}
	return nil
}

func (s selectedProjectWriter) UpsertProject(ctx context.Context, name, yaml string) error {
	if err := s.check(name); err != nil {
		return err
	}
	return s.store.UpsertProject(ctx, name, yaml)
}

func (s selectedProjectWriter) DeleteProject(ctx context.Context, name string) error {
	if err := s.check(name); err != nil {
		return err
	}
	return s.store.DeleteProject(ctx, name)
}
