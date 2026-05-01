package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

const fleetProjectStaleAfter = 15 * time.Minute

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
	mux.HandleFunc("/api/v1/fleet/worker", s.handleFleetWorker)
	mux.HandleFunc("/api/v1/fleet", s.handleFleet)
	mux.HandleFunc("/api/v1/fleet/actions", s.handleFleetAction)
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
	ReadOnly    bool                 `json:"read_only"`
	RefreshedAt string               `json:"refreshed_at"`
	Projects    []fleetProjectState  `json:"projects"`
	Summary     fleetSummary         `json:"summary"`
	Workers     []fleetWorkerState   `json:"workers"`
	Attention   []fleetWorkerState   `json:"attention"`
	Approvals   []fleetApprovalState `json:"approvals,omitempty"`
}

type fleetSummary struct {
	Projects          int `json:"projects"`
	Stale             int `json:"stale"`
	Errors            int `json:"errors"`
	Running           int `json:"running"`
	PROpen            int `json:"pr_open"`
	Failed            int `json:"failed"`
	Sessions          int `json:"sessions"`
	NeedsAttention    int `json:"needs_attention"`
	Approvals         int `json:"approvals"`
	ApprovalsPending  int `json:"approvals_pending"`
	ApprovalsStale    int `json:"approvals_stale"`
	ApprovalsApproved int `json:"approvals_approved"`
	ApprovalsRejected int `json:"approvals_rejected"`
}

type fleetProjectFreshness struct {
	StateUpdatedAt     string `json:"state_updated_at,omitempty"`
	LogUpdatedAt       string `json:"log_updated_at,omitempty"`
	SnapshotAt         string `json:"snapshot_at,omitempty"`
	SnapshotAge        string `json:"snapshot_age,omitempty"`
	SnapshotAgeSeconds int64  `json:"snapshot_age_seconds,omitempty"`
	Stale              bool   `json:"stale,omitempty"`
	Reason             string `json:"reason,omitempty"`
	StaleAfterSeconds  int64  `json:"stale_after_seconds"`
}

type fleetProjectState struct {
	Name            string                `json:"name"`
	Repo            string                `json:"repo"`
	ConfigPath      string                `json:"config_path"`
	DashboardURL    string                `json:"dashboard_url,omitempty"`
	StateDir        string                `json:"state_dir,omitempty"`
	MaxParallel     int                   `json:"max_parallel"`
	ReadOnly        bool                  `json:"read_only"`
	Summary         map[string]int        `json:"summary"`
	Running         int                   `json:"running"`
	PROpen          int                   `json:"pr_open"`
	Failed          int                   `json:"failed"`
	Sessions        int                   `json:"sessions"`
	NeedsAttention  int                   `json:"needs_attention"`
	Active          []sessionInfo         `json:"active,omitempty"`
	Attention       []sessionInfo         `json:"attention,omitempty"`
	Approvals       []fleetApprovalState  `json:"approvals,omitempty"`
	ApprovalSummary map[string]int        `json:"approval_summary,omitempty"`
	Actions         []controlAction       `json:"actions,omitempty"`
	Supervisor      supervisorInfo        `json:"supervisor"`
	Freshness       fleetProjectFreshness `json:"freshness"`
	Error           string                `json:"error,omitempty"`
}

type fleetApprovalState struct {
	ProjectName       string                  `json:"project_name"`
	ProjectRepo       string                  `json:"project_repo,omitempty"`
	DashboardURL      string                  `json:"dashboard_url,omitempty"`
	ID                string                  `json:"id"`
	DecisionID        string                  `json:"decision_id,omitempty"`
	Action            string                  `json:"action"`
	Target            *state.SupervisorTarget `json:"target,omitempty"`
	TargetLinks       []targetLinkInfo        `json:"target_links,omitempty"`
	IssueNumber       int                     `json:"issue_number,omitempty"`
	IssueURL          string                  `json:"issue_url,omitempty"`
	PRNumber          int                     `json:"pr_number,omitempty"`
	PRURL             string                  `json:"pr_url,omitempty"`
	Session           string                  `json:"session,omitempty"`
	SessionStatus     string                  `json:"session_status,omitempty"`
	Status            string                  `json:"status"`
	CreatedAt         string                  `json:"created_at,omitempty"`
	UpdatedAt         string                  `json:"updated_at,omitempty"`
	CreatedAge        string                  `json:"created_age,omitempty"`
	UpdatedAge        string                  `json:"updated_age,omitempty"`
	CreatedAgeSeconds int64                   `json:"created_age_seconds,omitempty"`
	UpdatedAgeSeconds int64                   `json:"updated_age_seconds,omitempty"`
	Risk              string                  `json:"risk,omitempty"`
	Summary           string                  `json:"summary,omitempty"`

	createdAt time.Time
	updatedAt time.Time
}

type fleetWorkerState struct {
	ProjectName       string          `json:"project_name"`
	ProjectRepo       string          `json:"project_repo,omitempty"`
	DashboardURL      string          `json:"dashboard_url,omitempty"`
	Slot              string          `json:"slot"`
	IssueNumber       int             `json:"issue_number"`
	IssueTitle        string          `json:"issue_title"`
	IssueURL          string          `json:"issue_url,omitempty"`
	Status            string          `json:"status"`
	StatusReason      string          `json:"status_reason,omitempty"`
	NextAction        string          `json:"next_action,omitempty"`
	NeedsAttention    bool            `json:"needs_attention,omitempty"`
	Backend           string          `json:"backend,omitempty"`
	PRNumber          int             `json:"pr_number,omitempty"`
	PRURL             string          `json:"pr_url,omitempty"`
	TokensUsedAttempt int             `json:"tokens_used_attempt"`
	TokensUsedTotal   int             `json:"tokens_used_total"`
	Runtime           string          `json:"runtime"`
	RuntimeSeconds    int64           `json:"runtime_seconds"`
	StartedAt         string          `json:"started_at"`
	FinishedAt        string          `json:"finished_at,omitempty"`
	NextRetryAt       string          `json:"next_retry_at,omitempty"`
	PID               int             `json:"pid,omitempty"`
	Alive             *bool           `json:"alive,omitempty"`
	Worktree          string          `json:"worktree,omitempty"`
	Branch            string          `json:"branch,omitempty"`
	TmuxSession       string          `json:"tmux_session,omitempty"`
	HasLog            bool            `json:"has_log"`
	RetryCount        int             `json:"retry_count,omitempty"`
	LastNotification  string          `json:"last_notification,omitempty"`
	Actions           []controlAction `json:"actions,omitempty"`
}

type fleetWorkerDetailResponse struct {
	Worker fleetWorkerState `json:"worker"`
	Log    fleetLogTail     `json:"log"`
}

