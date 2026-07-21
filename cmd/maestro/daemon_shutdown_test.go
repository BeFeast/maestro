package main

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type fakeDaemonShutdown struct {
	mu               sync.Mutex
	shutdownDeadline time.Time
	drainDeadline    time.Time
	drainStarted     chan struct{}
}

func (f *fakeDaemonShutdown) SetShutdownDeadline(deadline time.Time) {
	f.mu.Lock()
	f.shutdownDeadline = deadline
	f.mu.Unlock()
}

func (f *fakeDaemonShutdown) DrainUntil(ctx context.Context, deadline time.Time) {
	f.mu.Lock()
	f.drainDeadline = deadline
	f.mu.Unlock()
	close(f.drainStarted)
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func TestHandleDaemonSignalStreamBoundsWholeShutdownAndForceExitsOnce(t *testing.T) {
	oldGrace, oldForceExit := shutdownHandoffGrace, forceExit
	shutdownHandoffGrace = 50 * time.Millisecond
	defer func() {
		shutdownHandoffGrace = oldGrace
		forceExit = oldForceExit
	}()

	var exitCalls atomic.Int32
	exited := make(chan struct{}, 1)
	forceExit = func(code int) {
		if code != 0 {
			t.Errorf("forceExit code = %d, want 0", code)
		}
		if exitCalls.Add(1) == 1 {
			exited <- struct{}{}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakeDaemonShutdown{drainStarted: make(chan struct{})}
	runDone := make(chan struct{}) // deliberately never closed: Run is wedged
	signals := make(chan os.Signal, 2)
	signals <- syscall.SIGTERM

	started := time.Now()
	handlerDone := make(chan struct{})
	go func() {
		handleDaemonSignalStream(ctx, cancel, fake, 200*time.Millisecond, runDone, signals)
		close(handlerDone)
	}()

	select {
	case <-fake.drainStarted:
	case <-time.After(time.Second):
		t.Fatal("drain did not start")
	}
	fake.mu.Lock()
	shutdownDeadline, drainDeadline := fake.shutdownDeadline, fake.drainDeadline
	fake.mu.Unlock()
	reserved := shutdownDeadline.Sub(drainDeadline)
	if reserved < 40*time.Millisecond || reserved > 75*time.Millisecond {
		t.Fatalf("reserved handoff = %v, want about 50ms inside the total budget", reserved)
	}

	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("signal handler did not finish after the reserved drain phase")
	}
	if elapsed := time.Since(started); elapsed >= 195*time.Millisecond {
		t.Fatalf("daemon ctx cancelled at %v, want before the whole-shutdown deadline so handoff has reserved time", elapsed)
	}

	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("exact-deadline force-exit backstop did not fire")
	}
	time.Sleep(30 * time.Millisecond)
	if got := exitCalls.Load(); got != 1 {
		t.Fatalf("forceExit calls = %d, want exactly 1", got)
	}
}

func TestAwaitShutdownOrForceExitPrefersCompletedRun(t *testing.T) {
	oldForceExit := forceExit
	defer func() { forceExit = oldForceExit }()
	var exitCalls atomic.Int32
	forceExit = func(int) { exitCalls.Add(1) }

	runDone := make(chan struct{})
	close(runDone)
	awaitShutdownOrForceExit(time.Now().Add(10*time.Millisecond), runDone, nil)
	if got := exitCalls.Load(); got != 0 {
		t.Fatalf("forceExit calls = %d, want 0 after Run completed", got)
	}
}

func TestHandleDaemonSignalStreamSecondSignalShortensHardDeadline(t *testing.T) {
	oldHandoffGrace, oldForcedGrace, oldForceExit := shutdownHandoffGrace, forcedShutdownGrace, forceExit
	shutdownHandoffGrace = 50 * time.Millisecond
	forcedShutdownGrace = 40 * time.Millisecond
	defer func() {
		shutdownHandoffGrace = oldHandoffGrace
		forcedShutdownGrace = oldForcedGrace
		forceExit = oldForceExit
	}()

	var exitCalls atomic.Int32
	exited := make(chan struct{}, 1)
	forceExit = func(int) {
		if exitCalls.Add(1) == 1 {
			exited <- struct{}{}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakeDaemonShutdown{drainStarted: make(chan struct{})}
	runDone := make(chan struct{}) // deliberately wedged after cancellation
	signals := make(chan os.Signal, 2)
	signals <- syscall.SIGTERM

	handlerDone := make(chan struct{})
	go func() {
		handleDaemonSignalStream(ctx, cancel, fake, 2*time.Second, runDone, signals)
		close(handlerDone)
	}()

	select {
	case <-fake.drainStarted:
	case <-time.After(time.Second):
		t.Fatal("drain did not start")
	}
	secondAt := time.Now()
	signals <- syscall.SIGTERM

	select {
	case <-handlerDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second signal did not abort the worker drain promptly")
	}
	select {
	case <-exited:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second signal left the hard backstop on the original deadline")
	}
	if elapsed := time.Since(secondAt); elapsed > 250*time.Millisecond {
		t.Fatalf("forced shutdown backstop fired after %v, want the short second-signal grace", elapsed)
	}
	fake.mu.Lock()
	forcedDeadline := fake.shutdownDeadline
	fake.mu.Unlock()
	if remaining := forcedDeadline.Sub(secondAt); remaining < 20*time.Millisecond || remaining > 150*time.Millisecond {
		t.Fatalf("second signal deadline offset = %v, want about 40ms", remaining)
	}
	time.Sleep(30 * time.Millisecond)
	if got := exitCalls.Load(); got != 1 {
		t.Fatalf("forceExit calls = %d, want exactly 1 after second signal", got)
	}
}
