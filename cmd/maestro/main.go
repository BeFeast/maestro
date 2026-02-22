package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
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
  spawn     Spawn a worker for a specific issue number
  stop      Stop a worker session
  version   Print version

Run flags:
  --interval duration   Loop interval (default 10m)
  --once                Run once and exit
  --prompt string       Path to worker prompt base file

Spawn flags:
  --issue int           Issue number to work on
  --prompt string       Path to worker prompt base file

Stop flags:
  --session string      Session name to stop (e.g. pan-1)
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

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	interval := fs.Duration("interval", 10*time.Minute, "Loop interval")
	once := fs.Bool("once", false, "Run once and exit")
	promptPath := fs.String("prompt", "", "Path to worker prompt base file")
	fs.Parse(args)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

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
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	fs.Parse(args)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

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

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tISSUE\tSTATUS\tPID\tALIVE\tAGE\tTITLE")
	fmt.Fprintln(w, "-------\t-----\t------\t---\t-----\t---\t-----")
	for name, sess := range s.Sessions {
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
}

func spawnCmd(args []string) {
	fs := flag.NewFlagSet("spawn", flag.ExitOnError)
	issueNum := fs.Int("issue", 0, "Issue number")
	promptPath := fs.String("prompt", "", "Path to worker prompt base file")
	fs.Parse(args)

	if *issueNum == 0 {
		fmt.Fprintln(os.Stderr, "error: --issue is required")
		os.Exit(1)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

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
	sessionName := fs.String("session", "", "Session name to stop")
	fs.Parse(args)

	if *sessionName == "" {
		fmt.Fprintln(os.Stderr, "error: --session is required")
		os.Exit(1)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

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
