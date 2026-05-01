package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"gopkg.in/yaml.v3"
)

// FleetProject describes one Maestro project exposed in the fleet dashboard.
type FleetProject struct {
	Name         string `json:"name" yaml:"name"`
	ConfigPath   string `json:"config_path" yaml:"config"`
	DashboardURL string `json:"dashboard_url,omitempty" yaml:"dashboard_url"`

	cfg *config.Config
}

// NewFleetProject wraps an already-loaded config for in-process fleet serving.
func NewFleetProject(name, configPath, dashboardURL string, cfg *config.Config) FleetProject {
	if strings.TrimSpace(name) == "" && cfg != nil {
		name = defaultFleetProjectName(cfg.Repo)
	}
	return FleetProject{
		Name:         strings.TrimSpace(name),
		ConfigPath:   strings.TrimSpace(configPath),
		DashboardURL: strings.TrimSpace(dashboardURL),
		cfg:          cfg,
	}
}

// FleetFile is the YAML shape accepted by maestro serve --fleet.
type FleetFile struct {
	Projects []FleetProject `yaml:"projects"`
}

// LoadFleetProjects loads a fleet YAML file and resolves every project config.
func LoadFleetProjects(path string) ([]FleetProject, error) {
	path = expandFleetPath(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fleet file %s: %w", path, err)
	}
	var file FleetFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse fleet file %s: %w", path, err)
	}
	if len(file.Projects) == 0 {
		return nil, fmt.Errorf("fleet file %s has no projects", path)
	}

	baseDir := filepath.Dir(path)
	seen := make(map[string]struct{}, len(file.Projects))
	projects := make([]FleetProject, 0, len(file.Projects))
	for i, project := range file.Projects {
		configPath := expandFleetPath(project.ConfigPath)
		if configPath == "" {
			return nil, fmt.Errorf("fleet project %d: config is required", i+1)
		}
		if !filepath.IsAbs(configPath) {
			configPath = filepath.Join(baseDir, configPath)
		}
		project.ConfigPath = configPath
		cfg, err := config.LoadFrom(project.ConfigPath)
		if err != nil {
			return nil, fmt.Errorf("fleet project %d config %s: %w", i+1, project.ConfigPath, err)
		}
		project.cfg = cfg
		if strings.TrimSpace(project.Name) == "" {
			project.Name = defaultFleetProjectName(cfg.Repo)
		}
		project.Name = strings.TrimSpace(project.Name)
		key := strings.ToLower(project.Name)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("duplicate fleet project name %q", project.Name)
		}
		seen[key] = struct{}{}
		projects = append(projects, project)
	}
	return projects, nil
}

func expandFleetPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func defaultFleetProjectName(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "project"
	}
	parts := strings.Split(repo, "/")
	return parts[len(parts)-1]
}

// FleetServer exposes a read-only dashboard/API across multiple Maestro configs.
type FleetServer struct {
	projects []FleetProject
	host     string
	port     int
	readOnly bool
	srv      *http.Server
}

// NewFleet creates a FleetServer.
func NewFleet(projects []FleetProject, host string, port int, readOnly bool) *FleetServer {
	return &FleetServer{
		projects: projects,
		host:     host,
		port:     port,
		readOnly: readOnly,
	}
}

// Start begins serving the fleet dashboard. It blocks until shutdown.
func (s *FleetServer) Start(ctx context.Context) error {
	if s.port == 0 {
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/fleet", s.handleFleet)
	mux.HandleFunc("/", s.handleFleetDashboard)

	host := strings.TrimSpace(s.host)
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(s.port))
	s.srv = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.srv.Shutdown(shutdownCtx)
	}()

	log.Printf("[fleet] listening on %s", addr)
	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("fleet server: %w", err)
	}
	return nil
}

type fleetResponse struct {
	ReadOnly bool                `json:"read_only"`
	Projects []fleetProjectState `json:"projects"`
	Summary  fleetSummary        `json:"summary"`
	Workers  []fleetWorkerState  `json:"workers"`
}

type fleetSummary struct {
	Projects       int `json:"projects"`
	Running        int `json:"running"`
	PROpen         int `json:"pr_open"`
	Failed         int `json:"failed"`
	Sessions       int `json:"sessions"`
	NeedsAttention int `json:"needs_attention"`
}

