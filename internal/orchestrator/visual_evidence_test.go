package orchestrator

import (
	"fmt"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/state"
)

func visualTestConfig() *config.Config {
	return &config.Config{
		Repo: "owner/repo",
		Verify: config.VerifyConfig{Visual: config.VerifyVisualConfig{
			Enabled: true,
			Command: "./capture.sh",
			Paths:   []string{"web/**", "**/*.jsx"},
		}},
	}
}

func visualTestOrchestrator(cfg *config.Config) (*Orchestrator, *[]string) {
	comments := &[]string{}
	o := &Orchestrator{
		cfg:      cfg,
		notifier: &notify.Notifier{},
		ghCommentPRFn: func(prNumber int, body string) error {
			*comments = append(*comments, body)
			return nil
		},
		runVisualCaptureFn: func(v config.VerifyVisualConfig, worktreePath string) ([]string, error) {
			return nil, fmt.Errorf("capture command failed: exit 1")
		},
	}
	return o, comments
}

func TestEnsureVisualEvidence_NonUIPRUnaffected(t *testing.T) {
	o, comments := visualTestOrchestrator(visualTestConfig())
	o.ghPRChangedFilesFn = func(prNumber int) ([]string, error) {
		return []string{"internal/api/server.go", "docs/readme.md"}, nil
	}
	o.ghPRVisualEvidenceAttachedFn = func(prNumber int) (bool, error) {
		t.Fatal("non-UI PR must not check for attached evidence")
		return false, nil
	}

	sess := &state.Session{IssueNumber: 1, Status: state.StatusPROpen, Worktree: t.TempDir()}
	o.ensureVisualEvidence("slot-0", sess, github.PR{Number: 12})

	if sess.VisualEvidence != state.VisualEvidenceNotRequired {
		t.Fatalf("VisualEvidence = %q, want %q", sess.VisualEvidence, state.VisualEvidenceNotRequired)
	}
	if len(*comments) != 0 {
		t.Fatalf("non-UI PR must not get a warning comment, got %v", *comments)
	}
}

func TestEnsureVisualEvidence_AttachedEvidencePasses(t *testing.T) {
	o, comments := visualTestOrchestrator(visualTestConfig())
	o.ghPRChangedFilesFn = func(prNumber int) ([]string, error) {
		return []string{"web/src/app.css"}, nil
	}
	o.ghPRVisualEvidenceAttachedFn = func(prNumber int) (bool, error) {
		return true, nil
	}

	sess := &state.Session{IssueNumber: 2, Status: state.StatusPROpen, Worktree: t.TempDir()}
	o.ensureVisualEvidence("slot-0", sess, github.PR{Number: 13})

	if sess.VisualEvidence != state.VisualEvidenceAttached {
		t.Fatalf("VisualEvidence = %q, want %q", sess.VisualEvidence, state.VisualEvidenceAttached)
	}
	if len(*comments) != 0 {
		t.Fatalf("attached evidence must not warn, got %v", *comments)
	}
}

func TestEnsureVisualEvidence_MissingEvidenceWarnsOnce(t *testing.T) {
	o, comments := visualTestOrchestrator(visualTestConfig())
	o.ghPRChangedFilesFn = func(prNumber int) ([]string, error) {
		return []string{"web/src/app.css", "src/Panel.jsx"}, nil
	}
	o.ghPRVisualEvidenceAttachedFn = func(prNumber int) (bool, error) {
		return false, nil
	}

	sess := &state.Session{IssueNumber: 3, IssueTitle: "ui work", Status: state.StatusPROpen, Worktree: t.TempDir()}
	pr := github.PR{Number: 14}
	o.ensureVisualEvidence("slot-0", sess, pr)

	if sess.VisualEvidence != state.VisualEvidenceMissing {
		t.Fatalf("VisualEvidence = %q, want %q", sess.VisualEvidence, state.VisualEvidenceMissing)
	}
	if !strings.Contains(sess.VisualEvidenceDetail, "capture command failed") {
		t.Fatalf("VisualEvidenceDetail = %q, want capture failure detail", sess.VisualEvidenceDetail)
	}
	if len(*comments) != 1 {
		t.Fatalf("expected exactly one warning comment, got %d", len(*comments))
	}
	for _, want := range []string{
		"verify.visual",
		"`web/src/app.css`",
		"`src/Panel.jsx`",
		"capture command failed",
		"does not block merge",
	} {
		if !strings.Contains((*comments)[0], want) {
			t.Fatalf("warning comment missing %q:\n%s", want, (*comments)[0])
		}
	}

	// Second cycle: one-shot guard suppresses a repeat comment.
	o.ensureVisualEvidence("slot-0", sess, pr)
	if len(*comments) != 1 {
		t.Fatalf("warning comment must not repeat, got %d", len(*comments))
	}
}

