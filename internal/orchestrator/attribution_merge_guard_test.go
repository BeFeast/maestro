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

// mergeGuardSession builds a merge-eligible session carrying a complete
// internal attribution timeline. That state must not cause any target-branch
// mutation (#1000).
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

// Attribution is durable in Maestro state and Fleet, not in product commits.
// The merge path must continue to observe the authoritative PR gate regardless
// of how many internal backend segments the session carries (#974/#1000).
func TestAutoMergePRs_InternalAttributionNeverAmendsProductBranch(t *testing.T) {
	branch := "feat/sup-1000-internal-attribution"
	s := state.NewState()
	s.Sessions["sup-858"] = mergeGuardSession(branch, 864)

	ciQueried := false
	o := &Orchestrator{
		cfg: &config.Config{},
		listOpenPRsFn: func() ([]github.PR, error) {
			return []github.PR{{Number: 864, HeadRefName: branch}}, nil
		},
		ghPRCIStatusFn: func(prNumber int) (string, error) {
			ciQueried = true
			return "", errors.New("stop after proving normal gate observation")
		},
	}

	o.autoMergePRs(s)

	if !ciQueried {
		t.Fatal("normal CI gate observation was blocked by internal attribution state")
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
