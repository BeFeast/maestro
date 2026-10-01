package review

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/google/uuid"
)

// RearmProofPreLaunchHold selects the pre-launch proof for an operator rearm:
// the previous attempt's native consultation receipt shows a typed hold before
// any auxiliary launch (zero invocations, no launch or registration intent), so
// no physical request can have been made. The default (empty) kind is the
// native-outcome proof supervisor.NativeReviewRearmProof computes (#1233).
const RearmProofPreLaunchHold = "pre_launch_hold"

var errNativeReviewReceiptAbsent = errors.New("native review receipt absent")

// readNativeReviewReceipt mirrors the supervisor consultation store's private
// read discipline: refuse symlinked or group/world-writable roots and files so
// an unreadable or tampered store is never interpreted as "nothing launched".
func readNativeReviewReceipt(nativeDir, name string) ([]byte, error) {
	for _, p := range []string{nativeDir, filepath.Join(nativeDir, "supervisor-consultations")} {
		info, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || info.Mode()&0022 != 0 || !ok || st.Uid != uint32(os.Geteuid()) {
			return nil, aiexecution.Held("native_review_receipt_root_invalid")
		}
	}
	path := filepath.Join(nativeDir, "supervisor-consultations", name)
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	st, ok := before.Sys().(*syscall.Stat_t)
	const maxReceipt = 2 << 20
	if !before.Mode().IsRegular() || before.Mode()&0022 != 0 || !ok || st.Uid != uint32(os.Geteuid()) || before.Size() > maxReceipt {
		return nil, aiexecution.Held("native_review_receipt_invalid")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		return nil, aiexecution.Held("native_review_receipt_changed")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxReceipt+1))
	if err != nil || len(b) > maxReceipt {
		return nil, aiexecution.Held("native_review_receipt_unavailable")
	}
	return b, nil
}

// nativeReviewIntentFor reports whether a launch or registration marker in the
// native review store belongs to attemptID. Either marker means a launch was at
// least attempted, so the attempt can never be proven pre-launch.
func nativeReviewIntentFor(nativeDir, attemptID string) (bool, error) {
	launch, err := readNativeReviewReceipt(nativeDir, "launch.json")
	if err == nil {
		var marker struct {
			Identity supervisor.ConsultationIdentity `json:"identity"`
			IntentID string                          `json:"intent_id"`
		}
		if aiexecution.DecodeStrict(launch, &marker) != nil || uuid.Validate(marker.Identity.ID) != nil {
			return false, aiexecution.Held("native_launch_identity_conflict")
		}
		if marker.Identity.ID == attemptID {
			return true, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	registration, err := readNativeReviewReceipt(nativeDir, "registration.json")
	if err == nil {
		var marker struct {
			ConsultationID string          `json:"consultation_id"`
			Registration   json.RawMessage `json:"registration"`
		}
		if aiexecution.DecodeStrict(registration, &marker) != nil || uuid.Validate(marker.ConsultationID) != nil {
			return false, aiexecution.Held("native_registration_identity_conflict")
		}
		if marker.ConsultationID == attemptID {
			return true, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return false, nil
}

// readNativeReviewConsultation returns the durable consultation receipt for
// attemptID (current or archived). errNativeReviewReceiptAbsent means the store
// is readable and holds no receipt for the identity.
func readNativeReviewConsultation(nativeDir, attemptID string) (*supervisor.ConsultationReceipt, error) {
	for _, name := range []string{"current.json", attemptID + ".json"} {
		data, err := readNativeReviewReceipt(nativeDir, name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var receipt supervisor.ConsultationReceipt
		if aiexecution.DecodeStrict(data, &receipt) != nil || receipt.SchemaVersion != 1 || uuid.Validate(receipt.Identity.ID) != nil {
			return nil, aiexecution.Held("native_receipt_invalid")
		}
		if receipt.Identity.ID == attemptID {
			return &receipt, nil
		}
	}
	return nil, errNativeReviewReceiptAbsent
}

// nativeReviewReceiptPreLaunch reports whether the receipt proves a reviewer
// consultation that failed before any native launch: closed with status failed,
// zero invocations, no planned invocation and no surviving launch/registration
// intent. Anything else is treated as a possible launch.
func nativeReviewReceiptPreLaunch(r *supervisor.ConsultationReceipt, attemptID, projectID string) bool {
	if r == nil || r.Identity.Role != "reviewer" || r.Identity.ID != attemptID || r.Identity.CycleID != attemptID || r.Identity.ProjectID != projectID {
		return false
	}
	if r.Status != "failed" || r.EndedAt == nil || r.NativeOutcomeComplete || r.PlannedInvocation != nil || len(r.Invocations) != 0 {
		return false
	}
	for _, c := range r.Candidates {
		if c.Status == "launch_intent" || c.Status == "registration_intent" || c.IntentID != "" {
			return false
		}
	}
	return true
}

// nativeReviewPreLaunchHoldProven reports whether attemptID provably never
// reached a native launch. A missing receipt counts only when the store itself
// is readable (or absent): the native runner persists a receipt before it
// reserves auxiliary capacity or launches, so no receipt means no launch. Any
// read error, marker or recorded invocation fails closed as "possibly launched".
func nativeReviewPreLaunchHoldProven(nativeDir, attemptID, projectID string) bool {
	if uuid.Validate(attemptID) != nil || projectID == "" {
		return false
	}
	if _, err := os.Lstat(nativeDir); errors.Is(err, os.ErrNotExist) {
		return true
	}
	if _, err := os.Lstat(filepath.Join(nativeDir, "supervisor-consultations")); errors.Is(err, os.ErrNotExist) {
		return true
	}
	if intent, err := nativeReviewIntentFor(nativeDir, attemptID); err != nil || intent {
		return false
	}
	receipt, err := readNativeReviewConsultation(nativeDir, attemptID)
	if errors.Is(err, errNativeReviewReceiptAbsent) {
		return true
	}
	if err != nil {
		return false
	}
	return nativeReviewReceiptPreLaunch(receipt, attemptID, projectID)
}

// NativeReviewPreLaunchHoldProof digests the durable receipt of a reviewer
// consultation that was held before any native launch. It is the operator CAS
// value for OperatorRearmRequest.NativeProofSHA256 with NativeProofKind
// RearmProofPreLaunchHold. It never launches, reseals or deletes anything and
// refuses when the receipt is missing, shows an invocation, or a launch or
// registration intent for the attempt survives.
func NativeReviewPreLaunchHoldProof(nativeDir, attemptID, projectID, runID string) (string, error) {
	if uuid.Validate(attemptID) != nil || projectID == "" || runID == "" {
		return "", aiexecution.Held("native_review_rearm_identity_conflict")
	}
	intent, err := nativeReviewIntentFor(nativeDir, attemptID)
	if err != nil {
		return "", err
	}
	if intent {
		return "", aiexecution.Held("native_review_prelaunch_unverified")
	}
	receipt, err := readNativeReviewConsultation(nativeDir, attemptID)
	if err != nil {
		return "", err
	}
	if !nativeReviewReceiptPreLaunch(receipt, attemptID, projectID) {
		return "", aiexecution.Held("native_review_prelaunch_unverified")
	}
	b, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
