package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/configwatch"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/orchestrator"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/server"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/befeast/maestro/internal/versioning"
	"github.com/befeast/maestro/internal/watch"
	"github.com/befeast/maestro/internal/worker"
)

const usage = `maestro - AI coding agent orchestrator

Usage:
  maestro <command> [flags]

Commands:
  init          Interactive setup wizard for new projects
  run           Run the orchestration loop
  supervise     Run supervisor decision loop with safe queue actions
  serve         Run Mission Control read-only web dashboard/API
  status        Show current state
  logs          Show worker logs (tail -f)
  watch         Open tmux dashboard with live worker output
  spawn         Spawn a worker for a specific issue number
  stop          Stop a worker session
  kill          Kill a worker session by slot name
  import        Seed state from existing worktrees
  history       Show recently completed sessions
  cleanup       Remove worktrees for all completed/dead sessions
  version-bump  Bump project version based on merged PR labels
  version       Print version

Global flags:
  --config string       Path to config file (can be repeated for multiple projects)

  Multiple projects: pass --config for each project config file, or place
  configs in a maestro.d/ directory for automatic discovery.

Run flags:
  --interval duration   Loop interval (default 10m)
  --once                Run once and exit
  --prompt string       Path to worker prompt base file

Supervise flags:
  --once                Run one supervisor decision and exit
  --interval duration   Loop interval (default 5m)
  --json                Output decision as JSON
  maestro supervise approve <approval-or-decision-id>
  maestro supervise reject <approval-or-decision-id>

Serve flags:
  --fleet string        Path to fleet YAML file for multi-project dashboard
  --host string         Host/interface to bind (default from config, then 127.0.0.1)
  --port int            Port to bind (overrides server.port)
  --read-only           Disable mutating HTTP endpoints (default true)

Spawn flags:
  --issue int           Issue number to work on
  --prompt string       Path to worker prompt base file

Stop flags:
  --session string      Session name to stop (e.g. pan-1)

Kill:
  maestro kill <slot>   Kill a specific worker (e.g. maestro kill pan-1)

Version-bump flags:
  --pr int              PR number to read labels/commits from

Logs:
  maestro logs              List active worker logs + tmux attach hints
  maestro logs <slot>       Attach to worker tmux session (live), or tail log if done

History:
  maestro history              Show last 20 completed sessions
  maestro history --limit 50   Show last 50 completed sessions
  maestro history --json       Machine-readable JSON output
  maestro history --prune      Remove sessions older than retention period

Watch:
  maestro watch             Open tmux dashboard attached to live worker sessions
`

// version is set at build time via -ldflags "-X main.version=X.Y.Z".
// When not set (local builds), resolveVersion falls back to Go module/VCS info.
var version = "dev"

func resolveVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	// go install github.com/befeast/maestro/cmd/maestro@v1.2.3 sets info.Main.Version
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return strings.TrimPrefix(v, "v")
	}
	// Local build from git checkout — use VCS revision
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}
		return "dev-" + rev + dirty
	}
	return version
}

// multiFlag accumulates repeated --config flag values.
type multiFlag []string

func (f *multiFlag) String() string { return strings.Join(*f, ", ") }
func (f *multiFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[maestro] ")

	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "init":
		initCmd(args)
	case "run":
		runCmd(args)
	case "supervise":
		superviseCmd(args)
	case "serve":
		serveCmd(args)
	case "status":
		statusCmd(args)
	case "logs":
		logsCmd(args)
	case "watch":
		watchCmd(args)
	case "spawn":
		spawnCmd(args)
	case "stop":
		stopCmd(args)
	case "kill":
		killCmd(args)
	case "import":
		importCmd(args)
	case "history":
		historyCmd(args)
	case "cleanup":
		cleanupCmd(args)
	case "version-bump":
		versionBumpCmd(args)
	case "_watch-updater":
		watchUpdaterCmd(args)
	case "_watch-tail":
		watchTailCmd(args)
	case "version":
		fmt.Printf("maestro v%s\n", resolveVersion())
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", cmd, usage)
		os.Exit(1)
	}
}

// loadConfig loads config from a specific path or uses default discovery.
func loadConfig(configPath string) *config.Config {
	var cfg *config.Config
	var err error
	if configPath != "" {
		cfg, err = config.LoadFrom(configPath)
	} else {
		cfg, err = config.Load()
	}
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	return cfg
}

