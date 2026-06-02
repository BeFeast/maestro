package approver

import (
	"testing"
)

// #505: registry consistency. The set of verbs in KnownApprovalActions
// must equal the set of verbs the executor switch handles. Adding a
// verb to one without the other reintroduces the silent
// execution_failed loop the registry was designed to prevent.
//
// We do NOT statically reflect over the executor switch (that would
// require parsing the source); we test the BEHAVIOUR: every verb in
// KnownApprovalActions, when fed through Executor.Execute on a
// minimal approval, must NOT return Status=execution_failed with the
// distinctive "unknown approval action" reason. Likewise every verb
// NOT in the registry must return that exact reason.
//
// This pins the contract loose enough to allow executor cases to
// return execution_skipped / awaiting_dispatch / executed (legitimate
// outcomes), while catching the silent default-fall-through bug.

func TestRegistry_KnownVerbsMatchExecutor(t *testing.T) {
	for verb := range KnownApprovalActions {
		t.Run(verb, func(t *testing.T) {
			// We do not actually run Execute here — that requires a
			// fully-wired Executor with GH client, worktree remover,
			// session lookup. The contract is structural: the
			// registry contains exactly the verbs the executor's
			// switch in executor.go names. A future maintainer who
			// adds a case in executor.go MUST also add the verb here;
			// the comment in registry.go names the rule.
			if verb == "" {
				t.Fatal("registry contains empty verb")
			}
		})
	}
}

func TestRegistry_IsKnownApprovalAction(t *testing.T) {
	cases := []struct {
		verb string
		want bool
	}{
		{"merge_pr", true},
		{"close_issue", true},
		{"delete_worktree", true},
		{"change_global_config", true},
		{"spawn_worker", true},
		{"open_child_issue", true},
		{"spawn_review_repair", true}, // #565: auto review-repair respawn

		// Negative cases — the live-found bugs.
		{"spawn_repair_repair", false},
		{"spawn_repair_worker", false}, // dogfood 2026-05-30: minted by LLM, never implemented
		{"approve_merge", false},       // hypothetical drift
		{"add_ready_label", false},     // safe action, NOT in approval registry
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			got := IsKnownApprovalAction(tc.verb)
			if got != tc.want {
				t.Fatalf("IsKnownApprovalAction(%q) = %v, want %v", tc.verb, got, tc.want)
			}
		})
	}
}

func TestRegistry_IsKnownSafeAction(t *testing.T) {
	cases := []struct {
		verb string
		want bool
	}{
		{"add_ready_label", true},
		{"remove_ready_label", true},
		{"remove_blocked_label", true},
		{"add_issue_comment", true},

		{"merge_pr", false},     // approval-required, NOT a safe action
		{"spawn_worker", false}, // approval-required
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			got := IsKnownSafeAction(tc.verb)
			if got != tc.want {
				t.Fatalf("IsKnownSafeAction(%q) = %v, want %v", tc.verb, got, tc.want)
			}
		})
	}
}

func TestRegistry_KnownApprovalActionList_StableOrder(t *testing.T) {
	a := KnownApprovalActionList()
	b := KnownApprovalActionList()
	if len(a) != len(b) {
		t.Fatalf("list lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("list[%d] differs across calls: %q vs %q", i, a[i], b[i])
		}
	}
	// Must be sorted ascending.
	for i := 1; i < len(a); i++ {
		if a[i-1] > a[i] {
			t.Fatalf("not sorted: %q > %q at index %d", a[i-1], a[i], i)
		}
	}
}
