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

	"github.com/befeast/maestro/internal/approver"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/configwatch"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/orchestrator"
	"github.com/befeast/maestro/internal/outcome"
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
  drain         Stop spawning new workers and wait for in-flight workers to finish
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

Drain flags:
  --timeout duration    Max wait for in-flight workers to finish (default 30m)

  Graceful drain (#541): sets a state flag so the running supervisor stops
  spawning new workers, then waits for in-flight workers to finish. Exit 0 once
  drained, exit 1 on timeout (the flag is left set so you can investigate). Wire
  it as the systemd ExecStop so "systemctl --user restart" drains first:
    ExecStop=/usr/local/bin/maestro drain --config <cfg> --timeout 30m
  The drain flag clears automatically when the supervisor next starts.

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

// reorderArgs moves flag tokens ahead of positional arguments so that flags
// can be supplied in any position on the command line. Go's stdlib flag
// package stops parsing at the first non-flag argument, which makes the
// intuitive `maestro kill <slot> --config <path>` order fail. Reordering args
// before handing them to a FlagSet lets flags follow positionals.
//
// fs is used only to inspect which flags are registered and which are
// boolean (value-less); it is not parsed here. A literal `--` terminates
// reordering: it and everything after it is treated as positional and kept in
// place. Bare tokens that do not name a registered flag stay positional, so an
// unknown `--foo` is left untouched for the FlagSet to report.
func reorderArgs(fs *flag.FlagSet, args []string) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]

		// A literal `--` ends flag processing; keep the rest verbatim.
		if arg == "--" {
			positionals = append(positionals, args[i:]...)
			break
		}

		name, hasValue := flagToken(arg)
		if name == "" {
			// Not a flag-looking token (e.g. a positional, "-", or "").
			positionals = append(positionals, arg)
			continue
		}

		f := fs.Lookup(name)
		if f == nil {
			// Unknown flag: leave it in place so the FlagSet can report it.
			positionals = append(positionals, arg)
			continue
		}

		flags = append(flags, arg)

		// A non-boolean flag in `--flag value` form consumes the next token as
		// its value; pull it along so it is not stranded as a positional.
		if !hasValue && i+1 < len(args) && !isBoolFlag(f) {
			i++
			flags = append(flags, args[i])
		}
	}

	return append(flags, positionals...)
}

// flagToken reports the flag name for a `-flag`, `--flag`, `-flag=value`, or
// `--flag=value` token. It returns an empty name for non-flag tokens. hasValue
// is true when the token carries an inline `=value`.
func flagToken(arg string) (name string, hasValue bool) {
	if len(arg) < 2 || arg[0] != '-' {
		return "", false
	}
	body := arg[1:]
	if body[0] == '-' {
		body = body[1:]
	}
	// "--" (now empty body) or "-" are not flags.
	if body == "" || body[0] == '-' {
		return "", false
	}
	if eq := strings.IndexByte(body, '='); eq >= 0 {
		return body[:eq], true
	}
	return body, false
}

// isBoolFlag reports whether a registered flag is boolean, i.e. it does not
// consume a following argument as its value.
func isBoolFlag(f *flag.Flag) bool {
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
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
	case "night-start":
		nightStartCmd(args)
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
	case "drain":
		drainCmd(args)
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
	logConfigWarnings(cfg)
	return cfg
}

