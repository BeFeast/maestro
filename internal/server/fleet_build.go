package server

import (
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
	proj := NewFleetProject("", cfg.ResolvePath(), "", cfg)
	WireFleetProjectGitHub(&proj)
	return proj
}

// FleetProjectsFromConfigs builds one wired FleetProject per config.
func FleetProjectsFromConfigs(cfgs []*config.Config) []FleetProject {
	projects := make([]FleetProject, 0, len(cfgs))
	for _, cfg := range cfgs {
		projects = append(projects, NewFleetProjectWithGitHub(cfg))
	}
	return projects
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
