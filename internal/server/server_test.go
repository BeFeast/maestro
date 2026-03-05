package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

func setupTestServer(t *testing.T) (*Server, *config.Config) {
	t.Helper()
	dir := t.TempDir()

	cfg := &config.Config{
		Repo:        "test/repo",
		MaxParallel: 3,
		StateDir:    dir,
		Server:      config.ServerConfig{Port: 8765},
	}

	// Write test state
	st := state.NewState()
	now := time.Now().UTC()
	st.Sessions["slot-1"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "Fix bug",
		Status:      state.StatusRunning,
		Backend:     "claude",
		Branch:      "feat/slot-1-42-fix-bug",
		Worktree:    "/tmp/worktrees/slot-1",
		StartedAt:   now.Add(-10 * time.Minute),
		TokensUsed:  5000,
		PID:         1, // non-zero but won't be alive in tests
	}
	finished := now.Add(-5 * time.Minute)
	st.Sessions["slot-2"] = &state.Session{
		IssueNumber: 43,
		IssueTitle:  "Add feature",
		Status:      state.StatusPROpen,
		Backend:     "codex",
		Branch:      "feat/slot-2-43-add-feature",
		Worktree:    "/tmp/worktrees/slot-2",
		StartedAt:   now.Add(-30 * time.Minute),
		FinishedAt:  &finished,
		PRNumber:    10,
		TokensUsed:  8000,
	}
	st.Sessions["slot-3"] = &state.Session{
		IssueNumber: 44,
		IssueTitle:  "Refactor code",
		Status:      state.StatusDone,
		Backend:     "claude",
		Branch:      "feat/slot-3-44-refactor-code",
		StartedAt:   now.Add(-1 * time.Hour),
		FinishedAt:  &finished,
		PRNumber:    11,
		TokensUsed:  3000,
	}

	if err := state.Save(dir, st); err != nil {
		t.Fatalf("save test state: %v", err)
	}

	refreshCh := make(chan struct{}, 1)
	srv := New(cfg, refreshCh)
	return srv, cfg
}

