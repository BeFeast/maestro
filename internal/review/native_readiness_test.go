package review

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/forge"
	"github.com/befeast/maestro/internal/supervisor"
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
		p.Lenses = []Lens{&NativeClaudeLens{Stream: "llm-review-opus", Model: model, policy: aiexecution.Policy{RequireVerifiedRoute: true}, complete: func(_ context.Context, _ string, id string) (supervisor.NativeReviewResult, error) {
			calls.Add(1)
			if uuid.Validate(id) != nil {
				t.Errorf("not a durable claim UUID: %q", id)
			}
			return supervisor.NativeReviewResult{}, resultErr
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

func TestNativeReviewAccountingPendingPostsVerdictWithoutSecondAttempt(t *testing.T) {
	for _, blocking := range []bool{false, true} {
		now := time.Now()
		s := newAttemptStore(t)
		p, f := httpProducer(t, s, "unused", &now, 3)
		var calls atomic.Int64
		verdict := "[P2] a.go:2 — advisory only"
		if blocking {
			verdict = "[P1] a.go:2 — real bug"
		}
		p.Lenses = []Lens{&NativeClaudeLens{Stream: "llm-review-opus", Model: "claude-opus-5", policy: aiexecution.Policy{RequireVerifiedRoute: true}, complete: func(context.Context, string, string) (supervisor.NativeReviewResult, error) {
			calls.Add(1)
			// #1239: complete verdict, authority accounting still pending.
			return supervisor.NativeReviewResult{Output: verdict, AccountingPending: true}, nil
		}}}
		p.ExecutionPolicy.RequireVerifiedRoute = true
		if err := p.ProducePR(context.Background(), 7); err != nil {
			t.Fatalf("blocking=%v: pending accounting failed the stream: %v", blocking, err)
		}
		st := lastStatusFor(t, f, "llm-review-opus")
		want := forge.StatusSuccess
		if blocking {
			want = forge.StatusFailure
		}
		if st.State != want || !strings.Contains(st.Description, "accounting pending") || len(f.inlineComments) != 1 {
			t.Fatalf("blocking=%v: status=%+v inline=%v", blocking, st, f.inlineComments)
		}
		a := latestAttempt(t, s, scopeFor(f))
		if a.Outcome != "completed" || a.Reason != "review_completed_accounting_pending" || a.NextAction != "publish_or_reconcile_review" || a.EvidenceSHA256 == "" {
			t.Fatalf("blocking=%v: attempt=%+v", blocking, a)
		}
		// The settled status is the idempotency key: no second attempt is spent.
		f.statuses = append(f.statuses, forge.Status{Context: "llm-review-opus", State: want})
		now = now.Add(24 * time.Hour)
		if err := p.ProducePR(context.Background(), 7); err != nil || calls.Load() != 1 {
			t.Fatalf("blocking=%v: pending verdict replayed: calls=%d err=%v", blocking, calls.Load(), err)
		}
	}
}
