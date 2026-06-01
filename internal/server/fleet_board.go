package server

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/befeast/maestro/internal/github"
)

// fleetBoardRefreshInterval is how often the fleet API re-counts a project's
// GitHub Project board columns. The widget is operator-glance, not
// authoritative, so a 2-minute cadence is well within both the GraphQL budget
// and the freshness an operator notices.
const fleetBoardRefreshInterval = 2 * time.Minute

// fleetBoardDiscoverBackoff is how long to wait before re-attempting board
// discovery after a failure. Matches the orchestrator's discover backoff to
// avoid noisy reconnect storms on transient gh errors.
const fleetBoardDiscoverBackoff = 10 * time.Minute

// projectBoardClient is the narrow surface the fleet board refresher needs.
// *github.Client satisfies it; tests inject fakes via SetBoardClientForTest.
type projectBoardClient interface {
	DiscoverProject(projectNumber int) (*github.ProjectField, error)
	ListProjectItemStatusCounts(pf *github.ProjectField) ([]github.ProjectBoardColumn, int, error)
}

// fleetProjectBoard is the operator-facing snapshot of a project's GitHub
// Project board exposed via /api/v1/fleet. The fleet refresher updates it on
// a fixed cadence; the snapshot handler reads whatever is cached without ever
// blocking on a network call.
type fleetProjectBoard struct {
	Number     int                         `json:"number"`
	URL        string                      `json:"url,omitempty"`
	Owner      string                      `json:"owner,omitempty"`
	OwnerType  string                      `json:"owner_type,omitempty"`
	Columns    []github.ProjectBoardColumn `json:"columns,omitempty"`
	TotalItems int                         `json:"total_items"`
	FetchedAt  string                      `json:"fetched_at,omitempty"`
	Error      string                      `json:"error,omitempty"`
}

// boardState is the mutable per-project cache the refresher writes and the
// snapshot reads. Held by FleetProject behind a pointer so the value type
// stays cheap to copy through the project list.
type boardState struct {
	mu             sync.RWMutex
	client         projectBoardClient
	projectNumber  int
	field          *github.ProjectField
	snapshot       *fleetProjectBoard
	lastDiscoverAt time.Time
	discoverRetry  time.Time
}

// SetBoardClient wires the GitHub client and project number used to refresh
// this project's board snapshot. Calling with a nil client or non-positive
// number disables board surfacing for the project. Safe to call before
// FleetServer.Start; the refresher loop picks up whichever projects have a
// client configured.
func (p *FleetProject) SetBoardClient(client projectBoardClient, projectNumber int) {
	if p == nil {
		return
	}
	if client == nil || projectNumber <= 0 {
		p.board = nil
		return
	}
	p.board = &boardState{client: client, projectNumber: projectNumber}
}

// HasBoardClient reports whether the project has a configured board client.
// Used by FleetServer.Start to decide whether to spawn a refresher goroutine.
func (p *FleetProject) HasBoardClient() bool {
	return p != nil && p.board != nil && p.board.client != nil && p.board.projectNumber > 0
}

// snapshotBoard returns the latest cached board snapshot, or nil if the
// refresher has not yet produced one (or the project has no board client).
func (p *FleetProject) snapshotBoard() *fleetProjectBoard {
	if p == nil || p.board == nil {
		return nil
	}
	p.board.mu.RLock()
	defer p.board.mu.RUnlock()
	if p.board.snapshot == nil {
		return nil
	}
	// Return a copy to keep the caller from mutating cached state.
	out := *p.board.snapshot
	if len(p.board.snapshot.Columns) > 0 {
		out.Columns = append([]github.ProjectBoardColumn(nil), p.board.snapshot.Columns...)
	}
	return &out
}

// refreshOnce performs one discover-then-count cycle and updates the cache.
// Errors are logged and stored on the snapshot so the SPA can surface a
// degraded-but-known state; the cycle never blocks the caller.
func (b *boardState) refreshOnce() {
	b.mu.RLock()
	field := b.field
	discoverRetry := b.discoverRetry
	client := b.client
	projectNumber := b.projectNumber
	b.mu.RUnlock()

	if client == nil || projectNumber <= 0 {
		return
	}

	if field == nil {
		if !discoverRetry.IsZero() && time.Now().Before(discoverRetry) {
			return
		}
		pf, err := client.DiscoverProject(projectNumber)
		if err != nil {
			b.mu.Lock()
			b.discoverRetry = time.Now().Add(fleetBoardDiscoverBackoff)
			b.snapshot = &fleetProjectBoard{
				Number:    projectNumber,
				Error:     "discover project: " + err.Error(),
				FetchedAt: time.Now().UTC().Format(time.RFC3339),
			}
			b.mu.Unlock()
			log.Printf("[fleet-board] discover project %d failed: %v (backing off)", projectNumber, err)
			return
		}
		b.mu.Lock()
		b.field = pf
		b.discoverRetry = time.Time{}
		b.lastDiscoverAt = time.Now()
		b.mu.Unlock()
		field = pf
	}

	columns, total, err := client.ListProjectItemStatusCounts(field)
	if err != nil {
		b.mu.Lock()
		b.snapshot = &fleetProjectBoard{
			Number:    field.Number,
			URL:       github.ProjectBoardURL(field),
			Owner:     field.Owner,
			OwnerType: field.OwnerType,
			Error:     "count project items: " + err.Error(),
			FetchedAt: time.Now().UTC().Format(time.RFC3339),
		}
		b.mu.Unlock()
		log.Printf("[fleet-board] count project %d items failed: %v", field.Number, err)
		return
	}

	b.mu.Lock()
	b.snapshot = &fleetProjectBoard{
		Number:     field.Number,
		URL:        github.ProjectBoardURL(field),
		Owner:      field.Owner,
		OwnerType:  field.OwnerType,
		Columns:    columns,
		TotalItems: total,
		FetchedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	b.mu.Unlock()
}

// startBoardRefreshers fans out one refresher goroutine per project that has
// a board client configured. The goroutines respect ctx.Done() and exit
// cleanly on shutdown. Called from FleetServer.Start.
func (s *FleetServer) startBoardRefreshers(ctx context.Context) {
	if s == nil {
		return
	}
	for i := range s.projects {
		if !s.projects[i].HasBoardClient() {
			continue
		}
		project := &s.projects[i]
		go runBoardRefresher(ctx, project.board)
	}
}

func runBoardRefresher(ctx context.Context, b *boardState) {
	if b == nil {
		return
	}
	// Kick once immediately so the first /api/v1/fleet hit after startup
	// has data instead of an empty board widget.
	b.refreshOnce()
	ticker := time.NewTicker(fleetBoardRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.refreshOnce()
		}
	}
}
