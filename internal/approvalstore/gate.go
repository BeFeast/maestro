package approvalstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// Mode selects which store arbitrates the approve/reject claim.
type Mode string

const (
	// ModeJSON keeps the legacy per-project JSON read-merge-write path for
	// generic approvals. deploy_project is always claimed in configured SQLite
	// because its executor requires a durable approved→executing transition.
	ModeJSON Mode = "json"
	// ModeSQLite routes the claim through the transactional SQLite store
	// (claim-once) while still write-through-mirroring into JSON state for
	// compatibility during the transition.
	ModeSQLite Mode = "sqlite"
)

// ParseMode normalizes the --approvals-store flag value. Empty defaults to
// json (the safe default until the SQLite path is flipped on for a project).
func ParseMode(s string) (Mode, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", string(ModeJSON):
		return ModeJSON, nil
	case string(ModeSQLite):
		return ModeSQLite, nil
	default:
		return "", fmt.Errorf("unknown approvals store %q (want json|sqlite)", s)
	}
}

// Binding is everything ApplyDecision/FinalizeExecution need to resolve the
// approvals store for one project's approve/reject. StateDir is always the
// JSON state directory (mint source and write-through target); DBPath is the
// SQLite database used by generic approvals in ModeSQLite and by
// deploy_project in every mode.
//
// Handle is an optional already-open store. A long-lived server (the fleet /
// single-project HTTP server) MUST set Handle to one shared *Store: with a
// single pooled connection (MaxOpenConns(1)) every in-process claim serializes
// through that connection, so concurrent dashboard approves never collide on
// SQLITE_BUSY. When Handle is nil (the one-shot CLI path) the store is opened
// per call from DBPath.
type Binding struct {
	Mode     Mode
	DBPath   string
	StateDir string
	Repo     string
	Project  string
	Handle   *Store
}

// UseSQLite reports whether the SQLite claim path is selected.
func (b Binding) UseSQLite() bool { return b.Mode == ModeSQLite }

// resolveStore returns the store to use plus a cleanup func. When Handle is
// set it is reused (cleanup is a no-op); otherwise a fresh store is opened from
// DBPath and the cleanup closes it.
func (b Binding) resolveStore() (*Store, func(), error) {
	if b.Handle != nil {
		return b.Handle, func() {}, nil
	}
	store, err := Open(b.DBPath)
	if err != nil {
		return nil, func() {}, err
	}
	return store, func() { store.Close() }, nil
}

// ApplyDecision performs verb ("approve"|"reject") on approval id and returns
// the in-memory JSON *state.State to persist plus the updated *state.Approval.
// The caller owns state.Save (and, for the CLI, executing the approved
// side effect + FinalizeExecution).
//
// In ModeJSON generic approvals use st.ApproveApproval / st.RejectApproval —
// the returned state is mutated identically to the legacy path. Delivery is
// the deliberate exception: deploy_project always uses the configured SQLite
// store so a JSON-only approve cannot bypass or strand its durable execution
// claim.
//
// In ModeSQLite the pending approval is seeded into SQLite (write-through),
// claimed atomically there (claim-once across processes), and — on a winning
// claim — mirrored back into JSON state so the executor loop, which reads
// JSON via ListApprovedApprovals, still drives execution. A losing claim
// returns state.ErrApprovalNotPending ("already processed") without touching
// JSON, so exactly one caller proceeds to execute.
func ApplyDecision(b Binding, verb, id string, now time.Time, actor, reason string) (*state.State, *state.Approval, error) {
	verb = strings.TrimSpace(verb)
	if verb != "approve" && verb != "reject" {
		return nil, nil, fmt.Errorf("unknown approval verb %q", verb)
	}
	st, err := state.Load(b.StateDir)
	if err != nil {
		return nil, nil, err
	}
	a0, ok := st.FindApproval(id)
	if !ok {
		return st, nil, state.ErrApprovalNotFound
	}
	// deploy_project always uses the durable SQLite claim, irrespective of the
	// generic approvals-store mode. The delivery approval is already minted in
	// the configured DB so a JSON-only approve would otherwise leave SQLite
	// pending forever, and the delivery executor could never take its
	// approved→executing exactly-once claim.
	deliveryClaim := a0.Action == state.ApprovalActionDeployProject && a0.Delivery != nil
	if !b.UseSQLite() && !deliveryClaim {
		approval, terr := applyJSONTransition(st, verb, id, now, actor, reason)
		return st, approval, terr
	}

	// SQLite claim-once path.
	if strings.TrimSpace(b.DBPath) == "" && b.Handle == nil {
		b.DBPath = DefaultDBPath()
	}
	store, cleanup, err := b.resolveStore()
	if err != nil {
		return st, nil, err
	}
	defer cleanup()

	ctx := context.Background()
	rb := RowBinding{Project: b.Project, Repo: b.Repo, StateDir: b.StateDir}
	if deliveryClaim {
		// Delivery approvals are minted into their authoritative ledger by the
		// orchestrator. Never seed one from the JSON mirror during approval: a
		// CLI/server pointed at the wrong SQLite file would otherwise create a
		// second claimable row and execute the same revision twice. Requiring the
		// row to exist also makes the configured DB identity fail closed.
		authoritative, getErr := store.Get(ctx, b.StateDir, a0.ID)
		if getErr != nil {
			return st, nil, fmt.Errorf("delivery approval %s is absent from the configured authoritative ledger: %w", a0.ID, getErr)
		}
		if authoritative.Action != state.ApprovalActionDeployProject || authoritative.Delivery == nil {
			return st, nil, fmt.Errorf("configured ledger row %s is not a delivery approval", a0.ID)
		}
		a0 = replaceApproval(st, authoritative)
	} else {
		if _, err := store.Put(ctx, a0, rb); err != nil {
			return st, nil, err
		}
	}

	// Claim the RESOLVED approval id (a0.ID), not the user-supplied value:
	// FindApproval accepts either Approval.ID or DecisionID, but the row is
	// keyed under a0.ID, so claiming the original `id` (a decision-id alias)
	// would miss the row and spuriously return ErrApprovalNotFound. Scope the
	// claim to b.StateDir so a shared maestro.db never cross-claims another
	// project's same-id approval.
	var claimed *state.Approval
	switch verb {
	case "approve":
		claimed, err = store.Approve(ctx, b.StateDir, a0.ID, now, actor, reason)
	case "reject":
		claimed, err = store.Reject(ctx, b.StateDir, a0.ID, now, actor, reason)
	}
	if err != nil {
		if deliveryClaim && claimed != nil {
			return st, replaceApproval(st, claimed), err
		}
		// Mirror a stale / payload-mismatch transition into JSON so both
		// stores agree and the caller's partial-persist path records it.
		if errors.Is(err, state.ErrApprovalStale) || errors.Is(err, state.ErrApprovalPayloadMismatch) {
			_, _ = applyJSONTransition(st, verb, id, now, actor, reason)
		}
		return st, claimed, err
	}
	if deliveryClaim {
		return st, replaceApproval(st, claimed), nil
	}

	// Won the claim — write through to JSON. In the normal same-load case the
	// JSON record is still pending and the transition succeeds. If a rare
	// cross-process drift left JSON already at the claimed status, accept it;
	// otherwise the SQLite claim remains authoritative.
	jApproval, jErr := applyJSONTransition(st, verb, id, now, actor, reason)
	if jErr == nil {
		return st, jApproval, nil
	}
	if cur, ok := st.FindApproval(id); ok && cur.Status == claimed.Status {
		return st, cur, nil
	}
	return st, claimed, nil
}

