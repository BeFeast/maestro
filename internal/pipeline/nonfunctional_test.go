package pipeline

import "testing"

func TestAllPathsNonFunctional(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		files    []string
		want     bool
	}{
		{
			name:  "empty diff is never non-functional",
			files: nil,
			want:  false,
		},
		{
			name:  "docs-only default matches",
			files: []string{"docs/qa/ok-player-486.md"},
			want:  true,
		},
		{
			name:  "nested docs-only default matches",
			files: []string{"docs/a/b/c/record.md", "docs/index.md"},
			want:  true,
		},
		{
			name:  "any code path fails the classification",
			files: []string{"docs/qa/record.md", "internal/state/state.go"},
			want:  false,
		},
		{
			name:  "single code path fails",
			files: []string{"cmd/maestro/main.go"},
			want:  false,
		},
		{
			name:     "project-extended set matches records dir",
			patterns: []string{"docs/**", "qa/**"},
			files:    []string{"qa/session-486.json", "docs/note.md"},
			want:     true,
		},
		{
			name:     "extended set still rejects unlisted code",
			patterns: []string{"docs/**", "qa/**"},
			files:    []string{"qa/session-486.json", "internal/orchestrator/orchestrator.go"},
			want:     false,
		},
		{
			name:  "top-level doc file is not under docs/",
			files: []string{"README.md"},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AllPathsNonFunctional(tc.patterns, tc.files); got != tc.want {
				t.Fatalf("AllPathsNonFunctional(%v, %v) = %v, want %v", tc.patterns, tc.files, got, tc.want)
			}
		})
	}
}

func TestMatchesPathGlobDelegates(t *testing.T) {
	if !MatchesPathGlob("docs/**", "docs/a/b.md") {
		t.Fatalf("expected docs/** to match nested docs path")
	}
	if MatchesPathGlob("docs/**", "internal/x.go") {
		t.Fatalf("expected docs/** not to match a code path")
	}
}
