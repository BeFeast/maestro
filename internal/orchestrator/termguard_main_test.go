package orchestrator

import (
	"fmt"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/termguard/termguardtest"
)

// TestMain makes every fixture-built Orchestrator stop workers through a fake.
// A fixture without workerStopFn / workerStopProcessFn would otherwise reach
// the real worker teardown with its literal PID and tmux name, so the package
// defaults are replaced by refusals that fail the offending test. The
// termguard hook additionally refuses any real process termination this test
// binary does not own, wherever it is reached from (#1252).
func TestMain(m *testing.M) {
	defaultWorkerStop = func(_ *config.Config, slotName string, sess *state.Session) error {
		termguardtest.Refuse(fmt.Errorf("orchestrator fixture reached the real worker.Stop for %s (%s); inject workerStopFn (#1252)", slotName, describeStopTarget(sess)))
		return nil
	}
	defaultWorkerStopProcess = func(slotName string, sess *state.Session) error {
		termguardtest.Refuse(fmt.Errorf("orchestrator fixture reached the real worker.StopProcess for %s (%s); inject workerStopProcessFn (#1252)", slotName, describeStopTarget(sess)))
		return nil
	}
	termguardtest.Main(m)
}

func describeStopTarget(sess *state.Session) string {
	if sess == nil {
		return "nil session"
	}
	return fmt.Sprintf("pid=%d tmux=%q", sess.PID, sess.TmuxSession)
}
