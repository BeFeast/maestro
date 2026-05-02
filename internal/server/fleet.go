package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
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
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/server/web"
	"github.com/befeast/maestro/internal/state"
	"gopkg.in/yaml.v3"
)

const (
	fleetProjectStaleAfter             = 15 * time.Minute
	fleetSupervisorHeartbeatStaleAfter = fleetProjectStaleAfter
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
	mux.HandleFunc("/api/v1/fleet/worker", s.handleFleetWorker)
	mux.HandleFunc("/api/v1/fleet", s.handleFleet)
	mux.HandleFunc("/api/v1/fleet/actions", s.handleFleetAction)
	mux.HandleFunc("/approvals/audit", s.handleFleetApprovalAudit)
	mux.Handle("/static/", web.StaticHandler())
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
	ReadOnly      bool                 `json:"read_only"`
	RefreshedAt   string               `json:"refreshed_at"`
	Verdict       fleetVerdict         `json:"verdict"`
	OperatorBrief fleetOperatorBrief   `json:"operator_brief"`
	Projects      []fleetProjectState  `json:"projects"`
	Summary       fleetSummary         `json:"summary"`
	Workers       []fleetWorkerState   `json:"workers"`
	Attention     []fleetWorkerState   `json:"attention"`
	Approvals     []fleetApprovalState `json:"approvals,omitempty"`
}

type fleetVerdict struct {
	Tone     string `json:"tone"`
	Sentence string `json:"sentence"`
}

type fleetOperatorBrief struct {
	Tone        string `json:"tone"`
	Sentence    string `json:"sentence"`
	Project     string `json:"project,omitempty"`
	Kind        string `json:"kind,omitempty"`
	NextAction  string `json:"next_action,omitempty"`
	IssueNumber int    `json:"issue_number,omitempty"`
	IssueURL    string `json:"issue_url,omitempty"`
	PRNumber    int    `json:"pr_number,omitempty"`
	PRURL       string `json:"pr_url,omitempty"`
	Session     string `json:"session,omitempty"`
}

type fleetOperatorState struct {
	Kind        string `json:"kind"`
	Tone        string `json:"tone"`
	Label       string `json:"label"`
	Summary     string `json:"summary"`
	NextAction  string `json:"next_action,omitempty"`
	IssueNumber int    `json:"issue_number,omitempty"`
	IssueURL    string `json:"issue_url,omitempty"`
	PRNumber    int    `json:"pr_number,omitempty"`
	PRURL       string `json:"pr_url,omitempty"`
	Session     string `json:"session,omitempty"`
}

