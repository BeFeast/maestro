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
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/worker"
)

// Server provides an HTTP API for runtime observability.
type Server struct {
	cfg       *config.Config
	refreshCh chan<- struct{}
	srv       *http.Server
}

// New creates a new Server. refreshCh is used to trigger immediate poll cycles.
func New(cfg *config.Config, refreshCh chan<- struct{}) *Server {
	return &Server{
		cfg:       cfg,
		refreshCh: refreshCh,
	}
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
	mux.HandleFunc("/api/v1/", s.handleIssue)
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
	Repo                string                       `json:"repo"`
	MaxParallel         int                          `json:"max_parallel"`
	ReadOnly            bool                         `json:"read_only"`
	SupervisorPolicy    config.SupervisorConfig      `json:"supervisor_policy"`
	All                 []sessionInfo                `json:"all"`
	Running             []sessionInfo                `json:"running"`
	PROpen              []sessionInfo                `json:"pr_open"`
	Queued              []sessionInfo                `json:"queued"`
	TokenTotals         tokenTotalsInfo              `json:"token_totals"`
	Summary             map[string]int               `json:"summary"`
	StuckStates         []state.SupervisorStuckState `json:"stuck_states,omitempty"`
	Supervisor          supervisorInfo               `json:"supervisor"`
	SupervisorLatest    *state.SupervisorDecision    `json:"supervisor_latest,omitempty"`
	SupervisorDecisions []state.SupervisorDecision   `json:"supervisor_decisions,omitempty"`
	Approvals           []state.Approval             `json:"approvals,omitempty"`
}

type supervisorInfo struct {
	HasRun          bool                    `json:"has_run"`
	EmptyState      string                  `json:"empty_state,omitempty"`
	Latest          *supervisorDecisionInfo `json:"latest,omitempty"`
	LastSafeAction  *supervisorActionInfo   `json:"last_safe_action,omitempty"`
	ApprovalActions []supervisorActionInfo  `json:"approval_actions,omitempty"`
}

type supervisorDecisionInfo struct {
	ID                string                       `json:"id"`
	CreatedAt         time.Time                    `json:"created_at"`
	Project           string                       `json:"project"`
	Mode              string                       `json:"mode"`
	PolicyRule        string                       `json:"policy_rule,omitempty"`
	Status            string                       `json:"status,omitempty"`
	Summary           string                       `json:"summary"`
	RecommendedAction string                       `json:"recommended_action"`
	Target            *state.SupervisorTarget      `json:"target,omitempty"`
	TargetLinks       []targetLinkInfo             `json:"target_links,omitempty"`
	Risk              string                       `json:"risk"`
	Confidence        float64                      `json:"confidence"`
	ErrorClass        string                       `json:"error_class,omitempty"`
	Reasons           []string                     `json:"reasons,omitempty"`
	Mutations         []state.SupervisorMutation   `json:"mutations,omitempty"`
	StuckStates       []state.SupervisorStuckState `json:"stuck_states,omitempty"`
	StuckReasons      []string                     `json:"stuck_reasons,omitempty"`
	ProjectState      state.SupervisorProjectState `json:"project_state"`
	Queue             *supervisorQueueInfo         `json:"queue,omitempty"`
	ApprovalID        string                       `json:"approval_id,omitempty"`
}

