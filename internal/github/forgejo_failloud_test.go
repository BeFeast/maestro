package github

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/befeast/maestro/internal/config"
)

// stubGHNeverCalled arms the gh transport so that ANY leak from a forgejo-mode
// client to the gh CLI is caught two ways: the ghAPIRunner seam counts (and
// fails) an invocation, and ghExecutable points at a nonexistent binary so a
// path bypassing the seam (ghExec / ghOutputContext shapes) cannot silently
// reach the real gh — the repo is mirrored on github.com, so a leaked call
// would read the GitHub mirror instead of the Forgejo original (#1172 P0).
func stubGHNeverCalled(t *testing.T) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	origRunner := ghAPIRunner
	origExecutable := ghExecutable
	ghAPIRunner = func(args ...string) ([]byte, error) {
		calls.Add(1)
		t.Errorf("ghAPIRunner invoked in forgejo mode: gh %v", args)
		return nil, errors.New("gh must not be called in forgejo mode")
	}
	ghExecutable = "/nonexistent/forgejo-mode-must-not-exec-gh"
	t.Cleanup(func() {
		ghAPIRunner = origRunner
		ghExecutable = origExecutable
	})
	return &calls
}

// TestForgejoModeFailsLoud pins the M2 stage-C invariant: on a forgejo-mode
// client EVERY unported receiver method fails with ErrForgejoNotSupported and
// the gh CLI is never touched. One sample method per receiver-choke shape and
// per file (github.go, github_projects.go, review_threads.go,
// visual_evidence.go).
func TestForgejoModeFailsLoud(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "x")
	calls := stubGHNeverCalled(t)

	c := New("BeFeast/apertune", config.ForgeConfig{
		Kind:     "forgejo",
		BaseURL:  "http://forgejo.test",
		TokenEnv: "TEST_FORGEJO_TOKEN",
	})
	if c.fj == nil {
		t.Fatalf("fj = nil, want constructed forgejo client when token env is set")
	}
	if c.forgeErr != nil {
		t.Fatalf("forgeErr = %v, want nil when token env is set", c.forgeErr)
	}

	samples := []struct {
		name string
		call func() error
	}{
		// github.go — c.ghAPI funnel (getRESTPull under PRCIStatus/PRMergeStatus).
		{"PRCIStatus", func() error { _, err := c.PRCIStatus(1); return err }},
		{"PRMergeStatus", func() error { _, _, err := c.PRMergeStatus(1); return err }},
		{"GetIssue", func() error { _, err := c.GetIssue(1); return err }},
		// github.go — c.ghAPIWithArgs funnel.
		{"ListAllOpenPRs", func() error { _, err := c.ListAllOpenPRs(); return err }},
		// github.go — c.ghExec funnel (writes).
		{"CreatePR", func() error { _, err := c.CreatePR("t", "b", "main", "head"); return err }},
		{"CreateRelease", func() error { return c.CreateRelease("v0.0.1", "t") }},
		// github_projects.go — c.ghOutputContext funnel (GraphQL Projects).
		{"DiscoverProject", func() error { _, err := c.DiscoverProject(1); return err }},
		// review_threads.go — GraphQL through c.ghExec.
		{"PRUnresolvedReviewThreadsOnHead", func() error { _, _, err := c.PRUnresolvedReviewThreadsOnHead(1); return err }},
		// visual_evidence.go — c.ghAPIWithArgs funnel.
		{"PRChangedFiles", func() error { _, err := c.PRChangedFiles(1); return err }},
	}
	for _, s := range samples {
		err := s.call()
		if err == nil {
			t.Errorf("%s: err = nil, want ErrForgejoNotSupported", s.name)
			continue
		}
		if !errors.Is(err, ErrForgejoNotSupported) {
			t.Errorf("%s: err = %v, not errors.Is-matchable against ErrForgejoNotSupported", s.name, err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("gh runner invoked %d time(s) in forgejo mode, want 0", n)
	}
}

// TestForgejoModeEmptyTokenSurfacesForgeErr pins the construction contract: an
// empty (or whitespace) token env leaves fj nil and every call surfaces the
// token fault loudly — naming the env var to export — while staying
// errors.Is-matchable against the sentinel.
func TestForgejoModeEmptyTokenSurfacesForgeErr(t *testing.T) {
	t.Setenv("TEST_FORGEJO_TOKEN", "   ")
	calls := stubGHNeverCalled(t)

	c := New("BeFeast/apertune", config.ForgeConfig{
		Kind:     "forgejo",
		BaseURL:  "http://forgejo.test",
		TokenEnv: "TEST_FORGEJO_TOKEN",
	})
	if c.fj != nil {
		t.Fatalf("fj != nil, want nil when token env is empty")
	}
	if c.forgeErr == nil {
		t.Fatalf("forgeErr = nil, want token-env error")
	}

	samples := []struct {
		name string
		call func() error
	}{
		{"GetIssue", func() error { _, err := c.GetIssue(1); return err }},
		{"ListOpenPRs", func() error { _, err := c.ListOpenPRs(); return err }},
		{"CreatePR", func() error { _, err := c.CreatePR("t", "b", "main", "head"); return err }},
	}
	for _, s := range samples {
		err := s.call()
		if err == nil {
			t.Errorf("%s: err = nil, want token-env failure", s.name)
			continue
		}
		if !strings.Contains(err.Error(), "TEST_FORGEJO_TOKEN") {
			t.Errorf("%s: err = %v, want the empty token env named", s.name, err)
		}
		if !errors.Is(err, ErrForgejoNotSupported) {
			t.Errorf("%s: err = %v, not errors.Is-matchable against ErrForgejoNotSupported", s.name, err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("gh runner invoked %d time(s) in forgejo mode, want 0", n)
	}
}

// TestGitHubModeZeroForgeConfig pins that a zero config.ForgeConfig builds the
// historical GitHub-mode client: no forgejo routing, no forge error.
func TestGitHubModeZeroForgeConfig(t *testing.T) {
	c := New("owner/repo", config.ForgeConfig{})
	if c.isForgejo() {
		t.Fatalf("isForgejo() = true for zero ForgeConfig, want false")
	}
	if c.forgeErr != nil || c.fj != nil {
		t.Fatalf("zero ForgeConfig client carries forge state: fj=%v forgeErr=%v", c.fj, c.forgeErr)
	}
}