func TestHandleState(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	srv.handleState(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp stateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if resp.Repo != "test/repo" {
		t.Errorf("repo = %q, want %q", resp.Repo, "test/repo")
	}
	if resp.MaxParallel != 3 {
		t.Errorf("max_parallel = %d, want 3", resp.MaxParallel)
	}
	if len(resp.Running) != 1 {
		t.Errorf("running sessions = %d, want 1", len(resp.Running))
	}
	if len(resp.PROpen) != 1 {
		t.Errorf("pr_open sessions = %d, want 1", len(resp.PROpen))
	}
	if resp.TokenTotals.Total != 16000 {
		t.Errorf("total tokens = %d, want 16000", resp.TokenTotals.Total)
	}
	if resp.TokenTotals.Active != 13000 {
		t.Errorf("active tokens = %d, want 13000", resp.TokenTotals.Active)
	}
}

func TestHandleState_MethodNotAllowed(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	srv.handleState(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleWorkers(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	w := httptest.NewRecorder()
	srv.handleWorkers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	total := int(resp["total"].(float64))
	if total != 3 {
		t.Errorf("total workers = %d, want 3", total)
	}

	workers := resp["workers"].([]interface{})
	if len(workers) != 3 {
		t.Errorf("workers array len = %d, want 3", len(workers))
	}
}

func TestHandleIssue_Found(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/42", nil)
	w := httptest.NewRecorder()
	srv.handleIssue(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp issueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if resp.IssueNumber != 42 {
		t.Errorf("issue_number = %d, want 42", resp.IssueNumber)
	}
	if len(resp.Sessions) != 1 {
		t.Errorf("sessions = %d, want 1", len(resp.Sessions))
	}
	if resp.Sessions[0].IssueTitle != "Fix bug" {
		t.Errorf("issue_title = %q, want %q", resp.Sessions[0].IssueTitle, "Fix bug")
	}
}

func TestHandleIssue_NotFound(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/999", nil)
	w := httptest.NewRecorder()
	srv.handleIssue(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleIssue_InvalidNumber(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/abc", nil)
	w := httptest.NewRecorder()
	srv.handleIssue(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleRefresh(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:     "test/repo",
		StateDir: dir,
		Server:   config.ServerConfig{Port: 8765},
	}

	refreshCh := make(chan struct{}, 1)
	srv := New(cfg, refreshCh)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/refresh", nil)
	w := httptest.NewRecorder()
	srv.handleRefresh(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Check channel received signal
	select {
	case <-refreshCh:
		// ok
	default:
		t.Error("refresh channel did not receive signal")
	}
}

func TestHandleRefresh_AlreadyPending(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:     "test/repo",
		StateDir: dir,
		Server:   config.ServerConfig{Port: 8765},
	}

	refreshCh := make(chan struct{}, 1)
	refreshCh <- struct{}{} // pre-fill the channel
	srv := New(cfg, refreshCh)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/refresh", nil)
	w := httptest.NewRecorder()
	srv.handleRefresh(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "refresh already pending" {
		t.Errorf("status = %q, want %q", resp["status"], "refresh already pending")
	}
}

func TestHandleRefresh_MethodNotAllowed(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:     "test/repo",
		StateDir: dir,
		Server:   config.ServerConfig{Port: 8765},
	}

	refreshCh := make(chan struct{}, 1)
	srv := New(cfg, refreshCh)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/refresh", nil)
	w := httptest.NewRecorder()
	srv.handleRefresh(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleDashboard(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.handleDashboard(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	if !contains(body, "test/repo") {
		t.Error("dashboard should contain repo name")
	}
	if !contains(body, "Fix bug") {
		t.Error("dashboard should contain issue titles")
	}
}

func TestHandleDashboard_NotFoundForOtherPaths(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	w := httptest.NewRecorder()
	srv.handleDashboard(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleState_EmptyState(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Repo:        "test/repo",
		MaxParallel: 5,
		StateDir:    dir,
		Server:      config.ServerConfig{Port: 8765},
	}

	refreshCh := make(chan struct{}, 1)
	srv := New(cfg, refreshCh)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	srv.handleState(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp stateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(resp.Running) != 0 {
		t.Errorf("running = %d, want 0", len(resp.Running))
	}
	if resp.TokenTotals.Total != 0 {
		t.Errorf("total tokens = %d, want 0", resp.TokenTotals.Total)
	}
}

func TestStartDisabledPort(t *testing.T) {
	cfg := &config.Config{
		Repo:   "test/repo",
		Server: config.ServerConfig{Port: 0},
	}
	refreshCh := make(chan struct{}, 1)
	srv := New(cfg, refreshCh)

	// Start should return nil immediately when port is 0
	err := srv.Start(nil)
	if err != nil {
		t.Errorf("expected nil error for disabled port, got %v", err)
	}
}

func TestEscapeHTML(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{"<script>", "&lt;script&gt;"},
		{"a & b", "a &amp; b"},
		{`"quoted"`, "&quot;quoted&quot;"},
	}
	for _, tt := range tests {
		got := escapeHTML(tt.in)
		if got != tt.want {
			t.Errorf("escapeHTML(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestHandleState_InvalidStateDir(t *testing.T) {
	cfg := &config.Config{
		Repo:     "test/repo",
		StateDir: filepath.Join(os.TempDir(), "nonexistent-dir-12345", "nested"),
	}

	// Write a corrupt state file
	os.MkdirAll(cfg.StateDir, 0755)
	os.WriteFile(filepath.Join(cfg.StateDir, "state.json"), []byte("{invalid"), 0644)

	refreshCh := make(chan struct{}, 1)
	srv := New(cfg, refreshCh)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	w := httptest.NewRecorder()
	srv.handleState(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}

	// cleanup
	os.RemoveAll(cfg.StateDir)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