type fleetSummary struct {
	Projects            int   `json:"projects"`
	Stale               int   `json:"stale"`
	Errors              int   `json:"errors"`
	Active              int   `json:"active"`
	MonitoringPR        int   `json:"monitoring_pr"`
	DispatchPending     int   `json:"dispatch_pending"`
	QueueBlocked        int   `json:"queue_blocked"`
	OutcomeMissing      int   `json:"outcome_missing"`
	Running             int   `json:"running"`
	PROpen              int   `json:"pr_open"`
	Failed              int   `json:"failed"`
	Sessions            int   `json:"sessions"`
	NeedsAttention      int   `json:"needs_attention"`
	Approvals           int   `json:"approvals"`
	ApprovalsPending    int   `json:"approvals_pending"`
	ApprovalsHistorical int   `json:"approvals_historical"`
	ApprovalsStale      int   `json:"approvals_stale"`
	ApprovalsSuperseded int   `json:"approvals_superseded"`
	ApprovalsApproved   int   `json:"approvals_approved"`
	ApprovalsRejected   int   `json:"approvals_rejected"`
	ThroughputMerged7D  int   `json:"throughput_merged_7d"`
	ThroughputDaily7D   []int `json:"throughput_daily_7d,omitempty"`
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

type fleetQueueSnapshot struct {
	PolicyRule                    string                          `json:"policy_rule,omitempty"`
	Open                          int                             `json:"open"`
	Eligible                      int                             `json:"eligible"`
	Excluded                      int                             `json:"excluded"`
	Held                          int                             `json:"held"`
	BlockedByDependency           int                             `json:"blocked_by_dependency"`
	NonRunnableProjectStatusCount int                             `json:"non_runnable_project_status_count"`
	SelectedCandidate             *state.SupervisorIssueCandidate `json:"selected_candidate,omitempty"`
	TopSkippedReason              string                          `json:"top_skipped_reason,omitempty"`
	IdleReason                    string                          `json:"idle_reason,omitempty"`
}

type fleetProjectState struct {
	Name            string                `json:"name"`
	Repo            string                `json:"repo"`
	ConfigPath      string                `json:"config_path"`
	DashboardURL    string                `json:"dashboard_url,omitempty"`
	StateDir        string                `json:"state_dir,omitempty"`
	MaxParallel     int                   `json:"max_parallel"`
	ReadOnly        bool                  `json:"read_only"`
	OperatorState   fleetOperatorState    `json:"operator_state"`
	Outcome         outcome.Status        `json:"outcome"`
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
	QueueSnapshot   *fleetQueueSnapshot   `json:"queue_snapshot,omitempty"`
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
	DisplayStatus     string          `json:"display_status,omitempty"`
	StatusReason      string          `json:"status_reason,omitempty"`
	NextAction        string          `json:"next_action,omitempty"`
	NeedsAttention    bool            `json:"needs_attention,omitempty"`
	Live              bool            `json:"live"`
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
	throughputBuckets := newFleetThroughputBuckets(now, 7)
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
		addFleetOperatorSummary(&resp.Summary, item.OperatorState)
		resp.Summary.Running += item.Running
		resp.Summary.PROpen += item.PROpen
		resp.Summary.Failed += item.Failed
		resp.Summary.Sessions += item.Sessions
		resp.Summary.NeedsAttention += item.NeedsAttention
		addFleetThroughputSummary(throughputBuckets, workers)
		for _, approval := range item.Approvals {
			addFleetApprovalSummary(&resp.Summary, approval.Status)
		}
	}
	resp.Summary.ThroughputDaily7D = throughputBuckets.Counts()
	resp.Summary.ThroughputMerged7D = throughputBuckets.Total()
	sort.Slice(resp.Projects, func(i, j int) bool {
		li := fleetOperatorStatePriority(resp.Projects[i].OperatorState.Kind)
		ri := fleetOperatorStatePriority(resp.Projects[j].OperatorState.Kind)
		if li != ri {
			return li < ri
		}
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
	resp.OperatorBrief = buildFleetOperatorBrief(resp.Projects, now)
	resp.Verdict = buildFleetVerdict(resp, now)
	return resp
}

func buildFleetVerdict(resp fleetResponse, now time.Time) fleetVerdict {
	latest := latestFleetSupervisorDecision(resp.Projects)
	tone := fleetVerdictTone(resp.Summary, latest, now)
	parts := []string{
		fleetLivenessSentence(resp.Summary, resp.Projects, latest, now),
		fleetActivitySentence(resp.Summary, resp.Projects),
	}
	if pr := fleetPRSentence(resp.Summary.PROpen); pr != "" {
		parts = append(parts, pr)
	}
	parts = append(parts, fleetAttentionSentence(resp.Summary))
	if brief := strings.TrimSpace(resp.OperatorBrief.Sentence); brief != "" && !supervisorHeartbeatStale(latest, now) {
		parts = append(parts, brief)
	}
	return fleetVerdict{
		Tone:     tone,
		Sentence: strings.Join(parts, " "),
	}
}

func fleetVerdictTone(summary fleetSummary, latest *supervisorDecisionInfo, now time.Time) string {
	if latest == nil || supervisorHeartbeatStale(latest, now) {
		return "daemon-down"
	}
	if summary.Stale > 0 || summary.Errors > 0 || summary.NeedsAttention > 0 || summary.ApprovalsPending > 0 {
		return "attention"
	}
	if summary.Running > 0 {
		return "busy"
	}
	return "healthy"
}

func fleetLivenessSentence(summary fleetSummary, projects []fleetProjectState, latest *supervisorDecisionInfo, now time.Time) string {
	if latest == nil || latest.CreatedAt.IsZero() {
		return "Supervisor heartbeat unavailable."
	}
	if supervisorHeartbeatStale(latest, now) {
		sentence := fmt.Sprintf("Supervisor heartbeat lost %s ago.", formatFleetVerdictAge(latest.CreatedAt, now))
		if lastSafe := latestFleetSafeSupervisorAction(projects); lastSafe != nil {
			if safe := fleetLastSafeActionSentence(*lastSafe, now); safe != "" {
				sentence += " " + safe
			}
		}
		return sentence
	}
	if summary.Stale > 0 {
		return fmt.Sprintf("Supervisor healthy. %s.", staleProjectSnapshotPhrase(summary.Stale))
	}
	return "Supervisor healthy."
}

func supervisorHeartbeatStale(latest *supervisorDecisionInfo, now time.Time) bool {
	if latest == nil || latest.CreatedAt.IsZero() {
		return true
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return now.Sub(latest.CreatedAt) > fleetSupervisorHeartbeatStaleAfter
}

func fleetRunningSentence(running int, idleByPolicy bool) string {
	switch running {
	case 0:
		if idleByPolicy {
			return "No worker is running by policy."
		}
		return "No worker is running."
	case 1:
		return "1 worker is running."
	default:
		return fmt.Sprintf("%d workers are running.", running)
	}
}

func fleetActivitySentence(summary fleetSummary, projects []fleetProjectState) string {
	if summary.Running > 0 {
		return fleetRunningSentence(summary.Running, fleetIdleByPolicy(projects))
	}
	if summary.Active > 0 {
		pieces := make([]string, 0, 2)
		if summary.MonitoringPR > 0 {
			pieces = append(pieces, fleetCountPhrase(summary.MonitoringPR, "monitoring PR", "monitoring PRs"))
		}
		if summary.DispatchPending > 0 {
			pieces = append(pieces, fleetCountPhrase(summary.DispatchPending, "dispatch pending", "dispatch pending"))
		}
		if len(pieces) == 0 {
			return "No worker process is running, but the supervisor reports active work."
		}
		return "No worker process is running, but " + strings.Join(pieces, " and ") + "."
	}
	return fleetRunningSentence(0, fleetIdleByPolicy(projects))
}

func fleetCountPhrase(count int, singular, plural string) string {
	if count == 1 {
		return fmt.Sprintf("1 %s", singular)
	}
	return fmt.Sprintf("%d %s", count, plural)
}

func fleetPRSentence(prOpen int) string {
	switch prOpen {
	case 0:
		return ""
	case 1:
		return "1 PR is waiting for review."
	default:
		return fmt.Sprintf("%d PRs are waiting for review.", prOpen)
	}
}

func fleetAttentionSentence(summary fleetSummary) string {
	items := summary.NeedsAttention + summary.ApprovalsPending + summary.Errors + summary.Stale
	switch items {
	case 0:
		return "No item needs attention."
	case 1:
		return "1 item needs attention."
	default:
		return fmt.Sprintf("%d items need attention.", items)
	}
}

func addFleetOperatorSummary(summary *fleetSummary, operator fleetOperatorState) {
	kind := strings.TrimSpace(operator.Kind)
	if fleetOperatorStateIsActive(kind) {
		summary.Active++
	}
	switch kind {
	case "monitoring_pr":
		summary.MonitoringPR++
	case "pending_dispatch":
		summary.DispatchPending++
	case "queue_blocked":
		summary.QueueBlocked++
	case "outcome_missing":
		summary.OutcomeMissing++
	}
}

func fleetOperatorStateIsActive(kind string) bool {
	switch strings.TrimSpace(kind) {
	case "working", "monitoring_pr", "pending_dispatch":
		return true
	default:
		return false
	}
}

func buildFleetOperatorBrief(projects []fleetProjectState, now time.Time) fleetOperatorBrief {
	_ = now
	if len(projects) == 0 {
		return fleetOperatorBrief{Tone: "muted", Sentence: "No projects are configured in this fleet."}
	}
	var selected *fleetProjectState
	for i := range projects {
		project := &projects[i]
		if selected == nil || fleetOperatorStatePriority(project.OperatorState.Kind) < fleetOperatorStatePriority(selected.OperatorState.Kind) {
			selected = project
		}
	}
	if selected == nil {
		return fleetOperatorBrief{Tone: "muted", Sentence: "No project state is available."}
	}

	working, monitoring, pending, attention := 0, 0, 0, 0
	for _, project := range projects {
		switch project.OperatorState.Kind {
		case "working":
			working++
		case "monitoring_pr":
			monitoring++
		case "pending_dispatch":
			pending++
		case "attention", "error", "stale":
			attention++
		}
	}
	state := selected.OperatorState
	brief := fleetOperatorBrief{
		Tone:        state.Tone,
		Project:     selected.Name,
		Kind:        state.Kind,
		NextAction:  state.NextAction,
		IssueNumber: state.IssueNumber,
		IssueURL:    state.IssueURL,
		PRNumber:    state.PRNumber,
		PRURL:       state.PRURL,
		Session:     state.Session,
	}
	if attention > 0 || state.Tone == "attention" || state.Tone == "error" || state.Tone == "warn" {
		brief.Sentence = fmt.Sprintf("Operator brief: %s: %s — %s", selected.Name, state.Label, state.Summary)
		if next := strings.TrimSpace(state.NextAction); next != "" {
			brief.Sentence += "; next: " + next
		}
		return brief
	}
	parts := make([]string, 0, 3)
	if working > 0 {
		parts = append(parts, fleetCountPhrase(working, "working", "working"))
	}
	if monitoring > 0 {
		parts = append(parts, fleetCountPhrase(monitoring, "monitoring PR", "monitoring PRs"))
	}
	if pending > 0 {
		parts = append(parts, fleetCountPhrase(pending, "dispatch pending", "dispatch pending"))
	}
	if len(parts) == 0 {
		brief.Tone = "healthy"
		brief.Sentence = "Operator brief: all projects are idle by policy; no operator action is pending."
		return brief
	}
	brief.Tone = "busy"
	brief.Sentence = "Operator brief: " + strings.Join(parts, " · ") + "; no operator action is pending."
	return brief
}

func fleetOperatorStatePriority(kind string) int {
	switch strings.TrimSpace(kind) {
	case "error":
		return 0
	case "attention":
		return 1
	case "stale":
		return 2
	case "pending_dispatch":
		return 3
	case "working":
		return 4
	case "monitoring_pr":
		return 5
	case "outcome_missing":
		return 6
	case "queue_blocked":
		return 7
	case "idle":
		return 8
	default:
		return 9
	}
}

func latestFleetSupervisorDecision(projects []fleetProjectState) *supervisorDecisionInfo {
	var latest *supervisorDecisionInfo
	for i := range projects {
		decision := projects[i].Supervisor.Latest
		if decision == nil || decision.CreatedAt.IsZero() {
			continue
		}
		if latest == nil || decision.CreatedAt.After(latest.CreatedAt) {
			latest = decision
		}
	}
	return latest
}

func latestFleetSafeSupervisorAction(projects []fleetProjectState) *supervisorActionInfo {
	var latest *supervisorActionInfo
	for i := range projects {
		action := projects[i].Supervisor.LastSafeAction
		if action == nil || action.CreatedAt.IsZero() {
			continue
		}
		if latest == nil || action.CreatedAt.After(latest.CreatedAt) {
			latest = action
		}
	}
	return latest
}

func fleetLastSafeActionSentence(action supervisorActionInfo, now time.Time) string {
	summary := strings.TrimSpace(strings.Join(strings.Fields(action.Summary), " "))
	if summary == "" {
		summary = strings.TrimSpace(action.Action)
	}
	if summary == "" {
		return ""
	}
	if len([]rune(summary)) > 120 {
		runes := []rune(summary)
		summary = string(runes[:117]) + "..."
	}
	if action.CreatedAt.IsZero() {
		return fmt.Sprintf("Last safe action was %s.", strconv.Quote(summary))
	}
	return fmt.Sprintf("Last safe action was %s %s ago.", strconv.Quote(summary), formatFleetVerdictAge(action.CreatedAt, now))
}

func fleetIdleByPolicy(projects []fleetProjectState) bool {
	if len(projects) == 0 {
		return false
	}
	for _, project := range projects {
		if project.Error != "" {
			return false
		}
		if project.Running > 0 {
			return false
		}
		if project.QueueSnapshot == nil || strings.TrimSpace(project.QueueSnapshot.IdleReason) == "" {
			return false
		}
	}
	return true
}

func staleProjectSnapshotPhrase(count int) string {
	if count == 1 {
		return "1 project snapshot is stale"
	}
	return fmt.Sprintf("%d project snapshots are stale", count)
}

func formatFleetVerdictAge(t, now time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	d := now.Sub(t).Round(time.Second)
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		seconds := int(d / time.Second)
		if seconds <= 0 {
			return "just now"
		}
		return fmt.Sprintf("%ds", seconds)
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Round(time.Minute)/time.Minute))
	}
	if d < 24*time.Hour {
		rounded := d.Round(time.Minute)
		hours := int(rounded / time.Hour)
		minutes := int((rounded % time.Hour) / time.Minute)
		if minutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, minutes)
	}
	return fmt.Sprintf("%dd", int(d.Round(24*time.Hour)/(24*time.Hour)))
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
	if now.Sub(snapshotAt) > fleetProjectStaleAfter {
		freshness.Stale = true
		freshness.Reason = fmt.Sprintf("State/log snapshot has not changed for %s; stale after %s.", freshness.SnapshotAge, fleetProjectStaleAfter)
	}
	return freshness
}

