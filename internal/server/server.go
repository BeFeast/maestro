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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/server/web"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

// Server provides an HTTP API for runtime observability.
type Server struct {
	cfg       *config.Config
	refreshCh chan<- struct{}
	srv       *http.Server

	// gh is the GitHub client used by the safe-action dispatcher when the
	// server is not in read-only mode. nil disables safe actions (handlers
	// fall through to 501). Tests inject a fake via SetActionDeps.
	gh actionGitHubClient
	// audit, when non-nil, records executed safe actions. nil disables
	// auditing without disabling the action.
	audit actionAuditRecorder
	// auth gates every mutating endpoint (#487). When the configured token
	// is empty (default), behaviour is unchanged; when it is non-empty,
	// /actions, /approvals/.../{approve|reject}, and /refresh require
	// Authorization: Bearer <token>.
	auth authChecker
}

// SetActionDeps wires the GitHub client and (optional) audit recorder used
// by the safe-action dispatcher. Call this on startup before Serve, or in
// tests to inject a fake gh client.
func (s *Server) SetActionDeps(gh actionGitHubClient, audit actionAuditRecorder) {
	s.gh = gh
	s.audit = audit
}

// New creates a new Server. refreshCh is used to trigger immediate poll cycles.
func New(cfg *config.Config, refreshCh chan<- struct{}) *Server {
	auth := authChecker{}
	if cfg != nil {
		auth = newAuthChecker(cfg.Server.Auth)
	}
	return &Server{
		cfg:       cfg,
		refreshCh: refreshCh,
		auth:      auth,
	}
}

// SetAuthForTest replaces the auth checker. Test-only helper — production
// code wires auth via cfg.Server.Auth at New().
func (s *Server) SetAuthForTest(token, actorName string) {
	s.auth = newAuthCheckerForTest(token, actorName)
}

