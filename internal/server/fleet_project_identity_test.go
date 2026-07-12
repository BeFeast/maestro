package server

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
)

// The Fleet API project payload surfaces the stable project_id and the
// descriptive management_home block (#869) without leaking file contents.
func TestProjectSnapshotExposesProjectIdentity(t *testing.T) {
	cfg := &config.Config{
		Repo:      "owner/svc",
		StateDir:  t.TempDir(),
		ProjectID: "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		ManagementHome: config.ManagementHomeConfig{
			Kind:      "obsidian",
			Path:      "/home/god/Obsidian/Dev",
			Vault:     "Obsidian Vault",
			VaultPath: "Dev/Areas/maestro",
		},
	}
	proj := NewFleetProject("svc", filepath.Join(t.TempDir(), "svc.yaml"), "", cfg)
	fleet := NewFleet([]FleetProject{proj}, "127.0.0.1", 0, false)

	item, _ := fleet.projectSnapshot(proj, time.Now())
	if item.ProjectID != "3f2504e0-4f89-41d3-9a0c-0305e82c3301" {
		t.Fatalf("payload project_id = %q", item.ProjectID)
	}
	if item.ManagementHome == nil {
		t.Fatalf("payload management_home should be present")
	}
	if item.ManagementHome.Kind != "obsidian" || item.ManagementHome.VaultPath != "Dev/Areas/maestro" ||
		item.ManagementHome.Vault != "Obsidian Vault" || item.ManagementHome.Path != "/home/god/Obsidian/Dev" {
		t.Fatalf("management_home fields not surfaced verbatim: %+v", item.ManagementHome)
	}

	// The JSON projection carries the fields under stable snake_case keys.
	blob, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(blob)
	for _, want := range []string{
		`"project_id":"3f2504e0-4f89-41d3-9a0c-0305e82c3301"`,
		`"management_home":`,
		`"vault_path":"Dev/Areas/maestro"`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("payload JSON missing %s:\n%s", want, js)
		}
	}
}

// A legacy project without the identity fields omits both from the payload, so
// existing SPA clients see no new noise (#869).
func TestProjectSnapshotOmitsIdentityForLegacyProject(t *testing.T) {
	cfg := &config.Config{Repo: "owner/legacy", StateDir: t.TempDir()}
	proj := NewFleetProject("legacy", "", "", cfg)
	fleet := NewFleet([]FleetProject{proj}, "127.0.0.1", 0, false)

	item, _ := fleet.projectSnapshot(proj, time.Now())
	if item.ProjectID != "" {
		t.Fatalf("legacy project_id should be empty, got %q", item.ProjectID)
	}
	if item.ManagementHome != nil {
		t.Fatalf("legacy management_home should be nil, got %+v", item.ManagementHome)
	}
	blob, _ := json.Marshal(item)
	if strings.Contains(string(blob), "management_home") {
		t.Fatalf("legacy payload should omit management_home:\n%s", blob)
	}
}
