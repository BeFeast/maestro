package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/befeast/maestro/internal/aiexecution"
)

// NativeReviewRearmProof validates a failed review's exact durable OS and normal
// financial outcomes. It never reseals, launches, or infers zero dispatch from
// a local error. The digest pins all original receipt evidence for operator CAS.
func NativeReviewRearmProof(stateDir, attemptID, projectID, runID string) (string, error) {
	if err := NativeAuxiliaryOutcomeComplete(stateDir, attemptID); err != nil {
		return "", err
	}
	r, _, err := readNativeConsultation(stateDir, attemptID)
	if err != nil {
		return "", err
	}
	if r.Identity.Role != "reviewer" || r.Identity.ID != attemptID || r.Identity.CycleID != attemptID || r.Identity.ProjectID != projectID || r.Status != "failed" || runID == "" || len(r.Invocations) == 0 {
		return "", aiexecution.Held("native_review_rearm_identity_conflict")
	}
	for _, inv := range r.Invocations {
		if inv.ProcessLease == nil || inv.ProcessTermination == nil || !inv.ProcessTerminationVerified || inv.NativeSession == nil || inv.NativeSession.Outcome == nil || inv.NativeSession.Request.RunID != runID || inv.NativeSession.Outcome.OperatorRetirement != nil || inv.NativeSession.Outcome.UnresolvedAttempts != 0 {
			return "", aiexecution.Held("native_review_rearm_outcome_unverified")
		}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
