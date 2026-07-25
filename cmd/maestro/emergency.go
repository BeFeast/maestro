package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/configstore"
	"github.com/befeast/maestro/internal/emergencystore"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/worker"
)

// emergencyCmd is the fleet-wide EMERGENCY STOP verb (#840): one action that
// halts all LLM spend across the fleet. It writes the switch straight to the
// unified DB (--db), so it works even if the web/daemon is down — a running
// daemon picks the change up via its emergency-watch loop within one cycle. The
// flag is persistent (survives `systemctl restart maestro.service`); only
// `maestro emergency resume` clears it.
func emergencyCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: maestro emergency <stop-llm|stop-all|resume|status> [flags]")
		os.Exit(1)
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "stop-llm":
		emergencyStop(emergencystore.LevelLLMStopped, rest)
	case "stop-all":
		emergencyStop(emergencystore.LevelAllStopped, rest)
	case "resume":
		emergencyResume(rest)
	case "status":
		emergencyStatus(rest)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown emergency subcommand %q (want stop-llm|stop-all|resume|status)\n", sub)
		os.Exit(1)
	}
}

// emergencyFlags is the shared flag set: --db points at the unified maestro.db,
// --store (config store) is only consulted for the operator's Telegram config
// (notification) and, with --kill-workers, the project state dirs. --config lets
// the operator name YAML configs directly instead of the store.
type emergencyFlags struct {
	db          string
	store       string
	configs     multiFlag
	reason      string
	actor       string
	killWorkers bool
}

func emergencyFlagSet(name string) (*flag.FlagSet, *emergencyFlags) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	ef := &emergencyFlags{}
	fs.StringVar(&ef.db, "db", emergencystore.DefaultDBPath(), "Unified SQLite db the emergency switch lives in")
	fs.StringVar(&ef.store, "store", defaultConfigStorePath(), "Config store path (used for notifications and --kill-workers)")
	fs.Var(&ef.configs, "config", "Path to config file (can be repeated; alternative to --store)")
	fs.StringVar(&ef.reason, "reason", "", "Why the emergency stop was engaged (recorded + notified)")
	fs.StringVar(&ef.actor, "actor", "", "Who engaged it (default: current OS user)")
	fs.BoolVar(&ef.killWorkers, "kill-workers", true, "Deprecated no-op (always true): in-flight workers are always killed on emergency stop")
	return fs, ef
}

func emergencyStop(level emergencystore.Level, args []string) {
	fs, ef := emergencyFlagSet("emergency " + string(level))
	fs.Parse(reorderArgs(fs, args))

	store, err := emergencystore.Open(ef.db)
	if err != nil {
		log.Fatalf("open emergency switch db %s: %v", ef.db, err)
	}
	defer store.Close()

	actor := resolveActor(ef.actor)
	now := time.Now().UTC()
	if err := store.Set(context.Background(), level, actor, strings.TrimSpace(ef.reason), now); err != nil {
		log.Fatalf("engage emergency stop: %v", err)
	}

	st, _ := store.Get(context.Background())
	fmt.Printf("EMERGENCY STOP engaged — level=%s by=%s\n", st.Level, actor)
	fmt.Printf("All LLM calls and new worker spawns are halted fleet-wide; in-flight workers and attached verify/build children (java/gh/go/android) are killed. A running daemon applies this within one poll interval; the flag survives a restart.\n")
	if strings.TrimSpace(ef.reason) != "" {
		fmt.Printf("Reason: %s\n", strings.TrimSpace(ef.reason))
	}

	killed := killInFlightWorkers(ef)
	fmt.Printf("Killed %d in-flight worker(s).\n", killed)

	notifyEmergency(ef, st.ActivationMessage())
	fmt.Printf("Run `maestro emergency resume` to restore normal operation.\n")
}

func emergencyResume(args []string) {
	// resume accepts (and ignores) --llm as an alias: with a single fleet-wide
	// switch, resume clears whatever level is set.
	fs, ef := emergencyFlagSet("emergency resume")
	fs.Bool("llm", false, "Alias: clear the emergency stop (a single switch; resume clears any level)")
	fs.Parse(reorderArgs(fs, args))

	store, err := emergencystore.Open(ef.db)
	if err != nil {
		log.Fatalf("open emergency switch db %s: %v", ef.db, err)
	}
	defer store.Close()

	prev, _ := store.Get(context.Background())
	if !prev.Active() {
		fmt.Printf("No emergency stop is active — nothing to resume.\n")
		return
	}
	actor := resolveActor(ef.actor)
	if err := store.Resume(context.Background(), actor, time.Now().UTC()); err != nil {
		log.Fatalf("resume: %v", err)
	}
	cur, _ := store.Get(context.Background())
	fmt.Printf("Emergency stop cleared (was %s) by %s — normal operation resumes on the next cycle.\n", prev.Level, actor)
	notifyEmergency(ef, emergencystore.ResumeMessage(prev, cur))
}