func fleetQueueSnapshotFromSupervisor(info supervisorInfo) *fleetQueueSnapshot {
	if info.Latest == nil || info.Latest.QueueAnalysis == nil {
		return nil
	}
	analysis := info.Latest.QueueAnalysis
	policyRule := strings.TrimSpace(analysis.PolicyRule)
	if policyRule == "" {
		policyRule = strings.TrimSpace(info.Latest.PolicyRule)
	}
	snapshot := &fleetQueueSnapshot{
		PolicyRule:                    policyRule,
		Open:                          analysis.OpenIssues,
		Eligible:                      analysis.EligibleCandidates,
		Excluded:                      analysis.ExcludedIssues,
		Held:                          analysis.HeldIssues,
		BlockedByDependency:           analysis.BlockedByDependencyIssues,
		NonRunnableProjectStatusCount: analysis.NonRunnableProjectStatusCount,
		TopSkippedReason:              analysis.TopSkippedReason(),
		IdleReason:                    analysis.IdleReason(),
	}
	if analysis.SelectedCandidate != nil {
		candidate := *analysis.SelectedCandidate
		snapshot.SelectedCandidate = &candidate
	}
	return snapshot
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
	switch state.ApprovalStatus(status) {
	case state.ApprovalStatusPending:
		summary.Approvals++
		summary.ApprovalsPending++
	case state.ApprovalStatusStale:
		summary.ApprovalsHistorical++
		summary.ApprovalsStale++
	case state.ApprovalStatusSuperseded:
		summary.ApprovalsHistorical++
		summary.ApprovalsSuperseded++
	case state.ApprovalStatusApproved:
		summary.ApprovalsHistorical++
		summary.ApprovalsApproved++
	case state.ApprovalStatusRejected:
		summary.ApprovalsHistorical++
		summary.ApprovalsRejected++
	default:
		summary.ApprovalsHistorical++
	}
}

type fleetThroughputBuckets struct {
	days  int
	start time.Time
	end   time.Time
	total int
	items []int
}

