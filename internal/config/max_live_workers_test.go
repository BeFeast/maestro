package config

import "testing"

// #814: max_live_workers separates live implementation-worker capacity from
// pr_open PR-gate sessions. Default is 0 (legacy: pr_open counts against
// max_parallel); a negative value is clamped to 0 so a typo cannot silently
// disable dispatch.
func TestParse_MaxLiveWorkersDefaultsToZero(t *testing.T) {
	cfg, err := Parse([]byte("repo: owner/repo\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.MaxLiveWorkers != 0 {
		t.Errorf("MaxLiveWorkers = %d, want 0 (legacy default)", cfg.MaxLiveWorkers)
	}
}

func TestParse_MaxLiveWorkersExplicit(t *testing.T) {
	cfg, err := Parse([]byte("repo: owner/repo\nmax_parallel: 4\nmax_live_workers: 2\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.MaxLiveWorkers != 2 {
		t.Errorf("MaxLiveWorkers = %d, want 2", cfg.MaxLiveWorkers)
	}
	if cfg.MaxParallel != 4 {
		t.Errorf("MaxParallel = %d, want 4 (unchanged)", cfg.MaxParallel)
	}
}

func TestParse_MaxLiveWorkersNegativeClampsToZero(t *testing.T) {
	cfg, err := Parse([]byte("repo: owner/repo\nmax_live_workers: -3\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.MaxLiveWorkers != 0 {
		t.Errorf("MaxLiveWorkers = %d, want 0 (negative clamped)", cfg.MaxLiveWorkers)
	}
}
