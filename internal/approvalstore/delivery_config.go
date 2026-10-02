package approvalstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/state"
)

// InvalidateDeliveryConfig fences config drift before freshness can fetch from
// a changed forge. Only pending/approved rows become stale. Executing leases
// and terminal audit records are returned unchanged, including old digests.
// The execution claim independently repeats its digest check in its own tx.
func (s *Store) InvalidateDeliveryConfig(ctx context.Context, stateDir, id, expectedDigest string, now time.Time) (*state.Approval, error) {
	if strings.TrimSpace(expectedDigest) == "" {
		return nil, ErrDeliveryConfigMismatch
	}
	canonical, err := canonicalStateDir(stateDir)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	a, b, err := loadApprovalTx(ctx, tx, canonical, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, state.ErrApprovalNotFound
	}
	if err != nil {
		return nil, err
	}
	if a.Action != state.ApprovalActionDeployProject || a.Delivery == nil {
		return a, ErrDeliveryConfigMismatch
	}
	if strings.TrimSpace(a.Delivery.ConfigDigest) == strings.TrimSpace(expectedDigest) {
		return a, nil
	}
	if a.Status != state.ApprovalStatusPending && a.Status != state.ApprovalStatusApproved {
		return a, ErrDeliveryConfigMismatch
	}
	a.Delivery.StaleCause = state.DeliveryStaleCauseConfigDrift
	if err := markStaleTx(ctx, tx, a, b, now, "delivery config changed after approval; fresh approval required"); err != nil {
		return a, err
	}
	if err := tx.Commit(); err != nil {
		return a, err
	}
	return a, ErrDeliveryConfigMismatch
}
