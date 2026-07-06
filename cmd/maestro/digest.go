package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/digest"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/server"
	"github.com/befeast/maestro/internal/state"
)

// digestCmd implements `maestro digest`: a one-shot morning operator report
// aggregated across every fleet project (#703). It writes a Markdown report
// (Obsidian-vault friendly) and pings the notifier when decisions are pending.
func digestCmd(args []string) {
	fs := flag.NewFlagSet("digest", flag.ExitOnError)
	fleetPath := fs.String("fleet", "", "Path to fleet YAML file covering every project")
	var configs multiFlag
	fs.Var(&configs, "config", "Path to config file (can be repeated)")
	storePath, storeProject := configStoreFlags(fs)
	outPath := fs.String("out", "", "Markdown report destination: a directory (file named maestro-digest-YYYY-MM-DD.md) or a .md file path; empty prints to stdout only")
	doNotify := fs.Bool("notify", true, "Send a notifier summary when decide-today items > 0")
	staleHours := fs.Int("stale-review-hours", 24, "Age threshold (hours) before unresolved review findings are surfaced")
	jsonOut := fs.Bool("json", false, "Print the report as JSON to stdout instead of Markdown")
	fs.Parse(args)

	now := time.Now()
	projects, notifierCfg := loadDigestProjects(*fleetPath, configs, *storePath, *storeProject)

	report := digest.Collect(projects, digest.Options{
		Now:            now,
		StaleReviewAge: time.Duration(*staleHours) * time.Hour,
	})

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			log.Fatalf("encode report: %v", err)
		}
	} else {
		fmt.Print(report.Markdown())
	}

	writtenPath := ""
	if strings.TrimSpace(*outPath) != "" {
		path, err := writeDigestReport(*outPath, report.Markdown(), now)
		if err != nil {
			log.Fatalf("write report: %v", err)
		}
		writtenPath = path
		log.Printf("digest report written to %s", path)
	}

	if *doNotify && report.DecideTodayCount() > 0 {
		if notifierCfg == nil {
			log.Printf("digest: %d decision(s) pending but no project has a notifier target configured", report.DecideTodayCount())
		} else {
			n := notify.NewWithToken(notifierCfg.Telegram.BotToken, notifierCfg.Telegram.Target, notifierCfg.Telegram.Mode, notifierCfg.Telegram.OpenclawURL)
			if err := n.Send(report.NotifySummary(writtenPath)); err != nil {
				log.Printf("digest: notifier send failed: %v", err)
			}
		}
	}
}

// loadDigestProjects resolves the fleet membership (fleet.yaml, repeated
// --config flags, maestro.d/, or default discovery) into digest inputs.
// The returned notifier config is the first project with a notify target.
func loadDigestProjects(fleetPath string, configs multiFlag, storePath, storeProject string) ([]digest.Project, *config.Config) {
	var named []struct {
		name string
		cfg  *config.Config
	}
	if strings.TrimSpace(fleetPath) != "" {
		fleet, err := server.LoadFleetProjects(fleetPath)
		if err != nil {
			log.Fatalf("load fleet: %v", err)
		}
		for i := range fleet {
			named = append(named, struct {
				name string
				cfg  *config.Config
			}{fleet[i].Name, fleet[i].Cfg()})
		}
	} else {
		for _, cfg := range loadConfigsWithStore(configs, storePath, storeProject) {
			named = append(named, struct {
				name string
				cfg  *config.Config
			}{"", cfg})
		}
	}

	// #823: configure GitHub App auth from the first project carrying a complete
	// github_app block so the digest's reads use the installation token and the
	// report's auth line matches the daemon. A setup failure is non-fatal — the
	// command stays on PAT/gh and the report says so.
	for _, entry := range named {
		if entry.cfg == nil || !entry.cfg.GitHubApp.Configured() {
			continue
		}
		app := entry.cfg.GitHubApp
		if err := github.ConfigureAppAuth(app.AppID, app.InstallationID, app.PrivateKeyPath); err != nil {
			log.Printf("digest: github app auth setup failed (using PAT/gh): %v", err)
		}
		break
	}

	var projects []digest.Project
	var notifierCfg *config.Config
	for _, entry := range named {
		cfg := entry.cfg
		if cfg == nil {
			continue
		}
		st, err := state.Load(cfg.StateDir)
		if err != nil {
			log.Printf("warn: load state for %s: %v (sections from state will be empty)", cfg.Repo, err)
			st = nil
		}
		p := digest.ProjectFromConfig(entry.name, cfg, st, github.New(cfg.Repo))
		projects = append(projects, p)
		if notifierCfg == nil && strings.TrimSpace(cfg.Telegram.Target) != "" {
			notifierCfg = cfg
		}
	}
	if len(projects) == 0 {
		log.Fatalf("digest: no projects loaded")
	}
	return projects, notifierCfg
}

// writeDigestReport writes the Markdown report. out may be a directory (the
// file is named maestro-digest-YYYY-MM-DD.md inside it) or a .md file path.
func writeDigestReport(out, markdown string, now time.Time) (string, error) {
	path := out
	if !strings.HasSuffix(strings.ToLower(out), ".md") {
		path = filepath.Join(out, fmt.Sprintf("maestro-digest-%s.md", now.Format("2006-01-02")))
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(path, []byte(markdown), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
