package state

import (
	"testing"
	"time"
)

// #565 — per-(pr_number, head_sha) idempotency. Repeated calls inside a
// budget bump the attempt counter; once the budget is exhausted, further
// attempts return spawned=false so the dispatcher can fall through to
// operator review instead of silently looping.
func TestRecordReviewRepairAttempt_BudgetIdempotency(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	st := NewState()
	pr, headSHA, issue := 564, "deadbeef1234", 442

	// First attempt within budget=2 should succeed.
	track, spawned := st.RecordReviewRepairAttempt(pr, headSHA, issue, "approval-1", 2, now)
	if !spawned || track.Attempts != 1 || track.Exhausted {
		t.Fatalf("first attempt: track=%+v spawned=%v, want Attempts=1 spawned=true", track, spawned)
	}

	// Second attempt also within budget.
	track, spawned = st.RecordReviewRepairAttempt(pr, headSHA, issue, "approval-2", 2, now)
	if !spawned || track.Attempts != 2 {
		t.Fatalf("second attempt: track=%+v spawned=%v, want Attempts=2 spawned=true", track, spawned)
	}

	// Third attempt: budget exhausted — must NOT spawn.
	track, spawned = st.RecordReviewRepairAttempt(pr, headSHA, issue, "approval-3", 2, now)
	if spawned {
		t.Fatalf("third attempt spawned despite exhausted budget: track=%+v", track)
	}
	if !track.Exhausted {
		t.Fatalf("track.Exhausted=false after budget exhaust: %+v", track)
	}
}

// A new head SHA on the same PR creates a fresh (pr,head_sha) budget —
// the repair worker pushed an update and the supervisor should be able
// to retry on the new head.
func TestRecordReviewRepairAttempt_NewHeadSHASeparateBudget(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	st := NewState()
	pr, oldHead, newHead, issue := 564, "deadbeef1111", "deadbeef2222", 442

	// Exhaust the old head's budget.
	_, _ = st.RecordReviewRepairAttempt(pr, oldHead, issue, "a1", 1, now)
	_, spawned := st.RecordReviewRepairAttempt(pr, oldHead, issue, "a1", 1, now)
	if spawned {
		t.Fatalf("expected old-head budget to be exhausted")
	}

	// The new head has its own budget.
	track, spawned := st.RecordReviewRepairAttempt(pr, newHead, issue, "a2", 1, now)
	if !spawned || track.Attempts != 1 {
		t.Fatalf("new head: track=%+v spawned=%v, want fresh budget", track, spawned)
	}
}

// Lookup must return the recorded track for the (pr,head_sha) pair, and
// only that pair — distinct heads do not collide.
func TestLookupReviewRepairTrack_KeyIsolation(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	st := NewState()
	st.RecordReviewRepairAttempt(564, "head-a", 442, "x", 5, now)
	st.RecordReviewRepairAttempt(564, "head-b", 442, "x", 5, now)

	a, ok := st.LookupReviewRepairTrack(564, "head-a")
	if !ok || a.HeadSHA != "head-a" || a.Attempts != 1 {
		t.Fatalf("lookup head-a: ok=%v track=%+v", ok, a)
	}
	b, ok := st.LookupReviewRepairTrack(564, "head-b")
	if !ok || b.HeadSHA != "head-b" || b.Attempts != 1 {
		t.Fatalf("lookup head-b: ok=%v track=%+v", ok, b)
	}
	if _, ok := st.LookupReviewRepairTrack(564, "missing"); ok {
		t.Fatal("lookup for unknown head must return ok=false")
	}
}

// MarkReviewRepairExhausted flips the terminal flag and records the
// unresolved summary; calling twice does not duplicate the timestamp.
func TestMarkReviewRepairExhausted_Idempotent(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	st := NewState()
	pr, headSHA := 564, "deadbeef"
	track := st.MarkReviewRepairExhausted(pr, headSHA, "1 P0 finding unresolved", now)
	if !track.Exhausted || track.UnresolvedSummary != "1 P0 finding unresolved" {
		t.Fatalf("first mark: %+v", track)
	}
	stamp := track.ExhaustedAt
	track2 := st.MarkReviewRepairExhausted(pr, headSHA, "different summary", now.Add(time.Hour))
	if !track2.ExhaustedAt.Equal(stamp) {
		t.Fatalf("second mark moved ExhaustedAt from %v to %v", stamp, track2.ExhaustedAt)
	}
	if track2.UnresolvedSummary != "different summary" {
		t.Fatalf("second mark should refresh summary; got %q", track2.UnresolvedSummary)
	}
}

// approvalTargetsEqual now considers HeadSHA — two decisions for the
// same (action, pr) with different head SHAs must NOT coalesce at mint.
// This is the dispatcher-side guarantee that a new push retires the
// stale recommendation.
func TestApprovalTargetsEqual_HeadSHADifferentiates(t *testing.T) {
	a := &SupervisorTarget{Issue: 442, PR: 564, HeadSHA: "deadbeef"}
	b := &SupervisorTarget{Issue: 442, PR: 564, HeadSHA: "cafebabe"}
	if approvalTargetsEqual(a, b) {
		t.Fatalf("distinct head SHAs must not be equal: a=%+v b=%+v", a, b)
	}
	c := &SupervisorTarget{Issue: 442, PR: 564, HeadSHA: "deadbeef"}
	if !approvalTargetsEqual(a, c) {
		t.Fatalf("identical targets should be equal: a=%+v c=%+v", a, c)
	}
}
