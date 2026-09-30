package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/befeast/maestro/internal/controllerowner"
)

func TestControllerOwnerDispatchClassification(t *testing.T) {
	for _, tc := range []struct {
		command string
		args    []string
		want    bool
	}{
		{"daemon", nil, true}, {"daemon", []string{"--store", "copied.db", "--port", "0"}, true},
		{"run", []string{"--once"}, true}, {"run", []string{"--config", "other.yaml"}, true},
		{"night-start", []string{"--dry-run"}, true}, {"spawn", []string{"--issue", "1"}, true},
		{"supervise", nil, true}, {"supervise", []string{"--once", "--dry-run"}, true},
		{"supervise", []string{"--config", "approve", "--once"}, true},
		{"supervise", []string{"--config=reject"}, true},
		{"supervise", []string{"--unknown", "approve"}, true},
		{"supervise", []string{"approve", "--config", "x", "id"}, false},
		{"supervise", []string{"reject", "id"}, false},
		{"supervise", []string{"reconcile-delivery", "id"}, false},
		{"supervise", []string{"--config", "x", "--once", "approve", "id"}, false},
		{"supervise", []string{"--interval=1m", "--", "reject", "id"}, false},
		{"supervise", []string{"--config-store", "copied.db", "reconcile-delivery", "id"}, false},
		{"daemon", []string{"--help"}, false}, {"run", []string{"-h"}, false},
		{"run", []string{"--prompt", "--help"}, true},
		{"serve", nil, false}, {"config-store", []string{"edit", "project"}, false},
		{"project", []string{"apply"}, false}, {"settings", nil, false},
		{"emergency", []string{"resume"}, false}, {"pause", nil, false}, {"drain", nil, false},
		{"resume", nil, false}, {"stop", nil, false}, {"kill", nil, false},
		{"status", nil, false}, {"history", nil, false}, {"logs", nil, false},
		{"cleanup", nil, false}, {"import", nil, false}, {"version-bump", nil, false},
		{"candidate-preflight", nil, false}, {"selfcheck", nil, false},
		{"_worker-exec", nil, false}, {"_worker-lease-cleanup", nil, false}, {"stream-split", nil, false},
	} {
		t.Run(tc.command+"/"+fmtArgs(tc.args), func(t *testing.T) {
			if got := requiresControllerOwner(tc.command, tc.args); got != tc.want {
				t.Fatalf("requires owner=%v want=%v args=%q", got, tc.want, tc.args)
			}
		})
	}
}

func fmtArgs(args []string) string {
	var out string
	for _, arg := range args {
		out += arg + " "
	}
	return out
}

type controllerTestCloser struct{ calls int }

func (c *controllerTestCloser) Close() error { c.calls++; return nil }

func TestControllerOwnerRejectsBeforeCommandSideEffects(t *testing.T) {
	root := t.TempDir()
	sideEffect := filepath.Join(root, "maestro.db")
	for _, command := range []string{"daemon", "run", "night-start", "spawn", "supervise"} {
		owner, err := claimControllerForCommand(command, []string{"--once"}, func() (io.Closer, error) {
			return nil, controllerowner.ErrAlreadyOwned
		})
		if err == nil { // mirror main: the command callback is entered only on admission
			_ = os.WriteFile(sideEffect, []byte("must not execute"), 0600)
		}
		if owner != nil || !errors.Is(err, controllerowner.ErrAlreadyOwned) {
			t.Fatalf("admission=%v err=%v", owner, err)
		}
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatal("rejected controller changed resources")
	}
}

func TestControllerOwnerSingleClaimAtTopLevel(t *testing.T) {
	acquires := 0
	closer := &controllerTestCloser{}
	owner, err := claimControllerForCommand("night-start", nil, func() (io.Closer, error) { acquires++; return closer, nil })
	if err != nil {
		t.Fatal(err)
	}
	// Internal night-start -> runCmd and daemon -> supervisor calls do not pass
	// through the executable dispatch boundary, so there is only this one claim.
	if acquires != 1 || closer.calls != 0 {
		t.Fatal("ownership was not retained by dispatcher")
	}
	_ = owner.Close()
	if closer.calls != 1 {
		t.Fatal("dispatcher did not release ownership")
	}
	for _, cmd := range []string{"serve", "config-store", "status", "emergency"} {
		owner, err := claimControllerForCommand(cmd, nil, func() (io.Closer, error) { t.Fatal("admin attempted scheduler acquisition"); return nil, nil })
		if owner != nil || err != nil {
			t.Fatal("admin command changed admission")
		}
	}
}
