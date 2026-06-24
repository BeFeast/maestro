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
	"net/url"
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
//
// DashboardURL is auto-derived from Name as the project-scoped MC route
// (`/project/<name>`). Per-project legacy dashboard ports were retired in #516,
// so any `dashboard_url` value supplied by a fleet.yaml is overwritten on
// load. The field is retained on the snapshot for backward-compatibility with
// external JSON consumers.
type FleetProject struct {
	Name         string `json:"name" yaml:"name"`
	ConfigPath   string `json:"config_path" yaml:"config"`
	DashboardURL string `json:"dashboard_url,omitempty" yaml:"dashboard_url"`

	cfg *config.Config

	// actionGH is the GitHub client used by handleFleetAction's safe-action
	// dispatcher when the project is not in read-only mode. nil disables
	// safe actions for this project. Tests inject a fake via SetActionGH.
	actionGH actionGitHubClient

	// board is the GitHub Project board refresher state used by the
	// /api/v1/fleet snapshot to surface the operator-glance WIP rollup
	// (#529). nil disables board surfacing for the project. Wired by
	// cmd/maestro via SetBoardClient when github_projects is enabled.
	board *boardState
}

// SetActionGH wires the per-project GitHub client used by the safe-action
// dispatcher. Call this on startup; nil leaves safe actions disabled for the
// project (handler falls through to 501).
func (p *FleetProject) SetActionGH(gh actionGitHubClient) {
	if p == nil {
		return
	}
	p.actionGH = gh
}

// Cfg exposes the loaded *config.Config so callers (cmd/maestro) that need
// per-project values (e.g. repo) without importing internal/server's
// internals can reach them. Returns nil if the project was created without
// a config (tests).
func (p *FleetProject) Cfg() *config.Config {
	if p == nil {
		return nil
	}
	return p.cfg
}

// NewFleetProject wraps an already-loaded config for in-process fleet serving.
//
// The dashboardURL parameter is retained for source-compat with callers and
// older fleet.yaml files but its value is ignored: every project is now
// reachable at the project-scoped MC route on the aggregator port (#516).
// DashboardURL on the returned FleetProject is auto-derived from name.
func NewFleetProject(name, configPath, dashboardURL string, cfg *config.Config) FleetProject {
	if strings.TrimSpace(name) == "" && cfg != nil {
		name = defaultFleetProjectName(cfg.Repo)
	}
	trimmedName := strings.TrimSpace(name)
	return FleetProject{
		Name:         trimmedName,
		ConfigPath:   strings.TrimSpace(configPath),
		DashboardURL: fleetProjectScopedPath(trimmedName),
		cfg:          cfg,
	}
}

// fleetProjectScopedPath returns the SPA route that zooms in on a project on
// the unified Mission Control aggregator (`/project/<name>`). Per-project
// dashboard ports were retired in #516; this is the canonical address.
func fleetProjectScopedPath(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return "/project/" + url.PathEscape(name)
}

func fleetProjectFocusedPath(name string, issue int, pr int, approvalID string) string {
	base := fleetProjectScopedPath(name)
	if base == "" {
		return ""
	}
	params := url.Values{}
	if strings.TrimSpace(approvalID) != "" {
		params.Set("approval", strings.TrimSpace(approvalID))
	}
	if pr > 0 {
		params.Set("pr", strconv.Itoa(pr))
	}
	if issue > 0 {
		params.Set("issue", strconv.Itoa(issue))
	}
	if encoded := params.Encode(); encoded != "" {
		return base + "?" + encoded
	}
	return base
}

func fleetApprovalsFocusedPath(approvalID string) string {
	approvalID = strings.TrimSpace(approvalID)
	if approvalID == "" {
		return "/approvals"
	}
	return "/approvals?id=" + url.QueryEscape(approvalID)
}

func fleetWorkersFocusedPath(projectName string, session string) string {
	params := url.Values{}
	if strings.TrimSpace(projectName) != "" {
		params.Set("project", strings.TrimSpace(projectName))
	}
	if strings.TrimSpace(session) != "" {
		params.Set("slot", strings.TrimSpace(session))
	}
	if encoded := params.Encode(); encoded != "" {
		return "/workers?" + encoded
	}
	return "/workers"
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
	taken := make(map[string]bool, len(file.Projects))
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
		if explicit := strings.TrimSpace(project.Name); explicit == "" {
			// Derived (basename) name: auto-disambiguate distinct repos that
			// share a basename (org-a/api vs org-b/api) the same way `maestro
			// daemon` does via UniqueFleetName, instead of hard-erroring on a
			// legitimate two-repo layout (#764). UniqueFleetName records the
			// chosen name into taken.
			project.Name = UniqueFleetName(cfg.Repo, taken)
		} else {
			// Explicit name from the fleet file: a real duplicate is operator
			// error, so still reject it. Case-sensitive to match
			// UniqueFleetName and findProject, which address project-scoped
			// routes/actions by exact Project.Name (#764) — "Api" and "api" are
			// distinct routes, not a collision.
			if taken[explicit] {
				return nil, fmt.Errorf("duplicate fleet project name %q", explicit)
			}
			taken[explicit] = true
			project.Name = explicit
		}
		if legacy := strings.TrimSpace(project.DashboardURL); legacy != "" {
			log.Printf("[fleet] project %q: dashboard_url %q in %s is deprecated and ignored — the project is reachable at the unified MC route %s (#516)",
				project.Name, legacy, path, fleetProjectScopedPath(project.Name))
		}
		project.DashboardURL = fleetProjectScopedPath(project.Name)
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
	// projectsMu guards projects so the daemon's diff-loop can add/remove
	// flows at runtime (#757) while HTTP handlers read the slice concurrently.
	// Reads go through projectsSnapshot; mutations through AddProject /
	// RemoveProject.
	projectsMu sync.RWMutex
	projects   []FleetProject
	host       string
	port       int
	readOnly   bool
	srv        *http.Server
	// authMu guards auth so the daemon can re-derive the fleet token when a
	// project is hot-added/removed/edited (#768) while HTTP handlers read it
	// concurrently. Writes go through SetAuth / reauthLocked; reads through
	// authChecker(). #768 makes SetAuth a RUNTIME write (AddProject/RemoveProject
	// re-derive auth), which without this lock would race the handlers reading
	// s.auth — a data race under `go test -race`.
	authMu sync.RWMutex
	// auth gates every mutating fleet endpoint (#487, write-path premortem
	// #4). When the resolved token is empty the checker is disabled and
	// behaviour is unchanged; when non-empty, /api/v1/fleet/actions,
	// /api/v1/fleet/approvals/{id}/{approve|reject}, and /api/v1/audit/log
	// require Authorization: Bearer <token>. Always accessed under authMu.
	auth authChecker
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

// projectsSnapshot returns a copy of the project slice under the read lock so
// callers can iterate without holding the lock across per-project work (state
// loads, board calls). The backing array is fresh, so a concurrent AddProject /
// RemoveProject can never mutate it mid-iteration.
func (s *FleetServer) projectsSnapshot() []FleetProject {
	s.projectsMu.RLock()
	defer s.projectsMu.RUnlock()
	out := make([]FleetProject, len(s.projects))
	copy(out, s.projects)
	return out
}

// AddProject registers a project at runtime (daemon hot-add, #757). A project
// whose Name already exists is replaced in place rather than duplicated, so the
// fleet's by-name addressing (findProject) stays unambiguous. Safe for
// concurrent use with the HTTP read paths.
//
// The fleet's shared auth token is re-derived from the resulting project set
// (#768): a hot-added project whose config carries server.auth.token_env starts
// enforcing auth on the mutating endpoints without a daemon restart. Editing a
// project's row goes through the same path (the daemon replaces the project in
// place on reload).
func (s *FleetServer) AddProject(p FleetProject) {
	if s == nil {
		return
	}
	s.projectsMu.Lock()
	defer s.projectsMu.Unlock()
	for i := range s.projects {
		if s.projects[i].Name == p.Name {
			s.projects[i] = p
			s.reauthLocked()
			return
		}
	}
	s.projects = append(s.projects, p)
	s.reauthLocked()
}

// RemoveProject drops the project with the given fleet name (daemon hot-remove,
// #757) and reports whether one was found. Safe for concurrent use with the
// HTTP read paths. The shared auth token is re-derived from the remaining set
// (#768) — removing the last token-bearing project disables auth again.
func (s *FleetServer) RemoveProject(name string) bool {
	if s == nil {
		return false
	}
	s.projectsMu.Lock()
	defer s.projectsMu.Unlock()
	for i := range s.projects {
		if s.projects[i].Name == name {
			s.projects = append(s.projects[:i], s.projects[i+1:]...)
			s.reauthLocked()
			return true
		}
	}
	return false
}

// reauthLocked re-derives the fleet's shared auth token from the current
// project set and stores it under authMu (#768). The caller MUST hold
// projectsMu (lock order projectsMu→authMu), so the project mutation and the
// auth derivation it implies are one atomic step as far as any concurrent
// handler is concerned.
func (s *FleetServer) reauthLocked() {
	s.setAuthChecker(newAuthChecker(FleetAuthFromProjects(s.projects)))
}

// SetAuth configures fleet-level auth from the operator's resolved
// ServerAuthConfig. Empty token leaves auth disabled (backward-compat).
// Safe to call before Start and at runtime (the handlers read the checker
// live, #768).
func (s *FleetServer) SetAuth(cfg config.ServerAuthConfig) {
	if s == nil {
		return
	}
	s.setAuthChecker(newAuthChecker(cfg))
}

// SetAuthForTest replaces the auth checker. Test-only helper.
func (s *FleetServer) SetAuthForTest(token, actorName string) {
	if s == nil {
		return
	}
	s.setAuthChecker(newAuthCheckerForTest(token, actorName))
}

// setAuthChecker stores the auth checker under authMu.
func (s *FleetServer) setAuthChecker(a authChecker) {
	s.authMu.Lock()
	s.auth = a
	s.authMu.Unlock()
}

// authChecker returns the live auth checker under the read lock. Every fleet
// handler reads through this so a runtime SetAuth / hot-add re-derivation is
// applied to the next request without a server rebuild and without racing the
// write (#768).
func (s *FleetServer) authChecker() authChecker {
	s.authMu.RLock()
	defer s.authMu.RUnlock()
	return s.auth
}

// buildHandler returns the fleet mux wrapped with the auth middleware.
// When auth.Required() is true (#616: exposed install posture), every
// route — JSON read endpoints, the SPA HTML, static assets, mutating
// POSTs — rejects unauthenticated requests with 401 before the inner
// handler runs. When auth is disabled (default LAN posture), the
// middleware is a pass-through.
//
// The middleware reads the live checker per request (#768) rather than
// capturing it at build time, so a hot-add that enables auth gates the read
// path too — not only the mutating endpoints (which read s.authChecker()
// directly).
func (s *FleetServer) buildHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/fleet/worker", s.handleFleetWorker)
	mux.HandleFunc("/api/v1/fleet", s.handleFleet)
	mux.HandleFunc("/api/v1/fleet/actions", s.handleFleetAction)
	mux.HandleFunc("/api/v1/fleet/approvals/", s.handleFleetApproval)
	mux.HandleFunc("/api/v1/audit/log", s.handleFleetAuditLog)
	mux.HandleFunc("/approvals/audit", s.handleFleetApprovalAudit)
	mux.Handle("/static/", web.StaticHandler())
	mux.HandleFunc("/", s.handleFleetDashboard)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authMiddleware(mux, s.authChecker()).ServeHTTP(w, r)
	})
}

// HandlerForTest exposes the wrapped handler so tests can exercise the
// auth middleware without spinning up an httptest server.
func (s *FleetServer) HandlerForTest() http.Handler { return s.buildHandler() }