func newFleetThroughputBuckets(now time.Time, days int) *fleetThroughputBuckets {
	if days <= 0 {
		days = 7
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	start := end.AddDate(0, 0, -(days - 1))
	return &fleetThroughputBuckets{
		days:  days,
		start: start,
		end:   end,
		items: make([]int, days),
	}
}

func (b *fleetThroughputBuckets) Add(ts time.Time) {
	if b == nil || ts.IsZero() {
		return
	}
	day := time.Date(ts.UTC().Year(), ts.UTC().Month(), ts.UTC().Day(), 0, 0, 0, 0, time.UTC)
	if day.Before(b.start) || day.After(b.end) {
		return
	}
	offset := int(day.Sub(b.start) / (24 * time.Hour))
	if offset < 0 || offset >= len(b.items) {
		return
	}
	b.items[offset]++
	b.total++
}

func (b *fleetThroughputBuckets) Counts() []int {
	if b == nil {
		return nil
	}
	out := make([]int, len(b.items))
	copy(out, b.items)
	return out
}

func (b *fleetThroughputBuckets) Total() int {
	if b == nil {
		return 0
	}
	return b.total
}

func addFleetThroughputSummary(buckets *fleetThroughputBuckets, workers []fleetWorkerState) {
	if buckets == nil {
		return
	}
	for _, worker := range workers {
		if worker.Status != string(state.StatusDone) || worker.PRNumber <= 0 || strings.TrimSpace(worker.FinishedAt) == "" {
			continue
		}
		finishedAt, err := time.Parse(time.RFC3339, worker.FinishedAt)
		if err != nil {
			continue
		}
		buckets.Add(finishedAt)
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
		item.OperatorState = buildFleetProjectOperatorState(item)
		return item, nil
	}
	item.Repo = cfg.Repo
	item.StateDir = cfg.StateDir
	item.MaxParallel = cfg.MaxParallel
	item.ReadOnly = cfg.Server.ReadOnly || s.readOnly
	item.Outcome = outcome.StatusFor(cfg.Outcome, 0, time.Time{})
	item.Actions = projectActionAffordances(item.ReadOnly, "/api/v1/fleet/actions", item.Name)
	item.Freshness = fleetProjectFreshnessForState(cfg.StateDir, nil, now)

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		item.Error = err.Error()
		item.OperatorState = buildFleetProjectOperatorState(item)
		return item, nil
	}
	item.Freshness = fleetProjectFreshnessForState(cfg.StateDir, st, now)
	projectState := buildStateResponse(cfg, st)
	item.Summary = projectState.Summary
	item.Outcome = projectState.Outcome
	item.Running = len(projectState.Running)
	item.PROpen = len(projectState.PROpen)
	item.Failed = failedCount(projectState.Summary)
	item.Sessions = len(projectState.All)
	item.Supervisor = projectState.Supervisor
	item.QueueSnapshot = fleetQueueSnapshotFromSupervisor(item.Supervisor)
	item.Approvals = makeFleetApprovalStates(item, st, now)
	if len(item.Approvals) > 0 {
		item.ApprovalSummary = make(map[string]int)
		for _, approval := range item.Approvals {
			item.ApprovalSummary[approval.Status]++
		}
	}
	workers := make([]fleetWorkerState, 0, len(projectState.All))
	for _, worker := range projectState.All {
		worker.Actions = workerActionAffordances(item.ReadOnly, "/api/v1/fleet/actions", worker)
		if worker.NeedsAttention {
			item.NeedsAttention++
			item.Attention = append(item.Attention, worker)
		}
		workers = append(workers, makeFleetWorkerState(item, worker))
		if isFleetWorkerDefaultVisible(worker) {
			if len(item.Active) >= 6 {
				continue
			}
			item.Active = append(item.Active, worker)
		}
	}
	item.OperatorState = buildFleetProjectOperatorState(item)
	return item, workers
}

func buildFleetProjectOperatorState(project fleetProjectState) fleetOperatorState {
	if strings.TrimSpace(project.Error) != "" {
		return fleetOperatorState{
			Kind:       "error",
			Tone:       "error",
			Label:      "State error",
			Summary:    truncateFleetOperatorText(project.Error, 120),
			NextAction: "Fix the project state/config load error before Maestro can supervise it.",
		}
	}
	if project.NeedsAttention > 0 {
		state := fleetOperatorState{
			Kind:       "attention",
			Tone:       "attention",
			Label:      "Needs attention",
			Summary:    fmt.Sprintf("%d worker item(s) need operator review.", project.NeedsAttention),
			NextAction: "Open the worker detail and resolve the first blocking reason.",
		}
		if len(project.Attention) > 0 {
			worker := project.Attention[0]
			state.Session = worker.Slot
			state.IssueNumber = worker.IssueNumber
			state.IssueURL = firstNonEmpty(worker.IssueURL, githubIssueURL(project.Repo, worker.IssueNumber))
			state.PRNumber = worker.PRNumber
			state.PRURL = firstNonEmpty(worker.PRURL, githubPRURL(project.Repo, worker.PRNumber))
			if reason := strings.TrimSpace(worker.StatusReason); reason != "" {
				state.Summary = truncateFleetOperatorText(reason, 150)
			}
			if next := strings.TrimSpace(worker.NextAction); next != "" {
				state.NextAction = truncateFleetOperatorText(next, 150)
			}
		}
		return state
	}
	if project.Freshness.Stale {
		summary := strings.TrimSpace(project.Freshness.Reason)
		if summary == "" {
			summary = "Project snapshot is stale."
		}
		return fleetOperatorState{
			Kind:       "stale",
			Tone:       "warn",
			Label:      "Stale",
			Summary:    summary,
			NextAction: "Check the project supervisor service and state writer.",
		}
	}
	if project.Running > 0 {
		state := fleetOperatorState{
			Kind:    "working",
			Tone:    "busy",
			Label:   "Working",
			Summary: fmt.Sprintf("%d/%d worker slot(s) active.", project.Running, project.MaxParallel),
		}
		if len(project.Active) > 0 {
			worker := project.Active[0]
			state.Session = worker.Slot
			state.IssueNumber = worker.IssueNumber
			state.IssueURL = firstNonEmpty(worker.IssueURL, githubIssueURL(project.Repo, worker.IssueNumber))
			state.PRNumber = worker.PRNumber
			state.PRURL = firstNonEmpty(worker.PRURL, githubPRURL(project.Repo, worker.PRNumber))
			if worker.IssueNumber > 0 {
				state.Summary = fmt.Sprintf("%s is working on issue #%d.", worker.Slot, worker.IssueNumber)
			}
		}
		return state
	}
	if state, ok := fleetOperatorStateFromSupervisor(project); ok {
		return state
	}
	if project.PROpen > 0 {
		state := fleetOperatorState{
			Kind:       "monitoring_pr",
			Tone:       "busy",
			Label:      "Monitoring PR",
			Summary:    fmt.Sprintf("%d PR(s) in review/merge gate; no code worker is expected right now.", project.PROpen),
			NextAction: "Wait for checks/review; Maestro should merge or respawn only if gates change.",
		}
		for _, worker := range append(append([]sessionInfo{}, project.Active...), project.Attention...) {
			if worker.PRNumber > 0 {
				state.Session = worker.Slot
				state.IssueNumber = worker.IssueNumber
				state.IssueURL = firstNonEmpty(worker.IssueURL, githubIssueURL(project.Repo, worker.IssueNumber))
				state.PRNumber = worker.PRNumber
				state.PRURL = firstNonEmpty(worker.PRURL, githubPRURL(project.Repo, worker.PRNumber))
				break
			}
		}
		return state
	}
	if !project.Outcome.Configured {
		return fleetOperatorState{
			Kind:       "outcome_missing",
			Tone:       "warn",
			Label:      "Outcome missing",
			Summary:    "No outcome brief is configured, so Maestro cannot prove hands-off success.",
			NextAction: "Add an outcome brief for this project before expecting reliable unattended development.",
		}
	}
	q := project.QueueSnapshot
	if q == nil {
		return fleetOperatorState{Kind: "idle", Tone: "muted", Label: "Idle", Summary: "No queue snapshot is available yet."}
	}
	if q.Open == 0 {
		return fleetOperatorState{Kind: "idle", Tone: "healthy", Label: "Idle", Summary: "No open issues are available."}
	}
	if q.Eligible > 0 {
		state := fleetOperatorState{
			Kind:       "pending_dispatch",
			Tone:       "busy",
			Label:      "Dispatch pending",
			Summary:    fmt.Sprintf("%d eligible issue(s); waiting for the supervisor to start a worker.", q.Eligible),
			NextAction: "A worker should start on the next supervisor cycle; escalate if this exceeds the dispatch SLA.",
		}
		if q.SelectedCandidate != nil && q.SelectedCandidate.Number > 0 {
			state.IssueNumber = q.SelectedCandidate.Number
			state.IssueURL = githubIssueURL(project.Repo, q.SelectedCandidate.Number)
			state.Summary = fmt.Sprintf("Issue #%d is selected for the next worker.", q.SelectedCandidate.Number)
		}
		return state
	}
	summary := strings.TrimSpace(q.IdleReason)
	if summary == "" {
		summary = "Open issues exist, but none are runnable under the current policy."
	}
	return fleetOperatorState{
		Kind:       "queue_blocked",
		Tone:       "muted",
		Label:      "Held by policy",
		Summary:    summary,
		NextAction: "Change labels/dependencies/project status if these issues should run now.",
	}
}

