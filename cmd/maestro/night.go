package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// Phase 1.3: hands-off night-start command. Adds a preflight gate
// before delegating to runCmd, logs the result bundle, and refuses to
// spin if any precondition fails so an unattended run can't burn
// claude credits on a known-broken state.

type nightPreflight struct {
	StartedAt       time.Time         `json:"started_at"`
	ConfigPath      string            `json:"config_path"`
	BackendsChecked map[string]string `json:"backends_checked"` // backend -> "ok" | "missing" | "exhausted" | "skipped: ..."
	StatePath       string            `json:"state_path"`
	StateRead       bool              `json:"state_read"`
	StuckSupervisor bool              `json:"stuck_supervisor"`
	StalePending    int               `json:"stale_pending_approvals"`
	MaxRuntimeMin   int               `json:"max_runtime_minutes"`
	MaxRetriesIssue int               `json:"max_retries_per_issue"`
	WorkerChain     []string          `json:"worker_chain"`
	OK              bool              `json:"ok"`
	Reason          string            `json:"reason,omitempty"`
}

// nightStartCmd is the entry point bound to "maestro night-start <args>".
func nightStartCmd(args []string) {
	fs := flag.NewFlagSet("night-start", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to project config")
	dryRun := fs.Bool("dry-run", false, "Run preflight only, do not start the loop")
	interval := fs.Duration("interval", 2*time.Minute, "Reconciliation interval (passed to runCmd)")
	fs.Parse(reorderArgs(fs, args))

	if strings.TrimSpace(*configPath) == "" {
		log.Fatalf("night-start: --config is required")
	}
	cfg, err := config.LoadFrom(*configPath)
	if err != nil {
		log.Fatalf("night-start: load config: %v", err)
	}

	report := nightPreflight{
		StartedAt:       time.Now().UTC(),
		ConfigPath:      *configPath,
		BackendsChecked: make(map[string]string),
		StatePath:       cfg.StateDir,
		MaxRuntimeMin:   cfg.MaxRuntimeMinutes,
		MaxRetriesIssue: cfg.MaxRetriesPerIssue,
	}

	// 1. Worker chain — the exact effective route. Each one we can probe at all,
	//    we probe; missing is a non-fatal warning, exhausted is a soft fail
	//    (we can still rely on later fallbacks if any of them are ok).
	chain := nightWorkerChain(cfg)
	report.WorkerChain = chain

	anyAgentic := false
	for _, name := range chain {
		def, ok := cfg.Model.Backends[name]
		if !ok {
			report.BackendsChecked[name] = "missing: not declared in config.Model.Backends"
			continue
		}
		if def.NonAgentic {
			// Should have been rejected by config.parse already, but
			// defensive double-check.
			report.BackendsChecked[name] = "skipped: non_agentic helper, not a worker driver"
			continue
		}
		status := probeBackend(name, def)
		report.BackendsChecked[name] = status
		if status == "ok" {
			anyAgentic = true
		}
	}

	// 2. State checks.
	st, err := state.Load(cfg.StateDir)
	if err != nil {
		report.StateRead = false
		report.Reason = fmt.Sprintf("state.Load failed: %v", err)
	} else {
		report.StateRead = true
		report.StuckSupervisor = st.SupervisorStuck
		// Count pending approvals older than 2h — that is "stale" by any
		// reasonable SLA. A non-zero count means a previous run left
		// orphaned mints; refuse to start so the operator triages first.
		now := time.Now().UTC()
		stale := 0
		for _, a := range st.Approvals {
			if a.Status != state.ApprovalStatusPending {
				continue
			}
			if now.Sub(a.CreatedAt) > 2*time.Hour {
				stale++
			}
		}
		report.StalePending = stale
	}

	// 3. Reason synthesis — first failing precondition wins.
	if !anyAgentic {
		report.OK = false
		if report.Reason == "" {
			report.Reason = "no agentic backend available in worker chain (all entries missing/exhausted/non-agentic)"
		}
	} else if !report.StateRead {
		report.OK = false
		// Reason already set above.
	} else if report.StuckSupervisor {
		report.OK = false
		report.Reason = "state.SupervisorStuck=true from previous run — investigate before unattended start"
	} else if report.StalePending > 0 {
		report.OK = false
		report.Reason = fmt.Sprintf("%d pending approval(s) older than 2h — clear before unattended start", report.StalePending)
	} else if report.MaxRuntimeMin <= 0 {
		report.OK = false
		report.Reason = "config.max_runtime_minutes is unbounded — refuse to run unattended without a runtime ceiling"
	} else {
		report.OK = true
	}

	// 4. Persist the preflight bundle.
	dir := filepath.Join(os.Getenv("HOME"), ".maestro", "night-runs")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, fmt.Sprintf("%s.json", report.StartedAt.Format("2006-01-02T150405Z")))
	bundle, _ := json.MarshalIndent(report, "", "  ")
	_ = os.WriteFile(path, bundle, 0o644)

	// 5. Print to operator.
	fmt.Println(string(bundle))
	if !report.OK {
		log.Fatalf("night-start: preflight FAILED — %s\n  bundle: %s", report.Reason, path)
	}
	fmt.Fprintf(os.Stderr, "\n[night-start] preflight OK · bundle %s\n", path)
	if *dryRun {
		fmt.Fprintln(os.Stderr, "[night-start] --dry-run: not entering run-loop")
		return
	}

	// 6. Delegate to runCmd with the same config + interval. runCmd has
	//    its own flag set; we re-marshal what it expects.
	runArgs := []string{"--config", *configPath, "--interval", interval.String()}
	fmt.Fprintf(os.Stderr, "[night-start] entering run-loop (interval=%s) — Ctrl+C to stop\n", interval)
	runCmd(runArgs)
}

func nightWorkerChain(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	return append([]string(nil), cfg.Model.ResolvedRoute().Backends...)
}

// probeBackend runs a tiny "say hi" probe against the backend's CLI.
// Returns "ok", "missing: …", "exhausted: …", or "unknown error: …".
// Non-agentic backends are caught upstream; this only sees worker-chain
// backends. We use a 10s timeout so a hung CLI doesn't stall the
// whole preflight.
func probeBackend(name string, def config.BackendDef) string {
	cmdLine := strings.TrimSpace(def.Cmd)
	if cmdLine == "" {
		return "missing: empty cmd"
	}
	parts := strings.Fields(cmdLine)
	if len(parts) == 0 {
		return "missing: empty cmd"
	}
	binary := parts[0]
	if _, err := exec.LookPath(binary); err != nil {
		return fmt.Sprintf("missing: %v", err)
	}
	// Probe via stdin "hi", 10s timeout. We do NOT actually verify the
	// response is correct — only that the CLI doesn't immediately exit
	// with a known rate-limit signature. This keeps probe cost minimal
	// while still catching the common "claude exhausted overnight" case.
	cmd := exec.Command(binary, parts[1:]...)
	cmd.Stdin = strings.NewReader("hi")
	timer := time.AfterFunc(10*time.Second, func() {
		_ = cmd.Process.Kill()
	})
	defer timer.Stop()
	out, _ := cmd.CombinedOutput()
	output := strings.ToLower(string(out))
	if strings.Contains(output, "you've hit your") ||
		strings.Contains(output, "rate.limit") ||
		strings.Contains(output, "quota.exceeded") ||
		strings.Contains(output, "usage limit") {
		return "exhausted: " + truncForReport(string(out))
	}
	return "ok"
}

func truncForReport(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	// Strip newlines for single-line JSON value.
	s = strings.ReplaceAll(s, "\n", " · ")
	return s
}