// Start begins serving the fleet dashboard. It blocks until shutdown.
func (s *FleetServer) Start(ctx context.Context) error {
	ln, err := s.Listen()
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Listen binds the fleet's TCP port without serving. Splitting bind from serve
// lets the daemon detect an "address already in use" failure BEFORE it starts
// any project flow, so a bind error aborts startup immediately instead of
// blocking on stopAll's flow drain — which cannot interrupt a flow's in-flight,
// non-cancellable first RunOnce, hanging the startup-error return (#764 P2).
//
// Returns (nil, nil) when port == 0 (no web endpoint requested); Serve then
// no-ops on the nil listener. A non-nil error is a real bind failure.
func (s *FleetServer) Listen() (net.Listener, error) {
	if s.port == 0 {
		return nil, nil
	}
	host := strings.TrimSpace(s.host)
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(s.port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("fleet server bind %s: %w", addr, err)
	}
	return ln, nil
}

// Serve serves the fleet HTTP API on ln until ctx is cancelled, then gracefully
// shuts down. A nil ln (port 0) blocks on ctx and returns nil, matching the
// historical no-web behavior. Serve takes ownership of ln.
func (s *FleetServer) Serve(ctx context.Context, ln net.Listener) error {
	if ln == nil {
		<-ctx.Done()
		return nil
	}
	s.srv = &http.Server{
		Handler:      s.buildHandler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.srv.Shutdown(shutdownCtx)
	}()

	// #529: kick the per-project GitHub Project board refreshers; they exit
	// on ctx.Done(). No-op for projects without a configured board client.
	s.startBoardRefreshers(ctx)

	log.Printf("[fleet] listening on %s", ln.Addr())
	if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("fleet server: %w", err)
	}
	return nil
}

type fleetResponse struct {
	ReadOnly      bool                 `json:"read_only"`
	Version       string               `json:"version,omitempty"` // running maestro binary version (#698)
	RefreshedAt   string               `json:"refreshed_at"`
	NextAction    *fleetNextAction     `json:"next_action"`
	Verdict       fleetVerdict         `json:"verdict"`
	OperatorBrief fleetOperatorBrief   `json:"operator_brief"`
	Projects      []fleetProjectState  `json:"projects"`
	Summary       fleetSummary         `json:"summary"`
	Workers       []fleetWorkerState   `json:"workers"`
	Attention     []fleetWorkerState   `json:"attention"`
	Approvals     []fleetApprovalState `json:"approvals,omitempty"`
	// CostObservability is the fleet-wide token + USD rollup (#619).
	// The per-project block is also surfaced under each fleetProjectState
	// so the SPA can render both the hero ("today / 7d") and the
	// per-project drill-down without recomputing pricing client-side.
	CostObservability fleetGlobalCost `json:"cost_observability"`
}

// fleetNextAction names the single canonical operator action across the fleet.
// The selection algorithm is deterministic: items are bucketed into priority
// tiers P0..P3 and the oldest item by updated_at within the highest-occupied
// tier wins. picked_at echoes that underlying timestamp so the choice is
// stable across snapshots while the input is unchanged.
type fleetNextAction struct {
	Project   string `json:"project"`
	Kind      string `json:"kind"`
	TargetURL string `json:"target_url,omitempty"`
	Reason    string `json:"reason"`
	Priority  string `json:"priority"`
	PickedAt  string `json:"picked_at,omitempty"`

	// CTALabel is the verb-shaped button label the SPA header card
	// renders in place of the passive «Action required» text (#531).
	// Examples: "Approve PR #123", "Resolve stuck dispatch",
	// "Refresh stale snapshot". Empty when no action is required.
	CTALabel    string `json:"cta_label,omitempty"`
	PRNumber    int    `json:"pr_number,omitempty"`
	IssueNumber int    `json:"issue_number,omitempty"`
}

type fleetVerdict struct {
	Tone     string `json:"tone"`
	Sentence string `json:"sentence"`
	// Headline + Detail are the short, structured form the SPA hero
	// renders (issue #474). The legacy Sentence is preserved for
	// backward compatibility and as a tooltip / fallback. The SPA
	// prefers Headline+Detail when present.
	Headline string `json:"headline,omitempty"`
	Detail   string `json:"detail,omitempty"`
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
	Projects         int `json:"projects"`
	Stale            int `json:"stale"`
	Errors           int `json:"errors"`
	Active           int `json:"active"`
	MonitoringPR     int `json:"monitoring_pr"`
	DispatchPending  int `json:"dispatch_pending"`
	DispatchFailures int `json:"dispatch_failures"`
	QueueBlocked     int `json:"queue_blocked"`
	NoEligibleIssues int `json:"no_eligible_issues"`
	OutcomeMissing   int `json:"outcome_missing"`
	OutcomeDrift     int `json:"outcome_drift"`
	StaleWorkers     int `json:"stale_workers"`
	Running          int `json:"running"`
	PROpen           int `json:"pr_open"`
	// PRsOpen / WorkersRunning are truth-table mirrors expected by the
	// SPA (#566). PRsOpen extends pr_open with retry_exhausted /
	// failed / conflict_failed sessions whose PR is still open;
	// WorkersRunning mirrors `running` under the stable field name the
	// project card reads. Plain ints — never null.
	PRsOpen        int `json:"prs_open"`
	WorkersRunning int `json:"workers_running"`
	Failed         int `json:"failed"`
	Sessions       int `json:"sessions"`
	NeedsAttention int `json:"needs_attention"`
	// SelfResolving counts attention items that are convergence-bound
	// and need no operator action (e.g. retry_exhausted with a green PR
	// — the orchestrator auto-merges once the merge gate clears). See
	// issue #598: subtracted from NeedsAttention when computing the
	// fleet verdict tone so a self-resolving PR does not alarm with a
	// passive `Action required — p1` headline.
	SelfResolving       int   `json:"self_resolving,omitempty"`
	Approvals           int   `json:"approvals"`
	ApprovalsPending    int   `json:"approvals_pending"`
	ApprovalsActionable int   `json:"approvals_actionable,omitempty"`
	ApprovalsSuggestion int   `json:"approvals_suggestion,omitempty"`
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
	// EligibleRanked + SkippedCandidates carry the supervisor decision plane
	// (#720): the eligible set in real selection order and every skipped
	// candidate with its reason. Surfaced straight from the persisted
	// SupervisorDecision so Mission Control renders next/eligible/skipped
	// without any GitHub calls on the request path.
	EligibleRanked    []state.SupervisorIssueCandidate   `json:"eligible_ranked,omitempty"`
	SkippedCandidates []state.SupervisorSkippedCandidate `json:"skipped_candidates,omitempty"`
}

type fleetProjectState struct {
	Name               string `json:"name"`
	Repo               string `json:"repo"`
	ConfigPath         string `json:"config_path"`
	DashboardURL       string `json:"dashboard_url,omitempty"`
	StateDir           string `json:"state_dir,omitempty"`
	MaxParallel        int    `json:"max_parallel"`
	ReadOnly           bool   `json:"read_only"`
	DispatchSLASeconds int    `json:"dispatch_sla_seconds,omitempty"`

	// RestartRequired/RestartRequiredReason mirror the orchestrator's restart-required
	// signal (set when model.default / routing.* changed but cannot be hot-applied).
	RestartRequired       bool   `json:"restart_required,omitempty"`
	RestartRequiredReason string `json:"restart_required_reason,omitempty"`

	// Paused mirrors the first-class operator pause (#683): `maestro pause`
	// set the persisted flag, the orchestrator skips issue selection, and
	// in-flight workers finish normally. Surfaced so Mission Control and
	// the operator brief can tell an intentional pause from an outage.
	// Paused is a plain bool (never omitted) so the SPA reads an explicit
	// value for every project.
	Paused   bool      `json:"paused"`
	PausedAt time.Time `json:"paused_at,omitempty"`

	OperatorState fleetOperatorState `json:"operator_state"`
	Outcome       outcome.Status     `json:"outcome"`
	Summary       map[string]int     `json:"summary"`
	Running       int                `json:"running"`
	PROpen        int                `json:"pr_open"`
	// PRsOpen is the truthful count of open PRs for this project,
	// including retry_exhausted (or otherwise terminal) sessions whose
	// linked PR is still open and gate-blocked (#564). When the latest
	// supervisor decision reports ProjectState.OpenPRs (the GitHub
	// truth), that value is preferred over session-derived counts. The
	// field is a plain int so it is never null (#566).
	PRsOpen int `json:"prs_open"`
	// WorkersRunning mirrors Running with a stable field name expected
	// by the SPA project card (#566). It is the count of sessions in
	// StatusRunning. Always non-null.
	WorkersRunning int `json:"workers_running"`
	Failed         int `json:"failed"`
	Sessions       int `json:"sessions"`
	NeedsAttention int `json:"needs_attention"`
	// SelfResolving is the count of attention sessions on this project
	// that are convergence-bound (the orchestrator will resolve them
	// without operator action — see fleetSessionIsConvergenceBound and
	// issue #598). The SPA project card uses this to render a calm
	// "auto-merging" line instead of an alarming attention CTA.
	SelfResolving   int                   `json:"self_resolving,omitempty"`
	Active          []sessionInfo         `json:"active,omitempty"`
	Attention       []sessionInfo         `json:"attention,omitempty"`
	Approvals       []fleetApprovalState  `json:"approvals,omitempty"`
	ApprovalSummary map[string]int        `json:"approval_summary,omitempty"`
	Actions         []controlAction       `json:"actions,omitempty"`
	CloseCandidates []fleetCloseCandidate `json:"close_candidates,omitempty"`
	Supervisor      supervisorInfo        `json:"supervisor"`
	QueueSnapshot   *fleetQueueSnapshot   `json:"queue_snapshot,omitempty"`
	Freshness       fleetProjectFreshness `json:"freshness"`
	// ProjectBoard is the cached GitHub Project board snapshot (#529). nil
	// when the project has no github_projects configuration, or while the
	// background refresher has not yet produced its first result.
	ProjectBoard *fleetProjectBoard `json:"project_board,omitempty"`
	Error        string             `json:"error,omitempty"`

	// SupervisorPulse exposes the data the header verdict card uses to
	// render a positive liveness signal (issue #531): the last-cycle
	// timestamp (state.LastRunOnceAt), the configured poll interval,
	// the policy mode (cautious / read_only / …), and the last N
	// recommended_action verbs for the decision sparkline.
	SupervisorPulse fleetSupervisorPulse `json:"supervisor_pulse"`

	// BackendHealth is the cross-session per-backend availability snapshot
	// (#513 / #534). When a backend hits a provider rate-limit it goes to
	// state "cooldown" with RetryAfter set; the SPA renders this as a
	// header row («claude in cooldown until 21:00 UTC», «codex available»)
	// so an operator can see that a whole backend is paused and that
	// auto-recovery is on a known clock.
	BackendHealth map[string]state.BackendHealth `json:"backend_health,omitempty"`

	// BackendQuota is the per-backend subscription quota position (#704)
	// for backends with quota config: window/weekly percent used, reset
	// ETAs and whether dispatch is currently steered to fallbacks. The
	// SPA renders this as a gauge row next to the backend health pills.
	BackendQuota []fleetBackendQuota `json:"backend_quota,omitempty"`

	// CostObservability rolls the per-session token counters already
	// recorded by the orchestrator into today / 7d / lifetime token +
	// USD windows per backend and per issue (#619). Backends without
	// pricing configured render in the SPA as tokens only.
	CostObservability fleetCostObservability `json:"cost_observability"`

	// Epics is the epic-completion aggregate the supervisor stamped on
	// its latest decision (#650). The SPA renders per-epic progress
	// (children merged / total) and the "epic complete" badge when all
	// children are merged AND the configured outcome health gate is
	// healthy. Empty when the project has no open epic with parseable
	// children, or the supervisor has not run yet.
	Epics []state.EpicProgress `json:"epics,omitempty"`
	// EpicSummary is the operator-glance roll-up used by the project
	// card to render an "N epic(s) tracked, 1 complete" pill without
	// the SPA walking the Epics slice.
	EpicSummary fleetEpicSummary `json:"epic_summary"`

	// EffectiveConfig is the sanitized, parsed runtime config surfaced on
	// MC Settings (#622). It deliberately excludes raw YAML, commands,
	// auth, notification endpoints/tokens and local prompt paths.
	EffectiveConfig fleetEffectiveConfig `json:"effective_config"`
}

