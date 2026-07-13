package approver

// Phase 1.5 / #505: approval-action registry — the authoritative list of
// verbs the executor knows how to handle. The supervisor and any other
// approval-mint surface MUST consult this registry before persisting a
// pending approval; minting a verb the executor does not implement leads
// to the silent execution_failed loop seen on dogfood 2026-05-30 (an
// earlier shape of `spawn_repair_worker` piled up 4 execution_failed
// records before an operator noticed; #662 then resurfaced the issue on
// cautious projects where the verb was still emitted but the executor
// had no case — fix lands `spawn_repair_worker` as awaiting_dispatch).
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
	"close_issue_batch":     {},
	"delete_worktree":       {},
	"change_global_config":  {}, // executor returns execution_skipped pending YAML pipeline
	"apply_lesson_proposal": {},
	"spawn_worker":          {}, // executor returns awaiting_dispatch (#515)
	"open_child_issue":      {}, // executor returns awaiting_dispatch (#515)
	// #565: auto review-repair respawn. Executor returns awaiting_dispatch
	// — the orchestrator's dispatcher spawns a scoped opus repair worker
	// keyed on (pr_number, head_sha) so the same head is never
	// re-attempted. Covers the green+retry_exhausted+P0/P1-on-head case;
	// orthogonal to `spawn_repair_worker` below, which covers the
	// not-progressing / retry-exhausted-without-PR cases.
	"spawn_review_repair": {},
	// #662: classic repair worker. Executor returns awaiting_dispatch —
	// the orchestrator's dispatcher loop owns the actual respawn (see
	// supervisorSelectedRepairSpawn in internal/orchestrator). Before
	// #662 the supervisor emitted this verb deterministically (open PR
	// not progressing, retry-exhausted with no usable PR) but the
	// executor had no case, so cautious projects refused to mint the
	// approval every cycle and the issue stalled silently.
	"spawn_repair_worker": {},
	// #567: per-session worker-control verbs surfaced by the fleet
	// Mission Control snapshot. Executor calls into the WorkerController
	// to kill the tmux session + (for restart) flag the slot for the
	// next dispatcher respawn.
	"restart_worker": {},
	"stop_worker":    {},
	// #851: apply a groomed issue-body rewrite. The spec-groom step posts
	// the proposed rewrite as a comment and mints this approval carrying the
	// new body on Target.Body; executeEditIssueBody calls gh.EditIssueBody on
	// approve, and a reject leaves the issue untouched. add_issue_comment (the
	// safe verb used to post the proposal itself) stays OUT of this registry.
	"edit_issue_body": {},
	// #736: ready-label handoff fallback. The common path applies the
	// add_ready_label mutation directly as an operator-whitelisted safe
	// action (safe_actions), so it never reaches this registry. But when
	// the operator gates the verb behind approval_required instead, the
	// supervisor mints this approval and the executor applies the
	// configured ready label on approve (executeLabelIssueReady). Before
	// #736 the verb was absent here, so the at-mint guard refused it every
	// cycle and a cautious+LLM project's dynamic-wave handoff stalled
	// silently — the ready label was never applied and the orchestrator
	// starved. Note: the safe-action alias "add_ready_label" stays OUT of
	// this registry (it is a safe action, never an approval mint).
	"label_issue_ready": {},
	// #872: approval-gated post-merge delivery. Unlike the other verbs this one
	// is NOT dispatched by Executor.Execute — it needs the durable
	// approved→executing store claim before its side effect, so it runs through
	// the dedicated DeliveryExecutor (delivery.go). It is registered here so the
	// at-mint guard accepts the verb; dispatchAction routes it to
	// execution_skipped with a clear "use the delivery executor" summary rather
	// than the unknown-action failure.
	"deploy_project": {},
}

// KnownSafeActions is the canonical set of safe (non-approval-gated)
// verbs the supervisor / HTTP endpoint accepts. They are dispatched by
// internal/server/actions, NOT by executor.Execute, so they live here
// for symmetry but the at-mint guard does not gate them — safe actions
// never mint pending approvals.
var KnownSafeActions = map[string]struct{}{
	"add_ready_label":      {},
	"remove_ready_label":   {},
	"add_blocked_label":    {},
	"remove_blocked_label": {},
	"add_issue_comment":    {},
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
