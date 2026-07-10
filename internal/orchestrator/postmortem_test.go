package orchestrator

// #835: on respawn after a failed attempt, the new prompt carries a bounded
// post-mortem distilled from the previous attempt's own worker log, alongside
// (not replacing) the existing CI/review/conflict context. The full
// post-mortem is persisted to the state dir; a missing log degrades gracefully.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/state"
)

func TestAppendPriorAttemptPostmortem_AddsSection(t *testing.T) {
	base := "You are a coding agent."
	pm := "### Errors / failed commands observed\n- exit status 1"
	result := appendPriorAttemptPostmortem(base, pm, 2)

	for _, want := range []string{
		"You are a coding agent.",
		"Prior Attempt Post-Mortem (Attempt 2)",
		"exit status 1",
		"Do NOT repeat the same approach",
	} {
		if !strings.Contains(result, want) {
			t.Errorf("result missing %q:\n%s", want, result)
		}
	}
}

func postmortemRetryOrchestrator(t *testing.T, stateDir string, captured *string) *Orchestrator {
	t.Helper()
	cfg := &config.Config{
		Repo:               "owner/repo",
		MaxRetriesPerIssue: 3,
		MaxRetryBackoffMs:  300000,
		StateDir:           stateDir,
		Model: config.ModelConfig{
			Default:  "claude",
			Backends: map[string]config.BackendDef{"claude": {Cmd: "claude"}},
		},
	}
	return &Orchestrator{
		cfg:        cfg,
		notifier:   &notify.Notifier{},
		promptBase: "base prompt",
		getIssueFn: func(number int) (github.Issue, error) {
			return github.Issue{Number: 42, Title: "test issue", Body: "fix this"}, nil
		},
		respawnWorkerFn: func(cfg *config.Config, slotName string, sess *state.Session, repo string, issue github.Issue, promptBase string, backendName string) error {
			*captured = promptBase
			return nil
		},
	}
}

func TestRespawnDueRetries_PriorAttemptPostmortem_IncludedAndPersisted(t *testing.T) {
	stateDir := t.TempDir()
	logFile := filepath.Join(stateDir, "slot-1.log")
	logContent := strings.Join([]string{
		`{"type":"tool_use","name":"Edit","input":{"file_path":"internal/foo.go"}}`,
		"go test ./...",
		"--- FAIL: TestFoo (0.00s)",
		"exit status 1",
		"giving up",
	}, "\n")
	if err := os.WriteFile(logFile, []byte(logContent), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	captured := ""
	o := postmortemRetryOrchestrator(t, stateDir, &captured)

	s := state.NewState()
	retryAt := time.Now().UTC().Add(-1 * time.Minute)
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber:     42,
		IssueTitle:      "test issue",
		Status:          state.StatusDead,
		RetryCount:      1,
		NextRetryAt:     &retryAt,
		Backend:         "claude",
		LogFile:         logFile,
		CIFailureOutput: "tests failed: FAIL main_test.go:15",
	}

	o.respawnDueRetries(s, 10)

	if captured == "" {
		t.Fatal("respawnWorkerFn should have been called")
	}
	// Post-mortem section present…
	for _, want := range []string{
		"Prior Attempt Post-Mortem",
		"--- FAIL: TestFoo",
		"internal/foo.go",
	} {
		if !strings.Contains(captured, want) {
			t.Errorf("prompt missing post-mortem content %q", want)
		}
	}
	// …in addition to the existing CI context (not replacing it).
	if !strings.Contains(captured, "Previous CI Failure") {
		t.Error("prompt should still contain CI failure context alongside the post-mortem")
	}

	// Full post-mortem persisted to the state dir for operator inspection.
	persisted := filepath.Join(stateDir, "slot-1-attempt1-postmortem.md")
	data, err := os.ReadFile(persisted)
	if err != nil {
		t.Fatalf("expected persisted post-mortem at %s: %v", persisted, err)
	}
	if !strings.Contains(string(data), "--- FAIL: TestFoo") {
		t.Errorf("persisted post-mortem missing failure content:\n%s", data)
	}
}

