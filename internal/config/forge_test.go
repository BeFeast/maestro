package config

import (
	"strings"
	"testing"
)

// Backward compat (#1172 M1): every existing row has no forge block, so an
// absent block must parse unchanged and resolve to the GitHub default.
func TestParse_AbsentForgeBlockDefaultsToGitHub(t *testing.T) {
	cfg, err := Parse([]byte("repo: owner/repo\n"))
	if err != nil {
		t.Fatalf("legacy config without forge block must parse: %v", err)
	}
	if got := cfg.Forge.EffectiveKind(); got != ForgeKindGitHub {
		t.Fatalf("EffectiveKind() = %q, want %q", got, ForgeKindGitHub)
	}
	if cfg.Forge.IsForgejo() {
		t.Fatalf("IsForgejo() should be false without a forge block")
	}
	if got := cfg.Forge.APIRoot(); got != "" {
		t.Fatalf("APIRoot() = %q, want empty for the GitHub default", got)
	}
}

func TestParse_ForgeExplicitGitHubKindAccepted(t *testing.T) {
	cfg, err := Parse([]byte("repo: o/r\nforge:\n  kind: github\n"))
	if err != nil {
		t.Fatalf("explicit github kind should parse: %v", err)
	}
	if cfg.Forge.IsForgejo() || cfg.Forge.EffectiveKind() != ForgeKindGitHub {
		t.Fatalf("explicit github kind should resolve to the GitHub default, got %+v", cfg.Forge)
	}
}

func TestParse_ForgejoValidBlockAccepted(t *testing.T) {
	yamlDoc := "repo: o/r\nforge:\n  kind: forgejo\n  base_url: https://forge.example.com/\n"
	cfg, err := Parse([]byte(yamlDoc))
	if err != nil {
		t.Fatalf("valid forgejo block should parse: %v", err)
	}
	if !cfg.Forge.IsForgejo() {
		t.Fatalf("IsForgejo() should be true, got %+v", cfg.Forge)
	}
	if got, want := cfg.Forge.APIRoot(), "https://forge.example.com/api/v1"; got != want {
		t.Fatalf("APIRoot() = %q, want %q (trailing slash trimmed, /api/v1 appended)", got, want)
	}
	if got := cfg.Forge.EffectiveTokenEnv(); got != "FORGEJO_TOKEN" {
		t.Fatalf("EffectiveTokenEnv() default = %q, want FORGEJO_TOKEN", got)
	}
}

func TestParse_ForgejoTokenEnvOverride(t *testing.T) {
	yamlDoc := "repo: o/r\nforge:\n  kind: forgejo\n  base_url: https://forge.example.com\n  token_env: MY_FORGE_PAT\n"
	cfg, err := Parse([]byte(yamlDoc))
	if err != nil {
		t.Fatalf("forgejo block with token_env should parse: %v", err)
	}
	if got := cfg.Forge.EffectiveTokenEnv(); got != "MY_FORGE_PAT" {
		t.Fatalf("EffectiveTokenEnv() = %q, want the MY_FORGE_PAT override", got)
	}
}

