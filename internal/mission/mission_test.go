package mission

import (
	"testing"

	"github.com/befeast/maestro/internal/github"
)

func TestIsMissionIssue(t *testing.T) {
	tests := []struct {
		name          string
		issue         github.Issue
		missionLabels []string
		want          bool
	}{
		{
			name: "mission label matches",
			issue: github.Issue{
				Number: 1,
				Labels: []struct {
					Name string `json:"name"`
				}{{Name: "mission"}},
			},
			missionLabels: []string{"mission", "epic"},
			want:          true,
		},
		{
			name: "epic label matches",
			issue: github.Issue{
				Number: 2,
				Labels: []struct {
					Name string `json:"name"`
				}{{Name: "epic"}},
			},
			missionLabels: []string{"mission", "epic"},
			want:          true,
		},
		{
			name: "no mission label",
			issue: github.Issue{
				Number: 3,
				Labels: []struct {
					Name string `json:"name"`
				}{{Name: "bug"}},
			},
			missionLabels: []string{"mission", "epic"},
			want:          false,
		},
		{
			name: "case insensitive match",
			issue: github.Issue{
				Number: 4,
				Labels: []struct {
					Name string `json:"name"`
				}{{Name: "Mission"}},
			},
			missionLabels: []string{"mission"},
			want:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsMissionIssue(tt.issue, tt.missionLabels)
			if got != tt.want {
				t.Errorf("IsMissionIssue() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseChildSpecs(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []ChildSpec
	}{
		{
			name: "basic task list",
			body: `## Summary

Some epic description.

## Tasks

- [ ] Implement user authentication
  Add JWT-based auth flow
- [ ] Add database migrations
  Create schema for users table
- [ ] Write API tests

## Notes

Some extra info.`,
			want: []ChildSpec{
				{Title: "Implement user authentication", Body: "Add JWT-based auth flow"},
				{Title: "Add database migrations", Body: "Create schema for users table"},
				{Title: "Write API tests"},
			},
		},
		{
			name: "child issues heading",
			body: `## Child Issues

- [ ] First task
- [ ] Second task`,
			want: []ChildSpec{
				{Title: "First task"},
				{Title: "Second task"},
			},
		},
		{
			name: "subtasks heading",
			body: `## Subtasks

- [ ] Do thing A
  Details about A
- [ ] Do thing B`,
			want: []ChildSpec{
				{Title: "Do thing A", Body: "Details about A"},
				{Title: "Do thing B"},
			},
		},
		{
			name: "no task section",
			body: `## Summary

Just a regular issue with no tasks section.`,
			want: nil,
		},
		{
			name: "empty task section",
			body: `## Tasks

## Notes`,
			want: nil,
		},
		{
			name: "multi-line body",
			body: `## Tasks

- [ ] Complex task
  Line one of description
  Line two of description`,
			want: []ChildSpec{
				{Title: "Complex task", Body: "Line one of description\nLine two of description"},
			},
		},
		{
			name: "checked items are also parsed",
			body: `## Tasks

- [x] Already done task
- [ ] Pending task`,
			want: []ChildSpec{
				{Title: "Already done task"},
				{Title: "Pending task"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseChildSpecs(tt.body)
			if len(got) != len(tt.want) {
				t.Fatalf("ParseChildSpecs() returned %d specs, want %d\ngot: %+v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i].Title != tt.want[i].Title {
					t.Errorf("spec[%d].Title = %q, want %q", i, got[i].Title, tt.want[i].Title)
				}
				if got[i].Body != tt.want[i].Body {
					t.Errorf("spec[%d].Body = %q, want %q", i, got[i].Body, tt.want[i].Body)
				}
			}
		})
	}
}
