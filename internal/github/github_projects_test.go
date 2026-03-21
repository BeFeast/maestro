package github

import (
	"testing"
)

func TestKnownProjects_ContainsExpectedProjects(t *testing.T) {
	for _, num := range []int{4, 5} {
		cfg, ok := knownProjects[num]
		if !ok {
			t.Errorf("knownProjects missing project number %d", num)
			continue
		}
		if cfg.ProjectID == "" {
			t.Errorf("project %d has empty ProjectID", num)
		}
		if cfg.StatusFieldID == "" {
			t.Errorf("project %d has empty StatusFieldID", num)
		}
		for _, status := range []ProjectStatus{ProjectStatusTodo, ProjectStatusInProgress, ProjectStatusDone} {
			if _, ok := cfg.StatusOptions[status]; !ok {
				t.Errorf("project %d missing status option %q", num, status)
			}
		}
	}
}

func TestKnownProjects_UnknownProjectNumber(t *testing.T) {
	if _, ok := knownProjects[999]; ok {
		t.Error("expected project 999 to not exist in knownProjects")
	}
}

func TestProjectStatusConstants(t *testing.T) {
	if ProjectStatusTodo != "todo" {
		t.Errorf("ProjectStatusTodo = %q, want %q", ProjectStatusTodo, "todo")
	}
	if ProjectStatusInProgress != "in_progress" {
		t.Errorf("ProjectStatusInProgress = %q, want %q", ProjectStatusInProgress, "in_progress")
	}
	if ProjectStatusDone != "done" {
		t.Errorf("ProjectStatusDone = %q, want %q", ProjectStatusDone, "done")
	}
}

func TestDetectAndCacheProjectConfig_InvalidRepoFormat(t *testing.T) {
	c := &Client{Repo: "invalid-no-slash"}
	_, err := c.DetectAndCacheProjectConfig()
	if err == nil {
		t.Error("expected error for invalid repo format, got nil")
	}
}
