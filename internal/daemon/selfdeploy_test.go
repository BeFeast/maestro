package daemon

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/selfdeploy"
)

func selfDeployCfg(t *testing.T, repo string) *config.Config {
	t.Helper()
	return &config.Config{
		Repo:      repo,
		LocalPath: t.TempDir(),
		StateDir:  t.TempDir(),
		SelfDeploy: config.SelfDeployConfig{
			Enabled:            true,
			MinIntervalMinutes: 30,
		},
	}
}

// #758 acceptance criterion: two near-simultaneous merges in DIFFERENT projects
// must launch exactly ONE self-deploy (centralized debounce), not one per flow.
func TestRequestSelfDeployDebouncesAcrossFlows(t *testing.T) {
	var launches int64
	d := New(fakeLoader{}, Options{Port: 0, SelfDeployStateDir: t.TempDir()})
	d.selfDeployTrigger = func(cfg *config.Config, prNumber int) error {
		atomic.AddInt64(&launches, 1)
		return nil
	}

	cfgA := selfDeployCfg(t, "owner/alpha")
	cfgB := selfDeployCfg(t, "owner/beta")

	// Two flows merge PRs at the same instant.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = d.RequestSelfDeploy(cfgA, 100) }()
	go func() { defer wg.Done(); errs[1] = d.RequestSelfDeploy(cfgB, 200) }()
	wg.Wait()

	if got := atomic.LoadInt64(&launches); got != 1 {
		t.Fatalf("self-deploy launched %d times, want exactly 1 (cross-flow debounce)", got)
	}
	// Exactly one request deployed (nil); the other was debounced.
	debounced, deployed := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			deployed++
		case errors.Is(e, selfdeploy.ErrDebounced):
			debounced++
		default:
			t.Fatalf("unexpected RequestSelfDeploy error: %v", e)
		}
	}
	if deployed != 1 || debounced != 1 {
		t.Fatalf("deployed=%d debounced=%d, want 1 and 1", deployed, debounced)
	}
}

// The centralized deploy restarts exactly ONE unit: with no units configured the
// effective set defaults to the single maestro.service (#758).
func TestRequestSelfDeployRestartsExactlyOneUnit(t *testing.T) {
	var gotUnits []string
	d := New(fakeLoader{}, Options{Port: 0, SelfDeployStateDir: t.TempDir()})
	d.selfDeployTrigger = func(cfg *config.Config, prNumber int) error {
		gotUnits = cfg.SelfDeploy.EffectiveUnits()
		return nil
	}

	if err := d.RequestSelfDeploy(selfDeployCfg(t, "owner/alpha"), 100); err != nil {
		t.Fatalf("RequestSelfDeploy: %v", err)
	}
	if len(gotUnits) != 1 || gotUnits[0] != "maestro.service" {
		t.Fatalf("restarted units = %v, want exactly [maestro.service]", gotUnits)
	}
}

// The post-restart health probe is routed through the single fleet endpoint so
// verify hits one :port (the daemon serves no per-project HTTP server), and the
// fleet snapshot carries the version field the script SHA-matches (#758/#698).
func TestSelfDeployConfigRoutesHealthToFleetEndpoint(t *testing.T) {
	d := New(fakeLoader{}, Options{Host: "127.0.0.1", Port: 8786})
	dc := d.selfDeployConfig(&config.Config{
		Repo:       "owner/alpha",
		Server:     config.ServerConfig{Auth: config.ServerAuthConfig{TokenEnv: "MAESTRO_DASH_TOKEN"}},
		SelfDeploy: config.SelfDeployConfig{Enabled: true},
	})
	wantURL := "http://127.0.0.1:8786/api/v1/fleet"
	if got := dc.SelfDeploy.EffectiveHealthURL(dc.Server); got != wantURL {
		t.Fatalf("health URL = %q, want %q (single fleet endpoint)", got, wantURL)
	}
	if got := dc.SelfDeploy.HealthTokenEnv; got != "MAESTRO_DASH_TOKEN" {
		t.Fatalf("health token env = %q, want carried from server auth", got)
	}
}

// An explicit self_deploy.health_url is respected — the daemon does not clobber
// an operator-set probe target with the fleet endpoint.
func TestSelfDeployConfigKeepsExplicitHealthURL(t *testing.T) {
	d := New(fakeLoader{}, Options{Host: "127.0.0.1", Port: 8786})
	dc := d.selfDeployConfig(&config.Config{
		Repo:       "owner/alpha",
		SelfDeploy: config.SelfDeployConfig{Enabled: true, HealthURL: "http://10.0.0.1:9000/custom"},
	})
	if got := dc.SelfDeploy.HealthURL; got != "http://10.0.0.1:9000/custom" {
		t.Fatalf("health URL = %q, want the explicit value preserved", got)
	}
}

// Outside the debounce window the next merge deploys again — the centralized
// debounce gates a wave, it does not permanently suppress deploys.
func TestRequestSelfDeployFiresAfterWindow(t *testing.T) {
	var launches int64
	d := New(fakeLoader{}, Options{Port: 0, SelfDeployStateDir: t.TempDir()})
	d.selfDeployTrigger = func(cfg *config.Config, prNumber int) error {
		atomic.AddInt64(&launches, 1)
		return nil
	}
	cfg := selfDeployCfg(t, "owner/alpha")
	cfg.SelfDeploy.MinIntervalMinutes = 1

	if err := d.RequestSelfDeploy(cfg, 100); err != nil {
		t.Fatalf("first RequestSelfDeploy: %v", err)
	}
	// Push the last-trigger past the 1-minute window (both markers).
	past := time.Now().UTC().Add(-10 * time.Minute)
	d.selfDeployLast = past
	if err := selfdeploy.RecordTrigger(d.opts.SelfDeployStateDir, 100, past); err != nil {
		t.Fatal(err)
	}

	if err := d.RequestSelfDeploy(cfg, 200); err != nil {
		t.Fatalf("second RequestSelfDeploy after window: %v", err)
	}
	if got := atomic.LoadInt64(&launches); got != 2 {
		t.Fatalf("launches = %d, want 2 (a deploy fires again past the window)", got)
	}
}

// A launcher failure (the detached unit never started) must NOT record the
// debounce marker, so the next merge retries instead of being suppressed.
func TestRequestSelfDeployLauncherFailureNotMarked(t *testing.T) {
	d := New(fakeLoader{}, Options{Port: 0, SelfDeployStateDir: t.TempDir()})
	d.selfDeployTrigger = func(cfg *config.Config, prNumber int) error {
		return errors.New("systemd-run: exit status 1")
	}

	err := d.RequestSelfDeploy(selfDeployCfg(t, "owner/alpha"), 100)
	if err == nil || errors.Is(err, selfdeploy.ErrDebounced) {
		t.Fatalf("RequestSelfDeploy err = %v, want a launcher failure", err)
	}
	if !d.selfDeployLast.IsZero() {
		t.Error("in-memory marker recorded despite launcher failure")
	}
	if _, _, ok := selfdeploy.LastTrigger(d.opts.SelfDeployStateDir); ok {
		t.Error("on-disk marker recorded despite launcher failure — next merge would be debounced")
	}
}
