package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	ghProjects "github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/server/web"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

func TestLoadFleetProjects(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	configPath := filepath.Join(dir, "project.yaml")
	if err := os.WriteFile(configPath, []byte("repo: owner/project\nstate_dir: "+stateDir+"\nsession_prefix: prj\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	fleetPath := filepath.Join(dir, "fleet.yaml")
	if err := os.WriteFile(fleetPath, []byte("projects:\n  - name: Project\n    config: project.yaml\n    dashboard_url: http://127.0.0.1:8787\n"), 0o644); err != nil {
		t.Fatalf("write fleet: %v", err)
	}

	projects, err := LoadFleetProjects(fleetPath)
	if err != nil {
		t.Fatalf("LoadFleetProjects failed: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("projects len = %d, want 1", len(projects))
	}
	if projects[0].Name != "Project" {
		t.Fatalf("project name = %q", projects[0].Name)
	}
	if projects[0].cfg == nil || projects[0].cfg.Repo != "owner/project" {
		t.Fatalf("resolved config = %+v", projects[0].cfg)
	}
	// Per-project dashboard ports were retired in #516. The legacy
	// dashboard_url in fleet.yaml is silently overridden with the scoped
	// MC route on load, regardless of what value the YAML supplied.
	if projects[0].DashboardURL != "/project/Project" {
		t.Fatalf("dashboard url = %q, want unified MC scoped route", projects[0].DashboardURL)
	}
}

func TestLoadFleetProjectsScopesDashboardURLToMC(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	configPath := filepath.Join(dir, "project.yaml")
	if err := os.WriteFile(configPath, []byte("repo: owner/project\nstate_dir: "+stateDir+"\nsession_prefix: prj\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	fleetPath := filepath.Join(dir, "fleet.yaml")
	// Project name contains a space to verify path-escaping on the scoped MC route.
	if err := os.WriteFile(fleetPath, []byte("projects:\n  - name: Has Space\n    config: project.yaml\n"), 0o644); err != nil {
		t.Fatalf("write fleet: %v", err)
	}
	projects, err := LoadFleetProjects(fleetPath)
	if err != nil {
		t.Fatalf("LoadFleetProjects failed: %v", err)
	}
	if got, want := projects[0].DashboardURL, "/project/Has%20Space"; got != want {
		t.Fatalf("scoped MC URL = %q, want %q", got, want)
	}
}

func TestFleetAPIAggregatesProjects(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	firstStateDir := filepath.Join(dir, "one")
	secondStateDir := filepath.Join(dir, "two")
	finishedOne := now.Add(-20 * time.Hour)
	startedDoneOne := finishedOne.Add(-2 * time.Hour)
	finishedTwo := now.Add(-48 * time.Hour)
	startedDoneTwo := finishedTwo.Add(-3 * time.Hour)
	saveFleetTestState(t, firstStateDir, map[string]*state.Session{
		"one-1": {
			IssueNumber:     1,
			IssueTitle:      "Build thing",
			Status:          state.StatusRunning,
			StartedAt:       now.Add(-time.Minute),
			PID:             os.Getpid(),
			Backend:         "opencode",
			TokensUsedTotal: 1234,
		},
		"one-2": {
			IssueNumber:     2,
			IssueTitle:      "Review thing",
			Status:          state.StatusDone,
			StartedAt:       startedDoneOne,
			FinishedAt:      &finishedOne,
			PRNumber:        12,
			TokensUsedTotal: 42000,
		},
	})
	saveFleetTestState(t, secondStateDir, map[string]*state.Session{
		"two-1": {
			IssueNumber:     3,
			IssueTitle:      "Broken thing",
			Status:          state.StatusRetryExhausted,
			StartedAt:       now.Add(-3 * time.Minute),
			PRNumber:        31,
			CIFailureOutput: "tests failed",
		},
		"two-2": {
			IssueNumber: 4,
			IssueTitle:  "Merged thing",
			Status:      state.StatusDone,
			StartedAt:   startedDoneTwo,
			FinishedAt:  &finishedTwo,
			PRNumber:    44,
		},
	})

	projects := []FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "http://127.0.0.1:8787", &config.Config{
			Repo: "owner/one",
			Outcome: outcome.Brief{
				DesiredOutcome: "One is deployed",
				RuntimeTarget:  "https://one.example.com",
				HealthcheckURL: "https://one.example.com/healthz",
			},
			StateDir:    firstStateDir,
			MaxParallel: 2,
			Server:      config.ServerConfig{ReadOnly: true},
		}),
		NewFleetProject("Two", "/tmp/two.yaml", "", &config.Config{
			Repo:        "owner/two",
			StateDir:    secondStateDir,
			MaxParallel: 1,
		}),
	}
	srv := NewFleet(projects, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleet(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp fleetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Summary.Projects != 2 || resp.Summary.Running != 1 || resp.Summary.PROpen != 0 || resp.Summary.Failed != 1 || resp.Summary.Sessions != 4 || resp.Summary.NeedsAttention != 1 {
		t.Fatalf("unexpected summary: %+v", resp.Summary)
	}
	if resp.Summary.ThroughputMerged7D != 2 {
		t.Fatalf("throughput merged 7d = %d, want 2", resp.Summary.ThroughputMerged7D)
	}
	if len(resp.Summary.ThroughputDaily7D) != 7 {
		t.Fatalf("throughput daily len = %d, want 7", len(resp.Summary.ThroughputDaily7D))
	}
	visibleAttention := 0
	for _, worker := range resp.Workers {
		if worker.NeedsAttention {
			visibleAttention++
		}
	}
	if resp.Summary.NeedsAttention != visibleAttention {
		t.Fatalf("summary attention = %d, visible attention rows = %d", resp.Summary.NeedsAttention, visibleAttention)
	}
	if len(resp.Attention) != resp.Summary.NeedsAttention {
		t.Fatalf("attention inbox len = %d, want %d", len(resp.Attention), resp.Summary.NeedsAttention)
	}
	if resp.Projects[0].Name != "One" {
		t.Fatalf("first project = %q, want One", resp.Projects[0].Name)
	}
	if len(resp.Projects[0].Active) != 2 {
		t.Fatalf("project active len = %d, want 2", len(resp.Projects[0].Active))
	}
	if !resp.Projects[0].Outcome.Configured || resp.Projects[0].Outcome.Goal != "One is deployed" || resp.Projects[0].Outcome.HealthState != outcome.HealthUnknown {
		t.Fatalf("project outcome = %+v, want configured unknown health", resp.Projects[0].Outcome)
	}
	if len(resp.Workers) != 4 {
		t.Fatalf("fleet workers len = %d, want 4", len(resp.Workers))
	}
	worker := findFleetWorker(t, resp.Workers, "one-2")
	if worker.ProjectName != "One" || worker.ProjectRepo != "owner/one" {
		t.Fatalf("worker project = %q/%q, want One/owner/one", worker.ProjectName, worker.ProjectRepo)
	}
	if worker.IssueURL != "https://github.com/owner/one/issues/2" {
		t.Fatalf("worker issue_url = %q", worker.IssueURL)
	}
	if worker.PRURL != "https://github.com/owner/one/pull/12" {
		t.Fatalf("worker pr_url = %q", worker.PRURL)
	}
	if worker.TokensUsedTotal != 42000 {
		t.Fatalf("worker tokens = %d, want 42000", worker.TokensUsedTotal)
	}
	if worker.RuntimeSeconds <= 0 {
		t.Fatalf("worker runtime_seconds = %d, want positive runtime", worker.RuntimeSeconds)
	}
	if len(worker.Actions) != 5 {
		t.Fatalf("worker actions = %d, want 5", len(worker.Actions))
	}
	for _, action := range worker.Actions {
		assertFleetReadOnlyAction(t, action)
	}
	if len(resp.Projects[0].Actions) != 3 {
		t.Fatalf("project actions = %d, want 3", len(resp.Projects[0].Actions))
	}
	for _, action := range resp.Projects[0].Actions {
		assertFleetReadOnlyAction(t, action)
		if action.Target != "One" {
			t.Fatalf("project action target = %q, want project name One", action.Target)
		}
	}
	attentionWorker := findFleetWorker(t, resp.Workers, "two-1")
	if !attentionWorker.NeedsAttention {
		t.Fatal("retry-exhausted worker should need attention")
	}
	if !contains(attentionWorker.StatusReason, "checks failed") || !contains(attentionWorker.StatusReason, "PR #31 remains open") {
		t.Fatalf("attention status_reason = %q, want failed checks and open PR", attentionWorker.StatusReason)
	}
	if !contains(attentionWorker.NextAction, "Fix failing checks") {
		t.Fatalf("attention next_action = %q, want fix checks guidance", attentionWorker.NextAction)
	}
	if resp.Projects[1].NeedsAttention != len(resp.Projects[1].Attention) {
		t.Fatalf("project attention count = %d, reasons = %d", resp.Projects[1].NeedsAttention, len(resp.Projects[1].Attention))
	}
}

func TestFleetWorkersOrderActuallyRunningFirst(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "workers")
	finished := now.Add(-2 * time.Minute)
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"run-new": {
			IssueNumber: 9011,
			IssueTitle:  "Newest healthy worker",
			Status:      state.StatusRunning,
			StartedAt:   now.Add(-1 * time.Minute),
			PID:         os.Getpid(),
		},
		"run-old": {
			IssueNumber: 9012,
			IssueTitle:  "Older healthy worker",
			Status:      state.StatusRunning,
			StartedAt:   now.Add(-5 * time.Minute),
			PID:         os.Getpid(),
		},
		"stale-running": {
			IssueNumber: 9013,
			IssueTitle:  "Dead PID still marked running",
			Status:      state.StatusRunning,
			StartedAt:   now.Add(-30 * time.Second),
			PID:         0,
		},
		"review-recheck": {
			IssueNumber: 9014,
			IssueTitle:  "Review retry waiting on gates",
			Status:      state.StatusPROpen,
			StartedAt:   now.Add(-10 * time.Second),
			PRNumber:    14,
			RetryReason: state.RetryReasonReviewFeedback,
		},
		"code-landed": {
			IssueNumber: 9015,
			IssueTitle:  "Delivery verification needed",
			Status:      state.StatusCodeLanded,
			StartedAt:   now.Add(-20 * time.Second),
			FinishedAt:  &finished,
			PRNumber:    15,
		},
	})

	srv := NewFleet([]FleetProject{
		NewFleetProject("workers", "/tmp/workers.yaml", "", &config.Config{
			Repo:        "owner/workers",
			StateDir:    stateDir,
			MaxParallel: 5,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	if resp.Summary.Running != 2 || resp.Summary.WorkersRunning != 2 || resp.Summary.LiveWorkers != 2 {
		t.Fatalf("running summary = running:%d workers:%d live:%d, want 2/2/2", resp.Summary.Running, resp.Summary.WorkersRunning, resp.Summary.LiveWorkers)
	}
	project := findFleetProject(t, resp.Projects, "workers")
	if project.Running != 2 || project.WorkersRunning != 2 || project.LiveWorkers != 2 {
		t.Fatalf("project running = running:%d workers:%d live:%d, want 2/2/2", project.Running, project.WorkersRunning, project.LiveWorkers)
	}

	gotSlots := make([]string, 0, len(resp.Workers))
	for _, worker := range resp.Workers {
		gotSlots = append(gotSlots, worker.Slot)
	}
	wantPrefix := []string{"run-new", "run-old"}
	for i, want := range wantPrefix {
		if gotSlots[i] != want {
			t.Fatalf("worker order = %v, want healthy running prefix %v", gotSlots, wantPrefix)
		}
		if !fleetWorkerActuallyRunning(resp.Workers[i]) {
			t.Fatalf("prefix worker %q is not actually running: %+v", resp.Workers[i].Slot, resp.Workers[i])
		}
	}
	visibleRunning := 0
	for _, worker := range resp.Workers {
		if fleetWorkerActuallyRunning(worker) {
			visibleRunning++
			continue
		}
		break
	}
	if visibleRunning != resp.Summary.Running {
		t.Fatalf("visible running group = %d, summary.running = %d", visibleRunning, resp.Summary.Running)
	}

	stale := findFleetWorker(t, resp.Workers, "stale-running")
	if stale.Alive == nil || *stale.Alive || !stale.NeedsAttention {
		t.Fatalf("stale running alive/attention = %#v/%v, want alive=false attention", stale.Alive, stale.NeedsAttention)
	}
	if !contains(stale.StatusReason, "PID is not alive") || !contains(stale.NextAction, "reconciliation cycle") {
		t.Fatalf("stale running explanation = %q / %q", stale.StatusReason, stale.NextAction)
	}
	if fleetWorkerActuallyRunning(stale) {
		t.Fatalf("stale running row must not be classified as actually running: %+v", stale)
	}

	for _, slot := range []string{"review-recheck", "code-landed", "stale-running"} {
		for i, worker := range resp.Workers[:resp.Summary.Running] {
			if worker.Slot == slot {
				t.Fatalf("%s sorted into running prefix at index %d: %v", slot, i, gotSlots)
			}
		}
	}
}

func TestFleetTokenBudgetMarkerShowsStoppedWorkerAndConfiguredBudget(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	logFile := filepath.Join(dir, "sup-906.log")
	if err := os.WriteFile(logFile, []byte("working\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := worker.TokenBudgetMarker{
		Outcome:        worker.TokenBudgetExceededOutcome,
		Backend:        "claude",
		TokensObserved: 85_000,
		MaxTokens:      80_000,
		MeasuredAt:     time.Now().UTC(),
	}
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(worker.TokenBudgetMarkerPathForLog(logFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"sup-906": {
			IssueNumber: 906,
			IssueTitle:  "bounded work",
			Status:      state.StatusRunning,
			PID:         999999,
			LogFile:     logFile,
			Backend:     "claude",
			StartedAt:   time.Now().UTC().Add(-time.Minute),
		},
	})

	srv := NewFleet([]FleetProject{
		NewFleetProject("budget", "", "", &config.Config{
			Repo:            "owner/budget",
			StateDir:        stateDir,
			MaxParallel:     1,
			WorkerMaxTokens: 80_000,
		}),
	}, "127.0.0.1", 8786, true)
	got := findFleetWorker(t, srv.snapshot().Workers, "sup-906")
	if got.Status != string(state.StatusFailed) || got.DisplayStatus != worker.TokenBudgetExceededOutcome {
		t.Fatalf("status/display = %q/%q, want failed/token_budget_exceeded", got.Status, got.DisplayStatus)
	}
	if got.WorkerMaxTokens != 80_000 || got.TokensUsedAttempt != 85_000 || got.WorkerOutcome != worker.TokenBudgetExceededOutcome {
		t.Fatalf("budget view = %+v, want max=80000 usage=85000 outcome", got)
	}
	if !strings.Contains(got.StatusReason, "token budget") {
		t.Fatalf("status reason = %q, want budget stop reason", got.StatusReason)
	}
}

func TestFleetEffectiveConfigIsSanitized(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	saveFleetTestState(t, stateDir, nil)
	secret := "super-secret-token-622"
	enabled := false
	soft := 0.75
	cfg := &config.Config{
		Repo:                     "owner/settings",
		StateDir:                 stateDir,
		LocalPath:                filepath.Join(dir, "local-"+secret),
		WorkerPrompt:             filepath.Join(dir, "prompt-"+secret),
		MaxParallel:              4,
		ReviewGate:               "none",
		IssueLabels:              []string{"ready", "bug"},
		ExcludeLabels:            []string{"hold"},
		WorkerMaxTokens:          200000,
		WorkerSoftTokenThreshold: &soft,
		Telegram: config.TelegramConfig{
			BotToken:    secret,
			OpenclawURL: "https://relay.example/" + secret,
		},
		Model: config.ModelConfig{
			Default:          "codex",
			FallbackBackends: []string{"claude"},
			Backends: map[string]config.BackendDef{
				"codex": {
					Cmd:        "codex --token " + secret,
					ExtraArgs:  []string{"--api-key", secret},
					Provider:   "openai",
					Model:      "gpt-5.5",
					Effort:     "high",
					PromptMode: "stdin",
					Pricing:    config.BackendPricing{InputUSDPerMtok: 1.25, OutputUSDPerMtok: 10},
				},
				"helper": {
					Cmd:     "helper " + secret,
					Enabled: &enabled,
				},
			},
		},
		Supervisor: config.SupervisorConfig{
			Mode:                    "cautious",
			ReadyLabel:              "maestro-ready",
			BlockedLabel:            "blocked",
			ApprovalRequired:        []string{config.SupervisorActionChangeGlobalConfig},
			ApprovalRequiredActions: []string{"spawn_worker"},
			SafeActions:             []string{config.SupervisorActionAddIssueComment},
			CompletionGates:         config.SupervisorCompletionGatesConfig{RequiredLabels: []string{"runtime-verified"}},
		},
		SessionRetention: config.SessionRetentionConfig{
			KeepLast:    12,
			MinAgeDays:  3,
			ArchiveFile: filepath.Join(dir, "archive-"+secret+".jsonl"),
		},
	}
	srv := NewFleet([]FleetProject{NewFleetProject("Settings", "/tmp/settings.yaml", "", cfg)}, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("fleet snapshot leaked secret-bearing config value: %s", body)
	}

	var resp fleetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Projects) != 1 {
		t.Fatalf("projects len = %d, want 1", len(resp.Projects))
	}
	eff := resp.Projects[0].EffectiveConfig
	if eff.MaxParallel != 4 || eff.ReviewGate != "none" || eff.ModelPolicy.Default != "codex" {
		t.Fatalf("effective config basics = %+v", eff)
	}
	if eff.ApprovalAction != config.SupervisorActionChangeGlobalConfig {
		t.Fatalf("approval action = %q", eff.ApprovalAction)
	}
	if eff.Labels.Ready != "maestro-ready" || eff.Labels.Blocked != "blocked" || !containsString(eff.Labels.Completion, "runtime-verified") {
		t.Fatalf("labels = %+v", eff.Labels)
	}
	if !eff.Retention.Enabled || eff.Retention.KeepLast != 12 || eff.Retention.MinAge != (72*time.Hour).String() || !eff.Retention.ArchiveFilePresent {
		t.Fatalf("retention = %+v", eff.Retention)
	}
	if eff.CostCaps.WorkerMaxTokens != 200000 || eff.CostCaps.WorkerSoftTokenThreshold == nil || *eff.CostCaps.WorkerSoftTokenThreshold != soft || eff.CostCaps.BackendPricingConfigured != 1 {
		t.Fatalf("cost caps = %+v", eff.CostCaps)
	}
	if len(eff.ModelPolicy.Backends) != 2 {
		t.Fatalf("backends len = %d, want 2", len(eff.ModelPolicy.Backends))
	}
	var codex fleetEffectiveBackendConfig
	for _, backend := range eff.ModelPolicy.Backends {
		if backend.Name == "codex" {
			codex = backend
		}
	}
	if !codex.Enabled || codex.Provider != "openai" || codex.Model != "gpt-5.5" || !codex.PriceConfigured {
		t.Fatalf("codex backend = %+v", codex)
	}
}

func TestFleetEffectiveConfigShowsProviderLanesAndResolvedRoute(t *testing.T) {
	cfg := &config.Config{Model: config.ModelConfig{
		Default: "claude",
		ProviderLanes: []config.ProviderLane{
			{Provider: "anthropic", Default: "claude"},
			{Provider: "openai", Default: "sol", FallbackBackends: []string{"gpt55"}},
		},
		Backends: map[string]config.BackendDef{
			"claude": {Provider: "anthropic"},
			"sol":    {Provider: "openai", Model: "gpt-5.6-sol", Effort: "high"},
			"gpt55":  {Provider: "openai", Model: "gpt-5.5", Effort: "high"},
		},
	}}
	eff := buildFleetEffectiveConfig(cfg)
	if eff.ModelPolicy.SelectionReason != config.ModelRouteProviderLanes {
		t.Fatalf("selection reason = %q", eff.ModelPolicy.SelectionReason)
	}
	if !reflect.DeepEqual(eff.ModelPolicy.ResolvedRoute, []string{"claude", "sol", "gpt55"}) {
		t.Fatalf("resolved route = %v", eff.ModelPolicy.ResolvedRoute)
	}
	if len(eff.ModelPolicy.ProviderLanes) != 2 || eff.ModelPolicy.ProviderLanes[1].FallbackBackends[0] != "gpt55" {
		t.Fatalf("provider lanes = %+v", eff.ModelPolicy.ProviderLanes)
	}
}

// effective_config.settings reports each cost/LLM knob with the layer that
// supplied its value (#839), so Mission Control can highlight non-default overrides.
func TestFleetEffectiveConfigSettingsSource(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	saveFleetTestState(t, stateDir, nil)
	cfg := &config.Config{
		Repo:            "owner/settings",
		StateDir:        stateDir,
		WorkerMaxTokens: 250000,
		Supervisor:      config.SupervisorConfig{Enabled: false},
		SettingsSources: map[string]string{
			"supervisor.enabled": config.SettingSourceProject,
			"worker_max_tokens":  config.SettingSourceFleet,
		},
	}
	srv := NewFleet([]FleetProject{NewFleetProject("Settings", "/tmp/settings.yaml", "", cfg)}, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var resp fleetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byKey := map[string]fleetSettingSource{}
	for _, s := range resp.Projects[0].EffectiveConfig.Settings {
		byKey[s.Key] = s
	}
	if len(byKey) != len(config.FleetSettingKeys()) {
		t.Fatalf("settings len = %d, want %d", len(byKey), len(config.FleetSettingKeys()))
	}
	if s := byKey["supervisor.enabled"]; s.Source != config.SettingSourceProject || s.Value != "false" || s.IsDefault {
		t.Fatalf("supervisor.enabled = %+v", s)
	}
	if s := byKey["worker_max_tokens"]; s.Source != config.SettingSourceFleet || s.Value != "250000" || s.IsDefault {
		t.Fatalf("worker_max_tokens = %+v", s)
	}
	// An unmapped key falls back to builtin + is_default.
	if s := byKey["poll_interval_seconds"]; s.Source != config.SettingSourceBuiltin || !s.IsDefault {
		t.Fatalf("poll_interval_seconds = %+v", s)
	}
}

func TestFleetChangeGlobalConfigEnqueuesApproval(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	saveFleetTestState(t, stateDir, nil)
	cfg := &config.Config{
		Repo:     "owner/settings",
		StateDir: stateDir,
		Server:   config.ServerConfig{ReadOnly: false},
	}
	srv := NewFleet([]FleetProject{NewFleetProject("Settings", "/tmp/settings.yaml", "", cfg)}, "127.0.0.1", 8786, false)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/actions", bytes.NewBufferString(`{
		"action_id":"change_global_config",
		"project":"Settings",
		"reason":"lower max_parallel to 2 during provider cooldown"
	}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleFleetAction(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}

	var resp approvalEnqueueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.OK || resp.ActionID != config.SupervisorActionChangeGlobalConfig || resp.ApprovalID == "" {
		t.Fatalf("response = %+v", resp)
	}
	st, err := state.Load(stateDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if len(st.Approvals) != 1 {
		t.Fatalf("approvals len = %d, want 1", len(st.Approvals))
	}
	if st.Approvals[0].Action != config.SupervisorActionChangeGlobalConfig || st.Approvals[0].Status != state.ApprovalStatusPending {
		t.Fatalf("approval = %+v", st.Approvals[0])
	}
}

func TestFleetThroughputBucketsAggregateSevenDayWindows(t *testing.T) {
	now := time.Date(2026, 5, 2, 15, 0, 0, 0, time.UTC)
	donePR := func(pr int, finishedAt time.Time) fleetWorkerState {
		return fleetWorkerState{Status: string(state.StatusDone), PRNumber: pr, FinishedAt: finishedAt.Format(time.RFC3339)}
	}
	fullWindow := make([]fleetWorkerState, 0, 7)
	for daysAgo := 6; daysAgo >= 0; daysAgo-- {
		fullWindow = append(fullWindow, donePR(100+daysAgo, now.AddDate(0, 0, -daysAgo)))
	}
	partialWindow := []fleetWorkerState{
		{Status: string(state.StatusDone), PRNumber: 10, FinishedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)},
		{Status: string(state.StatusDone), PRNumber: 11, FinishedAt: now.Add(-24 * time.Hour).Format(time.RFC3339)},
		{Status: string(state.StatusDone), PRNumber: 12, FinishedAt: now.Add(-6 * 24 * time.Hour).Format(time.RFC3339)},
		{Status: string(state.StatusDone), PRNumber: 13, FinishedAt: now.Add(-8 * 24 * time.Hour).Format(time.RFC3339)},
		{Status: string(state.StatusFailed), PRNumber: 14, FinishedAt: now.Add(-time.Hour).Format(time.RFC3339)},
		{Status: string(state.StatusDone), PRNumber: 0, FinishedAt: now.Add(-time.Hour).Format(time.RFC3339)},
		{Status: string(state.StatusDone), PRNumber: 15, FinishedAt: "not-a-time"},
	}

	tests := []struct {
		name    string
		workers []fleetWorkerState
		want    []int
	}{
		{
			name: "zero data",
			want: []int{0, 0, 0, 0, 0, 0, 0},
		},
		{
			name:    "partial window",
			workers: partialWindow,
			want:    []int{1, 0, 0, 0, 0, 1, 1},
		},
		{
			name:    "full window",
			workers: fullWindow,
			want:    []int{1, 1, 1, 1, 1, 1, 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buckets := newFleetThroughputBuckets(now, 7)
			addFleetThroughputSummary(buckets, tc.workers)

			if got := buckets.Counts(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("counts = %v, want %v", got, tc.want)
			}
			wantTotal := 0
			for _, count := range tc.want {
				wantTotal += count
			}
			if buckets.Total() != wantTotal {
				t.Fatalf("total = %d, want %d", buckets.Total(), wantTotal)
			}
		})
	}
}

// Throughput is summed across projects: addFleetThroughputSummary is invoked
// once per project in snapshot(), so the bucket counts must accumulate rather
// than reset between calls.
func TestFleetThroughputBucketsAggregateAcrossProjects(t *testing.T) {
	now := time.Date(2026, 5, 2, 15, 0, 0, 0, time.UTC)
	donePR := func(pr int, finishedAt time.Time) fleetWorkerState {
		return fleetWorkerState{Status: string(state.StatusDone), PRNumber: pr, FinishedAt: finishedAt.Format(time.RFC3339)}
	}
	projectOne := []fleetWorkerState{
		donePR(101, now.Add(-2*time.Hour)),
		donePR(102, now.Add(-24*time.Hour)),
	}
	projectTwo := []fleetWorkerState{
		donePR(201, now.Add(-2*time.Hour)),
		donePR(202, now.Add(-6*24*time.Hour)),
	}
	projectThree := []fleetWorkerState(nil)

	buckets := newFleetThroughputBuckets(now, 7)
	addFleetThroughputSummary(buckets, projectOne)
	addFleetThroughputSummary(buckets, projectTwo)
	addFleetThroughputSummary(buckets, projectThree)

	wantCounts := []int{1, 0, 0, 0, 0, 1, 2}
	if got := buckets.Counts(); !reflect.DeepEqual(got, wantCounts) {
		t.Fatalf("counts = %v, want %v", got, wantCounts)
	}
	if buckets.Total() != 4 {
		t.Fatalf("total = %d, want 4", buckets.Total())
	}
}

func TestFleetAPIReviewRetryLifecycleDisplay(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	retryAt := now.Add(10 * time.Minute)
	stateDir := filepath.Join(dir, "review-retry")
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"retry-backoff": {
			IssueNumber:                 42,
			IssueTitle:                  "Address review feedback",
			Status:                      state.StatusDead,
			StartedAt:                   now.Add(-20 * time.Minute),
			FinishedAt:                  &now,
			PRNumber:                    12,
			RetryCount:                  1,
			NextRetryAt:                 &retryAt,
			PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
			RetryReason:                 state.RetryReasonReviewFeedback,
		},
		"retry-recheck": {
			IssueNumber: 43,
			IssueTitle:  "Wait for recheck",
			Status:      state.StatusPROpen,
			StartedAt:   now.Add(-30 * time.Minute),
			PRNumber:    13,
			RetryCount:  1,
			RetryReason: state.RetryReasonReviewFeedback,
		},
		"ci-retry": {
			IssueNumber:                 44,
			IssueTitle:                  "Retry failing checks",
			Status:                      state.StatusDead,
			StartedAt:                   now.Add(-40 * time.Minute),
			FinishedAt:                  &now,
			RetryCount:                  1,
			NextRetryAt:                 &retryAt,
			PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
			CIFailureOutput:             "checks failed",
		},
	})

	srv := NewFleet([]FleetProject{
		NewFleetProject("ReviewRetry", "/tmp/review-retry.yaml", "", &config.Config{
			Repo:        "owner/review-retry",
			StateDir:    stateDir,
			MaxParallel: 2,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	backoff := findFleetWorker(t, resp.Workers, "retry-backoff")
	if backoff.DisplayStatus != string(state.DisplayReviewRetryBackoff) {
		t.Fatalf("backoff display_status = %q, want review retry backoff", backoff.DisplayStatus)
	}
	if backoff.NeedsAttention {
		t.Fatal("review retry backoff should not need fleet attention")
	}
	if !contains(backoff.StatusReason, "waiting for the retry backoff") || !contains(backoff.NextAction, "scheduled retry worker") {
		t.Fatalf("backoff why = %q / %q, want retry worker wording", backoff.StatusReason, backoff.NextAction)
	}

	recheck := findFleetWorker(t, resp.Workers, "retry-recheck")
	if recheck.DisplayStatus != string(state.DisplayReviewRetryRecheck) {
		t.Fatalf("recheck display_status = %q, want review retry recheck", recheck.DisplayStatus)
	}
	if !contains(recheck.StatusReason, "waiting for CI, Greptile, or the merge gate") {
		t.Fatalf("recheck status_reason = %q, want CI/Greptile/merge gate wording", recheck.StatusReason)
	}
	ciRetry := findFleetWorker(t, resp.Workers, "ci-retry")
	if ciRetry.DisplayStatus != "" {
		t.Fatalf("ci retry display_status = %q, want raw dead state", ciRetry.DisplayStatus)
	}
	if !ciRetry.NeedsAttention || !contains(ciRetry.StatusReason, "retry is scheduled") {
		t.Fatalf("ci retry why = %q / attention %v, want dead retry guidance", ciRetry.StatusReason, ciRetry.NeedsAttention)
	}

	project := findFleetProject(t, resp.Projects, "ReviewRetry")
	if project.Failed != 1 || resp.Summary.Failed != 1 {
		t.Fatalf("failed counts = project %d fleet %d, want only CI retry counted", project.Failed, resp.Summary.Failed)
	}
	if project.NeedsAttention != 1 || resp.Summary.NeedsAttention != 1 {
		t.Fatalf("attention counts = project %d fleet %d, want only CI retry attention", project.NeedsAttention, resp.Summary.NeedsAttention)
	}
}

func TestFleetAPISuppressesResolvedStaleReviewFeedback(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	finished := now.Add(-5 * time.Minute)
	stateDir := filepath.Join(dir, "resolved-review-feedback")
	st := state.NewState()
	st.Sessions["merged-done"] = &state.Session{
		IssueNumber:                 359,
		IssueTitle:                  "Merged review feedback",
		Status:                      state.StatusDone,
		StartedAt:                   now.Add(-2 * time.Hour),
		FinishedAt:                  &finished,
		PRNumber:                    375,
		PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
		RetryReason:                 state.RetryReasonReviewFeedback,
	}
	st.Sessions["open-feedback"] = &state.Session{
		IssueNumber:                 360,
		IssueTitle:                  "Open review feedback",
		Status:                      state.StatusPROpen,
		StartedAt:                   now.Add(-time.Hour),
		PRNumber:                    376,
		PreviousAttemptFeedbackKind: state.RetryReasonReviewFeedback,
	}
	st.RecordSupervisorDecision(state.SupervisorDecision{
		ID:        "sup-review-feedback",
		CreatedAt: now,
		Project:   "owner/resolved-review-feedback",
		StuckStates: []state.SupervisorStuckState{
			{
				Code:              "stale_review_feedback",
				Severity:          "blocked",
				Summary:           "Issue #359 has review feedback, but no worker is currently fixing it.",
				RecommendedAction: "Respawn a worker with the saved review feedback or resolve the feedback manually.",
				Target:            &state.SupervisorTarget{Issue: 359, PR: 375, Session: "merged-done"},
			},
			{
				Code:              "stale_review_feedback",
				Severity:          "blocked",
				Summary:           "Issue #360 has review feedback, but no worker is currently fixing it.",
				RecommendedAction: "Respawn a worker with the saved review feedback or resolve the feedback manually.",
				Target:            &state.SupervisorTarget{Issue: 360, PR: 376, Session: "open-feedback"},
			},
		},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(stateDir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}
	cfg := &config.Config{Repo: "owner/resolved-review-feedback", StateDir: stateDir, MaxParallel: 2}

	single := buildStateResponse(cfg, st)
	singleDone := findSessionInfo(t, single.All, "merged-done")
	if singleDone.NeedsAttention || singleDone.DisplayStatus != "" || !contains(singleDone.StatusReason, "Issue is complete") {
		t.Fatalf("single-project done session = %+v, want neutral historical status", singleDone)
	}
	singleOpen := findSessionInfo(t, single.All, "open-feedback")
	if !singleOpen.NeedsAttention || !contains(singleOpen.StatusReason, "review feedback") {
		t.Fatalf("single-project open feedback = %+v, want attention", singleOpen)
	}

	srv := NewFleet([]FleetProject{
		NewFleetProject("ResolvedReviewFeedback", "/tmp/resolved-review-feedback.yaml", "", cfg),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	doneWorker := findFleetWorker(t, resp.Workers, "merged-done")
	if doneWorker.NeedsAttention || doneWorker.DisplayStatus != "" || !contains(doneWorker.StatusReason, "Issue is complete") {
		t.Fatalf("fleet done worker = %+v, want neutral historical status", doneWorker)
	}
	openWorker := findFleetWorker(t, resp.Workers, "open-feedback")
	if !openWorker.NeedsAttention || !contains(openWorker.StatusReason, "review feedback") {
		t.Fatalf("fleet open feedback worker = %+v, want attention", openWorker)
	}
	project := findFleetProject(t, resp.Projects, "ResolvedReviewFeedback")
	if project.NeedsAttention != 1 || resp.Summary.NeedsAttention != 1 || len(resp.Attention) != 1 {
		t.Fatalf("attention counts = project %d fleet %d inbox %d, want only open feedback", project.NeedsAttention, resp.Summary.NeedsAttention, len(resp.Attention))
	}
	if resp.Attention[0].Slot != "open-feedback" {
		t.Fatalf("attention inbox = %+v, want only open-feedback", resp.Attention)
	}
}

func TestFleetAPIIncludesQueueSnapshotMetadata(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	excludedStateDir := filepath.Join(dir, "excluded")
	candidateStateDir := filepath.Join(dir, "candidate")

	excludedState := state.NewState()
	excludedState.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-excluded",
		CreatedAt:         now,
		Project:           "owner/excluded",
		Summary:           "No issue is currently eligible under the dynamic wave policy.",
		RecommendedAction: "none",
		Risk:              "safe",
		PolicyRule:        "supervisor.dynamic_wave",
		QueueAnalysis: &state.SupervisorQueueAnalysis{
			PolicyRule:                    "supervisor.dynamic_wave",
			OpenIssues:                    4,
			EligibleCandidates:            0,
			ExcludedIssues:                1,
			HeldIssues:                    1,
			BlockedByDependencyIssues:     1,
			NonRunnableProjectStatusCount: 1,
			SkippedReasons: []string{
				"Issue #24 skipped by dynamic wave policy: excluded by label \"blocked\"",
				"Issue #25 skipped by dynamic wave policy: held/meta: mission parent issue",
				"Issue #26 skipped by dynamic wave policy: blocked by dependency: open issue(s) #12",
				"Issue #27 skipped by dynamic wave policy: project status \"In Progress\" is not runnable",
			},
		},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(excludedStateDir, excludedState); err != nil {
		t.Fatalf("save excluded state: %v", err)
	}

	candidateState := state.NewState()
	candidateState.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-candidate",
		CreatedAt:         now,
		Project:           "owner/candidate",
		Summary:           "Start a worker for issue #309.",
		RecommendedAction: "spawn_worker",
		Risk:              "mutating",
		PolicyRule:        "supervisor.dynamic_wave",
		QueueAnalysis: &state.SupervisorQueueAnalysis{
			PolicyRule:         "supervisor.dynamic_wave",
			OpenIssues:         3,
			EligibleCandidates: 2,
			ExcludedIssues:     1,
			SelectedCandidate: &state.SupervisorIssueCandidate{
				Number: 309,
				Title:  "Selected fleet card candidate",
			},
		},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(candidateStateDir, candidateState); err != nil {
		t.Fatalf("save candidate state: %v", err)
	}

	srv := NewFleet([]FleetProject{
		NewFleetProject("Excluded", "/tmp/excluded.yaml", "", &config.Config{
			Repo:        "owner/excluded",
			StateDir:    excludedStateDir,
			MaxParallel: 1,
		}),
		NewFleetProject("Candidate", "/tmp/candidate.yaml", "", &config.Config{
			Repo:        "owner/candidate",
			StateDir:    candidateStateDir,
			MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	excluded := findFleetProject(t, resp.Projects, "Excluded")
	if excluded.QueueSnapshot == nil {
		t.Fatal("excluded project queue snapshot is nil")
	}
	if excluded.QueueSnapshot.Open != 4 || excluded.QueueSnapshot.Eligible != 0 || excluded.QueueSnapshot.Excluded != 1 || excluded.QueueSnapshot.Held != 1 || excluded.QueueSnapshot.BlockedByDependency != 1 || excluded.QueueSnapshot.NonRunnableProjectStatusCount != 1 {
		t.Fatalf("excluded queue snapshot = %+v, want classified skipped counts", excluded.QueueSnapshot)
	}
	if !contains(excluded.QueueSnapshot.IdleReason, "Queue policy classified all 4 open issues") || !contains(excluded.QueueSnapshot.IdleReason, "blocked-by-dependency=1") {
		t.Fatalf("idle reason = %q, want classified explanation", excluded.QueueSnapshot.IdleReason)
	}
	if !contains(excluded.QueueSnapshot.TopSkippedReason, "excluded by label") {
		t.Fatalf("top skipped reason = %q, want excluded label reason", excluded.QueueSnapshot.TopSkippedReason)
	}
	if excluded.Supervisor.Latest == nil || excluded.Supervisor.Latest.QueueAnalysis == nil || excluded.Supervisor.Latest.QueueAnalysis.OpenIssues != 4 || excluded.Supervisor.Latest.QueueAnalysis.HeldIssues != 1 {
		t.Fatalf("supervisor latest queue analysis = %#v, want exposed analysis", excluded.Supervisor.Latest)
	}

	candidate := findFleetProject(t, resp.Projects, "Candidate")
	if candidate.QueueSnapshot == nil || candidate.QueueSnapshot.SelectedCandidate == nil || candidate.QueueSnapshot.SelectedCandidate.Number != 309 {
		t.Fatalf("candidate queue snapshot = %+v, want selected issue #309", candidate.QueueSnapshot)
	}
	if candidate.QueueSnapshot.IdleReason != "" {
		t.Fatalf("candidate idle reason = %q, want empty when eligible", candidate.QueueSnapshot.IdleReason)
	}
}

// #814: the fleet snapshot must separate live implementation workers from
// pr_open PR-gate sessions and explain a gate-bound-but-eligible project instead
// of rendering it as idle with 0 workers.
func TestFleetSnapshotSeparatesLiveWorkersFromPRGates(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	// Legacy project: max_parallel filled entirely by pr_open PR-gate sessions
	// while eligible work waits — the misleading "0 workers" case.
	gateDir := filepath.Join(dir, "gatebound")
	gateState := state.NewState()
	gateState.Sessions["gate-1"] = &state.Session{Status: state.StatusPROpen, PRNumber: 101, IssueNumber: 11}
	gateState.Sessions["gate-2"] = &state.Session{Status: state.StatusPROpen, PRNumber: 102, IssueNumber: 12}
	gateState.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-gatebound",
		CreatedAt:         now,
		Project:           "owner/gatebound",
		RecommendedAction: "spawn_worker",
		Risk:              "mutating",
		PolicyRule:        "supervisor.dynamic_wave",
		QueueAnalysis: &state.SupervisorQueueAnalysis{
			PolicyRule:         "supervisor.dynamic_wave",
			OpenIssues:         5,
			EligibleCandidates: 3,
		},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(gateDir, gateState); err != nil {
		t.Fatalf("save gate state: %v", err)
	}

	// Separated project: max_live_workers>0 keeps a live worker running while
	// several PRs are open — capacity is not blocked by the gates.
	liveDir := filepath.Join(dir, "separated")
	liveState := state.NewState()
	liveState.Sessions["run-1"] = &state.Session{Status: state.StatusRunning, PID: os.Getpid(), IssueNumber: 20}
	liveState.Sessions["gate-1"] = &state.Session{Status: state.StatusPROpen, PRNumber: 201, IssueNumber: 21}
	liveState.Sessions["gate-2"] = &state.Session{Status: state.StatusPROpen, PRNumber: 202, IssueNumber: 22}
	liveState.Sessions["gate-3"] = &state.Session{Status: state.StatusPROpen, PRNumber: 203, IssueNumber: 23}
	if err := state.Save(liveDir, liveState); err != nil {
		t.Fatalf("save live state: %v", err)
	}

	srv := NewFleet([]FleetProject{
		NewFleetProject("GateBound", "/tmp/gatebound.yaml", "", &config.Config{
			Repo:        "owner/gatebound",
			StateDir:    gateDir,
			MaxParallel: 2,
		}),
		NewFleetProject("Separated", "/tmp/separated.yaml", "", &config.Config{
			Repo:           "owner/separated",
			StateDir:       liveDir,
			MaxParallel:    2,
			MaxLiveWorkers: 3,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	gate := findFleetProject(t, resp.Projects, "GateBound")
	if gate.LiveWorkers != 0 || gate.WorkersRunning != 0 {
		t.Fatalf("gate-bound live workers = %d/%d, want 0", gate.LiveWorkers, gate.WorkersRunning)
	}
	if gate.PRGates != 2 {
		t.Errorf("gate-bound pr_gates = %d, want 2", gate.PRGates)
	}
	if gate.CapacityUsed != 2 || gate.CapacityBlockedByGates != 2 {
		t.Errorf("gate-bound capacity_used=%d blocked_by_gates=%d, want 2/2", gate.CapacityUsed, gate.CapacityBlockedByGates)
	}
	if gate.Activity != string(state.ActivityBlockedByGates) {
		t.Errorf("gate-bound activity = %q, want %q", gate.Activity, state.ActivityBlockedByGates)
	}
	if !contains(gate.ActivityReason, "Blocked by PR gates") {
		t.Errorf("gate-bound activity reason = %q, want blocked-by-PR-gates explanation", gate.ActivityReason)
	}

	sep := findFleetProject(t, resp.Projects, "Separated")
	if sep.LiveWorkers != 1 || sep.WorkersRunning != 1 {
		t.Errorf("separated live workers = %d/%d, want 1", sep.LiveWorkers, sep.WorkersRunning)
	}
	if sep.PRGates != 3 {
		t.Errorf("separated pr_gates = %d, want 3", sep.PRGates)
	}
	if sep.CapacityBlockedByGates != 0 {
		t.Errorf("separated blocked_by_gates = %d, want 0 (gates separated out)", sep.CapacityBlockedByGates)
	}
	if sep.Activity != string(state.ActivityImplementing) {
		t.Errorf("separated activity = %q, want %q", sep.Activity, state.ActivityImplementing)
	}

	if resp.Summary.LiveWorkers != 1 || resp.Summary.PRGates != 5 {
		t.Errorf("summary live_workers=%d pr_gates=%d, want 1/5", resp.Summary.LiveWorkers, resp.Summary.PRGates)
	}
	if resp.Summary.CapacityBlockedByGates != 2 {
		t.Errorf("summary capacity_blocked_by_gates = %d, want 2", resp.Summary.CapacityBlockedByGates)
	}
}

func TestFleetQueueSnapshotFromSupervisorCarriesDecisionPlane(t *testing.T) {
	info := supervisorInfo{
		Latest: &supervisorDecisionInfo{
			QueueAnalysis: &state.SupervisorQueueAnalysis{
				PolicyRule:         "supervisor.dynamic_wave",
				OpenIssues:         3,
				EligibleCandidates: 2,
				ExcludedIssues:     1,
				SelectedCandidate:  &state.SupervisorIssueCandidate{Number: 1, PriorityLabel: "p0"},
				EligibleRanked: []state.SupervisorIssueCandidate{
					{Number: 1, PriorityLabel: "p0"},
					{Number: 2, PriorityLabel: "p1"},
				},
				SkippedCandidates: []state.SupervisorSkippedCandidate{
					{Number: 3, PriorityLabel: "p2", Category: "held_meta", Reason: "title indicates epic"},
				},
			},
		},
	}

	snap := fleetQueueSnapshotFromSupervisor(info)
	if snap == nil {
		t.Fatalf("snapshot is nil")
	}
	if len(snap.EligibleRanked) != 2 || snap.EligibleRanked[0].Number != 1 || snap.EligibleRanked[1].Number != 2 {
		t.Fatalf("eligible ranked = %#v, want #1 then #2", snap.EligibleRanked)
	}
	if snap.EligibleRanked[0].PriorityLabel != "p0" {
		t.Fatalf("eligible ranked[0] priority = %q, want p0", snap.EligibleRanked[0].PriorityLabel)
	}
	if len(snap.SkippedCandidates) != 1 {
		t.Fatalf("skipped candidates = %#v, want one entry", snap.SkippedCandidates)
	}
	if got := snap.SkippedCandidates[0]; got.Number != 3 || got.Category != "held_meta" || got.Reason != "title indicates epic" {
		t.Fatalf("skipped candidate = %#v, want #3 held_meta", got)
	}

	// The copy is defensive: mutating the source analysis after building the
	// snapshot must not bleed into the served payload.
	info.Latest.QueueAnalysis.EligibleRanked[0].Number = 999
	if snap.EligibleRanked[0].Number != 1 {
		t.Fatalf("snapshot eligible ranked mutated with source: got #%d", snap.EligibleRanked[0].Number)
	}
}

func TestFleetAPIOperatorStateExplainsZeroRunningActiveWork(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	monitorStateDir := filepath.Join(dir, "monitor")
	candidateStateDir := filepath.Join(dir, "candidate")

	monitorState := state.NewState()
	monitorState.Sessions["pr-1"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "Review PR",
		Status:      state.StatusPROpen,
		StartedAt:   now.Add(-10 * time.Minute),
		PRNumber:    12,
	}
	monitorState.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-monitor",
		CreatedAt:         now,
		Project:           "owner/monitor",
		Summary:           "Monitor PR #12 until checks and review gates pass.",
		RecommendedAction: "monitor_open_pr",
		Risk:              "safe",
		Target:            &state.SupervisorTarget{Issue: 42, PR: 12, Session: "pr-1"},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(monitorStateDir, monitorState); err != nil {
		t.Fatalf("save monitor state: %v", err)
	}

	candidateState := state.NewState()
	candidateState.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-candidate",
		CreatedAt:         now,
		Project:           "owner/candidate",
		Summary:           "Start a worker for issue #309.",
		RecommendedAction: "spawn_worker",
		Risk:              "mutating",
		Target:            &state.SupervisorTarget{Issue: 309},
		QueueAnalysis: &state.SupervisorQueueAnalysis{
			OpenIssues:         3,
			EligibleCandidates: 2,
			SelectedCandidate: &state.SupervisorIssueCandidate{
				Number: 309,
				Title:  "Selected fleet candidate",
			},
		},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(candidateStateDir, candidateState); err != nil {
		t.Fatalf("save candidate state: %v", err)
	}

	srv := NewFleet([]FleetProject{
		NewFleetProject("Monitor", "/tmp/monitor.yaml", "", &config.Config{
			Repo:        "owner/monitor",
			StateDir:    monitorStateDir,
			MaxParallel: 1,
			Outcome:     outcome.Brief{DesiredOutcome: "Monitor outcome"},
		}),
		NewFleetProject("Candidate", "/tmp/candidate.yaml", "", &config.Config{
			Repo:        "owner/candidate",
			StateDir:    candidateStateDir,
			MaxParallel: 1,
			Outcome:     outcome.Brief{DesiredOutcome: "Candidate outcome"},
		}),
	}, "127.0.0.1", 8786, true)

	resp := srv.snapshot()
	if resp.Summary.Running != 0 || resp.Summary.Active != 2 || resp.Summary.MonitoringPR != 1 || resp.Summary.DispatchPending != 1 {
		t.Fatalf("summary = %+v, want zero running but two active operator states", resp.Summary)
	}

	monitor := findFleetProject(t, resp.Projects, "Monitor")
	if monitor.OperatorState.Kind != "monitoring_pr" || monitor.OperatorState.PRNumber != 12 || monitor.OperatorState.IssueNumber != 42 {
		t.Fatalf("monitor operator state = %+v, want monitoring PR #12 for issue #42", monitor.OperatorState)
	}
	monitorHTML := renderFleetProjectRailState(monitor)
	if contains(monitorHTML, "0/1 running") || !contains(monitorHTML, "Monitoring PR") {
		t.Fatalf("monitor rail state should explain PR monitoring without raw running counter, got:\n%s", monitorHTML)
	}

	candidate := findFleetProject(t, resp.Projects, "Candidate")
	if candidate.OperatorState.Kind != "pending_dispatch" || candidate.OperatorState.IssueNumber != 309 {
		t.Fatalf("candidate operator state = %+v, want pending dispatch for issue #309", candidate.OperatorState)
	}
	candidateHTML := renderFleetProjectRailState(candidate)
	if contains(candidateHTML, "0/1 running") || !contains(candidateHTML, "Dispatch pending") {
		t.Fatalf("candidate rail state should explain pending dispatch without raw running counter, got:\n%s", candidateHTML)
	}
}

func TestFleetAPIEscalatesSelectedPendingDispatchPastSLA(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "dispatch-sla")

	st := state.NewState()
	st.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-dispatch-sla",
		CreatedAt:         now.Add(-10 * time.Minute),
		Project:           "owner/dispatch-sla",
		Summary:           "Start a worker for issue #354.",
		RecommendedAction: "spawn_worker",
		Risk:              "mutating",
		Target:            &state.SupervisorTarget{Issue: 354},
		QueueAnalysis: &state.SupervisorQueueAnalysis{
			OpenIssues:         1,
			EligibleCandidates: 1,
			SelectedCandidate: &state.SupervisorIssueCandidate{
				Number: 354,
				Title:  "Selected overdue issue",
			},
		},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(stateDir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	srv := NewFleet([]FleetProject{
		NewFleetProject("DispatchSLA", "/tmp/dispatch-sla.yaml", "", &config.Config{
			Repo:        "owner/dispatch-sla",
			StateDir:    stateDir,
			MaxParallel: 1,
			Outcome:     outcome.Brief{DesiredOutcome: "Dispatch SLA outcome"},
			Supervisor: config.SupervisorConfig{
				DispatchSLASeconds: 60,
			},
		}),
	}, "127.0.0.1", 8786, true)

	resp := srv.snapshot()
	project := findFleetProject(t, resp.Projects, "DispatchSLA")
	if project.OperatorState.Kind != "dispatch_failure" || project.OperatorState.IssueNumber != 354 {
		t.Fatalf("operator state = %+v, want dispatch_failure for selected issue #354", project.OperatorState)
	}
	if resp.Summary.DispatchFailures != 1 || resp.Summary.DispatchPending != 0 {
		t.Fatalf("summary = %+v, want one dispatch failure and no pending dispatch", resp.Summary)
	}
	if resp.NextAction == nil || resp.NextAction.Priority != "P0" || resp.NextAction.Kind != "dispatch_failure" {
		t.Fatalf("next action = %+v, want P0 dispatch failure", resp.NextAction)
	}
	if !contains(resp.OperatorBrief.Sentence, "Dispatch SLA missed") {
		t.Fatalf("operator brief = %+v, want dispatch SLA action", resp.OperatorBrief)
	}
}

func TestFleetOperatorBriefNamesSingleHighestPriorityAction(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	staleWorkerStateDir := filepath.Join(dir, "stale-worker")
	noEligibleStateDir := filepath.Join(dir, "no-eligible")
	runningStateDir := filepath.Join(dir, "running")

	saveFleetTestState(t, staleWorkerStateDir, map[string]*state.Session{
		"stale-1": {
			IssueNumber: 42,
			IssueTitle:  "Stale worker",
			Status:      state.StatusRunning,
			StartedAt:   now.Add(-12 * time.Minute),
			PID:         0,
		},
	})
	noEligibleState := state.NewState()
	noEligibleState.RecordSupervisorDecision(state.SupervisorDecision{
		ID:                "sup-no-eligible",
		CreatedAt:         now,
		Project:           "owner/no-eligible",
		Summary:           "No issue is eligible under policy.",
		RecommendedAction: "none",
		Risk:              "safe",
		QueueAnalysis: &state.SupervisorQueueAnalysis{
			OpenIssues:         2,
			EligibleCandidates: 0,
			ExcludedIssues:     2,
		},
	}, state.DefaultSupervisorDecisionLimit)
	if err := state.Save(noEligibleStateDir, noEligibleState); err != nil {
		t.Fatalf("save no eligible state: %v", err)
	}
	saveFleetTestState(t, runningStateDir, map[string]*state.Session{
		"run-1": {
			IssueNumber: 77,
			IssueTitle:  "Running worker",
			Status:      state.StatusRunning,
			StartedAt:   now.Add(-time.Minute),
			PID:         os.Getpid(),
		},
	})

	srv := NewFleet([]FleetProject{
		NewFleetProject("NoEligible", "/tmp/no-eligible.yaml", "", &config.Config{
			Repo:        "owner/no-eligible",
			StateDir:    noEligibleStateDir,
			MaxParallel: 1,
			Outcome:     outcome.Brief{DesiredOutcome: "No eligible outcome"},
		}),
		NewFleetProject("Running", "/tmp/running.yaml", "", &config.Config{
			Repo:        "owner/running",
			StateDir:    runningStateDir,
			MaxParallel: 1,
			Outcome:     outcome.Brief{DesiredOutcome: "Running outcome"},
		}),
		NewFleetProject("StaleWorker", "/tmp/stale-worker.yaml", "", &config.Config{
			Repo:        "owner/stale-worker",
			StateDir:    staleWorkerStateDir,
			MaxParallel: 1,
			Outcome:     outcome.Brief{DesiredOutcome: "Stale worker outcome"},
		}),
	}, "127.0.0.1", 8786, true)

	resp := srv.snapshot()
	brief := resp.OperatorBrief
	if !brief.ActionRequired || brief.Kind != "stale_worker" || brief.Project != "StaleWorker" {
		t.Fatalf("operator brief = %+v, want stale worker action for StaleWorker", brief)
	}
	if brief.IssueNumber != 42 || brief.Session != "stale-1" || !contains(brief.Reason, "PID is not alive") {
		t.Fatalf("operator brief target/reason = %+v, want issue/session/dead PID", brief)
	}
	for _, want := range []string{"Global brief: action required", "StaleWorker", "issue #42", "session stale-1", "Reason:"} {
		if !contains(brief.Sentence, want) {
			t.Fatalf("operator brief sentence = %q, want %q", brief.Sentence, want)
		}
	}
	if contains(brief.Sentence, "issue #77") {
		t.Fatalf("operator brief should name one highest-priority action, got %q", brief.Sentence)
	}
	if resp.Summary.StaleWorkers != 1 || resp.Summary.NoEligibleIssues != 1 {
		t.Fatalf("summary = %+v, want stale worker and no eligible counts", resp.Summary)
	}
}

func TestFleetOperatorBriefPrioritizesPendingApproval(t *testing.T) {
	now := time.Now().UTC()
	brief := buildFleetOperatorBrief([]fleetProjectState{{
		Name: "BlockedQueue",
		OperatorState: fleetOperatorState{
			Kind:        "no_eligible_issues",
			Tone:        "attention",
			Label:       "No eligible issues",
			Summary:     "No issue is eligible under policy.",
			NextAction:  "Adjust labels or policy so a worker can run.",
			IssueNumber: 15,
		},
	}}, []fleetApprovalState{{
		ProjectName: "ApprovalProject",
		Status:      string(state.ApprovalStatusPending),
		Summary:     "Supervisor approval is waiting for operator review.",
		PRNumber:    44,
		Session:     "approval-44",
		createdAt:   now.Add(-time.Minute),
		updatedAt:   now,
	}}, now)

	if !brief.ActionRequired || brief.Kind != "approval_pending" || brief.Project != "ApprovalProject" {
		t.Fatalf("operator brief = %+v, want pending approval action for ApprovalProject", brief)
	}
	if brief.PRNumber != 44 || brief.Session != "approval-44" {
		t.Fatalf("operator brief target = %+v, want PR #44 and approval-44 session", brief)
	}
	if !contains(brief.NextAction, "Approve or reject") {
		t.Fatalf("operator brief next action = %q, want approval guidance", brief.NextAction)
	}
	if contains(brief.Sentence, "Next:") {
		t.Fatalf("operator brief sentence = %q, should leave next action for structured rendering", brief.Sentence)
	}
}

func TestFleetOperatorBriefEscalatesPendingApprovalPastSLA(t *testing.T) {
	now := time.Now().UTC()
	brief := buildFleetOperatorBrief([]fleetProjectState{{
		Name: "ApprovalProject",
	}}, []fleetApprovalState{{
		ProjectName: "ApprovalProject",
		Status:      string(state.ApprovalStatusPending),
		Summary:     "Supervisor approval is waiting past SLA.",
		PRNumber:    101,
		createdAt:   now.Add(-45 * time.Minute),
		updatedAt:   now.Add(-45 * time.Minute),
	}}, now)

	if brief.Tone != "daemon-down" {
		t.Fatalf("brief.Tone = %q, want daemon-down for past-SLA approval", brief.Tone)
	}
	if !contains(brief.Sentence, "Approval past SLA") {
		t.Fatalf("brief.Sentence = %q, want past SLA label", brief.Sentence)
	}
	if !contains(brief.NextAction, "30m") {
		t.Fatalf("brief.NextAction = %q, want 30m SLA mention", brief.NextAction)
	}
}

func TestFleetOperatorBriefEscalatesAnyPastSLAPendingApproval(t *testing.T) {
	now := time.Now().UTC()
	brief := buildFleetOperatorBrief([]fleetProjectState{{
		Name: "ApprovalProject",
	}}, []fleetApprovalState{
		{
			ProjectName: "NewApproval",
			Status:      string(state.ApprovalStatusPending),
			Summary:     "New approval is still within SLA.",
			PRNumber:    102,
			createdAt:   now.Add(-5 * time.Minute),
			updatedAt:   now.Add(-5 * time.Minute),
		},
		{
			ProjectName: "OldApproval",
			Status:      string(state.ApprovalStatusPending),
			Summary:     "Old approval breached SLA.",
			PRNumber:    101,
			createdAt:   now.Add(-45 * time.Minute),
			updatedAt:   now.Add(-45 * time.Minute),
		},
	}, now)

	if brief.Tone != "daemon-down" || brief.Project != "OldApproval" {
		t.Fatalf("brief = %+v, want oldest past-SLA approval escalated", brief)
	}
}

func TestHighestPriorityPendingFleetApprovalSelectsNewestPending(t *testing.T) {
	now := time.Now().UTC()
	selected := highestPriorityPendingFleetApproval([]fleetApprovalState{
		{
			ProjectName: "OldApproval",
			ID:          "old",
			Status:      string(state.ApprovalStatusPending),
			createdAt:   now.Add(-2 * time.Hour),
			updatedAt:   now.Add(-2 * time.Hour),
		},
		{
			ProjectName: "ApprovedApproval",
			ID:          "approved",
			Status:      string(state.ApprovalStatusApproved),
			createdAt:   now,
			updatedAt:   now,
		},
		{
			ProjectName: "NewApproval",
			ID:          "new",
			Status:      string(state.ApprovalStatusPending),
			createdAt:   now.Add(-time.Hour),
			updatedAt:   now.Add(-5 * time.Minute),
		},
	})

	if selected == nil || selected.ProjectName != "NewApproval" {
		t.Fatalf("selected approval = %+v, want newest pending approval", selected)
	}
}

func TestBuildFleetNextActionEmptyFleetReturnsNil(t *testing.T) {
	now := time.Now().UTC()
	if got := buildFleetNextAction(nil, nil, now); got != nil {
		t.Fatalf("buildFleetNextAction empty fleet = %+v, want nil", got)
	}
	idleProjects := []fleetProjectState{
		{Name: "Idle", OperatorState: fleetOperatorState{Kind: "idle"}},
		{Name: "Running", OperatorState: fleetOperatorState{Kind: "working"}},
		{Name: "Monitoring", OperatorState: fleetOperatorState{Kind: "monitoring_pr"}},
	}
	if got := buildFleetNextAction(idleProjects, nil, now); got != nil {
		t.Fatalf("buildFleetNextAction idle fleet = %+v, want nil", got)
	}
}

func TestBuildFleetNextActionPrefersP0OverOlderLowerTier(t *testing.T) {
	now := time.Now().UTC()
	startedRecent := now.Add(-2 * time.Minute).Format(time.RFC3339)
	startedAncient := now.Add(-6 * time.Hour).Format(time.RFC3339)

	projects := []fleetProjectState{
		{
			Name: "OldP2",
			OperatorState: fleetOperatorState{
				Kind:       "no_eligible_issues",
				Summary:    "All open issues are excluded by policy.",
				NextAction: "Adjust labels or policy so a worker can run.",
			},
			Supervisor: supervisorInfo{Latest: &supervisorDecisionInfo{CreatedAt: now.Add(-6 * time.Hour)}},
		},
		{
			Name: "RecentP0",
			OperatorState: fleetOperatorState{
				Kind:       "stale_worker",
				Summary:    "Worker PID is not alive.",
				NextAction: "Restart the worker.",
			},
			Attention: []sessionInfo{{Slot: "stale-1", Status: string(state.StatusRunning), StartedAt: startedRecent}},
		},
		{
			Name: "DispatchP0",
			OperatorState: fleetOperatorState{
				Kind:       "dispatch_failure",
				Summary:    "Supervisor dispatch failed.",
				NextAction: "Check supervisor logs.",
			},
			Supervisor: supervisorInfo{Latest: &supervisorDecisionInfo{CreatedAt: now.Add(-30 * time.Minute)}},
		},
		{
			Name: "AncientAttention",
			OperatorState: fleetOperatorState{
				Kind:       "attention",
				Summary:    "Worker needs review.",
				NextAction: "Open the worker detail.",
			},
			Attention: []sessionInfo{{Slot: "old-1", Status: string(state.StatusRunning), StartedAt: startedAncient}},
		},
	}

	got := buildFleetNextAction(projects, nil, now)
	if got == nil {
		t.Fatalf("buildFleetNextAction = nil, want a P0 candidate")
	}
	if got.Priority != "P0" {
		t.Fatalf("priority = %q, want P0 (a P0 must beat any older P1/P2)", got.Priority)
	}
	if got.Project != "DispatchP0" {
		t.Fatalf("project = %q, want DispatchP0 (the older of the two P0 candidates)", got.Project)
	}
	if got.Kind != "dispatch_failure" {
		t.Fatalf("kind = %q, want dispatch_failure", got.Kind)
	}
}

func TestBuildFleetNextActionWithinTierOldestUpdatedAtWins(t *testing.T) {
	now := time.Now().UTC()
	older := now.Add(-3 * time.Hour).Format(time.RFC3339)
	newer := now.Add(-30 * time.Minute).Format(time.RFC3339)

	projects := []fleetProjectState{
		{
			Name: "NewerP1",
			OperatorState: fleetOperatorState{
				Kind:       "attention",
				Summary:    "Worker needs review.",
				NextAction: "Open the worker detail.",
			},
			Attention: []sessionInfo{{Slot: "newer-1", Status: string(state.StatusRunning), StartedAt: newer}},
		},
		{
			Name: "OlderP1",
			OperatorState: fleetOperatorState{
				Kind:       "attention",
				Summary:    "Older worker needs review.",
				NextAction: "Open the older worker detail.",
			},
			Attention: []sessionInfo{{Slot: "older-1", Status: string(state.StatusRunning), StartedAt: older}},
		},
	}
	got := buildFleetNextAction(projects, nil, now)
	if got == nil {
		t.Fatalf("buildFleetNextAction = nil, want a P1 candidate")
	}
	if got.Priority != "P1" {
		t.Fatalf("priority = %q, want P1", got.Priority)
	}
	if got.Project != "OlderP1" {
		t.Fatalf("project = %q, want OlderP1 (the oldest by updated_at within P1)", got.Project)
	}
}

func TestBuildFleetNextActionApprovalPastSLABeatsRegularPending(t *testing.T) {
	now := time.Now().UTC()
	approvals := []fleetApprovalState{
		{
			ProjectName: "Fresh",
			ID:          "fresh",
			Status:      string(state.ApprovalStatusPending),
			Summary:     "Fresh approval is waiting.",
			PRURL:       "https://github.com/owner/fresh/pull/9",
			createdAt:   now.Add(-5 * time.Minute),
			updatedAt:   now.Add(-5 * time.Minute),
		},
		{
			ProjectName: "PastSLA",
			ID:          "past-sla",
			Status:      string(state.ApprovalStatusPending),
			Summary:     "Approval breached the SLA.",
			PRURL:       "https://github.com/owner/past-sla/pull/12",
			createdAt:   now.Add(-45 * time.Minute),
			updatedAt:   now.Add(-45 * time.Minute),
		},
	}
	got := buildFleetNextAction(nil, approvals, now)
	if got == nil {
		t.Fatalf("buildFleetNextAction = nil, want a past-SLA approval winner")
	}
	if got.Priority != "P0" || got.Project != "PastSLA" {
		t.Fatalf("got = %+v, want P0 PastSLA", got)
	}
	if got.Kind != "approval_pending" {
		t.Fatalf("kind = %q, want approval_pending", got.Kind)
	}
	if got.TargetURL != "/approvals?id=past-sla" {
		t.Fatalf("target_url = %q, want focused approvals URL", got.TargetURL)
	}
}

func TestBuildFleetNextActionSuggestionPastSLADoesNotEscalate(t *testing.T) {
	now := time.Now().UTC()
	approvals := []fleetApprovalState{{
		ProjectName:  "Apertune",
		ID:           "lesson-1",
		Status:       string(state.ApprovalStatusPending),
		Action:       config.SupervisorActionApplyLessonProposal,
		Summary:      "Apply lesson proposal for retry_exhausted in issue:1.",
		DashboardURL: "/approvals?id=lesson-1",
		createdAt:    now.Add(-2 * time.Hour),
		updatedAt:    now.Add(-2 * time.Hour),
	}}

	got := buildFleetNextAction(nil, approvals, now)
	if got == nil {
		t.Fatalf("buildFleetNextAction = nil, want low-priority suggestion candidate")
	}
	if got.Priority != "P3" {
		t.Fatalf("priority = %q, want P3 (suggestions must not become P0 when past SLA)", got.Priority)
	}
	if got.TargetURL != "/approvals?id=lesson-1" {
		t.Fatalf("target_url = %q, want focused approvals URL", got.TargetURL)
	}
}

func TestBuildFleetNextActionRealOperatorStateBeatsSuggestion(t *testing.T) {
	now := time.Now().UTC()
	approvals := []fleetApprovalState{{
		ProjectName: "Suggestions",
		ID:          "lesson-1",
		Status:      string(state.ApprovalStatusPending),
		Action:      config.SupervisorActionApplyLessonProposal,
		createdAt:   now.Add(-24 * time.Hour),
		updatedAt:   now.Add(-24 * time.Hour),
	}}
	projects := []fleetProjectState{{
		Name: "Runtime",
		OperatorState: fleetOperatorState{
			Kind:       "stale_worker",
			Summary:    "Worker PID is not alive.",
			NextAction: "Open the worker log.",
			Session:    "sup-1",
		},
		Attention: []sessionInfo{{Slot: "sup-1", Status: string(state.StatusRunning), StartedAt: now.Add(-time.Minute).Format(time.RFC3339)}},
	}}

	got := buildFleetNextAction(projects, approvals, now)
	if got == nil {
		t.Fatalf("buildFleetNextAction = nil, want stale worker candidate")
	}
	if got.Kind != "stale_worker" || got.Project != "Runtime" || got.Priority != "P0" {
		t.Fatalf("next action = %+v, want P0 stale_worker for Runtime", got)
	}
	if got.TargetURL != "/workers?project=Runtime&slot=sup-1" {
		t.Fatalf("target_url = %q, want focused worker URL", got.TargetURL)
	}
}

func TestSuggestionOnlyApprovalsDoNotAlarmHero(t *testing.T) {
	now := time.Now().UTC()
	summary := fleetSummary{}
	addFleetApprovalSummary(&summary, fleetApprovalState{
		Status: string(state.ApprovalStatusPending),
		Action: config.SupervisorActionApplyLessonProposal,
	})
	latest := &supervisorDecisionInfo{CreatedAt: now}

	if tone := fleetVerdictTone(summary, latest, now); tone != "healthy" {
		t.Fatalf("fleetVerdictTone = %q, want healthy for suggestion-only approvals", tone)
	}
	if sentence := fleetAttentionSentence(summary); sentence != "No item needs attention." {
		t.Fatalf("fleetAttentionSentence = %q, want calm sentence", sentence)
	}
	brief := buildFleetOperatorBrief([]fleetProjectState{{Name: "Apertune"}}, []fleetApprovalState{{
		ProjectName: "Apertune",
		Status:      string(state.ApprovalStatusPending),
		Action:      config.SupervisorActionApplyLessonProposal,
		createdAt:   now.Add(-time.Hour),
		updatedAt:   now.Add(-time.Hour),
	}}, now)
	if brief.ActionRequired || brief.Kind == "approval_pending" {
		t.Fatalf("operator brief = %+v, want no action-required approval for suggestion-only approvals", brief)
	}
}

func TestBuildFleetNextActionDispatchFailureUsesSupervisorTimestamp(t *testing.T) {
	now := time.Now().UTC()
	ancientAttention := now.Add(-12 * time.Hour).Format(time.RFC3339)
	recentDecision := now.Add(-5 * time.Minute)
	olderDecision := now.Add(-2 * time.Hour)

	projects := []fleetProjectState{
		{
			Name: "RecentDispatchFailure",
			OperatorState: fleetOperatorState{
				Kind:       "dispatch_failure",
				Summary:    "Supervisor dispatch failed.",
				NextAction: "Check supervisor logs.",
			},
			// An old attention session must NOT artificially backdate the
			// dispatch failure: buildFleetProjectOperatorState already returned
			// dispatch_failure ahead of attention, so the picked_at should
			// follow the supervisor decision that produced the failure.
			Attention:  []sessionInfo{{Slot: "ghost", Status: string(state.StatusRunning), StartedAt: ancientAttention}},
			Supervisor: supervisorInfo{Latest: &supervisorDecisionInfo{CreatedAt: recentDecision}},
		},
		{
			Name: "OlderDispatchFailure",
			OperatorState: fleetOperatorState{
				Kind:       "dispatch_failure",
				Summary:    "Supervisor dispatch failed earlier.",
				NextAction: "Check supervisor logs.",
			},
			Supervisor: supervisorInfo{Latest: &supervisorDecisionInfo{CreatedAt: olderDecision}},
		},
	}

	got := buildFleetNextAction(projects, nil, now)
	if got == nil {
		t.Fatalf("buildFleetNextAction = nil, want a P0 candidate")
	}
	if got.Priority != "P0" || got.Kind != "dispatch_failure" {
		t.Fatalf("got = %+v, want P0 dispatch_failure", got)
	}
	if got.Project != "OlderDispatchFailure" {
		t.Fatalf("project = %q, want OlderDispatchFailure (older supervisor decision wins, ancient attention session must not pull RecentDispatchFailure backwards)", got.Project)
	}
}

func TestBuildFleetNextActionPickedAtIsStableAcrossSnapshots(t *testing.T) {
	now := time.Now().UTC()
	startedAt := now.Add(-10 * time.Minute).Format(time.RFC3339)
	projects := []fleetProjectState{
		{
			Name: "AttentionProject",
			OperatorState: fleetOperatorState{
				Kind:       "attention",
				Summary:    "Worker needs review.",
				NextAction: "Open the worker detail.",
			},
			Attention: []sessionInfo{{Slot: "att-1", Status: string(state.StatusRunning), StartedAt: startedAt}},
		},
	}
	first := buildFleetNextAction(projects, nil, now)
	second := buildFleetNextAction(projects, nil, now.Add(2*time.Minute))
	if first == nil || second == nil {
		t.Fatalf("snapshots returned nil: first=%v second=%v", first, second)
	}
	if first.PickedAt == "" {
		t.Fatalf("picked_at is empty; want a stable timestamp derived from the input")
	}
	if first.PickedAt != second.PickedAt {
		t.Fatalf("picked_at = %q vs %q across snapshots; want stability when input is unchanged", first.PickedAt, second.PickedAt)
	}
	if first.Project != second.Project || first.Kind != second.Kind {
		t.Fatalf("selection drifted between snapshots: %+v vs %+v", first, second)
	}
}

func TestFleetAPISnapshotIncludesNextAction(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "needs-attention")
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"stuck-1": {
			IssueNumber: 7,
			IssueTitle:  "Stuck",
			Status:      state.StatusRetryExhausted,
			StartedAt:   now.Add(-30 * time.Minute),
			PRNumber:    71,
			// #598: CI failure evidence keeps this session actionable;
			// without it the session would be classified as
			// convergence-bound (auto_merging) and excluded from the
			// next-action picker.
			LastNotifiedStatus: "ci_failure",
			CIFailureOutput:    "FAIL: pkg/example TestStuck",
		},
	})
	srv := NewFleet([]FleetProject{
		NewFleetProject("StuckProject", "/tmp/stuck.yaml", "http://127.0.0.1:8787", &config.Config{
			Repo:        "owner/stuck",
			StateDir:    stateDir,
			MaxParallel: 1,
			Outcome:     outcome.Brief{DesiredOutcome: "Stuck outcome"},
		}),
	}, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp fleetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.NextAction == nil {
		t.Fatalf("next_action = nil, want a structured object")
	}
	if resp.NextAction.Project != "StuckProject" {
		t.Fatalf("next_action.project = %q, want StuckProject", resp.NextAction.Project)
	}
	if resp.NextAction.Priority == "" || resp.NextAction.Kind == "" || resp.NextAction.Reason == "" {
		t.Fatalf("next_action missing required fields: %+v", resp.NextAction)
	}
	// The verdict.sentence stays for backward compatibility.
	if strings.TrimSpace(resp.Verdict.Sentence) == "" {
		t.Fatalf("verdict.sentence is empty; expected backward-compatible sentence to remain populated")
	}

	// Snapshot the JSON top-level keys to lock the shape.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, ok := raw["next_action"]; !ok {
		t.Fatalf("/api/v1/fleet body missing top-level next_action key")
	}
	if _, ok := raw["verdict"]; !ok {
		t.Fatalf("/api/v1/fleet body missing top-level verdict key (backward-compat regression)")
	}
}

func TestFleetAPISnapshotNextActionIsNullWhenQuiet(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "quiet")
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"runner-1": {
			IssueNumber: 1,
			IssueTitle:  "Quiet runner",
			Status:      state.StatusRunning,
			StartedAt:   now.Add(-time.Minute),
			PID:         os.Getpid(),
		},
	})
	srv := NewFleet([]FleetProject{
		NewFleetProject("Quiet", "/tmp/quiet.yaml", "", &config.Config{
			Repo:        "owner/quiet",
			StateDir:    stateDir,
			MaxParallel: 1,
			Outcome:     outcome.Brief{DesiredOutcome: "Quiet outcome"},
		}),
	}, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	body, ok := raw["next_action"]
	if !ok {
		t.Fatalf("next_action key missing from response")
	}
	if string(body) != "null" {
		t.Fatalf("next_action = %s, want null when no operator action is needed", string(body))
	}
}

func TestFleetProjectOperatorStateDistinguishesBriefStates(t *testing.T) {
	now := time.Now().UTC()
	configuredOutcome := outcome.Status{Configured: true, Goal: "Runtime is healthy", HealthState: outcome.HealthHealthy}

	dispatch := buildFleetProjectOperatorState(fleetProjectState{
		Name:    "Dispatch",
		Repo:    "owner/dispatch",
		Outcome: configuredOutcome,
		Supervisor: supervisorInfo{Latest: &supervisorDecisionInfo{
			CreatedAt:         now,
			Status:            "failed",
			ErrorClass:        "github_api",
			Summary:           "Supervisor queue action failed for issue #9.",
			RecommendedAction: "label_issue_ready",
			Target:            &state.SupervisorTarget{Issue: 9},
		}},
	})
	if dispatch.Kind != "dispatch_failure" || dispatch.IssueNumber != 9 {
		t.Fatalf("dispatch operator state = %+v, want dispatch failure for issue #9", dispatch)
	}

	pendingWithinSLA := buildFleetProjectOperatorState(fleetProjectState{
		Name:               "Pending",
		Repo:               "owner/pending",
		Outcome:            configuredOutcome,
		DispatchSLASeconds: 3600,
		Supervisor: supervisorInfo{Latest: &supervisorDecisionInfo{
			CreatedAt:         now.Add(-5 * time.Minute),
			Summary:           "Start a worker for issue #354.",
			RecommendedAction: "spawn_worker",
			Target:            &state.SupervisorTarget{Issue: 354},
		}},
		QueueSnapshot: &fleetQueueSnapshot{
			Open:     1,
			Eligible: 1,
			SelectedCandidate: &state.SupervisorIssueCandidate{
				Number: 354,
				Title:  "Selected next issue",
			},
		},
	})
	if pendingWithinSLA.Kind != "pending_dispatch" || pendingWithinSLA.IssueNumber != 354 {
		t.Fatalf("pending operator state = %+v, want pending dispatch for selected issue #354", pendingWithinSLA)
	}

	pastSLA := buildFleetProjectOperatorState(fleetProjectState{
		Name:               "PastSLA",
		Repo:               "owner/past-sla",
		Outcome:            configuredOutcome,
		DispatchSLASeconds: 60,
		Supervisor: supervisorInfo{Latest: &supervisorDecisionInfo{
			CreatedAt:         now.Add(-5 * time.Minute),
			Summary:           "Start a worker for issue #355.",
			RecommendedAction: "spawn_worker",
			Target:            &state.SupervisorTarget{Issue: 355},
		}},
		QueueSnapshot: &fleetQueueSnapshot{
			Open:     1,
			Eligible: 1,
			SelectedCandidate: &state.SupervisorIssueCandidate{
				Number: 355,
				Title:  "Selected overdue issue",
			},
		},
	})
	if pastSLA.Kind != "dispatch_failure" || pastSLA.IssueNumber != 355 {
		t.Fatalf("past-SLA operator state = %+v, want dispatch failure for selected issue #355", pastSLA)
	}
	if !contains(pastSLA.Summary, "1m SLA") || !contains(pastSLA.Label, "Dispatch SLA") {
		t.Fatalf("past-SLA operator state = %+v, want SLA explanation", pastSLA)
	}

	drift := buildFleetProjectOperatorState(fleetProjectState{
		Name: "Runtime",
		Outcome: outcome.Status{
			Configured:  true,
			Goal:        "Runtime is healthy",
			HealthState: outcome.HealthFailing,
		},
		QueueSnapshot: &fleetQueueSnapshot{Open: 0},
	})
	if drift.Kind != "outcome_drift" || !contains(drift.Summary, "failing") {
		t.Fatalf("drift operator state = %+v, want runtime outcome drift", drift)
	}

	blocked := buildFleetProjectOperatorState(fleetProjectState{
		Name:          "BlockedQueue",
		Outcome:       configuredOutcome,
		QueueSnapshot: &fleetQueueSnapshot{Open: 3, Eligible: 0, Excluded: 3, IdleReason: "Policy excluded all 3 open issues."},
	})
	if blocked.Kind != "no_eligible_issues" || !contains(blocked.Summary, "Policy excluded") {
		t.Fatalf("blocked queue operator state = %+v, want no eligible issues", blocked)
	}

	idle := buildFleetProjectOperatorState(fleetProjectState{
		Name:          "Idle",
		Outcome:       configuredOutcome,
		QueueSnapshot: &fleetQueueSnapshot{Open: 0, IdleReason: "No open issues are available."},
	})
	if idle.Kind != "idle" || idle.Label != "Healthy idle" {
		t.Fatalf("idle operator state = %+v, want healthy idle", idle)
	}
}

func TestFleetVerdictCoversHeaderStates(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name      string
		sessions  map[string]*state.Session
		decisions []state.SupervisorDecision
		wantTone  string
		wantText  []string
	}{
		{
			name: "healthy idle by policy",
			decisions: []state.SupervisorDecision{{
				ID:                "sup-healthy-idle",
				CreatedAt:         now,
				Project:           "owner/healthy-idle",
				Summary:           "No open issues match the configured ready labels.",
				RecommendedAction: "none",
				Risk:              "safe",
				QueueAnalysis: &state.SupervisorQueueAnalysis{
					OpenIssues:         1,
					EligibleCandidates: 0,
					ExcludedIssues:     1,
				},
			}},
			wantTone: "healthy",
			wantText: []string{"Supervisor healthy.", "No worker is running by policy.", "No item needs attention."},
		},
		{
			name: "busy running worker",
			sessions: map[string]*state.Session{
				"busy-1": {
					IssueNumber: 11,
					IssueTitle:  "Build busy thing",
					Status:      state.StatusRunning,
					StartedAt:   now.Add(-time.Minute),
					PID:         os.Getpid(),
				},
			},
			decisions: []state.SupervisorDecision{{
				ID:                "sup-busy",
				CreatedAt:         now,
				Project:           "owner/busy",
				Summary:           "Worker is already running.",
				RecommendedAction: "wait_for_worker",
				Risk:              "safe",
			}},
			wantTone: "busy",
			wantText: []string{"Supervisor healthy.", "1 worker is running.", "No item needs attention."},
		},
		{
			name: "attention required",
			sessions: map[string]*state.Session{
				"dead-1": {
					IssueNumber: 12,
					IssueTitle:  "Dead worker",
					Status:      state.StatusDead,
					StartedAt:   now.Add(-2 * time.Minute),
				},
			},
			decisions: []state.SupervisorDecision{{
				ID:                "sup-attention",
				CreatedAt:         now,
				Project:           "owner/attention",
				Summary:           "Worker needs reconciliation.",
				RecommendedAction: "wait_for_reconciliation",
				Risk:              "safe",
			}},
			wantTone: "attention",
			wantText: []string{"Supervisor healthy.", "No worker is running.", "1 item needs attention."},
		},
		{
			name: "daemon down stale heartbeat",
			decisions: []state.SupervisorDecision{{
				ID:                "sup-stale",
				CreatedAt:         now.Add(-fleetSupervisorHeartbeatStaleAfter - time.Minute),
				Project:           "owner/stale",
				Summary:           "No worker slot is available.",
				RecommendedAction: "none",
				Risk:              "safe",
			}},
			wantTone: "daemon-down",
			wantText: []string{"Supervisor heartbeat lost", "Last safe action was", "No worker is running.", "No item needs attention."},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			stateDir := filepath.Join(dir, "state")
			saveFleetTestSnapshot(t, stateDir, tt.sessions, tt.decisions)
			srv := NewFleet([]FleetProject{
				NewFleetProject("Project", "/tmp/project.yaml", "", &config.Config{
					Repo:        "owner/project",
					StateDir:    stateDir,
					MaxParallel: 1,
				}),
			}, "127.0.0.1", 8786, true)

			resp := srv.snapshot()
			if resp.Verdict.Tone != tt.wantTone {
				t.Fatalf("verdict tone = %q, want %q; sentence=%q", resp.Verdict.Tone, tt.wantTone, resp.Verdict.Sentence)
			}
			for _, want := range tt.wantText {
				if !contains(resp.Verdict.Sentence, want) {
					t.Fatalf("verdict sentence = %q, want %q", resp.Verdict.Sentence, want)
				}
			}
		})
	}
}

// TestFleetSummaryRunningDistinguishesRunningFromRecent pins issue #496:
// the "in flight" hero on the Workers screen must reflect only sessions
// whose status is actually `running` — not every session that fell inside
// the 24-h `Live` activity window. With one running session alongside five
// terminal `done` sessions finished within the last hour, summary.Running
// must read 1, the fleet verdict headline must say "1 worker in flight.",
// and the historical 24-h reach (resp.Summary.Sessions) must still
// account for the recent activity that the SPA renders under "Recent 24h".
func TestFleetSummaryRunningDistinguishesRunningFromRecent(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "in-flight")

	sessions := map[string]*state.Session{
		"running-1": {
			IssueNumber: 1,
			IssueTitle:  "Actually running worker",
			Status:      state.StatusRunning,
			StartedAt:   now.Add(-3 * time.Minute),
			PID:         os.Getpid(),
		},
	}
	for i := 0; i < 5; i++ {
		finished := now.Add(-time.Duration(i+1) * time.Hour)
		started := finished.Add(-30 * time.Minute)
		slot := "done-" + strconv.Itoa(i+1)
		sessions[slot] = &state.Session{
			IssueNumber: 100 + i,
			IssueTitle:  "Recently completed worker",
			Status:      state.StatusDone,
			StartedAt:   started,
			FinishedAt:  &finished,
			PRNumber:    200 + i,
		}
	}
	saveFleetTestSnapshot(t, stateDir, sessions, []state.SupervisorDecision{{
		ID:                "sup-in-flight",
		CreatedAt:         now,
		Project:           "owner/in-flight",
		Summary:           "Worker is already running.",
		RecommendedAction: "wait_for_worker",
		Risk:              "safe",
	}})

	srv := NewFleet([]FleetProject{
		NewFleetProject("InFlight", "/tmp/in-flight.yaml", "", &config.Config{
			Repo:        "owner/in-flight",
			StateDir:    stateDir,
			MaxParallel: 2,
		}),
	}, "127.0.0.1", 8786, true)

	resp := srv.snapshot()

	if resp.Summary.Running != 1 {
		t.Fatalf("summary.running = %d, want 1 (5 done sessions in last 24 h must not count as in-flight)", resp.Summary.Running)
	}
	if resp.Summary.Sessions != 6 {
		t.Fatalf("summary.sessions = %d, want 6 (1 running + 5 done)", resp.Summary.Sessions)
	}

	headline := fleetVerdictHeadline(resp.Summary, resp.Projects[0].Supervisor.Latest, "busy", now)
	if headline != "1 worker in flight." {
		t.Fatalf("verdict headline = %q, want %q (must use summary.Running, not 24-h reach)", headline, "1 worker in flight.")
	}
}

func TestFleetVerdictDoesNotTreatProjectFreshnessStaleAsHeartbeatStale(t *testing.T) {
	now := time.Now().UTC()
	latest := &supervisorDecisionInfo{CreatedAt: now}
	resp := fleetResponse{
		Summary: fleetSummary{Projects: 1, Stale: 1},
		Projects: []fleetProjectState{{
			Supervisor: supervisorInfo{Latest: latest},
		}},
	}

	verdict := buildFleetVerdict(resp, now)
	if verdict.Tone != "attention" {
		t.Fatalf("verdict tone = %q, want attention; sentence=%q", verdict.Tone, verdict.Sentence)
	}
	if contains(verdict.Sentence, "heartbeat stale") || contains(verdict.Sentence, "heartbeat lost") {
		t.Fatalf("verdict sentence = %q, should not label stale snapshots as stale heartbeat", verdict.Sentence)
	}
	for _, want := range []string{"Supervisor healthy.", "1 project snapshot is stale.", "1 item needs attention."} {
		if !contains(verdict.Sentence, want) {
			t.Fatalf("verdict sentence = %q, want %q", verdict.Sentence, want)
		}
	}
}

func TestFleetIdleByPolicyRequiresEveryIdleProjectReason(t *testing.T) {
	policyIdle := fleetProjectState{QueueSnapshot: &fleetQueueSnapshot{IdleReason: "Policy excluded all 1 open issue."}}
	alsoPolicyIdle := fleetProjectState{QueueSnapshot: &fleetQueueSnapshot{IdleReason: "No open issues are available."}}

	if !fleetIdleByPolicy([]fleetProjectState{policyIdle, alsoPolicyIdle}) {
		t.Fatal("fleetIdleByPolicy = false, want true when every idle project has a policy reason")
	}
	if fleetIdleByPolicy([]fleetProjectState{policyIdle, {}}) {
		t.Fatal("fleetIdleByPolicy = true, want false when another idle project lacks a policy reason")
	}
}

func TestFleetAPISurfacesProjectErrorsAndStaleFreshness(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	healthyStateDir := filepath.Join(dir, "healthy")
	staleStateDir := filepath.Join(dir, "stale")
	brokenStateDir := filepath.Join(dir, "broken")
	finished := now.Add(-2 * time.Minute)
	saveFleetTestState(t, healthyStateDir, map[string]*state.Session{
		"healthy-1": {
			IssueNumber: 1,
			IssueTitle:  "Healthy done worker",
			Status:      state.StatusDone,
			StartedAt:   now.Add(-10 * time.Minute),
			FinishedAt:  &finished,
		},
	})
	saveFleetTestState(t, staleStateDir, map[string]*state.Session{
		"stale-1": {
			IssueNumber: 2,
			IssueTitle:  "Stale done worker",
			Status:      state.StatusDone,
			StartedAt:   now.Add(-20 * time.Minute),
			FinishedAt:  &finished,
		},
	})
	staleAt := now.Add(-fleetProjectStaleAfter - time.Minute)
	if err := os.Chtimes(state.StatePath(staleStateDir), staleAt, staleAt); err != nil {
		t.Fatalf("make state stale: %v", err)
	}
	if err := os.MkdirAll(brokenStateDir, 0o755); err != nil {
		t.Fatalf("create broken state dir: %v", err)
	}
	if err := os.WriteFile(state.StatePath(brokenStateDir), []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("write broken state: %v", err)
	}

	srv := NewFleet([]FleetProject{
		NewFleetProject("Healthy", "/tmp/healthy.yaml", "", &config.Config{
			Repo:        "owner/healthy",
			StateDir:    healthyStateDir,
			MaxParallel: 1,
		}),
		NewFleetProject("Stale", "/tmp/stale.yaml", "", &config.Config{
			Repo:        "owner/stale",
			StateDir:    staleStateDir,
			MaxParallel: 1,
		}),
		NewFleetProject("Broken", "/tmp/broken.yaml", "", &config.Config{
			Repo:        "owner/broken",
			StateDir:    brokenStateDir,
			MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)

	resp := srv.snapshot()
	if resp.RefreshedAt == "" {
		t.Fatal("fleet response should include refreshed_at")
	}
	if resp.Summary.Projects != 3 || resp.Summary.Stale != 1 || resp.Summary.Errors != 1 {
		t.Fatalf("summary = %+v, want 3 projects, 1 stale, 1 error", resp.Summary)
	}
	healthy := findFleetProject(t, resp.Projects, "Healthy")
	if healthy.Error != "" || healthy.Sessions != 1 {
		t.Fatalf("healthy project = %+v, want rendered without error", healthy)
	}
	if healthy.Freshness.SnapshotAt == "" || healthy.Freshness.Stale {
		t.Fatalf("healthy freshness = %+v, want fresh snapshot metadata", healthy.Freshness)
	}
	stale := findFleetProject(t, resp.Projects, "Stale")
	if !stale.Freshness.Stale || stale.Freshness.SnapshotAgeSeconds <= int64(fleetProjectStaleAfter/time.Second) {
		t.Fatalf("stale freshness = %+v, want stale snapshot metadata", stale.Freshness)
	}
	if !contains(stale.Freshness.Reason, "stale after") {
		t.Fatalf("stale reason = %q, want threshold explanation", stale.Freshness.Reason)
	}
	broken := findFleetProject(t, resp.Projects, "Broken")
	if broken.Error == "" || broken.Freshness.StateUpdatedAt == "" {
		t.Fatalf("broken project = %+v, want load error with state timestamp", broken)
	}
}

func TestFleetProjectFreshnessUsesRawAgeForStaleThreshold(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	saveFleetTestState(t, stateDir, nil)

	staleAt := now.Add(-fleetProjectStaleAfter - 100*time.Millisecond)
	if err := os.Chtimes(state.StatePath(stateDir), staleAt, staleAt); err != nil {
		t.Fatalf("make state barely stale: %v", err)
	}

	freshness := fleetProjectFreshnessForState(stateDir, nil, now)
	if freshness.SnapshotAgeSeconds != int64(fleetProjectStaleAfter/time.Second) {
		t.Fatalf("snapshot age seconds = %d, want rounded threshold", freshness.SnapshotAgeSeconds)
	}
	if !freshness.Stale {
		t.Fatalf("freshness = %+v, want stale based on raw age", freshness)
	}
}

func TestFleetAPIIncludesApprovalInboxMetadata(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "approvals")
	st := state.NewState()
	st.Sessions["slot-pending"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "Pending approval target",
		Status:      state.StatusRunning,
		StartedAt:   now.Add(-2 * time.Hour),
		PRNumber:    7,
	}
	st.Sessions["slot-stale"] = &state.Session{
		IssueNumber: 43,
		IssueTitle:  "Stale approval target",
		Status:      state.StatusRunning,
		StartedAt:   now.Add(-3 * time.Hour),
	}

	pending := st.RecordPendingApprovalForDecision(state.SupervisorDecision{
		ID:                "approval-pending",
		CreatedAt:         now.Add(-15 * time.Minute),
		Project:           "owner/approvals",
		Mode:              "active",
		Summary:           "Spawn a worker for issue #42.",
		RecommendedAction: "spawn_worker",
		Target:            &state.SupervisorTarget{Issue: 42, Session: "slot-pending"},
		Risk:              "approval_gated",
		Reasons:           []string{"Issue #42 is eligible"},
	}, now.Add(-15*time.Minute))
	approved := st.RecordPendingApprovalForDecision(state.SupervisorDecision{
		ID:                "approval-approved",
		CreatedAt:         now.Add(-30 * time.Minute),
		Project:           "owner/approvals",
		Summary:           "Merge PR #8.",
		RecommendedAction: "approve_merge",
		Target:            &state.SupervisorTarget{PR: 8},
		Risk:              "mutating",
	}, now.Add(-30*time.Minute))
	if _, err := st.ApproveApproval(approved.ID, now.Add(-20*time.Minute), "test", "covered by test"); err != nil {
		t.Fatalf("ApproveApproval: %v", err)
	}
	rejected := st.RecordPendingApprovalForDecision(state.SupervisorDecision{
		ID:                "approval-rejected",
		CreatedAt:         now.Add(-40 * time.Minute),
		Project:           "owner/approvals",
		Summary:           "Mark issue #44 blocked.",
		RecommendedAction: "mark_issue_blocked",
		Target:            &state.SupervisorTarget{Issue: 44},
		Risk:              "mutating",
	}, now.Add(-40*time.Minute))
	if _, err := st.RejectApproval(rejected.ID, now.Add(-25*time.Minute), "test", "covered by test"); err != nil {
		t.Fatalf("RejectApproval: %v", err)
	}
	stale := st.RecordPendingApprovalForDecision(state.SupervisorDecision{
		ID:                "approval-stale",
		CreatedAt:         now.Add(-50 * time.Minute),
		Project:           "owner/approvals",
		Summary:           "Start stale worker.",
		RecommendedAction: "spawn_worker",
		Target:            &state.SupervisorTarget{Issue: 43, Session: "slot-stale"},
		Risk:              "approval_gated",
	}, now.Add(-50*time.Minute))
	// #750: target-state drift no longer stales a pending approval (that churn
	// made the freshest id un-approvable a cycle later). Drive this fixture to
	// the stale state directly so the snapshot still exercises stale rendering.
	stale.Status = state.ApprovalStatusStale
	stale.UpdatedAt = now.Add(-10 * time.Minute)
	if err := state.Save(stateDir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	srv := NewFleet([]FleetProject{
		NewFleetProject("Approvals", "/tmp/approvals.yaml", "http://127.0.0.1:8789", &config.Config{
			Repo:        "owner/approvals",
			StateDir:    stateDir,
			MaxParallel: 2,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	if len(resp.Approvals) != 4 {
		t.Fatalf("fleet approvals len = %d, want 4", len(resp.Approvals))
	}
	if len(resp.Projects) != 1 || len(resp.Projects[0].Approvals) != 4 {
		t.Fatalf("project approvals = %+v, want 4 approvals", resp.Projects)
	}
	if resp.Summary.Approvals != 1 || resp.Summary.ApprovalsPending != 1 || resp.Summary.ApprovalsHistorical != 3 || resp.Summary.ApprovalsStale != 1 || resp.Summary.ApprovalsApproved != 1 || resp.Summary.ApprovalsRejected != 1 {
		t.Fatalf("approval summary = %+v, want one active and three historical approvals", resp.Summary)
	}
	if resp.Projects[0].ApprovalSummary[string(state.ApprovalStatusPending)] != 1 || resp.Projects[0].ApprovalSummary[string(state.ApprovalStatusStale)] != 1 {
		t.Fatalf("project approval summary = %+v, want pending and stale counts", resp.Projects[0].ApprovalSummary)
	}
	if resp.Approvals[0].ID != pending.ID || resp.Approvals[1].ID != stale.ID {
		t.Fatalf("approval order = %q, %q; want pending then stale", resp.Approvals[0].ID, resp.Approvals[1].ID)
	}

	approval := findFleetApproval(t, resp.Approvals, pending.ID)
	if approval.ProjectName != "Approvals" || approval.ProjectRepo != "owner/approvals" || approval.DashboardURL == "" {
		t.Fatalf("approval project metadata = %+v", approval)
	}
	if approval.IssueNumber != 42 || approval.IssueURL != "https://github.com/owner/approvals/issues/42" {
		t.Fatalf("approval issue metadata = %+v", approval)
	}
	if approval.PRNumber != 7 || approval.PRURL != "https://github.com/owner/approvals/pull/7" {
		t.Fatalf("approval PR metadata = %+v", approval)
	}
	if approval.Session != "slot-pending" || approval.SessionStatus != string(state.StatusRunning) {
		t.Fatalf("approval session metadata = %+v", approval)
	}
	if approval.Status != string(state.ApprovalStatusPending) || approval.Action != "spawn_worker" || approval.Risk != "approval_gated" || approval.Summary == "" {
		t.Fatalf("approval lifecycle metadata = %+v", approval)
	}
	if approval.CreatedAge == "" || approval.UpdatedAge == "" || approval.CreatedAgeSeconds <= 0 || approval.UpdatedAgeSeconds <= 0 {
		t.Fatalf("approval ages = %+v, want populated age fields", approval)
	}
	if len(approval.TargetLinks) != 3 {
		t.Fatalf("approval target links = %+v, want issue, PR, and session links", approval.TargetLinks)
	}

	staleApproval := findFleetApproval(t, resp.Approvals, stale.ID)
	if staleApproval.Status != string(state.ApprovalStatusStale) {
		t.Fatalf("stale approval status = %q, want stale", staleApproval.Status)
	}
}

func TestFleetApprovalSummaryCountsOnlyActivePendingApprovals(t *testing.T) {
	var summary fleetSummary
	for _, status := range []string{
		string(state.ApprovalStatusPending),
		string(state.ApprovalStatusSuperseded),
		string(state.ApprovalStatusStale),
		string(state.ApprovalStatusApproved),
		string(state.ApprovalStatusRejected),
	} {
		addFleetApprovalSummary(&summary, fleetApprovalState{Status: status})
	}

	if summary.Approvals != 1 || summary.ApprovalsPending != 1 {
		t.Fatalf("active approval summary = %+v, want one pending active approval", summary)
	}
	if summary.ApprovalsHistorical != 4 || summary.ApprovalsSuperseded != 1 || summary.ApprovalsStale != 1 || summary.ApprovalsApproved != 1 || summary.ApprovalsRejected != 1 {
		t.Fatalf("historical approval summary = %+v, want one per historical status", summary)
	}
}

func TestFleetApprovalTargetReplacesStaleSessionWithMatchedSession(t *testing.T) {
	st := state.NewState()
	st.Sessions["slot-new"] = &state.Session{
		IssueNumber: 42,
		PRNumber:    7,
		Status:      state.StatusRunning,
	}

	issue, pr, session, sessionStatus := fleetApprovalTarget(st, &state.SupervisorTarget{
		Issue:   42,
		Session: "slot-old",
	})

	if issue != 42 || pr != 7 || session != "slot-new" || sessionStatus != string(state.StatusRunning) {
		t.Fatalf("target metadata = issue:%d pr:%d session:%q status:%q, want matched current session", issue, pr, session, sessionStatus)
	}
}

func TestFleetAttentionInboxOrdersBySeverityAndFreshness(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "finance")
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"fin-running": {
			IssueNumber: 306,
			IssueTitle:  "Finance stale-running worker with a title long enough to exercise compact inbox layout",
			Status:      state.StatusRunning,
			StartedAt:   now.Add(-12 * time.Minute),
			Backend:     "opencode",
		},
		"fin-pr": {
			IssueNumber: 307,
			IssueTitle:  "Waiting PR state missing its pull request number",
			Status:      state.StatusPROpen,
			StartedAt:   now.Add(-1 * time.Minute),
		},
		"fin-retry": {
			IssueNumber:     308,
			IssueTitle:      "Retry exhausted with failed checks",
			Status:          state.StatusRetryExhausted,
			StartedAt:       now.Add(-30 * time.Minute),
			PRNumber:        88,
			CIFailureOutput: "go test failed",
		},
		"fin-dead": {
			IssueNumber: 309,
			IssueTitle:  "Dead worker needs reconciliation",
			Status:      state.StatusDead,
			StartedAt:   now.Add(-5 * time.Minute),
		},
	})

	srv := NewFleet([]FleetProject{
		NewFleetProject("finance", "/tmp/finance.yaml", "http://127.0.0.1:8788", &config.Config{
			Repo:        "owner/finance",
			StateDir:    stateDir,
			MaxParallel: 4,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	if len(resp.Attention) != 4 {
		t.Fatalf("attention inbox len = %d, want 4", len(resp.Attention))
	}
	gotSlots := make([]string, 0, len(resp.Attention))
	for _, worker := range resp.Attention {
		gotSlots = append(gotSlots, worker.Slot)
	}
	wantSlots := []string{"fin-dead", "fin-retry", "fin-running", "fin-pr"}
	for i, want := range wantSlots {
		if gotSlots[i] != want {
			t.Fatalf("attention order = %v, want %v", gotSlots, wantSlots)
		}
	}

	stale := findFleetWorker(t, resp.Attention, "fin-running")
	if stale.ProjectName != "finance" || stale.DashboardURL == "" {
		t.Fatalf("stale worker project/link = %+v", stale)
	}
	if stale.IssueNumber != 306 || stale.IssueURL != "https://github.com/owner/finance/issues/306" {
		t.Fatalf("stale worker issue metadata = %+v", stale)
	}
	if stale.Status != string(state.StatusRunning) || !stale.NeedsAttention {
		t.Fatalf("stale worker status/attention = %q/%v", stale.Status, stale.NeedsAttention)
	}
	if !contains(stale.StatusReason, "PID is not alive") || !contains(stale.NextAction, "reconciliation cycle") {
		t.Fatalf("stale worker why/next = %q/%q", stale.StatusReason, stale.NextAction)
	}
	if stale.RuntimeSeconds <= 0 || stale.Runtime == "" {
		t.Fatalf("stale worker age = %q/%d, want populated", stale.Runtime, stale.RuntimeSeconds)
	}

	retry := findFleetWorker(t, resp.Attention, "fin-retry")
	if retry.PRNumber != 88 || retry.PRURL != "https://github.com/owner/finance/pull/88" {
		t.Fatalf("retry PR metadata = %d/%q", retry.PRNumber, retry.PRURL)
	}
}

func TestFleetAttentionSeverityChecksStatusText(t *testing.T) {
	worker := fleetWorkerState{Status: "blocked_waiting"}
	if got := fleetAttentionSeverity(worker); got != 0 {
		t.Fatalf("blocked status severity = %d, want 0", got)
	}
}

func TestFleetWorkersIncludeAllActiveRows(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	sessions := make(map[string]*state.Session)
	for i := 1; i <= 7; i++ {
		slot := "one-" + strconv.Itoa(i)
		sessions[slot] = &state.Session{
			IssueNumber: i,
			IssueTitle:  "Worker",
			Status:      state.StatusRunning,
			StartedAt:   now.Add(-time.Duration(i) * time.Minute),
		}
	}
	saveFleetTestState(t, stateDir, sessions)

	srv := NewFleet([]FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "", &config.Config{
			Repo:        "owner/one",
			StateDir:    stateDir,
			MaxParallel: 7,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	if len(resp.Projects) != 1 {
		t.Fatalf("projects len = %d, want 1", len(resp.Projects))
	}
	if len(resp.Projects[0].Active) != 6 {
		t.Fatalf("project card active len = %d, want capped 6", len(resp.Projects[0].Active))
	}
	if len(resp.Workers) != 7 {
		t.Fatalf("fleet workers len = %d, want all 7", len(resp.Workers))
	}
	if resp.Summary.NeedsAttention != 7 {
		t.Fatalf("needs attention = %d, want 7", resp.Summary.NeedsAttention)
	}
}

func TestFleetWorkersIncludeRecentlyCompletedDoneRows(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	finished := now.Add(-15 * time.Minute)
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"one-1": {
			IssueNumber: 1,
			IssueTitle:  "Done thing",
			Status:      state.StatusDone,
			StartedAt:   now.Add(-45 * time.Minute),
			FinishedAt:  &finished,
		},
	})

	srv := NewFleet([]FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "", &config.Config{
			Repo:        "owner/one",
			StateDir:    stateDir,
			MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	if len(resp.Workers) != 1 {
		t.Fatalf("fleet workers len = %d, want recently completed worker", len(resp.Workers))
	}
	if resp.Workers[0].Status != string(state.StatusDone) {
		t.Fatalf("worker status = %q, want done", resp.Workers[0].Status)
	}
}

func TestFleetWorkersKeepHistoricalSessionsSearchableButOutOfDefaultScope(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	old := now.Add(-72 * time.Hour)
	stateDir := filepath.Join(dir, "state")
	sessions := make(map[string]*state.Session)
	for i := 1; i <= 55; i++ {
		finished := old.Add(-time.Duration(i) * time.Minute)
		slot := "hist-" + strconv.Itoa(i)
		sessions[slot] = &state.Session{
			IssueNumber: 1000 + i,
			IssueTitle:  "Historical done session",
			Status:      state.StatusDone,
			StartedAt:   finished.Add(-30 * time.Minute),
			FinishedAt:  fleetTimePtr(finished),
		}
	}
	recentFinished := now.Add(-15 * time.Minute)
	retryAt := now.Add(30 * time.Minute)
	sessions["live-running"] = &state.Session{
		IssueNumber: 1,
		IssueTitle:  "Running worker",
		Status:      state.StatusRunning,
		StartedAt:   now.Add(-time.Hour),
	}
	sessions["live-pr"] = &state.Session{
		IssueNumber: 2,
		IssueTitle:  "Open PR worker",
		Status:      state.StatusPROpen,
		StartedAt:   now.Add(-2 * time.Hour),
		PRNumber:    22,
	}
	sessions["live-retry"] = &state.Session{
		IssueNumber: 3,
		IssueTitle:  "Retry worker",
		Status:      state.StatusDead,
		StartedAt:   old,
		FinishedAt:  fleetTimePtr(old.Add(time.Hour)),
		NextRetryAt: &retryAt,
	}
	sessions["live-recent"] = &state.Session{
		IssueNumber: 4,
		IssueTitle:  "Recently completed worker",
		Status:      state.StatusDone,
		StartedAt:   now.Add(-45 * time.Minute),
		FinishedAt:  &recentFinished,
	}
	saveFleetTestState(t, stateDir, sessions)

	srv := NewFleet([]FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "", &config.Config{
			Repo:        "owner/one",
			StateDir:    stateDir,
			MaxParallel: 4,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	if len(resp.Workers) != 59 {
		t.Fatalf("fleet workers len = %d, want all 59 searchable sessions", len(resp.Workers))
	}
	project := findFleetProject(t, resp.Projects, "One")
	if project.Sessions != 59 {
		t.Fatalf("project sessions = %d, want 59", project.Sessions)
	}
	if len(project.Active) != 4 {
		t.Fatalf("project active len = %d, want live default set", len(project.Active))
	}

	defaultVisible := 0
	visibleAttention := 0
	historical := 0
	for _, worker := range resp.Workers {
		if worker.Live || worker.NeedsAttention {
			defaultVisible++
			if worker.NeedsAttention {
				visibleAttention++
			}
		} else {
			historical++
		}
	}
	if defaultVisible != 4 || historical != 55 {
		t.Fatalf("default/history counts = %d/%d, want 4/55", defaultVisible, historical)
	}
	if resp.Summary.NeedsAttention != visibleAttention {
		t.Fatalf("summary attention = %d, visible default attention = %d", resp.Summary.NeedsAttention, visibleAttention)
	}

	oldWorker := findFleetWorker(t, resp.Workers, "hist-1")
	if oldWorker.Live || oldWorker.NeedsAttention {
		t.Fatalf("old historical worker = %+v, want searchable but outside default live scope", oldWorker)
	}
	if !findFleetWorker(t, resp.Workers, "live-recent").Live {
		t.Fatal("recently changed done worker should stay in the default live scope")
	}
}

func TestFleetWorkerDetailIncludesMetadataAndLog(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	logFile := filepath.Join(dir, "logs", "one-1.log")
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.WriteFile(logFile, []byte("line one\n\x1b[31mline two\x1b[0m\nline three\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"one-1": {
			IssueNumber:     1,
			IssueTitle:      "Build thing",
			Status:          state.StatusRunning,
			StartedAt:       now.Add(-10 * time.Minute),
			Backend:         "opencode",
			Worktree:        filepath.Join(dir, "worktree"),
			Branch:          "maestro/one-1",
			PID:             999999,
			LogFile:         logFile,
			TokensUsedTotal: 1234,
		},
	})
	srv := NewFleet([]FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "http://127.0.0.1:8787", &config.Config{
			Repo:        "owner/one",
			StateDir:    stateDir,
			MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/worker?project=One&slot=one-1&lines=2", nil)
	w := httptest.NewRecorder()
	srv.handleFleetWorker(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp fleetWorkerDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	worker := resp.Worker
	if worker.ProjectName != "One" || worker.ProjectRepo != "owner/one" || worker.DashboardURL == "" {
		t.Fatalf("worker project metadata = %+v", worker)
	}
	if worker.Worktree == "" || worker.Branch != "maestro/one-1" {
		t.Fatalf("worker worktree/branch = %q/%q", worker.Worktree, worker.Branch)
	}
	if worker.Alive == nil || *worker.Alive {
		t.Fatalf("running worker should distinguish alive=false, got %#v", worker.Alive)
	}
	if !worker.NeedsAttention || !contains(worker.StatusReason, "PID is not alive") {
		t.Fatalf("worker attention reason = %q attention=%v", worker.StatusReason, worker.NeedsAttention)
	}
	if !worker.HasLog || !resp.Log.Available {
		t.Fatalf("log availability worker=%v log=%+v", worker.HasLog, resp.Log)
	}
	if contains(resp.Log.Text, "line one") || contains(resp.Log.Text, "\x1b") {
		t.Fatalf("log text should be tailed and ANSI-stripped: %q", resp.Log.Text)
	}
	if !contains(resp.Log.Text, "line two") || !contains(resp.Log.Text, "line three") {
		t.Fatalf("log text = %q, want recent lines", resp.Log.Text)
	}
	if resp.Log.Lines != 2 {
		t.Fatalf("log lines = %d, want actual tailed line count 2", resp.Log.Lines)
	}
}

func TestFleetWorkerDetailReportsActualLogLineCount(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	logFile := filepath.Join(dir, "logs", "one-1.log")
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.WriteFile(logFile, []byte("line one\nline two\nline three\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"one-1": {
			IssueNumber: 1,
			IssueTitle:  "Build thing",
			Status:      state.StatusDone,
			StartedAt:   now.Add(-10 * time.Minute),
			LogFile:     logFile,
		},
	})
	srv := NewFleet([]FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "", &config.Config{
			Repo:        "owner/one",
			StateDir:    stateDir,
			MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/worker?project=One&slot=one-1&lines=260", nil)
	w := httptest.NewRecorder()
	srv.handleFleetWorker(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp fleetWorkerDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Log.Lines != 3 {
		t.Fatalf("log lines = %d, want actual returned line count 3", resp.Log.Lines)
	}
}

func TestFleetWorkerDetailExplainsUnavailableLog(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"one-1": {
			IssueNumber: 1,
			IssueTitle:  "Done thing",
			Status:      state.StatusDone,
			StartedAt:   now.Add(-20 * time.Minute),
		},
	})
	srv := NewFleet([]FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "", &config.Config{
			Repo:        "owner/one",
			StateDir:    stateDir,
			MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/worker?project=One&slot=one-1", nil)
	w := httptest.NewRecorder()
	srv.handleFleetWorker(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp fleetWorkerDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Worker.Status != string(state.StatusDone) {
		t.Fatalf("worker status = %q, want done", resp.Worker.Status)
	}
	if resp.Log.Available || resp.Log.Reason == "" {
		t.Fatalf("log should be unavailable with a reason: %+v", resp.Log)
	}
}

func TestFleetDashboard(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, true)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.handleFleetDashboard(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	for _, want := range []string{
		"Maestro Mission Control",
		`id="root"`,
		"/static/mc/assets/",
		`data-theme="light"`,
		"/static/maestro-mark.svg",
	} {
		if !contains(body, want) {
			t.Fatalf("mission control shell should contain %q", want)
		}
	}
	mcJS := mcBundleJS(t)
	for _, want := range []string{
		"/api/v1/fleet",
		"/api/v1/fleet/worker",
	} {
		if !contains(mcJS, want) {
			t.Fatalf("mission control bundle should reference %q", want)
		}
	}
}

func TestFleetDashboardRendersHistoryCollapseControls(t *testing.T) {
	body := legacyFleetJS(t)
	for _, want := range []string{
		"function historySummaryRowHTML(workers, expanded)",
		"function historySummaryText(count, expanded)",
		"function toggleWorkerHistoryRows(button)",
		"worker-history-summary-row",
		"worker-history-row",
		"data-history-toggle",
		"aria-expanded=\"",
		"click to \" + (expanded ? \"collapse\" : \"expand\")",
		"history collapsed",
		"hasWorkerDrilldownFilters",
		"worker.live === true",
	} {
		if !contains(body, want) {
			t.Fatalf("dashboard history collapse renderer should contain %q", want)
		}
	}
}

func TestFleetDashboardCanClearProjectWorkerScope(t *testing.T) {
	body := web.MustReadTemplate("fleet.html") + legacyFleetJS(t)
	for _, want := range []string{
		`id="worker-project-reset"`,
		"Show all projects",
		"workerProjectResetEl.hidden = !projectScoped",
		`workerProjectResetEl.addEventListener("click", clearWorkerProjectScope)`,
	} {
		if !contains(body, want) {
			t.Fatalf("dashboard worker scope reset should contain %q", want)
		}
	}

	clearScope := dashboardSnippet(t, body, "function clearWorkerProjectScope()", "projectFilterEl.addEventListener")
	for _, want := range []string{
		`fleetState.selectedProject = "all";`,
		"updateQueryState();",
		"renderFleetWorkers();",
	} {
		if !contains(clearScope, want) {
			t.Fatalf("clear project scope handler should contain %q in:\n%s", want, clearScope)
		}
	}
}

func TestFleetDashboardRendersReadOnlySearchPalette(t *testing.T) {
	body := mcBundleJS(t)
	for _, want := range []string{
		"cmdk",
		"Jump to",
		"Type a command or search",
	} {
		if !contains(body, want) {
			t.Fatalf("mission control command palette should contain %q", want)
		}
	}
}

// TestMissionControlCommandPaletteSearchesFleetData pins the Cmd-K palette
// to its M-010 contract (issue #345): the palette indexes loaded fleet
// data — projects, sessions, GitHub issues, PRs, and approvals — so the
// operator can jump straight to a record by typing its slug or number.
// The Vite bundle is minified but kind: literals and the placeholder
// copy survive intact, so we assert against those rather than function
// names that mangle.
func TestMissionControlCommandPaletteSearchesFleetData(t *testing.T) {
	body := mcBundleJS(t)
	for _, want := range []string{
		// User-visible kinds for each searchable record type.
		`kind:"Project"`,
		`kind:"Dashboard"`,
		`kind:"Session"`,
		`kind:"Issue"`,
		`kind:"PR"`,
		`kind:"Approval"`,
		`kind:"Page"`,
		`kind:"Action"`,
		// Search input copy advertises the supported record types so an
		// operator with no fleet loaded still knows what they can type.
		"projects, slots, issues",
		// Number-prefix aliases (#123, "issue 123", "pr 123") so a bare
		// number is enough to land on a GH record.
		`"#"`,
		`"issue"`,
		`"pr"`,
		// Read-only footer hint surfaces that the palette never mutates state.
		"read-only",
	} {
		if !contains(body, want) {
			t.Fatalf("mission control command palette bundle should contain %q", want)
		}
	}

	// Guardrail: V1 must not expose write actions (approve/reject, retry,
	// dispatch, merge) through the palette UI itself. We allow the strings
	// to appear elsewhere in the bundle (the approvals screen uses them),
	// but the palette must never use any kind: literal that names a
	// write verb.
	for _, banned := range []string{
		`kind:"Approve"`,
		`kind:"Reject"`,
		`kind:"Retry"`,
		`kind:"Merge"`,
		`kind:"Dispatch"`,
	} {
		if contains(body, banned) {
			t.Fatalf("mission control command palette must not expose write action kind %q in V1", banned)
		}
	}
}

// TestMissionControlBrandMarkLinksHome pins the sidebar brand mark to its
// "brand mark → home / overview" contract (issue #537 / gap 13). Before
// the fix the «Maestro / mission control · v1.4» block had hover affordance
// but no navigation — a dead click. The bundle must now ship a real
// <a href="/"> wrapping the brand block AND keep the SPA pushState path
// active so a regular click does not full-page-reload the fleet snapshot.
func TestMissionControlBrandMarkLinksHome(t *testing.T) {
	body := mcBundleJS(t)
	for _, want := range []string{
		// Real anchor with href="/" and the sb-brand class — right-click
		// "open in new tab" / middle-click must work; bare div onClick
		// would silently strip that affordance.
		`href:"/"`,
		`className:"sb-brand"`,
		// aria-label survives minification verbatim — pins the
		// accessibility contract for the brand mark as a home link.
		`"aria-label":"Maestro mission control — home"`,
	} {
		if !contains(body, want) {
			t.Fatalf("mission control brand mark anchor should contain %q", want)
		}
	}
}

// TestMissionControlApprovalSlotShowsActionVerb pins the approval-card
// slot column to its «show action verb when target.session is empty»
// contract (issue #537 / gap 6). spawn_worker / label_issue_ready /
// add_issue_comment are minted before a worker slot exists; the legacy
// fallback rendered "#—" which read as a missing value. The bundle must
// now ship the helper that returns the action verb in that case.
func TestMissionControlApprovalSlotShowsActionVerb(t *testing.T) {
	body := mcBundleJS(t)
	// Action-verb literals the helper falls back to when no concrete
	// numeric target is present. These three actions are the ones called
	// out in the issue (target.session always empty by construction).
	for _, want := range []string{
		`"Starting worker"`,
		`"Mark issue ready"`,
		`"Comment on issue"`,
	} {
		if !contains(body, want) {
			t.Fatalf("mission control bundle should contain action-verb label %q for empty-session approvals", want)
		}
	}
}

func TestFleetDashboardSearchIndexUsesLoadedFleetData(t *testing.T) {
	body := legacyFleetJS(t)
	indexSnippet := dashboardSnippet(t, body, "function buildFleetSearchIndex()", "function fuzzySearchMatch")
	for _, want := range []string{
		"for (const project of fleetState.projects || [])",
		"for (const worker of fleetState.workers || [])",
		"for (const approval of fleetState.approvals || [])",
		`kind: "Project"`,
		`kind: "Dashboard"`,
		`kind: "Session"`,
		`kind: "Issue"`,
		`kind: "PR"`,
		"project.dashboard_url",
		"const url = searchProjectURL(project);",
		"worker.slot",
		"worker.issue_number",
		"worker.pr_number",
		`searchNumberAliases("issue", worker.issue_number)`,
		`searchNumberAliases("pr", worker.pr_number)`,
		"approval.issue_number",
		"approval.pr_number",
	} {
		if !contains(indexSnippet, want) {
			t.Fatalf("search index should contain %q in:\n%s", want, indexSnippet)
		}
	}
}

func TestFleetDashboardSearchRanksDefaultResultsBeforeLimit(t *testing.T) {
	body := legacyFleetJS(t)
	searchSnippet := dashboardSnippet(t, body, "function searchFleetResults(query)", "function searchResultID")
	for _, want := range []string{
		"const limit = searchTerms(query).length ? 12 : 10;",
		"scoreFleetSearchResult(result, query)",
		".sort((left, right) => {",
		".slice(0, limit)",
	} {
		if !contains(searchSnippet, want) {
			t.Fatalf("search results should contain %q in:\n%s", want, searchSnippet)
		}
	}
	if contains(searchSnippet, "if (!searchTerms(query).length) return index.slice(0, 10);") {
		t.Fatalf("default search results should be ranked before truncating in:\n%s", searchSnippet)
	}
}

func TestFleetDashboardSearchKeyboardAndSelectionAreReadOnly(t *testing.T) {
	body := legacyFleetJS(t)
	for _, want := range []string{
		"function isSearchShortcut(event)",
		"(event.metaKey || event.ctrlKey)",
		`toLowerCase() === "k"`,
		"openSearchPalette();",
		`event.key === "ArrowDown"`,
		`event.key === "ArrowUp"`,
		`event.key === "Enter"`,
		`event.key === "Escape"`,
	} {
		if !contains(body, want) {
			t.Fatalf("search keyboard support should contain %q", want)
		}
	}

	inputKeydownSnippet := dashboardSnippet(t, body, `searchInputEl.addEventListener("keydown"`, `projectFilterEl.addEventListener`)
	if !contains(inputKeydownSnippet, "event.stopPropagation();") {
		t.Fatalf("search input Escape handler should stop propagation in:\n%s", inputKeydownSnippet)
	}

	selectionSnippet := dashboardSnippet(t, body, "function openSearchURL(url)", "function workerSearchText(worker)")
	for _, want := range []string{
		`window.open(target, "_blank", "noopener,noreferrer")`,
		"selectWorker(result.project, result.slot)",
		"scopeSearchProject(result.project)",
	} {
		if !contains(selectionSnippet, want) {
			t.Fatalf("search selection should contain %q in:\n%s", want, selectionSnippet)
		}
	}
	for _, unwanted := range []string{"fetch(", "/api/v1/fleet/actions", "renderActions", "action-btn", http.MethodPost} {
		if contains(selectionSnippet, unwanted) {
			t.Fatalf("search selection should not expose write behavior %q in:\n%s", unwanted, selectionSnippet)
		}
	}
}

func TestFleetMCServesSPARoutes(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, true)
	for _, path := range []string{"/", "/fleet", "/workers", "/approvals", "/settings", "/project/demo"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		srv.handleFleetDashboard(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want %d", path, w.Code, http.StatusOK)
		}
		if !contains(w.Body.String(), `id="root"`) {
			t.Fatalf("%s should serve mission control shell", path)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/unknown-route", nil)
	w := httptest.NewRecorder()
	srv.handleFleetDashboard(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown route status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestFleetDashboardRailEmitsInlineDetailRow(t *testing.T) {
	projects := fleetDashboardFixtureProjects(t, 1)
	rail := renderFleetProjectRailRows(fleetDashboardSnapshot(t, projects).Projects)
	for _, want := range []string{
		"project-rail-toggle-cell",
		"data-rail-toggle=",
		"aria-controls=\"rail-detail-",
		"project-rail-detail-row",
		"rail-detail-block rail-detail-queue",
	} {
		if !contains(rail, want) {
			t.Fatalf("server rail renderer should expose %q in:\n%s", want, rail)
		}
	}
}

func TestFleetDashboardRendersUnconfiguredProjectAsSetupState(t *testing.T) {
	stateDir := t.TempDir()
	saveFleetTestSnapshot(t, stateDir, map[string]*state.Session{}, nil)
	projectConfig := &config.Config{
		Repo:        "owner/setup-needed",
		StateDir:    stateDir,
		MaxParallel: 1,
		Server:      config.ServerConfig{ReadOnly: true},
	}
	srv := NewFleet([]FleetProject{
		NewFleetProject("Setup Needed", "/tmp/setup-needed.yaml", "http://127.0.0.1:8787", projectConfig),
	}, "127.0.0.1", 8786, true)
	snapshot := srv.snapshot()
	project := findFleetProject(t, snapshot.Projects, "Setup Needed")

	if !fleetProjectUnconfigured(project) {
		t.Fatalf("project should be treated as unconfigured: %+v", project.Outcome)
	}
	row := renderFleetProjectRailRow(project)
	for _, want := range []string{
		"project-row--unconfigured",
		"project-row-unconfigured",
		"rail-state-unconfigured",
		"setup",
		"No outcome brief configured",
		"Set up &rarr;",
		"setup-link",
	} {
		if !contains(row, want) {
			t.Fatalf("unconfigured rail row should contain %q, got:\n%s", want, row)
		}
	}
	if outcomeHTML := renderFleetProjectRailOutcome(project); contains(outcomeHTML, `<span class="pill`) {
		t.Fatalf("unconfigured outcome rail should not render a health pill, got:\n%s", outcomeHTML)
	}
}

func TestFleetDashboardProjectCardRendersStructuredAttentionBody(t *testing.T) {
	project := fleetProjectState{
		Name:           "maestro",
		Repo:           "BeFeast/maestro",
		DashboardURL:   "http://127.0.0.1:8787",
		MaxParallel:    2,
		NeedsAttention: 1,
		OperatorState: fleetOperatorState{
			Kind:        "attention",
			Tone:        "attention",
			Label:       "Needs attention",
			Summary:     "Worker exited and is waiting for retry.",
			NextAction:  "Run a Maestro reconciliation cycle or review the failed attempt.",
			IssueNumber: 471,
			IssueURL:    "https://github.com/BeFeast/maestro/issues/471",
			Session:     "sup-84",
		},
		Outcome: outcome.Status{Configured: true, Goal: "Healthy", HealthState: outcome.HealthHealthy},
	}
	row := renderFleetProjectRailRow(project)
	for _, want := range []string{
		`data-needs-attention="1"`,
		`rail-structured`,
		`href="https://github.com/BeFeast/maestro/issues/471"`,
		`issue #471`,
		`rail-structured-session`,
		`data-project="maestro"`,
		`data-slot="sup-84"`,
		`(sup-84)`,
		`Run a Maestro reconciliation cycle`,
		`rail-cta rail-cta-attention project-attention-cta`,
		`Open attention &rarr;`,
	} {
		if !contains(row, want) {
			t.Fatalf("attention rail row should contain %q, got:\n%s", want, row)
		}
	}
}

func TestFleetDashboardProjectCardRendersHealthyIdlePill(t *testing.T) {
	project := fleetProjectState{
		Name:          "healthy",
		Repo:          "owner/healthy",
		DashboardURL:  "http://127.0.0.1:8787",
		MaxParallel:   1,
		OperatorState: fleetOperatorState{Kind: "idle", Tone: "healthy", Label: "Healthy idle", Summary: "No open issues."},
		Outcome:       outcome.Status{Configured: true, Goal: "Healthy", HealthState: outcome.HealthHealthy},
	}
	row := renderFleetProjectRailRow(project)
	if !contains(row, `rail-state-healthy_idle`) {
		t.Fatalf("healthy idle row should use rail-state-healthy_idle pill, got:\n%s", row)
	}
	if !contains(row, "Idle · healthy") {
		t.Fatalf("healthy idle row should display 'Idle · healthy' label, got:\n%s", row)
	}
	if contains(row, "project-attention-cta") {
		t.Fatalf("healthy idle row must not render attention CTA, got:\n%s", row)
	}
}

func TestFleetDashboardProjectCardFreshnessShowsAbsoluteTooltipAndClockForLongAges(t *testing.T) {
	project := fleetProjectState{
		Name:          "longidle",
		Repo:          "owner/longidle",
		DashboardURL:  "http://127.0.0.1:8787",
		MaxParallel:   1,
		OperatorState: fleetOperatorState{Kind: "idle", Tone: "healthy", Label: "Healthy idle"},
		Outcome:       outcome.Status{Configured: true, Goal: "Healthy", HealthState: outcome.HealthHealthy},
		Freshness: fleetProjectFreshness{
			SnapshotAt:         "2026-05-31T12:00:00Z",
			SnapshotAge:        "2h0m0s",
			SnapshotAgeSeconds: 7325,
		},
	}
	row := renderFleetProjectRailFreshness(project)
	if !contains(row, "2:02:05") {
		t.Fatalf("freshness > 1h should render hh:mm:ss, got:\n%s", row)
	}
	if !contains(row, "Snapshot at 2026-05-31T12:00:00Z") {
		t.Fatalf("freshness tooltip should include absolute snapshot timestamp, got:\n%s", row)
	}

	short := fleetProjectState{
		Name:    "shortidle",
		Outcome: outcome.Status{Configured: true},
		Freshness: fleetProjectFreshness{
			SnapshotAt:         "2026-05-31T12:00:00Z",
			SnapshotAge:        "50s",
			SnapshotAgeSeconds: 50,
		},
	}
	shortRow := renderFleetProjectRailFreshness(short)
	if !contains(shortRow, "50s") {
		t.Fatalf("freshness < 1h should keep humanized age, got:\n%s", shortRow)
	}
	if contains(shortRow, ":50") {
		t.Fatalf("freshness < 1h should not render hh:mm:ss form, got:\n%s", shortRow)
	}
	if !contains(shortRow, "Snapshot at 2026-05-31T12:00:00Z") {
		t.Fatalf("short freshness tooltip should still include absolute snapshot timestamp, got:\n%s", shortRow)
	}
}

func TestFleetDashboardProjectCardClickHandlerOpensAttentionScope(t *testing.T) {
	js := legacyFleetJS(t)
	for _, want := range []string{
		`function openProjectAttentionDrawer(projectName)`,
		`fleetState.filters.scope = "attention"`,
		`row.dataset.needsAttention === "1"`,
		`projectHasAttentionCTA`,
		`projectStateStructuredHTML`,
		`rail-structured-session`,
		`function formatClockDuration(totalSeconds)`,
		`"Snapshot at "`,
		`Idle · healthy`,
	} {
		if !contains(js, want) {
			t.Fatalf("fleet.js should expose %q for project card behavior", want)
		}
	}
}

func TestFleetDashboardProjectRailPlaceholdersAreNotReplacedFromProjectData(t *testing.T) {
	snapshot := fleetResponse{
		Projects: []fleetProjectState{{
			Name:         "{{FLEET_INITIAL_STATE}}",
			Repo:         "{{FLEET_PROJECT_RAIL_SUMMARY}}",
			ConfigPath:   "{{FLEET_PROJECT_RAIL_ROWS}}",
			DashboardURL: "http://127.0.0.1:8787",
			MaxParallel:  1,
			Outcome: outcome.Status{
				Configured:  true,
				Goal:        "{{FLEET_PROJECT_RAIL_SUMMARY}}",
				HealthState: outcome.HealthUnknown,
			},
			QueueSnapshot: &fleetQueueSnapshot{Open: 1, Eligible: 1},
			Freshness:     fleetProjectFreshness{SnapshotAge: "1m0s"},
		}},
	}
	body, err := renderFleetDashboardHTML(snapshot)
	if err != nil {
		t.Fatalf("render dashboard: %v", err)
	}

	summary := dashboardSnippet(t, body, `<div class="section-note" id="project-rail-summary">`, `</div>`)
	if !contains(summary, "1 project · 0 active · 0 attention") {
		t.Fatalf("summary placeholder was not replaced correctly, got:\n%s", summary)
	}
	rail := dashboardSnippet(t, body, `<tbody id="project-rail-body">`, `</tbody>`)
	if !contains(rail, "{{FLEET_INITIAL_STATE}}") || !contains(rail, "{{FLEET_PROJECT_RAIL_SUMMARY}}") {
		t.Fatalf("rail should preserve placeholder-like project text as data, got:\n%s", rail)
	}

	startMarker := `<script type="application/json" id="fleet-initial-state">`
	script := dashboardSnippet(t, body, startMarker, `</script>`)
	var decoded fleetResponse
	if err := json.Unmarshal([]byte(strings.TrimPrefix(script, startMarker)), &decoded); err != nil {
		t.Fatalf("initial state should remain valid JSON: %v\n%s", err, script)
	}
	if len(decoded.Projects) != 1 || decoded.Projects[0].Name != "{{FLEET_INITIAL_STATE}}" {
		t.Fatalf("initial state project data changed: %+v", decoded.Projects)
	}
}

func TestFleetDashboardServesFleetPath(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, true)
	req := httptest.NewRequest(http.MethodGet, "/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleetDashboard(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if !contains(w.Body.String(), "Maestro Mission Control") {
		t.Fatal("/fleet should serve mission control")
	}
}

func TestFleetDashboardEmbedsConfirmDialogScaffold(t *testing.T) {
	body := web.MustReadTemplate("fleet.html") + legacyFleetJS(t)
	for _, want := range []string{
		`id="confirm-dialog"`,
		`id="confirm-dialog-title"`,
		`id="confirm-dialog-body"`,
		`id="confirm-dialog-reason"`,
		`id="confirm-dialog-trigger"`,
		"function openConfirmDialog",
		"function postAuditLog",
		"/api/v1/audit/log",
		`params.get("v2") === "1"`,
	} {
		if !contains(body, want) {
			t.Fatalf("dashboard should embed confirm dialog scaffold token %q", want)
		}
	}
	resolverIndex := strings.Index(body, "resolver(payload || { confirmed: false")
	closeIndex := strings.Index(body, "confirmDialogEl.close()")
	if resolverIndex < 0 || closeIndex < 0 || resolverIndex > closeIndex {
		t.Fatalf("confirm dialog should resolve before close event can cancel it")
	}
}

func TestFleetAuditLogAppendsJSONLine(t *testing.T) {
	stateDir := t.TempDir()
	srv := NewFleet([]FleetProject{
		NewFleetProject("AuditTarget", "/tmp/audit-target.yaml", "", &config.Config{
			Repo: "owner/audit", StateDir: stateDir, MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)

	body := strings.NewReader(`{"actor":"operator","action":"v2_smoke_test","target":"project-x","reason":"smoke test entry","project":"AuditTarget"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/audit/log", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleFleetAuditLog(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["audit_id"] == "" {
		t.Fatalf("response missing audit_id: %s", w.Body.String())
	}

	logPath := filepath.Join(stateDir, "audit-log.jsonl")
	contents, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	line := strings.TrimSpace(string(contents))
	if line == "" {
		t.Fatalf("audit log is empty")
	}
	var entry fleetAuditLogEntry
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("unmarshal audit line: %v\nline=%s", err, line)
	}
	if entry.Actor != "operator" || entry.Action != "v2_smoke_test" || entry.Reason != "smoke test entry" {
		t.Fatalf("audit entry = %+v, want operator/v2_smoke_test/smoke test entry", entry)
	}
	if entry.AuditID != resp["audit_id"] {
		t.Fatalf("audit_id mismatch: response=%q line=%q", resp["audit_id"], entry.AuditID)
	}
	if entry.Timestamp == "" {
		t.Fatalf("audit entry missing timestamp")
	}
}

func TestFleetAuditLogRequiresActorAndAction(t *testing.T) {
	stateDir := t.TempDir()
	srv := NewFleet([]FleetProject{
		NewFleetProject("AuditTarget", "/tmp/audit-target.yaml", "", &config.Config{
			Repo: "owner/audit", StateDir: stateDir, MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/audit/log", strings.NewReader(`{"action":"only_action"}`))
	w := httptest.NewRecorder()
	srv.handleFleetAuditLog(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing actor: status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestFleetAuditLogRejectsUnknownProject(t *testing.T) {
	stateDir := t.TempDir()
	srv := NewFleet([]FleetProject{
		NewFleetProject("AuditTarget", "/tmp/audit-target.yaml", "", &config.Config{
			Repo: "owner/audit", StateDir: stateDir, MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)

	body := strings.NewReader(`{"actor":"operator","action":"v2_smoke_test","target":"project-x","reason":"smoke test entry","project":"MissingProject"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/audit/log", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleFleetAuditLog(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown project: status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(stateDir, "audit-log.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unknown project should not write fallback audit log, stat err=%v", err)
	}
}

func TestFleetDashboardServesApprovalAuditPath(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, true)
	req := httptest.NewRequest(http.MethodGet, "/approvals/audit", nil)
	w := httptest.NewRecorder()
	srv.handleFleetApprovalAudit(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if !contains(w.Body.String(), "Historical Approvals") || !contains(w.Body.String(), "Back to Fleet") {
		t.Fatalf("approval audit should render dedicated audit page, got:\n%s", w.Body.String())
	}
}

func TestFleetDashboardReadOnlyProjectControlsRenderQuietNote(t *testing.T) {
	body := legacyFleetJS(t)
	readOnlyBranch := dashboardSnippet(t, body,
		"if (project.read_only === true || fleetState.readOnly)",
		"function projectFreshnessHTML")

	if !contains(readOnlyBranch, "Write controls disabled in read-only mode.") {
		t.Fatalf("read-only project controls should render disabled-write footer, got:\n%s", readOnlyBranch)
	}
	if !contains(readOnlyBranch, "project-actions-readonly") {
		t.Fatalf("read-only project controls should use project-actions-readonly class, got:\n%s", readOnlyBranch)
	}
	for _, unwanted := range []string{"action-btn", "<button", "renderActions("} {
		if contains(readOnlyBranch, unwanted) {
			t.Fatalf("read-only project controls should not render button-like controls %q in:\n%s", unwanted, readOnlyBranch)
		}
	}
}

// #477: when not read-only, the legacy fleet view used to render an
// "Approval-gated controls" panel of unconditionally-disabled buttons.
// That dead affordance has been retired — Mission Control owns the live
// POSTs. The non-read-only branch now points operators at MC instead.
func TestFleetDashboardWritableProjectControlsPointToMissionControl(t *testing.T) {
	body := legacyFleetJS(t)
	writableBranch := dashboardSnippet(t, body,
		"if (project.read_only === true || fleetState.readOnly)",
		"function projectFreshnessHTML")

	if !contains(writableBranch, "Project controls live in Mission Control") {
		t.Fatalf("writable project controls should point at Mission Control, got:\n%s", writableBranch)
	}
	for _, unwanted := range []string{
		"Approval-gated controls",
		"action-btn",
		"<button",
	} {
		if contains(writableBranch, unwanted) {
			t.Fatalf("writable project controls should no longer surface dead affordance %q in:\n%s", unwanted, writableBranch)
		}
	}
}

func fleetDashboardBody(t *testing.T) string {
	t.Helper()
	return fleetDashboardBodyWithProjects(t, nil)
}

func fleetDashboardBodyWithProjects(t *testing.T, projects []FleetProject) string {
	t.Helper()
	srv := NewFleet(projects, "127.0.0.1", 8786, true)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.handleFleetDashboard(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	return w.Body.String()
}

func fleetDashboardSnapshot(t *testing.T, projects []FleetProject) fleetResponse {
	t.Helper()
	srv := NewFleet(projects, "127.0.0.1", 8786, true)
	return srv.snapshot()
}

func legacyFleetJS(t *testing.T) string {
	t.Helper()
	return web.MustReadStatic("tokens.css") + web.MustReadStatic("fleet.js") + web.MustReadStatic("fleet.css")
}

func mcBundleJS(t *testing.T) string {
	t.Helper()
	html := web.MustReadStatic("mc/index.html")
	const prefix = `src="/static/mc/assets/`
	start := strings.Index(html, prefix)
	if start < 0 {
		t.Fatal("mc/index.html missing bundled script src")
	}
	start += len(`src="`)
	end := strings.Index(html[start:], `"`)
	if end < 0 {
		t.Fatal("mc/index.html bundled script src malformed")
	}
	rel := html[start : start+end]
	if !strings.HasPrefix(rel, "/static/mc/assets/") {
		t.Fatalf("unexpected mc bundle path %q", rel)
	}
	return web.MustReadStatic(strings.TrimPrefix(rel, "/static/"))
}

func fleetDashboardFixtureProjects(t *testing.T, count int) []FleetProject {
	t.Helper()
	if count == 0 {
		return nil
	}
	dir := t.TempDir()
	now := time.Now().UTC()
	projects := make([]FleetProject, 0, count)
	for i := 1; i <= count; i++ {
		idx := strconv.Itoa(i)
		name := "Project " + idx
		stateDir := filepath.Join(dir, "project-"+idx)
		status := state.StatusDone
		prNumber := 0
		if i%2 == 0 {
			status = state.StatusPROpen
			prNumber = 100 + i
		}
		if i%3 == 0 {
			status = state.StatusRunning
		}
		sessions := map[string]*state.Session{
			"slot-" + idx: {
				IssueNumber: i,
				IssueTitle:  "Issue " + idx,
				Status:      status,
				StartedAt:   now.Add(-time.Duration(i) * time.Minute),
				PRNumber:    prNumber,
			},
		}
		decisions := []state.SupervisorDecision{{
			ID:                "decision-" + idx,
			CreatedAt:         now.Add(-time.Duration(i) * time.Minute),
			Summary:           "Queue snapshot for " + name,
			RecommendedAction: "none",
			Risk:              "low",
			QueueAnalysis: &state.SupervisorQueueAnalysis{
				OpenIssues:                    i + 2,
				EligibleCandidates:            1,
				ExcludedIssues:                i % 3,
				HeldIssues:                    i % 2,
				BlockedByDependencyIssues:     i % 4,
				NonRunnableProjectStatusCount: i % 2,
				SelectedCandidate: &state.SupervisorIssueCandidate{
					Number: i,
					Title:  "Issue " + idx,
				},
			},
		}}
		saveFleetTestSnapshot(t, stateDir, sessions, decisions)
		projects = append(projects, NewFleetProject(name, "/tmp/project-"+idx+".yaml", "http://127.0.0.1:878"+idx, &config.Config{
			Repo:        "owner/project-" + idx,
			StateDir:    stateDir,
			MaxParallel: 2,
			Outcome: outcome.Brief{
				DesiredOutcome: name + " outcome",
				RuntimeTarget:  "https://project-" + idx + ".example.com",
			},
			Server: config.ServerConfig{ReadOnly: true},
		}))
	}
	return projects
}

func dashboardSnippet(t *testing.T, body, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(body, startMarker)
	if start < 0 {
		t.Fatalf("dashboard missing start marker %q", startMarker)
	}
	rest := body[start:]
	end := strings.Index(rest, endMarker)
	if end < 0 {
		t.Fatalf("dashboard missing end marker %q after %q", endMarker, startMarker)
	}
	return rest[:end]
}

func TestFleetAPIFiltersStaleSessionsFromAttention(t *testing.T) {
	resetFleetStaleAuditDedup()
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	missingWorktree := filepath.Join(dir, "missing-worktree")
	presentWorktree := filepath.Join(dir, "present-worktree")
	if err := os.MkdirAll(presentWorktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	finishedOld := now.Add(-30 * time.Hour)
	finishedRecent := now.Add(-2 * time.Hour)
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"stale-1": {
			IssueNumber: 401,
			IssueTitle:  "Old retry-exhausted with no worktree",
			Status:      state.StatusDead,
			StartedAt:   finishedOld.Add(-time.Hour),
			FinishedAt:  &finishedOld,
			Worktree:    missingWorktree,
		},
		"live-running": {
			IssueNumber: 402,
			IssueTitle:  "Healthy worker",
			Status:      state.StatusRunning,
			StartedAt:   now.Add(-2 * time.Minute),
			Worktree:    presentWorktree,
		},
		"present-dead": {
			IssueNumber: 403,
			IssueTitle:  "Dead worker but worktree still present",
			Status:      state.StatusDead,
			StartedAt:   finishedOld.Add(-time.Hour),
			FinishedAt:  &finishedOld,
			Worktree:    presentWorktree,
		},
		"recent-dead": {
			IssueNumber: 404,
			IssueTitle:  "Recently dead worker, still inside idle window",
			Status:      state.StatusDead,
			StartedAt:   finishedRecent.Add(-30 * time.Minute),
			FinishedAt:  &finishedRecent,
			Worktree:    missingWorktree,
		},
	})

	cfg := &config.Config{
		Repo:        "owner/finance",
		StateDir:    stateDir,
		MaxParallel: 4,
	}
	srv := NewFleet([]FleetProject{
		NewFleetProject("finance", "/tmp/finance.yaml", "http://127.0.0.1:8788", cfg),
	}, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp fleetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, worker := range resp.Attention {
		if worker.Slot == "stale-1" {
			t.Fatalf("stale-1 must not appear in attention list: %+v", worker)
		}
	}

	stale := findFleetWorker(t, resp.Workers, "stale-1")
	if stale.NeedsAttention {
		t.Fatalf("stale-1 should be cleared from needs_attention but kept in workers list: %+v", stale)
	}
	if !contains(stale.StatusReason, "stale session reconciled") {
		t.Fatalf("stale-1 status reason = %q, want reconciliation explanation", stale.StatusReason)
	}

	// #566: a 30h-old dead worker ages past FleetAttentionTTL even when
	// its worktree is still on disk — the reconciler keeps it in the
	// workers list (it is not "stale session reconciled"), but the
	// fleet-level TTL drops it from `needs_attention`. The recent-dead
	// session (2h old) stays well inside the window.
	if findFleetWorker(t, resp.Workers, "present-dead").NeedsAttention {
		t.Fatalf("present-dead aged past TTL, must not remain in needs_attention")
	}
	if !findFleetWorker(t, resp.Workers, "recent-dead").NeedsAttention {
		t.Fatalf("recent-dead is inside the idle window, must remain in attention")
	}

	auditPath := filepath.Join(stateDir, "audit-log.jsonl")
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !contains(string(data), "stale_session_reconciled") {
		t.Fatalf("audit log missing reconciler entry: %s", data)
	}
	if !contains(string(data), "stale-1") {
		t.Fatalf("audit log missing stale slot: %s", data)
	}

	// Second snapshot must not duplicate the audit entry.
	w2 := httptest.NewRecorder()
	srv.handleFleet(w2, httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil))
	data2, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log second pass: %v", err)
	}
	if !bytes.Equal(data, data2) {
		t.Fatalf("audit log should be append-once for the same stale session.\nfirst:\n%s\nsecond:\n%s", data, data2)
	}
}

func TestFleetAPIDisabledReconcilerKeepsAttention(t *testing.T) {
	resetFleetStaleAuditDedup()
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	missingWorktree := filepath.Join(dir, "missing-worktree")
	// #566: keep this session well inside FleetAttentionTTL so the test
	// exercises the reconciler-disabled path, not the TTL gate.
	finishedRecent := now.Add(-90 * time.Minute)
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"stale-1": {
			IssueNumber: 501,
			Status:      state.StatusDead,
			StartedAt:   finishedRecent.Add(-time.Hour),
			FinishedAt:  &finishedRecent,
			Worktree:    missingWorktree,
		},
	})

	disabled := false
	cfg := &config.Config{
		Repo:        "owner/finance",
		StateDir:    stateDir,
		MaxParallel: 4,
		StaleSessionReconciler: config.StaleSessionReconcilerConfig{
			Enabled: &disabled,
		},
	}
	srv := NewFleet([]FleetProject{
		NewFleetProject("finance", "/tmp/finance.yaml", "http://127.0.0.1:8788", cfg),
	}, "127.0.0.1", 8786, true)

	resp := srv.snapshot()
	stale := findFleetWorker(t, resp.Workers, "stale-1")
	if !stale.NeedsAttention {
		t.Fatalf("disabled reconciler must not filter attention; got %+v", stale)
	}

	if _, err := os.Stat(filepath.Join(stateDir, "audit-log.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("disabled reconciler must not write audit entries; err = %v", err)
	}
}

// TestFleetAPIStaleDeadSessionsAgeOutOfAttention pins the #471 regression
// for #566: a `dead` session whose newest activity is older than
// FleetAttentionTTL must NOT surface as `live, needs_attention`, regardless
// of the stale-session reconciler config. The fleet verdict must stay
// healthy when the only remaining attention candidates are stale dead
// workers (the "Action required p1" verdict driven by 2-day-old sup-82/83/84
// must go away).
func TestFleetAPIStaleDeadSessionsAgeOutOfAttention(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	// 2 days old, like the sup-82/83/84 sessions on #471.
	finishedAncient := now.Add(-48 * time.Hour)
	freshStarted := now.Add(-10 * time.Minute)
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"sup-82": {
			IssueNumber: 82,
			IssueTitle:  "Ancient dead worker on removed backend",
			Status:      state.StatusDead,
			Backend:     "freellm",
			StartedAt:   finishedAncient.Add(-30 * time.Minute),
			FinishedAt:  &finishedAncient,
		},
		"sup-83": {
			IssueNumber: 83,
			IssueTitle:  "Ancient dead worker (failed)",
			Status:      state.StatusFailed,
			StartedAt:   finishedAncient.Add(-30 * time.Minute),
			FinishedAt:  &finishedAncient,
		},
		"sup-84": {
			IssueNumber: 84,
			IssueTitle:  "Ancient dead worker (conflict_failed)",
			Status:      state.StatusConflictFailed,
			StartedAt:   finishedAncient.Add(-30 * time.Minute),
			FinishedAt:  &finishedAncient,
		},
		"fresh-done": {
			IssueNumber: 90,
			IssueTitle:  "Recently completed worker",
			Status:      state.StatusDone,
			StartedAt:   freshStarted.Add(-30 * time.Minute),
			FinishedAt:  fleetTimePtr(freshStarted),
		},
	})

	srv := NewFleet([]FleetProject{
		NewFleetProject("maestro", "/tmp/maestro.yaml", "", &config.Config{
			Repo:        "befeast/maestro",
			StateDir:    stateDir,
			MaxParallel: 4,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	for _, slot := range []string{"sup-82", "sup-83", "sup-84"} {
		worker := findFleetWorker(t, resp.Workers, slot)
		if worker.NeedsAttention {
			t.Fatalf("%s aged past TTL but still needs_attention: %+v", slot, worker)
		}
		if worker.Live {
			t.Fatalf("%s aged past TTL but still Live=true: %+v", slot, worker)
		}
	}
	for _, w := range resp.Attention {
		switch w.Slot {
		case "sup-82", "sup-83", "sup-84":
			t.Fatalf("stale dead worker %s leaked into attention[]", w.Slot)
		}
	}
	project := findFleetProject(t, resp.Projects, "maestro")
	if project.NeedsAttention != 0 {
		t.Fatalf("project needs_attention = %d, want 0 (stale dead sessions must not count)", project.NeedsAttention)
	}
}

// TestFleetAPIRetryExhaustedWithOpenPRSelfResolvesCalmly pins the #598
// regression. A retry_exhausted session whose linked PR is still open and
// whose last notification is NOT a CI failure is convergence-bound: the
// orchestrator will auto-merge it once the merge gate clears. The session
// must stay surfaced (#564) AND counted in prs_open (#566), but the fleet
// verdict tone must read calm — never the alarming "Action required — p1"
// the legacy code path produced. The matching project card surfaces an
// "auto_merging" operator state so the SPA renders a calm
// "Auto-merging — no action needed" line instead of an attention CTA.
func TestFleetAPIRetryExhaustedWithOpenPRSelfResolvesCalmly(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	// 2 days old, like PR #564 in the dogfood log.
	finishedAncient := now.Add(-48 * time.Hour)
	saveFleetTestSnapshot(t, stateDir, map[string]*state.Session{
		"sup-stuck": {
			IssueNumber:        442,
			IssueTitle:         "Greptile P1 retry-exhausted with open PR",
			Status:             state.StatusRetryExhausted,
			PRNumber:           564,
			StartedAt:          finishedAncient.Add(-time.Hour),
			FinishedAt:         &finishedAncient,
			LastNotifiedStatus: "review_retry_exhausted",
		},
	}, []state.SupervisorDecision{
		{
			ID:                "dec-recent",
			CreatedAt:         now.Add(-1 * time.Minute),
			Project:           "maestro",
			RecommendedAction: "monitor_open_pr",
			Risk:              "safe",
		},
	})

	srv := NewFleet([]FleetProject{
		NewFleetProject("maestro", "/tmp/maestro.yaml", "", &config.Config{
			Repo:        "befeast/maestro",
			StateDir:    stateDir,
			MaxParallel: 4,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	stuck := findFleetWorker(t, resp.Workers, "sup-stuck")
	if !stuck.NeedsAttention {
		t.Fatalf("retry_exhausted with open PR must keep needs_attention regardless of age: %+v", stuck)
	}
	if !stuck.Live {
		t.Fatalf("retry_exhausted with open PR must stay Live: %+v", stuck)
	}
	if stuck.PRNumber != 564 {
		t.Fatalf("attention worker PR = %d, want 564", stuck.PRNumber)
	}

	foundInAttention := false
	for _, w := range resp.Attention {
		if w.Slot == "sup-stuck" {
			foundInAttention = true
			break
		}
	}
	if !foundInAttention {
		t.Fatalf("retry_exhausted with open PR must appear in attention[]: %+v", resp.Attention)
	}

	project := findFleetProject(t, resp.Projects, "maestro")
	if project.PRsOpen < 1 {
		t.Fatalf("project.prs_open = %d, want >=1 (retry_exhausted PR counts as open)", project.PRsOpen)
	}
	if resp.Summary.NeedsAttention < 1 {
		t.Fatalf("summary needs_attention = %d, want >=1", resp.Summary.NeedsAttention)
	}
	if resp.Summary.SelfResolving != resp.Summary.NeedsAttention {
		t.Fatalf("summary self_resolving = %d, want %d (every attention item is convergence-bound)", resp.Summary.SelfResolving, resp.Summary.NeedsAttention)
	}
	if resp.Verdict.Tone != "healthy" {
		t.Fatalf("verdict tone = %q, want healthy (convergence-bound PR must not alarm; #598)", resp.Verdict.Tone)
	}
	if project.OperatorState.Kind != "auto_merging" {
		t.Fatalf("project operator_state.kind = %q, want auto_merging (#598)", project.OperatorState.Kind)
	}
	if project.OperatorState.Tone != "healthy" {
		t.Fatalf("project operator_state.tone = %q, want healthy (#598)", project.OperatorState.Tone)
	}
	if resp.NextAction != nil {
		t.Fatalf("next_action = %+v, want nil (convergence-bound items are not operator-actionable; #598)", resp.NextAction)
	}
}

// TestFleetAPIRetryExhaustedWithFailedChecksRemainsActionable pins the
// alarm side of #598: a retry_exhausted PR whose last notification IS a
// CI failure is NOT convergence-bound (failing checks block the auto-merge
// path), so the fleet verdict must remain `attention` and the project
// card must surface the actionable kind, not the calm "auto_merging" one.
func TestFleetAPIRetryExhaustedWithFailedChecksRemainsActionable(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	finishedRecent := now.Add(-30 * time.Minute)
	saveFleetTestSnapshot(t, stateDir, map[string]*state.Session{
		"sup-failing": {
			IssueNumber:        443,
			IssueTitle:         "Retry exhausted with failing CI",
			Status:             state.StatusRetryExhausted,
			PRNumber:           600,
			StartedAt:          finishedRecent.Add(-time.Hour),
			FinishedAt:         &finishedRecent,
			LastNotifiedStatus: "ci_failure",
			CIFailureOutput:    "FAIL: pkg/foo TestBar",
		},
	}, []state.SupervisorDecision{
		{
			ID:                "dec-fail",
			CreatedAt:         now.Add(-1 * time.Minute),
			Project:           "maestro",
			RecommendedAction: "monitor_open_pr",
			Risk:              "safe",
		},
	})

	srv := NewFleet([]FleetProject{
		NewFleetProject("maestro", "/tmp/maestro.yaml", "", &config.Config{
			Repo:        "befeast/maestro",
			StateDir:    stateDir,
			MaxParallel: 4,
		}),
	}, "127.0.0.1", 8786, true)
	resp := srv.snapshot()

	if resp.Summary.NeedsAttention < 1 {
		t.Fatalf("summary needs_attention = %d, want >=1", resp.Summary.NeedsAttention)
	}
	if resp.Summary.SelfResolving != 0 {
		t.Fatalf("summary self_resolving = %d, want 0 (failing CI is not convergence-bound)", resp.Summary.SelfResolving)
	}
	if resp.Verdict.Tone != "attention" {
		t.Fatalf("verdict tone = %q, want attention (failing-CI PR is operator-actionable)", resp.Verdict.Tone)
	}
	project := findFleetProject(t, resp.Projects, "maestro")
	if project.OperatorState.Kind != "attention" {
		t.Fatalf("project operator_state.kind = %q, want attention", project.OperatorState.Kind)
	}
	if resp.NextAction == nil {
		t.Fatalf("next_action = nil, want a candidate naming PR #600")
	}
	if resp.NextAction.PRNumber != 600 {
		t.Fatalf("next_action.pr_number = %d, want 600", resp.NextAction.PRNumber)
	}
	if cta := resp.NextAction.CTALabel; cta == "" || !contains(cta, "600") {
		t.Fatalf("next_action.cta_label = %q, want a label naming PR #600", cta)
	}
}

// TestFleetAPIProjectCountersNonNull pins the #566 contract: per-project
// prs_open and workers_running are plain ints that always serialise as
// numbers — never null — so the SPA project card never falls back to
// "PRS OPEN 0 / WORKERS 0" because of a missing key.
func TestFleetAPIProjectCountersNonNull(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"slot-running": {
			IssueNumber: 1,
			IssueTitle:  "Running",
			Status:      state.StatusRunning,
			StartedAt:   now.Add(-5 * time.Minute),
			PID:         os.Getpid(),
		},
		"slot-pr-open": {
			IssueNumber: 2,
			IssueTitle:  "Waiting on PR",
			Status:      state.StatusPROpen,
			PRNumber:    100,
			StartedAt:   now.Add(-30 * time.Minute),
		},
		"slot-retry-pr": {
			IssueNumber: 3,
			IssueTitle:  "Retry exhausted with PR still open",
			Status:      state.StatusRetryExhausted,
			PRNumber:    101,
			StartedAt:   now.Add(-10 * time.Minute),
		},
	})

	srv := NewFleet([]FleetProject{
		NewFleetProject("maestro", "/tmp/maestro.yaml", "", &config.Config{
			Repo:        "befeast/maestro",
			StateDir:    stateDir,
			MaxParallel: 4,
		}),
	}, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	// The raw JSON must include the keys with numeric values — `null`
	// fails the contract.
	body := w.Body.String()
	if !strings.Contains(body, `"prs_open":`) {
		t.Fatalf("response missing prs_open key: %s", body)
	}
	if !strings.Contains(body, `"workers_running":`) {
		t.Fatalf("response missing workers_running key: %s", body)
	}
	if strings.Contains(body, `"prs_open":null`) || strings.Contains(body, `"workers_running":null`) {
		t.Fatalf("counters must never be null: %s", body)
	}

	var resp fleetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	project := findFleetProject(t, resp.Projects, "maestro")
	if project.WorkersRunning != 1 {
		t.Fatalf("workers_running = %d, want 1", project.WorkersRunning)
	}
	// PRsOpen includes both StatusPROpen and retry_exhausted-with-PR.
	if project.PRsOpen < 2 {
		t.Fatalf("prs_open = %d, want >=2 (StatusPROpen + retry_exhausted with PR)", project.PRsOpen)
	}
}

func TestFleetAPIDismissesDeadSessionWhenLinkedPRMerged(t *testing.T) {
	resetFleetStaleAuditDedup()
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	presentWorktree := filepath.Join(dir, "present-worktree")
	if err := os.MkdirAll(presentWorktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	finishedRecent := now.Add(-15 * time.Minute) // inside idle window
	branch := "feat/sup-46-347-confirmation-dialog"
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"sup-46": {
			IssueNumber: 347,
			IssueTitle:  "Confirmation dialog",
			PRNumber:    396,
			Status:      state.StatusRetryExhausted,
			Branch:      branch,
			StartedAt:   finishedRecent.Add(-time.Hour),
			FinishedAt:  &finishedRecent,
			Worktree:    presentWorktree,
		},
		"sup-44": {
			IssueNumber: 347,
			IssueTitle:  "Confirmation dialog (merged sibling)",
			PRNumber:    396,
			Status:      state.StatusCodeLanded,
			Branch:      branch,
			StartedAt:   finishedRecent.Add(-2 * time.Hour),
			FinishedAt:  &finishedRecent,
		},
	})

	cfg := &config.Config{
		Repo:        "owner/finance",
		StateDir:    stateDir,
		MaxParallel: 4,
	}
	srv := NewFleet([]FleetProject{
		NewFleetProject("finance", "/tmp/finance.yaml", "http://127.0.0.1:8788", cfg),
	}, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp fleetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, worker := range resp.Attention {
		if worker.Slot == "sup-46" {
			t.Fatalf("sup-46 must drop out of attention when its linked PR is merged: %+v", worker)
		}
	}

	stale := findFleetWorker(t, resp.Workers, "sup-46")
	if stale.NeedsAttention {
		t.Fatalf("sup-46 needs_attention should be cleared; got %+v", stale)
	}
	if !contains(stale.StatusReason, state.MergedPRReason) {
		t.Fatalf("sup-46 status reason = %q, want %q reflected", stale.StatusReason, state.MergedPRReason)
	}

	auditPath := filepath.Join(stateDir, "audit-log.jsonl")
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !contains(string(data), state.MergedPRReason) {
		t.Fatalf("audit log missing %q reason: %s", state.MergedPRReason, data)
	}

	// Second snapshot must not duplicate the audit entry.
	srv.handleFleet(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil))
	data2, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log second pass: %v", err)
	}
	if !bytes.Equal(data, data2) {
		t.Fatalf("audit log should be append-once for the same merged-PR reconciliation.\nfirst:\n%s\nsecond:\n%s", data, data2)
	}
}

func TestFleetAPIMergedPRDismissesDisabledKeepsAttention(t *testing.T) {
	resetFleetStaleAuditDedup()
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	presentWorktree := filepath.Join(dir, "present-worktree")
	if err := os.MkdirAll(presentWorktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	finishedRecent := now.Add(-15 * time.Minute) // inside idle window
	branch := "feat/sup-46-347-confirmation-dialog"
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"sup-46": {
			IssueNumber: 347,
			PRNumber:    396,
			Status:      state.StatusRetryExhausted,
			Branch:      branch,
			StartedAt:   finishedRecent.Add(-time.Hour),
			FinishedAt:  &finishedRecent,
			Worktree:    presentWorktree,
		},
		"sup-44": {
			IssueNumber: 347,
			PRNumber:    396,
			Status:      state.StatusCodeLanded,
			Branch:      branch,
			StartedAt:   finishedRecent.Add(-2 * time.Hour),
			FinishedAt:  &finishedRecent,
		},
	})

	mergedDisabled := false
	cfg := &config.Config{
		Repo:        "owner/finance",
		StateDir:    stateDir,
		MaxParallel: 4,
		StaleSessionReconciler: config.StaleSessionReconcilerConfig{
			MergedPRDismisses: &mergedDisabled,
		},
	}
	srv := NewFleet([]FleetProject{
		NewFleetProject("finance", "/tmp/finance.yaml", "http://127.0.0.1:8788", cfg),
	}, "127.0.0.1", 8786, true)

	resp := srv.snapshot()
	stale := findFleetWorker(t, resp.Workers, "sup-46")
	if !stale.NeedsAttention {
		t.Fatalf("merged_pr_dismisses=false must preserve PR-#400 behavior; got %+v", stale)
	}
}

// TestFleetAPIDoesNotDismissOnStaleDoneSibling guards against the
// non-merge StatusDone path (issue closed externally) being misread as
// "linked PR merged" by the reconciler. A sibling session on the same
// branch in StatusDone — with no StatusCodeLanded record anywhere — must
// not contribute to the merged-branch set.
func TestFleetAPIDoesNotDismissOnStaleDoneSibling(t *testing.T) {
	resetFleetStaleAuditDedup()
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	presentWorktree := filepath.Join(dir, "present-worktree")
	if err := os.MkdirAll(presentWorktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	finishedRecent := now.Add(-15 * time.Minute) // inside idle window
	branch := "feat/sup-46-347-confirmation-dialog"
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		"sup-46": {
			IssueNumber: 347,
			PRNumber:    396,
			Status:      state.StatusRetryExhausted,
			Branch:      branch,
			StartedAt:   finishedRecent.Add(-time.Hour),
			FinishedAt:  &finishedRecent,
			Worktree:    presentWorktree,
		},
		// Sibling reached StatusDone via a non-merge transition (issue
		// closed externally). It must not be treated as evidence that
		// PR #396 actually merged.
		"sup-44": {
			IssueNumber: 347,
			PRNumber:    396,
			Status:      state.StatusDone,
			Branch:      branch,
			StartedAt:   finishedRecent.Add(-2 * time.Hour),
			FinishedAt:  &finishedRecent,
		},
	})

	cfg := &config.Config{
		Repo:        "owner/finance",
		StateDir:    stateDir,
		MaxParallel: 4,
	}
	srv := NewFleet([]FleetProject{
		NewFleetProject("finance", "/tmp/finance.yaml", "http://127.0.0.1:8788", cfg),
	}, "127.0.0.1", 8786, true)

	resp := srv.snapshot()
	stale := findFleetWorker(t, resp.Workers, "sup-46")
	if !stale.NeedsAttention {
		t.Fatalf("sup-46 must remain in attention when no StatusCodeLanded sibling proves PR-merge; got %+v", stale)
	}
}

// TestFleetAPIDoesNotDismissWhenBranchReusedForDifferentPR guards
// against the issue-reopen scenario: an old session on branch X with PR
// 100 reached StatusCodeLanded; the issue was re-opened and a new
// session on the SAME branch but a DIFFERENT PR (200, still open)
// reached StatusRetryExhausted. The new session must not be dismissed
// just because the old PR on the same branch was merged.
func TestFleetAPIDoesNotDismissWhenBranchReusedForDifferentPR(t *testing.T) {
	resetFleetStaleAuditDedup()
	dir := t.TempDir()
	now := time.Now().UTC()
	stateDir := filepath.Join(dir, "state")
	presentWorktree := filepath.Join(dir, "present-worktree")
	if err := os.MkdirAll(presentWorktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	finishedRecent := now.Add(-15 * time.Minute)
	branch := "feat/sup-46-347-confirmation-dialog"
	saveFleetTestState(t, stateDir, map[string]*state.Session{
		// Old, properly-merged session on this branch.
		"sup-44": {
			IssueNumber: 347,
			PRNumber:    100,
			Status:      state.StatusCodeLanded,
			Branch:      branch,
			StartedAt:   finishedRecent.Add(-3 * time.Hour),
			FinishedAt:  &finishedRecent,
		},
		// Issue re-opened, new retry on the same branch with a NEW PR
		// that is still open; the retry exhausted. Must remain in
		// attention.
		"sup-46": {
			IssueNumber: 347,
			PRNumber:    200,
			Status:      state.StatusRetryExhausted,
			Branch:      branch,
			StartedAt:   finishedRecent.Add(-time.Hour),
			FinishedAt:  &finishedRecent,
			Worktree:    presentWorktree,
		},
	})

	cfg := &config.Config{
		Repo:        "owner/finance",
		StateDir:    stateDir,
		MaxParallel: 4,
	}
	srv := NewFleet([]FleetProject{
		NewFleetProject("finance", "/tmp/finance.yaml", "http://127.0.0.1:8788", cfg),
	}, "127.0.0.1", 8786, true)

	resp := srv.snapshot()
	stale := findFleetWorker(t, resp.Workers, "sup-46")
	if !stale.NeedsAttention {
		t.Fatalf("sup-46 must keep needs_attention when its own PR (200) is not the merged one (100); got %+v", stale)
	}
}

func resetFleetStaleAuditDedup() {
	fleetStaleAuditMu.Lock()
	fleetStaleAuditEmitted = make(map[string]struct{})
	fleetStaleAuditMu.Unlock()
}

func TestFleetActionReadOnlyRejectsMutation(t *testing.T) {
	srv := NewFleet(nil, "127.0.0.1", 8786, true)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/actions", nil)
	w := httptest.NewRecorder()
	srv.handleFleetAction(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if !contains(w.Body.String(), "read-only") {
		t.Fatalf("response = %q, want read-only explanation", w.Body.String())
	}
}

func TestFleetActionProjectReadOnlyRejectsMutation(t *testing.T) {
	srv := NewFleet([]FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "", &config.Config{
			Repo:   "owner/one",
			Server: config.ServerConfig{ReadOnly: true},
		}),
	}, "127.0.0.1", 8786, false)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/actions", bytes.NewBufferString(`{"action_id":"restart_worker","project":"One","slot":"one-1"}`))
	w := httptest.NewRecorder()
	srv.handleFleetAction(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if !contains(w.Body.String(), "read-only") {
		t.Fatalf("response = %q, want read-only explanation", w.Body.String())
	}
}

// TestFleetAction_Never501 pins the #475 contract for the fleet handler:
// the safe + approval dispatchers cover every verb that should ever land
// here, so no payload may surface the legacy "approval-backed action
// endpoints are not implemented yet" 501 stub.
func TestFleetAction_Never501(t *testing.T) {
	srv := NewFleet([]FleetProject{
		NewFleetProject("one", "/tmp/one.yaml", "", &config.Config{
			Repo:   "owner/one",
			Server: config.ServerConfig{ReadOnly: false},
			Supervisor: config.SupervisorConfig{
				ReadyLabel:   "maestro-ready",
				BlockedLabel: "blocked",
			},
		}),
	}, "127.0.0.1", 8786, false)

	for _, payload := range []string{
		``,
		`{}`,
		`{"project":"one","action_id":""}`,
		`{"project":"one","action_id":"do_a_barrel_roll"}`,
		`{"project":"one","action_id":"do_a_barrel_roll","issue_number":1}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/actions", bytes.NewBufferString(payload))
		w := httptest.NewRecorder()
		srv.handleFleetAction(w, req)
		if w.Code == http.StatusNotImplemented {
			t.Errorf("payload %q: status = 501; the legacy 'not implemented' stub must be gone (#475)", payload)
		}
		if contains(w.Body.String(), "not implemented yet") {
			t.Errorf("payload %q: body still surfaces legacy stub text: %s", payload, w.Body.String())
		}
	}
}

func findFleetWorker(t *testing.T, workers []fleetWorkerState, slot string) fleetWorkerState {
	t.Helper()
	for _, worker := range workers {
		if worker.Slot == slot {
			return worker
		}
	}
	t.Fatalf("worker %q not found in %+v", slot, workers)
	return fleetWorkerState{}
}

func findFleetProject(t *testing.T, projects []fleetProjectState, name string) fleetProjectState {
	t.Helper()
	for _, project := range projects {
		if project.Name == name {
			return project
		}
	}
	t.Fatalf("project %q not found in %+v", name, projects)
	return fleetProjectState{}
}

func findFleetApproval(t *testing.T, approvals []fleetApprovalState, id string) fleetApprovalState {
	t.Helper()
	for _, approval := range approvals {
		if approval.ID == id {
			return approval
		}
	}
	t.Fatalf("approval %q not found in %+v", id, approvals)
	return fleetApprovalState{}
}

func saveFleetTestState(t *testing.T, dir string, sessions map[string]*state.Session) {
	t.Helper()
	saveFleetTestSnapshot(t, dir, sessions, nil)
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

// TestFleetAPIIncludesProjectBoardSnapshot pins #529: when a fleet project
// has its GitHub Project board client wired, the snapshot exposes a
// project_board with the canonical URL and the per-column WIP counts the
// MC SPA renders on the project drawer.
func TestFleetAPIIncludesCloseCandidatesAndBatchAction(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:        "owner/repo",
		StateDir:    dir,
		MaxParallel: 2,
	}
	finished := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	st := state.NewState()
	st.Sessions["sup-1"] = &state.Session{IssueNumber: 7, PRNumber: 70, Status: state.StatusDone, FinishedAt: &finished}
	st.Sessions["sup-2"] = &state.Session{IssueNumber: 8, PRNumber: 80, Status: state.StatusDone, FinishedAt: &finished}
	st.Approvals = append(st.Approvals, state.Approval{
		Action: config.SupervisorActionCloseIssue,
		Status: state.ApprovalStatusExecuted,
		Target: &state.SupervisorTarget{Issue: 8},
	})
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	fleet := NewFleet([]FleetProject{NewFleetProject("Project", "", "", cfg)}, "127.0.0.1", 0, false)
	resp := fleet.snapshot()
	if len(resp.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(resp.Projects))
	}
	project := resp.Projects[0]
	if len(project.CloseCandidates) != 1 || project.CloseCandidates[0].IssueNumber != 7 || project.CloseCandidates[0].PRNumber != 70 {
		t.Fatalf("close candidates = %+v, want only issue #7 / PR #70", project.CloseCandidates)
	}
	var batch *controlAction
	for i := range project.Actions {
		if project.Actions[i].ID == config.SupervisorActionCloseIssueBatch {
			batch = &project.Actions[i]
			break
		}
	}
	if batch == nil {
		t.Fatalf("project actions = %+v, want close_issue_batch action", project.Actions)
	}
	if !batch.RequiresApproval || len(batch.Issues) != 1 || batch.Issues[0].Issue != 7 {
		t.Fatalf("batch action = %+v, want one approval-gated issue target", batch)
	}
}

func TestFleetAPIIncludesProjectBoardSnapshot(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "one")
	saveFleetTestState(t, stateDir, map[string]*state.Session{})

	project := NewFleetProject("Maestro", "/tmp/one.yaml", "", &config.Config{
		Repo:        "befeast/maestro",
		StateDir:    stateDir,
		MaxParallel: 1,
	})
	// Pre-seed the board snapshot so the snapshot path does not need to
	// touch the network; the refresher goroutine is started by
	// FleetServer.Start in production.
	project.board = &boardState{
		client:        fakeFleetBoardClient{},
		projectNumber: 5,
		snapshot: &fleetProjectBoard{
			Number:    5,
			URL:       "https://github.com/orgs/befeast/projects/5",
			Owner:     "befeast",
			OwnerType: "Organization",
			Columns: []ghProjects.ProjectBoardColumn{
				{Name: "Todo", OptionID: "opt-todo", Count: 4},
				{Name: "In Progress", OptionID: "opt-progress", Count: 2},
				{Name: "Done", OptionID: "opt-done", Count: 11},
			},
			TotalItems: 17,
			FetchedAt:  time.Now().UTC().Format(time.RFC3339),
		},
	}
	projects := []FleetProject{project}
	srv := NewFleet(projects, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp fleetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Projects) != 1 {
		t.Fatalf("projects len = %d, want 1", len(resp.Projects))
	}
	board := resp.Projects[0].ProjectBoard
	if board == nil {
		t.Fatalf("expected project_board on snapshot, got nil")
	}
	if board.URL != "https://github.com/orgs/befeast/projects/5" {
		t.Errorf("board URL = %q", board.URL)
	}
	if board.Number != 5 {
		t.Errorf("board number = %d, want 5", board.Number)
	}
	if board.TotalItems != 17 {
		t.Errorf("board total = %d, want 17", board.TotalItems)
	}
	if len(board.Columns) != 3 {
		t.Fatalf("board columns = %d, want 3", len(board.Columns))
	}
	if board.Columns[0].Name != "Todo" || board.Columns[0].Count != 4 {
		t.Errorf("Todo column = %+v", board.Columns[0])
	}
	if board.Columns[1].Name != "In Progress" || board.Columns[1].Count != 2 {
		t.Errorf("InProgress column = %+v", board.Columns[1])
	}
}

// TestFleetAPIOmitsProjectBoardWhenUnconfigured pins that projects without
// github_projects wiring keep the snapshot minimal — no empty board
// placeholders that would mislead the SPA into rendering a widget.
func TestFleetAPIOmitsProjectBoardWhenUnconfigured(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "one")
	saveFleetTestState(t, stateDir, map[string]*state.Session{})
	projects := []FleetProject{
		NewFleetProject("Maestro", "/tmp/one.yaml", "", &config.Config{
			Repo:        "befeast/maestro",
			StateDir:    stateDir,
			MaxParallel: 1,
		}),
	}
	srv := NewFleet(projects, "127.0.0.1", 8786, true)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleet(w, req)
	var resp fleetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Projects[0].ProjectBoard != nil {
		t.Fatalf("project_board should be nil when github_projects is unconfigured, got %+v", resp.Projects[0].ProjectBoard)
	}
}

// fakeFleetBoardClient is a no-op stand-in for the GitHub board client used
// when seeding cached snapshots directly in tests.
type fakeFleetBoardClient struct{}

func (fakeFleetBoardClient) DiscoverProject(int) (*ghProjects.ProjectField, error) {
	return nil, errors.New("not implemented")
}

func (fakeFleetBoardClient) ListProjectItemStatusCounts(*ghProjects.ProjectField) ([]ghProjects.ProjectBoardColumn, int, error) {
	return nil, 0, errors.New("not implemented")
}

func saveFleetTestSnapshot(t *testing.T, dir string, sessions map[string]*state.Session, decisions []state.SupervisorDecision) {
	t.Helper()
	st := state.NewState()
	for name, sess := range sessions {
		st.Sessions[name] = sess
	}
	for _, decision := range decisions {
		st.RecordSupervisorDecision(decision, state.DefaultSupervisorDecisionLimit)
	}
	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}
}

func fleetTimePtr(t time.Time) *time.Time {
	return &t
}

func assertFleetReadOnlyAction(t *testing.T, action controlAction) {
	t.Helper()
	wantLabels := map[string]string{
		"restart_worker":     "Restart",
		"stop_worker":        "Stop",
		"mark_issue_ready":   "Mark ready",
		"mark_issue_blocked": "Mark blocked",
		"approve_merge":      "Approve merge",
	}
	if want, ok := wantLabels[action.ID]; ok && action.Label != want {
		t.Fatalf("action %s label = %q, want %q", action.ID, action.Label, want)
	}
	if len(action.Label) > len("Approve merge") {
		t.Fatalf("action %s label = %q, want concise non-wrapping label", action.ID, action.Label)
	}
	if action.Description == "" {
		t.Fatalf("action %+v should describe the operation", action)
	}
	if action.Scope == "" || action.Target == "" {
		t.Fatalf("action %+v should include scope and target metadata", action)
	}
	if !action.Mutating || !action.Disabled {
		t.Fatalf("action %+v should be disabled mutating affordance in read-only mode", action)
	}
	// #567: safe verbs (mark_issue_*) execute directly through the
	// safe-action dispatcher and report safe_direct + requires_approval=false.
	// Approval-gated verbs (restart_worker / stop_worker / approve_merge)
	// keep the manual_approval_required policy. The disabled-in-read-only
	// invariant is identical for both groups.
	switch action.ID {
	case "mark_issue_ready", "mark_issue_blocked":
		if action.ApprovalPolicy != controlApprovalPolicySafe {
			t.Fatalf("safe action %s policy = %q, want %q", action.ID, action.ApprovalPolicy, controlApprovalPolicySafe)
		}
		if action.RequiresApproval {
			t.Fatalf("safe action %s should not require approval", action.ID)
		}
	default:
		if action.ApprovalPolicy != controlApprovalPolicyManual {
			t.Fatalf("approval policy = %q, want %q", action.ApprovalPolicy, controlApprovalPolicyManual)
		}
		if !action.RequiresApproval {
			t.Fatalf("approval-gated action %s should require approval", action.ID)
		}
	}
	if !contains(action.DisabledReason, "Read-only mode") {
		t.Fatalf("disabled reason = %q, want read-only explanation", action.DisabledReason)
	}
	if action.Method != http.MethodPost || action.Endpoint != "/api/v1/fleet/actions" {
		t.Fatalf("action endpoint = %s %s, want POST /api/v1/fleet/actions", action.Method, action.Endpoint)
	}
}

// TestFleetAPISurfacesBackendHealthAndAttribution pins #534: the fleet
// snapshot must echo state.BackendHealth on each project (so the SPA can
// render «claude in cooldown until 21:00 UTC») and must echo the per-
// session Attribution timeline on each worker (so the SPA can render
// «claude opus-4.8 xhigh (12m) → codex gpt-5.5 medium (4m, fallover)»).
func TestFleetAPISurfacesBackendHealthAndAttribution(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "one")
	now := time.Now().UTC()
	cooldownUntil := now.Add(2 * time.Hour)
	segStart := now.Add(-16 * time.Minute)
	segMid := now.Add(-4 * time.Minute)

	st := state.NewState()
	st.Sessions["one-1"] = &state.Session{
		IssueNumber: 87,
		IssueTitle:  "Surface backend health",
		Status:      state.StatusRunning,
		StartedAt:   segStart,
		Backend:     "codex",
		Attribution: []state.BackendAttribution{
			{
				Backend:   "claude",
				Provider:  "anthropic",
				Model:     "opus-4.8",
				Variant:   "opus[1m]",
				Effort:    "xhigh",
				StartedAt: segStart,
				EndedAt:   fleetTimePtr(segMid),
				EndReason: "provider_limit",
				Reason:    "initial_spawn",
			},
			{
				Backend:   "codex",
				Provider:  "openai",
				Model:     "gpt-5.5",
				Effort:    "medium",
				StartedAt: segMid,
				Reason:    "fallover",
			},
		},
	}
	st.BackendHealth["claude"] = state.BackendHealth{
		State:      state.BackendHealthCooldown,
		Reason:     "provider rate limit",
		Pattern:    "anthropic_5h_limit",
		Since:      now.Add(-5 * time.Minute),
		RetryAfter: &cooldownUntil,
	}
	st.BackendHealth["codex"] = state.BackendHealth{State: state.BackendHealthAvailable}
	if err := state.Save(stateDir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	srv := NewFleet([]FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "", &config.Config{
			Repo:        "owner/one",
			StateDir:    stateDir,
			MaxParallel: 1,
		}),
	}, "127.0.0.1", 8786, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp fleetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	project := findFleetProject(t, resp.Projects, "One")
	claudeHealth, ok := project.BackendHealth["claude"]
	if !ok {
		t.Fatalf("project.backend_health missing claude entry: %+v", project.BackendHealth)
	}
	if claudeHealth.State != state.BackendHealthCooldown {
		t.Fatalf("claude state = %q, want cooldown", claudeHealth.State)
	}
	if claudeHealth.RetryAfter == nil || !claudeHealth.RetryAfter.Equal(cooldownUntil) {
		t.Fatalf("claude retry_after = %v, want %v", claudeHealth.RetryAfter, cooldownUntil)
	}
	codexHealth, ok := project.BackendHealth["codex"]
	if !ok || codexHealth.State != state.BackendHealthAvailable {
		t.Fatalf("codex health = %+v, want available", codexHealth)
	}

	worker := findFleetWorker(t, resp.Workers, "one-1")
	if len(worker.Attribution) != 2 {
		t.Fatalf("worker.attribution len = %d, want 2 segments", len(worker.Attribution))
	}
	if worker.Attribution[0].Backend != "claude" || worker.Attribution[0].EndReason != "provider_limit" {
		t.Fatalf("first segment = %+v, want closed claude segment", worker.Attribution[0])
	}
	if worker.Attribution[1].Backend != "codex" || worker.Attribution[1].EndedAt != nil {
		t.Fatalf("second segment = %+v, want open codex segment", worker.Attribution[1])
	}
	if worker.Attribution[1].Reason != "fallover" {
		t.Fatalf("second segment.Reason = %q, want fallover", worker.Attribution[1].Reason)
	}
}

// #600: stale BackendHealth cooldown entries (RetryAfter in the past,
// successful PR-evidence sessions recorded after the cooldown was set)
// must be omitted from the fleet snapshot so the BACKENDS panel reflects
// reality. Mirrors the operator-reported scenario where claude+codex
// were producing merges while the panel reported them as "auto-recovery
// pending".
func TestFleetAPIClearsStaleBackendCooldown(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "one")
	now := time.Now().UTC()

	expiredRetry := now.Add(-48 * time.Hour)
	staleSince := now.Add(-72 * time.Hour)
	successStart := now.Add(-1 * time.Hour)

	st := state.NewState()
	st.Sessions["one-7"] = &state.Session{
		IssueNumber: 600,
		IssueTitle:  "Successful claude session post-cooldown",
		Status:      state.StatusPROpen,
		StartedAt:   successStart,
		Backend:     "claude",
		PRNumber:    597,
	}
	st.BackendHealth["claude"] = state.BackendHealth{
		State:       state.BackendHealthCooldown,
		Reason:      state.BackendBlockProviderLimit,
		Since:       staleSince,
		LastSession: "sup-83",
	}
	st.BackendHealth["codex"] = state.BackendHealth{
		State:      state.BackendHealthCooldown,
		Reason:     state.BackendBlockProviderLimit,
		Since:      staleSince,
		RetryAfter: &expiredRetry,
	}
	if err := state.Save(stateDir, st); err != nil {
		t.Fatalf("save state: %v", err)
	}

	srv := NewFleet([]FleetProject{
		NewFleetProject("One", "/tmp/one.yaml", "", &config.Config{
			Repo:        "owner/one",
			StateDir:    stateDir,
			MaxParallel: 1,
		}),
	}, "127.0.0.1", 8787, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil)
	w := httptest.NewRecorder()
	srv.handleFleet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp fleetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	project := findFleetProject(t, resp.Projects, "One")
	if got, ok := project.BackendHealth["claude"]; ok {
		t.Fatalf("claude cooldown should be cleared after a PR-evidence session, got %+v", got)
	}
	if got, ok := project.BackendHealth["codex"]; ok {
		t.Fatalf("codex cooldown should be cleared after RetryAfter elapses, got %+v", got)
	}
}

func TestLoadFleetProjectsAutoDisambiguatesSameBasename(t *testing.T) {
	// #764: two distinct repos that share a basename and have NO explicit name
	// must auto-disambiguate (api, org-b-api) instead of hard-erroring on a
	// duplicate fleet project name — consistent with `maestro daemon`.
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.yaml")
	bPath := filepath.Join(dir, "b.yaml")
	if err := os.WriteFile(aPath, []byte("repo: org-a/api\nstate_dir: "+filepath.Join(dir, "a")+"\nsession_prefix: api\n"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(bPath, []byte("repo: org-b/api\nstate_dir: "+filepath.Join(dir, "b")+"\nsession_prefix: api\n"), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}
	fleetPath := filepath.Join(dir, "fleet.yaml")
	if err := os.WriteFile(fleetPath, []byte("projects:\n  - config: a.yaml\n  - config: b.yaml\n"), 0o644); err != nil {
		t.Fatalf("write fleet: %v", err)
	}

	projects, err := LoadFleetProjects(fleetPath)
	if err != nil {
		t.Fatalf("LoadFleetProjects failed (want auto-disambiguation, not error): %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("projects len = %d, want 2", len(projects))
	}
	names := map[string]bool{projects[0].Name: true, projects[1].Name: true}
	if !names["api"] || !names["org-b-api"] {
		t.Fatalf("names = %v, want {api, org-b-api}", names)
	}
}

func TestLoadFleetProjectsRejectsExplicitDuplicateName(t *testing.T) {
	// An explicit duplicate name in the fleet file is operator error and must
	// still be rejected (only derived names auto-disambiguate, #764).
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "p.yaml")
	if err := os.WriteFile(cfgPath, []byte("repo: owner/project\nstate_dir: "+filepath.Join(dir, "s")+"\nsession_prefix: prj\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	fleetPath := filepath.Join(dir, "fleet.yaml")
	if err := os.WriteFile(fleetPath, []byte("projects:\n  - name: Dup\n    config: p.yaml\n  - name: Dup\n    config: p.yaml\n"), 0o644); err != nil {
		t.Fatalf("write fleet: %v", err)
	}
	if _, err := LoadFleetProjects(fleetPath); err == nil {
		t.Fatal("LoadFleetProjects: want error on explicit duplicate name, got nil")
	}
}

func boolPtr(b bool) *bool { return &b }

// TestAllBackendsBlockedPartialHealthNotFullyBlocked guards the #814 review
// finding: a health map holding only a cooldown entry for one backend must NOT
// read as fully blocked while other configured backends have no health entry
// yet and could still be dispatched.
func TestAllBackendsBlockedPartialHealthNotFullyBlocked(t *testing.T) {
	cfg := &config.Config{Model: config.ModelConfig{
		Default:          "claude",
		FallbackBackends: []string{"codex"},
		Backends: map[string]config.BackendDef{
			"claude": {},
			"codex":  {},
		},
	}}
	configured := configuredWorkerBackends(cfg)

	// Only claude is in cooldown; codex has no entry -> dispatch can still try
	// codex, so the project is not blocked_by_model_limits.
	health := map[string]state.BackendHealth{
		"claude": {State: state.BackendHealthCooldown},
	}
	if allBackendsBlocked(health, configured) {
		t.Fatal("partial cooldown (codex still available) must not report all backends blocked")
	}

	// Both configured backends blocked -> genuinely blocked.
	health["codex"] = state.BackendHealth{State: state.BackendHealthCooldown}
	if !allBackendsBlocked(health, configured) {
		t.Fatal("every configured backend in cooldown must report all backends blocked")
	}

	// An explicit available entry counts as an escape hatch.
	health["codex"] = state.BackendHealth{State: state.BackendHealthAvailable}
	if allBackendsBlocked(health, configured) {
		t.Fatal("an available backend must not report all backends blocked")
	}
}

// TestConfiguredWorkerBackendsOmitsDisabled confirms a disabled backend is not
// counted as a dispatchable escape hatch, so its absence from the blocked set
// cannot keep blocked_by_model_limits from firing when every enabled backend
// is down.
func TestConfiguredWorkerBackendsOmitsDisabled(t *testing.T) {
	cfg := &config.Config{Model: config.ModelConfig{
		Default:          "claude",
		FallbackBackends: []string{"codex"},
		Backends: map[string]config.BackendDef{
			"claude": {},
			"codex":  {Enabled: boolPtr(false)},
		},
	}}
	configured := configuredWorkerBackends(cfg)
	if len(configured) != 1 || configured[0] != "claude" {
		t.Fatalf("configured = %v, want [claude] (codex disabled)", configured)
	}
	// claude down + codex disabled -> fully blocked (codex is not an escape).
	health := map[string]state.BackendHealth{
		"claude": {State: state.BackendBlockUsageLimit},
	}
	if !allBackendsBlocked(health, configured) {
		t.Fatal("disabled codex must not prevent blocked_by_model_limits when claude is down")
	}
}

func TestConfiguredWorkerBackendsIgnoresBackendsOutsideResolvedRoute(t *testing.T) {
	cfg := &config.Config{Model: config.ModelConfig{
		Default: "claude",
		ProviderLanes: []config.ProviderLane{
			{Provider: "anthropic", Default: "claude"},
		},
		Backends: map[string]config.BackendDef{
			"claude": {Provider: "anthropic"},
			"helper": {Provider: "openai"},
		},
	}}
	configured := configuredWorkerBackends(cfg)
	if !reflect.DeepEqual(configured, []string{"claude"}) {
		t.Fatalf("configured = %v, want resolved route only", configured)
	}
	health := map[string]state.BackendHealth{
		"claude": {State: state.BackendHealthCooldown},
	}
	if !allBackendsBlocked(health, configured) {
		t.Fatal("unrouted helper backend must not hide a fully blocked route")
	}
}

// TestAllBackendsBlockedNoConfiguredFallsBackToHealthMap keeps the legacy
// health-map-only behavior when the configured set is unknown.
func TestAllBackendsBlockedNoConfiguredFallsBackToHealthMap(t *testing.T) {
	if allBackendsBlocked(nil, nil) {
		t.Fatal("empty health + no configured set must not report blocked")
	}
	blocked := map[string]state.BackendHealth{"claude": {State: state.BackendHealthCooldown}}
	if !allBackendsBlocked(blocked, nil) {
		t.Fatal("all recorded entries blocked + no configured set must report blocked")
	}
}
