package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/befeast/maestro/internal/admissioncontrol"
	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

// A first-generation start registers its native session at the admission
// authority before the rest of setup runs (worktree, execution proof, gateway
// binding observation, runner script, before_run hook). A setup failure after
// that point leaves the slot failed under a NativeRegistrationHold with a
// registered generation-1 receipt that never reached launch intent. Any
// non-empty hold retains the issue claim, and the only release is the explicit
// operator pre-launch recovery, which requires a registration that is still
// valid. Once the registration expires (its TTL is shorter than many outages)
// or the authority revokes it, nothing can ever launch it, so without this
// reconciliation the issue stays claimed until someone edits state by hand.
//
// ReconcileExpiredNativePrelaunch retires such a registration through the same
// abandoned-launch protocol as an abandoned successor launch: full absence
// proof, a durable seal intent, an authority seal that must confirm zero
// physical attempts, and an archived receipt. Only then is the hold cleared and
// the failed session released for redispatch, so ordinary selection starts the
// issue again under a fresh slot, generation and registration.

// nativePrelaunchExpiryClock is the wall clock compared against the
// registration expiry; tests pin it.
var nativePrelaunchExpiryClock = time.Now

// NativePrelaunchExpiryCandidate reports whether sess has the projection of a
// failed first-generation start held before launch: failed, held, and no native
// generation was ever stamped on it. It is a cheap routing filter only;
// ReconcileExpiredNativePrelaunch re-validates the whole projection and the
// receipt under the receipt lock.
func NativePrelaunchExpiryCandidate(sess *state.Session) bool {
	return sess != nil && sess.Status == state.StatusFailed && sess.NativeRegistrationHold != "" && !sess.ReleasedForRedispatch &&
		sess.WorkerGeneration == 0 && sess.NativeRoleRunID == "" && sess.NativeSessionID == ""
}

// ReconcileExpiredNativePrelaunch reconciles one failed first-generation slot
// whose generation-1 registration is registered (never launch_intent or
// launched), has no PID, log file, outcome, evidence or terminal marker, and
// has expired or been revoked at the authority. It reports whether the slot was
// released.
//
// Untouched, with a nil error: sessions outside the candidate projection, slots
// without a receipt (a hold raised before registration), receipts that are not
// registered (registration_intent never received an acknowledgement to seal;
// launch_intent and launched are uncertain launches that keep their existing
// fencing) and registrations that are still valid (explicit pre-launch
// recovery handles those).
//
// A typed hold error is returned, with the session hold and receipt left in
// place, when the projection or receipt identity conflicts, a never-launched
// proof is missing, local absence is unproven, or the authority cannot settle
// the registration with zero physical attempts. The next cycle repeats the
// same sequence; the persisted seal intent makes the authority call
// idempotent.
//
// A crash after the receipt is archived but before the released projection is
// saved is replayed from the exact archived record. No process is launched or
// signalled.
func ReconcileExpiredNativePrelaunch(cfg *config.Config, s *state.State, slot string) (bool, error) {
	if cfg == nil || s == nil || !cfg.AIExecution.RequireVerifiedRoute || cfg.WorkerNativeSessionRegistration == nil {
		return false, nil
	}
	sess := s.Sessions[slot]
	if !NativePrelaunchExpiryCandidate(sess) {
		return false, nil
	}
	if sess.NativeParentRoleRunID != "" || sess.NativeReceiptDir != "" || sess.PID != 0 || sess.TmuxSession != "" ||
		sess.ProcessLeaseUnit != "" || sess.ProcessLeaseManager != "" || !sess.StartedAt.IsZero() || sess.LogFile != "" ||
		sess.PRNumber != 0 || sess.Branch == "" || sess.Worktree != filepath.Join(cfg.WorktreeBase, slot) {
		return false, &NativeRegistrationHold{Code: "native_recovery_projection_conflict", Slot: slot}
	}
	// A hold raised before any registration never created the receipt
	// directory; do not create one (lockNativeWorker would) for it.
	if state.ValidateSlotID(slot) != nil {
		return false, &NativeRegistrationHold{Code: "identity_invalid", Slot: slot}
	}
	if _, err := os.Lstat(nativeReceiptDir(cfg.StateDir, slot)); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	dir, unlock, err := lockNativeWorker(cfg, slot)
	if err != nil {
		return false, err
	}
	defer unlock()
	hold := sess.NativeRegistrationHold
	r, err := readNativeWorkerReceipt(dir, 1)
	if errors.Is(err, os.ErrNotExist) {
		recorded, err := nativePrelaunchAbandonmentRecorded(cfg, dir, slot, sess)
		if err != nil || !recorded {
			return false, err
		}
		log.Printf("[worker] native prelaunch abandonment replayed for %s: generation 1 was already sealed and archived; hold %s cleared; issue #%d released for redispatch", slot, hold, sess.IssueNumber)
		releaseAbandonedNativePrelaunch(sess)
		return true, state.Save(cfg.StateDir, s)
	}
	if err != nil {
		return false, err
	}
	if r.Status != "registered" {
		return false, nil
	}
	if r.ProjectID != cfg.ProjectID || r.Slot != slot || r.Generation != 1 || r.IssueNumber != sess.IssueNumber ||
		r.Worktree != sess.Worktree || r.Branch != sess.Branch || r.ParentRoleRunID != "" || r.Acknowledgement == nil {
		return false, &NativeRegistrationHold{Code: "native_identity_conflict", Slot: slot}
	}
	if r.PID != 0 || r.LogFile != "" || r.OutcomeIntent != nil || r.Outcome != nil || r.NativeProcessEvidence != nil || r.OperatorRecovery != nil {
		return false, &NativeRegistrationHold{Code: "native_launch_abandonment_unproven", Slot: slot}
	}
	reason, err := nativePrelaunchRegistrationRetired(cfg, r)
	if err != nil || reason == "" {
		return false, err
	}
	prefix, err := abandonNeverLaunchedNativeRegistrationLocked(cfg, dir, slot, r, false, hold)
	if err != nil {
		return false, err
	}
	log.Printf("[worker] native prelaunch registration abandoned for %s: generation 1 (registered, %s) sealed with zero attempts and archived as %s; hold %s cleared; issue #%d released for redispatch", slot, reason, prefix, hold, sess.IssueNumber)
	releaseAbandonedNativePrelaunch(sess)
	return true, state.Save(cfg.StateDir, s)
}

