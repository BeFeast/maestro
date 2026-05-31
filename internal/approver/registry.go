package approver

// Phase 1.5 / #505: approval-action registry — the authoritative list of
// verbs the executor knows how to handle. The supervisor and any other
// approval-mint surface MUST consult this registry before persisting a
// pending approval; minting a verb the executor does not implement leads
// to the silent execution_failed loop seen on dogfood 2026-05-30
// (spawn_repair_worker piled up 4 execution_failed records before an
// operator noticed).
//
// Adding a new approval-required verb is a TWO-step deliberate change:
//   1. Add a case in executor.go::Execute() switch.
//   2. Add the verb to KnownApprovalActions below.
// Forgetting step 2 keeps the existing failure mode loud — supervisor
// rejects the verb at mint time, no silent loop.

// KnownApprovalActions is the canonical set of verbs the executor's
// Execute() switch implements (or explicitly skips with awaiting_dispatch
// / execution_skipped). Match it 1:1 with executor.go cases.
//
// Test internal/approver/registry_test.go pins this map against the
// executor switch by reading the source (or, in the simpler shape used
// here, by enumerating the verbs and checking each one routes through
// Execute() without falling into default).
var KnownApprovalActions = map[string]struct{}{
	"merge_pr":              {},
	"close_issue":           {},
	"delete_worktree":       {},
	"change_global_config":  {}, // executor returns execution_skipped pending YAML pipeline
	"spawn_worker":          {}, // executor returns awaiting_dispatch (#515)
	"open_child_issue":      {}, // executor returns awaiting_dispatch (#515)
}

// KnownSafeActions is the canonical set of safe (non-approval-gated)
// verbs the supervisor / HTTP endpoint accepts. They are dispatched by
// internal/server/actions, NOT by executor.Execute, so they live here
// for symmetry but the at-mint guard does not gate them — safe actions
// never mint pending approvals.
var KnownSafeActions = map[string]struct{}{
	"add_ready_label":       {},
	"remove_ready_label":    {},
	"remove_blocked_label":  {},
	"add_issue_comment":     {},
}

// IsKnownApprovalAction returns true if the verb is implemented by the
// executor (or explicitly handled with awaiting_dispatch / execution_skipped).
// Supervisor and HTTP enqueue paths MUST gate on this before persisting a
// pending approval — otherwise the silent execution_failed loop returns.
func IsKnownApprovalAction(verb string) bool {
	_, ok := KnownApprovalActions[verb]
	return ok
}

// IsKnownSafeAction returns true if the verb is an approved safe-action
// (label/comment) the supervisor or HTTP endpoint can dispatch directly
// without going through the approval queue.
func IsKnownSafeAction(verb string) bool {
	_, ok := KnownSafeActions[verb]
	return ok
}

// KnownApprovalActionList returns the sorted slice form of
// KnownApprovalActions for diagnostics (e.g. error messages that name
// the supported verbs).
func KnownApprovalActionList() []string {
	out := make([]string, 0, len(KnownApprovalActions))
	for k := range KnownApprovalActions {
		out = append(out, k)
	}
	// stable order — sort.Strings would pull in sort; tiny manual one is fine.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
