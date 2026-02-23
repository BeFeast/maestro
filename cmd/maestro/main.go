package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/orchestrator"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

const usage = `maestro - AI coding agent orchestrator

Usage:
  maestro <command> [flags]

Commands:
  run       Run the orchestration loop
  status    Show current state
  logs      Show worker logs (tail -f)
  watch     Open tmux dashboard with all worker logs
  spawn     Spawn a worker for a specific issue number
  stop      Stop a worker session
  version   Print version

Global flags:
  --config string       Path to config file (default: maestro.yaml)

Run flags:
  --interval duration   Loop interval (default 10m)
  --once                Run once and exit
  --prompt string       Path to worker prompt base file

Spawn flags:
  --issue int           Issue number to work on
  --prompt string       Path to worker prompt base file

Stop flags:
  --session string      Session name to stop (e.g. pan-1)

Logs:
  maestro logs              List active worker logs + tmux attach hints
  maestro logs <slot>       tail -f specific worker log (e.g. maestro logs pan-20)

Watch:
  maestro watch             Open tmux dashboard with all active worker logs
`

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
	case "run":
		runCmd(args)
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
	case "version":
		fmt.Println("maestro v0.1.0")
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

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to config file")
	interval := fs.Duration("interval", 10*time.Minute, "Loop interval")
	once := fs.Bool("once", false, "Run once and exit")
	promptPath := fs.String("prompt", "", "Path to worker prompt base file")
	fs.Parse(args)

	cfg := loadConfig(*configPath)

	orch := orchestrator.New(cfg)
	if err := orch.LoadPromptBase(*promptPath); err != nil {
		log.Printf("warn: load prompt: %v", err)
	}

	log.Printf("starting maestro — repo=%s interval=%s once=%v", cfg.Repo, *interval, *once)

	if err := orch.Run(*interval, *once); err != nil {
		log.Fatalf("run: %v", err)
	}
}

func statusCmd(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to config file")
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	fs.Parse(args)

	cfg := loadConfig(*configPath)

	s, err := state.Load(cfg.Repo)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(s)
		return
	}

	// Pretty table
	fmt.Printf("Repo:       %s\n", cfg.Repo)
	fmt.Printf("State file: %s\n", state.StatePath(cfg.Repo))
	fmt.Printf("Max parallel: %d\n\n", cfg.MaxParallel)

	if len(s.Sessions) == 0 {
		fmt.Println("No sessions.")
		return
	}

	// Sort session names for stable output
	names := make([]string, 0, len(s.Sessions))
	for name := range s.Sessions {
		names = append(names, name)
	}
	sort.Strings(names)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tISSUE\tSTATUS\tPID\tALIVE\tAGE\tTITLE")
	fmt.Fprintln(w, "-------\t-----\t------\t---\t-----\t---\t-----")
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
		fmt.Fprintf(w, "%s\t#%d\t%s\t%d\t%s\t%s\t%s\n",
			name, sess.IssueNumber, sess.Status, sess.PID, alive, age, truncate(sess.IssueTitle, 50))
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
}

