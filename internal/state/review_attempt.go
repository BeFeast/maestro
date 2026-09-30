package state

import "time"

// ReviewAttemptTrack owns HTTP review execution for one exact repository/PR/head/lens.
// Only state.Update may mutate it; ordinary Save merges cannot erase newer receipts.
type ReviewAttemptTrack struct {
	Revision    uint64          `json:"revision"`
	Repo        string          `json:"repo"`
	PR          int             `json:"pr"`
	Head        string          `json:"head"`
	Lens        string          `json:"lens"`
	MaxAttempts int             `json:"max_attempts"`
	Attempts    []ReviewAttempt `json:"attempts"`
}

type ReviewAttempt struct {
	ID             string     `json:"id"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	Outcome        string     `json:"outcome"`
	Reason         string     `json:"reason,omitempty"`
	NextAction     string     `json:"next_action,omitempty"`
	RetryAt        *time.Time `json:"retry_at,omitempty"`
	EvidenceFile   string     `json:"evidence_file,omitempty"`
	EvidenceSHA256 string     `json:"evidence_sha256,omitempty"`
}

func mergeReviewAttempts(current, ours map[string]ReviewAttemptTrack) map[string]ReviewAttemptTrack {
	if len(current)+len(ours) == 0 {
		return nil
	}
	merged := make(map[string]ReviewAttemptTrack, len(current)+len(ours))
	for key, track := range current {
		merged[key] = track
	}
	for key, track := range ours {
		previous, found := merged[key]
		if !found || track.Revision > previous.Revision {
			merged[key] = track
		}
	}
	return merged
}
