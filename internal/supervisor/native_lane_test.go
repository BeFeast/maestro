package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/google/uuid"
)

type fakeLaneReadiness struct {
	err   error
	calls int
}

func (f *fakeLaneReadiness) ObserveLaneReadiness(aiexecution.Policy) error {
	f.calls++
	return f.err
}

type countingAuxiliaryLimiter struct{ calls int }

func (c *countingAuxiliaryLimiter) ReserveAuxiliary(_, _ string) (func(), error) {
	c.calls++
	return func() {}, nil
}

// A managed lane that is not ready holds a native consultation before
// auxiliary capacity and before any authority registration. The receipt is the
// same closed zero-candidate pre-launch shape as a refused reservation, so a
// supervisor falls back deterministically and an operator rearm grant stays
// provably unspent; repeated cycles create no registrations.
func TestNativeConsultationLaneNotReadyClosesReceiptBeforeRegistration(t *testing.T) {
	for _, mode := range []string{"typed", "untyped"} {
		t.Run(mode, func(t *testing.T) {
			cfg, count, registrations := nativeConfig(t)
			aux := &countingAuxiliaryLimiter{}
			cfg.RuntimeAuxiliaryLimiter = aux
			want := "binding_route_unserved"
			lane := &fakeLaneReadiness{err: aiexecution.Held(want)}
			if mode == "untyped" {
				lane.err, want = errors.New("probe failed"), "binding_readiness_unobservable"
			}
			cfg.RuntimeNativeLaneReadiness = lane
			reviews := filepath.Join(cfg.StateDir, "native-reviews")
			for cycle := 0; cycle < 3; cycle++ {
				claim := uuid.NewString()
				_, err := CompleteNativeReview(context.Background(), cfg, "claude-opus-5", claim, "synthetic diff")
				var hold *aiexecution.Hold
				if !errors.As(err, &hold) || hold.Code != want {
					t.Fatalf("error=%v want %s", err, want)
				}
				r := loadReceipt(t, &config.Config{StateDir: reviews})
				if r.Identity.ID != claim || r.Status != "failed" || r.EndedAt == nil || r.PlannedInvocation != nil || len(r.Candidates) != 0 || len(r.Invocations) != 0 {
					t.Fatalf("receipt=%+v", r)
				}
				for _, marker := range []string{"launch.json", "registration.json"} {
					if _, err := os.Lstat(filepath.Join(reviews, "supervisor-consultations", marker)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("%s written before a ready lane: %v", marker, err)
					}
				}
			}
			registrations.mu.Lock()
			registered := len(registrations.requests)
			registrations.mu.Unlock()
			if aux.calls != 0 || nativeCalls(t, count) != 0 || lane.calls != 3 || registered != 0 {
				t.Fatalf("aux=%d native=%d probes=%d registrations=%d", aux.calls, nativeCalls(t, count), lane.calls, registered)
			}
			lane.err = nil
			if _, err := CompleteNativeReview(context.Background(), cfg, "claude-opus-5", uuid.NewString(), "synthetic diff"); err != nil && aux.calls == 0 {
				t.Fatalf("ready lane did not reach capacity reservation: %v", err)
			}
			if aux.calls != 1 {
				t.Fatalf("ready lane reservations=%d", aux.calls)
			}
		})
	}
}
