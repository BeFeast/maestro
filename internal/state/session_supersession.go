package state

// SupersedingSession returns the newest durable session that makes candidate a
// historical predecessor for operator-attention purposes. It is deliberately
// read-only: callers use the result to filter current attention and supervisor
// findings while preserving every session row, worktree, usage counter, and
// attribution record.
//
// A terminal failure can be superseded through either durable lifecycle link:
//   - a later session for the same issue; or
//   - a later session for the same PR, including a continuation issue.
//
// One issue has one current lifecycle: an earlier terminal attempt is history
// even when it owns a PR of its own. Review asked for the opposite — that an
// attempt with its own distinct PR always stay actionable — and that was
// declined deliberately (operator decision 2026-07-25): with an issue that has
// already shipped through a later PR, every abandoned attempt would keep
// emitting "reconcile the closed PR" findings forever. Suppression here is
// display truth only; the PR itself is untouched and every session row remains.
//
// Ordering compares SessionChangedAt — the newest durable lifecycle timestamp
// (started / finished / issue-closed / last output) — not StartedAt. Start
// order alone is not lifecycle order: a peer can start after the candidate and
// still finish before the candidate's own terminal transition, in which case
// the candidate holds the newest durable outcome and must stay actionable.
// An unknown or tied timestamp never proves that a peer is later. The newest
// qualifying peer wins, with the slot name used only as a deterministic tie
// breaker between peers that share the same timestamp.
func (s *State) SupersedingSession(candidate *Session) (string, *Session, bool) {
	if s == nil || candidate == nil || candidate.IssueNumber <= 0 || candidate.StartedAt.IsZero() || !sessionStatusCanBeSuperseded(candidate.Status) {
		return "", nil, false
	}
	candidateAt := SessionChangedAt(candidate)
	if candidateAt.IsZero() {
		return "", nil, false
	}

	var selectedSlot string
	var selected *Session
	var selectedAt = candidateAt
	for slot, peer := range s.Sessions {
		if peer == nil || peer == candidate || peer.StartedAt.IsZero() || peer.ReleasedForRedispatch {
			continue
		}
		sameIssue := peer.IssueNumber == candidate.IssueNumber
		sharedPR := candidate.PRNumber > 0 && peer.PRNumber == candidate.PRNumber
		if !sameIssue && !sharedPR {
			continue
		}
		if !sessionStatusCanSupersede(peer.Status) {
			continue
		}
		peerAt := SessionChangedAt(peer)
		if peerAt.IsZero() || !peerAt.After(candidateAt) {
			continue
		}
		if selected == nil || peerAt.After(selectedAt) || (peerAt.Equal(selectedAt) && slot < selectedSlot) {
			selectedSlot = slot
			selected = peer
			selectedAt = peerAt
		}
	}
	if selected == nil {
		return "", nil, false
	}
	return selectedSlot, selected, true
}

func sessionStatusCanBeSuperseded(status SessionStatus) bool {
	switch status {
	case StatusDead, StatusFailed, StatusConflictFailed, StatusRetryExhausted:
		return true
	default:
		return false
	}
}

// sessionStatusCanSupersede lists the statuses an authoritative later session
// may hold. StatusQueued is included: a replacement already waiting to run is
// the current lifecycle for that issue, and leaving it out kept an exhausted
// predecessor actionable while its successor was queued.
func sessionStatusCanSupersede(status SessionStatus) bool {
	switch status {
	case StatusRunning, StatusQueued, StatusPROpen, StatusCodeLanded, StatusDone,
		StatusDead, StatusFailed, StatusConflictFailed, StatusRetryExhausted:
		return true
	default:
		return false
	}
}
