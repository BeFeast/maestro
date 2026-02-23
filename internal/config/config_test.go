package config

import (
	"testing"
)

func TestParse_IssueLabelsNew(t *testing.T) {
	yaml := `
repo: owner/repo
issue_labels:
  - bug
  - enhancement
  - documentation
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"bug", "enhancement", "documentation"}
	if len(cfg.IssueLabels) != len(want) {
		t.Fatalf("IssueLabels = %v, want %v", cfg.IssueLabels, want)
	}
	for i, l := range cfg.IssueLabels {
		if l != want[i] {
			t.Errorf("IssueLabels[%d] = %q, want %q", i, l, want[i])
		}
	}
}

func TestParse_IssueLabelsBackwardCompat(t *testing.T) {
	yaml := `
repo: owner/repo
issue_label: bug
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.IssueLabels) != 1 || cfg.IssueLabels[0] != "bug" {
		t.Errorf("IssueLabels = %v, want [bug]", cfg.IssueLabels)
	}
}

func TestParse_IssueLabelsDefault(t *testing.T) {
	yaml := `
repo: owner/repo
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.IssueLabels) != 1 || cfg.IssueLabels[0] != "enhancement" {
		t.Errorf("IssueLabels = %v, want [enhancement]", cfg.IssueLabels)
	}
}

func TestParse_IssueLabelsLegacyMerged(t *testing.T) {
	yaml := `
repo: owner/repo
issue_label: bug
issue_labels:
  - enhancement
  - documentation
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// bug from issue_label should be appended to issue_labels
	want := []string{"enhancement", "documentation", "bug"}
	if len(cfg.IssueLabels) != len(want) {
		t.Fatalf("IssueLabels = %v, want %v", cfg.IssueLabels, want)
	}
	for i, l := range cfg.IssueLabels {
		if l != want[i] {
			t.Errorf("IssueLabels[%d] = %q, want %q", i, l, want[i])
		}
	}
}

func TestParse_IssueLabelsLegacyNoDuplicate(t *testing.T) {
	yaml := `
repo: owner/repo
issue_label: enhancement
issue_labels:
  - enhancement
  - bug
`
	cfg, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// enhancement already in issue_labels, should not duplicate
	want := []string{"enhancement", "bug"}
	if len(cfg.IssueLabels) != len(want) {
		t.Fatalf("IssueLabels = %v, want %v", cfg.IssueLabels, want)
	}
	for i, l := range cfg.IssueLabels {
		if l != want[i] {
			t.Errorf("IssueLabels[%d] = %q, want %q", i, l, want[i])
		}
	}
}