func replaceApproval(st *state.State, authoritative *state.Approval) *state.Approval {
	if st == nil || authoritative == nil {
		return authoritative
	}
	for i := range st.Approvals {
		if st.Approvals[i].ID == authoritative.ID {
			st.Approvals[i] = *authoritative
			return &st.Approvals[i]
		}
	}
	st.Approvals = append(st.Approvals, *authoritative)
	return &st.Approvals[len(st.Approvals)-1]
}

// FinalizeExecution mirrors a post-execution terminal status into the SQLite
// store after the caller has already recorded it in JSON state. It is a no-op
// in ModeJSON. The idempotent not-approved / not-found cases are tolerated so
// a concurrent finalize cannot turn into a spurious error.
func FinalizeExecution(b Binding, id string, status state.ApprovalStatus, now time.Time, actor, summary string) error {
	if !b.UseSQLite() {
		return nil
	}
	store, cleanup, err := b.resolveStore()
	if err != nil {
		return err
	}
	defer cleanup()
	ctx := context.Background()
	switch status {
	case state.ApprovalStatusExecuted:
		_, err = store.MarkExecuted(ctx, b.StateDir, id, now, actor, summary)
	case state.ApprovalStatusExecutionFailed:
		_, err = store.MarkExecutionFailed(ctx, b.StateDir, id, now, actor, summary)
	case state.ApprovalStatusExecutionSkipped:
		_, err = store.MarkExecutionSkipped(ctx, b.StateDir, id, now, actor, summary)
	case state.ApprovalStatusAwaitingDispatch:
		_, err = store.MarkAwaitingDispatch(ctx, b.StateDir, id, now, actor, summary)
	default:
		return fmt.Errorf("approvalstore: unsupported finalize status %q", status)
	}
	if errors.Is(err, state.ErrApprovalNotApproved) || errors.Is(err, state.ErrApprovalNotFound) {
		return nil
	}
	return err
}

// ReconcileMoot mirrors a moot-approval stale transition into the SQLite store
// after the caller has already recorded it in JSON state (#866). It is a no-op
// in ModeJSON. A missing row — the common case for an approval that was never
// approved, so was never seeded into SQLite — is tolerated (there is nothing to
// reconcile), as is a row already in a terminal status, so a repeated reconcile
// across cycles, restarts, and concurrent saves never turns into a spurious
// error. Only the JSON state is authoritative for the fleet read path; this
// keeps the claim arbiter from diverging.
func ReconcileMoot(b Binding, id string, now time.Time, reason string) error {
	if !b.UseSQLite() {
		return nil
	}
	store, cleanup, err := b.resolveStore()
	if err != nil {
		return err
	}
	defer cleanup()
	_, err = store.MarkStale(context.Background(), b.StateDir, id, now, reason)
	if errors.Is(err, state.ErrApprovalNotFound) {
		return nil
	}
	return err
}

func applyJSONTransition(st *state.State, verb, id string, now time.Time, actor, reason string) (*state.Approval, error) {
	switch verb {
	case "approve":
		return st.ApproveApproval(id, now, actor, reason)
	case "reject":
		return st.RejectApproval(id, now, actor, reason)
	default:
		return nil, fmt.Errorf("unknown approval verb %q", verb)
	}
}