// Start begins serving HTTP on the configured port. It blocks until the server
// is shut down. Returns nil if port is 0 (disabled).
func (s *Server) Start(ctx context.Context) error {
	if s.cfg.Server.Port == 0 {
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/state", s.handleState)
	mux.HandleFunc("/api/v1/workers", s.handleWorkers)
	mux.HandleFunc("/api/v1/logs/", s.handleLog)
	mux.HandleFunc("/api/v1/refresh", s.handleRefresh)
	mux.HandleFunc("/api/v1/actions", s.handleAction)
	mux.HandleFunc("/api/v1/approvals/", s.handleApproval)
	mux.HandleFunc("/api/v1/", s.handleIssue)
	mux.Handle("/static/", web.StaticHandler())
	mux.HandleFunc("/", s.handleDashboard)

	host := strings.TrimSpace(s.cfg.Server.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(s.cfg.Server.Port))
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

	log.Printf("[server] listening on %s", addr)
	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

func (s *Server) loadState() (*state.State, error) {
	return state.Load(s.cfg.StateDir)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// stateResponse is the JSON shape for GET /api/v1/state.
type stateResponse struct {
	Repo                string                         `json:"repo"`
	MaxParallel         int                            `json:"max_parallel"`
	ReadOnly            bool                           `json:"read_only"`
	Outcome             outcome.Status                 `json:"outcome"`
	Actions             []controlAction                `json:"actions,omitempty"`
	SupervisorPolicy    config.SupervisorConfig        `json:"supervisor_policy"`
	All                 []sessionInfo                  `json:"all"`
	Running             []sessionInfo                  `json:"running"`
	PROpen              []sessionInfo                  `json:"pr_open"`
	Queued              []sessionInfo                  `json:"queued"`
	TokenTotals         tokenTotalsInfo                `json:"token_totals"`
	Summary             map[string]int                 `json:"summary"`
	StuckStates         []state.SupervisorStuckState   `json:"stuck_states,omitempty"`
	Supervisor          supervisorInfo                 `json:"supervisor"`
	SupervisorLatest    *state.SupervisorDecision      `json:"supervisor_latest,omitempty"`
	SupervisorDecisions []state.SupervisorDecision     `json:"supervisor_decisions,omitempty"`
	Approvals           []state.Approval               `json:"approvals,omitempty"`
	BackendHealth       map[string]state.BackendHealth `json:"backend_health,omitempty"`
}

type supervisorInfo struct {
	HasRun          bool                    `json:"has_run"`
	EmptyState      string                  `json:"empty_state,omitempty"`
	Latest          *supervisorDecisionInfo `json:"latest,omitempty"`
	LastSafeAction  *supervisorActionInfo   `json:"last_safe_action,omitempty"`
	ApprovalActions []supervisorActionInfo  `json:"approval_actions,omitempty"`
}

type supervisorDecisionInfo struct {
	ID                string                         `json:"id"`
	CreatedAt         time.Time                      `json:"created_at"`
	Project           string                         `json:"project"`
	Mode              string                         `json:"mode"`
	PolicyRule        string                         `json:"policy_rule,omitempty"`
	Status            string                         `json:"status,omitempty"`
	Summary           string                         `json:"summary"`
	RecommendedAction string                         `json:"recommended_action"`
	OperatorSentence  string                         `json:"operator_sentence"`
	Target            *state.SupervisorTarget        `json:"target,omitempty"`
	TargetLinks       []targetLinkInfo               `json:"target_links,omitempty"`
	Risk              string                         `json:"risk"`
	Confidence        float64                        `json:"confidence"`
	ErrorClass        string                         `json:"error_class,omitempty"`
	Reasons           []string                       `json:"reasons,omitempty"`
	Mutations         []state.SupervisorMutation     `json:"mutations,omitempty"`
	StuckStates       []state.SupervisorStuckState   `json:"stuck_states,omitempty"`
	Outcome           *outcome.Status                `json:"outcome,omitempty"`
	StuckReasons      []string                       `json:"stuck_reasons,omitempty"`
	ProjectState      state.SupervisorProjectState   `json:"project_state"`
	QueueAnalysis     *state.SupervisorQueueAnalysis `json:"queue_analysis,omitempty"`
	Queue             *supervisorQueueInfo           `json:"queue,omitempty"`
	ApprovalID        string                         `json:"approval_id,omitempty"`
}

type supervisorActionInfo struct {
	Action           string                  `json:"action"`
	Summary          string                  `json:"summary"`
	OperatorSentence string                  `json:"operator_sentence"`
	Risk             string                  `json:"risk"`
	CreatedAt        time.Time               `json:"created_at"`
	Target           *state.SupervisorTarget `json:"target,omitempty"`
	TargetLinks      []targetLinkInfo        `json:"target_links,omitempty"`
	Disabled         bool                    `json:"disabled"`
	DisabledReason   string                  `json:"disabled_reason,omitempty"`
}

type targetLinkInfo struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	URL   string `json:"url,omitempty"`
}

type supervisorQueueInfo struct {
	Enabled  bool   `json:"enabled"`
	Label    string `json:"label,omitempty"`
	Position int    `json:"position,omitempty"`
	Total    int    `json:"total,omitempty"`
}

type tokenTotalsInfo struct {
	Active int `json:"active"`
	Total  int `json:"total"`
}

type controlAction struct {
	ID               string `json:"id"`
	Label            string `json:"label"`
	Description      string `json:"description,omitempty"`
	Scope            string `json:"scope"`
	Target           string `json:"target,omitempty"`
	IssueNumber      int    `json:"issue_number,omitempty"`
	PRNumber         int    `json:"pr_number,omitempty"`
	Mutating         bool   `json:"mutating"`
	RequiresApproval bool   `json:"requires_approval"`
	ApprovalPolicy   string `json:"approval_policy,omitempty"`
	Disabled         bool   `json:"disabled"`
	DisabledReason   string `json:"disabled_reason,omitempty"`
	Method           string `json:"method,omitempty"`
	Endpoint         string `json:"endpoint,omitempty"`
}

type controlActionRequest struct {
	ActionID    string `json:"action_id"`
	Project     string `json:"project,omitempty"`
	Slot        string `json:"slot,omitempty"`
	IssueNumber int    `json:"issue_number,omitempty"`
	PRNumber    int    `json:"pr_number,omitempty"`
	ApprovalID  string `json:"approval_id,omitempty"`

	// Body is the comment text for add_issue_comment safe actions.
	Body string `json:"body,omitempty"`
	// Actor / Reason are recorded in the audit log for executed actions.
	Actor  string `json:"actor,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type sessionInfo struct {
	Slot              string `json:"slot"`
	IssueNumber       int    `json:"issue_number"`
	IssueTitle        string `json:"issue_title"`
	IssueURL          string `json:"issue_url,omitempty"`
	Status            string `json:"status"`
	DisplayStatus     string `json:"display_status,omitempty"`
	StatusReason      string `json:"status_reason,omitempty"`
	NextAction        string `json:"next_action,omitempty"`
	NeedsAttention    bool   `json:"needs_attention,omitempty"`
	Live              bool   `json:"live"`
	Backend           string `json:"backend,omitempty"`
	PRNumber          int    `json:"pr_number,omitempty"`
	PRURL             string `json:"pr_url,omitempty"`
	TokensUsedAttempt int    `json:"tokens_used_attempt"`
	TokensUsedTotal   int    `json:"tokens_used_total"`
	// Runtime / RuntimeSeconds (legacy fields, kept for backwards
	// compatibility) reflect WORKFLOW elapsed time — StartedAt to
	// FinishedAt — including PR-open / CI / Greptile / merge waiting.
	// Prefer WorkerRuntimeSeconds for "how long the coding agent ran"
	// and WorkflowRuntimeSeconds for "how long the whole session took";
	// see #426.
	Runtime                string                  `json:"runtime"`
	RuntimeSeconds         int64                   `json:"runtime_seconds"`
	WorkerRuntime          string                  `json:"worker_runtime"`
	WorkerRuntimeSeconds   int64                   `json:"worker_runtime_seconds"`
	WorkflowRuntime        string                  `json:"workflow_runtime"`
	WorkflowRuntimeSeconds int64                   `json:"workflow_runtime_seconds"`
	PROpenRuntime          string                  `json:"pr_open_runtime,omitempty"`
	PROpenRuntimeSeconds   int64                   `json:"pr_open_runtime_seconds,omitempty"`
	StartedAt              string                  `json:"started_at"`
	FinishedAt             string                  `json:"finished_at,omitempty"`
	WorkerEndedAt          string                  `json:"worker_ended_at,omitempty"`
	PROpenedAt             string                  `json:"pr_opened_at,omitempty"`
	NextRetryAt            string                  `json:"next_retry_at,omitempty"`
	PID                    int                     `json:"pid,omitempty"`
	Alive                  *bool                   `json:"alive,omitempty"`
	Worktree               string                  `json:"worktree,omitempty"`
	Branch                 string                  `json:"branch,omitempty"`
	TmuxSession            string                  `json:"tmux_session,omitempty"`
	HasLog                 bool                    `json:"has_log"`
	RetryCount             int                     `json:"retry_count,omitempty"`
	LastNotification       string                  `json:"last_notification,omitempty"`
	BackendSelection       *state.BackendSelection `json:"backend_selection,omitempty"`
	// Attribution is the per-segment provider/model/variant/effort timeline
	// recorded across spawn / respawn / fallover (#513, PR #518). The SPA
	// renders the active segment inline on the session card and the full
	// list with EndReason between segments in the worker drawer.
	Attribution []state.BackendAttribution `json:"attribution,omitempty"`
	Actions     []controlAction            `json:"actions,omitempty"`
}

func makeSessionInfo(repo, slot string, sess *state.Session) sessionInfo {
	now := time.Now().UTC()
	info := sessionInfo{
		Slot:              slot,
		IssueNumber:       sess.IssueNumber,
		IssueTitle:        sess.IssueTitle,
		IssueURL:          githubIssueURL(repo, sess.IssueNumber),
		Status:            string(sess.Status),
		Backend:           sess.Backend,
		PRNumber:          sess.PRNumber,
		PRURL:             githubPRURL(repo, sess.PRNumber),
		TokensUsedAttempt: sess.TokensUsedAttempt,
		TokensUsedTotal:   sess.TokensUsedTotal,
		StartedAt:         sess.StartedAt.Format(time.RFC3339),
		Worktree:          sess.Worktree,
		Branch:            sess.Branch,
		TmuxSession:       watchSessionName(slot, sess),
		HasLog:            strings.TrimSpace(sess.LogFile) != "",
		RetryCount:        sess.RetryCount,
		LastNotification:  sess.LastNotifiedStatus,
		BackendSelection:  sess.BackendSelection,
		Attribution:       sess.Attribution,
		Live:              state.SessionLiveAt(sess, now),
	}

	// Calculate runtime breakdown (#426). The workflow runtime is the
	// total session wall-clock (StartedAt -> FinishedAt or now). The
	// worker runtime is the time the coding agent was actually executing
	// (StartedAt -> WorkerEndedAt). When WorkerEndedAt is not yet recorded
	// (legacy sessions or transitions that did not flow through MarkWorkerEnded),
	// fall back to FinishedAt and finally to now, so historical rows remain
	// understandable.
	end := now
	if sess.FinishedAt != nil {
		end = *sess.FinishedAt
		info.FinishedAt = sess.FinishedAt.Format(time.RFC3339)
	}
	workflowDur := end.Sub(sess.StartedAt).Round(time.Second)
	info.Runtime = workflowDur.String()
	info.RuntimeSeconds = int64(workflowDur / time.Second)
	info.WorkflowRuntime = workflowDur.String()
	info.WorkflowRuntimeSeconds = info.RuntimeSeconds

	workerEnd := end
	if sess.WorkerEndedAt != nil {
		workerEnd = *sess.WorkerEndedAt
		info.WorkerEndedAt = sess.WorkerEndedAt.Format(time.RFC3339)
	} else if sess.Status == state.StatusRunning {
		workerEnd = now
	}
	workerDur := workerEnd.Sub(sess.StartedAt).Round(time.Second)
	if workerDur < 0 {
		workerDur = 0
	}
	info.WorkerRuntime = workerDur.String()
	info.WorkerRuntimeSeconds = int64(workerDur / time.Second)

	if sess.PROpenedAt != nil {
		info.PROpenedAt = sess.PROpenedAt.Format(time.RFC3339)
		prEnd := end
		if sess.Status == state.StatusPROpen {
			prEnd = now
		}
		prDur := prEnd.Sub(*sess.PROpenedAt).Round(time.Second)
		if prDur < 0 {
			prDur = 0
		}
		info.PROpenRuntime = prDur.String()
		info.PROpenRuntimeSeconds = int64(prDur / time.Second)
	}

	if sess.Status == state.StatusRunning {
		info.PID = sess.PID
		alive := worker.IsAlive(sess.PID)
		info.Alive = &alive
	}
	if sess.NextRetryAt != nil {
		info.NextRetryAt = sess.NextRetryAt.Format(time.RFC3339)
	}
	attention := state.SessionAttentionForAt(sess, info.Alive, now)
	if displayStatus := state.SessionDisplayStatusForAt(sess, info.Alive, now); displayStatus != "" && displayStatus != info.Status {
		info.DisplayStatus = displayStatus
	}
	info.StatusReason = attention.Reason
	info.NextAction = attention.NextAction
	info.NeedsAttention = attention.NeedsAttention

	// #566: an old dead / failed / conflict_failed worker is reconcilable,
	// not actionable. Drop `needs_attention` and the derived `live` flag
	// once the session ages past FleetAttentionTTL so a 2-day-old zombie
	// session cannot dominate the fleet verdict. retry_exhausted with an
	// open PR (#564) and any session with a scheduled retry stay live.
	if attention.NeedsAttention && !state.SessionAttentionActionableAt(sess, now) {
		info.NeedsAttention = false
		info.Live = false
	}

	return info
}

func githubIssueURL(repo string, issueNumber int) string {
	if issueNumber <= 0 || !validGitHubRepo(repo) {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/issues/%d", strings.TrimSpace(repo), issueNumber)
}

func githubPRURL(repo string, prNumber int) string {
	if prNumber <= 0 || !validGitHubRepo(repo) {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", strings.TrimSpace(repo), prNumber)
}

func buildSupervisorInfo(cfg *config.Config, st *state.State) supervisorInfo {
	info := supervisorInfo{
		HasRun:          len(st.SupervisorDecisions) > 0,
		EmptyState:      "No Supervisor has run yet. Run the supervisor to record orchestration rationale.",
		ApprovalActions: make([]supervisorActionInfo, 0),
	}
	if !info.HasRun {
		return info
	}

	latest := st.LatestSupervisorDecision()
	if latest != nil {
		info.EmptyState = ""
		info.Latest = makeSupervisorDecisionInfo(cfg, st, *latest)
		if latest.Risk != "" && latest.Risk != "safe" && latest.RecommendedAction != "" {
			info.ApprovalActions = append(info.ApprovalActions, makeSupervisorActionInfo(cfg, *latest, true,
				"Supervisor controls are not implemented yet; this read-only panel only shows the required action."))
		}
	}

	if safe := latestSafeSupervisorDecision(st); safe != nil {
		action := makeSupervisorActionInfo(cfg, *safe, false, "")
		info.LastSafeAction = &action
	}
	return info
}

func makeSupervisorDecisionInfo(cfg *config.Config, st *state.State, decision state.SupervisorDecision) *supervisorDecisionInfo {
	return &supervisorDecisionInfo{
		ID:                decision.ID,
		CreatedAt:         decision.CreatedAt,
		Project:           decision.Project,
		Mode:              decision.Mode,
		PolicyRule:        decision.PolicyRule,
		Status:            decision.Status,
		Summary:           decision.Summary,
		RecommendedAction: decision.RecommendedAction,
		OperatorSentence:  supervisorOperatorSentence(decision.RecommendedAction, decision.Summary, decision.Target),
		Target:            decision.Target,
		TargetLinks:       supervisorTargetLinks(cfg.Repo, decision.Target),
		Risk:              decision.Risk,
		Confidence:        decision.Confidence,
		ErrorClass:        decision.ErrorClass,
		Reasons:           decision.Reasons,
		Mutations:         decision.Mutations,
		StuckStates:       decision.StuckStates,
		Outcome:           decision.Outcome,
		StuckReasons:      supervisorStuckReasons(decision),
		ProjectState:      decision.ProjectState,
		QueueAnalysis:     decision.QueueAnalysis,
		Queue:             supervisorQueueInfoForDecision(cfg, st, decision),
		ApprovalID:        decision.ApprovalID,
	}
}

func makeSupervisorActionInfo(cfg *config.Config, decision state.SupervisorDecision, disabled bool, disabledReason string) supervisorActionInfo {
	return supervisorActionInfo{
		Action:           decision.RecommendedAction,
		Summary:          decision.Summary,
		OperatorSentence: supervisorOperatorSentence(decision.RecommendedAction, decision.Summary, decision.Target),
		Risk:             decision.Risk,
		CreatedAt:        decision.CreatedAt,
		Target:           decision.Target,
		TargetLinks:      supervisorTargetLinks(cfg.Repo, decision.Target),
		Disabled:         disabled,
		DisabledReason:   disabledReason,
	}
}

func supervisorOperatorSentence(action, summary string, target *state.SupervisorTarget) string {
	raw := strings.TrimSpace(action)
	if raw == "" {
		raw = "none"
	}
	summary = strings.TrimSpace(summary)
	issue := supervisorIssuePhrase(target)
	pr := supervisorPRPhrase(target)
	session := supervisorSessionPhrase(target)

	switch raw {
	case "none":
		return "Skipped this tick because no safe action was available."
	case "monitor_open_pr":
		return fmt.Sprintf("Watching %s until checks and review pass.", pr)
	case "merge_pr":
		return fmt.Sprintf("Merging %s now that checks and review allow it.", pr)
	case "approve_merge":
		return fmt.Sprintf("Ready to merge %s when approval and checks allow it.", pr)
	case "skip_wave":
		return "Skipped this tick because the wave policy held the next worker."
	case "spawn_worker":
		return fmt.Sprintf("Starting a worker for %s.", issue)
	case "label_issue_ready":
		return fmt.Sprintf("Preparing %s for the worker queue.", issue)
	case "review_retry_exhausted":
		if target != nil && target.PR > 0 {
			return fmt.Sprintf("Reviewing failed feedback on %s before retrying work.", pr)
		}
		return fmt.Sprintf("Reviewing retry-exhausted work for %s before dispatching again.", issue)
	case "check_outcome_health":
		return "Checking runtime outcome health before sending more work."
	case "wait_for_review":
		return fmt.Sprintf("Waiting on review for %s.", pr)
	case "wait_for_ci":
		return fmt.Sprintf("Waiting on CI for %s.", pr)
	case "wait_for_running_worker", "wait_for_worker":
		return fmt.Sprintf("Waiting for %s to finish.", session)
	case "wait_for_capacity":
		return "Waiting for an available worker slot."
	case "wait_for_ordered_queue":
		return "Waiting for the ordered queue policy to allow the next issue."
	}

	if summary != "" {
		return fmt.Sprintf("Supervisor chose %s. %s", raw, summary)
	}
	return fmt.Sprintf("Supervisor chose %s. Inspect diagnostics for details.", raw)
}

func supervisorIssuePhrase(target *state.SupervisorTarget) string {
	if target != nil && target.Issue > 0 {
		return fmt.Sprintf("issue #%d", target.Issue)
	}
	return "the selected issue"
}

func supervisorPRPhrase(target *state.SupervisorTarget) string {
	if target != nil && target.PR > 0 {
		return fmt.Sprintf("PR #%d", target.PR)
	}
	return "the open PR"
}

func supervisorSessionPhrase(target *state.SupervisorTarget) string {
	if target != nil && strings.TrimSpace(target.Session) != "" {
		return "worker " + strings.TrimSpace(target.Session)
	}
	return "the running worker"
}

func latestSafeSupervisorDecision(st *state.State) *state.SupervisorDecision {
	var latest *state.SupervisorDecision
	for i := range st.SupervisorDecisions {
		decision := &st.SupervisorDecisions[i]
		if decision.Risk != "safe" {
			continue
		}
		if latest == nil || decision.CreatedAt.After(latest.CreatedAt) {
			latest = decision
		}
	}
	return latest
}

func supervisorTargetLinks(repo string, target *state.SupervisorTarget) []targetLinkInfo {
	if target == nil {
		return nil
	}
	links := make([]targetLinkInfo, 0, 3)
	if target.Issue > 0 {
		links = append(links, targetLinkInfo{
			Kind:  "issue",
			Label: fmt.Sprintf("Issue #%d", target.Issue),
			URL:   githubIssueURL(repo, target.Issue),
		})
	}
	if target.PR > 0 {
		links = append(links, targetLinkInfo{
			Kind:  "pr",
			Label: fmt.Sprintf("PR #%d", target.PR),
			URL:   githubPRURL(repo, target.PR),
		})
	}
	if strings.TrimSpace(target.Session) != "" {
		links = append(links, targetLinkInfo{
			Kind:  "session",
			Label: "Session " + strings.TrimSpace(target.Session),
		})
	}
	return links
}

func supervisorStuckReasons(decision state.SupervisorDecision) []string {
	if len(decision.StuckStates) > 0 {
		reasons := make([]string, 0, len(decision.StuckStates))
		for _, stuck := range decision.StuckStates {
			if strings.TrimSpace(stuck.Summary) != "" {
				reasons = append(reasons, stuck.Summary)
			}
		}
		return reasons
	}

	action := strings.TrimSpace(decision.RecommendedAction)
	if action == "none" || strings.HasPrefix(action, "wait_") || decision.Risk == "approval_gated" {
		return decision.Reasons
	}

	var reasons []string
	for _, reason := range decision.Reasons {
		lower := strings.ToLower(reason)
		if strings.Contains(lower, "blocked") || strings.Contains(lower, "skipped") || strings.Contains(lower, "exhausted") || strings.Contains(lower, "no eligible") || strings.Contains(lower, "no worker slot") || strings.Contains(lower, "missing") {
			reasons = append(reasons, reason)
		}
	}
	return reasons
}

func supervisorQueueInfoForDecision(cfg *config.Config, st *state.State, decision state.SupervisorDecision) *supervisorQueueInfo {
	if cfg == nil || !cfg.Supervisor.OrderedQueueActive() {
		return nil
	}
	queue := &supervisorQueueInfo{
		Enabled: true,
		Label:   "Supervisor ordered issue queue",
		Total:   len(cfg.Supervisor.OrderedQueue.Issues),
	}
	if decision.Target == nil || decision.Target.Issue <= 0 {
		return queue
	}
	for i, issue := range cfg.Supervisor.OrderedQueue.Issues {
		if issue == decision.Target.Issue {
			queue.Position = i + 1
			return queue
		}
	}
	return queue
}

func validGitHubRepo(repo string) bool {
	repo = strings.TrimSpace(repo)
	parts := strings.Split(repo, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

func watchSessionName(slot string, sess *state.Session) string {
	if strings.TrimSpace(sess.TmuxSession) != "" {
		return sess.TmuxSession
	}
	return worker.TmuxSessionName(slot)
}

func allSessionInfos(repo string, st *state.State) []sessionInfo {
	infos := make([]sessionInfo, 0, len(st.Sessions))
	for slot, sess := range st.Sessions {
		infos = append(infos, makeSessionInfo(repo, slot, sess))
	}
	applySupervisorAttention(infos, st.LatestSupervisorDecision())
	applyProjectStatusDisplay(infos, st)
	sort.Slice(infos, func(i, j int) bool {
		left, right := infos[i], infos[j]
		li := state.StatusPriority(state.SessionStatus(left.Status))
		ri := state.StatusPriority(state.SessionStatus(right.Status))
		if li != ri {
			return li < ri
		}
		if left.StartedAt != right.StartedAt {
			return left.StartedAt > right.StartedAt
		}
		return left.Slot < right.Slot
	})
	return infos
}

func applyProjectStatusDisplay(infos []sessionInfo, st *state.State) {
	if st == nil || len(st.ProjectStatusSync) == 0 {
		return
	}
	for i := range infos {
		record, ok := st.ProjectStatusSync[infos[i].IssueNumber]
		if !ok || strings.TrimSpace(record.Status) != "blocked" {
			continue
		}
		if !projectBlockedOverridesWorkerStatus(infos[i].Status) {
			continue
		}
		// #566: a terminal session that still has an open PR is a stuck PR
		// needing operator attention, not an issue parked on a dependency.
		// The run-loop syncs the Project item to "Blocked" on retry
		// exhaustion, so without this guard a green-but-gate-blocked PR is
		// hidden and the fleet verdict falsely reads "idle, healthy".
		if infos[i].PRNumber > 0 {
			continue
		}
		infos[i].DisplayStatus = "blocked"
		infos[i].NeedsAttention = false
		infos[i].Live = false
		infos[i].StatusReason = "Issue is blocked in GitHub Project; this previous worker attempt is not active work."
		infos[i].NextAction = "Unblock the issue or move it to a runnable project status before starting new work."
	}
}

func projectBlockedOverridesWorkerStatus(status string) bool {
	switch state.SessionStatus(status) {
	case state.StatusDead, state.StatusFailed, state.StatusConflictFailed, state.StatusRetryExhausted:
		return true
	default:
		return false
	}
}

func applySupervisorAttention(infos []sessionInfo, latest *state.SupervisorDecision) {
	if latest == nil || len(latest.StuckStates) == 0 {
		return
	}
	for i := range infos {
		for _, stuck := range latest.StuckStates {
			if !stuckTargetsSession(stuck, infos[i]) {
				continue
			}
			if staleReviewFeedbackResolved(stuck, infos[i]) {
				continue
			}
			attention := supervisorStuckNeedsAttention(stuck)
			if !attention {
				continue
			}
			if strings.TrimSpace(stuck.Summary) != "" {
				infos[i].StatusReason = stuck.Summary
			}
			if strings.TrimSpace(stuck.RecommendedAction) != "" {
				infos[i].NextAction = stuck.RecommendedAction
			}
			if attention {
				infos[i].NeedsAttention = true
			}
			break
		}
	}
}

func staleReviewFeedbackResolved(stuck state.SupervisorStuckState, info sessionInfo) bool {
	if stuck.Code != "stale_review_feedback" {
		return false
	}
	status := state.SessionStatus(info.Status)
	if status == state.StatusDone {
		return true
	}
	return state.IsTerminal(status) && stuck.Target != nil && info.PRNumber > 0 && stuck.Target.PR == info.PRNumber
}

func stuckTargetsSession(stuck state.SupervisorStuckState, info sessionInfo) bool {
	target := stuck.Target
	if target == nil {
		return false
	}
	if session := strings.TrimSpace(target.Session); session != "" {
		return session == info.Slot
	}
	if target.PR > 0 && target.PR == info.PRNumber {
		return true
	}
	return target.Issue > 0 && target.Issue == info.IssueNumber
}

func supervisorStuckNeedsAttention(stuck state.SupervisorStuckState) bool {
	switch stuck.Code {
	case "retry_exhausted", "retry_exhausted_open_pr", "dead_running_pid", "stale_worker_logs", "worker_timeout", "failing_checks", "closed_pr_with_active_session", "unmergeable_pr", "stale_review_feedback", "greptile_not_approved", state.StuckPolicyBlocksMerge, state.StuckReviewRepairSpawned, state.StuckReviewRepairExhausted:
		return true
	}
	return stuck.Severity == "blocked"
}

func sessionInfosWithActions(repo string, st *state.State, readOnly bool, endpoint string) []sessionInfo {
	infos := allSessionInfos(repo, st)
	for i := range infos {
		infos[i].Actions = workerActionAffordances(readOnly, endpoint, infos[i])
	}
	return infos
}

const (
	controlApprovalPolicyManual = "manual_approval_required"
	controlApprovalPolicySafe   = "safe_direct"
)

func projectActionAffordances(readOnly bool, endpoint, target string) []controlAction {
	return []controlAction{
		newSafeControlAction("mark_issue_ready", "Mark ready", "Add the maestro-ready label so the supervisor picks the issue up.", "project", target, 0, 0, endpoint, readOnly),
		newSafeControlAction("mark_issue_blocked", "Mark blocked", "Add the blocked label so the supervisor holds the issue.", "project", target, 0, 0, endpoint, readOnly),
	}
}

func workerActionAffordances(readOnly bool, endpoint string, worker sessionInfo) []controlAction {
	merge := newApprovalControlAction("approve_merge", "Approve merge", "Enqueue a cautious-gate approval to merge this PR.", "pull_request", worker.Slot, worker.IssueNumber, worker.PRNumber, endpoint, readOnly)
	if worker.PRNumber == 0 {
		// PR-less workers can't enqueue merge_pr regardless of read-only;
		// surface a verb-specific reason. In read-only mode this still
		// mentions read-only so the operator knows enabling writes alone
		// isn't sufficient — the worker needs to open a PR first.
		merge.Disabled = true
		if readOnly {
			merge.DisabledReason = readOnlyDisabledReason() + " Additionally, this worker has no PR to approve."
		} else {
			merge.DisabledReason = "No PR is associated with this worker; approve merge becomes available once a PR is open."
		}
	}
	return []controlAction{
		newApprovalControlAction("restart_worker", "Restart", "Enqueue a cautious-gate approval to restart this worker.", "worker", worker.Slot, worker.IssueNumber, worker.PRNumber, endpoint, readOnly),
		newApprovalControlAction("stop_worker", "Stop", "Enqueue a cautious-gate approval to stop this worker.", "worker", worker.Slot, worker.IssueNumber, worker.PRNumber, endpoint, readOnly),
		newSafeControlAction("mark_issue_ready", "Mark ready", "Add the maestro-ready label so the supervisor picks the issue up.", "issue", worker.Slot, worker.IssueNumber, worker.PRNumber, endpoint, readOnly),
		newSafeControlAction("mark_issue_blocked", "Mark blocked", "Add the blocked label so the supervisor holds the issue.", "issue", worker.Slot, worker.IssueNumber, worker.PRNumber, endpoint, readOnly),
		merge,
	}
}

// newSafeControlAction returns a button for a safe verb (label/comment).
// The control fires synchronously through dispatchSafeAction (no approval
// queue), so requires_approval=false. Disabled only in read-only mode.
func newSafeControlAction(id, label, description, scope, target string, issueNumber, prNumber int, endpoint string, readOnly bool) controlAction {
	a := controlAction{
		ID:               id,
		Label:            label,
		Description:      description,
		Scope:            scope,
		Target:           target,
		IssueNumber:      issueNumber,
		PRNumber:         prNumber,
		Mutating:         true,
		RequiresApproval: false,
		ApprovalPolicy:   controlApprovalPolicySafe,
		Method:           http.MethodPost,
		Endpoint:         endpoint,
	}
	if readOnly {
		a.Disabled = true
		a.DisabledReason = readOnlyDisabledReason()
	}
	return a
}

// newApprovalControlAction returns a button for an approval-gated verb
// (restart_worker / stop_worker / approve_merge). The control enqueues a
// pending Approval through dispatchApprovalAction; the executor / operator
// completes the cautious-gate verb. Disabled only in read-only mode.
func newApprovalControlAction(id, label, description, scope, target string, issueNumber, prNumber int, endpoint string, readOnly bool) controlAction {
	a := controlAction{
		ID:               id,
		Label:            label,
		Description:      description,
		Scope:            scope,
		Target:           target,
		IssueNumber:      issueNumber,
		PRNumber:         prNumber,
		Mutating:         true,
		RequiresApproval: true,
		ApprovalPolicy:   controlApprovalPolicyManual,
		Method:           http.MethodPost,
		Endpoint:         endpoint,
	}
	if readOnly {
		a.Disabled = true
		a.DisabledReason = readOnlyDisabledReason()
	}
	return a
}

func readOnlyDisabledReason() string {
	return "Read-only mode keeps write actions disabled. Restart maestro with --read-only=false (and an auth token) to enable."
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	st, err := s.loadState()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load state: %v", err))
		return
	}

	// #600: clear elapsed-RetryAfter / TTL-expired / proven-recovered
	// cooldown entries so dashboard consumers see a truthful health map
	// even between orchestrator cycles. Done here, not inside
	// buildStateResponse, so the side effect is visible at the call site.
	state.ReconcileBackendHealth(st, time.Now().UTC())
	writeJSON(w, http.StatusOK, buildStateResponse(s.cfg, st))
}

func buildStateResponse(cfg *config.Config, st *state.State) stateResponse {
	latestDecision := st.LatestSupervisorDecision()
	resp := stateResponse{
		Repo:                cfg.Repo,
		MaxParallel:         cfg.MaxParallel,
		ReadOnly:            cfg.Server.ReadOnly,
		Outcome:             outcomeStatusForState(cfg, st),
		Actions:             projectActionAffordances(cfg.Server.ReadOnly, "/api/v1/actions", cfg.Repo),
		BackendHealth:       st.BackendHealth,
		SupervisorPolicy:    cfg.Supervisor,
		All:                 make([]sessionInfo, 0, len(st.Sessions)),
		Running:             make([]sessionInfo, 0),
		PROpen:              make([]sessionInfo, 0),
		Queued:              make([]sessionInfo, 0),
		Summary:             make(map[string]int),
		Supervisor:          buildSupervisorInfo(cfg, st),
		SupervisorLatest:    latestDecision,
		SupervisorDecisions: st.SupervisorDecisions,
		Approvals:           st.Approvals,
	}
	if latestDecision != nil {
		resp.StuckStates = latestDecision.StuckStates
	}

	var activeTokens, totalTokens int
	for _, info := range sessionInfosWithActions(cfg.Repo, st, cfg.Server.ReadOnly, "/api/v1/actions") {
		resp.All = append(resp.All, info)
		summaryStatus := info.Status
		if info.DisplayStatus != "" {
			summaryStatus = info.DisplayStatus
		}
		resp.Summary[summaryStatus]++
		totalTokens += info.TokensUsedTotal

		switch state.SessionStatus(info.Status) {
		case state.StatusRunning:
			resp.Running = append(resp.Running, info)
			activeTokens += info.TokensUsedTotal
		case state.StatusPROpen:
			resp.PROpen = append(resp.PROpen, info)
			activeTokens += info.TokensUsedTotal
		case state.StatusQueued:
			resp.Queued = append(resp.Queued, info)
		}
	}

	resp.TokenTotals = tokenTotalsInfo{
		Active: activeTokens,
		Total:  totalTokens,
	}

	return resp
}

func outcomeStatusForState(cfg *config.Config, st *state.State) outcome.Status {
	if cfg == nil || st == nil {
		return outcome.StatusFor(outcome.Brief{}, 0, time.Time{})
	}
	if st.OutcomeHealth != nil {
		return outcome.StatusFor(cfg.Outcome, st.DonePRCount(), st.LastMergeAt, *st.OutcomeHealth)
	}
	return outcome.StatusFor(cfg.Outcome, st.DonePRCount(), st.LastMergeAt)
}

func (s *Server) handleWorkers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	st, err := s.loadState()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load state: %v", err))
		return
	}

	workers := sessionInfosWithActions(s.cfg.Repo, st, s.cfg.Server.ReadOnly, "/api/v1/actions")

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"workers": workers,
		"total":   len(workers),
	})
}

// issueResponse is the JSON shape for GET /api/v1/<issue_number>.
type issueResponse struct {
	IssueNumber int           `json:"issue_number"`
	Sessions    []sessionInfo `json:"sessions"`
}

func (s *Server) handleIssue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Parse issue number from path: /api/v1/<number>
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		writeError(w, http.StatusBadRequest, "issue number required")
		return
	}

	issueNum, err := strconv.Atoi(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid issue number: %s", path))
		return
	}

	st, err := s.loadState()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load state: %v", err))
		return
	}

	resp := issueResponse{
		IssueNumber: issueNum,
		Sessions:    make([]sessionInfo, 0),
	}

	for _, info := range sessionInfosWithActions(s.cfg.Repo, st, s.cfg.Server.ReadOnly, "/api/v1/actions") {
		if info.IssueNumber == issueNum {
			resp.Sessions = append(resp.Sessions, info)
		}
	}

	if len(resp.Sessions) == 0 {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no sessions found for issue #%d", issueNum))
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

type logResponse struct {
	Slot      string `json:"slot"`
	Lines     int    `json:"lines"`
	Truncated bool   `json:"truncated"`
	Text      string `json:"text"`
	UpdatedAt string `json:"updated_at"`
}

var ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	slot := strings.TrimPrefix(r.URL.Path, "/api/v1/logs/")
	slot = strings.Trim(slot, "/")
	if slot == "" || strings.Contains(slot, "/") {
		writeError(w, http.StatusBadRequest, "slot required")
		return
	}

	st, err := s.loadState()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("load state: %v", err))
		return
	}
	sess, ok := st.Sessions[slot]
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", slot))
		return
	}
	if strings.TrimSpace(sess.LogFile) == "" {
		writeJSON(w, http.StatusOK, logResponse{
			Slot:      slot,
			Lines:     0,
			Text:      "",
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		})
		return
	}

	lines := parsePositiveInt(r.URL.Query().Get("lines"), 240)
	if lines > 1000 {
		lines = 1000
	}
	text, truncated, err := tailFile(sess.LogFile, lines, 512*1024)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("read log: %v", err))
		return
	}
	text = stripANSI(text)
	writeJSON(w, http.StatusOK, logResponse{
		Slot:      slot,
		Lines:     lines,
		Truncated: truncated,
		Text:      text,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	})
}

func stripANSI(text string) string {
	return ansiEscapeRE.ReplaceAllString(text, "")
}

func parsePositiveInt(raw string, fallback int) int {
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func tailFile(path string, maxLines int, maxBytes int64) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	start := int64(0)
	truncated := false
	if info.Size() > maxBytes {
		start = info.Size() - maxBytes
		truncated = true
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", false, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", false, err
	}
	text := string(data)
	if truncated {
		if idx := strings.IndexByte(text, '\n'); idx >= 0 {
			text = text[idx+1:]
		}
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
		truncated = true
	}
	return strings.Join(lines, "\n"), truncated, nil
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// #487: auth BEFORE the read-only check. The acceptance criterion is
	// "every mutating POST without a valid credential returns 401 (not 200,
	// not 403, not 405)".
	if _, ok := requireAuth(w, r, s.auth); !ok {
		return
	}
	if s.cfg.Server.ReadOnly {
		writeError(w, http.StatusForbidden, "server is read-only")
		return
	}

	select {
	case s.refreshCh <- struct{}{}:
		writeJSON(w, http.StatusOK, map[string]string{"status": "refresh triggered"})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "refresh already pending"})
	}
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	stateDir := ""
	if s.cfg != nil {
		stateDir = s.cfg.StateDir
	}
	handleControlAction(w, r, s.cfg.Server.ReadOnly, "server", s.cfg, stateDir, s.gh, s.audit, s.auth)
}

func handleControlAction(w http.ResponseWriter, r *http.Request, readOnly bool, scope string, cfg *config.Config, stateDir string, gh actionGitHubClient, audit actionAuditRecorder, auth authChecker) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// #487: auth check fires BEFORE the read-only gate so an unauthenticated
	// attacker on a read-only deployment cannot probe verb existence via
	// 403 vs 401 mapping (acceptance: "always 401, never 200/403/405").
	authenticatedActor, ok := requireAuth(w, r, auth)
	if !ok {
		return
	}
	if readOnly {
		writeError(w, http.StatusForbidden, scope+" is read-only; write actions require approval-backed controls to be enabled in configuration")
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

	if res := dispatchSafeAction(req, cfg, gh, audit, authenticatedActor); res.handled {
		if res.err != nil {
			log.Printf("[server] safe action %q failed: %v", req.ActionID, res.err)
		}
		writeJSON(w, res.status, res.body)
		return
	}

	if res := dispatchApprovalAction(req, cfg, stateDir, audit, authenticatedActor); res.handled {
		if res.err != nil {
			log.Printf("[server] approval enqueue %q failed: %v", req.ActionID, res.err)
		}
		writeJSON(w, res.status, res.body)
		return
	}

	writeError(w, http.StatusNotImplemented, "approval-backed action endpoints are not implemented yet")
}

var dashboardHTML = web.MustReadTemplate("dashboard.html")

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	repo, err := json.Marshal(s.cfg.Repo)
	if err != nil {
		http.Error(w, fmt.Sprintf("marshal repo: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := strings.ReplaceAll(dashboardHTML, "__REPO_JSON__", string(repo))
	fmt.Fprint(w, page)
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