// loadConfigs resolves multiple config paths, maestro.d/ directory, or default discovery.
func loadConfigs(paths []string) []*config.Config {
	if len(paths) > 0 {
		var cfgs []*config.Config
		for _, p := range paths {
			cfg, err := config.LoadFrom(p)
			if err != nil {
				log.Fatalf("load config %s: %v", p, err)
			}
			cfgs = append(cfgs, cfg)
		}
		return cfgs
	}

	// Check for maestro.d/ directory
	if info, err := os.Stat("maestro.d"); err == nil && info.IsDir() {
		cfgs, err := config.LoadDir("maestro.d")
		if err != nil {
			log.Fatalf("load configs from maestro.d/: %v", err)
		}
		return cfgs
	}

	// Fall back to default single config discovery
	return []*config.Config{loadConfig("")}
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var configs multiFlag
	fs.Var(&configs, "config", "Path to config file (can be repeated)")
	interval := fs.Duration("interval", 10*time.Minute, "Loop interval")
	once := fs.Bool("once", false, "Run once and exit")
	promptPath := fs.String("prompt", "", "Path to worker prompt base file")
	fs.Parse(args)

	cfgs := loadConfigs(configs)

	if len(cfgs) == 1 {
		cfg := cfgs[0]
		orch := orchestrator.New(cfg)
		if err := orch.LoadPromptBase(*promptPath); err != nil {
			log.Printf("warn: load prompt: %v", err)
		}

		refreshCh := make(chan struct{}, 1)

		// Start HTTP server if configured
		if cfg.Server.Port > 0 {
			srv := server.New(cfg, refreshCh)
			go func() {
				if err := srv.Start(context.Background()); err != nil {
					log.Printf("[server] error: %v", err)
				}
			}()
		}

		// Start config file watcher for hot-reload
		ctx := context.Background()
		cfgPath := cfg.ResolvePath()
		if cfgPath != "" {
			reloadCh := configwatch.Watch(ctx, cfgPath, 2*time.Second)
			orch.SetConfigReloadCh(reloadCh)
		}

		// Use config-driven poll interval if set
		runInterval := *interval
		if cfg.PollIntervalSeconds > 0 {
			runInterval = time.Duration(cfg.PollIntervalSeconds) * time.Second
		}

		log.Printf("starting maestro — repo=%s prefix=%s interval=%s once=%v", cfg.Repo, cfg.SessionPrefix, runInterval, *once)
		if err := orch.Run(ctx, runInterval, *once, refreshCh); err != nil {
			log.Fatalf("run: %v", err)
		}
		return
	}

	// Multiple projects — run each in its own goroutine
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("received signal, shutting down all projects...")
		cancel()
	}()

	log.Printf("starting maestro with %d projects", len(cfgs))

	var wg sync.WaitGroup
	for _, cfg := range cfgs {
		wg.Add(1)
		go func(c *config.Config) {
			defer wg.Done()
			orch := orchestrator.New(c)
			if err := orch.LoadPromptBase(*promptPath); err != nil {
				log.Printf("[%s] warn: load prompt: %v", c.SessionPrefix, err)
			}

			refreshCh := make(chan struct{}, 1)

			// Start HTTP server if configured
			if c.Server.Port > 0 {
				srv := server.New(c, refreshCh)
				go func() {
					if err := srv.Start(ctx); err != nil {
						log.Printf("[%s][server] error: %v", c.SessionPrefix, err)
					}
				}()
			}

			// Start config file watcher for hot-reload
			cfgPath := c.ResolvePath()
			if cfgPath != "" {
				reloadCh := configwatch.Watch(ctx, cfgPath, 2*time.Second)
				orch.SetConfigReloadCh(reloadCh)
			}

			// Use config-driven poll interval if set
			runInterval := *interval
			if c.PollIntervalSeconds > 0 {
				runInterval = time.Duration(c.PollIntervalSeconds) * time.Second
			}

			log.Printf("[%s] starting — repo=%s interval=%s once=%v", c.SessionPrefix, c.Repo, runInterval, *once)
			if err := orch.Run(ctx, runInterval, *once, refreshCh); err != nil {
				log.Printf("[%s] run error: %v", c.SessionPrefix, err)
			}
		}(cfg)
	}
	wg.Wait()
}

