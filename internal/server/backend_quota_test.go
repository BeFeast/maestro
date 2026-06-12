package server

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// #704 acceptance 1: the fleet snapshot carries quota state for backends
// with quota config — percent used and reset ETA — and nothing for
// backends without it.
func TestBuildFleetBackendQuota(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude", Quota: config.BackendQuota{WindowTokens: 1000, WeeklyTokens: 10000}},
				"codex":  {Cmd: "codex"},
			},
		},
	}
	anchor := now.Add(-time.Hour)
	st := state.NewState()
	st.BackendQuotaUsage = map[string]*state.BackendQuotaUsage{
		"claude": {
			WindowStartedAt: &anchor,
			WindowTokens:    900,
			WeekStartedAt:   &anchor,
			WeekTokens:      900,
		},
	}

	rows := buildFleetBackendQuota(cfg, st, now)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (codex has no quota config): %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Backend != "claude" || row.WindowPercent != 90 || row.WeekPercent != 9 {
		t.Fatalf("row = %+v, want claude at window 90%% / week 9%%", row)
	}
	wantReset := anchor.Add(state.BackendQuotaWindow)
	if row.WindowResetAt == nil || !row.WindowResetAt.Equal(wantReset) {
		t.Fatalf("window reset = %v, want %v", row.WindowResetAt, wantReset)
	}
	if !row.Pressured {
		t.Fatal("row should be pressured at 90% with default 85% threshold")
	}
	if row.DispatchThreshold != config.DefaultQuotaDispatchThreshold {
		t.Fatalf("threshold = %v, want default %v", row.DispatchThreshold, config.DefaultQuotaDispatchThreshold)
	}
}

// No usage recorded yet → an empty, unpressured gauge rather than no row,
// so the operator sees quota tracking is active.
func TestBuildFleetBackendQuota_NoUsageYet(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{
		Model: config.ModelConfig{
			Default: "claude",
			Backends: map[string]config.BackendDef{
				"claude": {Cmd: "claude", Quota: config.BackendQuota{WindowTokens: 1000}},
			},
		},
	}

	rows := buildFleetBackendQuota(cfg, state.NewState(), now)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].WindowPercent != 0 || rows[0].Pressured {
		t.Fatalf("row = %+v, want empty unpressured gauge", rows[0])
	}
}
