package state

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSessionAttentionAdvisorFailureExposesExactFindings(t *testing.T) {
	findings := "Missing timeout coverage.\nValidation does not prove rollback."
	sess := &Session{
		Status:                    StatusFailed,
		Phase:                     PhaseAdvisor,
		AdvisorTerminalReason:     "review_rounds_exhausted",
		AdvisorUnresolvedFindings: findings,
	}
	attention := SessionAttentionFor(sess, nil)
	if !attention.NeedsAttention || !strings.Contains(attention.Reason, findings) || !strings.Contains(attention.Reason, "review_rounds_exhausted") {
		t.Fatalf("attention = %+v", attention)
	}
}

func TestSessionAdvisorReviewStateRoundTrips(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	want := &Session{
		IssueNumber:               928,
		Phase:                     PhaseAdvisor,
		PlanVersion:               2,
		AdvisorReviewRound:        2,
		AdvisorMaxReviewRounds:    5,
		AdvisorBackend:            "advisor",
		AdvisorModel:              "review-model",
		AdvisorVerdict:            "PLAN_REVISE",
		AdvisorUnresolvedFindings: "exact finding",
		AdvisorTerminalReason:     "review_rounds_exhausted",
		AdvisorReviews: []AdvisorReview{{
			PlanVersion: 2, ReviewRound: 2, Backend: "advisor", Model: "review-model", Verdict: "PLAN_REVISE", Findings: "exact finding", ReviewedAt: now,
		}},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Session
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.PlanVersion != want.PlanVersion || got.AdvisorReviewRound != want.AdvisorReviewRound || got.AdvisorUnresolvedFindings != want.AdvisorUnresolvedFindings || len(got.AdvisorReviews) != 1 || !got.AdvisorReviews[0].ReviewedAt.Equal(now) {
		t.Fatalf("round trip = %+v", got)
	}
}