type supervisorActionInfo struct {
	Action         string                  `json:"action"`
	Summary        string                  `json:"summary"`
	Risk           string                  `json:"risk"`
	CreatedAt      time.Time               `json:"created_at"`
	Target         *state.SupervisorTarget `json:"target,omitempty"`
	TargetLinks    []targetLinkInfo        `json:"target_links,omitempty"`
	Disabled       bool                    `json:"disabled"`
	DisabledReason string                  `json:"disabled_reason,omitempty"`
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

type sessionInfo struct {
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
	FinishedAt        string `json:"finished_at,omitempty"`
	NextRetryAt       string `json:"next_retry_at,omitempty"`
	PID               int    `json:"pid,omitempty"`
	Alive             *bool  `json:"alive,omitempty"`
	Worktree          string `json:"worktree,omitempty"`
	Branch            string `json:"branch,omitempty"`
	TmuxSession       string `json:"tmux_session,omitempty"`
	HasLog            bool   `json:"has_log"`
	RetryCount        int    `json:"retry_count,omitempty"`
	LastNotification  string `json:"last_notification,omitempty"`
}

func makeSessionInfo(repo, slot string, sess *state.Session) sessionInfo {
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
	}

	// Calculate runtime
	end := time.Now()
	if sess.FinishedAt != nil {
		end = *sess.FinishedAt
		info.FinishedAt = sess.FinishedAt.Format(time.RFC3339)
	}
	info.Runtime = end.Sub(sess.StartedAt).Round(time.Second).String()

	if sess.Status == state.StatusRunning {
		info.PID = sess.PID
		alive := worker.IsAlive(sess.PID)
		info.Alive = &alive
	}
	if sess.NextRetryAt != nil {
		info.NextRetryAt = sess.NextRetryAt.Format(time.RFC3339)
	}
	info.StatusReason, info.NeedsAttention = sessionStatusReason(sess, info.Alive)

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
		Target:            decision.Target,
		TargetLinks:       supervisorTargetLinks(cfg.Repo, decision.Target),
		Risk:              decision.Risk,
		Confidence:        decision.Confidence,
		ErrorClass:        decision.ErrorClass,
		Reasons:           decision.Reasons,
		Mutations:         decision.Mutations,
		StuckStates:       decision.StuckStates,
		StuckReasons:      supervisorStuckReasons(decision),
		ProjectState:      decision.ProjectState,
		Queue:             supervisorQueueInfoForDecision(cfg, st, decision),
		ApprovalID:        decision.ApprovalID,
	}
}

