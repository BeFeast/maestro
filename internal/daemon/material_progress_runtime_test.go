package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/server"
	"github.com/befeast/maestro/internal/state"
)

// The stalled-progress clock is a sibling flow, not a callback from the
// supervisor decision cycle. Even a supervisor loop that fails/returns before
// one successful Engine.Decide cannot prevent the local evaluator from writing
// its first durable tick.
func TestStartFlowMaterialProgressIndependentFromSupervisorFailure(t *testing.T) {
	cfg := testConfig(t, "owner/watchdog-runtime")
	enabled := true
	cfg.StalledProgressWatchdog.Enabled = &enabled
	cfg.StalledProgressWatchdog.MaxSilenceMinutes = 20
	cfg.StalledProgressWatchdog.EvalIntervalSecs = 1

	d := New(fakeLoader{cfgs: []*config.Config{cfg}}, Options{Port: 0, SuperviseInterval: time.Minute})
	d.runLoop = func(ctx context.Context, cfg *config.Config, opts Options, reloadCh <-chan *config.Config) {
		<-ctx.Done()
	}
	var supervisorReturned int64
	d.superviseLoop = func(context.Context, string, func() *config.Config, Options, <-chan struct{}) {
		atomic.StoreInt64(&supervisorReturned, 1) // models an unrecoverable first-cycle failure
	}
	d.watchdogLoop = func(context.Context, string, string, time.Duration, chan<- struct{}) {}

	flow := d.startFlow(context.Background(), "", server.NewFleetProjectWithGitHub(cfg))
	defer d.stopFlow(flow.key)
	if cfg.RuntimeSuperviseIntervalSeconds != 60 {
		t.Fatalf("runtime supervisor cadence = %d, want actual daemon interval 60", cfg.RuntimeSuperviseIntervalSeconds)
	}
	waitFor(t, func() bool { return atomic.LoadInt64(&supervisorReturned) == 1 })
	waitFor(t, func() bool {
		st, err := state.Load(cfg.StateDir)
		return err == nil && st.MaterialProgress != nil && !st.MaterialProgress.LastEvaluatedAt.IsZero()
	})
}
