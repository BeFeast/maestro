package orchestrator

import (
	"fmt"
	"testing"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
)

// #697: autoMergePRs must un-wedge green draft PRs by marking them ready for
// review before merging, while leaving deliberately-marked WIP/Partial drafts
// alone.

func TestAutoMergePRs_GreenDraftReadiedThenMerged(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a", Title: "feat: add thing (#100)", IsDraft: true}}
	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel"}
	o, merged := newMergeTestOrchestrator(cfg, prs)

	calls := []string{}
	o.ghMarkPRReadyFn = func(prNumber int) error {
		calls = append(calls, fmt.Sprintf("ready:%d", prNumber))
		return nil
	}
	mergeFn := o.ghMergePRFn
	o.ghMergePRFn = func(prNumber int) error {
		calls = append(calls, fmt.Sprintf("merge:%d", prNumber))
		return mergeFn(prNumber)
	}
	s := makeTestState(prs)

	o.autoMergePRs(s)

	if len(*merged) != 1 || (*merged)[0] != 10 {
		t.Fatalf("merged = %v, want [10]", *merged)
	}
	if len(calls) != 2 || calls[0] != "ready:10" || calls[1] != "merge:10" {
		t.Fatalf("calls = %v, want ready before merge in the same cycle", calls)
	}
}

func TestAutoMergePRs_MarkedDraftSkipped(t *testing.T) {
	cases := []struct {
		name string
		pr   github.PR
	}{
		{"partial title token", github.PR{Number: 10, HeadRefName: "feat/a", Title: "[Partial] feat: add thing (#100)", IsDraft: true}},
		{"wip title token", github.PR{Number: 10, HeadRefName: "feat/a", Title: "[WIP] feat: add thing (#100)", IsDraft: true}},
		{"wip title prefix", github.PR{Number: 10, HeadRefName: "feat/a", Title: "WIP: add thing (#100)", IsDraft: true}},
		{"body marker", github.PR{Number: 10, HeadRefName: "feat/a", Title: "feat: add thing (#100)", Body: "Refs #100\n\n<!-- maestro:partial -->\nblocked on X", IsDraft: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prs := []github.PR{tc.pr}
			cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel"}
			o, merged := newMergeTestOrchestrator(cfg, prs)
			o.ghMarkPRReadyFn = func(prNumber int) error {
				t.Fatalf("markPRReady(%d) called for a deliberately marked draft", prNumber)
				return nil
			}
			s := makeTestState(prs)

			o.autoMergePRs(s)

			if len(*merged) != 0 {
				t.Fatalf("merged = %v, want no merge for a marked draft", *merged)
			}
		})
	}
}

func TestAutoMergePRs_NonDraftNeverMarkedReady(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a", Title: "WIP: still merges when not a draft (#100)"}}
	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel"}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	o.ghMarkPRReadyFn = func(prNumber int) error {
		t.Fatalf("markPRReady(%d) called for a non-draft PR", prNumber)
		return nil
	}
	s := makeTestState(prs)

	o.autoMergePRs(s)

	if len(*merged) != 1 || (*merged)[0] != 10 {
		t.Fatalf("merged = %v, want [10] — non-draft PRs keep the normal merge flow", *merged)
	}
}

func TestAutoMergePRs_MarkReadyFailureSkipsMerge(t *testing.T) {
	prs := []github.PR{{Number: 10, HeadRefName: "feat/a", Title: "feat: add thing (#100)", IsDraft: true}}
	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "parallel"}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	o.ghMarkPRReadyFn = func(prNumber int) error {
		return fmt.Errorf("boom")
	}
	s := makeTestState(prs)

	o.autoMergePRs(s)

	if len(*merged) != 0 {
		t.Fatalf("merged = %v, want no merge when gh pr ready fails", *merged)
	}
}

func TestAutoMergePRs_SequentialMarkedDraftDoesNotStarveOthers(t *testing.T) {
	prs := []github.PR{
		{Number: 10, HeadRefName: "feat/a", Title: "[Partial] feat: blocked thing (#100)", IsDraft: true},
		{Number: 20, HeadRefName: "feat/b", Title: "feat: ready thing (#101)"},
	}
	cfg := &config.Config{Repo: "owner/repo", MergeStrategy: "sequential"}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	s := makeTestState(prs)

	o.autoMergePRs(s)

	if len(*merged) != 1 || (*merged)[0] != 20 {
		t.Fatalf("merged = %v, want [20] — the marked draft must not occupy the sequential merge slot", *merged)
	}
}

func TestPRHasDeliberateDraftMarker(t *testing.T) {
	cases := []struct {
		name string
		pr   github.PR
		want bool
	}{
		{"plain title", github.PR{Title: "feat: add thing (#100)"}, false},
		{"wip token", github.PR{Title: "[WIP] feat: add thing"}, true},
		{"partial token", github.PR{Title: "feat: add thing [Partial]"}, true},
		{"wip colon prefix", github.PR{Title: "WIP: add thing"}, true},
		{"wip word prefix", github.PR{Title: "wip add thing"}, true},
		{"bare wip", github.PR{Title: "WIP"}, true},
		{"partial colon prefix", github.PR{Title: "Partial: add thing"}, true},
		{"draft prefix", github.PR{Title: "Draft: add thing"}, true},
		{"wip mid-title is not a marker", github.PR{Title: "fix: handle WIP label parsing"}, false},
		{"body partial marker", github.PR{Title: "feat: x", Body: "<!-- maestro:partial -->"}, true},
		{"body wip marker", github.PR{Title: "feat: x", Body: "marker: maestro:wip"}, true},
		{"body without marker", github.PR{Title: "feat: x", Body: "Refs #100, partial progress described in prose"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := prHasDeliberateDraftMarker(tc.pr); got != tc.want {
				t.Fatalf("prHasDeliberateDraftMarker(%q / %q) = %v, want %v", tc.pr.Title, tc.pr.Body, got, tc.want)
			}
		})
	}
}
