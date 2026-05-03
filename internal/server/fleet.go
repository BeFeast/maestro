package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"sync"
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
	mux.HandleFunc("/api/v1/audit/log", s.handleFleetAuditLog)
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
	Tone           string `json:"tone"`
	Sentence       string `json:"sentence"`
	Project        string `json:"project,omitempty"`
	Kind           string `json:"kind,omitempty"`
	Reason         string `json:"reason,omitempty"`
	NextAction     string `json:"next_action,omitempty"`
	ActionRequired bool   `json:"action_required,omitempty"`
	IssueNumber    int    `json:"issue_number,omitempty"`
	IssueURL       string `json:"issue_url,omitempty"`
	PRNumber       int    `json:"pr_number,omitempty"`
	PRURL          string `json:"pr_url,omitempty"`
	Session        string `json:"session,omitempty"`
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
	DispatchFailures    int   `json:"dispatch_failures"`
	QueueBlocked        int   `json:"queue_blocked"`
	NoEligibleIssues    int   `json:"no_eligible_issues"`
	OutcomeMissing      int   `json:"outcome_missing"`
	OutcomeDrift        int   `json:"outcome_drift"`
	StaleWorkers        int   `json:"stale_workers"`
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
	PastSLA           bool                    `json:"past_sla,omitempty"`

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

type fleetAuditLogRequest struct {
	Actor   string `json:"actor"`
	Action  string `json:"action"`
	Target  string `json:"target,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Project string `json:"project,omitempty"`
}

type fleetAuditLogEntry struct {
	AuditID   string `json:"audit_id"`
	Timestamp string `json:"timestamp"`
	Actor     string `json:"actor"`
	Action    string `json:"action"`
	Target    string `json:"target,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Project   string `json:"project,omitempty"`
}

var fleetAuditLogMu sync.Mutex

var (
	fleetStaleAuditMu      sync.Mutex
	fleetStaleAuditEmitted = make(map[string]struct{})
)

func (s *FleetServer) handleFleetAuditLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req fleetAuditLogRequest
	if r.Body != nil {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("decode audit log request: %v", err))
			return
		}
	}
	actor := strings.TrimSpace(req.Actor)
	action := strings.TrimSpace(req.Action)
	if actor == "" || action == "" {
		writeError(w, http.StatusBadRequest, "actor and action are required")
		return
	}
	stateDir, err := s.fleetAuditLogStateDir(req.Project)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if stateDir == "" {
		writeError(w, http.StatusInternalServerError, "no state dir is available to record the audit entry")
		return
	}
	entry := fleetAuditLogEntry{
		AuditID:   newFleetAuditID(),
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Actor:     actor,
		Action:    action,
		Target:    strings.TrimSpace(req.Target),
		Reason:    strings.TrimSpace(req.Reason),
		Project:   strings.TrimSpace(req.Project),
	}
	if err := appendFleetAuditLogEntry(stateDir, entry); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("write audit log: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"audit_id": entry.AuditID})
}

func (s *FleetServer) fleetAuditLogStateDir(projectName string) (string, error) {
	projectName = strings.TrimSpace(projectName)
	if projectName != "" {
		project, ok := s.findProject(projectName)
		if !ok {
			return "", fmt.Errorf("unknown project %q", projectName)
		}
		if project.cfg != nil && strings.TrimSpace(project.cfg.StateDir) != "" {
			return project.cfg.StateDir, nil
		}
		return "", nil
	}
	for _, project := range s.projects {
		if project.cfg != nil && strings.TrimSpace(project.cfg.StateDir) != "" {
			return project.cfg.StateDir, nil
		}
	}
	return "", nil
}

