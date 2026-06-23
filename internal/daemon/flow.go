package daemon

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/configwatch"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/orchestrator"
	"github.com/befeast/maestro/internal/server"
)

// projectFlow is one project running inside the daemon: an orchestrator loop
// and a supervisor loop sharing a child context. Cancelling that context
// (stopFlow) tears both loops down.
type projectFlow struct {
	name   string
	cfg    *config.Config
	cancel context.CancelFunc
	done   chan struct{} // closed once both loops have exited
}

// newFleetProject wraps a config for in-process fleet serving, mirroring the
// `maestro serve` wiring: a safe-action GitHub client, plus the GitHub Project
// board client when github_projects is enabled. The project name is derived
// from the repo (NewFleetProject's default) to match the serve path.
func newFleetProject(cfg *config.Config) server.FleetProject {
	proj := server.NewFleetProject("", cfg.ResolvePath(), "", cfg)
	gh := github.New(cfg.Repo)
	proj.SetActionGH(gh)
	if cfg.GitHubProjects.Enabled && cfg.GitHubProjects.ProjectNumber > 0 {
		proj.SetBoardClient(gh, cfg.GitHubProjects.ProjectNumber)
	}
	return proj
}

// startFlow launches the orchestrator + supervisor loops for proj under a
// child of parent and registers the flow. Each loop runs in its own goroutine
// with a panic recover so a single project's crash can never take the daemon
// (or the other flows) down (#756).
func (d *Daemon) startFlow(parent context.Context, proj server.FleetProject) *projectFlow {
	cfg := proj.Cfg()
	fctx, cancel := context.WithCancel(parent)
	flow := &projectFlow{
		name:   proj.Name,
		cfg:    cfg,
		cancel: cancel,
		done:   make(chan struct{}),
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer recoverFlow(flow.name, "orchestrator")
		d.runLoop(fctx, cfg, d.opts)
	}()
	go func() {
		defer wg.Done()
		defer recoverFlow(flow.name, "supervise")
		d.superviseLoop(fctx, cfg, d.opts)
	}()
	go func() {
		wg.Wait()
		close(flow.done)
	}()

	d.mu.Lock()
	d.flows[flow.name] = flow
	d.mu.Unlock()
	return flow
}

// stopFlow cancels a flow's context, waits for both loops to drain, and
// deregisters it. Safe to call for an unknown name (no-op).
func (d *Daemon) stopFlow(name string) {
	d.mu.Lock()
	flow, ok := d.flows[name]
	if ok {
		delete(d.flows, name)
	}
	d.mu.Unlock()
	if !ok {
		return
	}
	flow.cancel()
	<-flow.done
	log.Printf("[daemon] stopped flow %q", name)
}

// stopAll drains every running flow. Called on daemon shutdown.
func (d *Daemon) stopAll() {
	d.mu.Lock()
	names := make([]string, 0, len(d.flows))
	for name := range d.flows {
		names = append(names, name)
	}
	d.mu.Unlock()
	for _, name := range names {
		d.stopFlow(name)
	}
}

// recoverFlow swallows a panic in a flow loop so it stays contained to that
// project. A panicking goroutine would otherwise crash the whole daemon.
func recoverFlow(name, loop string) {
	if r := recover(); r != nil {
		log.Printf("[daemon] flow %q %s loop panicked (contained, flow stopped): %v", name, loop, r)
	}
}

// runOrchestrator is the production run loop for one flow. It mirrors the
// single-project branch of `maestro run` minus the per-project HTTP server
// (the daemon serves one shared FleetServer instead) and minus the fatal exit
// on run error — a failing orchestrator must log-and-continue, never take the
// daemon down (#756). orchestrator.Run already log-and-continues on per-cycle
// errors and returns nil on ctx cancellation.
func runOrchestrator(ctx context.Context, cfg *config.Config, opts Options) {
	orch := orchestrator.New(cfg)
	orch.SetBinaryVersion(opts.Version)
	if err := orch.LoadPromptBase(opts.PromptPath); err != nil {
		log.Printf("[%s] warn: load prompt: %v", cfg.SessionPrefix, err)
	}

	refreshCh := make(chan struct{}, 1)

	// Config file watcher for hot-reload, matching `maestro run`.
	if cfgPath := cfg.ResolvePath(); cfgPath != "" {
		reloadCh := configwatch.Watch(ctx, cfgPath, 2*time.Second)
		orch.SetConfigReloadCh(reloadCh)
	}

	runInterval := opts.RunInterval
	if cfg.PollIntervalSeconds > 0 {
		runInterval = time.Duration(cfg.PollIntervalSeconds) * time.Second
	}

	log.Printf("[%s] starting orchestrator — repo=%s interval=%s", cfg.SessionPrefix, cfg.Repo, runInterval)
	if err := orch.Run(ctx, runInterval, false, refreshCh); err != nil {
		log.Printf("[%s] orchestrator exited: %v", cfg.SessionPrefix, err)
	}
}
