package review

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/google/uuid"
)

func TestStrictUnsupportedReviewLeavesNeverSend(t *testing.T) {
	var sent atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent.Add(1) }))
	defer srv.Close()
	policy := aiexecution.Policy{RequireVerifiedRoute: true}
	for _, lens := range []Lens{&ChatLens{BaseURL: srv.URL, Model: "same", APIKey: "fixture", ExecutionPolicy: policy}, &CursorLens{APIKey: "fixture", Binary: "must-not-execute", ExecutionPolicy: policy}} {
		_, err := lens.Run(context.Background(), "prompt")
		var hold *aiexecution.Hold
		if !errors.As(err, &hold) {
			t.Fatalf("missing typed hold: %v", err)
		}
	}
	if sent.Load() != 0 {
		t.Fatal("unsupported HTTP lane sent")
	}
}
func TestNativeReviewClaimRetainsUnknownAndCommittedOutcomes(t *testing.T) {
	for _, resultErr := range []error{aiexecution.Held("native_outcome_unverified"), errors.New("opaque CLI exit"), &GatewayTerminalError{Code: "upstream_transient", Terminal: &GatewayTerminal{StreamCommitted: true}}} {
		now := time.Now()
		s := newAttemptStore(t)
		p, f := httpProducer(t, s, "unused", &now, 3)
		var calls atomic.Int64
		model := "claude-opus-5"
		p.Lenses = []Lens{&NativeClaudeLens{Stream: "llm-review-opus", Model: model, policy: aiexecution.Policy{RequireVerifiedRoute: true}, complete: func(_ context.Context, _ string, id string) (string, error) {
			calls.Add(1)
			if uuid.Validate(id) != nil {
				t.Errorf("not a durable claim UUID: %q", id)
			}
			return "", resultErr
		}}}
		p.ExecutionPolicy.RequireVerifiedRoute = true
		_ = p.ProducePR(context.Background(), 7)
		now = now.Add(24 * time.Hour)
		_ = p.ProducePR(context.Background(), 7)
		if calls.Load() != 1 || latestAttempt(t, s, scopeFor(f)).Outcome != "held" {
			t.Fatalf("native unknown/committed replayed: calls=%d", calls.Load())
		}
	}
}