func fleetOperatorStateFromSupervisor(project fleetProjectState) (fleetOperatorState, bool) {
	latest := project.Supervisor.Latest
	if latest == nil {
		return fleetOperatorState{}, false
	}
	action := strings.TrimSpace(latest.RecommendedAction)
	summary := strings.TrimSpace(latest.Summary)
	target := latest.Target
	operator := fleetOperatorState{}
	switch action {
	case "monitor_open_pr", "approve_merge":
		operator = fleetOperatorState{
			Kind:       "monitoring_pr",
			Tone:       "busy",
			Label:      "Monitoring PR",
			Summary:    firstNonEmpty(summary, "A PR is in checks/review/merge gate; no code worker is expected right now."),
			NextAction: "Wait for checks and review gates, then merge or respawn from feedback.",
		}
	case "spawn_worker":
		operator = fleetOperatorState{
			Kind:       "pending_dispatch",
			Tone:       "busy",
			Label:      "Dispatch pending",
			Summary:    firstNonEmpty(summary, "Supervisor selected an issue and should start a worker."),
			NextAction: "A worker should start on the next supervisor cycle; escalate if this exceeds the dispatch SLA.",
		}
	case "wait_for_worker":
		return fleetOperatorState{Kind: "working", Tone: "busy", Label: "Working", Summary: firstNonEmpty(summary, "Supervisor is waiting for a worker to finish.")}, true
	default:
		if project.QueueSnapshot != nil && project.QueueSnapshot.SelectedCandidate != nil && project.QueueSnapshot.SelectedCandidate.Number > 0 && project.QueueSnapshot.Eligible > 0 {
			operator = fleetOperatorState{
				Kind:       "pending_dispatch",
				Tone:       "busy",
				Label:      "Dispatch pending",
				Summary:    fmt.Sprintf("Issue #%d is selected for the next worker.", project.QueueSnapshot.SelectedCandidate.Number),
				NextAction: "A worker should start on the next supervisor cycle; escalate if this exceeds the dispatch SLA.",
			}
			target = &state.SupervisorTarget{Issue: project.QueueSnapshot.SelectedCandidate.Number}
		} else {
			return fleetOperatorState{}, false
		}
	}
	operator = applyFleetOperatorTarget(project, operator, target)
	return operator, true
}

func applyFleetOperatorTarget(project fleetProjectState, operator fleetOperatorState, target *state.SupervisorTarget) fleetOperatorState {
	if target == nil {
		return operator
	}
	operator.IssueNumber = target.Issue
	operator.IssueURL = githubIssueURL(project.Repo, target.Issue)
	operator.PRNumber = target.PR
	operator.PRURL = githubPRURL(project.Repo, target.PR)
	operator.Session = target.Session
	return operator
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func truncateFleetOperatorText(value string, limit int) string {
	value = strings.TrimSpace(strings.Join(strings.Fields(value), " "))
	if limit <= 0 || len([]rune(value)) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit-3]) + "..."
}

func isFleetWorkerDefaultVisible(worker sessionInfo) bool {
	return worker.NeedsAttention || worker.Live
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
		DisplayStatus:     worker.DisplayStatus,
		StatusReason:      worker.StatusReason,
		NextAction:        worker.NextAction,
		NeedsAttention:    worker.NeedsAttention,
		Live:              worker.Live,
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
	case state.ApprovalStatusSuperseded:
		return 1
	case state.ApprovalStatusStale:
		return 2
	case state.ApprovalStatusApproved:
		return 3
	case state.ApprovalStatusRejected:
		return 4
	default:
		return 5
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
	if r.URL.Path != "/" && r.URL.Path != "/fleet" {
		http.NotFound(w, r)
		return
	}
	body, err := renderFleetDashboardHTML(s.snapshot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, body)
}

var fleetDashboardHTML = web.MustReadTemplate("fleet.html")
var fleetApprovalAuditHTML = web.MustReadTemplate("approvals-audit.html")

func renderFleetDashboardHTML(snapshot fleetResponse) (string, error) {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("marshal fleet dashboard initial state: %w", err)
	}
	body := strings.NewReplacer(
		"{{FLEET_PROJECT_RAIL_ROWS}}", renderFleetProjectRailRows(snapshot.Projects),
		"{{FLEET_PROJECT_RAIL_SUMMARY}}", html.EscapeString(fleetProjectRailSummary(snapshot.Projects)),
		"{{FLEET_INITIAL_STATE}}", string(data),
	).Replace(fleetDashboardHTML)
	return body, nil
}

func (s *FleetServer) handleFleetApprovalAudit(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/approvals/audit" {
		http.NotFound(w, r)
		return
	}
	body, err := renderFleetApprovalAuditHTML(s.snapshot())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, body)
}

