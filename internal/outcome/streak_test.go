package outcome

import (
	"testing"
	"time"
)

func failingCheck(name string) HealthCheckResult {
	return HealthCheckResult{
		CheckedAt: time.Now().UTC(),
		State:     HealthFailing,
		Checks:    []HealthCheckItem{{Name: name, Blocking: true, Status: "fail"}},
	}
}

func passingCheck(name string) HealthCheckResult {
	return HealthCheckResult{
		CheckedAt: time.Now().UTC(),
		State:     HealthHealthy,
		Checks:    []HealthCheckItem{{Name: name, Blocking: true, Status: "pass"}},
	}
}

// applyEmission mimics the caller marking a fingerprint after a successful
// notification/intake so the pure model's dedup can be exercised end to end.
func applyEmission(streaks []GateStreak, events []GateStreakEvent) []GateStreak {
	for _, event := range events {
		for i := range streaks {
			if streaks[i].Gate == event.Gate && streaks[i].Fingerprint == event.Fingerprint {
				if event.NotifyPending {
					streaks[i].NotifiedFingerprint = event.Fingerprint
				}
				if event.IntakePending {
					streaks[i].IntakeFingerprint = event.Fingerprint
					streaks[i].IntakeIssue = 4242
				}
			}
		}
	}
	return streaks
}

func TestRecordGateStreaks_ThresholdEmitsOnceThenDedupes(t *testing.T) {
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)

	// First failure: below threshold (default 2), no event.
	streaks, events := RecordGateStreaks(nil, failingCheck("deb-idle-return-smoke"), 0, "https://runtime/health", now)
	if len(events) != 0 {
		t.Fatalf("first failure emitted %d events, want 0", len(events))
	}
	if streaks[0].ConsecutiveFailures != 1 {
		t.Fatalf("count = %d, want 1", streaks[0].ConsecutiveFailures)
	}

	// Second consecutive same-gate failure crosses the threshold: exactly one
	// event, pending both notify and intake, with gate name + run link.
	streaks, events = RecordGateStreaks(streaks, failingCheck("deb-idle-return-smoke"), 0, "https://runtime/health", now.Add(time.Minute))
	if len(events) != 1 {
		t.Fatalf("second failure emitted %d events, want exactly 1", len(events))
	}
	ev := events[0]
	if ev.Gate != "deb-idle-return-smoke" || ev.ConsecutiveFailures != 2 {
		t.Fatalf("event = %+v, want gate deb-idle-return-smoke count 2", ev)
	}
	if !ev.NotifyPending || !ev.IntakePending {
		t.Fatalf("event = %+v, want notify+intake pending", ev)
	}
	if ev.RunLink != "https://runtime/health" {
		t.Fatalf("run link = %q, want the latest run link", ev.RunLink)
	}
	streaks = applyEmission(streaks, events)

	// Third identical failure: streak keeps counting but must not re-fire.
	streaks, events = RecordGateStreaks(streaks, failingCheck("deb-idle-return-smoke"), 0, "https://runtime/health", now.Add(2*time.Minute))
	if len(events) != 0 {
		t.Fatalf("third identical failure emitted %d events, want 0 (deduped)", len(events))
	}
	if streaks[0].ConsecutiveFailures != 3 {
		t.Fatalf("count = %d, want 3", streaks[0].ConsecutiveFailures)
	}
	if streaks[0].IntakeIssue != 4242 {
		t.Fatalf("intake issue = %d, want the deduped repair issue preserved", streaks[0].IntakeIssue)
	}
}

func TestRecordGateStreaks_PassResetsStreak(t *testing.T) {
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	streaks, _ := RecordGateStreaks(nil, failingCheck("portability-package-smoke"), 0, "", now)
	streaks, events := RecordGateStreaks(streaks, failingCheck("portability-package-smoke"), 0, "", now.Add(time.Minute))
	if len(events) != 1 {
		t.Fatalf("want one escalation before the pass, got %d", len(events))
	}
	streaks = applyEmission(streaks, events)

	streaks, events = RecordGateStreaks(streaks, passingCheck("portability-package-smoke"), 0, "", now.Add(2*time.Minute))
	if len(events) != 0 {
		t.Fatalf("pass emitted %d events, want 0", len(events))
	}
	if streaks[0].ConsecutiveFailures != 0 || streaks[0].Fingerprint != "" {
		t.Fatalf("pass did not reset streak: %+v", streaks[0])
	}
	if streaks[0].NotifiedFingerprint != "" || streaks[0].IntakeFingerprint != "" {
		t.Fatalf("pass did not clear emission marks: %+v", streaks[0])
	}

	// A fresh failure run after the reset must escalate again.
	streaks, _ = RecordGateStreaks(streaks, failingCheck("portability-package-smoke"), 0, "", now.Add(3*time.Minute))
	streaks, events = RecordGateStreaks(streaks, failingCheck("portability-package-smoke"), 0, "", now.Add(4*time.Minute))
	if len(events) != 1 {
		t.Fatalf("post-reset streak did not re-escalate: %d events", len(events))
	}
}

func TestRecordGateStreaks_DifferentGateStartsNewFingerprint(t *testing.T) {
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	streaks, _ := RecordGateStreaks(nil, failingCheck("gate-a"), 0, "", now)
	streaks, events := RecordGateStreaks(streaks, failingCheck("gate-b"), 0, "", now.Add(time.Minute))
	if len(events) != 0 {
		t.Fatalf("distinct gate failures should each be a single-count streak, got %d events", len(events))
	}
	byGate := map[string]GateStreak{}
	for _, s := range streaks {
		byGate[s.Gate] = s
	}
	if byGate["gate-a"].ConsecutiveFailures != 1 || byGate["gate-b"].ConsecutiveFailures != 1 {
		t.Fatalf("each gate should carry its own count-1 streak: %+v", streaks)
	}
	if byGate["gate-a"].Fingerprint == byGate["gate-b"].Fingerprint {
		t.Fatalf("distinct gates must have distinct fingerprints")
	}
}

func TestRecordGateStreaks_ReasonClassChangeStartsNewStreak(t *testing.T) {
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	failReason := func(status string) HealthCheckResult {
		return HealthCheckResult{
			CheckedAt: now,
			State:     HealthFailing,
			Checks:    []HealthCheckItem{{Name: "gate", Blocking: true, Status: status}},
		}
	}
	streaks, _ := RecordGateStreaks(nil, failReason("fail"), 0, "", now)
	streaks, events := RecordGateStreaks(streaks, failReason("error"), 0, "", now.Add(time.Minute))
	if len(events) != 0 {
		t.Fatalf("reason-class change should restart the streak at count 1, got %d events", len(events))
	}
	if streaks[0].ConsecutiveFailures != 1 || streaks[0].ReasonClass != "error" {
		t.Fatalf("streak did not restart on reason change: %+v", streaks[0])
	}
}

func TestRecordGateStreaks_IndeterminateDoesNotResetOrCount(t *testing.T) {
	now := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	pending := HealthCheckResult{
		CheckedAt: now,
		State:     HealthPending,
		Checks:    []HealthCheckItem{{Name: "gate", Blocking: true, Status: "pending"}},
	}
	streaks, _ := RecordGateStreaks(nil, failingCheck("gate"), 0, "", now)
	streaks, events := RecordGateStreaks(streaks, pending, 0, "", now.Add(time.Minute))
	if len(events) != 0 {
		t.Fatalf("pending run emitted %d events, want 0", len(events))
	}
	if streaks[0].ConsecutiveFailures != 1 {
		t.Fatalf("pending run must not change the count: %+v", streaks[0])
	}
}
