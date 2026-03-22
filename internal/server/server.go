package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
	mux.HandleFunc("/api/v1/refresh", s.handleRefresh)
	mux.HandleFunc("/api/v1/", s.handleIssue)
	mux.HandleFunc("/", s.handleDashboard)

	addr := fmt.Sprintf(":%d", s.cfg.Server.Port)
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
	Repo        string          `json:"repo"`
	MaxParallel int             `json:"max_parallel"`
	Running     []sessionInfo   `json:"running"`
	PROpen      []sessionInfo   `json:"pr_open"`
	Queued      []sessionInfo   `json:"queued"`
	TokenTotals tokenTotalsInfo `json:"token_totals"`
	Summary     map[string]int  `json:"summary"`
}

type tokenTotalsInfo struct {
	Active int `json:"active"`
	Total  int `json:"total"`
}

type sessionInfo struct {
	Slot              string `json:"slot"`
	IssueNumber       int    `json:"issue_number"`
	IssueTitle        string `json:"issue_title"`
	Status            string `json:"status"`
	Backend           string `json:"backend,omitempty"`
	PRNumber          int    `json:"pr_number,omitempty"`
	TokensUsedAttempt int    `json:"tokens_used_attempt"`
	TokensUsedTotal   int    `json:"tokens_used_total"`
	Runtime           string `json:"runtime"`
	StartedAt         string `json:"started_at"`
	FinishedAt        string `json:"finished_at,omitempty"`
	PID               int    `json:"pid,omitempty"`
	Alive             *bool  `json:"alive,omitempty"`
	Worktree          string `json:"worktree,omitempty"`
	Branch            string `json:"branch,omitempty"`
	RetryCount        int    `json:"retry_count,omitempty"`
}

func makeSessionInfo(slot string, sess *state.Session) sessionInfo {
	info := sessionInfo{
		Slot:              slot,
		IssueNumber:       sess.IssueNumber,
		IssueTitle:        sess.IssueTitle,
		Status:            string(sess.Status),
		Backend:           sess.Backend,
		PRNumber:          sess.PRNumber,
		TokensUsedAttempt: sess.TokensUsedAttempt,
		TokensUsedTotal:   sess.TokensUsedTotal,
		StartedAt:         sess.StartedAt.Format(time.RFC3339),
		Worktree:          sess.Worktree,
		Branch:            sess.Branch,
		RetryCount:        sess.RetryCount,
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

	return info
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

	resp := stateResponse{
		Repo:        s.cfg.Repo,
		MaxParallel: s.cfg.MaxParallel,
		Running:     make([]sessionInfo, 0),
		PROpen:      make([]sessionInfo, 0),
		Queued:      make([]sessionInfo, 0),
		Summary:     make(map[string]int),
	}

	var activeTokens, totalTokens int
	for slot, sess := range st.Sessions {
		info := makeSessionInfo(slot, sess)
		resp.Summary[string(sess.Status)]++
		totalTokens += sess.TokensUsedTotal

		switch sess.Status {
		case state.StatusRunning:
			resp.Running = append(resp.Running, info)
			activeTokens += sess.TokensUsedTotal
		case state.StatusPROpen:
			resp.PROpen = append(resp.PROpen, info)
			activeTokens += sess.TokensUsedTotal
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

	workers := make([]sessionInfo, 0, len(st.Sessions))
	for slot, sess := range st.Sessions {
		workers = append(workers, makeSessionInfo(slot, sess))
	}

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
			resp.Sessions = append(resp.Sessions, makeSessionInfo(slot, sess))
		}
	}

	if len(resp.Sessions) == 0 {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no sessions found for issue #%d", issueNum))
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
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

	st, err := s.loadState()
	if err != nil {
		http.Error(w, fmt.Sprintf("load state: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
<title>maestro — %s</title>
<meta http-equiv="refresh" content="10">
<style>
  body { font-family: monospace; background: #1a1a2e; color: #e0e0e0; margin: 2em; }
  h1 { color: #00d4ff; }
  table { border-collapse: collapse; width: 100%%; margin-top: 1em; }
  th, td { padding: 6px 12px; text-align: left; border-bottom: 1px solid #333; }
  th { color: #00d4ff; }
  .running { color: #00ff88; }
  .pr_open { color: #ffaa00; }
  .done { color: #888; }
  .dead, .failed { color: #ff4444; }
  .queued { color: #aaaaff; }
  a { color: #00d4ff; }
</style>
</head>
<body>
<h1>maestro — %s</h1>
<p>Sessions: %d | Max parallel: %d</p>
<table>
<tr><th>Slot</th><th>Issue</th><th>Status</th><th>Backend</th><th>PR</th><th>Tokens</th><th>Runtime</th></tr>
`, s.cfg.Repo, s.cfg.Repo, len(st.Sessions), s.cfg.MaxParallel)

	for slot, sess := range st.Sessions {
		end := time.Now()
		if sess.FinishedAt != nil {
			end = *sess.FinishedAt
		}
		runtime := end.Sub(sess.StartedAt).Round(time.Second)
		pr := "-"
		if sess.PRNumber > 0 {
			pr = fmt.Sprintf("#%d", sess.PRNumber)
		}
		tokens := worker.FormatTokens(sess.TokensUsedTotal)
		fmt.Fprintf(w, `<tr class="%s"><td>%s</td><td>#%d %s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>
`,
			sess.Status, slot, sess.IssueNumber, escapeHTML(sess.IssueTitle),
			sess.Status, sess.Backend, pr, tokens, runtime)
	}

	fmt.Fprintf(w, `</table>
<p style="margin-top:2em; color:#666">Auto-refreshes every 10s | <a href="/api/v1/state">JSON API</a></p>
</body>
</html>`)
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