type fleetProjectState struct {
	Name           string         `json:"name"`
	Repo           string         `json:"repo"`
	ConfigPath     string         `json:"config_path"`
	DashboardURL   string         `json:"dashboard_url,omitempty"`
	StateDir       string         `json:"state_dir,omitempty"`
	MaxParallel    int            `json:"max_parallel"`
	ReadOnly       bool           `json:"read_only"`
	Summary        map[string]int `json:"summary"`
	Running        int            `json:"running"`
	PROpen         int            `json:"pr_open"`
	Failed         int            `json:"failed"`
	Sessions       int            `json:"sessions"`
	NeedsAttention int            `json:"needs_attention"`
	Active         []sessionInfo  `json:"active,omitempty"`
	Supervisor     supervisorInfo `json:"supervisor"`
	Error          string         `json:"error,omitempty"`
}

type fleetWorkerState struct {
	ProjectName       string `json:"project_name"`
	ProjectRepo       string `json:"project_repo,omitempty"`
	DashboardURL      string `json:"dashboard_url,omitempty"`
	Slot              string `json:"slot"`
	IssueNumber       int    `json:"issue_number"`
	IssueTitle        string `json:"issue_title"`
	IssueURL          string `json:"issue_url,omitempty"`
	Status            string `json:"status"`
	StatusReason      string `json:"status_reason,omitempty"`
	NeedsAttention    bool   `json:"needs_attention,omitempty"`
	Backend           string `json:"backend,omitempty"`
	PRNumber          int    `json:"pr_number,omitempty"`
	PRURL             string `json:"pr_url,omitempty"`
	TokensUsedAttempt int    `json:"tokens_used_attempt"`
	TokensUsedTotal   int    `json:"tokens_used_total"`
	Runtime           string `json:"runtime"`
	StartedAt         string `json:"started_at"`
	Alive             *bool  `json:"alive,omitempty"`
}

func (s *FleetServer) handleFleet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.snapshot())
}

func (s *FleetServer) snapshot() fleetResponse {
	resp := fleetResponse{
		ReadOnly: s.readOnly,
		Projects: make([]fleetProjectState, 0, len(s.projects)),
		Workers:  make([]fleetWorkerState, 0),
	}
	for _, project := range s.projects {
		item, workers := s.projectSnapshot(project)
		resp.Projects = append(resp.Projects, item)
		resp.Workers = append(resp.Workers, workers...)
		resp.Summary.Projects++
		resp.Summary.Running += item.Running
		resp.Summary.PROpen += item.PROpen
		resp.Summary.Failed += item.Failed
		resp.Summary.Sessions += item.Sessions
		resp.Summary.NeedsAttention += item.NeedsAttention
	}
	sort.Slice(resp.Projects, func(i, j int) bool {
		if resp.Projects[i].Running != resp.Projects[j].Running {
			return resp.Projects[i].Running > resp.Projects[j].Running
		}
		return resp.Projects[i].Name < resp.Projects[j].Name
	})
	sort.SliceStable(resp.Workers, func(i, j int) bool {
		left, right := resp.Workers[i], resp.Workers[j]
		if left.NeedsAttention != right.NeedsAttention {
			return left.NeedsAttention
		}
		li := state.StatusPriority(state.SessionStatus(left.Status))
		ri := state.StatusPriority(state.SessionStatus(right.Status))
		if li != ri {
			return li < ri
		}
		if left.StartedAt != right.StartedAt {
			return left.StartedAt > right.StartedAt
		}
		if left.ProjectName != right.ProjectName {
			return left.ProjectName < right.ProjectName
		}
		return left.Slot < right.Slot
	})
	return resp
}

