package daemon

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/selfdeploy"
)

// RequestSelfDeploy is the daemon's centralized post-merge self-deploy trigger
// (#758). Before this, every orchestrator flow called selfdeploy.Trigger
// directly and debounced on its OWN per-project StateDir marker — so N flows
// merging PRs near-simultaneously each fired a deploy, a thundering herd that
// restarts the maestro unit N times and bounces the fleet mid-verify (#722).
//
// The daemon collapses that to a single authority: each flow's orchestrator
// signals here (wired through Orchestrator.SetSelfDeployStartFn), and this
// method serializes on selfDeployMu and debounces on a SINGLE marker spanning
// every flow. The first request in a merge wave launches exactly ONE deploy of
// ONE unit; the rest return selfdeploy.ErrDebounced, which the orchestrator
// treats as a benign skip rather than a failure.
//
// The debounce reads the most-recent trigger from two sources: an in-memory
// timestamp (deterministic within this process, so two goroutines racing in
// here can never both deploy) and the on-disk marker in
// opts.SelfDeployStateDir (so a daemon restarted by its OWN deploy still
// debounces the wave that triggered it). cfg is the requesting flow's config,
// supplying the build inputs (local_path, self_deploy block, state_dir); the
// post-restart health probe is routed through the single fleet endpoint so
// verify hits one :port, not a per-project server the daemon no longer runs.
func (d *Daemon) RequestSelfDeploy(cfg *config.Config, prNumber int) error {
	if cfg == nil {
		return fmt.Errorf("self-deploy: nil config")
	}

	d.selfDeployMu.Lock()
	defer d.selfDeployMu.Unlock()

	now := time.Now().UTC()
	window := time.Duration(cfg.SelfDeploy.EffectiveMinIntervalMinutes()) * time.Minute

	// Most-recent trigger across the in-memory marker (this process) and the
	// shared on-disk marker. Either one inside the window debounces.
	last, lastPR := d.selfDeployLast, d.selfDeployLastPR
	if t, pr, ok := selfdeploy.LastTrigger(d.opts.SelfDeployStateDir); ok && t.After(last) {
		last, lastPR = t, pr
	}
	if !last.IsZero() {
		if since := now.Sub(last); since >= 0 && since < window {
			log.Printf("[daemon] self-deploy debounced for PR #%d: last trigger (PR #%d) was %s ago (< %s window) — one deploy per merge wave", prNumber, lastPR, since.Round(time.Second), window)
			return selfdeploy.ErrDebounced
		}
	}

	if err := d.selfDeployTrigger(d.selfDeployConfig(cfg), prNumber); err != nil {
		return err
	}

	// Record on BOTH markers only after a successful launch, so a pure launcher
	// failure (systemd-run rejected the unit) does not suppress the next attempt.
	d.selfDeployLast, d.selfDeployLastPR = now, prNumber
	if err := selfdeploy.RecordTrigger(d.opts.SelfDeployStateDir, prNumber, now); err != nil {
		log.Printf("[daemon] self-deploy shared trigger marker write failed for PR #%d: %v", prNumber, err)
	}
	log.Printf("[daemon] self-deploy launched for PR #%d — one unit, single deploy debounced across flows", prNumber)
	return nil
}

// selfDeployConfig builds the config handed to the launcher from the requesting
// flow's config, overriding only what the centralized single-service model
// demands (#758):
//
//   - health probe routed to the SINGLE fleet endpoint (:port/api/v1/fleet,
//     which carries the running binary's version field) so verify hits one
//     endpoint. The daemon serves no per-project HTTP server, so the script's
//     default <server.port>/api/v1/state target would be a dead URL — we both
//     set the fleet URL and zero the requesting flow's Server.Port so the
//     derive-from-server fallback cannot resurface it. An explicit
//     self_deploy.health_url is respected; with no fleet port bound (port 0)
//     the probe is left empty for CLI-only verify.
//
// Units are left to config.EffectiveUnits, which already defaults to the single
// ["maestro.service"], so the restart touches exactly one unit. The shallow
// copy keeps the flow's own config untouched.
func (d *Daemon) selfDeployConfig(cfg *config.Config) *config.Config {
	dc := *cfg
	sd := cfg.SelfDeploy
	if strings.TrimSpace(sd.HealthURL) == "" {
		sd.HealthURL = d.fleetHealthURL() // "" when no fleet port is bound
		dc.Server.Port = 0                // never derive a dead per-project URL
		// Carry the auth token env so the probe authenticates against the single
		// endpoint, defaulting to the flow's own server auth env.
		if sd.HealthURL != "" && strings.TrimSpace(sd.HealthTokenEnv) == "" {
			sd.HealthTokenEnv = strings.TrimSpace(cfg.Server.Auth.TokenEnv)
		}
	}
	dc.SelfDeploy = sd
	return &dc
}

// fleetHealthURL is the single endpoint the post-restart health probe targets:
// the daemon's fleet snapshot, which surfaces the running binary's version
// (#698) the script SHA-matches against the deployed commit. Empty when no web
// endpoint is bound (port 0).
func (d *Daemon) fleetHealthURL() string {
	if d.opts.Port <= 0 {
		return ""
	}
	host := strings.TrimSpace(d.opts.Host)
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s/api/v1/fleet", net.JoinHostPort(host, strconv.Itoa(d.opts.Port)))
}