func TestParse_ForgeValidation(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name:    "unknown kind names allowed set",
			yaml:    "repo: o/r\nforge:\n  kind: gitlab\n",
			wantSub: `forge.kind "gitlab" is not supported (want "github", "forgejo"`,
		},
		{
			name:    "case-folded kind rejected (exact match only)",
			yaml:    "repo: o/r\nforge:\n  kind: Forgejo\n",
			wantSub: `forge.kind "Forgejo" is not supported`,
		},
		{
			name:    "forgejo without base_url",
			yaml:    "repo: o/r\nforge:\n  kind: forgejo\n",
			wantSub: "forge.base_url is required",
		},
		{
			name:    "base_url ending in /api/v1",
			yaml:    "repo: o/r\nforge:\n  kind: forgejo\n  base_url: https://forge.example.com/api/v1\n",
			wantSub: "/api/v1 is appended automatically",
		},
		{
			name:    "base_url ending in /api/v1/ (trailing slash)",
			yaml:    "repo: o/r\nforge:\n  kind: forgejo\n  base_url: https://forge.example.com/api/v1/\n",
			wantSub: "/api/v1 is appended automatically",
		},
		{
			name:    "base_url with query",
			yaml:    "repo: o/r\nforge:\n  kind: forgejo\n  base_url: https://forge.example.com/?x=1\n",
			wantSub: "must not contain a query or fragment",
		},
		{
			name:    "base_url with fragment",
			yaml:    "repo: o/r\nforge:\n  kind: forgejo\n  base_url: 'https://forge.example.com/#frag'\n",
			wantSub: "must not contain a query or fragment",
		},
		{
			name:    "base_url with bad scheme",
			yaml:    "repo: o/r\nforge:\n  kind: forgejo\n  base_url: ssh://forge.example.com\n",
			wantSub: "must use an http or https scheme",
		},
		{
			name:    "base_url without host",
			yaml:    "repo: o/r\nforge:\n  kind: forgejo\n  base_url: https:///path\n",
			wantSub: "must include a host",
		},
		{
			name:    "base_url with userinfo credentials",
			yaml:    "repo: o/r\nforge:\n  kind: forgejo\n  base_url: https://user:secret@forge.example.com\n",
			wantSub: "must not contain userinfo",
		},
		{
			name:    "base_url with dot-dot segment defeating the api/v1 guard",
			yaml:    "repo: o/r\nforge:\n  kind: forgejo\n  base_url: https://forge.example.com/api/v1/x/..\n",
			wantSub: "'.' or '..' path segments",
		},
		{
			name:    "base_url with dot segment",
			yaml:    "repo: o/r\nforge:\n  kind: forgejo\n  base_url: https://forge.example.com/./api\n",
			wantSub: "'.' or '..' path segments",
		},
		{
			name:    "github kind with base_url",
			yaml:    "repo: o/r\nforge:\n  kind: github\n  base_url: https://forge.example.com\n",
			wantSub: `forge.base_url is only valid with forge.kind "forgejo"`,
		},
		{
			name:    "empty kind with base_url",
			yaml:    "repo: o/r\nforge:\n  base_url: https://forge.example.com\n",
			wantSub: `forge.base_url is only valid with forge.kind "forgejo"`,
		},
		{
			name:    "github kind with token_env",
			yaml:    "repo: o/r\nforge:\n  kind: github\n  token_env: FORGEJO_TOKEN\n",
			wantSub: `forge.token_env is only valid with forge.kind "forgejo"`,
		},
		{
			name:    "token_env with space",
			yaml:    "repo: o/r\nforge:\n  kind: forgejo\n  base_url: https://forge.example.com\n  token_env: 'MY TOKEN'\n",
			wantSub: "must be an env var NAME",
		},
		{
			name:    "token_env with equals",
			yaml:    "repo: o/r\nforge:\n  kind: forgejo\n  base_url: https://forge.example.com\n  token_env: 'FORGEJO_TOKEN=abc'\n",
			wantSub: "must be an env var NAME",
		},
		{
			name:    "forgejo with github_projects enabled",
			yaml:    "repo: o/r\nforge:\n  kind: forgejo\n  base_url: https://forge.example.com\ngithub_projects:\n  enabled: true\n",
			wantSub: "no Forgejo equivalent",
		},
		{
			// #1172 M2: the mirror store is fed by GitHub webhooks and keyed by
			// bare owner/name — on a mirrored repo, mirror-first reads on a
			// forgejo row would silently serve the GitHub mirror's state.
			name:    "forgejo with mirror-first github_mirror",
			yaml:    "repo: o/r\nforge:\n  kind: forgejo\n  base_url: https://forge.example.com\ngithub_mirror:\n  source: mirror-first\n",
			wantSub: "fed by GitHub webhooks",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q should contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// The GitHub Projects rejection is a forgejo-only rule: a config with NO forge
// block and an enabled github_projects integration is every existing GitHub
// row with a Projects board — it must keep parsing unchanged. A leak of that
// check out of the forgejo branch would brick all of them at once.
func TestParse_GitHubProjectsEnabledWithoutForgeBlockAccepted(t *testing.T) {
	yamlDoc := "repo: o/r\ngithub_projects:\n  enabled: true\n  project_number: 5\n"
	cfg, err := Parse([]byte(yamlDoc))
	if err != nil {
		t.Fatalf("github_projects on a forge-less (GitHub default) row must parse: %v", err)
	}
	if !cfg.GitHubProjects.Enabled || cfg.GitHubProjects.ProjectNumber != 5 {
		t.Fatalf("github_projects not parsed verbatim: %+v", cfg.GitHubProjects)
	}
}

func TestParse_ForgejoWithDisabledGitHubProjectsAccepted(t *testing.T) {
	yamlDoc := "repo: o/r\nforge:\n  kind: forgejo\n  base_url: https://forge.example.com\ngithub_projects:\n  enabled: false\n"
	if _, err := Parse([]byte(yamlDoc)); err != nil {
		t.Fatalf("forgejo with explicitly disabled github_projects should parse: %v", err)
	}
}

// The mirror-first rejection is scoped to the SOURCE selector: a forgejo row
// with an explicit api-direct source (or a bare github_mirror block tuning
// reconcile cadence) must keep parsing — only mirror-served reads are the
// cross-forge hazard.
func TestParse_ForgejoWithAPIDirectMirrorAccepted(t *testing.T) {
	yamlDoc := "repo: o/r\nforge:\n  kind: forgejo\n  base_url: https://forge.example.com\ngithub_mirror:\n  source: api\n"
	if _, err := Parse([]byte(yamlDoc)); err != nil {
		t.Fatalf("forgejo with api-direct github_mirror should parse: %v", err)
	}
}

// The strict write path must accept the new forge block by name (KnownFields),
// while still rejecting a misspelled child key inside it.
func TestParseStrict_AcceptsForgeBlock(t *testing.T) {
	yamlDoc := "repo: o/r\nforge:\n  kind: forgejo\n  base_url: https://forge.example.com\n  token_env: FORGEJO_TOKEN\n"
	if _, err := ParseStrict([]byte(yamlDoc)); err != nil {
		t.Fatalf("strict decode of a valid forge block should pass: %v", err)
	}
	bad := "repo: o/r\nforge:\n  kind: forgejo\n  base_url: https://forge.example.com\n  token_envv: typo\n"
	if _, err := ParseStrict([]byte(bad)); err == nil || !strings.Contains(err.Error(), "token_envv") {
		t.Fatalf("strict decode should name the misspelled forge child key, got: %v", err)
	}
}

// #1172 M3 — the forge-aware web-URL helpers. GitHub and Forgejo diverge on
// the PR path segment (/pull/N vs /pulls/N), which is exactly why human links
// must come from these helpers instead of hardcoded github.com shapes.
func TestForgeConfigWebURLs(t *testing.T) {
	cases := []struct {
		name      string
		fc        ForgeConfig
		wantIssue string
		wantPR    string
	}{
		{
			name:      "zero config (github default)",
			fc:        ForgeConfig{},
			wantIssue: "https://github.com/o/r/issues/7",
			wantPR:    "https://github.com/o/r/pull/7",
		},
		{
			name:      "explicit github kind",
			fc:        ForgeConfig{Kind: ForgeKindGitHub},
			wantIssue: "https://github.com/o/r/issues/7",
			wantPR:    "https://github.com/o/r/pull/7",
		},
		{
			name:      "forgejo",
			fc:        ForgeConfig{Kind: ForgeKindForgejo, BaseURL: "https://forge.example.com"},
			wantIssue: "https://forge.example.com/o/r/issues/7",
			wantPR:    "https://forge.example.com/o/r/pulls/7",
		},
		{
			name:      "forgejo trailing slash trimmed",
			fc:        ForgeConfig{Kind: ForgeKindForgejo, BaseURL: "https://forge.example.com/"},
			wantIssue: "https://forge.example.com/o/r/issues/7",
			wantPR:    "https://forge.example.com/o/r/pulls/7",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fc.IssueWebURL("o/r", 7); got != tc.wantIssue {
				t.Errorf("IssueWebURL = %q, want %q", got, tc.wantIssue)
			}
			if got := tc.fc.PRWebURL("o/r", 7); got != tc.wantPR {
				t.Errorf("PRWebURL = %q, want %q", got, tc.wantPR)
			}
		})
	}
}