func renderFleetApprovalAuditHTML(snapshot fleetResponse) (string, error) {
	historical := historicalFleetApprovals(snapshot.Approvals)
	body := strings.NewReplacer(
		"{{APPROVAL_AUDIT_SUBTITLE}}", html.EscapeString(approvalAuditSubtitle(snapshot)),
		"{{APPROVAL_AUDIT_SUMMARY}}", html.EscapeString(approvalAuditSummary(historical)),
		"{{APPROVAL_AUDIT_ROWS}}", renderFleetApprovalAuditRows(historical),
	).Replace(fleetApprovalAuditHTML)
	return body, nil
}

func historicalFleetApprovals(items []fleetApprovalState) []fleetApprovalState {
	out := make([]fleetApprovalState, 0, len(items))
	for _, item := range items {
		if state.ApprovalStatus(item.Status) != state.ApprovalStatusPending {
			out = append(out, item)
		}
	}
	return out
}

func approvalAuditSubtitle(snapshot fleetResponse) string {
	return fmt.Sprintf("%d configured projects · %d active pending approvals", snapshot.Summary.Projects, snapshot.Summary.ApprovalsPending)
}

func approvalAuditSummary(items []fleetApprovalState) string {
	if len(items) == 0 {
		return "No historical approvals recorded."
	}
	counts := make(map[string]int)
	for _, item := range items {
		counts[item.Status]++
	}
	return approvalHistoryCountTextForAudit(counts, len(items))
}

func approvalHistoryCountTextForAudit(counts map[string]int, historicalCount int) string {
	known := counts[string(state.ApprovalStatusSuperseded)] + counts[string(state.ApprovalStatusStale)] + counts[string(state.ApprovalStatusApproved)] + counts[string(state.ApprovalStatusRejected)]
	parts := make([]string, 0, 5)
	appendPart := func(count int, label string) {
		if count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count, label))
		}
	}
	appendPart(counts[string(state.ApprovalStatusSuperseded)], "superseded")
	appendPart(counts[string(state.ApprovalStatusStale)], "stale")
	appendPart(counts[string(state.ApprovalStatusApproved)], "approved")
	appendPart(counts[string(state.ApprovalStatusRejected)], "rejected")
	if other := historicalCount - known; other > 0 {
		appendPart(other, "other")
	}
	if len(parts) == 0 {
		return "No historical approvals"
	}
	return strings.Join(parts, " · ")
}

func renderFleetApprovalAuditRows(items []fleetApprovalState) string {
	if len(items) == 0 {
		return `<div class="empty approval-empty approval-audit-empty">No historical approvals have been recorded yet.</div>`
	}
	var b strings.Builder
	for _, item := range items {
		b.WriteString(renderFleetApprovalCard(item, true))
	}
	return b.String()
}

func renderFleetApprovalCard(approval fleetApprovalState, muted bool) string {
	project := html.EscapeString(firstNonEmpty(approval.ProjectName, "-"))
	id := html.EscapeString(firstNonEmpty(approval.ID, "-"))
	action := html.EscapeString(actionLabelServer(firstNonEmpty(approval.Action, "-")))
	createdAge := html.EscapeString(firstNonEmpty(approval.CreatedAge, "-"))
	updatedAge := html.EscapeString(firstNonEmpty(approval.UpdatedAge, "-"))
	summary := html.EscapeString(firstNonEmpty(approval.Summary, "No summary recorded."))
	risk := html.EscapeString(supervisorRiskLabelServer(firstNonEmpty(approval.Risk, "-")))
	sessionStatus := ""
	if strings.TrimSpace(approval.SessionStatus) != "" {
		sessionStatus = `<span>Status ` + html.EscapeString(approval.SessionStatus) + `</span>`
	}
	classes := []string{"approval-card", "approval-" + cssTokenServer(approval.Status)}
	if muted {
		classes = append(classes, "approval-card-muted")
	}
	return `<article class="` + strings.Join(classes, " ") + `" title="` + summary + `">` +
		`<div class="approval-project"><strong title="` + project + `">` + linkHTMLServer(approval.DashboardURL, project) + `</strong>` +
		`<div class="approval-meta"><span title="` + id + `">` + id + `</span></div></div>` +
		`<div class="approval-action"><strong title="` + action + `">` + action + `</strong>` +
		`<div class="approval-meta"><span class="` + approvalStatusClassServer(approval.Status) + `">` + html.EscapeString(firstNonEmpty(approval.Status, "unknown")) + `</span></div></div>` +
		`<div class="approval-target">` + renderFleetApprovalTargetHTML(approval) + sessionStatus + `</div>` +
		`<div class="approval-main"><div class="approval-age"><span>Created ` + createdAge + ` ago</span><span>Updated ` + updatedAge + ` ago</span></div>` +
		`<div class="approval-risk"><span>` + risk + `</span></div>` +
		`<div class="approval-summary">` + summary + `</div></div>` +
		`</article>`
}

func renderFleetApprovalTargetHTML(approval fleetApprovalState) string {
	parts := make([]string, 0, 3)
	if approval.IssueNumber > 0 {
		parts = append(parts, linkHTMLServer(approval.IssueURL, fmt.Sprintf("Issue #%d", approval.IssueNumber)))
	}
	if approval.PRNumber > 0 {
		parts = append(parts, linkHTMLServer(approval.PRURL, fmt.Sprintf("PR #%d", approval.PRNumber)))
	}
	if strings.TrimSpace(approval.Session) != "" {
		parts = append(parts, `<span>Session `+html.EscapeString(approval.Session)+`</span>`)
	}
	if len(parts) == 0 {
		return `<span class="empty">No target</span>`
	}
	return strings.Join(parts, " ")
}

func approvalStatusClassServer(status string) string {
	return "pill a-" + cssTokenServer(status)
}

func actionLabelServer(action string) string {
	switch strings.TrimSpace(firstNonEmpty(action, "-")) {
	case "none":
		return "Skip tick"
	case "monitor_open_pr":
		return "Watch PR"
	case "approve_merge":
		return "Merge PR"
	case "spawn_worker":
		return "Start worker"
	case "label_issue_ready":
		return "Mark issue ready"
	case "review_retry_exhausted":
		return "Review retry-exhausted work"
	case "check_outcome_health":
		return "Check runtime health"
	case "wait_for_running_worker", "wait_for_worker":
		return "Wait for worker"
	case "wait_for_capacity":
		return "Wait for free slot"
	case "wait_for_ordered_queue":
		return "Wait for queue order"
	default:
		return strings.ReplaceAll(strings.TrimSpace(firstNonEmpty(action, "-")), "_", " ")
	}
}

func supervisorRiskLabelServer(risk string) string {
	switch strings.TrimSpace(firstNonEmpty(risk, "-")) {
	case "safe":
		return "Safe recommendation"
	case "mutating":
		return "Mutating action"
	case "approval_gated":
		return "Approval required"
	default:
		return strings.ReplaceAll(strings.TrimSpace(firstNonEmpty(risk, "-")), "_", " ")
	}
}