func superviseCmd(args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "approve", "reject":
			superviseApprovalCmd(args[0], args[1:], "")
			return
		}
	}

	fs := flag.NewFlagSet("supervise", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to config file")
	once := fs.Bool("once", false, "Run once and exit")
	interval := fs.Duration("interval", 5*time.Minute, "Loop interval")
	jsonOutput := fs.Bool("json", false, "Output decision as JSON")
	dryRun := fs.Bool("dry-run", false, "Compute decision without recording state")
	fs.Parse(args)
	if fs.NArg() > 0 {
		subcmd := fs.Arg(0)
		switch subcmd {
		case "approve", "reject":
			superviseApprovalCmd(subcmd, fs.Args()[1:], *configPath)
			return
		default:
			log.Fatalf("supervise: unexpected argument %q", subcmd)
		}
	}

	if !*once && *interval <= 0 {
		log.Fatalf("supervise: --interval must be positive")
	}

	cfg := loadConfig(*configPath)
	if *dryRun {
		cfg.Supervisor.DryRun = true
	}
	gh := github.New(cfg.Repo)
	runOnce := func() {
		decision, err := supervisor.RunOnce(cfg, gh)
		if err != nil {
			log.Fatalf("supervise: %v", err)
		}
		printSupervisorDecision(decision, *jsonOutput)
	}

	runOnce()
	if *once {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

func superviseApprovalCmd(action string, args []string, defaultConfigPath string) {
	fs := flag.NewFlagSet("supervise "+action, flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath, "Path to config file")
	actor := fs.String("actor", "cli", "Audit actor")
	reason := fs.String("reason", "", "Audit reason")
	fs.Parse(args)
	if fs.NArg() != 1 {
		log.Fatalf("supervise %s: expected approval or decision id", action)
	}

	cfg := loadConfig(*configPath)
	st, err := state.Load(cfg.StateDir)
	if err != nil {
		log.Fatalf("supervise %s: load state: %v", action, err)
	}

	id := fs.Arg(0)
	now := time.Now().UTC()
	var approval *state.Approval
	switch action {
	case "approve":
		approval, err = st.ApproveApproval(id, now, *actor, *reason)
	case "reject":
		approval, err = st.RejectApproval(id, now, *actor, *reason)
	default:
		log.Fatalf("supervise: unknown approval action %q", action)
	}
	if err != nil {
		if errors.Is(err, state.ErrApprovalStale) || errors.Is(err, state.ErrApprovalPayloadMismatch) {
			if saveErr := state.Save(cfg.StateDir, st); saveErr != nil {
				log.Fatalf("supervise %s: save stale approval: %v", action, saveErr)
			}
		}
		log.Fatalf("supervise %s: %v", action, err)
	}
	if err := state.Save(cfg.StateDir, st); err != nil {
		log.Fatalf("supervise %s: save state: %v", action, err)
	}
	fmt.Printf("Approval %s %s. No risky action was executed.\n", approval.ID, approval.Status)
}

func printSupervisorDecision(decision state.SupervisorDecision, jsonOutput bool) {
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(decision)
		return
	}

	fmt.Printf("Supervisor decision: %s\n", decision.RecommendedAction)
	if decision.Status != "" {
		fmt.Printf("Status: %s\n", decision.Status)
	}
	fmt.Printf("Summary: %s\n", decision.Summary)
	fmt.Printf("Risk: %s\n", decision.Risk)
	fmt.Printf("Confidence: %.2f\n", decision.Confidence)
	if decision.ErrorClass != "" {
		fmt.Printf("Error class: %s\n", decision.ErrorClass)
	}
	if decision.ApprovalID != "" {
		fmt.Printf("Approval: %s\n", decision.ApprovalID)
	}
	if decision.Target != nil {
		parts := supervisorTargetParts(decision.Target)
		if len(parts) > 0 {
			fmt.Printf("Target: %s\n", strings.Join(parts, ", "))
		}
	}
	if len(decision.Reasons) > 0 {
		fmt.Println("Reasons:")
		for _, reason := range decision.Reasons {
			fmt.Printf("  - %s\n", reason)
		}
	}
	if decision.QueueAnalysis != nil {
		printQueueAnalysis(decision.QueueAnalysis, "")
	}
	if len(decision.Mutations) > 0 {
		fmt.Println("Mutations:")
		for _, mutation := range decision.Mutations {
			fmt.Printf("  - %s", mutation.Type)
			if mutation.Issue > 0 {
				fmt.Printf(" issue #%d", mutation.Issue)
			}
			if mutation.Label != "" {
				fmt.Printf(" label %q", mutation.Label)
			}
			if mutation.Status != "" {
				fmt.Printf(" status %s", mutation.Status)
			}
			if mutation.ErrorClass != "" {
				fmt.Printf(" error_class %s", mutation.ErrorClass)
			}
			fmt.Println()
		}
	}
	if len(decision.StuckStates) > 0 {
		fmt.Println("Stuck states:")
		for _, stuck := range decision.StuckStates {
			fmt.Printf("  - %s [%s]: %s\n", stuck.Code, stuck.Severity, stuck.Summary)
			if stuck.RecommendedAction != "" {
				fmt.Printf("    next: %s\n", stuck.RecommendedAction)
			}
		}
	}
	fmt.Printf("Recorded: %s\n", decision.CreatedAt.Format(time.RFC3339))
}

func printQueueAnalysis(analysis *state.SupervisorQueueAnalysis, indent string) {
	if analysis == nil {
		return
	}
	fmt.Printf("%sQueue: open=%d eligible=%d excluded=%d non_runnable_project_status=%d\n", indent, analysis.OpenIssues, analysis.EligibleCandidates, analysis.ExcludedIssues, analysis.NonRunnableProjectStatusCount)
	if analysis.SelectedCandidate != nil {
		fmt.Printf("%sSelected candidate: issue #%d", indent, analysis.SelectedCandidate.Number)
		if analysis.SelectedCandidate.PriorityLabel != "" {
			fmt.Printf(" priority=%s", analysis.SelectedCandidate.PriorityLabel)
		}
		if analysis.SelectedCandidate.ProjectStatus != "" {
			fmt.Printf(" project_status=%q", analysis.SelectedCandidate.ProjectStatus)
		}
		fmt.Println()
	}
	for _, reason := range analysis.SkippedReasons {
		fmt.Printf("%sSkipped: %s\n", indent, reason)
	}
}

func supervisorTargetParts(target *state.SupervisorTarget) []string {
	if target == nil {
		return nil
	}
	var parts []string
	if target.Issue > 0 {
		parts = append(parts, fmt.Sprintf("issue #%d", target.Issue))
	}
	if target.PR > 0 {
		parts = append(parts, fmt.Sprintf("PR #%d", target.PR))
	}
	if strings.TrimSpace(target.Session) != "" {
		parts = append(parts, "session "+target.Session)
	}
	return parts
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var configs multiFlag
	fs.Var(&configs, "config", "Path to config file")
	fleetPath := fs.String("fleet", "", "Path to fleet YAML file")
	host := fs.String("host", "", "Host/interface to bind")
	port := fs.Int("port", 0, "Port to bind")
	readOnly := fs.Bool("read-only", true, "Disable mutating HTTP endpoints")
	fs.Parse(args)

	var cfgs []*config.Config
	if strings.TrimSpace(*fleetPath) == "" {
		cfgs = loadConfigs(configs)
	}
	if strings.TrimSpace(*fleetPath) != "" || len(cfgs) > 1 {
		var projects []server.FleetProject
		var err error
		if strings.TrimSpace(*fleetPath) != "" {
			projects, err = server.LoadFleetProjects(*fleetPath)
			if err != nil {
				log.Fatalf("load fleet: %v", err)
			}
		} else {
			projects = fleetProjectsFromConfigs(cfgs)
		}
		fleetHost := *host
		if strings.TrimSpace(fleetHost) == "" {
			fleetHost = "127.0.0.1"
		}
		if *port <= 0 {
			log.Fatalf("serve fleet requires --port")
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			cancel()
		}()

		log.Printf("serving fleet dashboard — projects=%d addr=%s:%d read_only=%v", len(projects), fleetHost, *port, *readOnly)
		if err := server.NewFleet(projects, fleetHost, *port, *readOnly).Start(ctx); err != nil {
			log.Fatalf("serve fleet: %v", err)
		}
		return
	}

	if len(cfgs) != 1 {
		log.Fatalf("serve requires exactly one config, got %d", len(cfgs))
	}
	cfg := cfgs[0]
	if strings.TrimSpace(*host) != "" {
		cfg.Server.Host = *host
	}
	if *port > 0 {
		cfg.Server.Port = *port
	}
	cfg.Server.ReadOnly = *readOnly
	if cfg.Server.Port <= 0 {
		log.Fatalf("serve requires server.port in config or --port")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	refreshCh := make(chan struct{}, 1)
	log.Printf("serving dashboard — repo=%s addr=%s:%d read_only=%v", cfg.Repo, cfg.Server.Host, cfg.Server.Port, cfg.Server.ReadOnly)
	if err := server.New(cfg, refreshCh).Start(ctx); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func fleetProjectsFromConfigs(cfgs []*config.Config) []server.FleetProject {
	projects := make([]server.FleetProject, 0, len(cfgs))
	for _, cfg := range cfgs {
		projects = append(projects, server.NewFleetProject(defaultFleetProjectName(cfg.Repo), cfg.ResolvePath(), "", cfg))
	}
	return projects
}

func defaultFleetProjectName(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "project"
	}
	parts := strings.Split(repo, "/")
	return parts[len(parts)-1]
}

func statusCmd(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	var configs multiFlag
	fs.Var(&configs, "config", "Path to config file (can be repeated)")
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	fs.Parse(args)

	cfgs := loadConfigs(configs)

	// JSON: for multiple projects, emit an array of objects
	if *jsonOutput && len(cfgs) > 1 {
		var results []map[string]interface{}
		for _, cfg := range cfgs {
			s, err := state.Load(cfg.StateDir)
			if err != nil {
				log.Printf("load state for %s: %v", cfg.Repo, err)
				continue
			}
			results = append(results, map[string]interface{}{
				"repo":   cfg.Repo,
				"prefix": cfg.SessionPrefix,
				"state":  s,
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(results)
		return
	}

	for i, cfg := range cfgs {
		if i > 0 {
			fmt.Print("\n---\n\n")
		}
		showProjectStatus(cfg, *jsonOutput)
	}
}

func showProjectStatus(cfg *config.Config, jsonOutput bool) {
	s, err := state.Load(cfg.StateDir)
	if err != nil {
		log.Fatalf("load state for %s: %v", cfg.Repo, err)
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(s)
		return
	}

	// Pretty table
	fmt.Printf("Repo:           %s\n", cfg.Repo)
	fmt.Printf("Session prefix: %s\n", cfg.SessionPrefix)
	fmt.Printf("State file:     %s\n", state.StatePath(cfg.StateDir))
	fmt.Printf("Max parallel:   %d\n", cfg.MaxParallel)
	showSupervisorPolicy(cfg)
	if len(cfg.MaxConcurrentByState) > 0 {
		// Sort keys for stable output
		stateNames := make([]string, 0, len(cfg.MaxConcurrentByState))
		for k := range cfg.MaxConcurrentByState {
			stateNames = append(stateNames, k)
		}
		sort.Strings(stateNames)
		statusCounts := s.CountByStatus()
		fmt.Printf("Per-state limits:\n")
		for _, name := range stateNames {
			limit := cfg.MaxConcurrentByState[name]
			current := statusCounts[state.SessionStatus(name)]
			fmt.Printf("  %-16s %d/%d\n", name+":", current, limit)
		}
	}
	fmt.Println()
	showLatestSupervisorDecision(s)
	showApprovals(s)

	if len(s.Sessions) == 0 {
		fmt.Println("No sessions.")
		return
	}

	// Sort sessions by status priority (running first), then by name
	names := make([]string, 0, len(s.Sessions))
	for name := range s.Sessions {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		pi := state.StatusPriority(s.Sessions[names[i]].Status)
		pj := state.StatusPriority(s.Sessions[names[j]].Status)
		if pi != pj {
			return pi < pj
		}
		return names[i] < names[j]
	})

	// Fetch CI status for pr_open sessions
	gh := github.New(cfg.Repo)
	ciStatuses := make(map[string]string) // session name → CI display string
	for _, name := range names {
		sess := s.Sessions[name]
		if (sess.Status == state.StatusPROpen || sess.Status == state.StatusQueued) && sess.PRNumber > 0 {
			ciStatus, err := gh.PRCIStatus(sess.PRNumber)
			if err != nil {
				ciStatuses[name] = "?"
			} else {
				switch ciStatus {
				case "success":
					ciStatuses[name] = "✅ pass"
				case "failure":
					ciStatuses[name] = "❌ fail"
				case "pending":
					ciStatuses[name] = "⏳ pending"
				default:
					ciStatuses[name] = "?"
				}
			}
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tISSUE\tSTATUS\tBACKEND\tPR\tCI\tPID\tALIVE\tAGE\tRETRIES\tTOKENS\tTITLE")
	fmt.Fprintln(w, "-------\t-----\t------\t-------\t--\t--\t---\t-----\t---\t-------\t------\t-----")
	for _, name := range names {
		sess := s.Sessions[name]
		alive := "-"
		if sess.Status == state.StatusRunning {
			if worker.IsAlive(sess.PID) {
				alive = "yes"
			} else {
				alive = "no"
			}
		}
		age := time.Since(sess.StartedAt).Round(time.Minute)
		retries := "-"
		if sess.RetryCount > 0 {
			if sess.NextRetryAt != nil && time.Now().Before(*sess.NextRetryAt) {
				remaining := time.Until(*sess.NextRetryAt).Round(time.Second)
				retries = fmt.Sprintf("%d (in %s)", sess.RetryCount, remaining)
			} else {
				retries = fmt.Sprintf("%d", sess.RetryCount)
			}
		}
		pr := "-"
		if sess.PRNumber > 0 {
			pr = fmt.Sprintf("#%d", sess.PRNumber)
		}
		ci := "-"
		if cs, ok := ciStatuses[name]; ok {
			ci = cs
		}
		tokens := worker.FormatTokens(sess.TokensUsedTotal)
		backend := sess.Backend
		if backend == "" {
			backend = "-"
		}
		fmt.Fprintf(w, "%s\t#%d\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			name, sess.IssueNumber, sess.Status, backend, pr, ci, sess.PID, alive, age, retries, tokens, truncate(sess.IssueTitle, 50))
	}
	w.Flush()

	// Show attach/log hints for running sessions
	fmt.Println()
	for _, name := range names {
		sess := s.Sessions[name]
		if sess.Status == state.StatusRunning && worker.IsAlive(sess.PID) {
			tmuxName := worker.TmuxSessionName(name)
			fmt.Printf("  %s:\n", name)
			fmt.Printf("    attach: tmux attach -t %s\n", tmuxName)
			fmt.Printf("    log:    tail -f %s\n", sess.LogFile)
		}
	}

	// Show blocked issues (issues with open blockers)
	if len(cfg.BlockerPatterns) > 0 {
		issues, err := gh.ListOpenIssues(cfg.IssueLabels)
		if err == nil {
			var blockedLines []string
			for _, issue := range issues {
				blockers := github.FindBlockers(issue.Body, cfg.BlockerPatterns)
				if len(blockers) == 0 {
					continue
				}
				var openBlockers []int
				for _, b := range blockers {
					closed, err := gh.IsIssueClosed(b)
					if err != nil || !closed {
						openBlockers = append(openBlockers, b)
					}
				}
				if len(openBlockers) > 0 {
					refs := make([]string, len(openBlockers))
					for i, b := range openBlockers {
						refs[i] = fmt.Sprintf("#%d", b)
					}
					blockedLines = append(blockedLines, fmt.Sprintf("  #%-6d blocked by %s  (%s)", issue.Number, strings.Join(refs, ", "), truncate(issue.Title, 50)))
				}
			}
			if len(blockedLines) > 0 {
				fmt.Println("\nBlocked issues:")
				for _, line := range blockedLines {
					fmt.Println(line)
				}
			}
		}
	}
}

func showSupervisorPolicy(cfg *config.Config) {
	mode := strings.TrimSpace(cfg.Supervisor.Mode)
	if mode == "" {
		mode = "cautious"
	}
	fmt.Printf("Supervisor:     mode=%s enabled=%v\n", mode, cfg.Supervisor.Enabled)
	if cfg.Supervisor.PolicyPath != "" {
		fmt.Printf("Policy file:    %s\n", cfg.Supervisor.PolicyPath)
	}
	if cfg.Supervisor.ReadyLabel != "" || cfg.Supervisor.BlockedLabel != "" {
		fmt.Printf("Policy labels:  ready=%s blocked=%s\n", valueOrDash(cfg.Supervisor.ReadyLabel), valueOrDash(cfg.Supervisor.BlockedLabel))
	}
	if len(cfg.Supervisor.ExcludedLabels) > 0 {
		fmt.Printf("Policy exclude: %s\n", strings.Join(cfg.Supervisor.ExcludedLabels, ", "))
	}
	if cfg.Supervisor.OrderedQueueActive() {
		fmt.Printf("Policy queue:   ordered (%d issue(s))\n", len(cfg.Supervisor.OrderedQueue.Issues))
	}
	if len(cfg.Supervisor.SafeActions) > 0 {
		fmt.Printf("Safe actions:   %s\n", strings.Join(cfg.Supervisor.SafeActions, ", "))
	}
	if len(cfg.Supervisor.ApprovalRequired) > 0 {
		fmt.Printf("Approval req.:  %s\n", strings.Join(cfg.Supervisor.ApprovalRequired, ", "))
	}
}

func valueOrDash(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	return value
}

func logsCmd(args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	var configs multiFlag
	fs.Var(&configs, "config", "Path to config file (can be repeated)")
	fs.Parse(args)
	args = fs.Args() // remaining args after flags

	cfgs := loadConfigs(configs)

	// If a specific slot is given, find it across all projects
	if len(args) > 0 && args[0] != "" && !strings.HasPrefix(args[0], "-") {
		slotName := args[0]
		for _, cfg := range cfgs {
			s, err := state.Load(cfg.StateDir)
			if err != nil {
				continue
			}
			sess, ok := s.Sessions[slotName]
			if !ok {
				continue
			}

			// If worker's tmux session is alive, attach to it for live output
			tmuxName := worker.TmuxSessionName(slotName)
			if sess.Status == state.StatusRunning && exec.Command("tmux", "has-session", "-t", tmuxName).Run() == nil {
				tmuxPath, err := exec.LookPath("tmux")
				if err != nil {
					log.Fatalf("find tmux: %v", err)
				}
				fmt.Printf("Attaching to tmux session %s (read-only)...\n", tmuxName)
				syscall.Exec(tmuxPath, []string{"tmux", "attach-session", "-t", tmuxName, "-r"}, os.Environ())
				log.Fatalf("exec tmux attach: should not reach here")
			}

			// Fallback: tail the log file
			if _, err := os.Stat(sess.LogFile); os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "error: log file not found: %s\n", sess.LogFile)
				os.Exit(1)
			}
			tailPath, err := exec.LookPath("tail")
			if err != nil {
				log.Fatalf("find tail: %v", err)
			}
			syscall.Exec(tailPath, []string{"tail", "-f", sess.LogFile}, os.Environ())
			log.Fatalf("exec tail: should not reach here")
		}
		fmt.Fprintf(os.Stderr, "error: session %q not found\n", slotName)
		os.Exit(1)
	}

	// No args — list all active worker logs across all projects
	type logEntry struct {
		name    string
		sess    *state.Session
		logDir  string
		prefix  string
		project string
	}
	var entries []logEntry
	for _, cfg := range cfgs {
		s, err := state.Load(cfg.StateDir)
		if err != nil {
			log.Printf("load state for %s: %v", cfg.Repo, err)
			continue
		}
		for name, sess := range s.Sessions {
			if sess.Status == state.StatusRunning {
				entries = append(entries, logEntry{
					name:    name,
					sess:    sess,
					logDir:  state.LogDir(cfg.StateDir),
					prefix:  cfg.SessionPrefix,
					project: cfg.Repo,
				})
			}
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	if len(entries) == 0 {
		fmt.Println("No active worker sessions.")
		return
	}

	multiProject := len(cfgs) > 1
	fmt.Println("Active worker logs:")
	currentProject := ""
	for _, e := range entries {
		if multiProject && e.project != currentProject {
			fmt.Printf("\n  [%s] %s:\n", e.prefix, e.project)
			currentProject = e.project
		}
		alive := ""
		if !worker.IsAlive(e.sess.PID) {
			alive = " (dead)"
		}
		fmt.Printf("  %s (#%d): %s%s\n", e.name, e.sess.IssueNumber, e.sess.LogFile, alive)
	}

	fmt.Println()
	fmt.Println("To attach to a worker:")
	for _, e := range entries {
		fmt.Printf("  tmux attach -t %s\n", worker.TmuxSessionName(e.name))
	}

	if !multiProject && len(entries) > 0 {
		fmt.Println()
		fmt.Printf("To watch all logs:\n  tail -f %s/%s-*.log\n", entries[0].logDir, entries[0].prefix)
	}
}

func showLatestSupervisorDecision(s *state.State) {
	decision := s.LatestSupervisorDecision()
	if decision == nil {
		return
	}
	fmt.Println("Supervisor:")
	fmt.Printf("  Latest action: %s (%s)\n", decision.RecommendedAction, formatRelativeTime(decision.CreatedAt))
	if decision.Summary != "" {
		fmt.Printf("  Summary: %s\n", decision.Summary)
	}
	fmt.Printf("  Risk: %s  Confidence: %.2f\n", decision.Risk, decision.Confidence)
	if decision.ApprovalID != "" {
		fmt.Printf("  Approval: %s\n", decision.ApprovalID)
	}
	if decision.Target != nil {
		parts := supervisorTargetParts(decision.Target)
		if len(parts) > 0 {
			fmt.Printf("  Target: %s\n", strings.Join(parts, ", "))
		}
	}
	if decision.QueueAnalysis != nil {
		printQueueAnalysis(decision.QueueAnalysis, "  ")
	}
	if len(decision.StuckStates) > 0 {
		fmt.Printf("  Stuck states: %d", len(decision.StuckStates))
		first := decision.StuckStates[0]
		if first.Code != "" {
			fmt.Printf(" (top: %s/%s)", first.Code, first.Severity)
		}
		fmt.Println()
	}
	fmt.Println()
}

func showApprovals(s *state.State) {
	if len(s.Approvals) == 0 {
		return
	}
	approvals := append([]state.Approval(nil), s.Approvals...)
	sort.Slice(approvals, func(i, j int) bool {
		return approvals[i].CreatedAt.After(approvals[j].CreatedAt)
	})
	fmt.Println("Approvals:")
	for _, approval := range approvals {
		target := "-"
		parts := supervisorTargetParts(approval.Target)
		if len(parts) > 0 {
			target = strings.Join(parts, ", ")
		}
		fmt.Printf("  %s  %s  %s  %s\n", approval.ID, approval.Status, approval.Action, target)
	}
	fmt.Println()
}

// watchPaneCmd builds a shell command for a watch pane.
// It tails the worker's log file with spinner/dot filtering via _watch-tail,
// then shows an exit prompt when the worker finishes.
func watchPaneCmd(selfBin string, name string, sess *state.Session) string {
	tmuxName := watchSessionName(name, sess)
	return fmt.Sprintf(
		`%s _watch-tail %q %q; `+
			`echo; echo '=== Worker session ended ==='; `+
			`echo; echo 'Press any key to exit...'; read -n1`,
		selfBin, sess.LogFile, tmuxName)
}

func watchSessionName(slotName string, sess *state.Session) string {
	if sess != nil && strings.TrimSpace(sess.TmuxSession) != "" {
		return sess.TmuxSession
	}
	return worker.TmuxSessionName(slotName)
}

func tmuxSessionAlive(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	return exec.Command("tmux", "has-session", "-t", name).Run() == nil
}

func watchCmd(args []string) {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	var configs multiFlag
	fs.Var(&configs, "config", "Path to config file (can be repeated)")
	fs.Parse(args)

	cfgs := loadConfigs(configs)

	// Collect workers that still have an active tmux session across all projects.
	type activeWorker struct {
		name     string
		sess     *state.Session
		stateDir string
	}
	var workers []activeWorker
	for _, cfg := range cfgs {
		s, err := state.Load(cfg.StateDir)
		if err != nil {
			log.Printf("load state for %s: %v", cfg.Repo, err)
			continue
		}
		for name, sess := range s.Sessions {
			tmuxName := watchSessionName(name, sess)
			if tmuxSessionAlive(tmuxName) {
				workers = append(workers, activeWorker{name, sess, cfg.StateDir})
			}
		}
	}

	if len(workers) == 0 {
		fmt.Println("No active workers.")
		os.Exit(0)
	}

	// Sort by name for deterministic pane order
	sort.Slice(workers, func(i, j int) bool {
		return workers[i].name < workers[j].name
	})

	const tmuxSession = "maestro-watch"

	// Kill stale session if exists
	exec.Command("tmux", "kill-session", "-t", tmuxSession).Run()

	// Resolve path to self for _watch-tail subcommand
	selfBin, _ := os.Executable()
	if selfBin == "" {
		selfBin = os.Args[0]
	}

	// Build pane mappings for the background status updater
	var paneMappings []watch.PaneMapping

	// Create new session with first worker — tail filtered log
	firstCmd := watchPaneCmd(selfBin, workers[0].name, workers[0].sess)
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", tmuxSession, "bash", "-c", firstCmd).CombinedOutput(); err != nil {
		log.Fatalf("tmux new-session: %v\n%s", err, out)
	}

	// Set rich pane title for first pane
	paneTitle := watch.FormatPaneTitle(workers[0].name, workers[0].sess)
	exec.Command("tmux", "select-pane", "-t", tmuxSession+":0.0", "-T", paneTitle).Run()
	paneMappings = append(paneMappings, watch.PaneMapping{
		PaneIndex: 0,
		SlotName:  workers[0].name,
		StateDir:  workers[0].stateDir,
	})

	// Split for each additional worker
	for i := 1; i < len(workers); i++ {
		paneCmd := watchPaneCmd(selfBin, workers[i].name, workers[i].sess)
		if out, err := exec.Command("tmux", "split-window", "-t", tmuxSession, "bash", "-c", paneCmd).CombinedOutput(); err != nil {
			log.Printf("tmux split-window for %s: %v\n%s", workers[i].name, err, out)
			continue
		}
		// Set rich pane title
		paneTitle := watch.FormatPaneTitle(workers[i].name, workers[i].sess)
		exec.Command("tmux", "select-pane", "-t", tmuxSession, "-T", paneTitle).Run()

		paneMappings = append(paneMappings, watch.PaneMapping{
			PaneIndex: i,
			SlotName:  workers[i].name,
			StateDir:  workers[i].stateDir,
		})

		// Re-tile after each split to keep things balanced
		exec.Command("tmux", "select-layout", "-t", tmuxSession, "tiled").Run()
	}

	// Enable pane titles display with rich border format
	exec.Command("tmux", "set-option", "-t", tmuxSession, "pane-border-status", "top").Run()
	exec.Command("tmux", "set-option", "-t", tmuxSession, "pane-border-format",
		"#[fg=white,bold] #{pane_title}").Run()

	// Kill stale updater processes before starting new one
	exec.Command("pkill", "-f", "maestro _watch-updater").Run()

	// Write pane mapping and start background updater to keep titles fresh
	if err := watch.WritePaneMap(watch.PaneMapFile, paneMappings); err != nil {
		log.Printf("[watch] warn: write pane map: %v (titles won't auto-refresh)", err)
	} else {
		updaterArgs := []string{selfBin, "_watch-updater"}
		for _, c := range configs {
			updaterArgs = append(updaterArgs, "--config", c)
		}
		updater := exec.Command(updaterArgs[0], updaterArgs[1:]...)
		updater.Stdout = nil
		updater.Stderr = nil
		if err := updater.Start(); err != nil {
			log.Printf("[watch] warn: start updater: %v (titles won't auto-refresh)", err)
		}
		// Detach — the updater will exit when the watch session dies
	}

	// Attach — replaces current process
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		log.Fatalf("find tmux: %v", err)
	}
	syscall.Exec(tmuxPath, []string{"tmux", "attach", "-t", tmuxSession}, os.Environ())
	log.Fatalf("exec tmux attach: should not reach here")
}

func watchUpdaterCmd(args []string) {
	fs := flag.NewFlagSet("_watch-updater", flag.ExitOnError)
	var configs multiFlag
	fs.Var(&configs, "config", "Path to config file (can be repeated)")
	fs.Parse(args)

	// The updater runs as a background daemon, refreshing pane titles every 3 seconds
	watch.RunUpdater(watch.PaneMapFile, 3*time.Second)
}

func watchTailCmd(args []string) {
	if len(args) < 2 {
		log.Fatal("usage: maestro _watch-tail <logfile> <tmux-session>")
	}
	watch.TailFiltered(args[0], args[1])
}

func spawnCmd(args []string) {
	fs := flag.NewFlagSet("spawn", flag.ExitOnError)
	var configs multiFlag
	fs.Var(&configs, "config", "Path to config file (can be repeated)")
	issueNum := fs.Int("issue", 0, "Issue number")
	promptPath := fs.String("prompt", "", "Path to worker prompt base file")
	fs.Parse(args)

	if *issueNum == 0 {
		fmt.Fprintln(os.Stderr, "error: --issue is required")
		os.Exit(1)
	}

	cfgs := loadConfigs(configs)
	if len(cfgs) > 1 {
		fmt.Fprintln(os.Stderr, "error: spawn requires a single --config (ambiguous with multiple projects)")
		os.Exit(1)
	}
	cfg := cfgs[0]

	s, err := state.Load(cfg.StateDir)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	// Load prompt: flag > config.WorkerPrompt > built-in fallback
	resolvedPromptPath := *promptPath
	if resolvedPromptPath == "" {
		resolvedPromptPath = cfg.WorkerPrompt
	}
	var promptBase string
	if resolvedPromptPath != "" {
		data, err := os.ReadFile(resolvedPromptPath)
		if err != nil {
			log.Fatalf("read prompt: %v", err)
		}
		promptBase = string(data)
	} else {
		promptBase = "You are a coding agent. Implement the given issue."
	}

	// Fetch issue details via gh CLI
	gh := github.New(cfg.Repo)
	issues, err := gh.ListOpenIssues(nil)
	if err != nil {
		log.Fatalf("fetch issues: %v", err)
	}

	var targetIssue *github.Issue
	for i := range issues {
		if issues[i].Number == *issueNum {
			targetIssue = &issues[i]
			break
		}
	}
	if targetIssue == nil {
		log.Fatalf("issue #%d not found in open issues", *issueNum)
	}

	// Resolve backend via 3-tier priority: label → auto-routing → default
	r := router.New(cfg)
	backendName, _ := r.ResolveBackend(*targetIssue)
	slotName, err := worker.Start(cfg, s, cfg.Repo, *targetIssue, promptBase, backendName)
	if err != nil {
		log.Fatalf("start worker: %v", err)
	}

	if err := state.Save(cfg.StateDir, s); err != nil {
		log.Fatalf("save state: %v", err)
	}

	fmt.Printf("Started worker %s for issue #%d: %s\n", slotName, *issueNum, targetIssue.Title)
}

func stopCmd(args []string) {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	var configs multiFlag
	fs.Var(&configs, "config", "Path to config file (can be repeated)")
	sessionName := fs.String("session", "", "Session name to stop")
	fs.Parse(args)

	if *sessionName == "" {
		fmt.Fprintln(os.Stderr, "error: --session is required")
		os.Exit(1)
	}

	cfgs := loadConfigs(configs)

	// Search across all projects for the session
	for _, cfg := range cfgs {
		s, err := state.Load(cfg.StateDir)
		if err != nil {
			continue
		}
		sess, ok := s.Sessions[*sessionName]
		if !ok {
			continue
		}

		if err := worker.Stop(cfg, *sessionName, sess); err != nil {
			log.Fatalf("stop worker: %v", err)
		}

		delete(s.Sessions, *sessionName)
		if err := state.Save(cfg.StateDir, s); err != nil {
			log.Fatalf("save state: %v", err)
		}

		fmt.Printf("Stopped and removed session %s\n", *sessionName)
		return
	}

	log.Fatalf("session %s not found", *sessionName)
}

func killCmd(args []string) {
	fs := flag.NewFlagSet("kill", flag.ExitOnError)
	var configs multiFlag
	fs.Var(&configs, "config", "Path to config file (can be repeated)")
	fs.Parse(args)
	args = fs.Args()

	if len(args) == 0 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "error: slot name is required\nUsage: maestro kill <slot>")
		os.Exit(1)
	}

	slotName := args[0]
	cfgs := loadConfigs(configs)

	// Search across all projects for the session
	for _, cfg := range cfgs {
		s, err := state.Load(cfg.StateDir)
		if err != nil {
			continue
		}
		sess, ok := s.Sessions[slotName]
		if !ok {
			continue
		}

		if err := worker.Stop(cfg, slotName, sess); err != nil {
			log.Fatalf("kill worker: %v", err)
		}

		now := time.Now().UTC()
		sess.Status = state.StatusDead
		sess.FinishedAt = &now

		if err := state.Save(cfg.StateDir, s); err != nil {
			log.Fatalf("save state: %v", err)
		}

		n := notify.NewWithToken(cfg.Telegram.BotToken, cfg.Telegram.Target, cfg.Telegram.Mode, cfg.Telegram.OpenclawURL)
		n.Sendf("maestro: manually killed worker %s (issue #%d: %s)", slotName, sess.IssueNumber, sess.IssueTitle)

		fmt.Printf("Killed session %s (issue #%d: %s)\n", slotName, sess.IssueNumber, sess.IssueTitle)
		return
	}

	fmt.Fprintf(os.Stderr, "error: session %q not found\n", slotName)
	os.Exit(1)
}

func importCmd(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	var configs multiFlag
	fs.Var(&configs, "config", "Path to config file (can be repeated)")
	fs.Parse(args)

	cfgs := loadConfigs(configs)

	for i, cfg := range cfgs {
		if len(cfgs) > 1 {
			if i > 0 {
				fmt.Println()
			}
			fmt.Printf("=== %s ===\n", cfg.Repo)
		}

		s, err := state.Load(cfg.StateDir)
		if err != nil {
			log.Printf("load state for %s: %v", cfg.Repo, err)
			continue
		}

		results, err := worker.Import(cfg, s)
		if err != nil {
			log.Printf("import for %s: %v", cfg.Repo, err)
			continue
		}

		if len(results) == 0 {
			fmt.Println("No worktrees found to import.")
			continue
		}

		imported := 0
		skipped := 0
		for _, r := range results {
			if r.Skipped {
				fmt.Printf("  skip: %s (%s) — %s\n", r.SlotName, r.Branch, r.SkipReason)
				skipped++
			} else {
				fmt.Printf("  imported: %s → issue #%d [%s]\n", r.SlotName, r.IssueNumber, r.Status)
				imported++
			}
		}

		fmt.Printf("\nImported %d, skipped %d.\n", imported, skipped)

		if imported > 0 {
			if err := state.Save(cfg.StateDir, s); err != nil {
				log.Printf("save state for %s: %v", cfg.Repo, err)
				continue
			}
			fmt.Printf("State saved to %s\n", state.StatePath(cfg.StateDir))
		}
	}
}

func historyCmd(args []string) {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to config file")
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	limit := fs.Int("limit", 20, "Number of recent sessions to show")
	prune := fs.Bool("prune", false, "Remove sessions older than retention period")
	retentionDays := fs.Int("retention-days", 30, "Retention period in days for pruning")
	fs.Parse(args)

	cfg := loadConfig(*configPath)

	s, err := state.Load(cfg.StateDir)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	if *prune {
		maxAge := time.Duration(*retentionDays) * 24 * time.Hour
		pruned := s.PruneOldSessions(maxAge)
		if pruned > 0 {
			if err := state.Save(cfg.StateDir, s); err != nil {
				log.Fatalf("save state: %v", err)
			}
		}
		fmt.Printf("Pruned %d sessions older than %d days.\n", pruned, *retentionDays)
		return
	}

	completed := s.CompletedSessions()
	if *limit > 0 && len(completed) > *limit {
		completed = completed[:*limit]
	}

	if *jsonOutput {
		type jsonEntry struct {
			Session    string `json:"session"`
			Issue      int    `json:"issue"`
			Title      string `json:"title"`
			Status     string `json:"status"`
			PRNumber   int    `json:"pr_number,omitempty"`
			Duration   string `json:"duration"`
			FinishedAt string `json:"finished_at,omitempty"`
			Backend    string `json:"backend,omitempty"`
		}
		entries := make([]jsonEntry, 0, len(completed))
		for _, c := range completed {
			entry := jsonEntry{
				Session:  c.SlotName,
				Issue:    c.IssueNumber,
				Title:    c.IssueTitle,
				Status:   string(c.Status),
				PRNumber: c.PRNumber,
				Duration: sessionDuration(c.Session),
				Backend:  c.Backend,
			}
			if c.FinishedAt != nil {
				entry.FinishedAt = c.FinishedAt.Format(time.RFC3339)
			}
			entries = append(entries, entry)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(entries)
		return
	}

	if len(completed) == 0 {
		fmt.Println("No completed sessions.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tISSUE\tOUTCOME\tPR\tDURATION\tFINISHED\tTITLE")
	fmt.Fprintln(w, "-------\t-----\t-------\t--\t--------\t--------\t-----")
	for _, c := range completed {
		pr := "-"
		if c.PRNumber > 0 {
			pr = fmt.Sprintf("#%d", c.PRNumber)
		}
		finished := "-"
		if c.FinishedAt != nil {
			finished = formatRelativeTime(*c.FinishedAt)
		}
		fmt.Fprintf(w, "%s\t#%d\t%s\t%s\t%s\t%s\t%s\n",
			c.SlotName, c.IssueNumber, outcomeLabel(c.Status),
			pr, sessionDuration(c.Session), finished, truncate(c.IssueTitle, 40))
	}
	w.Flush()
}

func cleanupCmd(args []string) {
	fs := flag.NewFlagSet("cleanup", flag.ExitOnError)
	var configs multiFlag
	fs.Var(&configs, "config", "Path to config file (can be repeated)")
	fs.Parse(args)

	cfgs := loadConfigs(configs)

	totalRemoved := 0
	totalErrors := 0

	for _, cfg := range cfgs {
		s, err := state.Load(cfg.StateDir)
		if err != nil {
			log.Printf("load state for %s: %v", cfg.Repo, err)
			continue
		}

		results := worker.CleanupWorktrees(cfg, s)
		if len(results) == 0 {
			if len(cfgs) > 1 {
				fmt.Printf("[%s] No worktrees to clean up.\n", cfg.Repo)
			}
			continue
		}

		for _, r := range results {
			if r.Removed {
				fmt.Printf("  removed: %s (issue #%d) — %s\n", r.SlotName, r.IssueNumber, r.Worktree)
				totalRemoved++
			} else {
				fmt.Printf("  error:   %s (issue #%d) — %v\n", r.SlotName, r.IssueNumber, r.Error)
				totalErrors++
			}
		}

		if err := state.Save(cfg.StateDir, s); err != nil {
			log.Printf("save state for %s: %v", cfg.Repo, err)
		}
	}

	fmt.Printf("\nCleaned up %d worktree(s)", totalRemoved)
	if totalErrors > 0 {
		fmt.Printf(", %d error(s)", totalErrors)
	}
	fmt.Println(".")
}

func versionBumpCmd(args []string) {
	fs := flag.NewFlagSet("version-bump", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to config file")
	prNumber := fs.Int("pr", 0, "PR number to read labels/commits from")
	fs.Parse(args)

	if *prNumber == 0 {
		fmt.Fprintln(os.Stderr, "error: --pr is required")
		os.Exit(1)
	}

	cfg := loadConfig(*configPath)
	gh := github.New(cfg.Repo)

	if err := versioning.Run(cfg, gh, *prNumber); err != nil {
		log.Fatalf("version bump: %v", err)
	}

	fmt.Println("Version bump complete.")
}

func sessionDuration(sess *state.Session) string {
	end := time.Now()
	if sess.FinishedAt != nil {
		end = *sess.FinishedAt
	}
	d := end.Sub(sess.StartedAt).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

func outcomeLabel(status state.SessionStatus) string {
	switch status {
	case state.StatusDone:
		return "merged"
	case state.StatusDead:
		return "died"
	case state.StatusConflictFailed:
		return "conflict"
	case state.StatusFailed:
		return "failed"
	default:
		return string(status)
	}
}

func formatRelativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		days := int(d.Hours()) / 24
		if days == 1 {
			return "1d ago"
		}
		return fmt.Sprintf("%dd ago", days)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
