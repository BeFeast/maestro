package server

import (
	"fmt"
	"strings"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
)

// WireFleetProjectGitHub attaches the per-project safe-action GitHub client and,
// when github_projects is enabled, the board client (#529). It is the single
// place the GitHub wiring lives, shared by every fleet entry point — `maestro
// serve --fleet`, serve fleet-from-configs, and `maestro daemon` — so a fix to
// token precedence (#487) or board wiring applies to all of them, not just one
// copy (#764).
func WireFleetProjectGitHub(proj *FleetProject) {
	if proj == nil {
		return
	}
	cfg := proj.Cfg()
	if cfg == nil {
		return
	}
	gh := github.New(cfg.Repo)
	proj.SetActionGH(gh)
	if cfg.GitHubProjects.Enabled && cfg.GitHubProjects.ProjectNumber > 0 {
		proj.SetBoardClient(gh, cfg.GitHubProjects.ProjectNumber)
	}
}

// NewFleetProjectWithGitHub builds a FleetProject for cfg (name derived from the
// repo basename) with its safe-action GitHub client and board client wired in.
// Shared by serve --fleet (one repo per config) and the daemon (one flow per
// config) so the construction lives in one place (#764).
func NewFleetProjectWithGitHub(cfg *config.Config) FleetProject {
	return NewFleetProjectWithGitHubNamed("", cfg)
}

// NewFleetProjectWithGitHubNamed is NewFleetProjectWithGitHub with an explicit
// fleet display name. An empty name falls back to the repo basename (the
// historical default). Callers that aggregate several configs pass a
// disambiguated name (see UniqueFleetName) so distinct repos that share a
// basename do not collide on Project.Name (#764).
func NewFleetProjectWithGitHubNamed(name string, cfg *config.Config) FleetProject {
	proj := NewFleetProject(name, cfg.ResolvePath(), "", cfg)
	WireFleetProjectGitHub(&proj)
	return proj
}

// FleetProjectsFromConfigs builds one wired FleetProject per config, assigning
// every project a fleet display name that is unique across the slice (#764) so
// the aggregating FleetServer — whose findProject keys on Project.Name — can
// address each one unambiguously even when two repos share a basename.
func FleetProjectsFromConfigs(cfgs []*config.Config) []FleetProject {
	projects := make([]FleetProject, 0, len(cfgs))
	taken := make(map[string]bool, len(cfgs))
	for _, cfg := range cfgs {
		name := UniqueFleetName(cfgRepo(cfg), taken)
		projects = append(projects, NewFleetProjectWithGitHubNamed(name, cfg))
	}
	return projects
}

// UniqueFleetName returns a fleet display name for repo that does not collide
// with any name already recorded in taken, and records the chosen name back
// into taken. It starts from the repo basename (the default display name) and,
// on a collision between distinct repos that share a basename (org-a/api vs
// org-b/api), disambiguates with the full owner-repo slug and then a numeric
// suffix.
//
// The fleet server addresses project-scoped routes (`/project/<name>`), worker
// details, audit logging, and safe actions by Project.Name — findProject
// returns the first exact match — so two projects sharing a Name would silently
// route the second repo's traffic to the first. Every project in one fleet must
// therefore carry a distinct Name (#764).
func UniqueFleetName(repo string, taken map[string]bool) string {
	base := defaultFleetProjectName(repo)
	if !taken[base] {
		taken[base] = true
		return base
	}
	// First fallback: the full owner-repo slug, which stays human-recognizable
	// in the dashboard ("org-b-api") instead of a bare counter.
	if slug := repoSlug(repo); slug != "" && slug != base && !taken[slug] {
		taken[slug] = true
		return slug
	}
	// Last resort: a numeric suffix on the basename.
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !taken[candidate] {
			taken[candidate] = true
			return candidate
		}
	}
}

// repoSlug renders owner/repo as a single path-safe token (owner-repo). Empty
// when repo is blank.
func repoSlug(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return ""
	}
	return strings.ReplaceAll(repo, "/", "-")
}

// cfgRepo safely reads cfg.Repo, tolerating a nil config.
func cfgRepo(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.Repo
}

// FleetAuthFromProjects returns the first non-empty Server.Auth config across
// the fleet. The fleet uses a single shared token (#487); per-project distinct
// tokens are intentionally out of scope.
func FleetAuthFromProjects(projects []FleetProject) config.ServerAuthConfig {
	for i := range projects {
		cfg := projects[i].Cfg()
		if cfg == nil {
			continue
		}
		if strings.TrimSpace(cfg.Server.Auth.TokenEnv) != "" {
			return cfg.Server.Auth
		}
	}
	return config.ServerAuthConfig{}
}