func logsCmd(args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to config file")
	fs.Parse(args)
	args = fs.Args() // remaining args after flags

	cfg := loadConfig(*configPath)

	s, err := state.Load(cfg.Repo)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	// If a specific slot is given, exec tail -f on it
	if len(args) > 0 && args[0] != "" && !strings.HasPrefix(args[0], "-") {
		slotName := args[0]
		sess, ok := s.Sessions[slotName]
		if !ok {
			fmt.Fprintf(os.Stderr, "error: session %q not found\n", slotName)
			os.Exit(1)
		}
		if _, err := os.Stat(sess.LogFile); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "error: log file not found: %s\n", sess.LogFile)
			os.Exit(1)
		}

		// Replace process with tail -f
		tailPath, err := exec.LookPath("tail")
		if err != nil {
			log.Fatalf("find tail: %v", err)
		}
		syscall.Exec(tailPath, []string{"tail", "-f", sess.LogFile}, os.Environ())
		// If exec fails we fall through
		log.Fatalf("exec tail: should not reach here")
	}

	// No args — list all active worker logs
	names := make([]string, 0, len(s.Sessions))
	for name, sess := range s.Sessions {
		if sess.Status == state.StatusRunning {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	if len(names) == 0 {
		fmt.Println("No active worker sessions.")
		return
	}

	fmt.Println("Active worker logs:")
	logDir := ""
	for _, name := range names {
		sess := s.Sessions[name]
		alive := ""
		if !worker.IsAlive(sess.PID) {
			alive = " (dead)"
		}
		fmt.Printf("  %s (#%d): %s%s\n", name, sess.IssueNumber, sess.LogFile, alive)
		logDir = state.LogDir(cfg.Repo)
	}

	fmt.Println()
	fmt.Println("To attach to a worker:")
	for _, name := range names {
		fmt.Printf("  tmux attach -t %s\n", worker.TmuxSessionName(name))
	}

	fmt.Println()
	fmt.Printf("To watch all logs:\n  tail -f %s/pan-*.log\n", logDir)
}

func watchCmd(args []string) {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to config file")
	fs.Parse(args)

	cfg := loadConfig(*configPath)

	s, err := state.Load(cfg.Repo)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	// Collect active running sessions
	type activeWorker struct {
		name string
		sess *state.Session
	}
	var workers []activeWorker
	for name, sess := range s.Sessions {
		if sess.Status == state.StatusRunning && worker.IsAlive(sess.PID) {
			workers = append(workers, activeWorker{name, sess})
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

	// Create new session with first worker's log
	firstCmd := fmt.Sprintf("tail -f %s", workers[0].sess.LogFile)
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", tmuxSession, firstCmd).CombinedOutput(); err != nil {
		log.Fatalf("tmux new-session: %v\n%s", err, out)
	}

	// Set pane title for first pane
	paneTitle := fmt.Sprintf("%s #%d", workers[0].name, workers[0].sess.IssueNumber)
	exec.Command("tmux", "select-pane", "-t", tmuxSession+":0.0", "-T", paneTitle).Run()

	// Split for each additional worker
	for i := 1; i < len(workers); i++ {
		tailCmd := fmt.Sprintf("tail -f %s", workers[i].sess.LogFile)
		if out, err := exec.Command("tmux", "split-window", "-t", tmuxSession, tailCmd).CombinedOutput(); err != nil {
			log.Printf("tmux split-window for %s: %v\n%s", workers[i].name, err, out)
			continue
		}
		// Set pane title
		paneTitle := fmt.Sprintf("%s #%d", workers[i].name, workers[i].sess.IssueNumber)
		exec.Command("tmux", "select-pane", "-t", tmuxSession, "-T", paneTitle).Run()

		// Re-tile after each split to keep things balanced
		exec.Command("tmux", "select-layout", "-t", tmuxSession, "tiled").Run()
	}

	// Enable pane titles display
	exec.Command("tmux", "set-option", "-t", tmuxSession, "pane-border-status", "top").Run()
	exec.Command("tmux", "set-option", "-t", tmuxSession, "pane-border-format", " #{pane_title} ").Run()

	// Attach — replaces current process
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		log.Fatalf("find tmux: %v", err)
	}
	syscall.Exec(tmuxPath, []string{"tmux", "attach", "-t", tmuxSession}, os.Environ())
	log.Fatalf("exec tmux attach: should not reach here")
}

func spawnCmd(args []string) {
	fs := flag.NewFlagSet("spawn", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to config file")
	issueNum := fs.Int("issue", 0, "Issue number")
	promptPath := fs.String("prompt", "", "Path to worker prompt base file")
	fs.Parse(args)

	if *issueNum == 0 {
		fmt.Fprintln(os.Stderr, "error: --issue is required")
		os.Exit(1)
	}

	cfg := loadConfig(*configPath)

	s, err := state.Load(cfg.Repo)
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
	issues, err := gh.ListOpenIssues("")
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

	slotName, err := worker.Start(cfg, s, cfg.Repo, *targetIssue, promptBase)
	if err != nil {
		log.Fatalf("start worker: %v", err)
	}

	if err := state.Save(cfg.Repo, s); err != nil {
		log.Fatalf("save state: %v", err)
	}

	fmt.Printf("Started worker %s for issue #%d: %s\n", slotName, *issueNum, targetIssue.Title)
}

func stopCmd(args []string) {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to config file")
	sessionName := fs.String("session", "", "Session name to stop")
	fs.Parse(args)

	if *sessionName == "" {
		fmt.Fprintln(os.Stderr, "error: --session is required")
		os.Exit(1)
	}

	cfg := loadConfig(*configPath)

	s, err := state.Load(cfg.Repo)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	sess, ok := s.Sessions[*sessionName]
	if !ok {
		log.Fatalf("session %s not found", *sessionName)
	}

	if err := worker.Stop(cfg, *sessionName, sess); err != nil {
		log.Fatalf("stop worker: %v", err)
	}

	delete(s.Sessions, *sessionName)

	if err := state.Save(cfg.Repo, s); err != nil {
		log.Fatalf("save state: %v", err)
	}

	fmt.Printf("Stopped and removed session %s\n", *sessionName)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