func emergencyStatus(args []string) {
	fs, ef := emergencyFlagSet("emergency status")
	fs.Parse(reorderArgs(fs, args))

	store, err := emergencystore.Open(ef.db)
	if err != nil {
		log.Fatalf("open emergency switch db %s: %v", ef.db, err)
	}
	defer store.Close()

	st, err := store.Get(context.Background())
	if err != nil {
		log.Fatalf("read emergency switch: %v", err)
	}
	if !st.Active() {
		fmt.Printf("emergency: none — normal operation.\n")
		return
	}
	fmt.Printf("emergency: %s\n", st.Level)
	if !st.Since.IsZero() {
		fmt.Printf("since:  %s\n", st.Since.Format(time.RFC3339))
	}
	if st.Actor != "" {
		fmt.Printf("actor:  %s\n", st.Actor)
	}
	if st.Reason != "" {
		fmt.Printf("reason: %s\n", st.Reason)
	}
}

// resolveActor names who engaged the switch: an explicit --actor, else the OS
// user, else "cli".
func resolveActor(explicit string) string {
	if a := strings.TrimSpace(explicit); a != "" {
		return a
	}
	if u, err := user.Current(); err == nil && strings.TrimSpace(u.Username) != "" {
		return u.Username
	}
	return "cli"
}

// emergencyConfigs loads the project configs for the notification / kill-workers
// paths. It is best-effort: the core switch write does not depend on it, so a
// missing config store or an unreadable YAML degrades to "no notification"
// rather than failing the stop. It never calls log.Fatalf.
func emergencyConfigs(ef *emergencyFlags) []*config.Config {
	if len(ef.configs) > 0 {
		cfgs := make([]*config.Config, 0, len(ef.configs))
		for _, path := range ef.configs {
			cfg, err := config.LoadFrom(path)
			if err != nil {
				log.Printf("warn: load config %s: %v", path, err)
				continue
			}
			cfgs = append(cfgs, cfg)
		}
		return cfgs
	}
	storePath := strings.TrimSpace(ef.store)
	if storePath == "" {
		return nil
	}
	store, err := configstore.Open(storePath)
	if err != nil {
		log.Printf("warn: open config store %s for notification/kill-workers: %v", storePath, err)
		return nil
	}
	defer store.Close()
	cfgs, err := store.LoadAll(context.Background())
	if err != nil {
		log.Printf("warn: load config store %s: %v", storePath, err)
		return nil
	}
	return cfgs
}

// notifyEmergency sends the notify_red-grade alert from the first project that
// configures a notification channel (Telegram target or the ntfy transport).
// Best-effort: the switch is already written; a notification failure only logs.
func notifyEmergency(ef *emergencyFlags, msg string) {
	for _, cfg := range emergencyConfigs(ef) {
		if cfg == nil || (strings.TrimSpace(cfg.Telegram.Target) == "" && !cfg.Notify.Ntfy.Enabled()) {
			continue
		}
		n := notify.NewWithToken(cfg.Telegram.BotToken, cfg.Telegram.Target, cfg.Telegram.Mode, cfg.Telegram.OpenclawURL)
		n.WithNtfy(cfg.Notify.Ntfy.BaseURL, cfg.Notify.Ntfy.Topic, cfg.Notify.Ntfy.Token())
		n.Sendf("%s", msg)
		// Fan the notify_red-grade alert to ntfy (#1018). Keyed by message so a
		// repeated identical emergency state does not re-notify.
		if err := n.Alert(notify.AlertEmergency, "emergency", "maestro emergency", msg); err != nil {
			log.Printf("[emergency] ntfy alert failed: %v", err)
		}
		return
	}
}

// killInFlightWorkers stops every running tmux worker across the loaded projects
// (the --kill-workers escalation). Best-effort per session so one failure does
// not abort the sweep.
func killInFlightWorkers(ef *emergencyFlags) int {
	res := worker.EmergencyKillAll(emergencyConfigs(ef))
	return res.Workers + res.Attached
}
