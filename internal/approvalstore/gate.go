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
	// ModeJSON keeps the legacy per-project JSON read-merge-write path.
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
// SQLite database used only in ModeSQLite.
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
// In ModeJSON this is exactly st.ApproveApproval / st.RejectApproval — the
// returned state is mutated identically to the legacy path (including the
// partial stale transition on a conflict), so the caller's existing save +
// error-mapping logic is unchanged.
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
	if !b.UseSQLite() {
		approval, terr := applyJSONTransition(st, verb, id, now, actor, reason)
		return st, approval, terr
	}

	// SQLite claim-once path.
	a0, ok := st.FindApproval(id)
	if !ok {
		return st, nil, state.ErrApprovalNotFound
	}
	store, cleanup, err := b.resolveStore()
	if err != nil {
		return st, nil, err
	}
	defer cleanup()

	ctx := context.Background()
	rb := RowBinding{Project: b.Project, Repo: b.Repo, StateDir: b.StateDir}
	if _, err := store.Put(ctx, a0, rb); err != nil {
		return st, nil, err
	}

	var claimed *state.Approval
	switch verb {
	case "approve":
		claimed, err = store.Approve(ctx, id, now, actor, reason)
	case "reject":
		claimed, err = store.Reject(ctx, id, now, actor, reason)
	}
	if err != nil {
		// Mirror a stale / payload-mismatch transition into JSON so both
		// stores agree and the caller's partial-persist path records it.
		if errors.Is(err, state.ErrApprovalStale) || errors.Is(err, state.ErrApprovalPayloadMismatch) {
			_, _ = applyJSONTransition(st, verb, id, now, actor, reason)
		}
		return st, claimed, err
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
		_, err = store.MarkExecuted(ctx, id, now, actor, summary)
	case state.ApprovalStatusExecutionFailed:
		_, err = store.MarkExecutionFailed(ctx, id, now, actor, summary)
	case state.ApprovalStatusExecutionSkipped:
		_, err = store.MarkExecutionSkipped(ctx, id, now, actor, summary)
	case state.ApprovalStatusAwaitingDispatch:
		_, err = store.MarkAwaitingDispatch(ctx, id, now, actor, summary)
	default:
		return fmt.Errorf("approvalstore: unsupported finalize status %q", status)
	}
	if errors.Is(err, state.ErrApprovalNotApproved) || errors.Is(err, state.ErrApprovalNotFound) {
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
