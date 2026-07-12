package supervisor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// TestProjectConfigPacketIncludesManagementHome verifies a configured project's
// stable id and Management Home (vault-relative + absolute path) plus the fixed
// PM-vs-executable boundary statement reach the supervisor project packet (#870).
func TestProjectConfigPacketIncludesManagementHome(t *testing.T) {
	cfg := testConfig(t)
	cfg.ProjectID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	cfg.ManagementHome = config.ManagementHomeConfig{
		Kind:      config.ManagementHomeKindObsidian,
		Path:      "/srv/example-vault/Dev/Areas/maestro",
		Vault:     "god",
		VaultPath: "Dev/Areas/maestro",
	}
	e := testEngine(cfg, &fakeReader{})

	pkt := e.projectConfigPacket(&state.State{})

	if pkt.ProjectID != "3f2504e0-4f89-41d3-9a0c-0305e82c3301" {
		t.Fatalf("ProjectID = %q, want the configured UUID", pkt.ProjectID)
	}
	if pkt.ManagementHome == nil {
		t.Fatal("ManagementHome packet is nil for a configured project")
	}
	if pkt.ManagementHome.VaultPath != "Dev/Areas/maestro" {
		t.Fatalf("VaultPath = %q, want Dev/Areas/maestro", pkt.ManagementHome.VaultPath)
	}
	if pkt.ManagementHome.Path != "/srv/example-vault/Dev/Areas/maestro" {
		t.Fatalf("Path = %q, want the absolute execution-host path", pkt.ManagementHome.Path)
	}
	if pkt.ManagementHome.Boundary != config.ManagementHomeBoundary {
		t.Fatalf("Boundary = %q, want the shared boundary statement", pkt.ManagementHome.Boundary)
	}
}

func TestProjectConfigPacketOmitsManagementHomeWhenUnconfigured(t *testing.T) {
	cfg := testConfig(t)
	e := testEngine(cfg, &fakeReader{})

	pkt := e.projectConfigPacket(&state.State{})

	if pkt.ProjectID != "" {
		t.Fatalf("ProjectID = %q, want empty for a legacy project", pkt.ProjectID)
	}
	if pkt.ManagementHome != nil {
		t.Fatalf("ManagementHome = %+v, want nil for an unconfigured project", pkt.ManagementHome)
	}

	// A legacy project must not emit management_home / project_id keys at all.
	raw, err := json.Marshal(pkt)
	if err != nil {
		t.Fatalf("marshal packet: %v", err)
	}
	if strings.Contains(string(raw), "management_home") {
		t.Fatalf("legacy packet leaked a management_home key: %s", raw)
	}
	if strings.Contains(string(raw), "project_id") {
		t.Fatalf("legacy packet leaked a project_id key: %s", raw)
	}
}