func makeSupervisorActionInfo(cfg *config.Config, decision state.SupervisorDecision, disabled bool, disabledReason string) supervisorActionInfo {
	return supervisorActionInfo{
		Action:         decision.RecommendedAction,
		Summary:        decision.Summary,
		Risk:           decision.Risk,
		CreatedAt:      decision.CreatedAt,
		Target:         decision.Target,
		TargetLinks:    supervisorTargetLinks(cfg.Repo, decision.Target),
		Disabled:       disabled,
		DisabledReason: disabledReason,
	}
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

func sessionStatusReason(sess *state.Session, alive *bool) (string, bool) {
	switch sess.Status {
	case state.StatusRunning:
		if alive != nil && !*alive {
			return "State says running, but the worker PID is not alive. Maestro should reconcile it on the next cycle.", true
		}
		if sess.PID == 0 {
			return "Worker is marked running, but no PID is recorded.", true
		}
		return "Worker process is alive and writing to its session log.", false
	case state.StatusPROpen:
		if sess.PRNumber > 0 {
			return "PR is open; Maestro is waiting for CI, review gate, merge interval, or conflict handling.", false
		}
		return "Session is waiting on an open PR, but no PR number is recorded yet.", true
	case state.StatusQueued:
		return "Worker is queued for follow-up processing before it can be merged.", false
	case state.StatusDead:
		if sess.NextRetryAt != nil {
			return "Worker exited; a retry is scheduled after the current backoff.", true
		}
		return "Worker exited and is waiting for retry or reconciliation.", true
	case state.StatusRetryExhausted:
		if sess.PRNumber > 0 {
			return "Retry limit exhausted with a PR still open; Maestro can still merge it when checks and review gates pass, but action is needed if checks fail or actionable review feedback remains.", true
		}
		return "Retry limit exhausted before a usable PR was produced.", true
	case state.StatusFailed:
		return "Worker failed after the configured retry policy.", true
	case state.StatusConflictFailed:
		return "Automatic conflict resolution failed; the branch needs manual rebase/conflict handling.", true
	case state.StatusDone:
		return "Issue is complete; PR merged or issue was closed and the session is terminal.", false
	default:
		return "Session is waiting for the next Maestro reconciliation cycle.", false
	}
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

	latestDecision := st.LatestSupervisorDecision()
	resp := stateResponse{
		Repo:                s.cfg.Repo,
		MaxParallel:         s.cfg.MaxParallel,
		ReadOnly:            s.cfg.Server.ReadOnly,
		SupervisorPolicy:    s.cfg.Supervisor,
		All:                 make([]sessionInfo, 0, len(st.Sessions)),
		Running:             make([]sessionInfo, 0),
		PROpen:              make([]sessionInfo, 0),
		Queued:              make([]sessionInfo, 0),
		Summary:             make(map[string]int),
		Supervisor:          buildSupervisorInfo(s.cfg, st),
		SupervisorLatest:    latestDecision,
		SupervisorDecisions: st.SupervisorDecisions,
		Approvals:           st.Approvals,
	}
	if latestDecision != nil {
		resp.StuckStates = latestDecision.StuckStates
	}

	var activeTokens, totalTokens int
	for _, info := range allSessionInfos(s.cfg.Repo, st) {
		resp.All = append(resp.All, info)
		resp.Summary[info.Status]++
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

	writeJSON(w, http.StatusOK, resp)
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

	workers := allSessionInfos(s.cfg.Repo, st)

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

	for slot, sess := range st.Sessions {
		if sess.IssueNumber == issueNum {
			resp.Sessions = append(resp.Sessions, makeSessionInfo(s.cfg.Repo, slot, sess))
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

const dashboardHTML = `<!DOCTYPE html>
<html>
<head>
<title>maestro</title>
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
    --queued: #a371f7;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    min-width: 320px;
    background: var(--bg);
    color: var(--text);
    font: 14px/1.45 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
  }
  header {
    height: 56px;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 20px;
    padding: 0 18px;
    border-bottom: 1px solid var(--line);
    background: #0b1016;
  }
  h1 {
    margin: 0;
    font-size: 18px;
    font-weight: 650;
    letter-spacing: 0;
  }
  .repo { color: var(--muted); font-size: 13px; }
  .topline, .stats, .workspace, .split, .log-head, .status-row {
    display: flex;
    align-items: center;
    gap: 10px;
  }
  .topline { min-width: 0; }
  .stats { flex-wrap: wrap; justify-content: flex-end; }
  .stat {
    min-width: 72px;
    padding: 2px 4px;
    text-align: right;
  }
  .stat strong { display: block; font-size: 15px; }
  .stat span { display: block; color: var(--muted); font-size: 11px; }
  main {
    height: calc(100vh - 56px);
    display: grid;
    grid-template-columns: minmax(520px, 1fr) minmax(420px, 0.72fr);
  }
  section {
    min-width: 0;
    min-height: 0;
    border-right: 1px solid var(--line);
  }
  section:last-child { border-right: 0; }
  .toolbar {
    height: 48px;
    padding: 0 14px;
    border-bottom: 1px solid var(--line);
    background: var(--panel);
    justify-content: space-between;
  }
  .toolbar h2 {
    margin: 0;
    font-size: 13px;
    font-weight: 650;
    color: var(--muted);
    text-transform: uppercase;
  }
  .filter input {
    width: min(260px, 36vw);
    height: 30px;
    border: 1px solid var(--line);
    border-radius: 6px;
    background: #0b1016;
    color: var(--text);
    padding: 0 10px;
    outline: none;
  }
  .filter input:focus { border-color: var(--accent); }
  .table-wrap {
    height: calc(100% - 48px);
    overflow: auto;
  }
  table {
    width: 100%;
    border-collapse: collapse;
    table-layout: fixed;
  }
  th, td {
    padding: 9px 10px;
    border-bottom: 1px solid var(--line);
    vertical-align: middle;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  th {
    position: sticky;
    top: 0;
    z-index: 1;
    background: var(--panel);
    color: var(--muted);
    font-size: 12px;
    font-weight: 600;
    text-align: left;
  }
  tbody tr { cursor: pointer; }
  tbody tr:hover, tbody tr.selected { background: #18212c; }
  tbody tr.row-attention { background: rgba(248,81,73,.07); }
  tbody tr.row-attention:hover, tbody tr.row-attention.selected { background: rgba(248,81,73,.13); }
  a {
    color: var(--accent);
    text-decoration: none;
  }
  a:hover { text-decoration: underline; }
  .slot { width: 92px; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
  .issue { width: auto; }
  .status { width: 128px; }
  .backend { width: 102px; }
  .pr { width: 64px; }
  .runtime { width: 88px; }
  .tokens { width: 78px; text-align: right; }
  .pill {
    display: inline-flex;
    align-items: center;
    max-width: 100%;
    height: 22px;
    padding: 0 8px;
    border-radius: 999px;
    border: 1px solid var(--line);
    font-size: 12px;
    font-weight: 650;
  }
  .s-running { color: var(--ok); border-color: rgba(63,185,80,.42); }
  .s-pr_open { color: var(--warn); border-color: rgba(210,153,34,.48); }
  .s-queued { color: var(--queued); border-color: rgba(163,113,247,.5); }
  .s-dead, .s-failed, .s-conflict_failed, .s-retry_exhausted { color: var(--bad); border-color: rgba(248,81,73,.5); }
  .s-done { color: var(--muted); }
  .pill-attention { color: var(--bad); border-color: rgba(248,81,73,.58); }
  .log-panel {
    min-width: 0;
    display: grid;
    grid-template-rows: 48px auto auto minmax(0, 1fr);
    background: #080c11;
  }
  .log-head {
    padding: 0 14px;
    border-bottom: 1px solid var(--line);
    background: var(--panel);
    justify-content: space-between;
    min-width: 0;
  }
  .log-title { min-width: 0; font-weight: 650; }
  .log-title span {
    color: var(--muted);
    font-weight: 500;
    margin-left: 8px;
  }
  .log-meta {
    color: var(--muted);
    font-size: 12px;
    white-space: nowrap;
  }
  .status-note {
    display: none;
    padding: 10px 14px;
    border-bottom: 1px solid var(--line);
    background: #0b1016;
    color: var(--muted);
    font-size: 12px;
  }
  .status-note.visible { display: block; }
  .status-note strong {
    color: var(--text);
    margin-right: 6px;
  }
  .status-note .links {
    display: inline-flex;
    gap: 10px;
    margin-left: 10px;
  }
  .supervisor-panel {
    border-bottom: 1px solid var(--line);
    background: #0b1016;
    padding: 12px 14px;
    font-size: 12px;
    color: var(--muted);
  }
  .supervisor-head, .supervisor-main, .supervisor-meta, .supervisor-links, .supervisor-actions {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }
  .supervisor-head { justify-content: space-between; margin-bottom: 6px; }
  .supervisor-title { color: var(--text); font-weight: 650; }
  .supervisor-time { white-space: nowrap; }
  .supervisor-action {
    color: var(--text);
    font-weight: 650;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .supervisor-summary { margin-top: 4px; color: var(--text); }
  .supervisor-meta, .supervisor-links, .supervisor-actions { flex-wrap: wrap; margin-top: 6px; }
  .supervisor-reasons {
    margin: 6px 0 0;
    padding-left: 16px;
  }
  .supervisor-reasons li { margin: 2px 0; }
  .supervisor-approval {
    height: 24px;
    border: 1px solid rgba(210,153,34,.45);
    border-radius: 6px;
    background: rgba(210,153,34,.08);
    color: var(--warn);
    cursor: not-allowed;
  }
  .supervisor-empty { color: var(--muted); }
  pre {
    margin: 0;
    min-height: 0;
    overflow: auto;
    padding: 14px;
    color: #d1d7e0;
    font: 12px/1.5 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    white-space: pre-wrap;
    word-break: break-word;
  }
  .empty {
    padding: 24px;
    color: var(--muted);
  }
  .muted { color: var(--muted); }
  .error { color: var(--bad); }
  @media (max-width: 980px) {
    header { height: auto; align-items: flex-start; flex-direction: column; padding: 12px 14px; }
    .stats { justify-content: flex-start; }
    main { height: auto; min-height: calc(100vh - 112px); grid-template-columns: 1fr; }
    section { border-right: 0; border-bottom: 1px solid var(--line); }
    .table-wrap { height: 52vh; }
    .log-panel { height: 48vh; }
    .backend, .tokens { display: none; }
  }
</style>
</head>
<body>
<header>
  <div class="topline">
    <h1>Maestro</h1>
    <div class="repo" id="repo"></div>
  </div>
  <div class="stats" id="stats"></div>
</header>
<main>
  <section>
    <div class="toolbar workspace">
      <h2>Workers</h2>
      <label class="filter"><input id="filter" type="search" placeholder="Filter"></label>
    </div>
    <div class="table-wrap">
      <table>
        <thead>
          <tr>
            <th class="slot">Slot</th>
            <th class="issue">Issue</th>
            <th class="status">Status</th>
            <th class="backend">Backend</th>
            <th class="pr">PR</th>
            <th class="runtime">Runtime</th>
            <th class="tokens">Tokens</th>
          </tr>
        </thead>
        <tbody id="workers"></tbody>
      </table>
    </div>
  </section>
  <section class="log-panel">
    <div class="log-head">
      <div class="log-title" id="log-title">Log <span></span></div>
      <div class="log-meta" id="log-meta"></div>
    </div>
    <div class="supervisor-panel" id="supervisor-panel"></div>
    <div class="status-note" id="status-note"></div>
    <pre id="log"><span class="muted">Select a worker.</span></pre>
  </section>
</main>
<script>
window.MAESTRO_REPO = __REPO_JSON__;

const state = {
  workers: [],
  supervisor: null,
  selected: "",
  filter: "",
  lastLog: null
};

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

const repoEl = document.getElementById("repo");
const statsEl = document.getElementById("stats");
const workersEl = document.getElementById("workers");
const filterEl = document.getElementById("filter");
const logEl = document.getElementById("log");
const logTitleEl = document.getElementById("log-title");
const logMetaEl = document.getElementById("log-meta");
const statusNoteEl = document.getElementById("status-note");
const supervisorPanelEl = document.getElementById("supervisor-panel");

repoEl.textContent = window.MAESTRO_REPO || "";

filterEl.addEventListener("input", () => {
  state.filter = filterEl.value.toLowerCase();
  renderWorkers();
});

function escapeText(value) {
  return String(value ?? "").replace(/[&<>"']/g, ch => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    "\"": "&quot;",
    "'": "&#39;"
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

function actionLabel(action) {
  return String(action || "-").replace(/_/g, " ");
}

function formatTimestamp(value) {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return String(value);
  const seconds = Math.max(0, Math.round((Date.now() - date.getTime()) / 1000));
  let relative = seconds + "s ago";
  if (seconds >= 86400) relative = Math.floor(seconds / 86400) + "d ago";
  else if (seconds >= 3600) relative = Math.floor(seconds / 3600) + "h ago";
  else if (seconds >= 60) relative = Math.floor(seconds / 60) + "m ago";
  return date.toLocaleString() + " (" + relative + ")";
}

function targetLinksHTML(links) {
  return (links || []).map(link => linkHTML(link.url, link.label)).join("");
}

function queueText(queue) {
  if (!queue || !queue.enabled) return "";
  const label = queue.label || "Queue";
  if (queue.position && queue.total) return label + ": " + queue.position + " of " + queue.total;
  if (queue.total) return label + ": " + queue.total + " item" + (queue.total === 1 ? "" : "s");
  return label + ": empty";
}

function statusLabel(worker) {
  if (worker.status === "running" && worker.alive === false) return "running stale";
  return worker.status || "-";
}

function pillClass(worker) {
  const base = "pill s-" + escapeText(worker.status || "unknown");
  if (worker.needs_attention || (worker.status === "running" && worker.alive === false)) {
    return base + " pill-attention";
  }
  return base;
}

function workerMatches(worker) {
  if (!state.filter) return true;
  const text = [worker.slot, worker.issue_number, worker.issue_title, worker.status, worker.backend, worker.pr_number]
    .join(" ")
    .toLowerCase();
  return text.includes(state.filter);
}

function sortWorkers(workers) {
  return [...workers].sort((a, b) => {
    const ar = statusRank[a.status] ?? 99;
    const br = statusRank[b.status] ?? 99;
    if (ar !== br) return ar - br;
    return String(b.started_at || "").localeCompare(String(a.started_at || ""));
  });
}

function renderStats(summary, total, maxParallel, readOnly) {
  const running = summary.running || 0;
  const prOpen = summary.pr_open || 0;
  const failed = (summary.dead || 0) + (summary.failed || 0) + (summary.retry_exhausted || 0) + (summary.conflict_failed || 0);
  const items = [
    ["Running", running + " / " + maxParallel],
    ["PR open", prOpen],
    ["Failed", failed],
    ["Sessions", total],
    ["Mode", readOnly ? "Read-only" : "Control"]
  ];
  statsEl.innerHTML = items.map(([label, value]) =>
    '<div class="stat"><strong>' + escapeText(value) + '</strong><span>' + escapeText(label) + '</span></div>'
  ).join("");
}

function renderSupervisor(info) {
  if (!info || !info.has_run || !info.latest) {
    const empty = info && info.empty_state ? info.empty_state : "No Supervisor has run yet.";
    supervisorPanelEl.innerHTML = '<div class="supervisor-head">' +
      '<span class="supervisor-title">Supervisor</span>' +
      '<span class="supervisor-time">empty</span>' +
      '</div>' +
      '<div class="supervisor-empty">' + escapeText(empty) + '</div>';
    return;
  }

  const latest = info.latest;
  const links = targetLinksHTML(latest.target_links);
  const queue = queueText(latest.queue);
  const meta = [
    latest.risk ? "Risk " + latest.risk : "",
    latest.confidence ? "Confidence " + Number(latest.confidence).toFixed(2) : "",
    queue
  ].filter(Boolean);
  const reasons = (latest.stuck_reasons && latest.stuck_reasons.length ? latest.stuck_reasons : latest.reasons || []).slice(0, 3);
  const reasonHTML = reasons.length ? '<ul class="supervisor-reasons">' + reasons.map(reason =>
    '<li>' + escapeText(reason) + '</li>'
  ).join("") + '</ul>' : "";
  const lastSafe = info.last_safe_action ? '<div class="supervisor-meta">' +
    '<span>Last safe action: ' + escapeText(actionLabel(info.last_safe_action.action)) + '</span>' +
    '<span>' + escapeText(formatTimestamp(info.last_safe_action.created_at)) + '</span>' +
    '</div>' : "";
  const approvals = (info.approval_actions || []).length ? '<div class="supervisor-actions">' +
    '<span>Requires approval:</span>' +
    (info.approval_actions || []).map(action =>
      '<button class="supervisor-approval" disabled title="' + escapeText(action.disabled_reason || "Controls not available yet") + '">' +
      escapeText(actionLabel(action.action)) +
      '</button>'
    ).join("") +
    '</div>' : "";

  supervisorPanelEl.innerHTML = '<div class="supervisor-head">' +
    '<span class="supervisor-title">Supervisor</span>' +
    '<span class="supervisor-time">' + escapeText(formatTimestamp(latest.created_at)) + '</span>' +
    '</div>' +
    '<div class="supervisor-main">' +
    '<span class="supervisor-action">' + escapeText(actionLabel(latest.recommended_action)) + '</span>' +
    (links ? '<span class="supervisor-links">' + links + '</span>' : "") +
    '</div>' +
    (latest.summary ? '<div class="supervisor-summary">' + escapeText(latest.summary) + '</div>' : "") +
    (meta.length ? '<div class="supervisor-meta">' + meta.map(item => '<span>' + escapeText(item) + '</span>').join("") + '</div>' : "") +
    reasonHTML + lastSafe + approvals;
}

function renderWorkers() {
  const visible = sortWorkers(state.workers).filter(workerMatches);
  if (visible.length === 0) {
    workersEl.innerHTML = '<tr><td colspan="7" class="empty">No workers.</td></tr>';
    return;
  }
  workersEl.innerHTML = visible.map(worker => {
    const selected = worker.slot === state.selected ? " selected" : "";
    const attention = worker.needs_attention ? " row-attention" : "";
    const issue = "#" + worker.issue_number;
    const pr = worker.pr_number ? "#" + worker.pr_number : "-";
    return '<tr class="' + selected + attention + '" data-slot="' + escapeText(worker.slot) + '">' +
      '<td class="slot">' + escapeText(worker.slot) + '</td>' +
      '<td class="issue">' + linkHTML(worker.issue_url, issue) + ' ' + escapeText(worker.issue_title) + '</td>' +
      '<td class="status"><span class="' + pillClass(worker) + '">' + escapeText(statusLabel(worker)) + '</span></td>' +
      '<td class="backend">' + escapeText(worker.backend || "-") + '</td>' +
      '<td class="pr">' + linkHTML(worker.pr_url, pr) + '</td>' +
      '<td class="runtime">' + escapeText(worker.runtime || "-") + '</td>' +
      '<td class="tokens">' + compactNumber(worker.tokens_used_total) + '</td>' +
    '</tr>';
  }).join("");
  workersEl.querySelectorAll("tr[data-slot]").forEach(row => {
    row.addEventListener("click", () => selectWorker(row.dataset.slot));
  });
}

function selectWorker(slot) {
  state.selected = slot;
  state.lastLog = null;
  renderWorkers();
  renderSelectedDetails();
  loadLog();
}

function emptyLogText(worker) {
  if (!worker) return "No log output yet.";
  if (worker.status === "running" && worker.backend === "claude") {
    return "No log output yet. Claude print mode may stay quiet until it finishes.";
  }
  if (worker.status === "running") return "No log output yet. Worker is still running.";
  return "No log output.";
}

function renderSelectedDetails() {
  const worker = state.workers.find(item => item.slot === state.selected);
  if (!worker) {
    statusNoteEl.classList.remove("visible");
    statusNoteEl.innerHTML = "";
    return;
  }

  const links = [];
  if (worker.issue_url) links.push(linkHTML(worker.issue_url, "Issue #" + worker.issue_number));
  if (worker.pr_url) links.push(linkHTML(worker.pr_url, "PR #" + worker.pr_number));
  const retry = worker.next_retry_at ? " Next retry: " + worker.next_retry_at + "." : "";
  statusNoteEl.innerHTML = '<strong>Why</strong>' +
    escapeText((worker.status_reason || "Waiting for next reconciliation cycle.") + retry) +
    (links.length ? '<span class="links">' + links.join("") + '</span>' : "");
  statusNoteEl.classList.add("visible");
}

async function loadState() {
  try {
    const response = await fetch("/api/v1/state", { cache: "no-store" });
    if (!response.ok) throw new Error(await response.text());
    const data = await response.json();
    state.workers = data.all || [];
    state.supervisor = data.supervisor || null;
    renderStats(data.summary || {}, state.workers.length, data.max_parallel || 0, data.read_only);
    renderSupervisor(state.supervisor);
    if (!state.selected && state.workers.length) state.selected = sortWorkers(state.workers)[0].slot;
    if (state.selected && !state.workers.some(worker => worker.slot === state.selected)) {
      state.selected = state.workers.length ? sortWorkers(state.workers)[0].slot : "";
      state.lastLog = null;
    }
    renderWorkers();
    renderSelectedDetails();
  } catch (err) {
    statsEl.innerHTML = '<span class="error">' + escapeText(err.message) + '</span>';
  }
}

async function loadLog() {
  if (!state.selected) {
    logTitleEl.innerHTML = 'Log <span></span>';
    logMetaEl.textContent = "";
    logEl.textContent = "Select a worker.";
    renderSelectedDetails();
    return;
  }
  const worker = state.workers.find(item => item.slot === state.selected);
  logTitleEl.innerHTML = 'Log <span>' + escapeText(state.selected) + (worker ? " #" + escapeText(worker.issue_number) : "") + '</span>';
  try {
    const response = await fetch("/api/v1/logs/" + encodeURIComponent(state.selected) + "?lines=260", { cache: "no-store" });
    if (!response.ok) throw new Error(await response.text());
    const data = await response.json();
    const text = data.text || "";
    logMetaEl.textContent = (data.truncated ? "tail " : "") + (data.updated_at || "");
    if (text !== state.lastLog) {
      state.lastLog = text;
      logEl.textContent = text || emptyLogText(worker);
      logEl.scrollTop = logEl.scrollHeight;
    }
  } catch (err) {
    logEl.textContent = "Log error: " + err.message;
  }
}

loadState().then(loadLog);
setInterval(loadState, 3000);
setInterval(loadLog, 2000);
</script>
</body>
</html>`

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
