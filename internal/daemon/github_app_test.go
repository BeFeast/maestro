package daemon

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/server"
)

// #823: with no project carrying a github_app block, the daemon leaves the
// process on the PAT/gh path — byte-identical to today.
func TestConfigureFleetGitHubAppAuth_NoConfigStaysPAT(t *testing.T) {
	projects := []server.FleetProject{
		server.NewFleetProjectWithGitHub(&config.Config{Repo: "owner/a"}),
		server.NewFleetProjectWithGitHub(&config.Config{Repo: "owner/b"}),
	}
	configureFleetGitHubAppAuth(projects)
	if got := github.GetAuthInfo().Mode; got != github.AuthModePAT {
		t.Fatalf("auth mode = %q, want pat when no project configures github_app", got)
	}
}

// A project with a github_app block pointing at a missing key file must NOT
// abort startup: ConfigureAppAuth fails, the helper logs and returns, and the
// process stays on PAT.
func TestConfigureFleetGitHubAppAuth_BadKeyDegradesToPAT(t *testing.T) {
	cfg := &config.Config{
		Repo: "owner/a",
		GitHubApp: config.GitHubAppConfig{
			AppID:          1,
			InstallationID: 2,
			PrivateKeyPath: "/nonexistent/app-key.pem",
		},
	}
	projects := []server.FleetProject{server.NewFleetProjectWithGitHub(cfg)}
	// Must not panic or block; must leave auth on PAT.
	configureFleetGitHubAppAuth(projects)
	if got := github.GetAuthInfo().Mode; got != github.AuthModePAT {
		t.Fatalf("auth mode = %q, want pat after a failed app-auth setup", got)
	}
}

// Configured() gates which project arms the singleton — a partial block is
// ignored so a half-filled github_app never silently disables the PAT path.
func TestConfigureFleetGitHubAppAuth_PartialConfigIgnored(t *testing.T) {
	cfg := &config.Config{
		Repo:      "owner/a",
		GitHubApp: config.GitHubAppConfig{AppID: 1}, // missing key path + installation
	}
	if cfg.GitHubApp.Configured() {
		t.Fatal("partial github_app should not be Configured()")
	}
	projects := []server.FleetProject{server.NewFleetProjectWithGitHub(cfg)}
	configureFleetGitHubAppAuth(projects)
	if got := github.GetAuthInfo().Mode; got != github.AuthModePAT {
		t.Fatalf("auth mode = %q, want pat for a partial github_app block", got)
	}
}

// Guard the log wording the runbook references so a future rename does not
// silently drop the operator's "which bucket am I on" signal.
func TestConfigureFleetGitHubAppAuth_LogsPATWhenUnconfigured(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	configureFleetGitHubAppAuth([]server.FleetProject{
		server.NewFleetProjectWithGitHub(&config.Config{Repo: "owner/a"}),
	})
	if !strings.Contains(buf.String(), "github app auth not configured") {
		t.Fatalf("expected PAT log line, got: %s", buf.String())
	}
}