// logConfigWarnings emits any non-fatal configuration issues at WARN
// level so an operator notices a hands-off project that has merge_pr in
// approval_required (etc.) without grepping the daemon journal. #425.
func logConfigWarnings(cfg *config.Config) {
	for _, msg := range cfg.Warnings() {
		log.Printf("warn: %s", msg)
	}
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
			logConfigWarnings(cfg)
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
		for _, cfg := range cfgs {
			logConfigWarnings(cfg)
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
			srv.SetActionDeps(github.New(cfg.Repo), nil)
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
				srv.SetActionDeps(github.New(c.Repo), nil)
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

	// Phase 1.2 (#499): supervise-loop liveness watchdog.
	//
	// The main loop blocks on RunOnce; if RunOnce wedges (deadlock,
	// runaway upstream call, network stall) the daemon emits no
	// further log lines and there is no in-band signal that anything
	// is wrong. A separate goroutine — running on its own ticker —
	// reads state.json directly and warns when LastRunOnceAt has not
	// been bumped in 3*interval. The warning persists into state
	// (SupervisorStuck=true) so the Fleet API and dashboards can
	// surface it without grepping the journal.
	go superviseWatchdog(ctx, cfg.StateDir, *interval)

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

// superviseWatchdog emits a loud log warning + persists SupervisorStuck=true
// in state.json when the supervise loop has not stamped state.LastRunOnceAt
// within 3*interval. It is a defense against silent wedges (#499).
//
// The ticker fires every interval (matching the supervise cadence so we
// don't over-poll the state file). On startup we grant a 3*interval grace
// before the first check, since LastRunOnceAt might be old after a daemon
// restart.
func superviseWatchdog(ctx context.Context, stateDir string, interval time.Duration) {
	if interval <= 0 {
		return
	}
	stuckThreshold := 3 * interval
	startedAt := time.Now()

	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		// Startup grace: don't warn before we've had time to see one
		// full cycle complete.
		if time.Since(startedAt) < stuckThreshold {
			continue
		}

		st, err := state.Load(stateDir)
		if err != nil {
			log.Printf("[supervise/watchdog] could not read state: %v (will retry)", err)
			continue
		}
		if st.LastRunOnceAt.IsZero() {
			// Daemon never completed a cycle. Loud warning, but only
			// after the startup grace.
			log.Printf("[supervise/watchdog] WARNING: no RunOnce has stamped state.LastRunOnceAt since the daemon started %s ago; check whether RunOnce is wedged",
				time.Since(startedAt).Round(time.Second))
			persistSupervisorStuck(stateDir, "no RunOnce has completed since daemon start")
			continue
		}
		gap := time.Since(st.LastRunOnceAt)
		if gap > stuckThreshold {
			reason := fmt.Sprintf("LastRunOnceAt=%s is %s old (threshold %s)",
				st.LastRunOnceAt.Format(time.RFC3339), gap.Round(time.Second), stuckThreshold)
			log.Printf("[supervise/watchdog] WARNING: supervise loop appears stuck — %s", reason)
			persistSupervisorStuck(stateDir, reason)
		}
	}
}

// persistSupervisorStuck flips SupervisorStuck=true in state.json so the
// Fleet API surfaces it. Best-effort: a save failure is logged but does
// not stop the watchdog.
func persistSupervisorStuck(stateDir, reason string) {
	st, err := state.Load(stateDir)
	if err != nil {
		log.Printf("[supervise/watchdog] persist: load state: %v", err)
		return
	}
	if st.SupervisorStuck && st.SupervisorStuckReason == reason {
		return // already set with the same reason; avoid log/save churn
	}
	st.SupervisorStuck = true
	st.SupervisorStuckReason = reason
	if err := state.Save(stateDir, st); err != nil {
		log.Printf("[supervise/watchdog] persist: save state: %v", err)
	}
}

func superviseApprovalCmd(action string, args []string, defaultConfigPath string) {
	fs := flag.NewFlagSet("supervise "+action, flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath, "Path to config file")
	actor := fs.String("actor", "cli", "Audit actor")
	reason := fs.String("reason", "", "Audit reason")
	fs.Parse(reorderArgs(fs, args))
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

	if action != "approve" {
		fmt.Printf("Approval %s %s.\n", approval.ID, approval.Status)
		return
	}

	// Execute the now-approved approval. Synchronous and blocking; the
	// caller (operator) sees the result on stdout and via exit code.
	ex := &approver.Executor{
		GH:        github.New(cfg.Repo),
		Worktrees: approver.WorktreeRemoverFunc(worker.RemoveWorktree),
		Cfg:       cfg,
		Sessions:  approver.SessionLookupFunc(st.SessionAt),
	}
	res := ex.Execute(approval)

	now = time.Now().UTC()
	switch res.Status {
	case state.ApprovalStatusExecuted:
		if _, mErr := st.MarkApprovalExecuted(approval.ID, now, *actor, res.Summary); mErr != nil {
			log.Fatalf("supervise approve: mark executed: %v", mErr)
		}
	case state.ApprovalStatusExecutionSkipped:
		if _, mErr := st.MarkApprovalExecutionSkipped(approval.ID, now, *actor, res.Summary); mErr != nil {
			log.Fatalf("supervise approve: mark skipped: %v", mErr)
		}
	default: // execution_failed
		msg := res.Summary
		if msg == "" && res.Err != nil {
			msg = res.Err.Error()
		}
		if _, mErr := st.MarkApprovalExecutionFailed(approval.ID, now, *actor, msg); mErr != nil {
			log.Fatalf("supervise approve: mark failed: %v", mErr)
		}
	}

	if err := state.Save(cfg.StateDir, st); err != nil {
		log.Fatalf("supervise approve: save state after execution: %v", err)
	}

	switch res.Status {
	case state.ApprovalStatusExecuted:
		fmt.Printf("Approval %s executed: %s\n", approval.ID, res.Summary)
	case state.ApprovalStatusExecutionSkipped:
		fmt.Printf("Approval %s skipped: %s\n", approval.ID, res.Summary)
	default:
		if res.Err != nil {
			log.Fatalf("Approval %s execution failed: %v", approval.ID, res.Err)
		}
		log.Fatalf("Approval %s execution failed: %s", approval.ID, res.Summary)
	}
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
			// LoadFleetProjects can't import github (cycle); wire the
			// per-project safe-action GH client here at the boundary.
			for i := range projects {
				if cfg := projects[i].Cfg(); cfg != nil {
					gh := github.New(cfg.Repo)
					projects[i].SetActionGH(gh)
					// #529: surface the GitHub Project board (WIP
					// rollup + URL) in the fleet snapshot when the
					// project enables github_projects.
					if cfg.GitHubProjects.Enabled && cfg.GitHubProjects.ProjectNumber > 0 {
						projects[i].SetBoardClient(gh, cfg.GitHubProjects.ProjectNumber)
					}
				}
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
		fleetSrv := server.NewFleet(projects, fleetHost, *port, *readOnly)
		// #487: fleet auth uses the first project that configures a token
		// env. A single shared token across the fleet is the deployment
		// expectation; per-project tokens are out of scope.
		fleetSrv.SetAuth(fleetAuthFromProjects(projects))
		if err := fleetSrv.Start(ctx); err != nil {
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
	srv := server.New(cfg, refreshCh)
	srv.SetActionDeps(github.New(cfg.Repo), nil)
	if err := srv.Start(ctx); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func fleetProjectsFromConfigs(cfgs []*config.Config) []server.FleetProject {
	projects := make([]server.FleetProject, 0, len(cfgs))
	for _, cfg := range cfgs {
		proj := server.NewFleetProject(defaultFleetProjectName(cfg.Repo), cfg.ResolvePath(), "", cfg)
		gh := github.New(cfg.Repo)
		proj.SetActionGH(gh)
		if cfg.GitHubProjects.Enabled && cfg.GitHubProjects.ProjectNumber > 0 {
			proj.SetBoardClient(gh, cfg.GitHubProjects.ProjectNumber)
		}
		projects = append(projects, proj)
	}
	return projects
}

// fleetAuthFromProjects returns the first non-empty Server.Auth config across
// the fleet. The fleet uses a single shared token (#487); per-project
// distinct tokens are intentionally out of scope.
func fleetAuthFromProjects(projects []server.FleetProject) config.ServerAuthConfig {
	for i := range projects {
		cfg := projects[i].Cfg()
		if cfg == nil {
			continue
		}
		if strings.TrimSpace(cfg.Server.Auth.TokenEnv) != "" {
			return cfg.Server.Auth
		}
	}
	return config.ServerAuthConfig{}
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
		var results []projectStatusJSON
		for _, cfg := range cfgs {
			s, err := state.Load(cfg.StateDir)
			if err != nil {
				log.Printf("load state for %s: %v", cfg.Repo, err)
				continue
			}
			results = append(results, buildProjectStatusJSON(cfg, s))
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

type projectStatusJSON struct {
	Repo    string         `json:"repo"`
	Prefix  string         `json:"prefix"`
	Outcome outcome.Status `json:"outcome"`
	State   *state.State   `json:"state"`
}

func buildProjectStatusJSON(cfg *config.Config, s *state.State) projectStatusJSON {
	return projectStatusJSON{
		Repo:    cfg.Repo,
		Prefix:  cfg.SessionPrefix,
		Outcome: outcomeStatusForState(cfg, s),
		State:   s,
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
		enc.Encode(buildProjectStatusJSON(cfg, s))
		return
	}

	// Pretty table
	fmt.Printf("Repo:           %s\n", cfg.Repo)
	fmt.Printf("Session prefix: %s\n", cfg.SessionPrefix)
	fmt.Printf("State file:     %s\n", state.StatePath(cfg.StateDir))
	fmt.Printf("Max parallel:   %d\n", cfg.MaxParallel)
	if s.RestartRequired {
		reason := s.RestartRequiredReason
		if reason == "" {
			reason = "model/routing config changed"
		}
		fmt.Printf("Restart req.:   yes — %s (run `systemctl --user restart` to apply)\n", reason)
	}
	showOutcomeStatus(cfg, s)
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
		var alivePtr *bool
		if sess.Status == state.StatusRunning {
			isAlive := worker.IsAlive(sess.PID)
			alivePtr = &isAlive
			if isAlive {
				alive = "yes"
			} else {
				alive = "no"
			}
		}
		displayStatus := state.SessionDisplayStatusFor(sess, alivePtr)
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
			name, sess.IssueNumber, displayStatus, backend, pr, ci, sess.PID, alive, age, retries, tokens, truncate(sess.IssueTitle, 50))
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

func showOutcomeStatus(cfg *config.Config, s *state.State) {
	status := outcomeStatusForState(cfg, s)
	if !status.Configured {
		fmt.Println("Outcome:        not configured (issue throughput alone is not enough)")
		fmt.Printf("Outcome next:   %s\n", status.NextAction)
		return
	}
	goal := valueOrDash(status.Goal)
	runtimeTarget := valueOrDash(status.RuntimeTarget)
	if strings.TrimSpace(status.RuntimeHost) != "" {
		runtimeTarget += " (" + status.RuntimeHost + ")"
	}
	fmt.Printf("Outcome:        %s\n", goal)
	fmt.Printf("Runtime target: %s\n", runtimeTarget)
	fmt.Printf("Outcome health: %s\n", valueOrDash(status.HealthState))
	if status.HealthCheckedAt != "" {
		fmt.Printf("Outcome checked: %s\n", status.HealthCheckedAt)
	}
	if status.HealthSummary != "" {
		fmt.Printf("Outcome check:  %s\n", status.HealthSummary)
	}
	if status.NextAction != "" {
		fmt.Printf("Outcome next:   %s\n", status.NextAction)
	}
}

func outcomeStatusForState(cfg *config.Config, s *state.State) outcome.Status {
	if cfg == nil || s == nil {
		return outcome.StatusFor(outcome.Brief{}, 0, time.Time{})
	}
	if s.OutcomeHealth != nil {
		return outcome.StatusFor(cfg.Outcome, s.DonePRCount(), s.LastMergeAt, *s.OutcomeHealth)
	}
	return outcome.StatusFor(cfg.Outcome, s.DonePRCount(), s.LastMergeAt)
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
	var pending []state.Approval
	historyCounts := make(map[state.ApprovalStatus]int)
	historyTotal := 0
	for _, approval := range approvals {
		if approval.Status == state.ApprovalStatusPending {
			pending = append(pending, approval)
			continue
		}
		historyCounts[approval.Status]++
		historyTotal++
	}
	if len(pending) == 0 {
		if historyTotal > 0 {
			fmt.Printf("Approvals: no pending approvals; %s.\n\n", approvalHistorySummary(historyCounts, historyTotal))
		}
		return
	}
	fmt.Println("Approvals:")
	for _, approval := range pending {
		target := "-"
		parts := supervisorTargetParts(approval.Target)
		if len(parts) > 0 {
			target = strings.Join(parts, ", ")
		}
		fmt.Printf("  %s  %s  %s  %s\n", approval.ID, approval.Status, approval.Action, target)
	}
	if historyTotal > 0 {
		fmt.Printf("  %s.\n", approvalHistorySummary(historyCounts, historyTotal))
	}
	fmt.Println()
}

func approvalHistorySummary(counts map[state.ApprovalStatus]int, total int) string {
	if total <= 0 {
		return "0 historical approvals hidden"
	}
	noun := "approval"
	if total != 1 {
		noun = "approvals"
	}
	known := 0
	var parts []string
	for _, status := range []state.ApprovalStatus{
		state.ApprovalStatusSuperseded,
		state.ApprovalStatusStale,
		state.ApprovalStatusApproved,
		state.ApprovalStatusRejected,
	} {
		count := counts[status]
		if count == 0 {
			continue
		}
		known += count
		parts = append(parts, fmt.Sprintf("%d %s", count, status))
	}
	if other := total - known; other > 0 {
		parts = append(parts, fmt.Sprintf("%d other", other))
	}
	summary := fmt.Sprintf("%d historical %s hidden", total, noun)
	if len(parts) > 0 {
		summary += " (" + strings.Join(parts, ", ") + ")"
	}
	return summary
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
	fs.Parse(reorderArgs(fs, args))

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

// drainDefaultTimeout is the default ceiling drain waits for in-flight workers.
const drainDefaultTimeout = 30 * time.Minute

// drainPollInterval is how often drain re-reads state to see if running
// workers have finished.
var drainPollInterval = 5 * time.Second

// errDrainTimeout is returned by drainWait when the timeout elapses before all
// in-flight workers finish.
var errDrainTimeout = errors.New("drain timed out with workers still running")

// drainWait blocks until runningCount() reports zero in-flight workers or the
// timeout elapses, polling on the given interval. It is the testable core of
// `maestro drain`: the CLI injects a state-backed runningCount and the real
// clock, tests inject a deterministic counter and a fake clock. progress is
// called once per observation (after the initial check and after each poll) so
// the CLI can log progress; it may be nil. Returns errDrainTimeout on timeout.
func drainWait(runningCount func() (int, error), timeout, interval time.Duration, now func() time.Time, sleep func(time.Duration), progress func(running int)) error {
	if interval <= 0 {
		interval = drainPollInterval
	}
	deadline := now().Add(timeout)
	for {
		running, err := runningCount()
		if err != nil {
			return err
		}
		if progress != nil {
			progress(running)
		}
		if running == 0 {
			return nil
		}
		if !now().Before(deadline) {
			return errDrainTimeout
		}
		// Do not overshoot the deadline on the final sleep.
		wait := interval
		if remaining := deadline.Sub(now()); remaining < wait {
			wait = remaining
		}
		if wait <= 0 {
			return errDrainTimeout
		}
		sleep(wait)
	}
}

func drainCmd(args []string) {
	fs := flag.NewFlagSet("drain", flag.ExitOnError)
	var configs multiFlag
	fs.Var(&configs, "config", "Path to config file (can be repeated)")
	timeout := fs.Duration("timeout", drainDefaultTimeout, "Max time to wait for in-flight workers to finish")
	fs.Parse(reorderArgs(fs, args))

	cfgs := loadConfigs(configs)
	if len(cfgs) > 1 {
		fmt.Fprintln(os.Stderr, "error: drain requires a single --config (one project at a time; see #541 out-of-scope)")
		os.Exit(1)
	}
	cfg := cfgs[0]

	// Set the drain flag so the running orchestrator stops spawning new
	// workers on its next cycle. This is a load-then-save under the state
	// lock; a concurrent orchestrator save resolves latest-write-wins.
	s, err := state.Load(cfg.StateDir)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}
	s.SetSpawnDrain(time.Now().UTC())
	if err := state.Save(cfg.StateDir, s); err != nil {
		log.Fatalf("save state (set drain): %v", err)
	}

	initialRunning := s.RunningSessionCount()
	fmt.Printf("Drain requested for %s — no new workers will be spawned. Waiting up to %s for %d in-flight worker(s) to finish...\n",
		cfg.Repo, timeout.String(), initialRunning)

	runningCount := func() (int, error) {
		cur, err := state.Load(cfg.StateDir)
		if err != nil {
			return 0, fmt.Errorf("load state: %w", err)
		}
		return cur.RunningSessionCount(), nil
	}
	lastReported := -1
	progress := func(running int) {
		if running != lastReported {
			fmt.Printf("  in-flight workers running: %d\n", running)
			lastReported = running
		}
	}

	err = drainWait(runningCount, *timeout, drainPollInterval, time.Now, time.Sleep, progress)
	if errors.Is(err, errDrainTimeout) {
		fmt.Fprintf(os.Stderr, "drain: timed out after %s with workers still running; drain flag left set so you can investigate (maestro status) or kill the stuck worker\n", timeout.String())
		os.Exit(1)
	}
	if err != nil {
		log.Fatalf("drain: %v", err)
	}

	fmt.Printf("Drained: no in-flight workers remain. Safe to restart the supervisor (the drain flag clears automatically on startup).\n")
}

func stopCmd(args []string) {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	var configs multiFlag
	fs.Var(&configs, "config", "Path to config file (can be repeated)")
	sessionName := fs.String("session", "", "Session name to stop")
	fs.Parse(reorderArgs(fs, args))

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
	fs.Parse(reorderArgs(fs, args))
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