type fleetEffectiveConfig struct {
	ModelPolicy    fleetModelPolicy      `json:"model_policy"`
	MaxParallel    int                   `json:"max_parallel"`
	ReviewGate     string                `json:"review_gate"`
	Labels         fleetConfigLabels     `json:"labels"`
	Retention      fleetConfigRetention  `json:"retention"`
	CostCaps       fleetConfigCostCaps   `json:"cost_caps"`
	SupervisorGate fleetConfigSupervisor `json:"supervisor_gate"`
	ApprovalAction string                `json:"approval_action"`
}

type fleetModelPolicy struct {
	Default          string                        `json:"default"`
	FallbackBackends []string                      `json:"fallback_backends,omitempty"`
	Backends         []fleetEffectiveBackendConfig `json:"backends"`
	Routing          fleetEffectiveRoutingConfig   `json:"routing"`
}

type fleetEffectiveBackendConfig struct {
	Name             string  `json:"name"`
	Enabled          bool    `json:"enabled"`
	Provider         string  `json:"provider,omitempty"`
	Model            string  `json:"model,omitempty"`
	Variant          string  `json:"variant,omitempty"`
	Effort           string  `json:"effort,omitempty"`
	PromptMode       string  `json:"prompt_mode,omitempty"`
	NonAgentic       bool    `json:"non_agentic,omitempty"`
	PriceConfigured  bool    `json:"price_configured"`
	InputUSDPerMtok  float64 `json:"input_usd_per_mtok,omitempty"`
	OutputUSDPerMtok float64 `json:"output_usd_per_mtok,omitempty"`
}

type fleetEffectiveRoutingConfig struct {
	Mode            string `json:"mode,omitempty"`
	RouterModel     string `json:"router_model,omitempty"`
	RouterModelName string `json:"router_model_name,omitempty"`
}

type fleetConfigLabels struct {
	Issue        []string `json:"issue,omitempty"`
	Exclude      []string `json:"exclude,omitempty"`
	Ready        string   `json:"ready,omitempty"`
	Blocked      string   `json:"blocked,omitempty"`
	Excluded     []string `json:"supervisor_excluded,omitempty"`
	AllowTypes   []string `json:"allow_issue_types,omitempty"`
	Mission      []string `json:"mission,omitempty"`
	Completion   []string `json:"completion_required,omitempty"`
	Verification string   `json:"verification,omitempty"`
}

type fleetConfigRetention struct {
	Enabled            bool   `json:"enabled"`
	KeepLast           int    `json:"keep_last,omitempty"`
	MinAge             string `json:"min_age,omitempty"`
	ArchiveEnabled     bool   `json:"archive_enabled"`
	ArchiveFilePresent bool   `json:"archive_file_present"`
}

type fleetConfigCostCaps struct {
	WorkerMaxTokens          int      `json:"worker_max_tokens,omitempty"`
	WorkerSoftTokenThreshold *float64 `json:"worker_soft_token_threshold,omitempty"`
	BackendPricingConfigured int      `json:"backend_pricing_configured"`
	BackendPricingTotal      int      `json:"backend_pricing_total"`
}

type fleetConfigSupervisor struct {
	Mode                    string   `json:"mode,omitempty"`
	DryRun                  bool     `json:"dry_run,omitempty"`
	DispatchSLASeconds      int      `json:"dispatch_sla_seconds,omitempty"`
	SafeActions             []string `json:"safe_actions,omitempty"`
	ApprovalRequired        []string `json:"approval_required,omitempty"`
	AllowedActions          []string `json:"allowed_actions,omitempty"`
	ApprovalRequiredActions []string `json:"approval_required_actions,omitempty"`
	CompletionGatesActive   bool     `json:"completion_gates_active"`
	HandoffPlannerActive    bool     `json:"handoff_planner_active"`
	ReviewRepairActive      bool     `json:"review_repair_active"`
	ReviewRepairBackend     string   `json:"review_repair_backend,omitempty"`
	ReviewRepairMaxRetries  int      `json:"review_repair_max_retries,omitempty"`
}

type fleetCloseCandidate struct {
	IssueNumber int    `json:"issue_number"`
	IssueURL    string `json:"issue_url,omitempty"`
	PRNumber    int    `json:"pr_number,omitempty"`
	PRURL       string `json:"pr_url,omitempty"`
	Session     string `json:"session,omitempty"`
	FinishedAt  string `json:"finished_at,omitempty"`
}

// fleetEpicSummary is the operator-glance roll-up of the project's
// epic-completion aggregate (#650). Zero values render as a calm
// "no epics tracked" line in the SPA project card.
type fleetEpicSummary struct {
	Tracked          int `json:"tracked"`
	Complete         int `json:"complete"`
	InProgress       int `json:"in_progress"`
	ChildrenTotal    int `json:"children_total"`
	ChildrenMerged   int `json:"children_merged"`
	ChildrenOpen     int `json:"children_open"`
	AwaitingApproval int `json:"awaiting_approval"`
}