// TestRespawnDueRetries_PostmortemRedactsInlinePEMTailInBothSinks guards the
// #835 review finding end-to-end: a tail-clipped private-key fragment embedded
// in a command line (`$ echo <fragment>`) must not survive into EITHER sink the
// respawn writes it to — the retry prompt or the persisted post-mortem file.
// The worker-level unit test proves ExtractPostmortem redacts the short padded
// tail; this asserts both orchestrator sinks carry the redacted form, since the
// finding named them both ("written to the saved post-mortem and copied into the
// retry prompt").
func TestRespawnDueRetries_PostmortemRedactsInlinePEMTailInBothSinks(t *testing.T) {
	const secretFragment = "Kj34GkxFhD90vcNLYLInFE=" // short '='-padded base64 tail
	stateDir := t.TempDir()
	logFile := filepath.Join(stateDir, "slot-1.log")
	logContent := strings.Join([]string{
		"running deploy",
		"$ echo " + secretFragment,
		"exit status 1",
	}, "\n")
	if err := os.WriteFile(logFile, []byte(logContent), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	captured := ""
	o := postmortemRetryOrchestrator(t, stateDir, &captured)

	s := state.NewState()
	retryAt := time.Now().UTC().Add(-1 * time.Minute)
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "test issue",
		Status:      state.StatusDead,
		RetryCount:  1,
		NextRetryAt: &retryAt,
		Backend:     "claude",
		LogFile:     logFile,
	}

	o.respawnDueRetries(s, 10)

	if captured == "" {
		t.Fatal("respawnWorkerFn should have been called")
	}
	// Sink 1: the retry prompt must carry the redacted form, not the raw tail.
	if strings.Contains(captured, secretFragment) {
		t.Errorf("inline PEM tail leaked into the retry prompt:\n%s", captured)
	}
	if !strings.Contains(captured, "REDACTED_PRIVATE_KEY_BLOCK") {
		t.Errorf("expected redaction marker in the retry prompt:\n%s", captured)
	}
	// Sink 2: the persisted post-mortem file must likewise be redacted.
	persisted := filepath.Join(stateDir, "slot-1-attempt1-postmortem.md")
	data, err := os.ReadFile(persisted)
	if err != nil {
		t.Fatalf("expected persisted post-mortem at %s: %v", persisted, err)
	}
	if strings.Contains(string(data), secretFragment) {
		t.Errorf("inline PEM tail leaked into the persisted post-mortem:\n%s", data)
	}
	if !strings.Contains(string(data), "REDACTED_PRIVATE_KEY_BLOCK") {
		t.Errorf("expected redaction marker in the persisted post-mortem:\n%s", data)
	}
}

func TestRespawnDueRetries_MissingLog_GracefulDegradation(t *testing.T) {
	stateDir := t.TempDir()
	captured := ""
	o := postmortemRetryOrchestrator(t, stateDir, &captured)

	s := state.NewState()
	retryAt := time.Now().UTC().Add(-1 * time.Minute)
	s.Sessions["slot-1"] = &state.Session{
		IssueNumber: 42,
		IssueTitle:  "test issue",
		Status:      state.StatusDead,
		RetryCount:  1,
		NextRetryAt: &retryAt,
		Backend:     "claude",
		LogFile:     filepath.Join(stateDir, "logs", "nonexistent.log"),
	}

	o.respawnDueRetries(s, 10)

	if captured == "" {
		t.Fatal("respawn should proceed even without a readable log")
	}
	if strings.Contains(captured, "Prior Attempt Post-Mortem") {
		t.Error("missing log should not add a post-mortem section")
	}
	// No stray post-mortem file written.
	if _, err := os.Stat(filepath.Join(stateDir, "slot-1-attempt1-postmortem.md")); err == nil {
		t.Error("no post-mortem file should be persisted when the log is missing")
	}
}
