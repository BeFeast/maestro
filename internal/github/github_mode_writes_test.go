package github

// GitHub-mode write invocation tests (#1172 M3). The forgejo dispatch added a
// branch at the top of every write method; these pin that a zero-ForgeConfig
// client still issues the EXACT historical gh argv for each write — the write
// sibling of TestGitHubModeCoreReadsUnchanged. The gh CLI is replaced by a
// recording stub, so nothing touches the network.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/befeast/maestro/internal/config"
)

// stubGHArgvRecorder points ghExecutable at a script that appends every
// invocation's argv to a log (one arg per line, invocations separated by a
// 0x1e record-separator line), prints the canned stdout, and exits with the
// given code. The returned reader parses the log back into per-invocation
// argv slices, so assertions compare byte-identical argument vectors.
func stubGHArgvRecorder(t *testing.T, stdout string, exitCode int) func() [][]string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "argv.log")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" >> \"%s\"\nprintf '\\036\\n' >> \"%s\"\nprintf '%%s' '%s'\nexit %d\n",
		logPath, logPath, stdout, exitCode)
	stubGHExecutable(t, script)
	return func() [][]string {
		raw, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("read argv log: %v", err)
		}
		var invocations [][]string
		for _, block := range strings.Split(string(raw), "\x1e\n") {
			block = strings.TrimSuffix(block, "\n")
			if block == "" {
				continue
			}
			invocations = append(invocations, strings.Split(block, "\n"))
		}
		return invocations
	}
}

