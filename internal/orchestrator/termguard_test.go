package orchestrator

import (
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/termguard/termguardtest"
)

// TestFixtureWithoutStopSeamIsRefused pins the TestMain contract: an
// Orchestrator fixture without workerStopFn / workerStopProcessFn never
// reaches the real worker teardown with its literal PID and tmux name (#1252).
func TestFixtureWithoutStopSeamIsRefused(t *testing.T) {
	o := &Orchestrator{cfg: &config.Config{Repo: "owner/repo"}}
	sess := &state.Session{Status: state.StatusRunning, PID: 4242, TmuxSession: "maestro-ok-player-1"}

	err := termguardtest.ExpectRefusal(t, func() { _ = o.stopWorker("ok-player-1", sess) })
	if !strings.Contains(err.Error(), "inject workerStopFn") || !strings.Contains(err.Error(), "pid=4242") {
		t.Fatalf("stopWorker refusal = %v, want it to name the missing workerStopFn and pid=4242", err)
	}
	err = termguardtest.ExpectRefusal(t, func() { _ = o.stopWorkerProcess("ok-player-1", sess) })
	if !strings.Contains(err.Error(), "inject workerStopProcessFn") {
		t.Fatalf("stopWorkerProcess refusal = %v, want it to name the missing workerStopProcessFn", err)
	}
	if sess.PID != 4242 || sess.TmuxSession != "maestro-ok-player-1" {
		t.Fatalf("refused stop mutated the session: pid=%d tmux=%q", sess.PID, sess.TmuxSession)
	}
}
