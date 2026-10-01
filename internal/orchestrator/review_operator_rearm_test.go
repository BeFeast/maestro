package orchestrator

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func TestRunOncePausedExhaustedPRConsumesExplicitReviewTriggerDespiteFailedAggregate(t *testing.T) {
	for _, mode := range []string{"queued", "no_grant", "lookup_failed", "verdict_unavailable", "head_changed", "partial_rollup", "passed", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			const head = "873a89c69f47ea1c89dbeb46e889cbb71c6e28ed"
			cfg := llmReviewTestConfig()
			cfg.StateDir = t.TempDir()
			cfg.MaxParallel = 1
			cfg.ReviewProducer = config.ReviewProducerConfig{Enabled: mode != "disabled", NativeOpus: true, MaxAttempts: 1}
			prs := []github.PR{{Number: 386, HeadRefName: "retained-branch", IsDraft: true}}
			o, merged := newMergeTestOrchestrator(cfg, prs)
			o.repo = cfg.Repo
			o.tmuxSessionExistsFn = func(string) bool { return false }
			o.isIssueClosedFn = func(int) (bool, error) { return false, nil }
			o.ghPRHeadSHAFn = func(int) (string, error) {
				if mode == "head_changed" {
					return strings.Repeat("a", 40), nil
				}
				return head, nil
			}
			o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
				return github.PRCheckRollup{HeadSHA: head, Verdict: "failure", Complete: mode != "partial_rollup", Fingerprint: strings.Repeat("1", 64), Signals: []github.PRCheckSignal{
					{Name: "build", Status: "completed", Conclusion: "success"},
					{Name: "llm-review-opus", Status: "completed", Conclusion: "error"},
				}}, nil
			}
			o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) {
				if mode == "verdict_unavailable" {
					return github.ReviewGateVerdict{}, errors.New("unavailable")
				}
				return github.ReviewGateVerdict{Observed: true, Streams: []github.ReviewStreamVerdict{{Name: "llm-review-opus", Observed: true, LookupFailed: mode == "lookup_failed", Passed: mode == "passed"}}}, nil
			}
			o.reviewRearmQueuedFn = func(pr int, sha, stream string) bool {
				return mode != "no_grant" && pr == 386 && sha == head && stream == "llm-review-opus"
			}
			calls := make(chan producedCall, 2)
			o.reviewProduceFn = func(pr int, sha string, streams []string, _ config.ReviewProducerConfig, _ config.ForgeConfig) {
				calls <- producedCall{pr, sha, streams}
			}
			s := makeTestState(prs)
			now := time.Now().UTC()
			s.Paused, s.PausedAt = true, now
			sess := s.Sessions["slot-0"]
			sess.Status = state.StatusRetryExhausted
			sess.RetryCount, sess.UnexpectedExitRetries = 1, 1
			sess.LastNotifiedStatus = "ci_retry_exhausted"
			original := *sess
			if err := state.Save(cfg.StateDir, s); err != nil {
				t.Fatal(err)
			}
			if err := o.RunOnce(); err != nil {
				t.Fatal(err)
			}
			if mode == "queued" {
				select {
				case call := <-calls:
					if call.pr != 386 || call.head != head || !reflect.DeepEqual(call.streams, []string{"llm-review-opus"}) {
						t.Fatalf("unexpected dispatch: %+v", call)
					}
				case <-time.After(time.Second):
					t.Fatal("failed aggregate prevented explicit review trigger")
				}
			} else {
				o.reviewProduceMu.Lock()
				inFlight := o.reviewProduceInFlight[386]
				o.reviewProduceMu.Unlock()
				if inFlight {
					t.Fatal("untrusted/ungranted review started")
				}
				select {
				case call := <-calls:
					t.Fatalf("untrusted/ungranted trigger: %+v", call)
				default:
				}
			}
			after, err := state.Load(cfg.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			got := after.Sessions["slot-0"]
			if !after.Paused || got.Status != original.Status || got.PRNumber != original.PRNumber || got.Branch != original.Branch || got.RetryCount != original.RetryCount || got.UnexpectedExitRetries != original.UnexpectedExitRetries || len(*merged) != 0 {
				t.Fatalf("review trigger changed pause/identity/history or merged: session=%+v merged=%v", got, *merged)
			}
			snapshot := mustLatestPRGateSnapshot(t, after, 100, 386)
			if mode != "partial_rollup" && snapshot.CIEffectiveVerdict != state.PRGateCIFailure {
				t.Fatal("review trigger falsified CI")
			}
		})
	}
}
