package worker

import (
	"testing"
)

func TestParseIssueFromBranch_MaestroStyle(t *testing.T) {
	tests := []struct {
		branch string
		want   int
	}{
		{"feat/pan-1-42-my-feature-title", 42},
		{"feat/mae-33-9-feat-maestro-import", 9},
		{"feat/abc-100-123-some-title", 123},
		{"feat/ab-1-7-short", 7},
	}
	for _, tt := range tests {
		got, err := ParseIssueFromBranch(tt.branch)
		if err != nil {
			t.Errorf("ParseIssueFromBranch(%q) error: %v", tt.branch, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseIssueFromBranch(%q) = %d, want %d", tt.branch, got, tt.want)
		}
	}
}

func TestParseIssueFromBranch_IssueStyle(t *testing.T) {
	tests := []struct {
		branch string
		want   int
	}{
		{"feat/issue-42-add-login", 42},
		{"fix/issue-7-bug", 7},
		{"issue-123-something", 123},
		{"feature/issue/99", 99},
	}
	for _, tt := range tests {
		got, err := ParseIssueFromBranch(tt.branch)
		if err != nil {
			t.Errorf("ParseIssueFromBranch(%q) error: %v", tt.branch, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseIssueFromBranch(%q) = %d, want %d", tt.branch, got, tt.want)
		}
	}
}

func TestParseIssueFromBranch_NoMatch(t *testing.T) {
	branches := []string{
		"main",
		"develop",
		"feat/add-login-page",
		"fix/typo-in-readme",
	}
	for _, branch := range branches {
		_, err := ParseIssueFromBranch(branch)
		if err == nil {
			t.Errorf("ParseIssueFromBranch(%q) expected error, got nil", branch)
		}
	}
}

func TestParseWorktreeLine(t *testing.T) {
	tests := []struct {
		line       string
		wantPath   string
		wantBranch string
		wantOK     bool
	}{
		{
			"/home/user/.worktrees/maestro/pan-1  abc1234 [feat/pan-1-42-title]",
			"/home/user/.worktrees/maestro/pan-1",
			"feat/pan-1-42-title",
			true,
		},
		{
			"/home/user/repo  def5678 [main]",
			"/home/user/repo",
			"main",
			true,
		},
		{
			"/home/user/repo  abc1234 (detached HEAD)",
			"", "", false,
		},
		{
			"/home/user/repo  abc1234 (bare)",
			"", "", false,
		},
		{
			"", "", "", false,
		},
	}
	for _, tt := range tests {
		path, branch, ok := parseWorktreeLine(tt.line)
		if ok != tt.wantOK {
			t.Errorf("parseWorktreeLine(%q) ok=%v, want %v", tt.line, ok, tt.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if path != tt.wantPath {
			t.Errorf("parseWorktreeLine(%q) path=%q, want %q", tt.line, path, tt.wantPath)
		}
		if branch != tt.wantBranch {
			t.Errorf("parseWorktreeLine(%q) branch=%q, want %q", tt.line, branch, tt.wantBranch)
		}
	}
}

func TestParseSlotNumber(t *testing.T) {
	tests := []struct {
		slotName string
		want     int
	}{
		{"pan-1", 1},
		{"mae-33", 33},
		{"abc-100", 100},
		{"noslot", 0},
		{"", 0},
		{"pan-abc", 0},
	}
	for _, tt := range tests {
		got := parseSlotNumber(tt.slotName)
		if got != tt.want {
			t.Errorf("parseSlotNumber(%q) = %d, want %d", tt.slotName, got, tt.want)
		}
	}
}
