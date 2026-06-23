package server

import (
	"testing"

	"github.com/befeast/maestro/internal/config"
)

// UniqueFleetName must keep the basename for the first claimant and disambiguate
// later collisions so the aggregating FleetServer can address every project by a
// distinct Project.Name (#764).
func TestUniqueFleetName(t *testing.T) {
	taken := map[string]bool{}

	if got := UniqueFleetName("org-a/api", taken); got != "api" {
		t.Fatalf("first org-a/api = %q, want %q", got, "api")
	}
	// Distinct repo, same basename → owner-repo slug.
	if got := UniqueFleetName("org-b/api", taken); got != "org-b-api" {
		t.Fatalf("colliding org-b/api = %q, want %q", got, "org-b-api")
	}
	// A third collider whose slug is also taken falls back to a numeric suffix.
	taken["org-c-api"] = true
	if got := UniqueFleetName("org-c/api", taken); got != "api-2" {
		t.Fatalf("third api collider = %q, want %q", got, "api-2")
	}
	// An unrelated basename is untouched.
	if got := UniqueFleetName("org-a/worker", taken); got != "worker" {
		t.Fatalf("org-a/worker = %q, want %q", got, "worker")
	}
}

// FleetProjectsFromConfigs must produce one project per config, each with a
// unique Name even when two configs share a repo basename.
func TestFleetProjectsFromConfigsUniqueNames(t *testing.T) {
	cfgs := []*config.Config{
		{Repo: "org-a/api"},
		{Repo: "org-b/api"},
		{Repo: "org-a/worker"},
	}
	projects := FleetProjectsFromConfigs(cfgs)
	if len(projects) != 3 {
		t.Fatalf("projects = %d, want 3", len(projects))
	}
	seen := map[string]bool{}
	for _, p := range projects {
		if p.Name == "" {
			t.Fatalf("project for repo %q has empty name", p.Cfg().Repo)
		}
		if seen[p.Name] {
			t.Fatalf("duplicate fleet name %q", p.Name)
		}
		seen[p.Name] = true
	}
}
