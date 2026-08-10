package orchestrator

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/review"
)

// maybeProduceMissingReview kicks the in-process llm-review producer for the
// gate's unobserved llm-review-* streams on this head (#1162 S5). This is the
// daemon-side trigger the #1148 bash glue was interim for: the gate already
// knows exactly which required stream has produced no signal — that knowledge
// becomes production instead of a wait.
//
// Duplicate kicks are cheap by design: the producer's first act is the
// per-head settled/pending idempotency check against the posted statuses, so
// the in-flight guard here only prevents concurrent runs from this process.
// Streams whose read failed (LookupFailed) are skipped — unproven silence is
// not a reason to spend a model run.
func (o *Orchestrator) maybeProduceMissingReview(prNumber int, headSHA string, verdict github.ReviewGateVerdict) {
	if o.cfg == nil || !o.cfg.ReviewProducer.Enabled {
		return
	}
	var missing []string
	for _, sv := range verdict.Streams {
		if !strings.HasPrefix(sv.Name, "llm-review-") {
			continue
		}
		if sv.Observed || sv.LookupFailed {
			continue
		}
		missing = append(missing, sv.Name)
	}
	if len(missing) == 0 {
		return
	}

	// One run per PR (not per head): the producer reviews the PR's CURRENT
	// head regardless of the head this cycle observed, so a head-keyed guard
	// would let a run kicked on the old head race a second one kicked on the
	// new — two full model passes over the same commit.
	o.reviewProduceMu.Lock()
	if o.reviewProduceInFlight == nil {
		o.reviewProduceInFlight = make(map[int]bool)
	}
	if o.reviewProduceInFlight[prNumber] {
		o.reviewProduceMu.Unlock()
		return
	}
	o.reviewProduceInFlight[prNumber] = true
	o.reviewProduceMu.Unlock()

	produce := o.reviewProduceFn
	if produce == nil {
		produce = o.produceReviewStreams
	}
	// Snapshot the config on this goroutine: the orchestrator loop owns both
	// this call and the hot-reload writes, so reading o.cfg here is
	// race-free, while the spawned goroutine below must never touch it.
	rp := o.cfg.ReviewProducer
	go func() {
		defer func() {
			o.reviewProduceMu.Lock()
			delete(o.reviewProduceInFlight, prNumber)
			o.reviewProduceMu.Unlock()
		}()
		produce(prNumber, headSHA, missing, rp)
	}()
}

// reviewProducerRunTimeout bounds one full producer pass (all lenses on one
// PR). Individual lens runs are already bounded at 10 minutes each; this is
// the outer fence so a producer goroutine can never outlive its usefulness.
const reviewProducerRunTimeout = 30 * time.Minute

// reviewLenses maps the missing stream names to configured lens runners.
// Credentials resolve from the environment by name (*_env indirection) at
// kick time; a stream whose credentials are absent still gets its lens — the
// producer's Available check turns that into the explicit
// "skipped: credentials not configured" error status, never silence.
// rp travels by value: this runs on the producer goroutine, which must not
// read o.cfg (the orchestrator loop hot-reloads it without a lock).
func reviewLenses(streams []string, rp config.ReviewProducerConfig) []review.Lens {
	var lenses []review.Lens
	for _, stream := range streams {
		switch stream {
		case "llm-review-opus":
			lenses = append(lenses, &review.ChatLens{
				Stream:  stream,
				BaseURL: rp.ChatBaseURL,
				APIKey:  os.Getenv(rp.EffectiveChatAPIKeyEnv()),
				Model:   rp.EffectiveOpusModel(),
			})
		case "llm-review-terra":
			lenses = append(lenses, &review.ChatLens{
				Stream:  stream,
				BaseURL: rp.ChatBaseURL,
				APIKey:  os.Getenv(rp.EffectiveChatAPIKeyEnv()),
				Model:   rp.EffectiveTerraModel(),
			})
		case "llm-review-cursor":
			lenses = append(lenses, &review.CursorLens{
				Stream: stream,
				Model:  rp.EffectiveCursorModel(),
				APIKey: os.Getenv(rp.EffectiveCursorAPIKeyEnv()),
			})
		}
	}
	return lenses
}

// produceReviewStreams runs the producer for one PR head. The forge is the
// GitHub client for now; per-project forge selection (Forgejo) arrives with
// the Forgejo project wiring — the producer itself is already forge-agnostic.
// Runs on the producer goroutine: everything it needs arrives by value.
func (o *Orchestrator) produceReviewStreams(prNumber int, headSHA string, streams []string, rp config.ReviewProducerConfig) {
	lenses := reviewLenses(streams, rp)
	if len(lenses) == 0 {
		return
	}
	p := &review.Producer{
		Forge:             github.NewForge(),
		Repo:              o.repo,
		Lenses:            lenses,
		PendingStaleAfter: rp.EffectivePendingStale(),
		MaxDiffBytes:      rp.EffectiveMaxDiffBytes(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), reviewProducerRunTimeout)
	defer cancel()
	log.Printf("[orch] PR #%d: producing llm-review streams %v on %s", prNumber, streams, headSHA)
	if err := p.ProducePR(ctx, prNumber); err != nil {
		log.Printf("[orch] PR #%d: llm-review producer: %v", prNumber, err)
		return
	}
	log.Printf("[orch] PR #%d: llm-review production complete on %s", prNumber, headSHA)
}
