package state

import (
	"testing"
	"time"
)

func TestSupersedingSessionUsesLaterCurrentIssueLifecycle(t *testing.T) {
	base := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	st := NewState()
	failedA := &Session{IssueNumber: 976, Status: StatusFailed, StartedAt: base.Add(100 * time.Millisecond), TokensUsedTotal: 11}
	failedB := &Session{IssueNumber: 976, Status: StatusDead, StartedAt: base.Add(200 * time.Millisecond), TokensUsedTotal: 22}
	current := &Session{IssueNumber: 976, Status: StatusDone, PRNumber: 1033, StartedAt: base.Add(300 * time.Millisecond), TokensUsedTotal: 33}
	st.Sessions["attempt-a"] = failedA
	st.Sessions["attempt-b"] = failedB
	st.Sessions["current"] = current

	for _, predecessor := range []*Session{failedA, failedB} {
		slot, got, ok := st.SupersedingSession(predecessor)
		if !ok || slot != "current" || got != current {
			t.Fatalf("superseding session = %q, %p, %v; want current %p", slot, got, ok, current)
		}
	}
	if _, _, ok := st.SupersedingSession(current); ok {
		t.Fatal("authoritative current session must not be superseded")
	}

	if len(st.Sessions) != 3 || st.Sessions["attempt-a"].TokensUsedTotal != 11 || st.Sessions["attempt-b"].TokensUsedTotal != 22 {
		t.Fatalf("supersession mutated historical sessions: %#v", st.Sessions)
	}
}

func TestSupersedingSessionKeepsNewestRegressionCurrent(t *testing.T) {
	base := time.Date(2026, 7, 21, 11, 0, 0, 0, time.UTC)
	st := NewState()
	failedA := &Session{IssueNumber: 976, Status: StatusFailed, StartedAt: base}
	failedB := &Session{IssueNumber: 976, Status: StatusDead, StartedAt: base.Add(time.Second)}
	current := &Session{IssueNumber: 976, Status: StatusFailed, PRNumber: 1033, StartedAt: base.Add(2 * time.Second)}
	st.Sessions["attempt-a"] = failedA
	st.Sessions["attempt-b"] = failedB
	st.Sessions["current"] = current

	for _, predecessor := range []*Session{failedA, failedB} {
		slot, got, ok := st.SupersedingSession(predecessor)
		if !ok || slot != "current" || got != current {
			t.Fatalf("regressed current owner = %q, %p, %v; want current %p", slot, got, ok, current)
		}
	}
	if _, _, ok := st.SupersedingSession(current); ok {
		t.Fatal("current regressed session must remain actionable")
	}
}

func TestSupersedingSessionUsesSharedPRContinuation(t *testing.T) {
	base := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	st := NewState()
	source := &Session{IssueNumber: 900, Status: StatusDead, PRNumber: 1033, StartedAt: base}
	continuation := &Session{IssueNumber: 976, Status: StatusPROpen, PRNumber: 1033, StartedAt: base.Add(time.Second)}
	st.Sessions["source"] = source
	st.Sessions["continuation"] = continuation

	slot, got, ok := st.SupersedingSession(source)
	if !ok || slot != "continuation" || got != continuation {
		t.Fatalf("shared-PR superseding session = %q, %p, %v; want continuation %p", slot, got, ok, continuation)
	}

	unrelated := *continuation
	unrelated.PRNumber = 1034
	st.Sessions["continuation"] = &unrelated
	if _, _, ok := st.SupersedingSession(source); ok {
		t.Fatal("different-PR continuation must not supersede source issue")
	}
}

func TestSupersedingSessionRequiresProvenLaterDurableOwner(t *testing.T) {
	started := time.Date(2026, 7, 21, 13, 0, 0, 0, time.UTC)
	st := NewState()
	candidate := &Session{IssueNumber: 976, Status: StatusFailed, StartedAt: started}
	st.Sessions["candidate"] = candidate
	st.Sessions["same-time"] = &Session{IssueNumber: 976, Status: StatusDone, PRNumber: 1033, StartedAt: started}
	st.Sessions["queued"] = &Session{IssueNumber: 976, Status: StatusQueued, StartedAt: started.Add(time.Second)}
	st.Sessions["released"] = &Session{IssueNumber: 976, Status: StatusDone, PRNumber: 1034, StartedAt: started.Add(2 * time.Second), ReleasedForRedispatch: true}

	if _, _, ok := st.SupersedingSession(candidate); ok {
		t.Fatal("same-time, queued, or released peers must not prove supersession")
	}
}

func TestSupersedingSessionSelectionIsDeterministic(t *testing.T) {
	base := time.Date(2026, 7, 21, 14, 0, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		st := NewState()
		candidate := &Session{IssueNumber: 976, Status: StatusFailed, StartedAt: base}
		st.Sessions["candidate"] = candidate
		st.Sessions["later-z"] = &Session{IssueNumber: 976, Status: StatusPROpen, StartedAt: base.Add(time.Second)}
		st.Sessions["later-a"] = &Session{IssueNumber: 976, Status: StatusCodeLanded, StartedAt: base.Add(time.Second)}

		slot, _, ok := st.SupersedingSession(candidate)
		if !ok || slot != "later-a" {
			t.Fatalf("iteration %d selected %q, %v; want deterministic later-a", i, slot, ok)
		}
	}
}
