package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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
			Path:      "/srv/example-vault/Dev",
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

// Flipping a live row between forges (#1172) rebinds the transport the flow was
// built on at startup, so an EFFECTIVE forge-identity change is an identity
// change. Spelling out a default (kind: github, token_env: FORGEJO_TOKEN) is
// NOT a change — comparing the raw struct there would freeze hot reload for
// every unrelated edit on the row.
func TestIdentityChangedFlagsForgeChange(t *testing.T) {
	base := &config.Config{
		Repo:          "owner/svc",
		StateDir:      "/state/svc",
		SessionPrefix: "svc",
	}

	// Absent block vs an explicitly spelled-out GitHub default: no change.
	explicitGitHub := *base
	explicitGitHub.Forge = config.ForgeConfig{Kind: "github"}
	if identityChanged(base, &explicitGitHub) {
		t.Fatalf("spelling out kind: github (== the default) must stay hot-reloadable")
	}

	// A real kind flip to forgejo requires a restart.
	moved := *base
	moved.Forge = config.ForgeConfig{Kind: "forgejo", BaseURL: "https://forge.example.com"}
	if !identityChanged(base, &moved) {
		t.Fatalf("flipping a row from the GitHub default to forgejo must require a restart")
	}

	// Identical populated forge blocks on both sides are not a change.
	a := *base
	a.Forge = config.ForgeConfig{Kind: "forgejo", BaseURL: "https://forge.example.com", TokenEnv: "FORGEJO_TOKEN"}
	b := a
	if identityChanged(&a, &b) {
		t.Fatalf("equal forge blocks must not be flagged as an identity change")
	}

	// Spelling out the token_env default vs leaving it empty: no change.
	implicitToken := a
	implicitToken.Forge.TokenEnv = ""
	if identityChanged(&a, &implicitToken) {
		t.Fatalf("token_env: FORGEJO_TOKEN (== the default) vs empty must stay hot-reloadable")
	}

	// Re-pointing base_url rebinds the transport target.
	rehomed := a
	rehomed.Forge.BaseURL = "https://other.example.com"
	if !identityChanged(&a, &rehomed) {
		t.Fatalf("a forge.base_url change must require a restart")
	}

	// Re-pointing token_env to a NON-default name rebinds credentials.
	c := b
	c.Forge.TokenEnv = "OTHER_TOKEN"
	if !identityChanged(&b, &c) {
		t.Fatalf("a forge.token_env change to a non-default name must require a restart")
	}
}

// A real store-watch metadata edit must reach the live Fleet API without
// restarting the flow. This covers the full store -> watcher -> holder/dashboard
// path, not only the identityChanged predicate.
func TestWatchStoreProjectMetadataReloadUpdatesFleetWithoutRestart(t *testing.T) {
	store := newFakeWatchStore()
	cfg := testConfig(t, "owner/alpha")
	cfg.ProjectID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	cfg.ManagementHome = config.ManagementHomeConfig{
		Kind: "obsidian", Path: "/vault/Dev", Vault: "Vault", VaultPath: "Dev/Areas/alpha",
	}
	store.Set("alpha", cfg)

	var run, sup loopTracker
	d := newWatchDaemon(store, run.loop, sup.superviseLoop)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	waitForNames(t, d, "alpha")
	waitFor(t, func() bool {
		id, vaultPath := fleetProjectIdentityMetadata(t, d, "alpha")
		return id == cfg.ProjectID && vaultPath == "Dev/Areas/alpha"
	})
	waitFor(t, func() bool { return atomic.LoadInt64(&run.started) == 1 })

	edited := *cfg
	edited.ManagementHome = cfg.ManagementHome
	edited.ManagementHome.VaultPath = "Dev/Areas/alpha-renamed"
	store.Set("alpha", &edited)

	waitFor(t, func() bool {
		id, vaultPath := fleetProjectIdentityMetadata(t, d, "alpha")
		return id == cfg.ProjectID && vaultPath == "Dev/Areas/alpha-renamed"
	})
	if got := atomic.LoadInt64(&run.started); got != 1 {
		t.Fatalf("run loops started = %d, want 1 (metadata edit must not restart flow)", got)
	}
	if got := atomic.LoadInt64(&run.stopped); got != 0 {
		t.Fatalf("run loops stopped = %d before shutdown, want 0", got)
	}
	d.mu.Lock()
	flows := len(d.flows)
	d.mu.Unlock()
	if flows != 1 {
		t.Fatalf("flows = %d, want 1 after metadata-only reload", flows)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}

func fleetProjectIdentityMetadata(t *testing.T, d *Daemon, name string) (string, string) {
	t.Helper()
	fleet := waitForFleet(t, d)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	rec := httptest.NewRecorder()
	fleet.HandlerForTest().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/fleet = %d, want 200", rec.Code)
	}
	var resp struct {
		Projects []struct {
			Name           string `json:"name"`
			ProjectID      string `json:"project_id"`
			ManagementHome struct {
				VaultPath string `json:"vault_path"`
			} `json:"management_home"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode fleet response: %v", err)
	}
	for _, project := range resp.Projects {
		if project.Name == name {
			return project.ProjectID, project.ManagementHome.VaultPath
		}
	}
	return "", ""
}
