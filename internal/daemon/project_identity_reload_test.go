package daemon

import (
	"testing"

	"github.com/befeast/maestro/internal/config"
)

// A metadata-only edit — adding or changing project_id / management_home (#869)
// — must NOT be flagged as an identity change, so the store-watch reload pump
// applies it live (holder swap + Fleet API UpdateProjectConfig) instead of
// forcing a flow restart. Identity fields (repo/state_dir/session_prefix) still
// require a restart.
func TestIdentityChangedIgnoresProjectMetadata(t *testing.T) {
	base := &config.Config{
		Repo:          "owner/svc",
		StateDir:      "/state/svc",
		SessionPrefix: "svc",
	}
	withMetadata := &config.Config{
		Repo:          "owner/svc",
		StateDir:      "/state/svc",
		SessionPrefix: "svc",
		ProjectID:     "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		ManagementHome: config.ManagementHomeConfig{
			Kind:      "obsidian",
			Path:      "/home/god/Obsidian/Dev",
			Vault:     "Obsidian Vault",
			VaultPath: "Dev/Areas/maestro",
		},
	}

	if identityChanged(base, withMetadata) {
		t.Fatalf("adding project_id/management_home must be hot-reloadable, not an identity change")
	}

	// Changing only the management_home Area path is likewise metadata-only.
	moved := *withMetadata
	moved.ManagementHome.VaultPath = "Dev/Areas/renamed"
	if identityChanged(withMetadata, &moved) {
		t.Fatalf("changing management_home metadata must not require a restart")
	}

	// A real identity change (state_dir) still requires a restart.
	restartNeeded := *withMetadata
	restartNeeded.StateDir = "/state/other"
	if !identityChanged(withMetadata, &restartNeeded) {
		t.Fatalf("state_dir change must still be flagged as an identity change")
	}
}
