package server

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// Catalog metadata is read-only display context. It never becomes executable
// policy, changes model.backends, or participates in admission config digests.
type fleetModelCatalog struct {
	Status    string              `json:"status"`
	UpdatedAt string              `json:"updated_at,omitempty"`
	Models    []fleetCatalogModel `json:"models,omitempty"`
}

type fleetCatalogModel struct {
	ID       string `yaml:"id" json:"id"`
	Upstream string `yaml:"upstream" json:"provider,omitempty"`
	Tier     string `yaml:"tier" json:"tier,omitempty"`
}

func readFleetModelCatalog() fleetModelCatalog {
	path := strings.TrimSpace(os.Getenv("MAESTRO_MODEL_CATALOG"))
	if path == "" {
		root, err := os.UserConfigDir()
		if err != nil {
			return fleetModelCatalog{Status: "unavailable"}
		}
		path = filepath.Join(root, "ai-hub-ops", "catalog.yml")
	}
	return readFleetModelCatalogFile(path)
}

func readFleetModelCatalogFile(path string) fleetModelCatalog {
	unavailable := fleetModelCatalog{Status: "unavailable"}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 1<<20 {
		return unavailable
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return unavailable
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(st, opened) {
		return unavailable
	}
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return unavailable
	}
	var document struct {
		Version int                 `yaml:"version"`
		Models  []fleetCatalogModel `yaml:"models"`
	}
	if err := yaml.Unmarshal(b, &document); err != nil || document.Version != 1 || len(document.Models) == 0 || len(document.Models) > 1000 {
		return unavailable
	}
	catalog := fleetModelCatalog{Status: "available", UpdatedAt: opened.ModTime().UTC().Format(time.RFC3339)}
	seen := make(map[string]bool)
	for _, model := range document.Models {
		if model.ID == "" || len(model.ID) > 256 || strings.ContainsAny(model.ID, "\r\n\t ") || seen[model.ID] || len(model.Upstream) > 80 || len(model.Tier) > 40 {
			return unavailable
		}
		seen[model.ID] = true
		if strings.HasPrefix(model.ID, "direct/") {
			continue
		}
		catalog.Models = append(catalog.Models, model)
	}
	sort.Slice(catalog.Models, func(i, j int) bool { return catalog.Models[i].ID < catalog.Models[j].ID })
	return catalog
}

func (c fleetModelCatalog) lookup(model string) (string, string) {
	if c.Status != "available" || model == "" {
		return "unavailable", ""
	}
	for _, entry := range c.Models {
		if entry.ID == model {
			return "listed", entry.Upstream
		}
	}
	return "not_listed", ""
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func fleetBackendHarness(name string, def config.BackendDef) string {
	if args := strings.Fields(def.Cmd); len(args) > 0 {
		return filepath.Base(args[0])
	}
	return config.ResolveBackendKind(name, def.Provider, def.Cmd)
}

func fleetBackendCommandModel(def config.BackendDef) string {
	args := append(strings.Fields(def.Cmd), def.ExtraArgs...)
	var model string
	for i := 0; i < len(args); i++ {
		if args[i] == "--model" || args[i] == "-m" {
			if i+1 < len(args) {
				model = args[i+1]
				i++
			}
		} else if strings.HasPrefix(args[i], "--model=") {
			model = strings.TrimPrefix(args[i], "--model=")
		}
	}
	return model
}

func fleetBackendReferences(cfg *config.Config) map[string][]string {
	refs := make(map[string][]string)
	add := func(name, path string) {
		if name = strings.TrimSpace(name); name != "" {
			refs[name] = append(refs[name], path)
		}
	}
	add(cfg.Model.EffectiveDefault(), "model.default")
	for i, name := range cfg.Model.FallbackBackends {
		add(name, fmt.Sprintf("model.fallback_backends[%d]", i))
	}
	for i, lane := range cfg.Model.ProviderLanes {
		add(lane.Default, fmt.Sprintf("model.provider_lanes[%d].default", i))
		for j, name := range lane.FallbackBackends {
			add(name, fmt.Sprintf("model.provider_lanes[%d].fallback_backends[%d]", i, j))
		}
	}
	add(cfg.Supervisor.Backend, "supervisor.backend")
	add(cfg.Supervisor.ReviewRepair.Backend, "supervisor.review_repair.backend")
	if cfg.Routing.Mode == "auto" {
		add(cfg.Routing.RouterModel, "routing.router_model")
	}
	for key, name := range cfg.Routing.TaskTypeBackends {
		add(name, "routing.task_type_backends."+key)
	}
	for key, tier := range cfg.Routing.Tiers {
		add(tier.Backend, "routing.tiers."+key+".backend")
	}
	for name, role := range map[string]config.RoleConfig{"planner": cfg.Pipeline.Planner, "advisor": cfg.Pipeline.Advisor, "implementer": cfg.Pipeline.Implementer, "validator": cfg.Pipeline.Validator} {
		add(role.Backend, "pipeline."+name+".backend")
	}
	for _, paths := range refs {
		sort.Strings(paths)
	}
	return refs
}
