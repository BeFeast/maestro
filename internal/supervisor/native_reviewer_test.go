package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/google/uuid"
)

type auxiliaryFixture struct{ acquired, released atomic.Int64 }

func (a *auxiliaryFixture) ReserveAuxiliary(_, _ string) (func(), error) {
	a.acquired.Add(1)
	return func() { a.released.Add(1) }, nil
}

func (a *auxiliaryFixture) ReconcileAuxiliary(dir, id string) error {
	if err := NativeAuxiliaryOutcomeComplete(dir, id); err != nil {
		return err
	}
	a.released.Add(1)
	return nil
}

func TestNativeReviewerExactModelAndEveryLocalOutcomeHeld(t *testing.T) {
	for _, mode := range []string{"success", "failed", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			cfg, count, fixture := nativeConfig(t)
			aux := &auxiliaryFixture{}
			cfg.RuntimeAuxiliaryLimiter = aux
			receiptCfg := *cfg
			receiptCfg.StateDir = filepath.Join(cfg.StateDir, "native-reviews")
			fixture.cfg = &receiptCfg
			def := cfg.Model.Backends["primary"]
			def.Cmd = strings.TrimSuffix(def.Cmd, " fail")
			if mode == "failed" {
				def.Cmd += " fail"
			}
			if mode == "cancelled" {
				def.Cmd += " slow"
				script := "#!/bin/sh\ncat >/dev/null\nprintf partial-output\nprintf 'call\\n' >> '" + count + "'\nsleep 5\n"
				if err := os.WriteFile(filepath.Join(cfg.LocalPath, "claude"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			cfg.Model.Backends["primary"] = def
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if mode == "cancelled" {
				go func() {
					for i := 0; i < 100; i++ {
						if b, _ := os.ReadFile(count); len(b) > 0 {
							cancel()
							return
						}
						time.Sleep(10 * time.Millisecond)
					}
				}()
			}
			claim := uuid.NewString()
			out, err := CompleteNativeReview(ctx, cfg, "claude-opus-5", claim, "synthetic diff")
			var hold *aiexecution.Hold
			if !errors.As(err, &hold) || hold.Code != "native_outcome_unverified" || out != "" {
				t.Fatalf("output=%q error=%v", out, err)
			}
			if aux.acquired.Load() != 1 || aux.released.Load() != 0 || nativeCalls(t, count) != 1 {
				t.Fatal("unsettled launch released or fell back")
			}
			r := loadReceipt(t, &receiptCfg)
			if r.Identity.Role != "reviewer" || r.Identity.ID != claim || len(r.Invocations) != 1 || r.Invocations[0].EffectiveCLIModel == nil || *r.Invocations[0].EffectiveCLIModel != "claude-opus-5" || r.Capability.Ready() {
				t.Fatalf("receipt=%+v", r)
			}
			checkpoint := r.Invocations[0].OutputCheckpoint
			if checkpoint == nil {
				t.Fatal("launched output not checkpointed")
			}
			path := filepath.Join(receiptCfg.StateDir, "supervisor-consultations", checkpoint.Filename)
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			var record nativeOutputRecord
			if json.Unmarshal(data, &record) != nil || record.RoleRunID != claim || record.NativeSessionID != r.Invocations[0].ID || record.SHA256 != checkpoint.SHA256 || record.Complete != (mode == "success") {
				t.Fatalf("checkpoint=%+v", record)
			}
			if mode == "success" && string(record.Data) != "done" {
				t.Fatal("successful output lost")
			}
			if mode == "cancelled" && string(record.Data) != "partial-output" {
				t.Fatalf("partial output lost: %q", record.Data)
			}
			info, statErr := os.Stat(path)
			if statErr != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("raw checkpoint permissions: %v %v", info, statErr)
			}
			fixture.mu.Lock()
			if len(fixture.requests) != 1 || fixture.requests[0].Role != "reviewer" || len(fixture.problems) > 0 {
				t.Errorf("requests=%+v problems=%v", fixture.requests, fixture.problems)
			}
			fixture.mu.Unlock()
			_, err = CompleteNativeReview(context.Background(), cfg, "claude-opus-5", uuid.NewString(), "retry")
			if !errors.As(err, &hold) || hold.Code != "native_outcome_unverified" {
				t.Fatal(err)
			}
			if nativeCalls(t, count) != 1 || aux.acquired.Load() != 1 {
				t.Fatal("fresh identity bypassed unresolved outcome")
			}
			pending, err := PendingAuxiliaryRuns(cfg.StateDir)
			if err != nil || len(pending) != 1 {
				t.Fatalf("pending=%v error=%v", pending, err)
			}
		})
	}
}

func TestNativeReviewerStrictProofAndControllerAbsentNeverLaunch(t *testing.T) {
	for _, mode := range []string{"no_controller", "no_proof"} {
		t.Run(mode, func(t *testing.T) {
			cfg, count, fixture := nativeConfig(t)
			receiptCfg := *cfg
			receiptCfg.StateDir = filepath.Join(cfg.StateDir, "native-reviews")
			fixture.cfg = &receiptCfg
			aux := &auxiliaryFixture{}
			if mode == "no_proof" {
				cfg.RuntimeAuxiliaryLimiter = aux
				cfg.AIExecution.RequireVerifiedRoute = true
			}
			_, err := CompleteNativeReview(context.Background(), cfg, "requested", uuid.NewString(), "synthetic")
			var hold *aiexecution.Hold
			if !errors.As(err, &hold) || nativeCalls(t, count) != 0 {
				t.Fatalf("error=%v calls=%d", err, nativeCalls(t, count))
			}
			if mode == "no_proof" && aux.released.Load() != 1 {
				t.Fatal("prelaunch hold leaked permit")
			}
		})
	}
}