func (s *FleetServer) projectSnapshot(project FleetProject) (fleetProjectState, []fleetWorkerState) {
	cfg := project.cfg
	item := fleetProjectState{
		Name:         project.Name,
		ConfigPath:   project.ConfigPath,
		DashboardURL: project.DashboardURL,
	}
	if cfg == nil {
		item.Error = "missing resolved project config"
		return item, nil
	}
	item.Repo = cfg.Repo
	item.StateDir = cfg.StateDir
	item.MaxParallel = cfg.MaxParallel
	item.ReadOnly = cfg.Server.ReadOnly || s.readOnly

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		item.Error = err.Error()
		return item, nil
	}
	projectState := buildStateResponse(cfg, st)
	item.Summary = projectState.Summary
	item.Running = len(projectState.Running)
	item.PROpen = len(projectState.PROpen)
	item.Failed = failedCount(projectState.Summary)
	item.Sessions = len(projectState.All)
	item.Supervisor = projectState.Supervisor
	workers := make([]fleetWorkerState, 0)
	for _, worker := range projectState.All {
		if worker.NeedsAttention {
			item.NeedsAttention++
		}
		if isFleetWorkerVisible(worker) {
			workers = append(workers, makeFleetWorkerState(item, worker))
			if len(item.Active) >= 6 {
				continue
			}
			item.Active = append(item.Active, worker)
		}
	}
	return item, workers
}

func isFleetWorkerVisible(worker sessionInfo) bool {
	return worker.Status == string(state.StatusRunning) || worker.Status == string(state.StatusPROpen) || worker.NeedsAttention
}

func makeFleetWorkerState(project fleetProjectState, worker sessionInfo) fleetWorkerState {
	return fleetWorkerState{
		ProjectName:       project.Name,
		ProjectRepo:       project.Repo,
		DashboardURL:      project.DashboardURL,
		Slot:              worker.Slot,
		IssueNumber:       worker.IssueNumber,
		IssueTitle:        worker.IssueTitle,
		IssueURL:          worker.IssueURL,
		Status:            worker.Status,
		StatusReason:      worker.StatusReason,
		NeedsAttention:    worker.NeedsAttention,
		Backend:           worker.Backend,
		PRNumber:          worker.PRNumber,
		PRURL:             worker.PRURL,
		TokensUsedAttempt: worker.TokensUsedAttempt,
		TokensUsedTotal:   worker.TokensUsedTotal,
		Runtime:           worker.Runtime,
		StartedAt:         worker.StartedAt,
		Alive:             worker.Alive,
	}
}

func failedCount(summary map[string]int) int {
	return summary[string(state.StatusDead)] +
		summary[string(state.StatusFailed)] +
		summary[string(state.StatusRetryExhausted)] +
		summary[string(state.StatusConflictFailed)]
}

func (s *FleetServer) handleFleetDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, fleetDashboardHTML)
}

