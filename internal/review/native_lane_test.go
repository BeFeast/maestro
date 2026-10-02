package review

import (
	"context"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
)

type fakeLaneReadiness struct {
	err   error
	calls int
}

func (f *fakeLaneReadiness) ObserveLaneReadiness(aiexecution.Policy) error {
	f.calls++
	return f.err
}

type recordingAuxiliaryLimiter struct {
	mu  sync.Mutex
	ids []string
}

func (r *recordingAuxiliaryLimiter) ReserveAuxiliary(_, id string) (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
	return func() {}, nil
}

func (r *recordingAuxiliaryLimiter) reserved(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, got := range r.ids {
		if got == id {
			return true
		}
	}
	return false
}

// An ordinary native review claim is not made while the managed lane is not
// ready: no status is posted (the stream stays unobserved, so Check cannot
// wedge on a status without an attempt) and no attempt is spent. The next
// cycle with a ready lane claims and runs exactly once.
func TestNativeReviewLaneNotReadyPostsNothingAndClaimsNothing(t *testing.T) {
	now := time.Now()
	s := newAttemptStore(t)
	p, f := httpProducer(t, s, "unused", &now, 1)
	lane := &fakeLaneReadiness{err: aiexecution.Held("binding_credential_unverified")}
	var calls atomic.Int64
	p.Lenses = []Lens{&NativeClaudeLens{Stream: "llm-review-opus", Model: "claude-opus-5", policy: aiexecution.Policy{RequireVerifiedRoute: true}, readiness: lane, complete: func(context.Context, string, string) (supervisor.NativeReviewResult, error) {
		calls.Add(1)
		return supervisor.NativeReviewResult{Output: "NO_FINDINGS"}, nil
	}}}
	p.ExecutionPolicy.RequireVerifiedRoute = true
	for cycle := 0; cycle < 3; cycle++ {
		if err := p.ProducePR(context.Background(), 7); err == nil {
			t.Fatal("paused lane reported a produced review")
		}
	}
	st, err := state.Load(s.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || len(f.posted) != 0 || len(st.ReviewAttempts[scopeFor(f).key()].Attempts) != 0 {
		t.Fatalf("paused lane calls=%d posted=%v attempts=%+v", calls.Load(), f.posted, st.ReviewAttempts)
	}
	if err := p.Attempts.Check(scopeFor(f), p.now(), 1, false); err != nil {
		t.Fatalf("paused lane wedged the stream: %v", err)
	}
	lane.err = nil
	if err := p.ProducePR(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || latestAttempt(t, s, scopeFor(f)).Outcome != "completed" {
		t.Fatalf("ready lane calls=%d", calls.Load())
	}
}

// A queued operator rearm grant is neither hinted nor exercised while the lane
// is not ready, and stays queued until it is.
func TestNativeOperatorRearmWaitsForReadyLane(t *testing.T) {
	p, cfg, scope, req, calls := rearmFixture(t)
	if _, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, req, p.now()); err != nil {
		t.Fatal(err)
	}
	lane := &fakeLaneReadiness{err: aiexecution.Held("binding_route_unserved")}
	cfg.RuntimeNativeLaneReadiness = lane
	p.Lenses[0].(*NativeClaudeLens).readiness = lane
	before, _ := state.Load(cfg.StateDir)
	if p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
		t.Fatal("grant hinted while the lane is not ready")
	}
	_ = p.ProducePR(context.Background(), scope.PR)
	after, _ := state.Load(cfg.StateDir)
	if calls.Load() != 0 || rearmSidecar(t, cfg.StateDir, scope, ".claimed") || !reflect.DeepEqual(after.ReviewAttempts[scope.key()], before.ReviewAttempts[scope.key()]) {
		t.Fatalf("grant exercised under a paused lane: calls=%d", calls.Load())
	}
	lane.err = nil
	if !p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
		t.Fatal("grant not queued once the lane is ready")
	}
	_ = p.ProducePR(context.Background(), scope.PR)
	if calls.Load() != 1 || !rearmSidecar(t, cfg.StateDir, scope, ".claimed") {
		t.Fatalf("ready lane did not exercise the grant once: calls=%d", calls.Load())
	}
}

// The lane can stop being ready between the claim's preflight and the native
// runner. The runner's own probe then closes a zero-candidate receipt before
// any registration, which the settlement proves pre-launch: the grant stays
// unclaimed, history is unchanged, and the hold reason is recorded.
func TestNativeOperatorRearmRunnerLaneHoldIsProvenPreLaunch(t *testing.T) {
	p, cfg, scope, req, _ := rearmFixture(t)
	if _, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, req, p.now()); err != nil {
		t.Fatal(err)
	}
	before, _ := state.Load(cfg.StateDir)
	limiter := &recordingAuxiliaryLimiter{}
	runnerLane := &fakeLaneReadiness{err: aiexecution.Held("binding_route_unserved")}
	var calls atomic.Int32
	var claimed string
	lens := p.Lenses[0].(*NativeClaudeLens)
	lens.limiter = limiter
	lens.complete = func(ctx context.Context, prompt string, id string) (supervisor.NativeReviewResult, error) {
		calls.Add(1)
		claimed = id
		runner := nativeRunnerConfig(cfg, limiter)
		runner.RuntimeNativeLaneReadiness = runnerLane
		return supervisor.CompleteNativeReview(ctx, runner, "claude-opus-5", id, prompt)
	}
	if err := p.ProducePR(context.Background(), scope.PR); err == nil {
		t.Fatal("held run reported success")
	}
	after, _ := state.Load(cfg.StateDir)
	// Preflights reserve throwaway IDs; the runner's lane probe holds before
	// its own reservation for the claim.
	if calls.Load() != 1 || runnerLane.calls != 1 || limiter.reserved(claimed) {
		t.Fatalf("runner calls=%d lane probes=%d claim reserved=%v", calls.Load(), runnerLane.calls, limiter.reserved(claimed))
	}
	if rearmSidecar(t, cfg.StateDir, scope, ".claimed") || !reflect.DeepEqual(after.ReviewAttempts[scope.key()].Attempts, before.ReviewAttempts[scope.key()].Attempts) {
		t.Fatal("pre-launch lane hold consumed the grant")
	}
	held, err := p.Attempts.readRearmHold(filepath.Join(cfg.StateDir, "review-rearms"), scope)
	if err != nil || held.AttemptID != claimed || held.Code != "binding_route_unserved" || held.Retractions != 1 {
		t.Fatalf("hold record = %+v err=%v", held, err)
	}
	if !nativeReviewPreLaunchHoldProven(filepath.Join(cfg.StateDir, "native-reviews"), claimed, cfg.ProjectID) {
		t.Fatal("lane hold receipt is not pre-launch evidence")
	}
}
