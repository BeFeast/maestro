package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/review"
	"github.com/befeast/maestro/internal/state"
)

func TestReviewProducerSelectsOnlyDueRecordedHTTPHold(t *testing.T) {
	o := producerOrchestrator(true)
	o.cfg.StateDir = t.TempDir()
	o.cfg.ReviewProducer.MaxAttempts = 2
	if err := state.Save(o.cfg.StateDir, state.NewState()); err != nil {
		t.Fatal(err)
	}
	store := &review.AttemptStore{StateDir: o.cfg.StateDir}
	scope := review.AttemptScope{Repo: o.repo, PR: 7, Head: "head", Lens: "llm-review-opus"}
	now := time.Now().Add(-2 * time.Minute)
	id, err := store.Claim(scope, now, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(scope, id, now, &review.GatewayTerminalError{Code: "upstream_transient", Terminal: &review.GatewayTerminal{Reason: "upstream_transient", RetryScope: "same_request", Attempted: 1}}); err != nil {
		t.Fatal(err)
	}
	calls, wg := produceRecorder(o)
	wg.Add(1)
	o.maybeProduceMissingReview(7, "head", github.ReviewGateVerdict{Streams: []github.ReviewStreamVerdict{
		{Name: "llm-review-opus", Observed: true, Pending: true},
		{Name: "llm-review-terra", Observed: true, Pending: true},
	}})
	wg.Wait()
	if len(*calls) != 1 || len((*calls)[0].streams) != 1 || (*calls)[0].streams[0] != "llm-review-opus" {
		t.Fatalf("calls=%+v", *calls)
	}
	// Claimed, changed head, passed stream and reduced configured allowance are not due.
	if _, err := store.Claim(scope, time.Now(), 2); err != nil {
		t.Fatal(err)
	}
	o.maybeProduceMissingReview(7, "head", github.ReviewGateVerdict{Streams: []github.ReviewStreamVerdict{{Name: "llm-review-opus", Observed: true}}})
	o.maybeProduceMissingReview(7, "other-head", github.ReviewGateVerdict{Streams: []github.ReviewStreamVerdict{{Name: "llm-review-opus", Observed: true}}})
	if len(*calls) != 1 {
		t.Fatal("unknown active claim or old head was dispatched")
	}
}
