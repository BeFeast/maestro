package configwatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
)

// fakeStore is an in-memory ProjectStore so WatchStore can be exercised without
// a real SQLite config store. updated_at is bumped by Set, mirroring how the
// real store advances a project's timestamp on UpsertProject.
type fakeStore struct {
	mu      sync.Mutex
	cfgs    map[string]*config.Config
	stamps  map[string]time.Time
	loadErr error
	fpErr   error
	clock   time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		cfgs:   map[string]*config.Config{},
		stamps: map[string]time.Time{},
		clock:  time.Unix(1_700_000_000, 0).UTC(),
	}
}

func (f *fakeStore) Set(name string, cfg *config.Config) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(time.Second)
	f.cfgs[name] = cfg
	f.stamps[name] = f.clock
}

func (f *fakeStore) Delete(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.cfgs, name)
	delete(f.stamps, name)
}

func (f *fakeStore) setLoadErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadErr = err
}

func (f *fakeStore) Load(ctx context.Context, name string) (*config.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	cfg, ok := f.cfgs[name]
	if !ok {
		return nil, errors.New("not found")
	}
	return cfg, nil
}

func (f *fakeStore) ProjectsFingerprint(ctx context.Context) (map[string]time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fpErr != nil {
		return nil, f.fpErr
	}
	out := make(map[string]time.Time, len(f.stamps))
	for k, v := range f.stamps {
		out[k] = v
	}
	return out, nil
}

func TestWatchStore_DetectsConfigChange(t *testing.T) {
	store := newFakeStore()
	store.Set("alpha", &config.Config{Repo: "owner/alpha", MaxParallel: 3})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := WatchStore(ctx, store, "alpha", 50*time.Millisecond)

	// Seeded with the current timestamp: no spurious reload before a change.
	select {
	case cfg := <-ch:
		t.Fatalf("unexpected reload before any change: %+v", cfg)
	case <-time.After(200 * time.Millisecond):
	}

	// Bump updated_at by writing a new config.
	store.Set("alpha", &config.Config{Repo: "owner/alpha", MaxParallel: 10})

	select {
	case cfg := <-ch:
		if cfg.MaxParallel != 10 {
			t.Fatalf("MaxParallel = %d, want 10", cfg.MaxParallel)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for store reload")
	}
}

func TestWatchStore_NoChangeNoEvent(t *testing.T) {
	store := newFakeStore()
	store.Set("alpha", &config.Config{Repo: "owner/alpha"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := WatchStore(ctx, store, "alpha", 50*time.Millisecond)

	select {
	case cfg := <-ch:
		t.Fatalf("should not reload an unchanged project, got %+v", cfg)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestWatchStore_MissingProjectNoEvent(t *testing.T) {
	// A project absent from the store must not emit — the daemon diff-loop owns
	// removal; WatchStore staying quiet keeps the running flow on its last good
	// config until the diff-loop drains it.
	store := newFakeStore()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := WatchStore(ctx, store, "ghost", 50*time.Millisecond)

	select {
	case cfg := <-ch:
		t.Fatalf("should not reload a missing project, got %+v", cfg)
	case <-time.After(400 * time.Millisecond):
	}
}

func TestWatchStore_LoadErrorKeepsPrevious(t *testing.T) {
	store := newFakeStore()
	store.Set("alpha", &config.Config{Repo: "owner/alpha", MaxParallel: 1})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := WatchStore(ctx, store, "alpha", 50*time.Millisecond)

	// A change lands, but Load fails — no config is emitted and last is NOT
	// advanced, so a later successful change still reloads.
	store.setLoadErr(errors.New("boom"))
	store.Set("alpha", &config.Config{Repo: "owner/alpha", MaxParallel: 7})

	select {
	case cfg := <-ch:
		t.Fatalf("should not emit while Load errors, got %+v", cfg)
	case <-time.After(400 * time.Millisecond):
	}

	store.setLoadErr(nil)
	store.Set("alpha", &config.Config{Repo: "owner/alpha", MaxParallel: 9})
	select {
	case cfg := <-ch:
		if cfg.MaxParallel != 9 {
			t.Fatalf("MaxParallel = %d, want 9", cfg.MaxParallel)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for reload after Load recovered")
	}
}

func TestWatchStore_ContextCancelClosesChannel(t *testing.T) {
	store := newFakeStore()
	store.Set("alpha", &config.Config{Repo: "owner/alpha"})

	ctx, cancel := context.WithCancel(context.Background())
	ch := WatchStore(ctx, store, "alpha", 50*time.Millisecond)
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			// Drained a buffered value first; the next read must be the close.
			select {
			case _, ok := <-ch:
				if ok {
					t.Fatal("channel should be closed after context cancel")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("channel not closed after context cancel")
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after context cancel")
	}
}