func cssTokenServer(value string) string {
	value = strings.ToLower(strings.TrimSpace(firstNonEmpty(value, "unknown")))
	if value == "" {
		return "unknown"
	}
	var b strings.Builder
	lastUnderscore := false
	for _, r := range value {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "unknown"
	}
	return out
}

func linkHTMLServer(url, label string) string {
	text := html.EscapeString(firstNonEmpty(label, "-"))
	href := strings.TrimSpace(url)
	if href == "" {
		return text
	}
	return `<a href="` + html.EscapeString(href) + `" target="_blank" rel="noreferrer">` + text + `</a>`
}

func fleetProjectRailSummary(projects []fleetProjectState) string {
	if len(projects) == 0 {
		return "No configured projects."
	}
	active := 0
	attention := 0
	for _, project := range projects {
		if fleetOperatorStateIsActive(project.OperatorState.Kind) {
			active++
		}
		attention += project.NeedsAttention
	}
	return fmt.Sprintf("%d project%s · %d active · %d attention", len(projects), pluralSuffix(len(projects)), active, attention)
}

func renderFleetProjectRailRows(projects []fleetProjectState) string {
	if len(projects) == 0 {
		return `<tr class="project-rail-empty"><td colspan="7" class="empty">No configured projects are available in this fleet.</td></tr>`
	}
	var b strings.Builder
	for _, project := range projects {
		b.WriteString(renderFleetProjectRailRow(project))
	}
	return b.String()
}

func renderFleetProjectRailRow(project fleetProjectState) string {
	rowClasses := []string{"project-rail-row", fleetProjectRailStateClass(project)}
	if fleetProjectUnconfigured(project) {
		rowClasses = append(rowClasses, "project-row--unconfigured")
	}
	rowClass := strings.Join(rowClasses, " ")
	return `<tr class="` + html.EscapeString(rowClass) + `">` +
		`<td class="project-rail-project">` + renderFleetProjectIdentity(project) + `</td>` +
		`<td class="project-rail-state-cell">` + renderFleetProjectRailState(project) + `</td>` +
		`<td class="project-rail-queue-cell">` + renderFleetProjectRailQueue(project) + `</td>` +
		`<td class="project-rail-pr-cell">` + renderFleetProjectRailPR(project) + `</td>` +
		`<td class="project-rail-outcome-cell">` + renderFleetProjectRailOutcome(project) + `</td>` +
		`<td class="project-rail-freshness-cell">` + renderFleetProjectRailFreshness(project) + `</td>` +
		`<td class="project-rail-links-cell">` + renderFleetProjectRailLinks(project) + `</td>` +
		`</tr>`
}

func renderFleetProjectIdentity(project fleetProjectState) string {
	name := strings.TrimSpace(project.Name)
	if name == "" {
		name = "project"
	}
	primary := html.EscapeString(name)
	if strings.TrimSpace(project.DashboardURL) != "" {
		primary = `<a href="` + html.EscapeString(project.DashboardURL) + `" target="_blank" rel="noreferrer">` + primary + `</a>`
	}
	repo := strings.TrimSpace(project.Repo)
	if repo == "" {
		repo = strings.TrimSpace(project.ConfigPath)
	}
	return `<div class="rail-project-name">` + primary + `</div>` +
		`<div class="rail-project-repo" title="` + html.EscapeString(repo) + `">` + html.EscapeString(repo) + `</div>`
}

func renderFleetProjectRailState(project fleetProjectState) string {
	if fleetProjectUnconfigured(project) {
		parts := []string{
			`<span class="pill rail-state-unconfigured">setup</span>`,
			`<div class="rail-subline" title="No outcome brief configured">No outcome brief configured</div>`,
			`<div class="rail-note rail-setup-link">Set up &rarr;</div>`,
		}
		if project.Error != "" {
			parts = append(parts, `<div class="rail-alert" title="`+html.EscapeString(project.Error)+`">State error</div>`)
		}
		if project.Freshness.Stale {
			parts = append(parts, `<div class="rail-warn">Stale snapshot</div>`)
		}
		return strings.Join(parts, "")
	}

	operator := project.OperatorState
	label := fleetProjectStateLabel(project)
	summary := strings.TrimSpace(operator.Summary)
	if summary == "" {
		summary = fmt.Sprintf("%d/%d worker process(es) running.", project.Running, project.MaxParallel)
	}
	parts := []string{
		`<span class="pill ` + html.EscapeString(fleetProjectStatePillClass(project)) + `">` + html.EscapeString(label) + `</span>`,
		`<div class="rail-subline" title="` + html.EscapeString(summary) + `">` + html.EscapeString(summary) + `</div>`,
	}
	if next := strings.TrimSpace(operator.NextAction); next != "" {
		parts = append(parts, `<div class="rail-note" title="`+html.EscapeString(next)+`">Next: `+html.EscapeString(next)+`</div>`)
	}
	if project.Error != "" {
		parts = append(parts, `<div class="rail-alert" title="`+html.EscapeString(project.Error)+`">State error</div>`)
	}
	if project.Freshness.Stale && operator.Kind != "stale" {
		parts = append(parts, `<div class="rail-warn">Stale snapshot</div>`)
	}
	return strings.Join(parts, "")
}

func renderFleetProjectRailQueue(project fleetProjectState) string {
	q := project.QueueSnapshot
	if q == nil {
		return `<span class="empty">No queue snapshot</span>`
	}
	parts := []string{
		fmt.Sprintf("open=%d", q.Open),
		fmt.Sprintf("eligible=%d", q.Eligible),
		fmt.Sprintf("excluded=%d", q.Excluded),
		fmt.Sprintf("held/meta=%d", q.Held),
		fmt.Sprintf("blocked-deps=%d", q.BlockedByDependency),
	}
	lines := []string{`<div class="rail-mainline">` + html.EscapeString(strings.Join(parts, " · ")) + `</div>`}
	if q.SelectedCandidate != nil && q.SelectedCandidate.Number > 0 {
		selected := fmt.Sprintf("selected #%d", q.SelectedCandidate.Number)
		if title := strings.TrimSpace(q.SelectedCandidate.Title); title != "" {
			selected += " " + title
		}
		lines = append(lines, `<div class="rail-subline">`+html.EscapeString(selected)+`</div>`)
	}
	if idleReason := strings.TrimSpace(q.IdleReason); idleReason != "" && project.Running == 0 {
		lines = append(lines, `<div class="rail-warn" title="`+html.EscapeString(idleReason)+`">`+html.EscapeString(idleReason)+`</div>`)
	}
	return strings.Join(lines, "")
}