type fleetLogTail struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Lines     int    `json:"lines"`
	Truncated bool   `json:"truncated"`
	Text      string `json:"text,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

func (s *FleetServer) handleFleet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.snapshot())
}

func (s *FleetServer) handleFleetWorker(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	projectName := strings.TrimSpace(r.URL.Query().Get("project"))
	slot := strings.TrimSpace(r.URL.Query().Get("slot"))
	if projectName == "" || slot == "" {
		writeError(w, http.StatusBadRequest, "project and slot are required")
		return
	}

	project, ok := s.findProject(projectName)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("project %q not found", projectName))
		return
	}
	if project.cfg == nil {
		writeError(w, http.StatusInternalServerError, "project config is unavailable")
		return
	}

	st, err := state.Load(project.cfg.StateDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load state: %v", err))
		return
	}
	sess, ok := st.Sessions[slot]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", slot))
		return
	}

	projectState := fleetProjectState{
		Name:         project.Name,
		Repo:         project.cfg.Repo,
		DashboardURL: project.DashboardURL,
		ReadOnly:     project.cfg.Server.ReadOnly || s.readOnly,
	}
	infos := []sessionInfo{makeSessionInfo(project.cfg.Repo, slot, sess)}
	applySupervisorAttention(infos, st.LatestSupervisorDecision())
	infos[0].Actions = workerActionAffordances(projectState.ReadOnly, "/api/v1/fleet/actions", infos[0])
	worker := makeFleetWorkerState(projectState, infos[0])
	lines := parsePositiveInt(r.URL.Query().Get("lines"), 260)
	if lines > 1000 {
		lines = 1000
	}
	writeJSON(w, http.StatusOK, fleetWorkerDetailResponse{
		Worker: worker,
		Log:    makeFleetLogTail(sess, lines),
	})
}

func (s *FleetServer) findProject(name string) (FleetProject, bool) {
	for _, project := range s.projects {
		if project.Name == name {
			return project, true
		}
	}
	return FleetProject{}, false
}

func makeFleetLogTail(sess *state.Session, lines int) fleetLogTail {
	updatedAt := time.Now().UTC().Format(time.RFC3339)
	logFile := strings.TrimSpace(sess.LogFile)
	if logFile == "" {
		return fleetLogTail{
			Available: false,
			Reason:    "No log file is recorded for this session.",
			Lines:     0,
			UpdatedAt: updatedAt,
		}
	}

	text, truncated, err := tailFile(logFile, lines, 512*1024)
	if err != nil {
		reason := "Log file could not be read on this host."
		if os.IsNotExist(err) {
			reason = "A log file is recorded for this session, but it is not available on this host."
		}
		return fleetLogTail{
			Available: false,
			Reason:    reason,
			Lines:     0,
			UpdatedAt: updatedAt,
		}
	}

	return fleetLogTail{
		Available: true,
		Lines:     countLines(text),
		Truncated: truncated,
		Text:      stripANSI(text),
		UpdatedAt: updatedAt,
	}
}

func countLines(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}

func (s *FleetServer) handleFleetAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.readOnly {
		writeError(w, http.StatusForbidden, "fleet server is read-only; write actions require approval-backed controls to be enabled in configuration")
		return
	}

	var req controlActionRequest
	if r.Body != nil {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("decode action request: %v", err))
			return
		}
	}
	if project, ok := s.findProject(req.Project); ok && project.cfg != nil && project.cfg.Server.ReadOnly {
		writeError(w, http.StatusForbidden, "fleet project is read-only; write actions require approval-backed controls to be enabled in configuration")
		return
	}
	writeError(w, http.StatusNotImplemented, "approval-backed action endpoints are not implemented yet")
}

func (s *FleetServer) snapshot() fleetResponse {
	now := time.Now().UTC()
	resp := fleetResponse{
		ReadOnly:    s.readOnly,
		RefreshedAt: formatFleetTime(now),
		Projects:    make([]fleetProjectState, 0, len(s.projects)),
		Workers:     make([]fleetWorkerState, 0),
		Attention:   make([]fleetWorkerState, 0),
		Approvals:   make([]fleetApprovalState, 0),
	}
	for _, project := range s.projects {
		item, workers := s.projectSnapshot(project, now)
		resp.Projects = append(resp.Projects, item)
		resp.Workers = append(resp.Workers, workers...)
		resp.Approvals = append(resp.Approvals, item.Approvals...)
		for _, worker := range item.Attention {
			resp.Attention = append(resp.Attention, makeFleetWorkerState(item, worker))
		}
		resp.Summary.Projects++
		if item.Freshness.Stale {
			resp.Summary.Stale++
		}
		if item.Error != "" {
			resp.Summary.Errors++
		}
		resp.Summary.Running += item.Running
		resp.Summary.PROpen += item.PROpen
		resp.Summary.Failed += item.Failed
		resp.Summary.Sessions += item.Sessions
		resp.Summary.NeedsAttention += item.NeedsAttention
		for _, approval := range item.Approvals {
			addFleetApprovalSummary(&resp.Summary, approval.Status)
		}
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
	sort.SliceStable(resp.Attention, func(i, j int) bool {
		left, right := resp.Attention[i], resp.Attention[j]
		li := fleetAttentionSeverity(left)
		ri := fleetAttentionSeverity(right)
		if li != ri {
			return li < ri
		}
		lt := fleetWorkerStartedAt(left)
		rt := fleetWorkerStartedAt(right)
		if !lt.Equal(rt) {
			return lt.After(rt)
		}
		if left.ProjectName != right.ProjectName {
			return left.ProjectName < right.ProjectName
		}
		return left.Slot < right.Slot
	})
	sortFleetApprovals(resp.Approvals)
	return resp
}

func newFleetProjectFreshness() fleetProjectFreshness {
	return fleetProjectFreshness{
		StaleAfterSeconds: int64(fleetProjectStaleAfter / time.Second),
	}
}

func fleetProjectFreshnessForState(stateDir string, st *state.State, now time.Time) fleetProjectFreshness {
	freshness := newFleetProjectFreshness()
	stateUpdatedAt := fileModTime(state.StatePath(stateDir))
	logUpdatedAt := latestProjectLogModTime(st)
	snapshotAt := latestTime(stateUpdatedAt, logUpdatedAt)

	if !stateUpdatedAt.IsZero() {
		freshness.StateUpdatedAt = formatFleetTime(stateUpdatedAt)
	}
	if !logUpdatedAt.IsZero() {
		freshness.LogUpdatedAt = formatFleetTime(logUpdatedAt)
	}
	if snapshotAt.IsZero() {
		freshness.Reason = "No state snapshot has been written yet."
		return freshness
	}

	freshness.SnapshotAt = formatFleetTime(snapshotAt)
	freshness.SnapshotAge = formatFleetAge(snapshotAt, now)
	freshness.SnapshotAgeSeconds = fleetAgeSeconds(snapshotAt, now)
	if time.Duration(freshness.SnapshotAgeSeconds)*time.Second > fleetProjectStaleAfter {
		freshness.Stale = true
		freshness.Reason = fmt.Sprintf("State/log snapshot has not changed for %s; stale after %s.", freshness.SnapshotAge, fleetProjectStaleAfter)
	}
	return freshness
}

func latestProjectLogModTime(st *state.State) time.Time {
	if st == nil {
		return time.Time{}
	}
	var latest time.Time
	for _, sess := range st.Sessions {
		if sess == nil {
			continue
		}
		updatedAt := fileModTime(strings.TrimSpace(sess.LogFile))
		latest = latestTime(latest, updatedAt)
	}
	return latest
}

func fileModTime(path string) time.Time {
	if strings.TrimSpace(path) == "" {
		return time.Time{}
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return time.Time{}
	}
	return info.ModTime().UTC()
}

func latestTime(left, right time.Time) time.Time {
	if left.IsZero() || right.After(left) {
		return right
	}
	return left
}

func addFleetApprovalSummary(summary *fleetSummary, status string) {
	summary.Approvals++
	switch state.ApprovalStatus(status) {
	case state.ApprovalStatusPending:
		summary.ApprovalsPending++
	case state.ApprovalStatusStale:
		summary.ApprovalsStale++
	case state.ApprovalStatusApproved:
		summary.ApprovalsApproved++
	case state.ApprovalStatusRejected:
		summary.ApprovalsRejected++
	}
}

func fleetAttentionSeverity(worker fleetWorkerState) int {
	if text := strings.ToLower(worker.Status + " " + worker.StatusReason + " " + worker.NextAction); strings.Contains(text, "blocked") {
		return 0
	}
	switch state.SessionStatus(worker.Status) {
	case state.StatusDead, state.StatusFailed, state.StatusConflictFailed, state.StatusRetryExhausted:
		return 0
	case state.StatusRunning:
		return 1
	case state.StatusPROpen, state.StatusQueued:
		return 2
	default:
		return 3
	}
}

func fleetWorkerStartedAt(worker fleetWorkerState) time.Time {
	startedAt, err := time.Parse(time.RFC3339, worker.StartedAt)
	if err != nil {
		return time.Time{}
	}
	return startedAt
}

func (s *FleetServer) projectSnapshot(project FleetProject, now time.Time) (fleetProjectState, []fleetWorkerState) {
	cfg := project.cfg
	item := fleetProjectState{
		Name:         project.Name,
		ConfigPath:   project.ConfigPath,
		DashboardURL: project.DashboardURL,
		Freshness:    newFleetProjectFreshness(),
	}
	if cfg == nil {
		item.Error = "missing resolved project config"
		return item, nil
	}
	item.Repo = cfg.Repo
	item.StateDir = cfg.StateDir
	item.MaxParallel = cfg.MaxParallel
	item.ReadOnly = cfg.Server.ReadOnly || s.readOnly
	item.Actions = projectActionAffordances(item.ReadOnly, "/api/v1/fleet/actions")
	item.Freshness = fleetProjectFreshnessForState(cfg.StateDir, nil, now)

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		item.Error = err.Error()
		return item, nil
	}
	item.Freshness = fleetProjectFreshnessForState(cfg.StateDir, st, now)
	projectState := buildStateResponse(cfg, st)
	item.Summary = projectState.Summary
	item.Running = len(projectState.Running)
	item.PROpen = len(projectState.PROpen)
	item.Failed = failedCount(projectState.Summary)
	item.Sessions = len(projectState.All)
	item.Supervisor = projectState.Supervisor
	item.Approvals = makeFleetApprovalStates(item, st, now)
	if len(item.Approvals) > 0 {
		item.ApprovalSummary = make(map[string]int)
		for _, approval := range item.Approvals {
			item.ApprovalSummary[approval.Status]++
		}
	}
	workers := make([]fleetWorkerState, 0)
	for _, worker := range projectState.All {
		worker.Actions = workerActionAffordances(item.ReadOnly, "/api/v1/fleet/actions", worker)
		if worker.NeedsAttention {
			item.NeedsAttention++
			item.Attention = append(item.Attention, worker)
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
	if worker.Status == string(state.StatusRunning) || worker.Status == string(state.StatusPROpen) || worker.NeedsAttention {
		return true
	}
	if worker.Status == string(state.StatusDone) {
		finishedAt, err := time.Parse(time.RFC3339, worker.FinishedAt)
		return err == nil && time.Since(finishedAt) <= 24*time.Hour
	}
	return false
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
		NextAction:        worker.NextAction,
		NeedsAttention:    worker.NeedsAttention,
		Backend:           worker.Backend,
		PRNumber:          worker.PRNumber,
		PRURL:             worker.PRURL,
		TokensUsedAttempt: worker.TokensUsedAttempt,
		TokensUsedTotal:   worker.TokensUsedTotal,
		Runtime:           worker.Runtime,
		RuntimeSeconds:    worker.RuntimeSeconds,
		StartedAt:         worker.StartedAt,
		FinishedAt:        worker.FinishedAt,
		NextRetryAt:       worker.NextRetryAt,
		PID:               worker.PID,
		Alive:             worker.Alive,
		Worktree:          worker.Worktree,
		Branch:            worker.Branch,
		TmuxSession:       worker.TmuxSession,
		HasLog:            worker.HasLog,
		RetryCount:        worker.RetryCount,
		LastNotification:  worker.LastNotification,
		Actions:           worker.Actions,
	}
}

func makeFleetApprovalStates(project fleetProjectState, st *state.State, now time.Time) []fleetApprovalState {
	if st == nil || len(st.Approvals) == 0 {
		return nil
	}
	items := make([]fleetApprovalState, 0, len(st.Approvals))
	for _, approval := range st.Approvals {
		items = append(items, makeFleetApprovalState(project, st, approval, now))
	}
	sortFleetApprovals(items)
	return items
}

func makeFleetApprovalState(project fleetProjectState, st *state.State, approval state.Approval, now time.Time) fleetApprovalState {
	issue, pr, session, sessionStatus := fleetApprovalTarget(st, approval.Target)
	createdAt := approval.CreatedAt.UTC()
	updatedAt := approval.UpdatedAt.UTC()
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	item := fleetApprovalState{
		ProjectName:       project.Name,
		ProjectRepo:       project.Repo,
		DashboardURL:      project.DashboardURL,
		ID:                approval.ID,
		DecisionID:        approval.DecisionID,
		Action:            approval.Action,
		Target:            approval.Target,
		IssueNumber:       issue,
		IssueURL:          githubIssueURL(project.Repo, issue),
		PRNumber:          pr,
		PRURL:             githubPRURL(project.Repo, pr),
		Session:           session,
		SessionStatus:     sessionStatus,
		Status:            string(approval.Status),
		Risk:              approval.Risk,
		Summary:           approval.Summary,
		CreatedAt:         formatFleetTime(createdAt),
		UpdatedAt:         formatFleetTime(updatedAt),
		CreatedAge:        formatFleetAge(createdAt, now),
		UpdatedAge:        formatFleetAge(updatedAt, now),
		CreatedAgeSeconds: fleetAgeSeconds(createdAt, now),
		UpdatedAgeSeconds: fleetAgeSeconds(updatedAt, now),
		createdAt:         createdAt,
		updatedAt:         updatedAt,
	}
	item.TargetLinks = fleetApprovalTargetLinks(project.Repo, item)
	return item
}

func fleetApprovalTarget(st *state.State, target *state.SupervisorTarget) (issue int, pr int, session string, sessionStatus string) {
	if target != nil {
		issue = target.Issue
		pr = target.PR
		session = strings.TrimSpace(target.Session)
	}
	if st == nil {
		return issue, pr, session, sessionStatus
	}
	if session != "" {
		if sess := st.Sessions[session]; sess != nil {
			if issue == 0 {
				issue = sess.IssueNumber
			}
			if pr == 0 {
				pr = sess.PRNumber
			}
			sessionStatus = string(sess.Status)
			return issue, pr, session, sessionStatus
		}
		session = ""
	}

	matchedSession := ""
	matchedIssue := issue
	matchedPR := pr
	matchedSessionStatus := ""
	for slot, sess := range st.Sessions {
		if sess == nil {
			continue
		}
		if (issue > 0 && sess.IssueNumber == issue) || (pr > 0 && sess.PRNumber == pr) {
			if matchedSession != "" {
				matchedSession = ""
				matchedSessionStatus = ""
				break
			}
			matchedSession = slot
			matchedIssue = issue
			if matchedIssue == 0 {
				matchedIssue = sess.IssueNumber
			}
			matchedPR = pr
			if matchedPR == 0 {
				matchedPR = sess.PRNumber
			}
			matchedSessionStatus = string(sess.Status)
		}
	}
	if matchedSession != "" {
		session = matchedSession
		issue = matchedIssue
		pr = matchedPR
		sessionStatus = matchedSessionStatus
	}
	return issue, pr, session, sessionStatus
}

func fleetApprovalTargetLinks(repo string, approval fleetApprovalState) []targetLinkInfo {
	links := make([]targetLinkInfo, 0, 3)
	if approval.IssueNumber > 0 {
		links = append(links, targetLinkInfo{
			Kind:  "issue",
			Label: fmt.Sprintf("Issue #%d", approval.IssueNumber),
			URL:   githubIssueURL(repo, approval.IssueNumber),
		})
	}
	if approval.PRNumber > 0 {
		links = append(links, targetLinkInfo{
			Kind:  "pr",
			Label: fmt.Sprintf("PR #%d", approval.PRNumber),
			URL:   githubPRURL(repo, approval.PRNumber),
		})
	}
	if strings.TrimSpace(approval.Session) != "" {
		links = append(links, targetLinkInfo{
			Kind:  "session",
			Label: "Session " + strings.TrimSpace(approval.Session),
		})
	}
	return links
}

func sortFleetApprovals(items []fleetApprovalState) {
	sort.SliceStable(items, func(i, j int) bool {
		left, right := items[i], items[j]
		li := fleetApprovalStatusRank(left.Status)
		ri := fleetApprovalStatusRank(right.Status)
		if li != ri {
			return li < ri
		}
		lt := left.updatedAt
		if lt.IsZero() {
			lt = left.createdAt
		}
		rt := right.updatedAt
		if rt.IsZero() {
			rt = right.createdAt
		}
		if !lt.Equal(rt) {
			return lt.After(rt)
		}
		if left.ProjectName != right.ProjectName {
			return left.ProjectName < right.ProjectName
		}
		return left.ID < right.ID
	})
}

func fleetApprovalStatusRank(status string) int {
	switch state.ApprovalStatus(status) {
	case state.ApprovalStatusPending:
		return 0
	case state.ApprovalStatusStale:
		return 1
	case state.ApprovalStatusApproved:
		return 2
	case state.ApprovalStatusRejected:
		return 3
	default:
		return 4
	}
}

func formatFleetTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func formatFleetAge(t, now time.Time) string {
	seconds := fleetAgeSeconds(t, now)
	if seconds == 0 && t.IsZero() {
		return ""
	}
	return (time.Duration(seconds) * time.Second).String()
}

func fleetAgeSeconds(t, now time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	d := now.Sub(t).Round(time.Second)
	if d < 0 {
		return 0
	}
	return int64(d / time.Second)
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
  .stats {
    display: grid;
    grid-template-columns: repeat(5, minmax(68px, 1fr));
    gap: 10px;
    width: min(520px, 100%);
  }
  .stat { text-align: right; min-width: 0; }
  .stat strong { display: block; font-size: 18px; font-variant-numeric: tabular-nums; }
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
  .approval-inbox {
    margin-bottom: 16px;
    border: 1px solid rgba(210,153,34,.35);
    background: linear-gradient(180deg, rgba(210,153,34,.08), rgba(21,27,35,.96) 90%);
  }
  .approval-list {
    display: grid;
    gap: 8px;
    padding: 12px 14px 14px;
  }
  .approval-card {
    display: grid;
    grid-template-columns: minmax(130px, .7fr) minmax(130px, .75fr) minmax(160px, 1fr) minmax(0, 2fr);
    gap: 10px;
    min-width: 0;
    padding: 10px 12px;
    border: 1px solid rgba(41,49,61,.9);
    border-left: 3px solid var(--line);
    background: rgba(16,22,29,.86);
  }
  .approval-card.approval-pending { border-left-color: var(--warn); background: rgba(210,153,34,.08); }
  .approval-card.approval-stale { border-left-color: var(--bad); background: rgba(248,81,73,.09); }
  .approval-card.approval-approved { border-left-color: var(--ok); background: rgba(63,185,80,.06); }
  .approval-card.approval-rejected { border-left-color: #ff7b72; background: rgba(255,123,114,.07); }
  .approval-project,
  .approval-action,
  .approval-target,
  .approval-main { min-width: 0; }
  .approval-project strong,
  .approval-action strong {
    display: block;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .approval-meta,
  .approval-age,
  .approval-risk {
    display: flex;
    flex-wrap: wrap;
    gap: 4px 8px;
    margin-top: 4px;
    color: var(--muted);
    font-size: 12px;
  }
  .approval-target { display: flex; flex-wrap: wrap; gap: 6px; align-content: flex-start; font-size: 12px; }
  .approval-summary { margin-top: 5px; color: var(--text); line-height: 1.35; }
  .link-button {
    border: 0;
    background: transparent;
    color: var(--accent);
    cursor: pointer;
    font: inherit;
    padding: 0;
  }
  .link-button:hover { text-decoration: underline; }
  .attention-inbox {
    margin-bottom: 16px;
    border: 1px solid rgba(248,81,73,.35);
    background: linear-gradient(180deg, rgba(248,81,73,.08), rgba(21,27,35,.96) 90%);
  }
  .attention-list {
    display: grid;
    gap: 10px;
    padding: 12px 14px 14px;
  }
  .attention-card {
    display: grid;
    grid-template-columns: minmax(150px, .7fr) minmax(0, 2.3fr);
    gap: 12px;
    min-width: 0;
    padding: 12px;
    border: 1px solid rgba(41,49,61,.9);
    background: rgba(16,22,29,.86);
  }
  .attention-card.selected { outline: 1px solid rgba(88,166,255,.65); outline-offset: -1px; }
  .attention-card[data-slot] { cursor: pointer; }
  .attention-card[data-slot]:hover { background: rgba(24,33,44,.92); }
  .attention-context, .attention-main, .attention-issue { min-width: 0; }
  .attention-project {
    display: block;
    overflow: hidden;
    font-weight: 700;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .attention-meta {
    display: flex;
    flex-wrap: wrap;
    gap: 5px 8px;
    margin-top: 5px;
    color: var(--muted);
    font-size: 12px;
  }
  .attention-top {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto auto;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }
  .attention-pr {
    overflow: hidden;
    color: var(--muted);
    font-size: 12px;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .attention-lines {
    display: grid;
    gap: 3px;
    margin-top: 7px;
    color: var(--muted);
    font-size: 12px;
    line-height: 1.4;
  }
  .attention-lines strong { color: var(--warn); font-weight: 650; }
  .attention-empty { padding: 12px; }
  .fleet-workers {
    margin-bottom: 16px;
    border: 1px solid var(--line);
    background: var(--panel);
  }

  .worker-detail {
    margin-bottom: 16px;
    border: 1px solid var(--line);
    background: var(--panel);
  }
  .worker-detail .section-head { border-bottom-color: rgba(41,49,61,.9); }
  .detail-body { padding: 14px; }
  .detail-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
    gap: 10px;
    margin-bottom: 12px;
  }
  .detail-field {
    min-width: 0;
    padding: 9px 10px;
    border: 1px solid rgba(41,49,61,.85);
    background: var(--panel-2);
  }
  .detail-field span {
    display: block;
    margin-bottom: 3px;
    color: var(--muted);
    font-size: 11px;
    font-weight: 650;
    text-transform: uppercase;
  }
  .detail-field strong {
    display: block;
    overflow: hidden;
    color: var(--text);
    font-size: 13px;
    font-weight: 500;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .detail-note {
    margin-bottom: 12px;
    padding: 10px 12px;
    border-left: 3px solid var(--accent);
    background: rgba(88,166,255,.08);
    color: var(--text);
  }
  .detail-note.attention {
    border-left-color: var(--bad);
    background: rgba(248,81,73,.1);
  }
  .detail-links { display: flex; flex-wrap: wrap; gap: 10px; margin-top: 6px; }
  .log-tail-head {
    display: flex;
    justify-content: space-between;
    gap: 12px;
    margin-bottom: 8px;
    color: var(--muted);
    font-size: 12px;
  }
  .log-tail pre {
    max-height: 360px;
    margin: 0;
    padding: 12px;
    overflow: auto;
    border: 1px solid rgba(41,49,61,.85);
    background: #05080d;
    color: #dbe7f3;
    font: 12px/1.45 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    white-space: pre-wrap;
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
  .worker-controls {
    display: grid;
    grid-template-columns: minmax(220px, 2fr) repeat(5, minmax(118px, 1fr));
    gap: 10px;
    padding: 12px 14px;
    border-bottom: 1px solid var(--line);
    background: rgba(16,22,29,.72);
  }
  .worker-controls label { display: grid; gap: 4px; min-width: 0; }
  .worker-controls span {
    color: var(--muted);
    font-size: 11px;
    font-weight: 650;
    text-transform: uppercase;
  }
  .worker-controls input,
  .worker-controls select,
  .worker-controls button {
    min-width: 0;
    width: 100%;
    border: 1px solid var(--line);
    border-radius: 8px;
    background: var(--panel-2);
    color: var(--text);
    font: inherit;
    padding: 7px 9px;
  }
  .worker-controls button { align-self: end; cursor: pointer; color: var(--accent); }
  .worker-controls button:hover { border-color: rgba(88,166,255,.65); }
  .table-scroll { overflow-x: auto; }
  .worker-table {
    width: 100%;
    min-width: 1180px;
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
  .worker-table td { max-width: 0; }
  .worker-table tbody tr.row-running { background: rgba(63,185,80,.055); }
  .worker-table tbody tr.row-pr { background: rgba(88,166,255,.055); }
  .worker-table tbody tr.row-attention { background: rgba(248,81,73,.1); }
  .worker-table tbody tr.selected { outline: 1px solid rgba(88,166,255,.65); outline-offset: -1px; }
  .worker-table tbody tr:hover { background: #18212c; }
  .worker-table tbody tr[data-slot] { cursor: pointer; }
  .project-col { width: 140px; font-weight: 650; }
  .slot-col { width: 92px; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
  .issue-col { width: auto; }
  .status-col { width: 132px; }
  .backend-col { width: 108px; }
  .pr-col { width: 70px; }
  .runtime-col { width: 90px; }
  .tokens-col { width: 82px; text-align: right; }
  .action-col { width: 270px; }
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
  .project.project-stale { border-color: rgba(210,153,34,.55); }
  .project.project-error { border-color: rgba(248,81,73,.55); }
  .project-head {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 14px;
    padding: 14px 14px 10px;
    border-bottom: 1px solid var(--line);
  }
  .project-head-main { min-width: 0; }
  .project-head-side {
    display: flex;
    flex-direction: column;
    align-items: flex-end;
    gap: 7px;
    min-width: 0;
  }
  .project h2 { margin: 0; font-size: 17px; }
  .repo { color: var(--muted); margin-top: 2px; font-size: 13px; }
  .freshness {
    display: flex;
    flex-wrap: wrap;
    gap: 5px 8px;
    margin-top: 5px;
    color: var(--muted);
    font-size: 12px;
  }
  .links { display: flex; gap: 10px; white-space: nowrap; font-size: 13px; }
  .badges { display: flex; flex-wrap: wrap; justify-content: flex-end; gap: 6px; }
  .badge {
    display: inline-block;
    max-width: 140px;
    overflow: hidden;
    padding: 1px 8px;
    border: 1px solid var(--line);
    border-radius: 999px;
    font-size: 12px;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .badge-stale { color: var(--warn); border-color: rgba(210,153,34,.55); background: rgba(210,153,34,.08); }
  .badge-error { color: var(--bad); border-color: rgba(248,81,73,.55); background: rgba(248,81,73,.08); }
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
  .supervisor, .workers, .project-actions { padding: 12px 14px; border-bottom: 1px solid var(--line); }
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
  .project-worker-slot { width: 68px; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
  .project-worker-status { width: 124px; padding-right: 8px; }
  .project-worker-issue { min-width: 0; padding-right: 8px; }
  .project-worker-runtime { width: 64px; text-align: right; color: var(--muted); }
  .issue-main {
    display: flex;
    align-items: baseline;
    gap: 5px;
    min-width: 0;
    overflow: hidden;
  }
  .issue-main a { flex: 0 0 auto; }
  .issue-title {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .pill {
    display: inline-block;
    max-width: 100%;
    overflow: hidden;
    padding: 1px 8px;
    border: 1px solid var(--line);
    border-radius: 999px;
    font-size: 12px;
    text-overflow: ellipsis;
    vertical-align: middle;
    white-space: nowrap;
  }
  .s-running { color: var(--ok); border-color: rgba(63,185,80,.45); }
  .s-pr_open { color: var(--accent); border-color: rgba(88,166,255,.45); }
  .s-done { color: var(--ok); border-color: rgba(63,185,80,.45); }
  .s-dead, .s-failed, .s-conflict_failed, .s-retry_exhausted { color: var(--bad); border-color: rgba(248,81,73,.45); }
  .a-pending { color: var(--warn); border-color: rgba(210,153,34,.55); background: rgba(210,153,34,.08); }
  .a-stale { color: var(--bad); border-color: rgba(248,81,73,.55); background: rgba(248,81,73,.08); }
  .a-approved { color: var(--ok); border-color: rgba(63,185,80,.55); background: rgba(63,185,80,.08); }
  .a-rejected { color: #ff7b72; border-color: rgba(255,123,114,.55); background: rgba(255,123,114,.08); }
  .attention { color: var(--bad); border-color: rgba(248,81,73,.45); }
  .actions { display: flex; gap: 6px; flex-wrap: wrap; }
  .action-btn {
    height: 24px;
    border: 1px solid rgba(210,153,34,.45);
    border-radius: 6px;
    background: rgba(210,153,34,.08);
    color: var(--warn);
    font: inherit;
    font-size: 12px;
    cursor: not-allowed;
  }
  .action-note { margin-top: 6px; color: var(--muted); font-size: 12px; white-space: normal; }
  .empty { color: var(--muted); margin-top: 8px; }
  .worker-table .empty { padding: 18px 14px; margin: 0; text-align: center; }
  .why-line {
    margin-top: 3px;
    color: var(--muted);
    font-size: 12px;
    line-height: 1.35;
    white-space: normal;
    overflow: hidden;
    text-overflow: clip;
  }
  .why-line strong { color: var(--warn); font-weight: 650; }
  .project-why {
    padding: 12px 14px;
    border-bottom: 1px solid var(--line);
    background: #0b1016;
  }
  .why-item { margin-top: 7px; color: var(--muted); font-size: 12px; line-height: 1.4; }
  .why-item strong { color: var(--text); }
  .error { color: var(--bad); border: 1px solid rgba(248,81,73,.35); border-radius: 10px; background: rgba(248,81,73,.08); padding: 12px 14px; }
  @media (max-width: 980px) {
    header { align-items: flex-start; flex-direction: column; }
    .stats { width: 100%; }
    .worker-controls { grid-template-columns: repeat(3, minmax(0, 1fr)); }
    .worker-controls .search-control { grid-column: 1 / -1; }
    .worker-table { min-width: 1080px; }
  }
  @media (max-width: 700px) {
    header { align-items: flex-start; flex-direction: column; }
    .stats { grid-template-columns: repeat(2, minmax(0, 1fr)); }
    .stat { text-align: left; }
    main { padding: 10px; }
    .section-head { flex-direction: column; }
    .section-note { text-align: left; }
    .worker-controls { grid-template-columns: 1fr; }
    .approval-card { grid-template-columns: 1fr; }
    .attention-card { grid-template-columns: 1fr; }
    .attention-top { grid-template-columns: minmax(0, 1fr) auto; }
    .attention-pr { grid-column: 1 / -1; }
    .grid { grid-template-columns: 1fr; }
    .project-head { flex-direction: column; }
    .project-head-side { align-items: flex-start; }
    .badges { justify-content: flex-start; }
    .metric-row { grid-template-columns: repeat(2, 1fr); }
    .detail-grid { grid-template-columns: 1fr; }
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
  <section class="approval-inbox" id="approval-inbox" aria-live="polite">
    <div class="section-head">
      <div>
        <h2>Approval Inbox</h2>
        <div class="sub">Read-only lifecycle view of supervisor approvals across projects.</div>
      </div>
      <div class="section-note" id="approval-summary">Loading approvals...</div>
    </div>
    <div class="approval-list" id="approval-list"></div>
  </section>
  <section class="attention-inbox" id="attention-inbox" aria-live="polite">
    <div class="section-head">
      <div>
        <h2>Attention Inbox</h2>
        <div class="sub">One ordered list of the worker/project items that need an operator now.</div>
      </div>
      <div class="section-note" id="attention-summary">Loading attention...</div>
    </div>
    <div class="attention-list" id="attention-list"></div>
  </section>
  <section class="fleet-workers">
    <div class="section-head">
      <div>
        <h2>Fleet Workers</h2>
        <div class="sub">Unified active, recent, and attention queue across projects.</div>
      </div>
      <div class="section-note" id="worker-summary">Loading workers...</div>
    </div>
    <div class="worker-controls" id="worker-controls">
      <label class="search-control" for="worker-filter"><span>Search</span><input id="worker-filter" type="search" placeholder="Project, issue, status, backend, or PR"></label>
      <label for="status-filter"><span>Status</span><select id="status-filter"></select></label>
      <label for="backend-filter"><span>Backend</span><select id="backend-filter"></select></label>
      <label for="pr-filter"><span>PR</span><select id="pr-filter"><option value="all">Any PR</option><option value="with">With PR</option><option value="without">No PR</option></select></label>
      <label for="worker-sort"><span>Sort</span><select id="worker-sort"><option value="status">Status</option><option value="project">Project</option><option value="issue">Issue</option><option value="runtime">Runtime</option><option value="pr">PR</option></select></label>
      <button type="button" id="sort-direction" aria-label="Toggle sort direction">Asc</button>
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
            <th class="action-col">Actions</th>
          </tr>
        </thead>
        <tbody id="fleet-workers-body"></tbody>
      </table>
    </div>
  </section>
  <section class="worker-detail" id="worker-detail" aria-live="polite">
    <div class="section-head">
      <div>
        <h2>Worker Detail</h2>
        <div class="sub">Select a worker row to inspect session state and recent log output.</div>
      </div>
      <div class="section-note" id="worker-detail-summary">No worker selected</div>
    </div>
    <div class="detail-body" id="worker-detail-body">
      <div class="empty">Select a fleet worker to show metadata and log output.</div>
    </div>
  </section>
  <div class="grid" id="projects"></div>
</main>
<script>
const projectsEl = document.getElementById("projects");
const statsEl = document.getElementById("stats");
const subtitleEl = document.getElementById("subtitle");
const tabsEl = document.getElementById("project-tabs");
const approvalListEl = document.getElementById("approval-list");
const approvalSummaryEl = document.getElementById("approval-summary");
const attentionListEl = document.getElementById("attention-list");
const attentionSummaryEl = document.getElementById("attention-summary");
const fleetWorkersEl = document.getElementById("fleet-workers-body");
const workerSummaryEl = document.getElementById("worker-summary");
const workerDetailSummaryEl = document.getElementById("worker-detail-summary");
const workerDetailBodyEl = document.getElementById("worker-detail-body");
const workerFilterEl = document.getElementById("worker-filter");
const statusFilterEl = document.getElementById("status-filter");
const backendFilterEl = document.getElementById("backend-filter");
const prFilterEl = document.getElementById("pr-filter");
const workerSortEl = document.getElementById("worker-sort");
const sortDirectionEl = document.getElementById("sort-direction");

const defaultSortDirections = { status: "asc", project: "asc", issue: "asc", runtime: "desc", pr: "asc" };
const validSortKeys = new Set(["status", "project", "issue", "runtime", "pr"]);
const validSortDirs = new Set(["asc", "desc"]);
const statusOrder = new Map([
  ["running", 0],
  ["pr_open", 1],
  ["queued", 2],
  ["dead", 3],
  ["failed", 4],
  ["conflict_failed", 5],
  ["retry_exhausted", 6],
  ["done", 7]
]);

const fleetState = {
  selectedProject: "all",
  readOnly: true,
  selectedWorkerKey: "",
  filters: {
    query: "",
    status: "all",
    backend: "all",
    pr: "all"
  },
  sortKey: "status",
  sortDir: "asc",
  projects: [],
  approvals: [],
  attention: [],
  workers: [],
  detail: null,
  refreshedAt: ""
};

loadStateFromQuery();

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

function issueSummaryHTML(worker) {
  const issue = worker.issue_number ? "#" + worker.issue_number : "-";
  return '<span class="issue-main">' + linkHTML(worker.issue_url, issue) +
    '<span class="issue-title">' + escapeText(worker.issue_title || "") + '</span></span>';
}

function issueSummaryText(worker) {
  const issue = worker.issue_number ? "#" + worker.issue_number : "-";
  return (issue + " " + (worker.issue_title || "")).trim();
}

function actionLabel(action) {
  return String(action || "-").replace(/_/g, " ");
}

function cssToken(value) {
  return String(value || "unknown").toLowerCase().replace(/[^a-z0-9_-]+/g, "_");
}

function actionDisabledReason(actions) {
  const action = (actions || []).find(item => item.disabled_reason);
  return action ? action.disabled_reason : "Write actions require approval-backed configuration.";
}

function renderActions(actions) {
  const items = actions || [];
  if (!items.length) return '<span class="empty">No controls.</span>';
  return '<div class="actions">' + items.map(action =>
    '<button type="button" class="action-btn" disabled aria-disabled="true" title="' +
    escapeText(action.disabled_reason || "Write action unavailable") + '">' +
    escapeText(action.label || actionLabel(action.id)) + '</button>'
  ).join("") + '</div>' +
  '<div class="action-note">' + escapeText(actionDisabledReason(items)) + '</div>';
}

function approvalStatusRank(status) {
  switch (status) {
  case "pending": return 0;
  case "stale": return 1;
  case "approved": return 2;
  case "rejected": return 3;
  default: return 4;
  }
}

function approvalTimeMillis(approval) {
  const updated = Date.parse(approval.updated_at || "");
  if (Number.isFinite(updated)) return updated;
  const created = Date.parse(approval.created_at || "");
  return Number.isFinite(created) ? created : 0;
}

function sortApprovals(approvals) {
  return approvals.map((approval, index) => ({ approval, index }))
    .sort((left, right) => {
      const status = compareNumber(approvalStatusRank(left.approval.status), approvalStatusRank(right.approval.status));
      if (status !== 0) return status;
      const freshness = compareNumber(approvalTimeMillis(right.approval), approvalTimeMillis(left.approval));
      if (freshness !== 0) return freshness;
      const project = compareText(left.approval.project_name, right.approval.project_name);
      if (project !== 0) return project;
      const id = compareText(left.approval.id, right.approval.id);
      if (id !== 0) return id;
      return left.index - right.index;
    })
    .map(entry => entry.approval);
}

function approvalsFromData(data) {
  const approvals = Array.isArray(data.approvals)
    ? data.approvals.slice()
    : (data.projects || []).flatMap(project => (project.approvals || []).map(approval => ({
      ...approval,
      project_name: approval.project_name || project.name,
      project_repo: approval.project_repo || project.repo,
      dashboard_url: approval.dashboard_url || project.dashboard_url
    })));
  return sortApprovals(approvals);
}

function approvalStatusClass(approval) {
  return "pill a-" + cssToken(approval.status || "unknown");
}

function approvalCardClass(approval) {
  return "approval-card approval-" + cssToken(approval.status || "unknown");
}

function approvalSessionVisible(approval) {
  return (fleetState.workers || []).some(worker =>
    worker.project_name === approval.project_name && worker.slot === approval.session);
}

function approvalTargetHTML(approval) {
  const links = [];
  if (approval.issue_number) links.push(linkHTML(approval.issue_url, "Issue #" + approval.issue_number));
  if (approval.pr_number) links.push(linkHTML(approval.pr_url, "PR #" + approval.pr_number));
  if (approval.session) {
    if (approvalSessionVisible(approval)) {
      links.push('<button type="button" class="link-button approval-session-link" data-project="' +
        escapeText(approval.project_name || "") + '" data-slot="' + escapeText(approval.session || "") + '">Session ' +
        escapeText(approval.session) + '</button>');
    } else {
      links.push('<span>Session ' + escapeText(approval.session) + '</span>');
    }
  }
  return links.length ? links.join(" ") : '<span class="empty">No target</span>';
}

function renderApprovalInbox() {
  const approvals = fleetState.approvals || [];
  if (!approvals.length) {
    approvalSummaryEl.textContent = "No approvals";
    approvalListEl.innerHTML = '<div class="empty approval-empty">No supervisor approvals are recorded across the fleet.</div>';
    return;
  }

  const counts = approvals.reduce((acc, approval) => {
    const status = approval.status || "unknown";
    acc[status] = (acc[status] || 0) + 1;
    return acc;
  }, {});
  approvalSummaryEl.textContent = (counts.pending || 0) + " pending · " + (counts.stale || 0) + " stale · " +
    (counts.approved || 0) + " approved · " + (counts.rejected || 0) + " rejected";
  approvalListEl.innerHTML = approvals.map(approval => {
    const project = approval.project_name || "-";
    const id = approval.id || "-";
    const action = actionLabel(approval.action || "-");
    const createdAge = approval.created_age || "-";
    const updatedAge = approval.updated_age || "-";
    const sessionStatus = approval.session_status ? "Status " + approval.session_status : "";
    return '<article class="' + approvalCardClass(approval) + '" title="' + escapeText(approval.summary || "") + '">' +
      '<div class="approval-project"><strong title="' + escapeText(project) + '">' + linkHTML(approval.dashboard_url, project) + '</strong>' +
        '<div class="approval-meta"><span title="' + escapeText(id) + '">' + escapeText(id) + '</span></div></div>' +
      '<div class="approval-action"><strong title="' + escapeText(action) + '">' + escapeText(action) + '</strong>' +
        '<div class="approval-meta"><span class="' + approvalStatusClass(approval) + '">' + escapeText(approval.status || "unknown") + '</span></div></div>' +
      '<div class="approval-target">' + approvalTargetHTML(approval) + (sessionStatus ? '<span>' + escapeText(sessionStatus) + '</span>' : "") + '</div>' +
      '<div class="approval-main"><div class="approval-age"><span>Created ' + escapeText(createdAge) + ' ago</span><span>Updated ' + escapeText(updatedAge) + ' ago</span></div>' +
        '<div class="approval-risk"><span>Risk ' + escapeText(approval.risk || "-") + '</span></div>' +
        '<div class="approval-summary">' + escapeText(approval.summary || "No summary recorded.") + '</div></div>' +
    '</article>';
  }).join("");

  approvalListEl.querySelectorAll(".approval-session-link[data-slot]").forEach(button => {
    button.addEventListener("click", () => selectWorker(button.dataset.project || "", button.dataset.slot || ""));
  });
}

function formatTimestamp(value) {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return String(value);
  return date.toLocaleString();
}

function formatDurationSeconds(value) {
  const seconds = Number(value || 0);
  if (!Number.isFinite(seconds) || seconds <= 0) return "";
  if (seconds < 60) return Math.round(seconds) + "s";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return minutes + "m";
  const hours = Math.round(minutes / 60);
  if (hours < 48) return hours + "h";
  return Math.round(hours / 24) + "d";
}

function workerKey(worker) {
  return (worker.project_name || "") + "\u001f" + (worker.slot || "");
}

function selectedWorker() {
  if (!fleetState.selectedWorkerKey) return null;
  return (fleetState.workers || []).find(worker => workerKey(worker) === fleetState.selectedWorkerKey) || null;
}

function normalizeParam(value, fallback) {
  const normalized = String(value ?? "").trim();
  return normalized === "" ? fallback : normalized;
}

function loadStateFromQuery() {
  const params = new URLSearchParams(window.location.search);
  fleetState.selectedProject = normalizeParam(params.get("project"), "all");
  fleetState.filters.query = normalizeParam(params.get("q"), "");
  fleetState.filters.status = normalizeParam(params.get("status"), "all");
  fleetState.filters.backend = normalizeParam(params.get("backend"), "all");
  const prFilter = normalizeParam(params.get("pr"), "all");
  fleetState.filters.pr = ["all", "with", "without"].includes(prFilter) ? prFilter : "all";
  const sortKey = normalizeParam(params.get("sort"), "status");
  fleetState.sortKey = validSortKeys.has(sortKey) ? sortKey : "status";
  const sortDir = normalizeParam(params.get("dir"), defaultSortDirections[fleetState.sortKey] || "asc");
  fleetState.sortDir = validSortDirs.has(sortDir) ? sortDir : (defaultSortDirections[fleetState.sortKey] || "asc");
}

function updateQueryState() {
  const params = new URLSearchParams(window.location.search);
  setQueryParam(params, "project", fleetState.selectedProject, "all");
  setQueryParam(params, "q", fleetState.filters.query, "");
  setQueryParam(params, "status", fleetState.filters.status, "all");
  setQueryParam(params, "backend", fleetState.filters.backend, "all");
  setQueryParam(params, "pr", fleetState.filters.pr, "all");
  setQueryParam(params, "sort", fleetState.sortKey, "status");
  setQueryParam(params, "dir", fleetState.sortDir, defaultSortDirections[fleetState.sortKey] || "asc");
  const next = params.toString();
  const url = window.location.pathname + (next ? "?" + next : "");
  window.history.replaceState(null, "", url);
}

function setQueryParam(params, key, value, defaultValue) {
  if (value && value !== defaultValue) {
    params.set(key, value);
  } else {
    params.delete(key);
  }
}

function uniqueSorted(values) {
  return Array.from(new Set(values.map(value => String(value ?? "").trim()).filter(Boolean)))
    .sort((left, right) => left.localeCompare(right, undefined, { numeric: true, sensitivity: "base" }));
}

function optionHTML(value, label, selectedValue) {
  const selected = value === selectedValue ? " selected" : "";
  return '<option value="' + escapeText(value) + '"' + selected + '>' + escapeText(label) + '</option>';
}

function selectOptionsHTML(allLabel, values, selectedValue) {
  const options = [optionHTML("all", allLabel, selectedValue)];
  if (selectedValue !== "all" && !values.includes(selectedValue)) {
    options.push(optionHTML(selectedValue, selectedValue + " (not present)", selectedValue));
  }
  for (const value of values) {
    options.push(optionHTML(value, value, selectedValue));
  }
  return options.join("");
}

function renderFilterOptions() {
  statusFilterEl.innerHTML = selectOptionsHTML("All statuses", uniqueSorted((fleetState.workers || []).map(worker => worker.status)), fleetState.filters.status);
  backendFilterEl.innerHTML = selectOptionsHTML("All backends", uniqueSorted((fleetState.workers || []).map(worker => worker.backend)), fleetState.filters.backend);
}

function syncFilterControls() {
  workerFilterEl.value = fleetState.filters.query;
  statusFilterEl.value = fleetState.filters.status;
  backendFilterEl.value = fleetState.filters.backend;
  prFilterEl.value = fleetState.filters.pr;
  workerSortEl.value = fleetState.sortKey;
  sortDirectionEl.textContent = fleetState.sortDir === "desc" ? "Desc" : "Asc";
  sortDirectionEl.setAttribute("aria-label", "Sort " + fleetState.sortDir + "; activate to switch direction");
}

function normalizedSearchText(value) {
  return String(value ?? "").trim().toLowerCase();
}

function workerSearchText(worker) {
  const issueNumber = worker.issue_number ? String(worker.issue_number) : "";
  const prNumber = worker.pr_number ? String(worker.pr_number) : "";
  return [
    worker.project_name,
    worker.project_repo,
    worker.slot,
    issueNumber,
    issueNumber ? "#" + issueNumber : "",
    worker.issue_title,
    worker.status,
    statusLabel(worker),
    worker.backend,
    prNumber,
    prNumber ? "#" + prNumber : "no pr",
    worker.runtime
  ].map(normalizedSearchText).join(" ");
}

function workerMatchesFilters(worker) {
  if (fleetState.filters.status !== "all" && worker.status !== fleetState.filters.status) return false;
  if (fleetState.filters.backend !== "all" && (worker.backend || "") !== fleetState.filters.backend) return false;
  if (fleetState.filters.pr === "with" && !worker.pr_number) return false;
  if (fleetState.filters.pr === "without" && worker.pr_number) return false;
  const terms = normalizedSearchText(fleetState.filters.query).split(/\s+/).filter(Boolean);
  if (!terms.length) return true;
  const haystack = workerSearchText(worker);
  return terms.every(term => haystack.includes(term));
}

function filteredWorkers(includeProjectFilter) {
  return (fleetState.workers || []).filter(worker => {
    if (includeProjectFilter && fleetState.selectedProject !== "all" && worker.project_name !== fleetState.selectedProject) return false;
    return workerMatchesFilters(worker);
  });
}

function selectedProjectWorkers() {
  if (fleetState.selectedProject === "all") return fleetState.workers || [];
  return (fleetState.workers || []).filter(worker => worker.project_name === fleetState.selectedProject);
}

function hasWorkerFilters() {
  return fleetState.filters.query !== "" || fleetState.filters.status !== "all" || fleetState.filters.backend !== "all" || fleetState.filters.pr !== "all";
}

function workerNeedsAttention(worker) {
  return worker.needs_attention || (worker.status === "running" && worker.alive === false);
}

function statusRank(worker) {
  const attention = workerNeedsAttention(worker) ? 0 : 1;
  const rank = statusOrder.has(worker.status) ? statusOrder.get(worker.status) : 99;
  return attention * 100 + rank;
}

function compareText(left, right) {
  return String(left || "").localeCompare(String(right || ""), undefined, { numeric: true, sensitivity: "base" });
}

function compareNumber(left, right) {
  const leftNumber = Number(left);
  const rightNumber = Number(right);
  const leftValue = Number.isFinite(leftNumber) ? leftNumber : Number.MAX_SAFE_INTEGER;
  const rightValue = Number.isFinite(rightNumber) ? rightNumber : Number.MAX_SAFE_INTEGER;
  if (leftValue === rightValue) return 0;
  return leftValue < rightValue ? -1 : 1;
}

function runtimeSeconds(worker) {
  const value = Number(worker.runtime_seconds || 0);
  return Number.isFinite(value) ? value : 0;
}

function compareWorkers(left, right, key) {
  switch (key) {
  case "project":
    return compareText(left.project_name, right.project_name);
  case "issue":
    return compareNumber(left.issue_number || Number.MAX_SAFE_INTEGER, right.issue_number || Number.MAX_SAFE_INTEGER);
  case "runtime":
    return compareNumber(runtimeSeconds(left), runtimeSeconds(right));
  case "pr":
    return compareNumber(left.pr_number || Number.MAX_SAFE_INTEGER, right.pr_number || Number.MAX_SAFE_INTEGER);
  case "status":
  default:
    return compareNumber(statusRank(left), statusRank(right));
  }
}

function sortWorkers(workers) {
  const direction = fleetState.sortDir === "desc" ? -1 : 1;
  return workers.map((worker, index) => ({ worker, index }))
    .sort((left, right) => {
      const result = compareWorkers(left.worker, right.worker, fleetState.sortKey);
      if (result !== 0) return result * direction;
      return left.index - right.index;
    })
    .map(entry => entry.worker);
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

function rowClass(worker) {
  if (worker.needs_attention || (worker.status === "running" && worker.alive === false)) return "row-attention";
  if (worker.status === "running") return "row-running";
  if (worker.status === "pr_open") return "row-pr";
  return "";
}

function workerWhyText(worker) {
  const reason = (worker.status_reason || "").trim();
  const action = (worker.next_action || "").trim();
  if (!reason && !action) return "";
  if (!reason) return "Next: " + action;
  const sep = reason.endsWith(".") || reason.endsWith("!") || reason.endsWith("?") ? " " : ". ";
  return reason + (action ? sep + "Next: " + action : "");
}

function workerWhyHTML(worker) {
  if (!worker.needs_attention && worker.status === "running") return "";
  const why = workerWhyText(worker);
  if (!why) return "";
  return '<div class="why-line"><strong>Why:</strong> ' + escapeText(why) + '</div>';
}

function attentionKey(worker) {
  return [worker.project_name || "", worker.slot || "", worker.issue_number || ""].join("\u001f");
}

function startedAtMillis(worker) {
  const startedAt = Date.parse(worker.started_at || "");
  return Number.isFinite(startedAt) ? startedAt : 0;
}

function attentionSeverityRank(worker) {
  const text = [worker.status, worker.status_reason, worker.next_action].map(normalizedSearchText).join(" ");
  if (text.includes("blocked") || ["dead", "failed", "conflict_failed", "retry_exhausted"].includes(worker.status)) return 0;
  if (worker.status === "running") return 1;
  if (worker.status === "pr_open" || worker.status === "queued") return 2;
  return 3;
}

function sortAttentionWorkers(workers) {
  return workers.map((worker, index) => ({ worker, index }))
    .sort((left, right) => {
      const severity = compareNumber(attentionSeverityRank(left.worker), attentionSeverityRank(right.worker));
      if (severity !== 0) return severity;
      const freshness = compareNumber(startedAtMillis(right.worker), startedAtMillis(left.worker));
      if (freshness !== 0) return freshness;
      const project = compareText(left.worker.project_name, right.worker.project_name);
      if (project !== 0) return project;
      const slot = compareText(left.worker.slot, right.worker.slot);
      if (slot !== 0) return slot;
      return left.index - right.index;
    })
    .map(entry => entry.worker);
}

function attentionFromData(data) {
  const items = [];
  const seen = new Set();
  const add = worker => {
    if (!worker || !workerNeedsAttention(worker)) return;
    const key = attentionKey(worker);
    if (seen.has(key)) return;
    seen.add(key);
    items.push(worker);
  };

  if (Array.isArray(data.attention)) {
    data.attention.forEach(add);
  }
  if (!Array.isArray(data.attention) && Array.isArray(data.workers)) {
    data.workers.forEach(add);
  }
  for (const project of data.projects || []) {
    for (const worker of project.attention || []) {
      add({
        ...worker,
        project_name: worker.project_name || project.name,
        project_repo: worker.project_repo || project.repo,
        dashboard_url: worker.dashboard_url || project.dashboard_url
      });
    }
  }
  return sortAttentionWorkers(items);
}

function attentionReasonText(worker) {
  return (worker.status_reason || "").trim() || "Needs operator review.";
}

function attentionNextActionText(worker) {
  return (worker.next_action || "").trim() || "Open the worker detail and choose the next safe action.";
}

function renderAttentionInbox() {
  const attention = fleetState.attention || [];
  if (!attention.length) {
    attentionSummaryEl.textContent = "No projects need attention";
    attentionListEl.innerHTML = '<div class="empty attention-empty">No projects need attention right now. The fleet is waiting normally.</div>';
    return;
  }

  const severe = attention.filter(worker => attentionSeverityRank(worker) === 0).length;
  attentionSummaryEl.textContent = attention.length + " item" + (attention.length === 1 ? "" : "s") + " need attention" +
    (severe ? " · " + severe + " blocked/dead/retry" : "");
  attentionListEl.innerHTML = attention.map(worker => {
    const project = worker.project_name || "-";
    const slot = worker.slot || "-";
    const age = worker.runtime || "-";
    const pr = worker.pr_number ? linkHTML(worker.pr_url, "PR #" + worker.pr_number) : "No PR";
    const selected = workerKey(worker) === fleetState.selectedWorkerKey ? " selected" : "";
    return '<article class="attention-card' + selected + '" data-project="' + escapeText(worker.project_name || "") + '" data-slot="' + escapeText(worker.slot || "") + '" tabindex="0" title="' + escapeText(attentionReasonText(worker)) + '">' +
      '<div class="attention-context">' +
        '<span class="attention-project" title="' + escapeText(project) + '">' + linkHTML(worker.dashboard_url, project) + '</span>' +
        '<div class="attention-meta"><span>Slot ' + escapeText(slot) + '</span><span>Age ' + escapeText(age) + '</span></div>' +
      '</div>' +
      '<div class="attention-main">' +
        '<div class="attention-top">' +
          '<div class="attention-issue" title="' + escapeText(issueSummaryText(worker)) + '">' + issueSummaryHTML(worker) + '</div>' +
          '<span class="' + statusClass(worker) + '" title="' + escapeText(statusLabel(worker)) + '">' + escapeText(statusLabel(worker)) + '</span>' +
          '<span class="attention-pr">' + pr + '</span>' +
        '</div>' +
        '<div class="attention-lines"><div><strong>Why:</strong> ' + escapeText(attentionReasonText(worker)) + '</div>' +
        '<div><strong>Next:</strong> ' + escapeText(attentionNextActionText(worker)) + '</div></div>' +
      '</div>' +
    '</article>';
  }).join("");

  attentionListEl.querySelectorAll(".attention-card[data-slot]").forEach(card => {
    card.addEventListener("click", () => selectWorker(card.dataset.project || "", card.dataset.slot || ""));
    card.addEventListener("keydown", event => {
      if (event.key === "Enter" || event.key === " ") {
        event.preventDefault();
        selectWorker(card.dataset.project || "", card.dataset.slot || "");
      }
    });
  });
  attentionListEl.querySelectorAll("a").forEach(link => {
    link.addEventListener("click", event => event.stopPropagation());
  });
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
    updateQueryState();
  }

  const filtered = filteredWorkers(false);
  const counts = new Map();
  for (const worker of filtered) {
    const name = worker.project_name || "";
    counts.set(name, (counts.get(name) || 0) + 1);
  }

  const tabs = [{ name: "all", label: "All projects", count: filtered.length }].concat(
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
      updateQueryState();
      renderProjectTabs();
      renderFleetWorkers();
    });
  });
}

function renderFleetWorkers() {
  const base = selectedProjectWorkers();
  const visible = sortWorkers(filteredWorkers(true));
  const projectLabel = fleetState.selectedProject === "all" ? "all projects" : fleetState.selectedProject;
  const filterText = hasWorkerFilters() ? " (" + visible.length + " of " + base.length + " after filters)" : "";
  const attentionCount = visible.filter(worker => worker.needs_attention).length;
  workerSummaryEl.textContent = visible.length + " active / recent / attention worker" + (visible.length === 1 ? "" : "s") + " in " + projectLabel +
    filterText + (attentionCount ? " · " + attentionCount + " need attention" : "");

  if (visible.length === 0) {
    const empty = hasWorkerFilters()
      ? "No workers match the current filters."
      : fleetState.selectedProject === "all"
      ? "No active, recent, or attention workers across configured projects."
      : "No active, recent, or attention workers for " + fleetState.selectedProject + ".";
    fleetWorkersEl.innerHTML = '<tr><td colspan="9" class="empty">' + escapeText(empty) + '</td></tr>';
    return;
  }

  fleetWorkersEl.innerHTML = visible.map(worker => {
    const pr = worker.pr_number ? "#" + worker.pr_number : "-";
    const project = worker.project_name || "-";
    const issueText = issueSummaryText(worker);
    const selected = workerKey(worker) === fleetState.selectedWorkerKey ? " selected" : "";
    return '<tr class="' + rowClass(worker) + selected + '" data-project="' + escapeText(worker.project_name || "") + '" data-slot="' + escapeText(worker.slot || "") + '" tabindex="0">' +
      '<td class="project-col" title="' + escapeText(project) + '">' + linkHTML(worker.dashboard_url, project) + '</td>' +
      '<td class="slot-col" title="' + escapeText(worker.slot || "-") + '">' + escapeText(worker.slot || "-") + '</td>' +
      '<td class="issue-col" title="' + escapeText(issueText) + '">' + issueSummaryHTML(worker) + workerWhyHTML(worker) + '</td>' +
      '<td class="status-col" title="' + escapeText(statusLabel(worker)) + '"><span class="' + statusClass(worker) + '">' + escapeText(statusLabel(worker)) + '</span></td>' +
      '<td class="backend-col" title="' + escapeText(worker.backend || "-") + '">' + escapeText(worker.backend || "-") + '</td>' +
      '<td class="pr-col" title="' + escapeText(pr) + '">' + linkHTML(worker.pr_url, pr) + '</td>' +
      '<td class="runtime-col" title="' + escapeText(worker.runtime || "-") + '">' + escapeText(worker.runtime || "-") + '</td>' +
      '<td class="tokens-col">' + compactNumber(worker.tokens_used_total) + '</td>' +
      '<td class="action-col">' + renderActions(worker.actions || []) + '</td>' +
    '</tr>';
  }).join("");

  fleetWorkersEl.querySelectorAll("tr[data-slot]").forEach(row => {
    row.addEventListener("click", () => selectWorker(row.dataset.project || "", row.dataset.slot || ""));
    row.addEventListener("keydown", event => {
      if (event.key === "Enter" || event.key === " ") {
        event.preventDefault();
        selectWorker(row.dataset.project || "", row.dataset.slot || "");
      }
    });
  });
}

function selectWorker(projectName, slot) {
  fleetState.selectedWorkerKey = projectName + "\u001f" + slot;
  fleetState.detail = null;
  renderAttentionInbox();
  renderFleetWorkers();
  renderWorkerDetailLoading(projectName, slot);
  loadWorkerDetail();
}

function renderWorkerDetailLoading(projectName, slot) {
  workerDetailSummaryEl.textContent = projectName && slot ? projectName + " / " + slot : "Loading worker";
  workerDetailBodyEl.innerHTML = '<div class="empty">Loading worker detail...</div>';
}

function emptyLogText(worker) {
  if (!worker) return "No log output available.";
  if (worker.status === "running" && worker.backend === "claude") {
    return "Log file is available, but Claude print mode may stay quiet until it finishes.";
  }
  if (worker.status === "running") return "Log file is available, but no output has been written yet.";
  return "Log file is available, but no output was captured.";
}

function aliveText(worker) {
  if (!worker || worker.alive === undefined || worker.alive === null) return "-";
  return worker.alive ? "true" : "false";
}

function detailField(label, value) {
  return '<div class="detail-field"><span>' + escapeText(label) + '</span><strong title="' + escapeText(value || "-") + '">' + escapeText(value || "-") + '</strong></div>';
}

function renderWorkerDetail(data) {
  if (!fleetState.selectedWorkerKey) {
    workerDetailSummaryEl.textContent = "No worker selected";
    workerDetailBodyEl.innerHTML = '<div class="empty">Select a fleet worker to show metadata and log output.</div>';
    return;
  }
  if (!data || !data.worker) {
    const worker = selectedWorker();
    if (!worker) {
      workerDetailSummaryEl.textContent = "Worker unavailable";
      workerDetailBodyEl.innerHTML = '<div class="empty">Selected worker is no longer visible in the fleet snapshot.</div>';
      return;
    }
    data = { worker: worker, log: { available: false, reason: "Worker detail has not loaded yet." } };
  }

  const worker = data.worker;
  const log = data.log || {};
  const issue = worker.issue_number ? "#" + worker.issue_number : "-";
  const pr = worker.pr_number ? "#" + worker.pr_number : "-";
  const links = [];
  if (worker.issue_url) links.push(linkHTML(worker.issue_url, "Issue " + issue));
  if (worker.pr_url) links.push(linkHTML(worker.pr_url, "PR " + pr));
  workerDetailSummaryEl.textContent = (worker.project_name || "-") + " / " + (worker.slot || "-") + " / " + statusLabel(worker);

  const fields = [
    detailField("Project", worker.project_name || "-"),
    detailField("Slot", worker.slot || "-"),
    detailField("Issue", issue + (worker.issue_title ? " " + worker.issue_title : "")),
    detailField("PR", pr),
    detailField("Backend", worker.backend || "-"),
    detailField("Status", statusLabel(worker)),
    detailField("Alive", aliveText(worker)),
    detailField("Attention", worker.needs_attention ? "yes" : "no"),
    detailField("Worktree", worker.worktree || "-"),
    detailField("Branch", worker.branch || "-"),
    detailField("Started", formatTimestamp(worker.started_at)),
    detailField("Finished", formatTimestamp(worker.finished_at)),
    detailField("Runtime", worker.runtime || "-"),
    detailField("Next retry", formatTimestamp(worker.next_retry_at)),
    detailField("Retry count", worker.retry_count ? String(worker.retry_count) : "0"),
    detailField("Log", worker.has_log ? "recorded" : "not recorded")
  ].join("");

  const noteClass = worker.needs_attention || (worker.status === "running" && worker.alive === false) ? " detail-note attention" : "detail-note";
  const reason = workerWhyText(worker) || "Waiting for the next Maestro reconciliation cycle.";
  const logText = log.available ? (log.text || emptyLogText(worker)) : (log.reason || "Log output is unavailable for this session.");
  const logMeta = log.available
    ? (log.truncated ? "tail, " : "") + (log.updated_at || "")
    : "unavailable";

  workerDetailBodyEl.innerHTML = '<div class="detail-grid">' + fields + '</div>' +
    '<div class="' + noteClass + '"><strong>State</strong> ' + escapeText(reason) +
      (links.length ? '<div class="detail-links">' + links.join("") + '</div>' : "") +
    '</div>' +
    '<div class="project-actions"><div class="label">Approval-gated controls</div>' + renderActions(worker.actions || []) + '</div>' +
    '<div class="log-tail">' +
      '<div class="log-tail-head"><strong>Recent log tail</strong><span>' + escapeText(logMeta) + '</span></div>' +
      '<pre>' + escapeText(logText) + '</pre>' +
    '</div>';
}

async function loadWorkerDetail() {
  const worker = selectedWorker();
  if (!worker) {
    fleetState.detail = null;
    renderWorkerDetail(null);
    return;
  }
  const key = workerKey(worker);
  try {
    const url = "/api/v1/fleet/worker?project=" + encodeURIComponent(worker.project_name || "") + "&slot=" + encodeURIComponent(worker.slot || "") + "&lines=260";
    const response = await fetch(url, { cache: "no-store" });
    if (!response.ok) throw new Error(await response.text());
    if (key !== fleetState.selectedWorkerKey) return;
    fleetState.detail = await response.json();
    renderWorkerDetail(fleetState.detail);
  } catch (err) {
    if (key !== fleetState.selectedWorkerKey) return;
    workerDetailSummaryEl.textContent = "Worker detail error";
    workerDetailBodyEl.innerHTML = '<div class="error">Unable to load worker detail: ' + escapeText(err.message) + '</div>';
  }
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

function supervisorWhyText(project) {
  const latest = project && project.supervisor && project.supervisor.latest;
  if (!latest) return "";
  if (latest.summary) return latest.summary;
  const reasons = latest.stuck_reasons && latest.stuck_reasons.length ? latest.stuck_reasons : latest.reasons || [];
  return reasons.length ? reasons[0] : "";
}

function renderProjectWhy(project) {
  const attention = project.attention || [];
  if (attention.length) {
    return '<div class="project-why"><div class="label">Why Attention</div>' +
      attention.map(worker => '<div class="why-item"><strong>' + escapeText(worker.slot || "-") + '</strong> ' +
        escapeText(workerWhyText(worker) || "Needs operator review.") + '</div>').join("") +
      '</div>';
  }
  const why = supervisorWhyText(project);
  if ((project.running || 0) === 0 && why) {
    return '<div class="project-why"><div class="label">Why Not Running</div>' +
      '<div class="why-item">' + escapeText(why) + '</div></div>';
  }
  return "";
}

function renderWorkers(project) {
  const workers = project.active || [];
  if (!workers.length) {
    return '<div class="workers"><div class="label">Active / recent / attention</div><div class="empty">No active, recent, or attention workers.</div></div>';
  }
  return '<div class="workers"><div class="label">Active / recent / attention</div><table>' +
    workers.map(worker => '<tr>' +
      '<td class="project-worker-slot">' + escapeText(worker.slot) + '</td>' +
      '<td class="project-worker-status"><span class="' + statusClass(worker) + '">' + escapeText(statusLabel(worker)) + '</span></td>' +
      '<td class="project-worker-issue" title="' + escapeText(issueSummaryText(worker)) + '">' + issueSummaryHTML(worker) + workerWhyHTML(worker) + '</td>' +
      '<td class="project-worker-runtime">' + escapeText(worker.runtime || "-") + '</td>' +
    '</tr>').join("") +
  '</table></div>';
}

function renderProjectActions(project) {
  return '<div class="project-actions"><div class="label">Approval-gated controls</div>' +
    renderActions(project.actions || []) + '</div>';
}

function projectFreshnessHTML(project) {
  const freshness = project.freshness || {};
  const age = freshness.snapshot_age ? "Snapshot " + freshness.snapshot_age + " ago" : "No snapshot yet";
  const details = [];
  if (freshness.state_updated_at) details.push("State " + formatTimestamp(freshness.state_updated_at));
  if (freshness.log_updated_at) details.push("Logs " + formatTimestamp(freshness.log_updated_at));
  const title = freshness.reason || details.join(" · ") || age;
  return '<div class="freshness" title="' + escapeText(title) + '"><span>' + escapeText(age) + '</span></div>';
}

function projectBadgesHTML(project) {
  const badges = [];
  if (project.error) {
    badges.push('<span class="badge badge-error">State error</span>');
  }
  if (project.freshness && project.freshness.stale) {
    const threshold = formatDurationSeconds(project.freshness.stale_after_seconds);
    badges.push('<span class="badge badge-stale">Stale' + (threshold ? ' &gt;' + escapeText(threshold) : '') + '</span>');
  }
  return badges.length ? '<div class="badges">' + badges.join("") + '</div>' : '';
}

function projectClass(project) {
  let cls = "project";
  if (project.error) cls += " project-error";
  if (project.freshness && project.freshness.stale) cls += " project-stale";
  return cls;
}

function projectHeaderHTML(project, rightHTML) {
  return '<div class="project-head"><div class="project-head-main"><h2>' + escapeText(project.name) + '</h2><div class="repo">' +
    escapeText(project.repo || project.config_path || "") + '</div>' + projectFreshnessHTML(project) + '</div>' +
    '<div class="project-head-side">' + (rightHTML || "") + projectBadgesHTML(project) + '</div></div>';
}

function renderProject(project) {
  if (project.error) {
    return '<article class="' + projectClass(project) + '">' + projectHeaderHTML(project, "") +
      '<div class="error">State error: ' + escapeText(project.error) + '</div></article>';
  }
  const failed = countFailed(project);
  const links = '<div class="links">' + linkHTML(project.dashboard_url, "Dashboard") + " " +
    linkHTML(project.repo ? "https://github.com/" + project.repo : "", "GitHub") + '</div>';
  return '<article class="' + projectClass(project) + '">' +
    projectHeaderHTML(project, links) +
    '<div class="metric-row">' +
      '<div class="metric"><strong>' + escapeText(project.running || 0) + " / " + escapeText(project.max_parallel || 0) + '</strong><span>Running</span></div>' +
      '<div class="metric"><strong>' + escapeText(project.pr_open || 0) + '</strong><span>PR open</span></div>' +
      '<div class="metric"><strong>' + escapeText(failed) + '</strong><span>Failed</span></div>' +
      '<div class="metric"><strong>' + escapeText(project.sessions || 0) + '</strong><span>Sessions</span></div>' +
      '<div class="metric"><strong>' + escapeText(project.needs_attention || 0) + '</strong><span>Attention</span></div>' +
    '</div>' +
    renderProjectWhy(project) +
    renderProjectActions(project) +
    renderSupervisor(project) +
    renderWorkers(project) +
  '</article>';
}

function refreshWorkersFromControls() {
  updateQueryState();
  renderProjectTabs();
  renderFleetWorkers();
}

workerFilterEl.addEventListener("input", () => {
  fleetState.filters.query = workerFilterEl.value.trim();
  refreshWorkersFromControls();
});

statusFilterEl.addEventListener("change", () => {
  fleetState.filters.status = statusFilterEl.value || "all";
  refreshWorkersFromControls();
});

backendFilterEl.addEventListener("change", () => {
  fleetState.filters.backend = backendFilterEl.value || "all";
  refreshWorkersFromControls();
});

prFilterEl.addEventListener("change", () => {
  fleetState.filters.pr = prFilterEl.value || "all";
  refreshWorkersFromControls();
});

workerSortEl.addEventListener("change", () => {
  const nextSort = validSortKeys.has(workerSortEl.value) ? workerSortEl.value : "status";
  if (nextSort !== fleetState.sortKey) {
    fleetState.sortKey = nextSort;
    fleetState.sortDir = defaultSortDirections[nextSort] || "asc";
  }
  syncFilterControls();
  updateQueryState();
  renderFleetWorkers();
});

sortDirectionEl.addEventListener("click", () => {
  fleetState.sortDir = fleetState.sortDir === "desc" ? "asc" : "desc";
  syncFilterControls();
  updateQueryState();
  renderFleetWorkers();
});

async function loadFleet() {
  try {
    const response = await fetch("/api/v1/fleet", { cache: "no-store" });
    if (!response.ok) throw new Error(await response.text());
    const data = await response.json();
    fleetState.readOnly = data.read_only !== false;
    fleetState.refreshedAt = data.refreshed_at || "";
    fleetState.projects = data.projects || [];
    fleetState.workers = fleetWorkersFromData(data);
    fleetState.approvals = approvalsFromData(data);
    fleetState.attention = attentionFromData(data);
    if (fleetState.selectedWorkerKey && !selectedWorker()) {
      fleetState.selectedWorkerKey = "";
      fleetState.detail = null;
    }
    const controlMode = fleetState.readOnly ? "read-only controls disabled" : "controls require approval configuration";
    const summary = data.summary || {};
    const alerts = [];
    if (summary.stale) alerts.push(summary.stale + " stale");
    if (summary.errors) alerts.push(summary.errors + " error" + (summary.errors === 1 ? "" : "s"));
    subtitleEl.textContent = "Last refresh " + formatTimestamp(fleetState.refreshedAt) + " · " +
      fleetState.projects.length + " configured project" + (fleetState.projects.length === 1 ? "" : "s") + " · " + controlMode +
      (alerts.length ? " · " + alerts.join(" · ") : "");
    renderFilterOptions();
    syncFilterControls();
    renderStats(summary);
    renderProjectTabs();
    renderApprovalInbox();
    renderAttentionInbox();
    renderFleetWorkers();
    renderWorkerDetail(fleetState.detail);
    projectsEl.innerHTML = fleetState.projects.map(renderProject).join("");
  } catch (err) {
    subtitleEl.textContent = "Fleet API error" + (fleetState.refreshedAt ? " · Last successful refresh " + formatTimestamp(fleetState.refreshedAt) : "");
    approvalSummaryEl.textContent = "Fleet API error";
    approvalListEl.innerHTML = '<div class="error">Unable to load approval inbox.</div>';
    attentionSummaryEl.textContent = "Fleet API error";
    attentionListEl.innerHTML = '<div class="error">Unable to load attention inbox.</div>';
    workerSummaryEl.textContent = "Fleet API error";
    fleetWorkersEl.innerHTML = '<tr><td colspan="9" class="empty">Unable to load fleet workers.</td></tr>';
    projectsEl.innerHTML = '<div class="error">' + escapeText(err.message) + '</div>';
  }
}

renderFilterOptions();
syncFilterControls();
loadFleet();
setInterval(loadFleet, 3000);
setInterval(loadWorkerDetail, 2000);
</script>
</body>
</html>`