// nativePrelaunchRegistrationRetired reports why the registration of r can no
// longer authorize a launch: "expired" by the local clock (beginLaunch refuses
// it from then on), or "revoked" / "expired_at_authority" when replaying the
// exact persisted registration request (idempotent, the same call explicit
// recovery makes) is refused by the authority for that reason. "" means the
// registration is still valid or its status is unknown, and the slot is left
// to explicit recovery.
func nativePrelaunchRegistrationRetired(cfg *config.Config, r *NativeWorkerReceipt) (string, error) {
	if r.Request.ExpiresAt <= nativePrelaunchExpiryClock().Unix() {
		return "expired", nil
	}
	client, err := nativeClient(cfg)
	if err != nil {
		return "", err
	}
	_, err = registerNativeWorker(client, r.Request)
	var hold *admissioncontrol.Hold
	if errors.As(err, &hold) {
		switch hold.Code {
		case "registration_revoked":
			return "revoked", nil
		case "registration_expired":
			return "expired_at_authority", nil
		}
	}
	return "", nil
}

// releaseAbandonedNativePrelaunch clears the hold and releases the issue claim
// of a failed slot whose only registration was retired with zero attempts. The
// session stays as the audit record of the attempt; its outcome keeps it out of
// the per-issue failed-attempt budget, and the retained worktree no longer
// claims the issue.
func releaseAbandonedNativePrelaunch(sess *state.Session) {
	sess.NativeRegistrationHold = ""
	sess.ReleasedForRedispatch = true
	sess.WorkerOutcome = state.WorkerOutcomeNativePrelaunchAbandoned
	if sess.FinishedAt == nil {
		now := time.Now().UTC()
		sess.FinishedAt = &now
	}
}

// nativePrelaunchAbandonmentRecorded reports whether the generation-1
// registration of the failed slot sess was already sealed and archived by
// ReconcileExpiredNativePrelaunch, i.e. a crash or save failure came between
// the archive and the released projection. The record must name this slot as
// a parentless generation-1 registered receipt abandoned under the session's
// current hold, carry a valid zero-attempt authority seal, and its archived
// receipt must be the record's exact bytes for the session's issue, worktree
// and branch.
func nativePrelaunchAbandonmentRecorded(cfg *config.Config, dir, slot string, sess *state.Session) (bool, error) {
	invalid := &NativeRegistrationHold{Code: "receipt_invalid", LaunchUncertain: true, Slot: slot}
	for index := 1; index <= 64; index++ {
		prefix := nativeLaunchAbandonmentPrefix(1, index)
		if _, err := os.Lstat(filepath.Join(dir, prefix+".receipt.json")); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return false, invalid
		}
		b, err := readOwnedRegularNoFollow(filepath.Join(dir, prefix+".outcome.json"), 64<<10)
		if err != nil {
			return false, invalid
		}
		var record NativeLaunchAbandonmentRecord
		if aiexecution.DecodeStrict(b, &record) != nil || record.SchemaVersion != 1 || record.Generation != 1 ||
			admissioncontrol.ValidateNativeOutcome(record.Outcome, record.OutcomeIntent) != nil || record.Outcome.PhysicalAttempts != 0 {
			return false, invalid
		}
		if record.Slot != slot || record.ParentRoleRunID != "" || record.PreviousStatus != "registered" || record.ClearedHold != sess.NativeRegistrationHold {
			continue
		}
		archived, err := readOwnedRegularNoFollow(filepath.Join(dir, prefix+".receipt.json"), 64<<10)
		if err != nil {
			return false, invalid
		}
		sum := sha256.Sum256(archived)
		var receipt NativeWorkerReceipt
		if hex.EncodeToString(sum[:]) != record.ReceiptSHA256 || aiexecution.DecodeStrict(archived, &receipt) != nil ||
			receipt.Generation != 1 || receipt.Slot != slot || receipt.ProjectID != cfg.ProjectID || receipt.Status != "registered" ||
			receipt.RoleRunID != record.RoleRunID || receipt.Request.NativeSessionID != record.NativeSessionID || receipt.ParentRoleRunID != "" ||
			record.OutcomeIntent.Binding != receipt.Request.Binding {
			return false, invalid
		}
		if receipt.IssueNumber != sess.IssueNumber || receipt.Worktree != sess.Worktree || receipt.Branch != sess.Branch {
			continue
		}
		return true, nil
	}
	return false, nil
}
