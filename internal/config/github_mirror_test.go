package config

import (
	"testing"
	"time"
)

func TestParse_GitHubMirror_DefaultIsAPIDirect(t *testing.T) {
	cfg, err := parse([]byte("repo: owner/repo\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.GitHubMirror.Source != GitHubSourceAPI {
		t.Fatalf("default source = %q, want %q", cfg.GitHubMirror.Source, GitHubSourceAPI)
	}
	if cfg.GitHubMirror.MirrorFirst() {
		t.Fatal("MirrorFirst() must be false by default (today's behavior until soaked)")
	}
	if got := cfg.GitHubMirror.StaleHorizon(); got != 24*time.Hour {
		t.Fatalf("default StaleHorizon = %s, want 24h", got)
	}
	if got := cfg.GitHubMirror.ReconcileInterval(); got != DefaultMirrorReconcileInterval {
		t.Fatalf("default ReconcileInterval = %s, want %s", got, DefaultMirrorReconcileInterval)
	}
}

func TestParse_GitHubMirror_MirrorFirst(t *testing.T) {
	yaml := `
repo: owner/repo
github_mirror:
  source: mirror-first
  stale_seconds: 300
  reconcile_seconds: 600
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.GitHubMirror.MirrorFirst() {
		t.Fatal("MirrorFirst() should be true for source: mirror-first")
	}
	if got := cfg.GitHubMirror.StaleHorizon(); got != 5*time.Minute {
		t.Fatalf("StaleHorizon = %s, want 5m", got)
	}
	if got := cfg.GitHubMirror.ReconcileInterval(); got != 10*time.Minute {
		t.Fatalf("ReconcileInterval = %s, want 10m", got)
	}
}

func TestParse_GitHubMirror_UnknownDefaultsToAPI(t *testing.T) {
	// A typo (or any unrecognised value) resolves to API-direct — the safe
	// direction: a mis-set flag can only fall back to the authoritative API.
	yaml := `
repo: owner/repo
github_mirror:
  source: mirorr-frist
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.GitHubMirror.Source != GitHubSourceAPI {
		t.Fatalf("unknown source normalized to %q, want %q", cfg.GitHubMirror.Source, GitHubSourceAPI)
	}
	if cfg.GitHubMirror.MirrorFirst() {
		t.Fatal("a typo must not enable mirror-first")
	}
}

func TestParse_GitHubMirror_ExplicitAPIEscapeHatch(t *testing.T) {
	yaml := `
repo: owner/repo
github_mirror:
  source: api
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.GitHubMirror.MirrorFirst() {
		t.Fatal("source: api is the escape hatch — MirrorFirst() must be false")
	}
}
