package orchestrator

import (
	"sync"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/review"
)

type producedCall struct {
	pr      int
	head    string
	streams []string
}

// produceRecorder wires the test seam and returns the call log guarded for
// the goroutine handoff.
func produceRecorder(o *Orchestrator) (*[]producedCall, *sync.WaitGroup) {
	var mu sync.Mutex
	var calls []producedCall
	var wg sync.WaitGroup
	o.reviewProduceFn = func(pr int, head string, streams []string, rp config.ReviewProducerConfig) {
		mu.Lock()
		calls = append(calls, producedCall{pr: pr, head: head, streams: streams})
		mu.Unlock()
		wg.Done()
	}
	return &calls, &wg
}

func producerOrchestrator(enabled bool) *Orchestrator {
	return &Orchestrator{
		cfg: &config.Config{
			Repo: "owner/repo",
			ReviewProducer: config.ReviewProducerConfig{
				Enabled:     enabled,
				ChatBaseURL: "http://cliproxy.test",
			},
		},
		repo: "owner/repo",
	}
}

func TestMaybeProduceMissingReview_SelectsUnobservedLLMStreams(t *testing.T) {
	o := producerOrchestrator(true)
	calls, wg := produceRecorder(o)
	wg.Add(1)
	o.maybeProduceMissingReview(7, "head1", github.ReviewGateVerdict{
		Pending: true,
		Streams: []github.ReviewStreamVerdict{
			{Name: "llm-review-opus", Observed: true, Pending: true},
			{Name: "llm-review-cursor", Observed: false, Pending: true},
			{Name: "greptile", Observed: false, Pending: true},
			{Name: "llm-review-terra", Observed: false, Pending: true, LookupFailed: true},
		},
	})
	wg.Wait()
	if len(*calls) != 1 {
		t.Fatalf("calls = %+v, want exactly one production", *calls)
	}
	got := (*calls)[0]
	if got.pr != 7 || got.head != "head1" {
		t.Fatalf("call = %+v", got)
	}
	// Only the unobserved llm-review-* stream with a trustworthy read:
	// observed opus is already produced, greptile is not ours, terra's
	// silence is unproven (LookupFailed).
	if len(got.streams) != 1 || got.streams[0] != "llm-review-cursor" {
		t.Fatalf("streams = %v, want [llm-review-cursor]", got.streams)
	}
}

func TestMaybeProduceMissingReview_DisabledDoesNothing(t *testing.T) {
	o := producerOrchestrator(false)
	calls, _ := produceRecorder(o)
	o.maybeProduceMissingReview(7, "head1", github.ReviewGateVerdict{
		Pending: true,
		Streams: []github.ReviewStreamVerdict{{Name: "llm-review-opus", Pending: true}},
	})
	// The goroutine is never spawned when disabled; nothing to wait for.
	time.Sleep(10 * time.Millisecond)
	if len(*calls) != 0 {
		t.Fatalf("calls = %+v, want none when disabled", *calls)
	}
}

func TestMaybeProduceMissingReview_InFlightDedup(t *testing.T) {
	o := producerOrchestrator(true)
	var mu sync.Mutex
	started := 0
	release := make(chan struct{})
	done := make(chan struct{}, 2)
	o.reviewProduceFn = func(pr int, head string, streams []string, rp config.ReviewProducerConfig) {
		mu.Lock()
		started++
		mu.Unlock()
		<-release
		done <- struct{}{}
	}
	verdict := github.ReviewGateVerdict{
		Pending: true,
		Streams: []github.ReviewStreamVerdict{{Name: "llm-review-opus", Pending: true}},
	}
	o.maybeProduceMissingReview(7, "head1", verdict)
	// Wait until the first goroutine has claimed the PR.
	for {
		o.reviewProduceMu.Lock()
		claimed := o.reviewProduceInFlight[7]
		o.reviewProduceMu.Unlock()
		if claimed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// A second kick — even on a NEW head — must dedup: the running producer
	// already reviews the PR's current head, so a head-keyed second run would
	// double-spend on the same commit.
	o.maybeProduceMissingReview(7, "head2", verdict)
	close(release)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if started != 1 {
		t.Fatalf("started = %d, want 1 — the second kick on the same PR must dedup", started)
	}
}

func TestReviewLenses_ConfigMapping(t *testing.T) {
	t.Setenv("CLIPROXY_API_KEY", "proxy-key")
	t.Setenv("CURSOR_API_KEY", "cursor-key")
	rp := config.ReviewProducerConfig{Enabled: true, ChatBaseURL: "http://cliproxy.test"}
	lenses := reviewLenses([]string{"llm-review-opus", "llm-review-terra", "llm-review-cursor", "greptile"}, rp)
	if len(lenses) != 3 {
		t.Fatalf("lenses = %d, want 3 (greptile is not produceable)", len(lenses))
	}
	opus, ok := lenses[0].(*review.ChatLens)
	if !ok || opus.Model != "claude-opus-5" || opus.BaseURL != "http://cliproxy.test" || opus.APIKey != "proxy-key" {
		t.Fatalf("opus lens = %+v", lenses[0])
	}
	terra := lenses[1].(*review.ChatLens)
	if terra.Model != "gpt-5.6-terra" {
		t.Fatalf("terra model = %q", terra.Model)
	}
	cursor, ok := lenses[2].(*review.CursorLens)
	if !ok || cursor.Model != "composer-2.5" || cursor.APIKey != "cursor-key" {
		t.Fatalf("cursor lens = %+v", lenses[2])
	}
}

func TestReviewLenses_MissingCredsStillBuildsLens(t *testing.T) {
	t.Setenv("CLIPROXY_API_KEY", "")
	lenses := reviewLenses([]string{"llm-review-opus"}, config.ReviewProducerConfig{Enabled: true, ChatBaseURL: "http://cliproxy.test"})
	if len(lenses) != 1 {
		t.Fatalf("lenses = %d, want 1", len(lenses))
	}
	// The producer's Available check owns the creds decision — it must get
	// the lens so the explicit error status is posted instead of silence.
	if err := lenses[0].Available(); err == nil {
		t.Fatal("lens without a key must report unavailable")
	}
}
