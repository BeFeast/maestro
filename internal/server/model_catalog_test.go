package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"golang.org/x/sys/unix"
)

func TestModelCatalogDisplayDoesNotMutateExecutablePolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.yml")
	if err := os.WriteFile(path, []byte("version: 1\nmodels:\n  - id: gpt-6-astra\n    upstream: openai\n    tier: primary\n  - id: claude-fable-5-1\n    upstream: anthropic\n  - id: direct/private-route\n    upstream: direct\nclients:\n  token: synthetic-secret-must-not-leak\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAESTRO_MODEL_CATALOG", path)
	cfg := &config.Config{Model: config.ModelConfig{Default: "pilot", Backends: map[string]config.BackendDef{
		"pilot":   {Cmd: "claude --model claude-opus-4-8", Model: "claude-opus-4-8", Provider: "anthropic"},
		"codex":   {Cmd: "/local/bin/codex --model kimi-k2.7-code", Model: "gpt-6-astra", Provider: "openai"},
		"current": {Cmd: "claude --model=claude-fable-5-1", Model: "claude-fable-5-1", Provider: "anthropic"},
	}}}
	cfg.Supervisor.Backend = "current"
	before, err := config.AIExecutionConfigDigest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	effective := buildFleetEffectiveConfig(cfg)
	after, err := config.AIExecutionConfigDigest(cfg)
	if err != nil || before != after {
		t.Fatal("display changed executable policy")
	}
	if len(effective.ModelPolicy.Catalog.Models) != 2 {
		t.Fatal("direct routes included or current models lost")
	}
	byName := make(map[string]fleetEffectiveBackendConfig)
	for _, b := range effective.ModelPolicy.Backends {
		byName[b.Name] = b
	}
	if b := byName["pilot"]; b.CatalogStatus != "not_listed" || len(b.References) != 1 || b.CommandModel != "claude-opus-4-8" {
		t.Fatalf("pilot mapping changed: %+v", b)
	}
	if b := byName["codex"]; b.Harness != "codex" || b.CommandModel != "kimi-k2.7-code" || b.CatalogStatus != "not_listed" || b.References == nil || len(b.References) != 0 {
		t.Fatalf("misleading Codex mapping: %+v", b)
	}
	if b := byName["current"]; b.CatalogStatus != "listed" || b.CatalogProvider != "anthropic" || len(b.References) != 1 {
		t.Fatalf("current mapping wrong: %+v", b)
	}
	raw, _ := json.Marshal(effective)
	if strings.Contains(string(raw), "synthetic-secret") || strings.Contains(string(raw), "/local/bin") {
		t.Fatal("raw configuration leaked")
	}
}

func TestModelCatalogUnavailableDoesNotDeclareModelsObsolete(t *testing.T) {
	for _, kind := range []string{"missing", "symlink", "directory", "fifo", "oversized", "invalid", "duplicate", "wrong_version"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "catalog.yml")
			switch kind {
			case "symlink":
				if err := os.Symlink("/nonexistent", path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.WriteFile(path, make([]byte, (1<<20)+1), 0600); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				os.WriteFile(path, []byte("models: [\n"), 0600)
			case "duplicate":
				os.WriteFile(path, []byte("version: 1\nmodels:\n - id: duplicate\n - id: duplicate\n"), 0600)
			case "wrong_version":
				os.WriteFile(path, []byte("version: 2\nmodels:\n - id: example\n"), 0600)
			}
			catalog := readFleetModelCatalogFile(path)
			status, _ := catalog.lookup("existing-model")
			if catalog.Status != "unavailable" || status != "unavailable" {
				t.Fatalf("unreadable catalog mislabeled model: %+v", catalog)
			}
		})
	}
}

func TestBackendReferencesCoverWorkerAuxiliaryAndRoutingChoices(t *testing.T) {
	cfg := &config.Config{Model: config.ModelConfig{Default: "default", FallbackBackends: []string{"fallback"}, ProviderLanes: []config.ProviderLane{{Default: "lane", FallbackBackends: []string{"lane-fallback"}}}}}
	cfg.Supervisor.Backend = "supervisor"
	cfg.Supervisor.ReviewRepair.Backend = "repair"
	cfg.Routing.Mode = "auto"
	cfg.Routing.RouterModel = "router"
	cfg.Routing.TaskTypeBackends = map[string]string{"feature": "task"}
	cfg.Routing.Tiers = map[string]config.RoutingTier{"top": {Backend: "tier"}}
	cfg.Pipeline.Advisor.Backend = "advisor"
	refs := fleetBackendReferences(cfg)
	for _, name := range []string{"default", "fallback", "lane", "lane-fallback", "supervisor", "repair", "router", "task", "tier", "advisor"} {
		if len(refs[name]) != 1 {
			t.Fatalf("reference missing: %s", name)
		}
	}
}

func TestBackendReferencesOmitInactiveRouter(t *testing.T) {
	for _, mode := range []string{"", "manual", "policy"} {
		t.Run(mode, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Model.Default = "worker"
			cfg.Routing.Mode = mode
			cfg.Routing.RouterModel = "obsolete-router"
			refs := fleetBackendReferences(cfg)
			if len(refs["obsolete-router"]) != 0 {
				t.Fatalf("inactive router reported as a reference in %q mode: %v", mode, refs)
			}
			if len(refs["worker"]) != 1 {
				t.Fatalf("worker reference lost: %v", refs)
			}
		})
	}
}
