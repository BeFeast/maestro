package github

import (
	"reflect"
	"sort"
	"testing"
)

func TestFindBlockers_BasicPattern(t *testing.T) {
	body := "This issue is blocked by #42 and depends on #99."
	patterns := []string{`blocked by #(\d+)`, `depends on #(\d+)`}
	got := FindBlockers(body, patterns)
	sort.Ints(got)
	want := []int{42, 99}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindBlockers() = %v, want %v", got, want)
	}
}

func TestFindBlockers_CaseInsensitive(t *testing.T) {
	body := "BLOCKED BY #10\nBlocked By #20\nblocked by #30"
	patterns := []string{`blocked by #(\d+)`}
	got := FindBlockers(body, patterns)
	sort.Ints(got)
	want := []int{10, 20, 30}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindBlockers() = %v, want %v", got, want)
	}
}

func TestFindBlockers_Deduplicates(t *testing.T) {
	body := "blocked by #42 and also blocked by #42"
	patterns := []string{`blocked by #(\d+)`}
	got := FindBlockers(body, patterns)
	if len(got) != 1 || got[0] != 42 {
		t.Errorf("FindBlockers() = %v, want [42]", got)
	}
}

func TestFindBlockers_NoMatch(t *testing.T) {
	body := "This issue has no blockers."
	patterns := []string{`blocked by #(\d+)`}
	got := FindBlockers(body, patterns)
	if len(got) != 0 {
		t.Errorf("FindBlockers() = %v, want empty", got)
	}
}

func TestFindBlockers_EmptyPatterns(t *testing.T) {
	body := "blocked by #42"
	got := FindBlockers(body, nil)
	if len(got) != 0 {
		t.Errorf("FindBlockers() = %v, want empty", got)
	}
}

func TestFindBlockers_EmptyBody(t *testing.T) {
	patterns := []string{`blocked by #(\d+)`}
	got := FindBlockers("", patterns)
	if len(got) != 0 {
		t.Errorf("FindBlockers() = %v, want empty", got)
	}
}

func TestFindBlockers_InvalidRegex(t *testing.T) {
	body := "blocked by #42"
	patterns := []string{`[invalid`, `blocked by #(\d+)`}
	got := FindBlockers(body, patterns)
	// Should still find the match from the valid pattern
	if len(got) != 1 || got[0] != 42 {
		t.Errorf("FindBlockers() = %v, want [42]", got)
	}
}

func TestFindBlockers_MultipleMatches(t *testing.T) {
	body := "blocked by #10, blocked by #20, blocked by #30"
	patterns := []string{`blocked by #(\d+)`}
	got := FindBlockers(body, patterns)
	sort.Ints(got)
	want := []int{10, 20, 30}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindBlockers() = %v, want %v", got, want)
	}
}

func TestFindBlockers_MultilineBody(t *testing.T) {
	body := `## Description
This feature needs some work.

## Dependencies
- blocked by #100
- depends on #200

## Notes
Nothing else.`
	patterns := []string{`blocked by #(\d+)`, `depends on #(\d+)`}
	got := FindBlockers(body, patterns)
	sort.Ints(got)
	want := []int{100, 200}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindBlockers() = %v, want %v", got, want)
	}
}

func TestHasLabel_CaseInsensitive(t *testing.T) {
	issue := Issue{
		Labels: []struct {
			Name string `json:"name"`
		}{{Name: "Bug"}},
	}
	if !HasLabel(issue, []string{"bug"}) {
		t.Error("HasLabel should be case-insensitive")
	}
}

func TestHasLabel_NoMatch(t *testing.T) {
	issue := Issue{
		Labels: []struct {
			Name string `json:"name"`
		}{{Name: "enhancement"}},
	}
	if HasLabel(issue, []string{"bug"}) {
		t.Error("HasLabel should return false when no labels match")
	}
}