func TestEnsureVisualEvidence_CaptureProducesOutputButUnattachedStillWarns(t *testing.T) {
	o, comments := visualTestOrchestrator(visualTestConfig())
	o.ghPRChangedFilesFn = func(prNumber int) ([]string, error) {
		return []string{"web/index.html"}, nil
	}
	o.ghPRVisualEvidenceAttachedFn = func(prNumber int) (bool, error) {
		return false, nil
	}
	o.runVisualCaptureFn = func(v config.VerifyVisualConfig, worktreePath string) ([]string, error) {
		return []string{".maestro/screenshots/home.png"}, nil
	}

	sess := &state.Session{IssueNumber: 4, Status: state.StatusPROpen, Worktree: t.TempDir()}
	o.ensureVisualEvidence("slot-0", sess, github.PR{Number: 15})

	if sess.VisualEvidence != state.VisualEvidenceMissing {
		t.Fatalf("VisualEvidence = %q, want %q", sess.VisualEvidence, state.VisualEvidenceMissing)
	}
	if !strings.Contains(sess.VisualEvidenceDetail, "none are attached") {
		t.Fatalf("VisualEvidenceDetail = %q", sess.VisualEvidenceDetail)
	}
	if len(*comments) != 1 {
		t.Fatalf("expected one warning comment, got %d", len(*comments))
	}
}

func TestEnsureVisualEvidence_TransientErrorRetriesNextCycle(t *testing.T) {
	o, comments := visualTestOrchestrator(visualTestConfig())
	o.ghPRChangedFilesFn = func(prNumber int) ([]string, error) {
		return nil, fmt.Errorf("api rate limited")
	}

	sess := &state.Session{IssueNumber: 5, Status: state.StatusPROpen, Worktree: t.TempDir()}
	o.ensureVisualEvidence("slot-0", sess, github.PR{Number: 16})

	if sess.VisualEvidence != "" {
		t.Fatalf("transient error must leave the session unstamped, got %q", sess.VisualEvidence)
	}
	if len(*comments) != 0 {
		t.Fatalf("transient error must not comment, got %v", *comments)
	}
}

func TestEnsureVisualEvidence_InactiveConfigDoesNothing(t *testing.T) {
	cfg := &config.Config{Repo: "owner/repo"} // verify.visual not configured
	o, comments := visualTestOrchestrator(cfg)
	o.ghPRChangedFilesFn = func(prNumber int) ([]string, error) {
		t.Fatal("inactive verify.visual must not list changed files")
		return nil, nil
	}

	sess := &state.Session{IssueNumber: 6, Status: state.StatusPROpen}
	o.ensureVisualEvidence("slot-0", sess, github.PR{Number: 17})

	if sess.VisualEvidence != "" || len(*comments) != 0 {
		t.Fatalf("inactive config must be a no-op, got status=%q comments=%v", sess.VisualEvidence, *comments)
	}
}

func TestEnsureVisualEvidence_MissingWorktreeStillWarns(t *testing.T) {
	o, comments := visualTestOrchestrator(visualTestConfig())
	o.ghPRChangedFilesFn = func(prNumber int) ([]string, error) {
		return []string{"web/app.jsx"}, nil
	}
	o.ghPRVisualEvidenceAttachedFn = func(prNumber int) (bool, error) {
		return false, nil
	}
	o.runVisualCaptureFn = func(v config.VerifyVisualConfig, worktreePath string) ([]string, error) {
		t.Fatal("capture must not run without a worktree")
		return nil, nil
	}

	sess := &state.Session{IssueNumber: 7, Status: state.StatusPROpen, Worktree: "/no/such/worktree"}
	o.ensureVisualEvidence("slot-0", sess, github.PR{Number: 18})

	if sess.VisualEvidence != state.VisualEvidenceMissing {
		t.Fatalf("VisualEvidence = %q, want %q", sess.VisualEvidence, state.VisualEvidenceMissing)
	}
	if !strings.Contains(sess.VisualEvidenceDetail, "could not be run") {
		t.Fatalf("VisualEvidenceDetail = %q", sess.VisualEvidenceDetail)
	}
	if len(*comments) != 1 {
		t.Fatalf("expected one warning comment, got %d", len(*comments))
	}
}

func TestVisualEvidenceWarningCommentTruncatesFileList(t *testing.T) {
	files := []string{"a.jsx", "b.jsx", "c.jsx", "d.jsx", "e.jsx", "f.jsx", "g.jsx"}
	comment := visualEvidenceWarningComment(files, "capture command failed: exit 1")
	if !strings.Contains(comment, "… and 2 more") {
		t.Fatalf("expected truncation marker in comment:\n%s", comment)
	}
	if strings.Contains(comment, "`g.jsx`") {
		t.Fatalf("files beyond the cap must not be listed:\n%s", comment)
	}
}
