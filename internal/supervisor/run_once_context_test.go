package supervisor

import (
	"context"
	"errors"
	"testing"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// cancellingReader cancels the cycle from inside the first GitHub read, which
// is where a real cancellation lands: mid-Decide, not between calls.
type cancellingReader struct {
	*fakeReader
	cancel context.CancelFunc
	reads  int
}

func (r *cancellingReader) ListOpenIssues(labels []string) ([]github.Issue, error) {
	r.reads++
	r.cancel()
	return r.fakeReader.ListOpenIssues(labels)
}

// TestRunOnceReturnsOnAnAlreadyCancelledContext pins the cheapest half of the
// #1119 contract: a cycle whose context is dead before it starts does no work.
func TestRunOnceReturnsOnAnAlreadyCancelledContext(t *testing.T) {
	cfg := testConfig(t)
	reader := &fakeReader{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := RunOnce(ctx, cfg, reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce error = %v, want context.Canceled", err)
	}
	if reader.issueCalls != 0 {
		t.Errorf("cancelled cycle still read %d issue page(s) from GitHub", reader.issueCalls)
	}
}

// TestRunOnceStopsAtThePhaseBoundaryAfterCancellation covers the case the
// daemon actually relies on: the cancel arrives while the cycle is already
// running, and RunOnce abandons it at the next phase boundary instead of
// carrying on into the mutating phase and stamping a heartbeat for a cycle
// that was told to stop.
func TestRunOnceStopsAtThePhaseBoundaryAfterCancellation(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &cancellingReader{fakeReader: &fakeReader{}, cancel: cancel}

	if _, err := RunOnce(ctx, cfg, reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce error = %v, want context.Canceled", err)
	}
	if reader.reads == 0 {
		t.Fatal("the cycle never reached the GitHub read, so this test did not exercise a mid-cycle cancel")
	}
	st, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if !st.LastRunOnceAt.IsZero() {
		t.Error("a cancelled cycle stamped LastRunOnceAt: it ran through the mutating phase instead of stopping at the boundary")
	}
	if latest := st.LatestSupervisorDecision(); latest != nil {
		t.Error("a cancelled cycle recorded a decision: it ran through the mutating phase instead of stopping at the boundary")
	}
}