func appendFleetAuditLogEntry(stateDir string, entry fleetAuditLogEntry) error {
	if strings.TrimSpace(stateDir) == "" {
		return fmt.Errorf("audit log state dir is empty")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(stateDir, "audit-log.jsonl")
	encoded, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	fleetAuditLogMu.Lock()
	defer fleetAuditLogMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return nil
}

func newFleetAuditID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("audit-%d", time.Now().UnixNano())
	}
	return "audit-" + hex.EncodeToString(buf[:])
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
	resp.OperatorBrief = buildFleetOperatorBrief(resp.Projects, resp.Approvals, now)
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
	if summary.Stale > 0 || summary.Errors > 0 || summary.NeedsAttention > 0 || summary.ApprovalsPending > 0 || summary.DispatchFailures > 0 || summary.OutcomeDrift > 0 || summary.NoEligibleIssues > 0 {
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
	items := summary.NeedsAttention + summary.ApprovalsPending + summary.Errors + summary.Stale + summary.DispatchFailures + summary.OutcomeDrift + summary.NoEligibleIssues
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
	case "dispatch_failure":
		summary.DispatchFailures++
	case "queue_blocked", "no_eligible_issues":
		summary.QueueBlocked++
		summary.NoEligibleIssues++
	case "outcome_missing":
		summary.OutcomeMissing++
	case "outcome_drift":
		summary.OutcomeDrift++
	case "stale_worker":
		summary.StaleWorkers++
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

func buildFleetOperatorBrief(projects []fleetProjectState, approvals []fleetApprovalState, now time.Time) fleetOperatorBrief {
	if len(projects) == 0 {
		return fleetOperatorBrief{Tone: "muted", Sentence: "Global brief: no projects are configured in this fleet."}
	}

	if approval := oldestPastSLAPendingFleetApproval(approvals, now); approval != nil {
		return fleetOperatorBrief{
			Tone:           "daemon-down",
			Sentence:       fleetActionRequiredSentence(approval.ProjectName, "Approval past SLA", approval.Summary, approval.IssueNumber, approval.PRNumber, approval.Session),
			Project:        approval.ProjectName,
			Kind:           "approval_pending",
			Reason:         truncateFleetOperatorText(approval.Summary, 150),
			NextAction:     "Pending approval is past the " + fleetApprovalSLAText() + " SLA. Approve or reject it now.",
			ActionRequired: true,
			IssueNumber:    approval.IssueNumber,
			IssueURL:       approval.IssueURL,
			PRNumber:       approval.PRNumber,
			PRURL:          approval.PRURL,
			Session:        approval.Session,
		}
	}

	if approval := highestPriorityPendingFleetApproval(approvals); approval != nil {
		return fleetOperatorBrief{
			Tone:           "attention",
			Sentence:       fleetActionRequiredSentence(approval.ProjectName, "Approval pending", approval.Summary, approval.IssueNumber, approval.PRNumber, approval.Session),
			Project:        approval.ProjectName,
			Kind:           "approval_pending",
			Reason:         truncateFleetOperatorText(approval.Summary, 150),
			NextAction:     "Approve or reject the pending supervisor approval after checking the target state.",
			ActionRequired: true,
			IssueNumber:    approval.IssueNumber,
			IssueURL:       approval.IssueURL,
			PRNumber:       approval.PRNumber,
			PRURL:          approval.PRURL,
			Session:        approval.Session,
		}
	}

	var action *fleetProjectState
	for i := range projects {
		project := &projects[i]
		if !fleetOperatorKindNeedsAction(project.OperatorState.Kind) {
			continue
		}
		if action == nil || fleetOperatorStatePriority(project.OperatorState.Kind) < fleetOperatorStatePriority(action.OperatorState.Kind) {
			action = project
		}
	}
	if action != nil {
		state := action.OperatorState
		brief := fleetOperatorBrief{
			Tone:           fleetActionTone(state.Tone),
			Project:        action.Name,
			Kind:           state.Kind,
			Reason:         state.Summary,
			NextAction:     state.NextAction,
			ActionRequired: true,
			IssueNumber:    state.IssueNumber,
			IssueURL:       state.IssueURL,
			PRNumber:       state.PRNumber,
			PRURL:          state.PRURL,
			Session:        state.Session,
		}
		brief.Sentence = fleetActionRequiredSentence(action.Name, state.Label, state.Summary, state.IssueNumber, state.PRNumber, state.Session)
		return brief
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
	parts := make([]string, 0, 3)
	if working > 0 {
		parts = append(parts, fleetCountPhrase(working, "project running work", "projects running work"))
	}
	if monitoring > 0 {
		parts = append(parts, fleetCountPhrase(monitoring, "project waiting for CI/review", "projects waiting for CI/review"))
	}
	if pending > 0 {
		parts = append(parts, fleetCountPhrase(pending, "project dispatch pending", "projects dispatch pending"))
	}
	if len(parts) == 0 {
		return fleetOperatorBrief{Tone: "healthy", Kind: "idle", Sentence: "Global brief: all projects are healthy idle; no operator action is needed right now."}
	}
	if attention > 0 {
		parts = append(parts, fleetCountPhrase(attention, "project with passive attention", "projects with passive attention"))
	}
	return fleetOperatorBrief{Tone: "busy", Kind: "active", Sentence: "Global brief: " + strings.Join(parts, "; ") + "; no operator action is needed right now."}
}

const fleetApprovalSLASeconds int64 = 30 * 60

func fleetApprovalSLAText() string {
	return "30m"
}

func approvalPastSLA(approval *fleetApprovalState, now time.Time) bool {
	if approval == nil {
		return false
	}
	if approval.CreatedAgeSeconds > fleetApprovalSLASeconds {
		return true
	}
	if !approval.createdAt.IsZero() && now.Sub(approval.createdAt) > time.Duration(fleetApprovalSLASeconds)*time.Second {
		return true
	}
	return false
}

func oldestPastSLAPendingFleetApproval(approvals []fleetApprovalState, now time.Time) *fleetApprovalState {
	var selected *fleetApprovalState
	for i := range approvals {
		approval := &approvals[i]
		if state.ApprovalStatus(approval.Status) != state.ApprovalStatusPending || !approvalPastSLA(approval, now) {
			continue
		}
		if selected == nil || fleetApprovalRecency(*approval).Before(fleetApprovalRecency(*selected)) {
			selected = approval
		}
	}
	return selected
}

func highestPriorityPendingFleetApproval(approvals []fleetApprovalState) *fleetApprovalState {
	var selected *fleetApprovalState
	for i := range approvals {
		approval := &approvals[i]
		if state.ApprovalStatus(approval.Status) != state.ApprovalStatusPending {
			continue
		}
		if selected == nil {
			selected = approval
			continue
		}
		approvalRank := fleetApprovalStatusRank(approval.Status)
		selectedRank := fleetApprovalStatusRank(selected.Status)
		if approvalRank < selectedRank || (approvalRank == selectedRank && fleetApprovalRecency(*approval).After(fleetApprovalRecency(*selected))) {
			selected = approval
		}
	}
	return selected
}

func fleetOperatorKindNeedsAction(kind string) bool {
	switch strings.TrimSpace(kind) {
	case "error", "dispatch_failure", "stale_worker", "attention", "stale", "outcome_drift", "no_eligible_issues", "queue_blocked":
		return true
	default:
		return false
	}
}

func fleetActionTone(tone string) string {
	switch strings.TrimSpace(tone) {
	case "error", "daemon-down":
		return "daemon-down"
	case "busy":
		return "busy"
	case "healthy":
		return "healthy"
	default:
		return "attention"
	}
}

func fleetActionRequiredSentence(project, label, reason string, issueNumber, prNumber int, session string) string {
	project = firstNonEmpty(project, "project")
	label = firstNonEmpty(label, "Operator action")
	reason = firstNonEmpty(reason, "Maestro needs an operator decision.")
	sentence := fmt.Sprintf("Global brief: action required in %s", project)
	if target := fleetBriefTargetPhrase(issueNumber, prNumber, session); target != "" {
		sentence += " on " + target
	}
	sentence += fmt.Sprintf(": %s. Reason: %s", label, reason)
	return sentence
}

func fleetBriefTargetPhrase(issueNumber, prNumber int, session string) string {
	parts := make([]string, 0, 3)
	if issueNumber > 0 {
		parts = append(parts, fmt.Sprintf("issue #%d", issueNumber))
	}
	if prNumber > 0 {
		parts = append(parts, fmt.Sprintf("PR #%d", prNumber))
	}
	if session = strings.TrimSpace(session); session != "" {
		parts = append(parts, "session "+session)
	}
	return strings.Join(parts, ", ")
}

func fleetOperatorStatePriority(kind string) int {
	switch strings.TrimSpace(kind) {
	case "error":
		return 0
	case "dispatch_failure":
		return 1
	case "stale_worker":
		return 2
	case "attention":
		return 3
	case "outcome_drift":
		return 4
	case "stale":
		return 5
	case "pending_dispatch":
		return 6
	case "working":
		return 7
	case "monitoring_pr":
		return 8
	case "no_eligible_issues", "queue_blocked":
		return 9
	case "outcome_missing":
		return 10
	case "idle":
		return 11
	default:
		return 12
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
	staleAudits := reconcileStaleSessions(cfg, st, now)
	staleSlots := make(map[string]state.StaleSessionAudit, len(staleAudits))
	for _, audit := range staleAudits {
		staleSlots[audit.Slot] = audit
	}
	if len(staleAudits) > 0 {
		recordStaleSessionAudits(cfg.StateDir, project.Name, staleAudits)
	}
	workers := make([]fleetWorkerState, 0, len(projectState.All))
	for _, worker := range projectState.All {
		worker.Actions = workerActionAffordances(item.ReadOnly, "/api/v1/fleet/actions", worker)
		if audit, isStale := staleSlots[worker.Slot]; isStale {
			worker.NeedsAttention = false
			if reason := strings.TrimSpace(audit.Reason); reason != "" {
				worker.StatusReason = "stale session reconciled: " + reason
			}
			worker.NextAction = "No action required: this session was reconciled as stale by the fleet API."
		}
		if worker.NeedsAttention {
			item.NeedsAttention++
			item.Attention = append(item.Attention, worker)
		}
		workers = append(workers, makeFleetWorkerState(item, worker))
		if _, isStale := staleSlots[worker.Slot]; isStale {
			continue
		}
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

func reconcileStaleSessions(cfg *config.Config, st *state.State, now time.Time) []state.StaleSessionAudit {
	if cfg == nil || st == nil {
		return nil
	}
	policy := state.StaleSessionPolicy{
		Enabled:                cfg.StaleSessionReconciler.IsEnabled(),
		IdleAfter:              time.Duration(cfg.StaleSessionReconciler.IdleAfter()) * time.Minute,
		RequireWorktreeMissing: cfg.StaleSessionReconciler.WorktreeMissingRequired(),
	}
	if !policy.Enabled {
		return nil
	}
	return st.ReconcileStaleSessions(now, policy, worktreeExistsOnDisk)
}

func worktreeExistsOnDisk(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

func recordStaleSessionAudits(stateDir, projectName string, audits []state.StaleSessionAudit) {
	if strings.TrimSpace(stateDir) == "" || len(audits) == 0 {
		return
	}
	project := strings.TrimSpace(projectName)
	for _, audit := range audits {
		key := project + "\x00" + audit.Slot + "\x00" + audit.Reason
		fleetStaleAuditMu.Lock()
		_, alreadyEmitted := fleetStaleAuditEmitted[key]
		if !alreadyEmitted {
			fleetStaleAuditEmitted[key] = struct{}{}
		}
		fleetStaleAuditMu.Unlock()
		if alreadyEmitted {
			continue
		}
		entry := fleetAuditLogEntry{
			AuditID:   newFleetAuditID(),
			Timestamp: audit.At.UTC().Format(time.RFC3339Nano),
			Actor:     "fleet-reconciler",
			Action:    "stale_session_reconciled",
			Target:    audit.Slot,
			Reason:    audit.Reason,
			Project:   project,
		}
		if err := appendFleetAuditLogEntry(stateDir, entry); err != nil {
			log.Printf("[fleet] stale-session audit write failed for %s: %v", audit.Slot, err)
			fleetStaleAuditMu.Lock()
			delete(fleetStaleAuditEmitted, key)
			fleetStaleAuditMu.Unlock()
		}
	}
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
	if state, ok := fleetDispatchFailureOperatorState(project); ok {
		return state
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
			worker := highestPriorityAttentionSession(project.Attention)
			if fleetSessionLooksStale(worker) {
				state.Kind = "stale_worker"
				state.Label = "Stale worker"
			}
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
	if state, ok := fleetOutcomeDriftOperatorState(project); ok {
		return state
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
		return fleetOperatorState{Kind: "idle", Tone: "healthy", Label: "Healthy idle", Summary: "No open issues are available."}
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
		Kind:       "no_eligible_issues",
		Tone:       "warn",
		Label:      "No eligible issues",
		Summary:    summary,
		NextAction: "Change labels/dependencies/project status if these issues should run now.",
	}
}

func highestPriorityAttentionSession(workers []sessionInfo) sessionInfo {
	if len(workers) == 0 {
		return sessionInfo{}
	}
	selected := workers[0]
	for _, worker := range workers[1:] {
		left := fleetSessionAttentionSeverity(worker)
		right := fleetSessionAttentionSeverity(selected)
		if left < right {
			selected = worker
			continue
		}
		if left == right && worker.StartedAt > selected.StartedAt {
			selected = worker
		}
	}
	return selected
}

func fleetSessionAttentionSeverity(worker sessionInfo) int {
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

func fleetSessionLooksStale(worker sessionInfo) bool {
	if state.SessionStatus(worker.Status) != state.StatusRunning {
		return false
	}
	text := strings.ToLower(worker.StatusReason + " " + worker.NextAction)
	return strings.Contains(text, "not alive") || strings.Contains(text, "no pid") || strings.Contains(text, "not produced new output") || strings.Contains(text, "silent worker") || strings.Contains(text, "stale") || strings.Contains(text, "timeout")
}

func fleetDispatchFailureOperatorState(project fleetProjectState) (fleetOperatorState, bool) {
	latest := project.Supervisor.Latest
	if latest == nil {
		return fleetOperatorState{}, false
	}
	if strings.TrimSpace(latest.Status) != "failed" && strings.TrimSpace(latest.ErrorClass) == "" {
		return fleetOperatorState{}, false
	}
	operator := fleetOperatorState{
		Kind:       "dispatch_failure",
		Tone:       "error",
		Label:      "Dispatch failure",
		Summary:    firstNonEmpty(latest.Summary, "Supervisor dispatch or queue action failed."),
		NextAction: fleetDispatchFailureNextAction(latest.ErrorClass),
	}
	return applyFleetOperatorTarget(project, operator, latest.Target), true
}

func fleetDispatchFailureNextAction(errorClass string) string {
	switch strings.TrimSpace(errorClass) {
	case "github_auth":
		return "Fix GitHub authentication/permissions, then rerun the project supervisor."
	case "github_rate_limited":
		return "Wait for GitHub rate limits to clear, then rerun the project supervisor."
	case "github_not_found":
		return "Check the target issue/project exists and is accessible, then rerun the project supervisor."
	default:
		return "Fix the failed supervisor queue action, then rerun the project supervisor."
	}
}

func fleetOutcomeDriftOperatorState(project fleetProjectState) (fleetOperatorState, bool) {
	if !project.Outcome.Configured {
		return fleetOperatorState{}, false
	}
	if latest := project.Supervisor.Latest; latest != nil {
		if strings.TrimSpace(latest.RecommendedAction) == "check_outcome_health" {
			operator := fleetOperatorState{
				Kind:       "outcome_drift",
				Tone:       "attention",
				Label:      "Outcome drift",
				Summary:    firstNonEmpty(latest.Summary, "Runtime outcome health needs verification."),
				NextAction: firstNonEmpty(project.Outcome.NextAction, "Run the configured runtime/deploy healthcheck before dispatching more issue work."),
			}
			return applyFleetOperatorTarget(project, operator, latest.Target), true
		}
		for _, stuck := range latest.StuckStates {
			if stuck.Code != state.StuckNoOutcomeProgress {
				continue
			}
			operator := fleetOperatorState{
				Kind:       "outcome_drift",
				Tone:       "attention",
				Label:      "Outcome drift",
				Summary:    firstNonEmpty(stuck.Summary, "Runtime outcome health has not caught up with merged PRs."),
				NextAction: firstNonEmpty(stuck.RecommendedAction, project.Outcome.NextAction),
			}
			return applyFleetOperatorTarget(project, operator, stuck.Target), true
		}
	}

	health := strings.TrimSpace(project.Outcome.HealthState)
	switch health {
	case outcome.HealthFailing:
		return fleetOutcomeHealthState(project, health), true
	case outcome.HealthUnknown, outcome.HealthUnmonitored:
		if project.Outcome.MergedPRs > 0 {
			return fleetOutcomeHealthState(project, health), true
		}
	}
	return fleetOperatorState{}, false
}

func fleetOutcomeHealthState(project fleetProjectState, health string) fleetOperatorState {
	goal := firstNonEmpty(project.Outcome.Goal, project.Outcome.DesiredOutcome, "the configured runtime outcome")
	return fleetOperatorState{
		Kind:       "outcome_drift",
		Tone:       "attention",
		Label:      "Outcome drift",
		Summary:    fmt.Sprintf("Runtime outcome health is %s for %s.", strings.ReplaceAll(firstNonEmpty(health, outcome.HealthUnknown), "_", " "), goal),
		NextAction: firstNonEmpty(project.Outcome.NextAction, "Run the configured runtime/deploy healthcheck before dispatching more issue work."),
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
	case "check_outcome_health":
		operator = fleetOperatorState{
			Kind:       "outcome_drift",
			Tone:       "attention",
			Label:      "Outcome drift",
			Summary:    firstNonEmpty(summary, "Runtime outcome health needs verification before more issue throughput."),
			NextAction: firstNonEmpty(project.Outcome.NextAction, "Run the configured runtime/deploy healthcheck before dispatching more issue work."),
		}
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
	if approval.Status == state.ApprovalStatusPending {
		item.PastSLA = approvalPastSLA(&item, now)
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
		lt := fleetApprovalRecency(left)
		rt := fleetApprovalRecency(right)
		if !lt.Equal(rt) {
			return lt.After(rt)
		}
		if left.ProjectName != right.ProjectName {
			return left.ProjectName < right.ProjectName
		}
		return left.ID < right.ID
	})
}

func fleetApprovalRecency(approval fleetApprovalState) time.Time {
	if !approval.updatedAt.IsZero() {
		return approval.updatedAt
	}
	return approval.createdAt
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
	if approval.PastSLA {
		classes = append(classes, "approval-past-sla")
	}
	slaLabelAttr := ""
	if approval.PastSLA {
		slaLabelAttr = ` data-sla-label="` + html.EscapeString(fleetApprovalSLAText()) + `"`
	}
	return `<article class="` + strings.Join(classes, " ") + `" title="` + summary + `">` +
		`<div class="approval-project"><strong title="` + project + `">` + linkHTMLServer(approval.DashboardURL, project) + `</strong>` +
		`<div class="approval-meta"><span title="` + id + `">` + id + `</span></div></div>` +
		`<div class="approval-action"><strong title="` + action + `">` + action + `</strong>` +
		`<div class="approval-meta"` + slaLabelAttr + `><span class="` + approvalStatusClassServer(approval.Status) + `">` + html.EscapeString(firstNonEmpty(approval.Status, "unknown")) + `</span></div></div>` +
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
		return `<tr class="project-rail-empty"><td colspan="8" class="empty">No configured projects are available in this fleet.</td></tr>`
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
	detailID := "rail-detail-" + cssTokenServer(project.Name)
	toggleCell := `<td class="project-rail-toggle-cell">` +
		`<button type="button" class="project-rail-toggle" data-rail-toggle="` + html.EscapeString(detailID) + `" aria-expanded="false" aria-controls="` + html.EscapeString(detailID) + `" aria-label="Expand project detail">` +
		`<span class="project-rail-toggle-caret" aria-hidden="true">&#9656;</span>` +
		`</button></td>`
	mainRow := `<tr class="` + html.EscapeString(rowClass) + `" aria-controls="` + html.EscapeString(detailID) + `">` +
		toggleCell +
		`<td class="project-rail-project">` + renderFleetProjectIdentity(project) + `</td>` +
		`<td class="project-rail-state-cell">` + renderFleetProjectRailState(project) + `</td>` +
		`<td class="project-rail-queue-cell">` + renderFleetProjectRailQueue(project) + `</td>` +
		`<td class="project-rail-pr-cell">` + renderFleetProjectRailPR(project) + `</td>` +
		`<td class="project-rail-outcome-cell">` + renderFleetProjectRailOutcome(project) + `</td>` +
		`<td class="project-rail-freshness-cell">` + renderFleetProjectRailFreshness(project) + `</td>` +
		`<td class="project-rail-links-cell">` + renderFleetProjectRailLinks(project) + `</td>` +
		`</tr>`
	detailRow := `<tr class="project-rail-detail-row" id="` + html.EscapeString(detailID) + `" hidden>` +
		`<td colspan="8">` + renderFleetProjectRailDetail(project) + `</td>` +
		`</tr>`
	return mainRow + detailRow
}

func renderFleetProjectRailDetail(project fleetProjectState) string {
	parts := []string{
		`<div class="rail-detail-block rail-detail-queue"><div class="rail-detail-label">Queue</div>` + renderFleetProjectRailQueueDetail(project) + `</div>`,
		`<div class="rail-detail-block rail-detail-outcome"><div class="rail-detail-label">Outcome</div>` + renderFleetProjectRailOutcomeDetail(project) + `</div>`,
		`<div class="rail-detail-block rail-detail-decision"><div class="rail-detail-label">Last decision</div>` + renderFleetProjectRailDecisionDetail(project) + `</div>`,
	}
	return `<div class="project-rail-detail">` + strings.Join(parts, "") + `</div>`
}

func renderFleetProjectRailQueueDetail(project fleetProjectState) string {
	q := project.QueueSnapshot
	if q == nil {
		return `<div class="rail-detail-empty">No queue snapshot.</div>`
	}
	open := q.Open
	ready := q.Eligible
	held := q.Held
	readyPct := 0
	heldPct := 0
	if open > 0 {
		readyPct = (ready * 100) / open
		heldPct = (held * 100) / open
		if readyPct > 100 {
			readyPct = 100
		}
		if heldPct > 100 {
			heldPct = 100
		}
	}
	bar := `<div class="rail-detail-queue-bar" role="img" aria-label="Queue health">` +
		`<span class="rail-detail-queue-bar-segment ready" style="width:` + fmt.Sprintf("%d", readyPct) + `%"></span>` +
		`<span class="rail-detail-queue-bar-segment held" style="width:` + fmt.Sprintf("%d", heldPct) + `%"></span>` +
		`</div>`
	caption := fmt.Sprintf("%d ready · %d held · %d open", ready, held, open)
	idle := strings.TrimSpace(q.IdleReason)
	captionHTML := `<div class="rail-detail-queue-caption">` + html.EscapeString(caption) + `</div>`
	if idle != "" {
		captionHTML += `<div class="rail-detail-queue-idle">` + html.EscapeString(idle) + `</div>`
	}
	return bar + captionHTML
}

func renderFleetProjectRailOutcomeDetail(project fleetProjectState) string {
	if fleetProjectUnconfigured(project) {
		return `<div class="rail-detail-empty">No outcome brief configured.</div>`
	}
	o := project.Outcome
	health := strings.TrimSpace(o.HealthState)
	if health == "" {
		health = "unknown"
	}
	goal := strings.TrimSpace(o.Goal)
	if goal == "" {
		goal = "No outcome brief configured"
	}
	return `<span class="pill outcome-` + html.EscapeString(cssTokenServer(health)) + `">` + html.EscapeString(strings.ReplaceAll(health, "_", " ")) + `</span>` +
		`<div class="rail-detail-outcome-goal">` + html.EscapeString(goal) + `</div>`
}

func renderFleetProjectRailDecisionDetail(project fleetProjectState) string {
	sup := project.Supervisor
	if !sup.HasRun || sup.Latest == nil {
		return `<div class="rail-detail-empty">No supervisor decision yet.</div>`
	}
	latest := sup.Latest
	sentence := strings.TrimSpace(latest.OperatorSentence)
	if sentence == "" {
		sentence = supervisorOperatorSentence(latest.RecommendedAction, latest.Summary, latest.Target)
	}
	raw := strings.TrimSpace(latest.RecommendedAction)
	if raw == "" {
		raw = "none"
	}
	return `<div class="rail-detail-decision-sentence" title="Raw action: ` + html.EscapeString(raw) + `">` + html.EscapeString(sentence) + `</div>`
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
	mainline := fmt.Sprintf("%d ready", q.Eligible)
	subline := fmt.Sprintf("%d open", q.Open)
	if q.Held > 0 {
		subline = fmt.Sprintf("%d held · %d open", q.Held, q.Open)
	} else if q.SelectedCandidate != nil && q.SelectedCandidate.Number > 0 {
		subline = fmt.Sprintf("selected #%d", q.SelectedCandidate.Number)
	}
	return `<div class="rail-mainline">` + html.EscapeString(mainline) + `</div>` +
		`<div class="rail-subline" title="` + html.EscapeString(strings.TrimSpace(q.IdleReason)) + `">` + html.EscapeString(subline) + `</div>`
}

func renderFleetProjectRailPR(project fleetProjectState) string {
	if project.PROpen == 0 {
		return `<span class="empty">—</span>`
	}
	links := fleetProjectPRLinks(project, 3)
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
		if !worker.Live || strings.EqualFold(worker.Status, string(state.StatusDone)) {
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
		return `<div class="rail-subline rail-setup-copy" title="No outcome brief configured">No outcome brief configured</div>` +
			`<div class="rail-note rail-setup-link">Set up &rarr;</div>`
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
	url := strings.TrimSpace(project.DashboardURL)
	if url == "" {
		url = fleetProjectGitHubURL(project.Repo)
	}
	label := "Open"
	if fleetProjectUnconfigured(project) {
		label = "Set up"
	}
	if url == "" {
		return `<span class="empty">—</span>`
	}
	return `<div class="rail-open-link"><a href="` + html.EscapeString(url) + `" target="_blank" rel="noreferrer">` + html.EscapeString(label) + ` &rarr;</a></div>`
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