func renderFleetProjectRailPR(project fleetProjectState) string {
	links := fleetProjectPRLinks(project, 3)
	if project.PROpen == 0 && len(links) == 0 {
		return `<span class="empty">No open PR</span>`
	}
	var b strings.Builder
	b.WriteString(`<div class="rail-mainline">`)
	b.WriteString(html.EscapeString(fmt.Sprintf("%d open", project.PROpen)))
	b.WriteString(`</div>`)
	if len(links) > 0 {
		b.WriteString(`<div class="rail-links">`)
		for i, link := range links {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(link)
		}
		b.WriteString(`</div>`)
	} else if url := fleetProjectPullsURL(project.Repo); url != "" {
		b.WriteString(`<div class="rail-links"><a href="` + html.EscapeString(url) + `" target="_blank" rel="noreferrer">Open PRs</a></div>`)
	}
	return b.String()
}

func fleetProjectPRLinks(project fleetProjectState, limit int) []string {
	if limit <= 0 {
		return nil
	}
	seen := map[int]struct{}{}
	links := make([]string, 0, limit)
	add := func(worker sessionInfo) {
		if worker.PRNumber <= 0 || len(links) >= limit {
			return
		}
		if _, ok := seen[worker.PRNumber]; ok {
			return
		}
		seen[worker.PRNumber] = struct{}{}
		url := strings.TrimSpace(worker.PRURL)
		if url == "" {
			url = githubPRURL(project.Repo, worker.PRNumber)
		}
		label := fmt.Sprintf("PR #%d", worker.PRNumber)
		if url == "" {
			links = append(links, html.EscapeString(label))
			return
		}
		links = append(links, `<a href="`+html.EscapeString(url)+`" target="_blank" rel="noreferrer">`+html.EscapeString(label)+`</a>`)
	}
	for _, worker := range project.Active {
		add(worker)
	}
	for _, worker := range project.Attention {
		add(worker)
	}
	return links
}

func renderFleetProjectRailOutcome(project fleetProjectState) string {
	if fleetProjectUnconfigured(project) {
		next := strings.TrimSpace(project.Outcome.NextAction)
		if next == "" {
			next = "Add an outcome brief to this project's Maestro config."
		}
		return `<div class="rail-subline rail-setup-copy" title="No outcome brief configured">No outcome brief configured</div>` +
			`<div class="rail-note rail-setup-link" title="` + html.EscapeString(next) + `">Set up &rarr;</div>`
	}

	health := strings.TrimSpace(project.Outcome.HealthState)
	if health == "" {
		health = outcome.HealthUnknown
	}
	goal := strings.TrimSpace(project.Outcome.Goal)
	if !project.Outcome.Configured || goal == "" {
		goal = "No outcome brief configured"
	}
	parts := []string{
		`<span class="pill outcome-` + html.EscapeString(fleetCSSClassToken(health)) + `">` + html.EscapeString(strings.ReplaceAll(health, "_", " ")) + `</span>`,
		`<div class="rail-subline" title="` + html.EscapeString(goal) + `">` + html.EscapeString(goal) + `</div>`,
	}
	if next := strings.TrimSpace(project.Outcome.NextAction); next != "" {
		parts = append(parts, `<div class="rail-note" title="`+html.EscapeString(next)+`">`+html.EscapeString(next)+`</div>`)
	}
	return strings.Join(parts, "")
}

func renderFleetProjectRailFreshness(project fleetProjectState) string {
	freshness := project.Freshness
	age := strings.TrimSpace(freshness.SnapshotAge)
	if age == "" {
		age = "No snapshot yet"
	} else {
		age = "Snapshot " + age + " ago"
	}
	details := make([]string, 0, 3)
	if freshness.StateUpdatedAt != "" {
		details = append(details, "State "+freshness.StateUpdatedAt)
	}
	if freshness.LogUpdatedAt != "" {
		details = append(details, "Logs "+freshness.LogUpdatedAt)
	}
	if freshness.Reason != "" {
		details = append(details, freshness.Reason)
	}
	return `<div class="rail-mainline" title="` + html.EscapeString(strings.Join(details, " · ")) + `">` + html.EscapeString(age) + `</div>`
}

func renderFleetProjectRailLinks(project fleetProjectState) string {
	links := make([]string, 0, 3)
	if fleetProjectUnconfigured(project) {
		if setupURL := strings.TrimSpace(project.DashboardURL); setupURL != "" {
			links = append(links, `<a class="setup-link" href="`+html.EscapeString(setupURL)+`" target="_blank" rel="noreferrer">Set up &rarr;</a>`)
		} else if setupURL := fleetProjectGitHubURL(project.Repo); setupURL != "" {
			links = append(links, `<a class="setup-link" href="`+html.EscapeString(setupURL)+`" target="_blank" rel="noreferrer">Set up &rarr;</a>`)
		}
	}
	if strings.TrimSpace(project.DashboardURL) != "" {
		links = append(links, `<a href="`+html.EscapeString(project.DashboardURL)+`" target="_blank" rel="noreferrer">Dashboard</a>`)
	}
	if url := fleetProjectGitHubURL(project.Repo); url != "" {
		links = append(links, `<a href="`+html.EscapeString(url)+`" target="_blank" rel="noreferrer">GitHub</a>`)
	}
	links = append(links, `<button type="button" class="link-button project-workers-link" data-project="`+html.EscapeString(project.Name)+`">Workers</button>`)
	return `<div class="rail-links">` + strings.Join(links, " ") + `</div>`
}

func fleetProjectStateLabel(project fleetProjectState) string {
	if fleetProjectUnconfigured(project) {
		return "setup"
	}
	if label := strings.TrimSpace(project.OperatorState.Label); label != "" {
		return label
	}
	return "Idle"
}

func fleetProjectStatePillClass(project fleetProjectState) string {
	if fleetProjectUnconfigured(project) {
		return "rail-state-unconfigured"
	}
	key := strings.TrimSpace(project.OperatorState.Kind)
	if key == "" {
		key = "idle"
	}
	return "rail-state-" + fleetCSSClassToken(key)
}

func fleetProjectRailStateClass(project fleetProjectState) string {
	if fleetProjectUnconfigured(project) {
		return "project-row-unconfigured"
	}
	key := strings.TrimSpace(project.OperatorState.Kind)
	if key == "" {
		key = "idle"
	}
	return "project-row-" + fleetCSSClassToken(key)
}

func fleetProjectUnconfigured(project fleetProjectState) bool {
	return !project.Outcome.Configured
}

func fleetProjectGitHubURL(repo string) string {
	repo = strings.TrimSpace(repo)
	if !validGitHubRepo(repo) {
		return ""
	}
	return "https://github.com/" + repo
}

func fleetProjectPullsURL(repo string) string {
	base := fleetProjectGitHubURL(repo)
	if base == "" {
		return ""
	}
	return base + "/pulls?q=is%3Apr+is%3Aopen"
}

func fleetCSSClassToken(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func pluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}