const fleetDashboardHTML = `<!DOCTYPE html>
<html>
<head>
<title>maestro fleet</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  :root {
    color-scheme: dark;
    --bg: #0d1117;
    --panel: #151b23;
    --panel-2: #10161d;
    --line: #29313d;
    --text: #e6edf3;
    --muted: #8b949e;
    --accent: #58a6ff;
    --ok: #3fb950;
    --warn: #d29922;
    --bad: #f85149;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    background: var(--bg);
    color: var(--text);
    font: 14px/1.45 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
  }
  header {
    min-height: 64px;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 18px;
    padding: 12px 20px;
    border-bottom: 1px solid var(--line);
    background: #0b1016;
  }
  h1 { margin: 0; font-size: 19px; letter-spacing: 0; }
  .sub { color: var(--muted); font-size: 13px; }
  .stats { display: flex; gap: 18px; flex-wrap: wrap; justify-content: flex-end; }
  .stat { text-align: right; min-width: 64px; }
  .stat strong { display: block; font-size: 18px; }
  .stat span { color: var(--muted); font-size: 12px; }
  main { padding: 18px; }
  .project-tabs {
    display: flex;
    gap: 8px;
    margin-bottom: 14px;
    padding-bottom: 4px;
    overflow-x: auto;
  }
  .project-tab {
    flex: 0 0 auto;
    border: 1px solid var(--line);
    border-radius: 999px;
    background: var(--panel-2);
    color: var(--muted);
    padding: 6px 11px;
    cursor: pointer;
    font: inherit;
    white-space: nowrap;
  }
  .project-tab.active {
    color: var(--text);
    border-color: rgba(88,166,255,.65);
    background: rgba(88,166,255,.12);
  }
  .project-tab .count { margin-left: 6px; color: var(--muted); font-size: 12px; }
  .fleet-workers {
    margin-bottom: 16px;
    border: 1px solid var(--line);
    background: var(--panel);
  }
  .section-head {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 14px;
    padding: 14px;
    border-bottom: 1px solid var(--line);
  }
  .section-head h2 { margin: 0; font-size: 17px; }
  .section-note { color: var(--muted); font-size: 13px; text-align: right; }
  .table-scroll { overflow-x: auto; }
  .worker-table {
    width: 100%;
    min-width: 920px;
    border-collapse: collapse;
    table-layout: fixed;
  }
  .worker-table th, .worker-table td {
    padding: 9px 10px;
    border-bottom: 1px solid rgba(41,49,61,.8);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    vertical-align: middle;
  }
  .worker-table th {
    color: var(--muted);
    font-size: 12px;
    font-weight: 650;
    text-align: left;
    background: var(--panel-2);
  }
  .worker-table tbody tr.row-running { background: rgba(63,185,80,.055); }
  .worker-table tbody tr.row-pr { background: rgba(88,166,255,.055); }
  .worker-table tbody tr.row-attention { background: rgba(248,81,73,.1); }
  .worker-table tbody tr:hover { background: #18212c; }
  .project-col { width: 140px; font-weight: 650; }
  .slot-col { width: 92px; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
  .issue-col { width: auto; }
  .status-col { width: 132px; }
  .backend-col { width: 108px; }
  .pr-col { width: 70px; }
  .runtime-col { width: 90px; }
  .tokens-col { width: 82px; text-align: right; }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(360px, 1fr));
    gap: 14px;
  }
  .project {
    border: 1px solid var(--line);
    background: var(--panel);
    min-height: 260px;
  }
  .project-head {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 14px;
    padding: 14px 14px 10px;
    border-bottom: 1px solid var(--line);
  }
  .project h2 { margin: 0; font-size: 17px; }
  .repo { color: var(--muted); margin-top: 2px; font-size: 13px; }
  .links { display: flex; gap: 10px; white-space: nowrap; font-size: 13px; }
  a { color: var(--accent); text-decoration: none; }
  a:hover { text-decoration: underline; }
  .metric-row {
    display: grid;
    grid-template-columns: repeat(5, 1fr);
    border-bottom: 1px solid var(--line);
  }
  .metric {
    padding: 10px 8px;
    border-right: 1px solid var(--line);
    text-align: center;
  }
  .metric:last-child { border-right: 0; }
  .metric strong { display: block; font-size: 16px; }
  .metric span { display: block; color: var(--muted); font-size: 11px; }
  .supervisor, .workers { padding: 12px 14px; border-bottom: 1px solid var(--line); }
  .label { color: var(--muted); font-weight: 650; text-transform: uppercase; font-size: 12px; }
  .decision { margin-top: 5px; color: var(--text); }
  .decision small { color: var(--muted); }
  .project table { width: 100%; border-collapse: collapse; margin-top: 8px; table-layout: fixed; }
  .project td {
    padding: 7px 0;
    border-top: 1px solid rgba(41,49,61,.7);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .project td:nth-child(1) { width: 78px; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
  .project td:nth-child(2) { width: 74px; }
  .project td:nth-child(4) { width: 70px; text-align: right; color: var(--muted); }
  .pill {
    display: inline-block;
    padding: 1px 8px;
    border: 1px solid var(--line);
    border-radius: 999px;
    font-size: 12px;
  }
  .s-running { color: var(--ok); border-color: rgba(63,185,80,.45); }
  .s-pr_open { color: var(--accent); border-color: rgba(88,166,255,.45); }
  .s-dead, .s-failed, .s-conflict_failed, .s-retry_exhausted { color: var(--bad); border-color: rgba(248,81,73,.45); }
  .attention { color: var(--bad); border-color: rgba(248,81,73,.45); }
  .empty { color: var(--muted); margin-top: 8px; }
  .worker-table .empty { padding: 18px 14px; margin: 0; text-align: center; }
  .error { color: var(--bad); padding: 12px 14px; }
  @media (max-width: 700px) {
    header { align-items: flex-start; flex-direction: column; }
    .stats { justify-content: flex-start; }
    main { padding: 10px; }
    .section-head { flex-direction: column; }
    .section-note { text-align: left; }
    .grid { grid-template-columns: 1fr; }
    .metric-row { grid-template-columns: repeat(2, 1fr); }
  }
</style>
</head>
<body>
<header>
  <div>
    <h1>Maestro Fleet</h1>
    <div class="sub" id="subtitle">Loading projects...</div>
  </div>
  <div class="stats" id="stats"></div>
</header>
<main>
  <nav class="project-tabs" id="project-tabs" aria-label="Fleet projects"></nav>
  <section class="fleet-workers">
    <div class="section-head">
      <div>
        <h2>Fleet Workers</h2>
        <div class="sub">Unified active and attention queue across projects.</div>
      </div>
      <div class="section-note" id="worker-summary">Loading workers...</div>
    </div>
    <div class="table-scroll">
      <table class="worker-table">
        <thead>
          <tr>
            <th class="project-col">Project</th>
            <th class="slot-col">Slot</th>
            <th class="issue-col">Issue</th>
            <th class="status-col">Status</th>
            <th class="backend-col">Backend</th>
            <th class="pr-col">PR</th>
            <th class="runtime-col">Runtime</th>
            <th class="tokens-col">Tokens</th>
          </tr>
        </thead>
        <tbody id="fleet-workers-body"></tbody>
      </table>
    </div>
  </section>
  <div class="grid" id="projects"></div>
</main>
<script>
const projectsEl = document.getElementById("projects");
const statsEl = document.getElementById("stats");
const subtitleEl = document.getElementById("subtitle");
const tabsEl = document.getElementById("project-tabs");
const fleetWorkersEl = document.getElementById("fleet-workers-body");
const workerSummaryEl = document.getElementById("worker-summary");

const fleetState = {
  selectedProject: "all",
  projects: [],
  workers: []
};

function escapeText(value) {
  return String(value ?? "").replace(/[&<>"']/g, ch => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", "\"": "&quot;", "'": "&#39;"
  }[ch]));
}

function compactNumber(value) {
  const n = Number(value || 0);
  if (!n) return "-";
  if (n < 1000) return String(n);
  if (n < 1000000) return (n / 1000).toFixed(n < 10000 ? 1 : 0).replace(/\.0$/, "") + "k";
  return (n / 1000000).toFixed(1).replace(/\.0$/, "") + "M";
}

function linkHTML(url, label) {
  if (!url) return escapeText(label);
  return '<a href="' + escapeText(url) + '" target="_blank" rel="noreferrer">' + escapeText(label) + '</a>';
}

function statusLabel(worker) {
  if (worker.status === "running" && worker.alive === false) return "running stale";
  return worker.status || "-";
}

function statusClass(worker) {
  let cls = "pill s-" + escapeText(worker.status || "unknown");
  if (worker.needs_attention || (worker.status === "running" && worker.alive === false)) cls += " attention";
  return cls;
}

const statusRank = {
  running: 0,
  pr_open: 1,
  queued: 2,
  dead: 3,
  failed: 4,
  conflict_failed: 5,
  retry_exhausted: 6,
  done: 7
};

function sortWorkers(workers) {
  return [...workers].sort((a, b) => {
    if (Boolean(a.needs_attention) !== Boolean(b.needs_attention)) return a.needs_attention ? -1 : 1;
    const ar = statusRank[a.status] ?? 99;
    const br = statusRank[b.status] ?? 99;
    if (ar !== br) return ar - br;
    const time = String(b.started_at || "").localeCompare(String(a.started_at || ""));
    if (time !== 0) return time;
    const project = String(a.project_name || "").localeCompare(String(b.project_name || ""));
    if (project !== 0) return project;
    return String(a.slot || "").localeCompare(String(b.slot || ""));
  });
}

function rowClass(worker) {
  if (worker.needs_attention || (worker.status === "running" && worker.alive === false)) return "row-attention";
  if (worker.status === "running") return "row-running";
  if (worker.status === "pr_open") return "row-pr";
  return "";
}

function fleetWorkersFromData(data) {
  if (Array.isArray(data.workers)) return data.workers;
  return (data.projects || []).flatMap(project => (project.active || []).map(worker => ({
    ...worker,
    project_name: project.name,
    project_repo: project.repo,
    dashboard_url: project.dashboard_url
  })));
}

function countFailed(project) {
  return project.failed || 0;
}

function renderStats(summary) {
  const items = [
    ["Projects", summary.projects || 0],
    ["Running", summary.running || 0],
    ["PR open", summary.pr_open || 0],
    ["Failed", summary.failed || 0],
    ["Attention", summary.needs_attention || 0]
  ];
  statsEl.innerHTML = items.map(([label, value]) =>
    '<div class="stat"><strong>' + escapeText(value) + '</strong><span>' + escapeText(label) + '</span></div>'
  ).join("");
}

function renderProjectTabs() {
  const projectNames = new Set((fleetState.projects || []).map(project => project.name));
  if (fleetState.selectedProject !== "all" && !projectNames.has(fleetState.selectedProject)) {
    fleetState.selectedProject = "all";
  }

  const counts = new Map();
  for (const worker of fleetState.workers || []) {
    const name = worker.project_name || "";
    counts.set(name, (counts.get(name) || 0) + 1);
  }

  const tabs = [{ name: "all", label: "All projects", count: (fleetState.workers || []).length }].concat(
    (fleetState.projects || []).map(project => ({
      name: project.name,
      label: project.name,
      count: counts.get(project.name) || 0
    }))
  );

  tabsEl.innerHTML = tabs.map(tab => {
    const active = tab.name === fleetState.selectedProject ? " active" : "";
    return '<button type="button" class="project-tab' + active + '" data-project="' + escapeText(tab.name) + '">' +
      escapeText(tab.label) + '<span class="count">' + escapeText(tab.count) + '</span></button>';
  }).join("");

  tabsEl.querySelectorAll("button[data-project]").forEach(button => {
    button.addEventListener("click", () => {
      fleetState.selectedProject = button.dataset.project || "all";
      renderProjectTabs();
      renderFleetWorkers();
    });
  });
}

function renderFleetWorkers() {
  const selected = fleetState.selectedProject;
  const workers = selected === "all"
    ? (fleetState.workers || [])
    : (fleetState.workers || []).filter(worker => worker.project_name === selected);
  const visible = sortWorkers(workers);
  const projectLabel = selected === "all" ? "all projects" : selected;
  workerSummaryEl.textContent = visible.length + " active / attention worker" + (visible.length === 1 ? "" : "s") + " in " + projectLabel;

  if (visible.length === 0) {
    const empty = selected === "all"
      ? "No active workers or attention states across configured projects."
      : "No active workers or attention states for " + selected + ".";
    fleetWorkersEl.innerHTML = '<tr><td colspan="8" class="empty">' + escapeText(empty) + '</td></tr>';
    return;
  }

  fleetWorkersEl.innerHTML = visible.map(worker => {
    const issue = worker.issue_number ? "#" + worker.issue_number : "-";
    const pr = worker.pr_number ? "#" + worker.pr_number : "-";
    return '<tr class="' + rowClass(worker) + '">' +
      '<td class="project-col">' + linkHTML(worker.dashboard_url, worker.project_name || "-") + '</td>' +
      '<td class="slot-col">' + escapeText(worker.slot || "-") + '</td>' +
      '<td class="issue-col">' + linkHTML(worker.issue_url, issue) + ' ' + escapeText(worker.issue_title || "") + '</td>' +
      '<td class="status-col"><span class="' + statusClass(worker) + '">' + escapeText(statusLabel(worker)) + '</span></td>' +
      '<td class="backend-col">' + escapeText(worker.backend || "-") + '</td>' +
      '<td class="pr-col">' + linkHTML(worker.pr_url, pr) + '</td>' +
      '<td class="runtime-col">' + escapeText(worker.runtime || "-") + '</td>' +
      '<td class="tokens-col">' + compactNumber(worker.tokens_used_total) + '</td>' +
    '</tr>';
  }).join("");
}

function renderSupervisor(project) {
  const sup = project.supervisor;
  if (!sup || !sup.has_run || !sup.latest) {
    return '<div class="supervisor"><div class="label">Supervisor</div><div class="empty">No supervisor decision yet.</div></div>';
  }
  const latest = sup.latest;
  const reasons = (latest.stuck_reasons && latest.stuck_reasons.length ? latest.stuck_reasons : latest.reasons || []).slice(0, 2);
  return '<div class="supervisor">' +
    '<div class="label">Supervisor</div>' +
    '<div class="decision"><strong>' + escapeText(latest.recommended_action || "none") + '</strong> · ' +
    escapeText(latest.summary || "") + '<br><small>Risk ' + escapeText(latest.risk || "-") +
    (latest.confidence ? " · Confidence " + Number(latest.confidence).toFixed(2) : "") + '</small></div>' +
    (reasons.length ? '<div class="empty">' + reasons.map(escapeText).join("<br>") + '</div>' : "") +
  '</div>';
}

function renderWorkers(project) {
  const workers = project.active || [];
  if (!workers.length) {
    return '<div class="workers"><div class="label">Active / attention</div><div class="empty">No active workers or attention states.</div></div>';
  }
  return '<div class="workers"><div class="label">Active / attention</div><table>' +
    workers.map(worker => '<tr>' +
      '<td>' + escapeText(worker.slot) + '</td>' +
      '<td><span class="' + statusClass(worker) + '">' + escapeText(statusLabel(worker)) + '</span></td>' +
      '<td>' + linkHTML(worker.issue_url, "#" + worker.issue_number) + ' ' + escapeText(worker.issue_title || "") + '</td>' +
      '<td>' + escapeText(worker.runtime || "-") + '</td>' +
    '</tr>').join("") +
  '</table></div>';
}

function renderProject(project) {
  if (project.error) {
    return '<article class="project"><div class="project-head"><div><h2>' + escapeText(project.name) +
      '</h2><div class="repo">' + escapeText(project.config_path || "") + '</div></div></div>' +
      '<div class="error">State error: ' + escapeText(project.error) + '</div></article>';
  }
  const failed = countFailed(project);
  return '<article class="project">' +
    '<div class="project-head"><div><h2>' + escapeText(project.name) + '</h2><div class="repo">' +
    escapeText(project.repo || "") + '</div></div><div class="links">' +
    linkHTML(project.dashboard_url, "Dashboard") + " " + linkHTML(project.repo ? "https://github.com/" + project.repo : "", "GitHub") +
    '</div></div>' +
    '<div class="metric-row">' +
      '<div class="metric"><strong>' + escapeText(project.running || 0) + " / " + escapeText(project.max_parallel || 0) + '</strong><span>Running</span></div>' +
      '<div class="metric"><strong>' + escapeText(project.pr_open || 0) + '</strong><span>PR open</span></div>' +
      '<div class="metric"><strong>' + escapeText(failed) + '</strong><span>Failed</span></div>' +
      '<div class="metric"><strong>' + escapeText(project.sessions || 0) + '</strong><span>Sessions</span></div>' +
      '<div class="metric"><strong>' + escapeText(project.needs_attention || 0) + '</strong><span>Attention</span></div>' +
    '</div>' +
    renderSupervisor(project) +
    renderWorkers(project) +
  '</article>';
}

async function loadFleet() {
  try {
    const response = await fetch("/api/v1/fleet", { cache: "no-store" });
    if (!response.ok) throw new Error(await response.text());
    const data = await response.json();
    fleetState.projects = data.projects || [];
    fleetState.workers = fleetWorkersFromData(data);
    subtitleEl.textContent = fleetState.projects.length + " configured project" + (fleetState.projects.length === 1 ? "" : "s");
    renderStats(data.summary || {});
    renderProjectTabs();
    renderFleetWorkers();
    projectsEl.innerHTML = fleetState.projects.map(renderProject).join("");
  } catch (err) {
    subtitleEl.textContent = "Fleet API error";
    workerSummaryEl.textContent = "Fleet API error";
    fleetWorkersEl.innerHTML = '<tr><td colspan="8" class="empty">Unable to load fleet workers.</td></tr>';
    projectsEl.innerHTML = '<div class="error">' + escapeText(err.message) + '</div>';
  }
}

loadFleet();
setInterval(loadFleet, 3000);
</script>
</body>
</html>`