// fleetSupervisorPulse describes the supervisor's liveness, cadence and
// recent decision verbs at the project level. The SPA aggregates these
// across the fleet to render the header card. Zero values render as
// "unknown" so older servers / unconfigured projects degrade gracefully.
type fleetSupervisorPulse struct {
	LastRunOnceAt         string   `json:"last_run_once_at,omitempty"`
	LastRunOnceAgeSeconds int64    `json:"last_run_once_age_seconds,omitempty"`
	PollIntervalSeconds   int      `json:"poll_interval_seconds,omitempty"`
	Mode                  string   `json:"mode,omitempty"`
	RecentActions         []string `json:"recent_actions,omitempty"`
	Stuck                 bool     `json:"stuck,omitempty"`
	StuckReason           string   `json:"stuck_reason,omitempty"`
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
	ProjectName    string `json:"project_name"`
	ProjectRepo    string `json:"project_repo,omitempty"`
	DashboardURL   string `json:"dashboard_url,omitempty"`
	Slot           string `json:"slot"`
	IssueNumber    int    `json:"issue_number"`
	IssueTitle     string `json:"issue_title"`
	IssueURL       string `json:"issue_url,omitempty"`
	Status         string `json:"status"`
	DisplayStatus  string `json:"display_status,omitempty"`
	StatusReason   string `json:"status_reason,omitempty"`
	NextAction     string `json:"next_action,omitempty"`
	NeedsAttention bool   `json:"needs_attention,omitempty"`
	Live           bool   `json:"live"`
	Backend        string `json:"backend,omitempty"`
	// #730: model the backend self-reported for this run (Pi --mode json).
	// Empty for backends that do not self-report a model.
	Model string `json:"model,omitempty"`
	// BackendSelection records why this backend was chosen (label, role, auto,
	// default, router_error, phase, review_repair). Surfaced on the fleet drawer
	// so operators can tell task-based routing from label-pinned defaults. (#427)
	BackendSelection  *state.BackendSelection `json:"backend_selection,omitempty"`
	PRNumber          int                     `json:"pr_number,omitempty"`
	PRURL             string                  `json:"pr_url,omitempty"`
	TokensUsedAttempt int                     `json:"tokens_used_attempt"`
	TokensUsedTotal   int                     `json:"tokens_used_total"`
	// CostUSDEstimate is the $ estimate for TokensUsedTotal under the
	// project's configured per-backend pricing (#619), OR the backend's
	// self-reported cost when present (#730, Pi --mode json cost.total). 0
	// when neither is set; the SPA renders that as tokens only without
	// computing pricing client-side.
	CostUSDEstimate float64 `json:"cost_usd_estimate,omitempty"`
	// CostUSDBackend is the USD cost the backend self-reported (#730).
	// Zero when not reported; surfaced separately from the pricing estimate
	// so Mission Control can mark the value as backend-reported.
	CostUSDBackend float64 `json:"cost_usd_backend,omitempty"`
	// Runtime / RuntimeSeconds (legacy fields, kept for backwards
	// compatibility) reflect workflow elapsed time and include PR-open /
	// CI / Greptile / merge waiting. See #426 — WorkerRuntimeSeconds is
	// the agent's wall-clock; WorkflowRuntimeSeconds is the full session.
	Runtime                string `json:"runtime"`
	RuntimeSeconds         int64  `json:"runtime_seconds"`
	WorkerRuntime          string `json:"worker_runtime"`
	WorkerRuntimeSeconds   int64  `json:"worker_runtime_seconds"`
	WorkflowRuntime        string `json:"workflow_runtime"`
	WorkflowRuntimeSeconds int64  `json:"workflow_runtime_seconds"`
	PROpenRuntime          string `json:"pr_open_runtime,omitempty"`
	PROpenRuntimeSeconds   int64  `json:"pr_open_runtime_seconds,omitempty"`
	StartedAt              string `json:"started_at"`
	FinishedAt             string `json:"finished_at,omitempty"`
	WorkerEndedAt          string `json:"worker_ended_at,omitempty"`
	PROpenedAt             string `json:"pr_opened_at,omitempty"`
	NextRetryAt            string `json:"next_retry_at,omitempty"`
	PID                    int    `json:"pid,omitempty"`
	Alive                  *bool  `json:"alive,omitempty"`
	Worktree               string `json:"worktree,omitempty"`
	Branch                 string `json:"branch,omitempty"`
	TmuxSession            string `json:"tmux_session,omitempty"`
	HasLog                 bool   `json:"has_log"`
	RetryCount             int    `json:"retry_count,omitempty"`
	LastNotification       string `json:"last_notification,omitempty"`
	// Attribution is the per-segment backend timeline for this session
	// (#513 / #534). The SPA renders the active segment inline on the card
	// and the complete list with EndReason between segments inside the
	// worker drawer.
	Attribution []state.BackendAttribution `json:"attribution,omitempty"`
	Actions     []controlAction            `json:"actions,omitempty"`
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
	pricing := backendPricingMap(project.cfg)
	infos[0].CostUSDEstimate = sessionCostUSD(sess, pricing)
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
	for _, project := range s.projectsSnapshot() {
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
	// #487: auth fires BEFORE the read-only gate so an unauthenticated POST
	// always sees 401 (never 403/405). Spec: "every mutating POST without a
	// valid credential returns 401".
	authenticatedActor, ok := requireAuth(w, r, s.authChecker())
	if !ok {
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

	project, projectOK := s.findProject(req.Project)
	if projectOK && project.cfg != nil && project.cfg.Server.ReadOnly {
		writeError(w, http.StatusForbidden, "fleet project is read-only; write actions require approval-backed controls to be enabled in configuration")
		return
	}

	var (
		gh    actionGitHubClient
		audit actionAuditRecorder
		cfg   *config.Config
	)
	if projectOK {
		cfg = project.cfg
		gh = project.actionGH
		audit = s.fleetActionAudit(project)
	}
	if translateUIActionID(strings.TrimSpace(req.ActionID)) == config.SupervisorActionCloseIssueBatch && len(req.Issues) == 0 && cfg != nil {
		if st, err := state.Load(cfg.StateDir); err == nil {
			req.Issues = fleetCloseCandidateTargets(fleetCloseCandidates(fleetProjectState{Name: project.Name, Repo: cfg.Repo}, st))
		}
	}

	if res := dispatchSafeAction(req, cfg, gh, audit, authenticatedActor); res.handled {
		if res.err != nil {
			log.Printf("[fleet] safe action %q for project %q failed: %v", req.ActionID, req.Project, res.err)
		}
		writeJSON(w, res.status, res.body)
		return
	}

	stateDir := ""
	if cfg != nil {
		stateDir = cfg.StateDir
	}
	if res := dispatchApprovalAction(req, cfg, stateDir, audit, authenticatedActor); res.handled {
		if res.err != nil {
			log.Printf("[fleet] approval enqueue %q for project %q failed: %v", req.ActionID, req.Project, res.err)
		}
		writeJSON(w, res.status, res.body)
		return
	}

	// #475 (1/3): the safe and approval-required action endpoints are now
	// implemented; this fallback is reachable only when a verb is neither
	// safe, approval-required, nor a known UI-translation alias — i.e. an
	// unknown action_id. Return 400, not the legacy 501 stub.
	id := strings.TrimSpace(req.ActionID)
	if id == "" {
		writeError(w, http.StatusBadRequest, "action_id is required")
		return
	}
	writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown action_id %q", id))
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
	// #487: audit log writes are forensic-integrity-critical. An
	// unauthenticated attacker on the LAN must not be able to write
	// arbitrary entries that bury the real attack signal under noise. Auth
	// fires BEFORE payload parsing so probes cannot enumerate the project
	// list via 400 differences.
	authenticatedActor, ok := requireAuth(w, r, s.authChecker())
	if !ok {
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
	actor := resolveActor(authenticatedActor, req.Actor, "")
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
	for _, project := range s.projectsSnapshot() {
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
	projects := s.projectsSnapshot()
	resp := fleetResponse{
		ReadOnly:    s.readOnly,
		Version:     binaryVersion,
		RefreshedAt: formatFleetTime(now),
		Projects:    make([]fleetProjectState, 0, len(projects)),
		Workers:     make([]fleetWorkerState, 0),
		Attention:   make([]fleetWorkerState, 0),
		Approvals:   make([]fleetApprovalState, 0),
	}
	throughputBuckets := newFleetThroughputBuckets(now, 7)
	for _, project := range projects {
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
		resp.Summary.PRsOpen += item.PRsOpen
		resp.Summary.WorkersRunning += item.WorkersRunning
		resp.Summary.Failed += item.Failed
		resp.Summary.Sessions += item.Sessions
		resp.Summary.NeedsAttention += item.NeedsAttention
		resp.Summary.SelfResolving += item.SelfResolving
		addFleetThroughputSummary(throughputBuckets, workers)
		for _, approval := range item.Approvals {
			addFleetApprovalSummary(&resp.Summary, approval)
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
	resp.NextAction = buildFleetNextAction(resp.Projects, resp.Approvals, now)
	resp.Verdict = buildFleetVerdict(resp, now)
	resp.CostObservability = rollupGlobalCost(resp.Projects)
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
	headline, detail := buildFleetVerdictShort(resp, latest, tone, now)
	return fleetVerdict{
		Tone:     tone,
		Sentence: strings.Join(parts, " "),
		Headline: headline,
		Detail:   detail,
	}
}

// buildFleetVerdictShort produces the short, structured form of the
// verdict for the SPA hero (issue #474). Headline is a 2–4 word status
// like "Supervisor healthy." or "Daemon offline."; Detail is ONE
// concise qualifier sentence derived from next_action / operator_brief
// rather than the full attention/PR/activity recap (which is already
// surfaced via the hb-meta chips and stat cards).
func buildFleetVerdictShort(resp fleetResponse, latest *supervisorDecisionInfo, tone string, now time.Time) (string, string) {
	headline := fleetVerdictHeadline(resp.Summary, latest, tone, now)
	detail := fleetVerdictDetail(resp, latest, now)
	return headline, detail
}

func fleetVerdictHeadline(summary fleetSummary, latest *supervisorDecisionInfo, tone string, now time.Time) string {
	if supervisorHeartbeatStale(latest, now) {
		return "Daemon offline."
	}
	switch tone {
	case "attention":
		return "Action required."
	case "busy":
		if summary.Running > 0 {
			return fmt.Sprintf("%d worker%s in flight.", summary.Running, fleetVerdictPlural(summary.Running))
		}
		return "Working."
	case "healthy":
		if summary.Running == 0 && summary.PROpen == 0 && summary.NeedsAttention == 0 {
			return "Idle, healthy."
		}
		return "Supervisor healthy."
	default:
		return "Supervisor healthy."
	}
}

func fleetVerdictPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// fleetVerdictDetail returns the single most operator-relevant
// qualifier sentence: prefer next_action (kind + project), fall back to
// operator_brief, else a one-line activity summary. NEVER returns the
// chained metric recap that hb-meta already shows.
func fleetVerdictDetail(resp fleetResponse, latest *supervisorDecisionInfo, now time.Time) string {
	if supervisorHeartbeatStale(latest, now) {
		if latest != nil && !latest.CreatedAt.IsZero() {
			return fmt.Sprintf("Last seen %s ago.", formatFleetVerdictAge(latest.CreatedAt, now))
		}
		return "No supervisor heartbeat."
	}
	na := resp.NextAction
	if na != nil && strings.TrimSpace(na.Project) != "" && strings.TrimSpace(na.Kind) != "" {
		project := strings.TrimSpace(na.Project)
		kind := strings.ReplaceAll(strings.TrimSpace(na.Kind), "_", " ")
		if pr := strings.TrimSpace(na.Priority); pr != "" {
			return fmt.Sprintf("%s in %s — %s.", titleCaseFleetVerdict(kind), project, strings.ToLower(pr))
		}
		return fmt.Sprintf("%s in %s.", titleCaseFleetVerdict(kind), project)
	}
	if resp.Summary.Running == 0 && resp.Summary.PROpen == 0 && resp.Summary.NeedsAttention == 0 && resp.Summary.DispatchPending == 0 {
		return "Nothing needs you right now."
	}
	if resp.Summary.PROpen > 0 {
		return fmt.Sprintf("%d PR%s waiting for review.", resp.Summary.PROpen, fleetVerdictPlural(resp.Summary.PROpen))
	}
	if resp.Summary.Running > 0 {
		return "Workers in flight; CI watching."
	}
	if resp.Summary.NeedsAttention > 0 {
		return fmt.Sprintf("%d item%s need attention.", resp.Summary.NeedsAttention, fleetVerdictPlural(resp.Summary.NeedsAttention))
	}
	return ""
}

// titleCaseFleetVerdict capitalises the first letter of a verb phrase
// like "approval pending" → "Approval pending" without mangling the
// rest. Avoids importing "golang.org/x/text/cases" for one verb.
func titleCaseFleetVerdict(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func fleetVerdictTone(summary fleetSummary, latest *supervisorDecisionInfo, now time.Time) string {
	if latest == nil || supervisorHeartbeatStale(latest, now) {
		return "daemon-down"
	}
	// #598: subtract convergence-bound items (retry_exhausted + open PR
	// + checks green) so a self-resolving PR never alarms the verdict.
	// Reserve `attention` for items that genuinely need a human:
	// stale snapshots, project errors, pending approvals, dispatch
	// failures, outcome drift, or empty/stuck queues.
	actionableAttention := summary.NeedsAttention - summary.SelfResolving
	if actionableAttention < 0 {
		actionableAttention = 0
	}
	if summary.Stale > 0 || summary.Errors > 0 || actionableAttention > 0 || fleetActionableApprovalCount(summary) > 0 || summary.DispatchFailures > 0 || summary.OutcomeDrift > 0 || summary.NoEligibleIssues > 0 {
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
	// #598: convergence-bound items (retry_exhausted + open PR + checks
	// green) are not operator-actionable; subtract them from the
	// attention count so the sentence reads honestly. Self-resolving
	// items are surfaced as a separate calm clause when present.
	selfResolving := summary.SelfResolving
	if selfResolving > summary.NeedsAttention {
		selfResolving = summary.NeedsAttention
	}
	actionable := summary.NeedsAttention - selfResolving
	items := actionable + fleetActionableApprovalCount(summary) + summary.Errors + summary.Stale + summary.DispatchFailures + summary.OutcomeDrift + summary.NoEligibleIssues
	var base string
	switch items {
	case 0:
		base = "No item needs attention."
	case 1:
		base = "1 item needs attention."
	default:
		base = fmt.Sprintf("%d items need attention.", items)
	}
	if selfResolving > 0 {
		var tail string
		if selfResolving == 1 {
			tail = "1 PR is auto-merging — no action needed."
		} else {
			tail = fmt.Sprintf("%d PRs are auto-merging — no action needed.", selfResolving)
		}
		return base + " " + tail
	}
	return base
}

func fleetActionableApprovalCount(summary fleetSummary) int {
	if summary.ApprovalsActionable > 0 || summary.ApprovalsSuggestion > 0 {
		return summary.ApprovalsActionable
	}
	return summary.ApprovalsPending
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

// fleetNextActionCandidate is one possible "what needs me now" item before
// priority/age sorting picks the canonical winner.
type fleetNextActionCandidate struct {
	Project     string
	Kind        string
	TargetURL   string
	Reason      string
	Priority    int
	UpdatedAt   time.Time
	tieKey      string
	CTALabel    string
	PRNumber    int
	IssueNumber int
}

// fleetNextActionPriorityForKind maps an operator-state kind onto a priority
// tier (lower number = higher priority). The second return value reports
// whether the kind is a "needs operator" candidate at all; non-actionable
// kinds (working, monitoring_pr, idle, ...) are excluded from the brief.
func fleetNextActionPriorityForKind(kind string) (int, bool) {
	switch strings.TrimSpace(kind) {
	case "error", "dispatch_failure", "stale_worker":
		return 0, true
	case "attention":
		return 1, true
	case "outcome_drift", "stale", "no_eligible_issues", "queue_blocked":
		return 2, true
	case "outcome_missing":
		return 3, true
	default:
		return 0, false
	}
}

// fleetProjectOperatorUpdatedAt returns a stable timestamp for a project's
// current operator state. The source is chosen from the operator-state kind
// so that the timestamp matches the signal that drove the state — e.g. a
// dispatch_failure should age from the supervisor decision, not from an
// older attention session that the dispatch failure already preempted in
// buildFleetProjectOperatorState. The choice is "stable" in the sense that
// it does not depend on `now`, so a snapshot computed twice over the same
// input returns the same picked_at.
func fleetProjectOperatorUpdatedAt(project fleetProjectState) time.Time {
	kind := strings.TrimSpace(project.OperatorState.Kind)
	switch kind {
	case "attention", "stale_worker":
		if len(project.Attention) > 0 {
			worker := highestPriorityAttentionSession(project.Attention)
			if t := parseFleetWorkerTime(worker.StartedAt); !t.IsZero() {
				return t
			}
		}
	case "dispatch_failure", "outcome_drift", "queue_blocked", "no_eligible_issues":
		if project.Supervisor.Latest != nil && !project.Supervisor.Latest.CreatedAt.IsZero() {
			return project.Supervisor.Latest.CreatedAt.UTC()
		}
	case "stale":
		if t := parseFleetWorkerTime(project.Freshness.SnapshotAt); !t.IsZero() {
			return t
		}
	}
	// Fallbacks for kinds without a primary source, or when the primary
	// source is empty: prefer the most recent supervisor decision, then the
	// snapshot freshness timestamp.
	if project.Supervisor.Latest != nil && !project.Supervisor.Latest.CreatedAt.IsZero() {
		return project.Supervisor.Latest.CreatedAt.UTC()
	}
	if t := parseFleetWorkerTime(project.Freshness.SnapshotAt); !t.IsZero() {
		return t
	}
	return time.Time{}
}

func parseFleetWorkerTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

func fleetApprovalIsSuggestion(approval *fleetApprovalState) bool {
	if approval == nil {
		return false
	}
	switch strings.TrimSpace(approval.Action) {
	case config.SupervisorActionApplyLessonProposal:
		return true
	default:
		return false
	}
}

func fleetNextActionTargetForProject(project *fleetProjectState) string {
	if project == nil {
		return ""
	}
	op := project.OperatorState
	if strings.TrimSpace(op.Session) != "" {
		switch strings.TrimSpace(op.Kind) {
		case "attention", "stale_worker":
			return fleetWorkersFocusedPath(project.Name, op.Session)
		}
	}
	return fleetProjectFocusedPath(project.Name, op.IssueNumber, op.PRNumber, "")
}

// buildFleetNextAction picks the single canonical next operator action across
// the fleet using the priority + age algorithm documented in
// docs/fleet-mission-control-runbook.md. Returns nil when nothing needs the
// operator right now.
func buildFleetNextAction(projects []fleetProjectState, approvals []fleetApprovalState, now time.Time) *fleetNextAction {
	candidates := make([]fleetNextActionCandidate, 0, len(projects)+len(approvals))

	for i := range approvals {
		approval := &approvals[i]
		if state.ApprovalStatus(approval.Status) != state.ApprovalStatusPending {
			continue
		}
		priority := 1
		reason := "Approve or reject the pending supervisor approval after checking the target state."
		if fleetApprovalIsSuggestion(approval) {
			priority = 3
			reason = "Review the supervisor suggestion when higher-priority operator decisions are clear."
		} else if approvalPastSLA(approval, now) {
			priority = 0
			reason = "Pending approval is past the " + fleetApprovalSLAText() + " SLA. Approve or reject it now."
		}
		if summary := strings.TrimSpace(approval.Summary); summary != "" {
			reason = summary + " " + reason
		}
		target := firstNonEmpty(approval.DashboardURL, fleetApprovalsFocusedPath(approval.ID))
		candidates = append(candidates, fleetNextActionCandidate{
			Project:     approval.ProjectName,
			Kind:        "approval_pending",
			TargetURL:   target,
			Reason:      truncateFleetOperatorText(reason, 200),
			Priority:    priority,
			UpdatedAt:   fleetApprovalRecency(*approval),
			tieKey:      "approval|" + approval.ProjectName + "|" + approval.ID,
			CTALabel:    fleetNextActionCTAForApproval(approval),
			PRNumber:    approval.PRNumber,
			IssueNumber: approval.IssueNumber,
		})
	}

	for i := range projects {
		project := &projects[i]
		kind := strings.TrimSpace(project.OperatorState.Kind)
		priority, ok := fleetNextActionPriorityForKind(kind)
		if !ok {
			continue
		}
		reason := strings.TrimSpace(project.OperatorState.NextAction)
		if reason == "" {
			reason = strings.TrimSpace(project.OperatorState.Summary)
		}
		if reason == "" {
			reason = strings.TrimSpace(project.OperatorState.Label)
		}
		target := fleetNextActionTargetForProject(project)
		candidates = append(candidates, fleetNextActionCandidate{
			Project:     project.Name,
			Kind:        kind,
			TargetURL:   target,
			Reason:      truncateFleetOperatorText(reason, 200),
			Priority:    priority,
			UpdatedAt:   fleetProjectOperatorUpdatedAt(*project),
			tieKey:      "project|" + project.Name + "|" + kind,
			CTALabel:    fleetNextActionCTAForProject(kind, project.OperatorState),
			PRNumber:    project.OperatorState.PRNumber,
			IssueNumber: project.OperatorState.IssueNumber,
		})
	}

	if len(candidates) == 0 {
		return nil
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		l, r := candidates[i], candidates[j]
		if l.Priority != r.Priority {
			return l.Priority < r.Priority
		}
		// Within a tier the oldest known timestamp wins. A zero/unknown
		// timestamp is treated as "newer than any known time" so a candidate
		// with no updated_at loses to a candidate that has one — otherwise a
		// project missing input data would always grab the brief.
		switch {
		case l.UpdatedAt.IsZero() && r.UpdatedAt.IsZero():
			// fall through to the deterministic tie breaker
		case l.UpdatedAt.IsZero():
			return false
		case r.UpdatedAt.IsZero():
			return true
		case !l.UpdatedAt.Equal(r.UpdatedAt):
			return l.UpdatedAt.Before(r.UpdatedAt)
		}
		return l.tieKey < r.tieKey
	})

	winner := candidates[0]
	return &fleetNextAction{
		Project:     winner.Project,
		Kind:        winner.Kind,
		TargetURL:   winner.TargetURL,
		Reason:      winner.Reason,
		Priority:    fmt.Sprintf("P%d", winner.Priority),
		PickedAt:    formatFleetTime(winner.UpdatedAt),
		CTALabel:    winner.CTALabel,
		PRNumber:    winner.PRNumber,
		IssueNumber: winner.IssueNumber,
	}
}

// fleetNextActionCTAForApproval returns the verb-shaped button label the
// header card uses for a pending approval (issue #531). The label names
// the *effect* the operator confirms when they click — "Approve PR #123"
// rather than a generic "Action required".
func fleetNextActionCTAForApproval(approval *fleetApprovalState) string {
	if approval == nil {
		return ""
	}
	switch strings.TrimSpace(approval.Action) {
	case "merge_pr":
		if approval.PRNumber > 0 {
			return fmt.Sprintf("Approve PR #%d", approval.PRNumber)
		}
		return "Approve merge"
	case "close_issue":
		if approval.IssueNumber > 0 {
			return fmt.Sprintf("Close issue #%d", approval.IssueNumber)
		}
		return "Close issue"
	case config.SupervisorActionCloseIssueBatch:
		return "Close verified issues"
	case "delete_worktree":
		return "Delete worktree"
	case "change_global_config":
		return "Apply config change"
	case "spawn_worker":
		if approval.IssueNumber > 0 {
			return fmt.Sprintf("Start worker on #%d", approval.IssueNumber)
		}
		return "Start worker"
	case "label_issue_ready":
		if approval.IssueNumber > 0 {
			return fmt.Sprintf("Mark issue #%d ready", approval.IssueNumber)
		}
		return "Mark issue ready"
	}
	if approval.PRNumber > 0 {
		return fmt.Sprintf("Review PR #%d", approval.PRNumber)
	}
	if approval.IssueNumber > 0 {
		return fmt.Sprintf("Review issue #%d", approval.IssueNumber)
	}
	return "Review approval"
}

// fleetNextActionCTAForProject returns the header CTA for a project-level
// operator state kind (dispatch_failure, outcome_drift, stale_worker, …).
// Each branch names a concrete next step so the button replaces the
// passive «Action required» text with a verb the operator can act on.
// See issue #598: a self-resolving state ("auto_merging") returns "" so the
// SPA can render a calm "Auto-merging — no action needed" line instead of
// a button.
func fleetNextActionCTAForProject(kind string, op fleetOperatorState) string {
	switch strings.TrimSpace(kind) {
	case "error":
		return "Investigate project error"
	case "dispatch_failure":
		if op.Label == "Self-deploy failed" {
			return "Redeploy maestro binary" // #711
		}
		return "Resolve stuck dispatch"
	case "stale_worker":
		if op.PRNumber > 0 {
			return fmt.Sprintf("Open worker log for PR #%d", op.PRNumber)
		}
		if op.Session != "" {
			return "Open worker log for " + op.Session
		}
		return "Open worker log"
	case "attention":
		if op.PRNumber > 0 {
			if strings.Contains(strings.ToLower(op.Summary), "conflict") {
				return fmt.Sprintf("Resolve conflict on PR #%d", op.PRNumber)
			}
			if strings.Contains(strings.ToLower(op.Summary), "check") {
				return fmt.Sprintf("Fix failing checks on PR #%d", op.PRNumber)
			}
			return fmt.Sprintf("Review PR #%d", op.PRNumber)
		}
		if op.IssueNumber > 0 {
			return fmt.Sprintf("Review issue #%d", op.IssueNumber)
		}
		return "Review attention"
	case "auto_merging":
		return ""
	case "outcome_drift":
		return "Reconcile outcome drift"
	case "outcome_missing":
		return "Configure outcome"
	case "no_eligible_issues":
		return "Queue more work"
	case "queue_blocked":
		return "Unblock queue"
	case "stale":
		return "Refresh stale snapshot"
	}
	return ""
}

const fleetApprovalSLASeconds int64 = 30 * 60
const fleetDispatchDefaultSLASeconds int64 = 5 * 60

func fleetApprovalSLAText() string {
	return "30m"
}

func fleetDispatchSLASeconds(project fleetProjectState) int64 {
	if project.DispatchSLASeconds > 0 {
		return int64(project.DispatchSLASeconds)
	}
	return fleetDispatchDefaultSLASeconds
}

func fleetDispatchSLAText(project fleetProjectState) string {
	return fleetDurationText(time.Duration(fleetDispatchSLASeconds(project)) * time.Second)
}

func fleetDurationText(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}

func fleetPendingDispatchPastSLA(project fleetProjectState, now time.Time) bool {
	if project.Supervisor.Latest == nil || project.Supervisor.Latest.CreatedAt.IsZero() {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return now.Sub(project.Supervisor.Latest.CreatedAt.UTC()) > time.Duration(fleetDispatchSLASeconds(project))*time.Second
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
		if state.ApprovalStatus(approval.Status) != state.ApprovalStatusPending || fleetApprovalIsSuggestion(approval) || !approvalPastSLA(approval, now) {
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
		if state.ApprovalStatus(approval.Status) != state.ApprovalStatusPending || fleetApprovalIsSuggestion(approval) {
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

// fleetOperatorKindIsSelfResolving reports whether an operator-state kind
// represents a convergence-bound state that the orchestrator will resolve
// on its own (#598). The fleet verdict and the next-action picker treat
// these as calm signals — never as `Action required — p1`.
func fleetOperatorKindIsSelfResolving(kind string) bool {
	return strings.TrimSpace(kind) == "auto_merging"
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
	case "auto_merging":
		// #598: convergence-bound, calm — sort alongside monitoring_pr,
		// before idle/queue states so the project card still surfaces
		// near the top of the list but never alarms the verdict.
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

// fleetEpicProgressFromState returns the epic-completion aggregate the
// supervisor stamped on its latest decision (#650), together with the
// operator-glance roll-up the project card renders. Returns an empty
// slice and zero-valued summary when no decision is recorded or the
// recorded decision has no epic aggregate.
func fleetEpicProgressFromState(st *state.State) ([]state.EpicProgress, fleetEpicSummary) {
	if st == nil {
		return nil, fleetEpicSummary{}
	}
	latest := st.LatestSupervisorDecision()
	if latest == nil || len(latest.Epics) == 0 {
		return nil, fleetEpicSummary{}
	}
	epics := append([]state.EpicProgress(nil), latest.Epics...)
	summary := fleetEpicSummary{Tracked: len(epics)}
	epicNumbers := make(map[int]bool, len(epics))
	for _, epic := range epics {
		summary.ChildrenTotal += epic.TotalChildren
		summary.ChildrenMerged += epic.MergedCount
		summary.ChildrenOpen += epic.OpenCount
		if epic.Complete {
			summary.Complete++
		} else {
			summary.InProgress++
		}
		if epic.Number > 0 {
			epicNumbers[epic.Number] = true
		}
	}
	// Cross-reference pending close_issue approvals against the epic
	// list so the SPA can render an "awaiting approval" pill on the
	// epic row. The executor verb stays `close_issue`; the epic-vs-
	// child-close discriminator is whether the approval's target issue
	// is one of the open epics surfaced by this snapshot.
	for _, approval := range st.Approvals {
		if approval.Status != state.ApprovalStatusPending {
			continue
		}
		if approval.Action != config.SupervisorActionCloseIssue {
			continue
		}
		if approval.Target == nil {
			continue
		}
		if epicNumbers[approval.Target.Issue] {
			summary.AwaitingApproval++
		}
	}
	return epics, summary
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
	if len(analysis.EligibleRanked) > 0 {
		snapshot.EligibleRanked = append([]state.SupervisorIssueCandidate(nil), analysis.EligibleRanked...)
	}
	if len(analysis.SkippedCandidates) > 0 {
		snapshot.SkippedCandidates = append([]state.SupervisorSkippedCandidate(nil), analysis.SkippedCandidates...)
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

func addFleetApprovalSummary(summary *fleetSummary, approval fleetApprovalState) {
	switch state.ApprovalStatus(approval.Status) {
	case state.ApprovalStatusPending:
		summary.Approvals++
		summary.ApprovalsPending++
		if fleetApprovalIsSuggestion(&approval) {
			summary.ApprovalsSuggestion++
		} else {
			summary.ApprovalsActionable++
		}
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

func buildFleetEffectiveConfig(cfg *config.Config) fleetEffectiveConfig {
	if cfg == nil {
		return fleetEffectiveConfig{}
	}
	backends := make([]fleetEffectiveBackendConfig, 0, len(cfg.Model.Backends))
	priced := 0
	for name, def := range cfg.Model.Backends {
		if def.Pricing.Configured() {
			priced++
		}
		backends = append(backends, fleetEffectiveBackendConfig{
			Name:             name,
			Enabled:          def.IsEnabled(),
			Provider:         strings.TrimSpace(def.Provider),
			Model:            strings.TrimSpace(def.Model),
			Variant:          strings.TrimSpace(def.Variant),
			Effort:           strings.TrimSpace(def.Effort),
			PromptMode:       strings.TrimSpace(def.PromptMode),
			NonAgentic:       def.NonAgentic,
			PriceConfigured:  def.Pricing.Configured(),
			InputUSDPerMtok:  def.Pricing.InputUSDPerMtok,
			OutputUSDPerMtok: def.Pricing.OutputUSDPerMtok,
		})
	}
	sort.Slice(backends, func(i, j int) bool { return backends[i].Name < backends[j].Name })

	retention := cfg.SessionRetention
	return fleetEffectiveConfig{
		ModelPolicy: fleetModelPolicy{
			Default:          strings.TrimSpace(cfg.Model.Default),
			FallbackBackends: append([]string(nil), cfg.Model.FallbackBackends...),
			Backends:         backends,
			Routing: fleetEffectiveRoutingConfig{
				Mode:            strings.TrimSpace(cfg.Routing.Mode),
				RouterModel:     strings.TrimSpace(cfg.Routing.RouterModel),
				RouterModelName: strings.TrimSpace(cfg.Routing.RouterModelName),
			},
		},
		MaxParallel: cfg.MaxParallel,
		ReviewGate:  strings.TrimSpace(cfg.ReviewGate),
		Labels: fleetConfigLabels{
			Issue:        append([]string(nil), cfg.IssueLabels...),
			Exclude:      append([]string(nil), cfg.ExcludeLabels...),
			Ready:        strings.TrimSpace(cfg.Supervisor.ReadyLabel),
			Blocked:      strings.TrimSpace(cfg.Supervisor.BlockedLabel),
			Excluded:     append([]string(nil), cfg.Supervisor.ExcludedLabels...),
			AllowTypes:   append([]string(nil), cfg.Supervisor.AllowIssueTypes...),
			Mission:      append([]string(nil), cfg.Missions.Labels...),
			Completion:   append([]string(nil), cfg.Supervisor.CompletionGates.RequiredLabels...),
			Verification: strings.TrimSpace(cfg.Supervisor.CompletionGates.VerificationLabel),
		},
		Retention: fleetConfigRetention{
			Enabled:            retention.IsEnabled(),
			KeepLast:           retention.EffectiveKeepLast(),
			MinAge:             retention.EffectiveMinAge().String(),
			ArchiveEnabled:     retention.ArchiveEnabled(),
			ArchiveFilePresent: strings.TrimSpace(retention.ArchiveFile) != "",
		},
		CostCaps: fleetConfigCostCaps{
			WorkerMaxTokens:          cfg.WorkerMaxTokens,
			WorkerSoftTokenThreshold: cfg.WorkerSoftTokenThreshold,
			BackendPricingConfigured: priced,
			BackendPricingTotal:      len(backends),
		},
		SupervisorGate: fleetConfigSupervisor{
			Mode:                    strings.TrimSpace(cfg.Supervisor.Mode),
			DryRun:                  cfg.Supervisor.DryRun,
			DispatchSLASeconds:      int(fleetDispatchSLASeconds(fleetProjectState{DispatchSLASeconds: cfg.Supervisor.DispatchSLASeconds})),
			SafeActions:             append([]string(nil), cfg.Supervisor.SafeActions...),
			ApprovalRequired:        append([]string(nil), cfg.Supervisor.ApprovalRequired...),
			AllowedActions:          append([]string(nil), cfg.Supervisor.AllowedActions...),
			ApprovalRequiredActions: append([]string(nil), cfg.Supervisor.ApprovalRequiredActions...),
			CompletionGatesActive:   cfg.Supervisor.CompletionGates.Active(),
			HandoffPlannerActive:    cfg.Supervisor.HandoffPlanner.Active(),
			ReviewRepairActive:      cfg.Supervisor.ReviewRepair.Active(),
			ReviewRepairBackend:     cfg.Supervisor.ReviewRepair.EffectiveBackend(),
			ReviewRepairMaxRetries:  cfg.Supervisor.ReviewRepair.EffectiveMaxRetries(),
		},
		ApprovalAction: config.SupervisorActionChangeGlobalConfig,
	}
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
	item.DispatchSLASeconds = cfg.Supervisor.DispatchSLASeconds
	item.Outcome = outcome.StatusFor(cfg.Outcome, 0, time.Time{})
	item.Actions = projectActionAffordances(item.ReadOnly, "/api/v1/fleet/actions", item.Name)
	item.EffectiveConfig = buildFleetEffectiveConfig(cfg)
	item.Freshness = fleetProjectFreshnessForState(cfg.StateDir, nil, now)
	item.ProjectBoard = project.snapshotBoard()

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		item.Error = err.Error()
		item.OperatorState = buildFleetProjectOperatorState(item)
		return item, nil
	}
	item.Freshness = fleetProjectFreshnessForState(cfg.StateDir, st, now)
	item.RestartRequired = st.RestartRequired
	item.RestartRequiredReason = st.RestartRequiredReason
	item.Paused = st.PauseActive()
	item.PausedAt = st.PausedAt
	item.CloseCandidates = fleetCloseCandidates(item, st)
	if len(item.CloseCandidates) > 0 {
		item.Actions = append(item.Actions, closeIssueBatchControlAction(item.ReadOnly, "/api/v1/fleet/actions", item.Name, item.CloseCandidates))
	}
	// #600: normalize stale cooldown entries for display so the BACKENDS
	// panel matches reality between orchestrator cycles — RetryAfter in
	// the past, max-cooldown TTL elapsed, or a successful PR-evidence
	// session recorded after the cooldown was set all render as healthy.
	state.ReconcileBackendHealth(st, now)
	item.BackendHealth = st.BackendHealth
	item.BackendQuota = buildFleetBackendQuota(cfg, st, now)
	item.CostObservability = buildFleetCostObservability(cfg, st, now)
	projectState := buildStateResponse(cfg, st)
	item.Summary = projectState.Summary
	item.Outcome = projectState.Outcome
	item.Running = len(projectState.Running)
	item.PROpen = len(projectState.PROpen)
	item.WorkersRunning = item.Running
	item.PRsOpen = fleetTruthfulOpenPRCount(projectState, st)
	item.Failed = failedCount(projectState.Summary)
	item.Sessions = len(projectState.All)
	item.Supervisor = projectState.Supervisor
	item.QueueSnapshot = fleetQueueSnapshotFromSupervisor(item.Supervisor)
	item.SupervisorPulse = buildFleetSupervisorPulse(cfg, st, now)
	item.Epics, item.EpicSummary = fleetEpicProgressFromState(st)
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
			if fleetSessionIsConvergenceBound(worker) {
				item.SelfResolving++
			}
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

func fleetCloseCandidates(project fleetProjectState, st *state.State) []fleetCloseCandidate {
	if st == nil {
		return nil
	}
	alreadyClosed := fleetIssuesCoveredByExecutedCloseApproval(st)
	candidates := make([]fleetCloseCandidate, 0)
	for slot, sess := range st.Sessions {
		if sess == nil || sess.IssueNumber <= 0 || sess.PRNumber <= 0 || sess.Status != state.StatusDone {
			continue
		}
		if alreadyClosed[sess.IssueNumber] {
			continue
		}
		candidates = append(candidates, fleetCloseCandidate{
			IssueNumber: sess.IssueNumber,
			IssueURL:    githubIssueURL(project.Repo, sess.IssueNumber),
			PRNumber:    sess.PRNumber,
			PRURL:       githubPRURL(project.Repo, sess.PRNumber),
			Session:     slot,
			FinishedAt:  formatOptionalFleetTime(sess.FinishedAt),
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].IssueNumber != candidates[j].IssueNumber {
			return candidates[i].IssueNumber < candidates[j].IssueNumber
		}
		return candidates[i].PRNumber < candidates[j].PRNumber
	})
	return candidates
}

func fleetCloseCandidateTargets(candidates []fleetCloseCandidate) []state.SupervisorIssueTarget {
	if len(candidates) == 0 {
		return nil
	}
	out := make([]state.SupervisorIssueTarget, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.IssueNumber <= 0 {
			continue
		}
		out = append(out, state.SupervisorIssueTarget{Issue: candidate.IssueNumber, PR: candidate.PRNumber})
	}
	return out
}

func fleetIssuesCoveredByExecutedCloseApproval(st *state.State) map[int]bool {
	covered := make(map[int]bool)
	if st == nil {
		return covered
	}
	for _, approval := range st.Approvals {
		if approval.Status != state.ApprovalStatusExecuted {
			continue
		}
		switch approval.Action {
		case config.SupervisorActionCloseIssue:
			if approval.Target != nil && approval.Target.Issue > 0 {
				covered[approval.Target.Issue] = true
			}
		case config.SupervisorActionCloseIssueBatch:
			if approval.Target == nil {
				continue
			}
			for _, target := range approval.Target.Issues {
				if target.Issue > 0 {
					covered[target.Issue] = true
				}
			}
		}
	}
	return covered
}

func reconcileStaleSessions(cfg *config.Config, st *state.State, now time.Time) []state.StaleSessionAudit {
	if cfg == nil || st == nil {
		return nil
	}
	policy := state.StaleSessionPolicy{
		Enabled:                cfg.StaleSessionReconciler.IsEnabled(),
		IdleAfter:              time.Duration(cfg.StaleSessionReconciler.IdleAfter()) * time.Minute,
		RequireWorktreeMissing: cfg.StaleSessionReconciler.WorktreeMissingRequired(),
		MergedPRDismisses:      cfg.StaleSessionReconciler.MergedPRDismissesEnabled(),
	}
	if !policy.Enabled {
		return nil
	}
	if policy.MergedPRDismisses {
		policy.PRStateForBranchPR = mergedBranchLookupFromState(st)
	}
	return st.ReconcileStaleSessions(now, policy, worktreeExistsOnDisk)
}

// mergedBranchLookupFromState derives a (branch, PRNumber) -> PR state lookup
// from the session table already persisted in state. Only StatusCodeLanded
// sessions contribute to the set: that is the one terminal status the
// orchestrator sets exclusively after observing the linked PR as merged.
// StatusDone is intentionally excluded because the orchestrator also reaches
// it on non-merge paths (issue closed externally) and would otherwise produce
// false dismissals.
//
// The lookup is keyed by (branch, PRNumber) so a stale CodeLanded record on
// the same branch but with a different PRNumber (e.g. after the original
// issue was re-opened and a new session retried on the same branch) cannot
// poison a live retry. The lookup uses no network calls, so the snapshot
// path stays fast and side-effect free.
func mergedBranchLookupFromState(st *state.State) func(string, int) string {
	if st == nil {
		return nil
	}
	type key struct {
		branch   string
		prNumber int
	}
	merged := make(map[key]struct{})
	for _, sess := range st.Sessions {
		if sess == nil {
			continue
		}
		if sess.Status != state.StatusCodeLanded {
			continue
		}
		branch := strings.TrimSpace(sess.Branch)
		if branch == "" || sess.PRNumber <= 0 {
			continue
		}
		merged[key{branch: branch, prNumber: sess.PRNumber}] = struct{}{}
	}
	if len(merged) == 0 {
		return func(string, int) string { return "" }
	}
	return func(branch string, prNumber int) string {
		if prNumber <= 0 {
			return ""
		}
		if _, ok := merged[key{branch: strings.TrimSpace(branch), prNumber: prNumber}]; ok {
			return "MERGED"
		}
		return ""
	}
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
		// #598: separate "self-resolving" sessions (retry_exhausted with an
		// open PR and no failing-check evidence — convergence will auto-merge
		// once gates clear) from genuinely operator-actionable ones. When all
		// attention is self-resolving the project reads calm ("auto-merging,
		// no action needed") instead of alarming the fleet verdict with a
		// passive `Action required — p1`.
		actionable, autoMerging := partitionFleetAttentionByResolvability(project.Attention)
		if len(actionable) == 0 && len(autoMerging) > 0 {
			return fleetAutoMergingOperatorState(project, autoMerging[0])
		}
		state := fleetOperatorState{
			Kind:       "attention",
			Tone:       "attention",
			Label:      "Needs attention",
			Summary:    fmt.Sprintf("%d worker item(s) need operator review.", project.NeedsAttention),
			NextAction: "Open the worker detail and resolve the first blocking reason.",
		}
		pickFrom := actionable
		if len(pickFrom) == 0 {
			pickFrom = project.Attention
		}
		if len(pickFrom) > 0 {
			worker := highestPriorityAttentionSession(pickFrom)
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
	// Operator pause (#683): once no in-flight work remains, an intentional
	// pause reads as "Paused" — not dead, not stalled, not idle. Placed
	// after the stale check so a genuinely dead unit still surfaces as an
	// outage, and after the running/PR checks so a finishing worker still
	// reads "Working" / "Monitoring PR" while it lands (the SPA shows the
	// paused badge alongside either way).
	if project.Paused && project.PROpen == 0 {
		return fleetOperatorState{
			Kind:       "paused",
			Tone:       "muted",
			Label:      "Paused",
			Summary:    "Project execution is paused by an operator; issue selection is skipped.",
			NextAction: "Run `maestro resume --config <cfg>` to restore issue selection.",
		}
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
			if fleetPendingDispatchPastSLA(project, time.Now().UTC()) {
				state = fleetDispatchSLAOperatorState(project, state)
			}
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

// partitionFleetAttentionByResolvability splits attention sessions into
// operator-actionable items and convergence-bound "self-resolving" items
// (see issue #598). A self-resolving session is currently the
// retry_exhausted-with-open-PR-and-no-failed-checks shape: the orchestrator
// will auto-merge it as soon as the merge gate clears, so the fleet verdict
// must not alarm with `Action required — p1`. Other shapes are returned in
// the actionable slice; the order of both slices mirrors the input.
func partitionFleetAttentionByResolvability(workers []sessionInfo) (actionable, autoMerging []sessionInfo) {
	for _, w := range workers {
		if fleetSessionIsConvergenceBound(w) {
			autoMerging = append(autoMerging, w)
			continue
		}
		actionable = append(actionable, w)
	}
	return actionable, autoMerging
}

// fleetSessionIsConvergenceBound reports whether a session in the
// attention list is "self-resolving" — convergence (the orchestrator's
// auto-merge once gates clear) will resolve it without operator action.
// Today this is the retry_exhausted-with-open-PR-and-no-failing-checks
// shape from issue #598: a green PR whose retry budget has been used up
// but whose merge will land naturally once the merge gate clears. A PR
// known to have failed checks (CIFailureOutput / LastNotifiedStatus =
// ci_failure) is NOT convergence-bound and stays actionable.
func fleetSessionIsConvergenceBound(worker sessionInfo) bool {
	if state.SessionStatus(worker.Status) != state.StatusRetryExhausted {
		return false
	}
	if worker.PRNumber <= 0 {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(worker.LastNotification), "ci_failure") {
		return false
	}
	return true
}

// fleetAutoMergingOperatorState builds the calm operator state for a
// project whose attention list contains only convergence-bound sessions
// (#598). Tone reads `healthy` so the fleet verdict does not alarm; the
// summary names the PR and the next-action line says explicitly that no
// operator action is required.
func fleetAutoMergingOperatorState(project fleetProjectState, worker sessionInfo) fleetOperatorState {
	summary := fmt.Sprintf("PR #%d retry budget exhausted but checks are green; Maestro will auto-merge once the merge gate clears.", worker.PRNumber)
	return fleetOperatorState{
		Kind:        "auto_merging",
		Tone:        "healthy",
		Label:       "Auto-merging",
		Summary:     summary,
		NextAction:  "Auto-merging — no action needed.",
		Session:     worker.Slot,
		IssueNumber: worker.IssueNumber,
		IssueURL:    firstNonEmpty(worker.IssueURL, githubIssueURL(project.Repo, worker.IssueNumber)),
		PRNumber:    worker.PRNumber,
		PRURL:       firstNonEmpty(worker.PRURL, githubPRURL(project.Repo, worker.PRNumber)),
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
	// #711: a failed/rolled-back self-deploy lands as a supervisor finding with
	// a `self-deploy-` decision ID. Surface it as an error-tone, high-priority
	// operator state with deploy-specific copy so an undeployed-but-merged host
	// is loud in Mission Control instead of silent (or mislabeled as a queue
	// dispatch failure). It reuses the dispatch_failure kind so it inherits the
	// existing error-tier priority/needs-action/next-action plumbing.
	if strings.HasPrefix(strings.TrimSpace(latest.ID), "self-deploy-") {
		operator := fleetOperatorState{
			Kind:       "dispatch_failure",
			Tone:       "error",
			Label:      "Self-deploy failed",
			Summary:    firstNonEmpty(latest.Summary, "Self-deploy of the maestro binary failed; the host may be running an undeployed version."),
			NextAction: firstNonEmpty(latest.RecommendedAction, "Inspect journalctl --user -u 'maestro-self-deploy-*', then redeploy the merged binary by hand."),
		}
		return applyFleetOperatorTarget(project, operator, latest.Target), true
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
	case "notify_red":
		operator = fleetOperatorState{
			Kind:       "infra_blocker",
			Tone:       "attention",
			Label:      "Infra blocker",
			Summary:    firstNonEmpty(summary, "A required source of truth is unavailable; Maestro must not report the project as healthy or idle."),
			NextAction: "Restore the blocked dependency or wait for the documented reset before trusting queue/project reconciliation.",
		}
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
	case "merge_pr":
		// #425 (sup-98): the supervisor recommended a merge but the
		// project policy lists merge_pr in approval_required, so a human
		// must click. Surface as "approval required" instead of the
		// generic "monitoring PR" so the dashboard shows the operator the
		// blocker, not a passive "monitoring" pill.
		if stuck, ok := findStuckState(latest.StuckStates, state.StuckPolicyBlocksMerge); ok {
			operator = fleetOperatorState{
				Kind:       "approval_required",
				Tone:       "attention",
				Label:      "Approval required",
				Summary:    firstNonEmpty(stuck.Summary, summary, "PR is ready to merge but supervisor policy requires operator approval."),
				NextAction: "Open the PR or the approvals queue and approve the merge so Maestro can complete the green-PR path.",
			}
			if stuck.Target != nil {
				target = stuck.Target
			}
		} else {
			operator = fleetOperatorState{
				Kind:       "merge_ready",
				Tone:       "busy",
				Label:      "Ready to merge",
				Summary:    firstNonEmpty(summary, "PR is ready to merge; supervisor will execute on the next cycle."),
				NextAction: "Wait for the supervisor's next tick to merge the PR.",
			}
		}
	case "spawn_worker":
		operator = fleetOperatorState{
			Kind:       "pending_dispatch",
			Tone:       "busy",
			Label:      "Dispatch pending",
			Summary:    firstNonEmpty(summary, "Supervisor selected an issue and should start a worker."),
			NextAction: "A worker should start on the next supervisor cycle; escalate if this exceeds the dispatch SLA.",
		}
	case "spawn_repair_worker":
		operator = fleetOperatorState{
			Kind:       "pending_dispatch",
			Tone:       "attention",
			Label:      "Repair pending",
			Summary:    firstNonEmpty(summary, "Supervisor selected repair work for a failing PR or outcome."),
			NextAction: "Start or approve the repair worker; the existing PR/outcome is not sufficient for completion.",
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
	if operator.Kind == "pending_dispatch" && operator.IssueNumber > 0 && fleetPendingDispatchPastSLA(project, time.Now().UTC()) {
		operator = fleetDispatchSLAOperatorState(project, operator)
	}
	return operator, true
}

func fleetDispatchSLAOperatorState(project fleetProjectState, pending fleetOperatorState) fleetOperatorState {
	sla := fleetDispatchSLAText(project)
	issue := pending.IssueNumber
	summary := pending.Summary
	if issue > 0 {
		summary = fmt.Sprintf("Issue #%d was selected for dispatch, but no worker started within the %s SLA.", issue, sla)
	}
	return fleetOperatorState{
		Kind:        "dispatch_failure",
		Tone:        "error",
		Label:       "Dispatch SLA missed",
		Summary:     firstNonEmpty(summary, "Supervisor selected work, but no worker started within the dispatch SLA."),
		NextAction:  "Check the supervisor/orchestrator dispatch loop and rerun the project supervisor after clearing the blocker.",
		IssueNumber: pending.IssueNumber,
		IssueURL:    pending.IssueURL,
		PRNumber:    pending.PRNumber,
		PRURL:       pending.PRURL,
		Session:     pending.Session,
	}
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

func findStuckState(stucks []state.SupervisorStuckState, code string) (state.SupervisorStuckState, bool) {
	for _, stuck := range stucks {
		if stuck.Code == code {
			return stuck, true
		}
	}
	return state.SupervisorStuckState{}, false
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
		ProjectName:            project.Name,
		ProjectRepo:            project.Repo,
		DashboardURL:           project.DashboardURL,
		Slot:                   worker.Slot,
		IssueNumber:            worker.IssueNumber,
		IssueTitle:             worker.IssueTitle,
		IssueURL:               worker.IssueURL,
		Status:                 worker.Status,
		DisplayStatus:          worker.DisplayStatus,
		StatusReason:           worker.StatusReason,
		NextAction:             worker.NextAction,
		NeedsAttention:         worker.NeedsAttention,
		Live:                   worker.Live,
		Backend:                worker.Backend,
		Model:                  worker.Model,
		BackendSelection:       worker.BackendSelection,
		PRNumber:               worker.PRNumber,
		PRURL:                  worker.PRURL,
		TokensUsedAttempt:      worker.TokensUsedAttempt,
		TokensUsedTotal:        worker.TokensUsedTotal,
		CostUSDEstimate:        worker.CostUSDEstimate,
		CostUSDBackend:         worker.CostUSDBackend,
		Runtime:                worker.Runtime,
		RuntimeSeconds:         worker.RuntimeSeconds,
		WorkerRuntime:          worker.WorkerRuntime,
		WorkerRuntimeSeconds:   worker.WorkerRuntimeSeconds,
		WorkflowRuntime:        worker.WorkflowRuntime,
		WorkflowRuntimeSeconds: worker.WorkflowRuntimeSeconds,
		PROpenRuntime:          worker.PROpenRuntime,
		PROpenRuntimeSeconds:   worker.PROpenRuntimeSeconds,
		StartedAt:              worker.StartedAt,
		FinishedAt:             worker.FinishedAt,
		WorkerEndedAt:          worker.WorkerEndedAt,
		PROpenedAt:             worker.PROpenedAt,
		NextRetryAt:            worker.NextRetryAt,
		PID:                    worker.PID,
		Alive:                  worker.Alive,
		Worktree:               worker.Worktree,
		Branch:                 worker.Branch,
		TmuxSession:            worker.TmuxSession,
		HasLog:                 worker.HasLog,
		RetryCount:             worker.RetryCount,
		LastNotification:       worker.LastNotification,
		Attribution:            worker.Attribution,
		Actions:                worker.Actions,
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
		DashboardURL:      fleetApprovalsFocusedPath(approval.ID),
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

func formatOptionalFleetTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return formatFleetTime(*t)
}

// fleetSupervisorPulseRecentLimit caps the number of recommended_action
// verbs we forward to the SPA header sparkline (issue #531). Ten is enough
// to read "idle is positive" at a glance without flooding the JSON.
const fleetSupervisorPulseRecentLimit = 10

// buildFleetSupervisorPulse extracts the liveness/cadence data the header
// verdict card needs: when the last run_once landed, the configured poll
// interval, the policy mode, and the last N recommended_action verbs for
// the decision sparkline. Returns a zero pulse when cfg/st are nil so the
// hero degrades to "unknown" instead of panicking.
func buildFleetSupervisorPulse(cfg *config.Config, st *state.State, now time.Time) fleetSupervisorPulse {
	pulse := fleetSupervisorPulse{}
	if cfg != nil {
		pulse.PollIntervalSeconds = cfg.PollIntervalSeconds
		pulse.Mode = strings.TrimSpace(cfg.Supervisor.Mode)
	}
	if st == nil {
		return pulse
	}
	if !st.LastRunOnceAt.IsZero() {
		pulse.LastRunOnceAt = formatFleetTime(st.LastRunOnceAt)
		pulse.LastRunOnceAgeSeconds = fleetAgeSeconds(st.LastRunOnceAt, now)
	}
	pulse.Stuck = st.SupervisorStuck
	pulse.StuckReason = strings.TrimSpace(st.SupervisorStuckReason)
	pulse.RecentActions = recentSupervisorActions(st.SupervisorDecisions, fleetSupervisorPulseRecentLimit)
	return pulse
}

// recentSupervisorActions returns the last `limit` recommended_action
// verbs from the decision log in chronological order (oldest → newest).
// Empty verbs are skipped so callers can render a clean sparkline
// without `"-"` placeholders.
func recentSupervisorActions(decisions []state.SupervisorDecision, limit int) []string {
	if limit <= 0 || len(decisions) == 0 {
		return nil
	}
	verbs := make([]string, 0, limit)
	start := 0
	if len(decisions) > limit {
		start = len(decisions) - limit
	}
	for _, d := range decisions[start:] {
		verb := strings.TrimSpace(d.RecommendedAction)
		if verb == "" {
			continue
		}
		verbs = append(verbs, verb)
	}
	if len(verbs) == 0 {
		return nil
	}
	return verbs
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

// fleetTruthfulOpenPRCount returns the project-level open-PR count expected
// by the SPA project card (#566). It is broader than the legacy
// `pr_open` (sessions in StatusPROpen): a session in StatusRetryExhausted
// with PRNumber > 0 is exactly the "gate-blocked PR that needs a human"
// case (#564) and must contribute to the count. When the latest supervisor
// decision reports ProjectState.OpenPRs (the GitHub truth, populated from
// ListOpenPRs), the larger of the two values wins so a transient session
// drift never under-counts the dashboard while a stale supervisor decision
// cannot over-count it.
func fleetTruthfulOpenPRCount(projectState stateResponse, st *state.State) int {
	count := len(projectState.PROpen)
	if st != nil {
		seen := make(map[int]struct{}, count)
		for _, info := range projectState.PROpen {
			if info.PRNumber > 0 {
				seen[info.PRNumber] = struct{}{}
			}
		}
		for _, sess := range st.Sessions {
			if sess == nil || sess.PRNumber <= 0 {
				continue
			}
			switch sess.Status {
			case state.StatusRetryExhausted, state.StatusFailed, state.StatusConflictFailed:
			default:
				continue
			}
			if _, ok := seen[sess.PRNumber]; ok {
				continue
			}
			seen[sess.PRNumber] = struct{}{}
			count++
		}
	}
	if latest := projectState.SupervisorLatest; latest != nil {
		if latest.ProjectState.OpenPRs > count {
			count = latest.ProjectState.OpenPRs
		}
	}
	return count
}

func (s *FleetServer) handleFleetDashboard(w http.ResponseWriter, r *http.Request) {
	if !isFleetMCRoute(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, web.MustReadStatic("mc/index.html"))
}

func isFleetMCRoute(path string) bool {
	switch path {
	case "/", "/fleet", "/workers", "/approvals", "/settings":
		return true
	}
	if strings.HasPrefix(path, "/project/") && len(path) > len("/project/") {
		return true
	}
	return false
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
	case "notify_red":
		return "Notify red"
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
	needsAttention := "0"
	if fleetProjectHasAttentionCTA(project) {
		needsAttention = "1"
	}
	mainRow := `<tr class="` + html.EscapeString(rowClass) + `" data-project="` + html.EscapeString(project.Name) + `" data-needs-attention="` + needsAttention + `" aria-controls="` + html.EscapeString(detailID) + `">` +
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
	}
	if structured := renderFleetProjectRailStructured(project, summary); structured != "" {
		parts = append(parts, structured)
	} else {
		parts = append(parts, `<div class="rail-subline" title="`+html.EscapeString(summary)+`">`+html.EscapeString(summary)+`</div>`)
	}
	if project.Error != "" {
		parts = append(parts, `<div class="rail-alert" title="`+html.EscapeString(project.Error)+`">State error</div>`)
	}
	if project.Freshness.Stale && operator.Kind != "stale" {
		parts = append(parts, `<div class="rail-warn">Stale snapshot</div>`)
	}
	return strings.Join(parts, "")
}

func renderFleetProjectRailStructured(project fleetProjectState, summary string) string {
	op := project.OperatorState
	issueNumber := op.IssueNumber
	session := strings.TrimSpace(op.Session)
	issueURL := strings.TrimSpace(op.IssueURL)
	nextAction := strings.TrimSpace(op.NextAction)
	if issueNumber == 0 && session == "" && nextAction == "" {
		return ""
	}
	parts := make([]string, 0, 2)
	if issueNumber > 0 {
		issueLabel := fmt.Sprintf("issue #%d", issueNumber)
		if issueURL != "" {
			parts = append(parts, `<a class="rail-structured-issue" href="`+html.EscapeString(issueURL)+`" target="_blank" rel="noreferrer">`+html.EscapeString(issueLabel)+`</a>`)
		} else {
			parts = append(parts, `<span class="rail-structured-issue">`+html.EscapeString(issueLabel)+`</span>`)
		}
	}
	if session != "" {
		parts = append(parts, `<button type="button" class="link-button rail-structured-session" data-project="`+html.EscapeString(project.Name)+`" data-slot="`+html.EscapeString(session)+`" title="Open session `+html.EscapeString(session)+`">`+html.EscapeString("("+session+")")+`</button>`)
	}
	if len(parts) == 0 {
		return ""
	}
	lead := strings.Join(parts, " ")
	reason := nextAction
	if reason == "" {
		reason = summary
	}
	reasonHTML := ""
	if reason != "" {
		reasonHTML = ` · <span class="rail-structured-reason" title="` + html.EscapeString(reason) + `">` + html.EscapeString(reason) + `</span>`
	}
	return `<div class="rail-subline rail-structured" title="` + html.EscapeString(summary) + `">` + lead + reasonHTML + `</div>`
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
	ageLabel := formatFleetFreshnessAge(freshness.SnapshotAge, freshness.SnapshotAgeSeconds)
	text := "No snapshot yet"
	if ageLabel != "" {
		text = "Snapshot " + ageLabel + " ago"
	}
	tooltipParts := make([]string, 0, 3)
	if strings.TrimSpace(freshness.SnapshotAt) != "" {
		tooltipParts = append(tooltipParts, "Snapshot at "+freshness.SnapshotAt)
	}
	if strings.TrimSpace(freshness.Reason) != "" {
		tooltipParts = append(tooltipParts, freshness.Reason)
	}
	tooltip := strings.Join(tooltipParts, " · ")
	if tooltip == "" {
		tooltip = text
	}
	return `<div class="rail-mainline" title="` + html.EscapeString(tooltip) + `">` + html.EscapeString(text) + `</div>`
}

func formatFleetFreshnessAge(rawAge string, ageSeconds int64) string {
	if ageSeconds >= 3600 {
		return formatClockDuration(ageSeconds)
	}
	if trimmed := strings.TrimSpace(rawAge); trimmed != "" {
		return trimmed
	}
	if ageSeconds > 0 {
		return fmt.Sprintf("%ds", ageSeconds)
	}
	return ""
}

func formatClockDuration(totalSeconds int64) string {
	if totalSeconds < 0 {
		totalSeconds = 0
	}
	h := totalSeconds / 3600
	m := (totalSeconds % 3600) / 60
	s := totalSeconds % 60
	return fmt.Sprintf("%d:%02d:%02d", h, m, s)
}

func renderFleetProjectRailLinks(project fleetProjectState) string {
	if fleetProjectHasAttentionCTA(project) {
		op := project.OperatorState
		reason := strings.TrimSpace(op.NextAction)
		if reason == "" {
			reason = strings.TrimSpace(op.Summary)
		}
		if reason == "" {
			reason = "Open the attention tab for this project."
		}
		label := fleetProjectAttentionCTALabel(project)
		return `<button type="button" class="rail-cta rail-cta-attention project-attention-cta" data-project="` + html.EscapeString(project.Name) + `" title="` + html.EscapeString(reason) + `">` + html.EscapeString(label) + ` &rarr;</button>`
	}
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
	op := project.OperatorState
	if (strings.TrimSpace(op.Kind) == "" || op.Kind == "idle") && op.Tone == "healthy" {
		return "Idle · healthy"
	}
	if label := strings.TrimSpace(op.Label); label != "" {
		return label
	}
	return "Idle"
}

func fleetProjectStateKindKey(project fleetProjectState) string {
	op := project.OperatorState
	key := strings.TrimSpace(op.Kind)
	if key == "" {
		key = "idle"
	}
	if key == "idle" && op.Tone == "healthy" {
		key = "healthy_idle"
	}
	return key
}

func fleetProjectStatePillClass(project fleetProjectState) string {
	if fleetProjectUnconfigured(project) {
		return "rail-state-unconfigured"
	}
	return "rail-state-" + fleetCSSClassToken(fleetProjectStateKindKey(project))
}

func fleetProjectRailStateClass(project fleetProjectState) string {
	if fleetProjectUnconfigured(project) {
		return "project-row-unconfigured"
	}
	return "project-row-" + fleetCSSClassToken(fleetProjectStateKindKey(project))
}

func fleetProjectHasAttentionCTA(project fleetProjectState) bool {
	if fleetProjectUnconfigured(project) {
		return false
	}
	if project.NeedsAttention > 0 {
		return true
	}
	switch strings.TrimSpace(project.OperatorState.Kind) {
	case "attention", "stale_worker", "dispatch_failure", "error":
		return true
	}
	return false
}

func fleetProjectAttentionCTALabel(project fleetProjectState) string {
	op := project.OperatorState
	switch strings.TrimSpace(op.Kind) {
	case "attention":
		return "Open attention"
	case "stale_worker":
		return "Review stale worker"
	case "dispatch_failure":
		return "Resolve dispatch"
	case "error":
		return "Fix project error"
	}
	if label := strings.TrimSpace(op.Label); label != "" {
		return "Open " + label
	}
	return "Open attention"
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

// fleetActionAudit returns an audit recorder closure for the given project,
// or nil if the project has no usable state dir. The closure writes to the
// project's audit log via the same mechanism as handleFleetAuditLog.
func (s *FleetServer) fleetActionAudit(project FleetProject) actionAuditRecorder {
	stateDir, err := s.fleetAuditLogStateDir(project.Name)
	if err != nil || strings.TrimSpace(stateDir) == "" {
		return nil
	}
	projectName := project.Name
	return func(actor, action, target, reason string) error {
		entry := fleetAuditLogEntry{
			AuditID:   newFleetAuditID(),
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Actor:     actor,
			Action:    action,
			Target:    target,
			Reason:    reason,
			Project:   projectName,
		}
		return appendFleetAuditLogEntry(stateDir, entry)
	}
}