// TestGitHubModeWriteArgsUnchanged walks every ported write on a github-mode
// client and asserts the exact pre-M3 gh argv, in order — including the
// comment-then-close pair, the empty-comment skip, and the URL-derived
// numbers from CreatePR (/pull/N scrape) and CreateIssue (last path segment).
func TestGitHubModeWriteArgsUnchanged(t *testing.T) {
	readInvocations := stubGHArgvRecorder(t, "https://github.com/owner/repo/pull/12", 0)
	c := New("owner/repo", config.ForgeConfig{})
	const sha = "59e99c49c27d3e2f73bae1657f07cd2f9a15f926"

	if n, err := c.CreatePR("feat: t", "pr body", "main", "feat/x"); err != nil || n != 12 {
		t.Fatalf("CreatePR = %d, %v; want 12 scraped from /pull/12", n, err)
	}
	if err := c.UpdatePRBody(7, "new body"); err != nil {
		t.Fatalf("UpdatePRBody: %v", err)
	}
	if err := c.ClosePR(7, "why closed"); err != nil {
		t.Fatalf("ClosePR: %v", err)
	}
	if err := c.ClosePR(8, ""); err != nil {
		t.Fatalf("ClosePR (empty comment): %v", err)
	}
	if err := c.MergePRAtHead(7, sha); err != nil {
		t.Fatalf("MergePRAtHead: %v", err)
	}
	if err := c.MarkPRReady(7); err != nil {
		t.Fatalf("MarkPRReady: %v", err)
	}
	if err := c.UpdateBranch(7); err != nil {
		t.Fatalf("UpdateBranch: %v", err)
	}
	if err := c.CloseIssue(9, "done"); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}
	if n, err := c.CreateIssue("it", "ib", []string{"l1", "l2"}); err != nil || n != 12 {
		t.Fatalf("CreateIssue = %d, %v; want 12 from the URL's last segment", n, err)
	}
	if err := c.EditIssueBody(9, "eb"); err != nil {
		t.Fatalf("EditIssueBody: %v", err)
	}
	if err := c.AddIssueLabel(9, "blocked"); err != nil {
		t.Fatalf("AddIssueLabel: %v", err)
	}
	if err := c.RemoveIssueLabel(9, "blocked"); err != nil {
		t.Fatalf("RemoveIssueLabel: %v", err)
	}
	if err := c.EnsureLabel("lab", "#d93f0b", "desc"); err != nil {
		t.Fatalf("EnsureLabel: %v", err)
	}
	if err := c.CommentIssue(9, "hi"); err != nil {
		t.Fatalf("CommentIssue: %v", err)
	}
	if err := c.CommentPR(7, "yo"); err != nil {
		t.Fatalf("CommentPR: %v", err)
	}
	if err := c.CreateRelease("v1.2.3", "Release title"); err != nil {
		t.Fatalf("CreateRelease: %v", err)
	}

	want := [][]string{
		{"pr", "create", "--repo", "owner/repo", "--title", "feat: t", "--body", "pr body", "--base", "main", "--head", "feat/x"},
		{"pr", "edit", "7", "--repo", "owner/repo", "--body", "new body"},
		{"pr", "comment", "7", "--repo", "owner/repo", "--body", "why closed"},
		{"pr", "close", "7", "--repo", "owner/repo"},
		{"pr", "close", "8", "--repo", "owner/repo"},
		{"pr", "merge", "7", "--repo", "owner/repo", "--squash", "--delete-branch", "--match-head-commit", sha},
		{"pr", "ready", "7", "--repo", "owner/repo"},
		{"pr", "update-branch", "7", "--repo", "owner/repo"},
		{"issue", "comment", "9", "--repo", "owner/repo", "--body", "done"},
		{"issue", "close", "9", "--repo", "owner/repo"},
		{"issue", "create", "--repo", "owner/repo", "--title", "it", "--body", "ib", "--label", "l1", "--label", "l2"},
		{"issue", "edit", "9", "--repo", "owner/repo", "--body", "eb"},
		{"issue", "edit", "9", "--repo", "owner/repo", "--add-label", "blocked"},
		{"issue", "edit", "9", "--repo", "owner/repo", "--remove-label", "blocked"},
		{"label", "create", "lab", "--repo", "owner/repo", "--force", "--color", "d93f0b", "--description", "desc"},
		{"issue", "comment", "9", "--repo", "owner/repo", "--body", "hi"},
		{"pr", "comment", "7", "--repo", "owner/repo", "--body", "yo"},
		{"release", "create", "v1.2.3", "--repo", "owner/repo", "--title", "Release title", "--generate-notes"},
	}
	got := readInvocations()
	if len(got) != len(want) {
		t.Fatalf("gh invocations = %d, want %d:\n%q", len(got), len(want), got)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("gh invocation %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
}

// TestGitHubModeMergeNotUpToDateWrapsSentinel pins the gh-path half of the
// merge error contract: the historical "not up to date" stderr needle now
// also wraps ErrMergeNotUpToDate (errors.Is-matchable) while keeping the full
// gh output in the message; any other merge failure stays unclassified.
func TestGitHubModeMergeNotUpToDateWrapsSentinel(t *testing.T) {
	const sha = "59e99c49c27d3e2f73bae1657f07cd2f9a15f926"
	t.Run("needle match wraps the sentinel", func(t *testing.T) {
		stubGHArgvRecorder(t, "X Pull Request is not mergeable: the head branch is not up to date with the base branch", 1)
		c := New("owner/repo", config.ForgeConfig{})
		err := c.MergePRAtHead(10, sha)
		if err == nil {
			t.Fatal("merge must fail when gh exits non-zero")
		}
		if !errors.Is(err, ErrMergeNotUpToDate) {
			t.Fatalf("err = %v, want the sentinel wrapped on the gh needle", err)
		}
		if !strings.Contains(err.Error(), "the head branch is not up to date with the base branch") {
			t.Fatalf("err = %v, want the full gh output preserved in the chain", err)
		}
	})
	t.Run("other failure stays unclassified", func(t *testing.T) {
		stubGHArgvRecorder(t, "X Pull Request is not mergeable: the merge commit cannot be cleanly created", 1)
		c := New("owner/repo", config.ForgeConfig{})
		err := c.MergePRAtHead(10, sha)
		if err == nil {
			t.Fatal("merge must fail when gh exits non-zero")
		}
		if errors.Is(err, ErrMergeNotUpToDate) {
			t.Fatalf("err = %v: a real-conflict failure must NOT be classified as out-of-date", err)
		}
	})
}
