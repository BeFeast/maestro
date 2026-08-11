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

// TestForgejoModeFailsLoud pins the M2 invariant: on a forgejo-mode client
// EVERY still-unported receiver method fails with ErrForgejoNotSupported and
// the gh CLI is never touched — one sample per receiver-choke shape, plus one
// per explicit early guard (the methods that would otherwise half-work now
// that the core-read funnels are ported). visual_evidence.go no longer
// contributes a sample: both of its methods are ported.
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
		// M4 flip note (#1172): the check-runs/status consumer family moved
		// OUT of this list — PRCIStatus, PRCheckRollup, PRMergeStatus,
		// PRChecksOutput, PRFailingChecks, CIFailureSummary,
		// CollectPRReviewFeedback, greptileReviewComments,
		// PRReviewGateVerdict/namedReviewStreamVerdict,
		// PRHasCriticalReviewOnHead, CollectReviewFeedback,
		// PRBlockingReviewFindingsOnHead, and PRUnresolvedReviewThreadsOnHead
		// are ported (forgejo-mode httptest coverage lives in
		// forgejo_port_m4_test.go). What stays guarded below is the
		// greptile-only family (new explicit guards), the raw check-run /
		// commit reads that have no forgejo equivalent (funnel chokes), and
		// the unported write + Projects surface.

		// github.go — NEW explicit greptile-only guards (M4): their reads are
		// all ported now, so without the guard each would half-work — forever
		// "pending"/"no findings" on a reviewer that never posts on forgejo.
		{"PRGreptileApproved", func() error { _, _, err := c.PRGreptileApproved(1); return err }},
		{"prGreptileReviewStreamVerdict", func() error { _, err := c.prGreptileReviewStreamVerdict(1); return err }},
		{"PRHighSeverityReviewOnHead", func() error { _, _, _, err := c.PRHighSeverityReviewOnHead(1); return err }},
		// github.go — c.ghAPI funnel.
		{"RateLimit", func() error { _, err := c.RateLimit(); return err }},
		// github.go — c.ghAPI funnel: commit timestamp read, only reachable
		// from the greptile comment fallback (behind the guard above), kept
		// loud in its own right.
		{"commitCommittedAt", func() error { _, err := c.commitCommittedAt("59e99c49c27d3e2f73bae1657f07cd2f9a15f926"); return err }},
		// github.go — c.ghAPIWithArgs funnel: the raw check-run reads. No
		// check-runs API exists on forgejo; the M4 rollup consumers dispatch
		// to statuses-only siblings BEFORE ever reaching these.
		{"checkRunsForSHA", func() error { _, err := c.checkRunsForSHA("59e99c49c27d3e2f73bae1657f07cd2f9a15f926"); return err }},
		{"checkRunAnnotations", func() error { _, err := c.checkRunAnnotations(1); return err }},
		// github.go — c.ghExec funnel: the writes are ported in M3, so the one
		// remaining write-shaped guard is MarkPRReady (EditPullRequestOption
		// has no draft toggle on 16.0.1 — explicit guard until M7).
		{"MarkPRReady", func() error { return c.MarkPRReady(1) }},
		// github_projects.go — c.ghOutputContext funnel (GraphQL Projects).
		{"DiscoverProject", func() error { _, err := c.DiscoverProject(1); return err }},
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
// token fault loudly, naming the env var to export. PORTED methods surface it
// as a plain error WITHOUT the sentinel — the operation is supported, the
// configuration is broken, and a caller branching on ErrForgejoNotSupported
// to mean "feature absent on this forge" must not confuse the two. Unported
// methods keep wrapping the sentinel (with the token fault in the message).
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
		name         string
		call         func() error
		wantSentinel bool
	}{
		// Ported reads: the token fault, no sentinel.
		{"GetIssue", func() error { _, err := c.GetIssue(1); return err }, false},
		{"ListOpenPRs", func() error { _, err := c.ListOpenPRs(); return err }, false},
		{"BranchHeadSHA", func() error { _, err := c.BranchHeadSHA("main"); return err }, false},
		// Ported write (M3): same contract as the reads — the token fault
		// without the sentinel.
		{"CreatePR", func() error { _, err := c.CreatePR("t", "b", "main", "head"); return err }, false},
		// Unported write: sentinel, with the token fault in the message.
		{"MarkPRReady", func() error { return c.MarkPRReady(1) }, true},
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
		if got := errors.Is(err, ErrForgejoNotSupported); got != s.wantSentinel {
			t.Errorf("%s: errors.Is(err, ErrForgejoNotSupported) = %v, want %v (err = %v)", s.name, got, s.wantSentinel, err)
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
