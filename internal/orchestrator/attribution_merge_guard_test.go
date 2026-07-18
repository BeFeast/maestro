package orchestrator

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

// mergeGuardSession builds a merge-eligible session that carries attribution, so
// autoMergePRs will attempt the pre-merge attribution amend before deciding
// whether to merge.
func mergeGuardSession(branch string, pr int) *state.Session {
	return &state.Session{
		IssueNumber: 858,
		IssueTitle:  "attribution amend race",
		Status:      state.StatusPROpen,
		PRNumber:    pr,
		Branch:      branch,
		Worktree:    "/tmp/does-not-matter",
		Attribution: []state.BackendAttribution{{
			Backend:   "claude",
			Provider:  "anthropic",
			Model:     "opus-4.8",
			Effort:    "xhigh",
			StartedAt: time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC),
		}},
	}
}

// When the pre-merge attribution amend defers (the branch is still advancing
// under a concurrent worker/operator push), autoMergePRs must NOT let the PR
// reach the merge decision this cycle: merging now would land it permanently
// without the required Maestro-Backend trailer while the deferral's promised
// retry never runs. CI observation is still safe and required for watchdog
// truth; a later quiet cycle can stamp the trailer and then merge (#858/#940).
func TestAutoMergePRs_DeferredAttributionBlocksMerge(t *testing.T) {
	branch := "feat/sup-858-amend-race"
	s := state.NewState()
	s.Sessions["sup-858"] = mergeGuardSession(branch, 864)

	ciQueried := false
	merged := false
	o := &Orchestrator{
		cfg: &config.Config{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{{Number: 864, HeadRefName: branch}}, nil
		},
		amendHeadFn: func(worktreePath, b string, attribution []state.BackendAttribution, now time.Time) error {
			return errAmendDeferred
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			ciQueried = true
			return "success", nil
		},
		ghMergePRFn: func(prNumber int) error {
			merged = true
			return nil
		},
	}

	o.autoMergePRs(s)

	if merged {
		t.Fatal("deferred attribution amend must block the merge — ghMergePRFn was called (#858)")
	}
	if !ciQueried {
		t.Fatal("deferred attribution amend must still observe CI for watchdog truth")
	}
	// The session stays merge-eligible so a later quiet cycle completes it.
	if s.Sessions["sup-858"].Status != state.StatusPROpen {
		t.Fatalf("session status = %q, want pr_open (still awaiting a quiet cycle)", s.Sessions["sup-858"].Status)
	}
}

// An unexpected amend failure is just as unsafe as a known deferral: the
// trailer is not known to be on the remote, so the merge path must fail closed.
// Read-only CI observation remains allowed and keeps durable gate state fresh.
func TestAutoMergePRs_UnexpectedAttributionFailureBlocksMerge(t *testing.T) {
	branch := "feat/sup-873-amend-error"
	s := state.NewState()
	s.Sessions["sup-873"] = mergeGuardSession(branch, 875)

	ciQueried := false
	merged := false
	o := &Orchestrator{
		cfg: &config.Config{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{{Number: 875, HeadRefName: branch}}, nil
		},
		amendHeadFn: func(worktreePath, b string, attribution []state.BackendAttribution, now time.Time) error {
			return errors.New("ordinary git failure")
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			ciQueried = true
			return "success", nil
		},
		ghMergePRFn: func(prNumber int) error {
			merged = true
			return nil
		},
	}

	o.autoMergePRs(s)

	if merged {
		t.Fatal("unexpected attribution failure must block the merge")
	}
	if !ciQueried {
		t.Fatal("unexpected attribution failure must still observe CI for watchdog truth")
	}
}

// The guard is specific to the deferral: when the attribution amend lands (or is
// a no-op), autoMergePRs proceeds past the attribution step into the normal
// merge flow (here it reaches the CI query). This proves the guard does not
// over-block PRs whose trailer is already settled.
func TestAutoMergePRs_SettledAttributionProceedsToMergeFlow(t *testing.T) {
	branch := "feat/sup-858-amend-settled"
	s := state.NewState()
	s.Sessions["sup-858"] = mergeGuardSession(branch, 870)

	ciQueried := false
	o := &Orchestrator{
		cfg: &config.Config{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{{Number: 870, HeadRefName: branch}}, nil
		},
		amendHeadFn: func(worktreePath, b string, attribution []state.BackendAttribution, now time.Time) error {
			return nil // trailer landed / already present
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			ciQueried = true
			// Return an error so the flow stops here without needing the full
			// review-gate/merge machinery; reaching this hook is the assertion.
			return "", errors.New("ci lookup stops the test here")
		},
	}

	o.autoMergePRs(s)

	if !ciQueried {
		t.Fatal("a settled attribution amend must let the merge flow proceed past attribution to the CI query")
	}
}

// amendGitCommand must pin LC_ALL=C on the git subprocess so git's "stale info"
// force-with-lease rejection is emitted in English regardless of the host
// locale. Without this the git child inherits the orchestrator's environment,
// and under a non-English locale the rejection is translated — so
// isStaleInfoLeaseRejection would miss the real race and take the hard-error
// path instead of retrying/deferring (#858).
func TestAmendGitCommand_ForcesCLocale(t *testing.T) {
	// Simulate an orchestrator running under a translating locale.
	t.Setenv("LC_ALL", "fr_FR.UTF-8")

	cmd := amendGitCommand(t.TempDir(), "status", "--porcelain")

	// os/exec keeps the last value for a duplicate key, so resolve LC_ALL the
	// same way the child process will: last occurrence wins.
	got := ""
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "LC_ALL=") {
			got = strings.TrimPrefix(kv, "LC_ALL=")
		}
	}
	if got != "C" {
		t.Fatalf("amendGitCommand LC_ALL resolved to %q, want \"C\" (inherited locale would translate git's stale-info text)", got)
	}
}

// isStaleInfoLeaseRejection recognizes git's English stale-info marker (which is
// what git emits under the forced C locale) and is unfazed by casing/framing,
// while not false-positiving on unrelated push failures.
func TestIsStaleInfoLeaseRejection(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"english stale info", " ! [rejected]        feat -> feat (stale info)", true},
		{"uppercase framing", "! [REJECTED] (STALE INFO)", true},
		{"non-fast-forward is not stale", " ! [rejected] feat -> feat (non-fast-forward)", false},
		{"auth failure is not stale", "fatal: Authentication failed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStaleInfoLeaseRejection(tc.out); got != tc.want {
				t.Fatalf("isStaleInfoLeaseRejection(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}
