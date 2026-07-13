package config

import (
	"strings"
	"testing"
)

// exampleProjectIdentityYAML mirrors the issue's example config (#869): a valid
// UUID plus a complete obsidian management_home block.
const exampleProjectIdentityYAML = `
repo: BeFeast/maestro
project_id: 3f2504e0-4f89-41d3-9a0c-0305e82c3301
management_home:
  kind: obsidian
  path: /srv/example-vault/Dev
  vault: Obsidian Vault
  vault_path: Dev/Areas/maestro
`

func TestParse_ProjectIdentityExampleRoundTrips(t *testing.T) {
	cfg, err := Parse([]byte(exampleProjectIdentityYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.ProjectID != "3f2504e0-4f89-41d3-9a0c-0305e82c3301" {
		t.Fatalf("project_id = %q, want the example UUID", cfg.ProjectID)
	}
	mh := cfg.ManagementHome
	if mh.Kind != "obsidian" || mh.Path != "/srv/example-vault/Dev" ||
		mh.Vault != "Obsidian Vault" || mh.VaultPath != "Dev/Areas/maestro" {
		t.Fatalf("management_home not parsed verbatim: %+v", mh)
	}
	if !mh.Configured() {
		t.Fatalf("Configured() should be true for a populated block")
	}
}

func TestParse_LegacyConfigWithoutIdentityParses(t *testing.T) {
	cfg, err := Parse([]byte("repo: owner/repo\n"))
	if err != nil {
		t.Fatalf("legacy config must still parse: %v", err)
	}
	if cfg.ProjectID != "" {
		t.Fatalf("legacy project_id should be empty, got %q", cfg.ProjectID)
	}
	if cfg.ManagementHome.Configured() {
		t.Fatalf("legacy management_home should be unconfigured, got %+v", cfg.ManagementHome)
	}
}

func TestParse_ProjectIDMustBeUUID(t *testing.T) {
	_, err := Parse([]byte("repo: owner/repo\nproject_id: not-a-uuid\n"))
	if err == nil {
		t.Fatalf("expected malformed project_id to be rejected")
	}
	if !strings.Contains(err.Error(), "project_id") || !strings.Contains(err.Error(), "UUID") {
		t.Fatalf("error should name project_id/UUID, got: %v", err)
	}
}

func TestParse_ManagementHomeValidation(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name:    "unknown kind",
			yaml:    "repo: o/r\nmanagement_home:\n  kind: notion\n  path: /x\n  vault: V\n  vault_path: a/b\n",
			wantSub: "kind",
		},
		{
			name:    "missing kind",
			yaml:    "repo: o/r\nmanagement_home:\n  path: /x\n  vault: V\n  vault_path: a/b\n",
			wantSub: "kind is required",
		},
		{
			name:    "empty path",
			yaml:    "repo: o/r\nmanagement_home:\n  kind: obsidian\n  vault: V\n  vault_path: a/b\n",
			wantSub: "path is required",
		},
		{
			name:    "missing vault",
			yaml:    "repo: o/r\nmanagement_home:\n  kind: obsidian\n  path: /x\n  vault_path: a/b\n",
			wantSub: "vault is required",
		},
		{
			name:    "missing vault_path",
			yaml:    "repo: o/r\nmanagement_home:\n  kind: obsidian\n  path: /x\n  vault: V\n",
			wantSub: "vault_path is required",
		},
		{
			name:    "absolute vault_path",
			yaml:    "repo: o/r\nmanagement_home:\n  kind: obsidian\n  path: /x\n  vault: V\n  vault_path: /abs/path\n",
			wantSub: "must be vault-relative",
		},
		{
			name:    "traversal vault_path",
			yaml:    "repo: o/r\nmanagement_home:\n  kind: obsidian\n  path: /x\n  vault: V\n  vault_path: Dev/../../etc\n",
			wantSub: "traversal",
		},
		{
			name:    "backslash traversal vault_path",
			yaml:    "repo: o/r\nmanagement_home:\n  kind: obsidian\n  path: /x\n  vault: V\n  vault_path: 'Dev\\..\\secret'\n",
			wantSub: "backslashes",
		},
		{
			name:    "windows absolute vault_path",
			yaml:    "repo: o/r\nmanagement_home:\n  kind: obsidian\n  path: /x\n  vault: V\n  vault_path: C:/Vault/Dev\n",
			wantSub: "vault-relative",
		},
		{
			name:    "non-normalized vault_path",
			yaml:    "repo: o/r\nmanagement_home:\n  kind: obsidian\n  path: /x\n  vault: V\n  vault_path: Dev//Areas/./foo/\n",
			wantSub: "normalized",
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

func TestParse_ManagementHomeValidBlockAccepted(t *testing.T) {
	valid := "repo: o/r\nmanagement_home:\n  kind: obsidian\n  path: /x\n  vault: V\n  vault_path: Dev/Areas/foo\n"
	if _, err := Parse([]byte(valid)); err != nil {
		t.Fatalf("valid management_home should parse: %v", err)
	}
}

func TestParseStrict_RejectsUnknownKeyByName(t *testing.T) {
	// A misspelled management_home child must be rejected with the exact name.
	bad := "repo: o/r\nmanagement_home:\n  kind: obsidian\n  path: /x\n  vault: V\n  vault_path: a/b\n  vault_pathh: typo\n"
	_, err := ParseStrict([]byte(bad))
	if err == nil {
		t.Fatalf("strict decode should reject the misspelled child key")
	}
	if !strings.Contains(err.Error(), "vault_pathh") {
		t.Fatalf("error should name the offending key, got: %v", err)
	}

	// A misspelled top-level key is likewise rejected.
	_, err = ParseStrict([]byte("repo: o/r\nproject_idd: 3f2504e0-4f89-41d3-9a0c-0305e82c3301\n"))
	if err == nil || !strings.Contains(err.Error(), "project_idd") {
		t.Fatalf("strict decode should name the misspelled top-level key, got: %v", err)
	}

	// SupervisorConfig has a custom UnmarshalYAML for legacy-read bookkeeping;
	// strict writes must still reject typos inside that subtree.
	_, err = ParseStrict([]byte("repo: o/r\nsupervisor:\n  ready_labell: typo\n"))
	if err == nil || !strings.Contains(err.Error(), "ready_labell") {
		t.Fatalf("strict decode should name the misspelled supervisor key, got: %v", err)
	}
}

func TestParseStrict_AcceptsKnownKeysAndLegacy(t *testing.T) {
	if _, err := ParseStrict([]byte(exampleProjectIdentityYAML)); err != nil {
		t.Fatalf("strict decode of the example config should pass: %v", err)
	}
	// Legacy config with no identity fields must still pass strict decode.
	if _, err := ParseStrict([]byte("repo: owner/repo\n")); err != nil {
		t.Fatalf("strict decode of a legacy config should pass: %v", err)
	}
}

func TestParse_TolerantReadKeepsLegacyUnknownKey(t *testing.T) {
	// The tolerant read path must still accept an unknown key (legacy rows) so a
	// stored config written before strict decode existed continues to load (#869).
	if _, err := Parse([]byte("repo: owner/repo\nsome_future_key: 1\n")); err != nil {
		t.Fatalf("tolerant Parse should ignore an unknown key, got: %v", err)
	}
	// While the strict write path rejects that same key by name.
	if _, err := ParseStrict([]byte("repo: owner/repo\nsome_future_key: 1\n")); err == nil {
		t.Fatalf("strict Parse should reject the unknown key")
	}
}
