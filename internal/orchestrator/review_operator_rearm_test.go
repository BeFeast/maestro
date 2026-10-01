package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/forge"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/review"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
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

// rearmTestLimiter stands in for the daemon's auxiliary capacity owner.
type rearmTestLimiter struct {
	mu     sync.Mutex
	code   string
	probes int
}

func (l *rearmTestLimiter) set(code string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.code = code
}

func (l *rearmTestLimiter) ReserveAuxiliary(_, _ string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.probes++
	if l.code != "" {
		return nil, aiexecution.Held(l.code)
	}
	return func() {}, nil
}

// rearmTestForge is the minimal forge the real review producer needs: one PR
// at a fixed head whose prior opus review settled with an error status.
type rearmTestForge struct {
	pr forge.PR
	mu sync.Mutex
	// posted collects every status the producer wrote.
	posted []forge.Status
}

func (f *rearmTestForge) GetPR(context.Context, string, int) (forge.PR, error) { return f.pr, nil }
func (f *rearmTestForge) GetPRDiff(context.Context, string, int) ([]byte, error) {
	return []byte("diff --git a/a.go b/a.go\n+x\n"), nil
}
func (f *rearmTestForge) CommitStatuses(context.Context, string, string) ([]forge.Status, error) {
	return []forge.Status{{Context: "llm-review-opus", State: forge.StatusError, Description: "review held: request_invalid"}}, nil
}
func (f *rearmTestForge) CreateCommitStatus(_ context.Context, _, _ string, status forge.Status) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posted = append(f.posted, status)
	return nil
}
func (f *rearmTestForge) CreateReviewComment(context.Context, string, int, string, string, int, string) error {
	return nil
}
func (f *rearmTestForge) CreateComment(context.Context, string, int, string) error { return nil }

// #1233 RunOnce regression: a paused project whose PR carries an aggregate CI
// failure caused by the prior native review holds a queued operator grant.
// While auxiliary capacity is exhausted the daemon's authorized trigger does
// not dispatch the producer at all (the grant's preflight observes the hold),
// so the grant files, the attempt history, the pause and the session identity
// are untouched and the red gate is not falsified. Once capacity frees the
// grant is dispatched exactly once through the real producer and the real
// native lens; the runner is entered and fails without a typed hold, which
// spends the grant, and no later cycle dispatches it again.
func TestRunOncePausedPRRetainsQueuedGrantWhileAuxiliaryCapacityExhausted(t *testing.T) {
	const head = "873a89c69f47ea1c89dbeb46e889cbb71c6e28ed"
	cfg := llmReviewTestConfig()
	cfg.StateDir = t.TempDir()
	cfg.ProjectID = "pilot-project"
	cfg.MaxParallel = 1
	cfg.ReviewProducer = config.ReviewProducerConfig{Enabled: true, NativeOpus: true, MaxAttempts: 1}
	cfg.AIExecution = aiexecution.Policy{RequireVerifiedRoute: true}
	cfg.Supervisor.NativeSessionRegistration = &config.NativeSessionRegistrationConfig{BudgetRunID: "pilot-run"}
	limiter := &rearmTestLimiter{code: "auxiliary_capacity_exhausted"}
	cfg.RuntimeAuxiliaryLimiter = limiter
	prs := []github.PR{{Number: 386, HeadRefName: "retained-branch", IsDraft: true}}
	o, merged := newMergeTestOrchestrator(cfg, prs)
	o.repo = cfg.Repo
	o.tmuxSessionExistsFn = func(string) bool { return false }
	o.isIssueClosedFn = func(int) (bool, error) { return false, nil }
	o.ghPRHeadSHAFn = func(int) (string, error) { return head, nil }
	o.ghPRCheckRollupFn = func(int) (github.PRCheckRollup, error) {
		return github.PRCheckRollup{HeadSHA: head, Verdict: "failure", Complete: true, Fingerprint: strings.Repeat("1", 64), Signals: []github.PRCheckSignal{
			{Name: "build", Status: "completed", Conclusion: "success"},
			{Name: "llm-review-opus", Status: "completed", Conclusion: "error"},
		}}, nil
	}
	o.ghPRReviewGateVerdictFn = func(int, []string) (github.ReviewGateVerdict, error) {
		return github.ReviewGateVerdict{Observed: true, Streams: []github.ReviewStreamVerdict{{Name: "llm-review-opus", Observed: true}}}, nil
	}
	// The real producer runs behind the dispatch seam with the real native
	// lens; only the forge is faked.
	fg := &rearmTestForge{pr: forge.PR{Number: 386, Title: "retained", HeadSHA: head, BaseRef: "main"}}
	dispatched := make(chan producedCall, 4)
	o.reviewProduceFn = func(pr int, sha string, streams []string, rp config.ReviewProducerConfig, _ config.ForgeConfig) {
		p := &review.Producer{
			ExecutionPolicy: rp.RuntimeExecutionPolicy,
			Attempts:        &review.AttemptStore{StateDir: cfg.StateDir},
			MaxAttempts:     rp.EffectiveMaxAttempts(),
			Forge:           fg,
			Repo:            cfg.Repo,
			ExpectedHead:    sha,
			Lenses:          []review.Lens{review.NewNativeClaudeLens("llm-review-opus", rp.EffectiveOpusModel(), rp.RuntimeNativeConfig)},
			Logf:            func(string, ...any) {},
		}
		// The native runner is entered and fails before any launch without a
		// typed hold: such an attempt may have launched and spends the grant.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = p.ProducePR(ctx, pr)
		dispatched <- producedCall{pr, sha, streams}
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
	// Durable history: the prior native review was held before any launch
	// (zero invocations) and the operator queued one explicit rearm for it.
	store := &review.AttemptStore{StateDir: cfg.StateDir}
	scope := review.AttemptScope{Repo: cfg.Repo, PR: 386, Head: head, Lens: "llm-review-opus"}
	prior, err := store.Claim(scope, now, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(scope, prior, now, errors.New("request invalid")); err != nil {
		t.Fatal(err)
	}
	nativeDir := filepath.Join(cfg.StateDir, "native-reviews")
	if err := os.MkdirAll(filepath.Join(nativeDir, "supervisor-consultations"), 0700); err != nil {
		t.Fatal(err)
	}
	ended := now
	receipt, err := json.Marshal(supervisor.ConsultationReceipt{SchemaVersion: 1, Identity: supervisor.ConsultationIdentity{ID: prior, ProjectID: cfg.ProjectID, CycleID: prior, Role: "reviewer"}, StartedAt: now, EndedAt: &ended, Status: "failed", Candidates: []supervisor.CandidateReceipt{}, Invocations: []supervisor.InvocationReceipt{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nativeDir, "supervisor-consultations", prior+".json"), receipt, 0600); err != nil {
		t.Fatal(err)
	}
	proof, err := review.NativeReviewPreLaunchHoldProof(nativeDir, prior, cfg.ProjectID, "pilot-run")
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	var history []state.ReviewAttempt
	for _, track := range seeded.ReviewAttempts {
		history = track.Attempts
	}
	if len(history) != 1 || history[0].ID != prior || history[0].Outcome != "held" {
		t.Fatalf("seeded history = %+v", history)
	}
	req := review.OperatorRearmRequest{PreviousAttemptID: prior, EvidenceSHA256: history[0].EvidenceSHA256, NativeProofSHA256: proof, NativeProofKind: review.RearmProofPreLaunchHold, Actor: "operator", Reason: "request construction repaired; one more same-run review", ExpiresAt: now.Add(time.Hour)}
	if _, err := store.AuthorizeNativeRearm(cfg, scope, req, now); err != nil {
		t.Fatal(err)
	}
	rearmDir := filepath.Join(cfg.StateDir, "review-rearms")
	grantFiles := func() map[string]string {
		entries, err := os.ReadDir(rearmDir)
		if err != nil {
			t.Fatal(err)
		}
		files := map[string]string{}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".lock") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(rearmDir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			files[e.Name()] = string(b)
		}
		return files
	}
	grantBefore := grantFiles()
	if len(grantBefore) != 1 {
		t.Fatalf("grant files = %v", grantBefore)
	}
	var grantName string
	for name := range grantBefore {
		grantName = name
	}
	attempts := func() []state.ReviewAttempt {
		st, err := state.Load(cfg.StateDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, track := range st.ReviewAttempts {
			return track.Attempts
		}
		return nil
	}
	assertUnchanged := func(when string) {
		t.Helper()
		after, err := state.Load(cfg.StateDir)
		if err != nil {
			t.Fatal(err)
		}
		got := after.Sessions["slot-0"]
		if !after.Paused || got.Status != original.Status || got.PRNumber != original.PRNumber || got.Branch != original.Branch || got.RetryCount != original.RetryCount || got.UnexpectedExitRetries != original.UnexpectedExitRetries || len(*merged) != 0 {
			t.Fatalf("%s: review trigger changed pause/identity/history or merged: session=%+v merged=%v", when, got, *merged)
		}
		if snapshot := mustLatestPRGateSnapshot(t, after, 100, 386); snapshot.CIEffectiveVerdict != state.PRGateCIFailure {
			t.Fatalf("%s: review trigger falsified CI", when)
		}
	}
	noDispatch := func(when string) {
		t.Helper()
		select {
		case call := <-dispatched:
			t.Fatalf("%s: unexpected dispatch %+v", when, call)
		case <-time.After(200 * time.Millisecond):
		}
	}

	// Capacity exhausted: two cycles, no dispatch, nothing written.
	for cycle := 1; cycle <= 2; cycle++ {
		if err := o.RunOnce(); err != nil {
			t.Fatal(err)
		}
		noDispatch("exhausted")
		if got := grantFiles(); !reflect.DeepEqual(got, grantBefore) {
			t.Fatalf("exhausted cycle %d: grant files changed: %v", cycle, got)
		}
		if got := attempts(); !reflect.DeepEqual(got, history) {
			t.Fatalf("exhausted cycle %d: attempts changed: %+v", cycle, got)
		}
		assertUnchanged("exhausted")
	}
	limiter.mu.Lock()
	probes := limiter.probes
	limiter.mu.Unlock()
	if probes == 0 {
		t.Fatal("capacity was never observed before dispatch")
	}

	// Capacity frees: dispatched exactly once, grant spent, attempt recorded.
	limiter.set("")
	if err := o.RunOnce(); err != nil {
		t.Fatal(err)
	}
	select {
	case call := <-dispatched:
		if call.pr != 386 || call.head != head || !reflect.DeepEqual(call.streams, []string{"llm-review-opus"}) {
			t.Fatalf("unexpected dispatch: %+v", call)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("freed capacity did not dispatch the queued grant")
	}
	for i := 0; i < 100; i++ {
		o.reviewProduceMu.Lock()
		inFlight := o.reviewProduceInFlight[386]
		o.reviewProduceMu.Unlock()
		if !inFlight {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	spent := grantFiles()
	if spent[grantName] != grantBefore[grantName] || len(spent) != 2 {
		t.Fatalf("grant not spent exactly once: %v", spent)
	}
	claimed := false
	for name := range spent {
		if strings.HasSuffix(name, ".claimed") {
			claimed = true
		}
		if strings.HasSuffix(name, ".launching") {
			t.Fatalf("launch marker survived settlement: %v", spent)
		}
	}
	if !claimed {
		t.Fatalf("exercised grant not consumed: %v", spent)
	}
	if got := attempts(); len(got) != 2 || !reflect.DeepEqual(got[0], history[0]) || got[1].Outcome != "held" {
		t.Fatalf("exercised attempt not recorded: %+v", got)
	}
	if store.NativeRearmQueued(cfg, scope, time.Now()) {
		t.Fatal("spent grant remains queued")
	}
	assertUnchanged("freed")

	// A later cycle must not dispatch the spent grant again.
	if err := o.RunOnce(); err != nil {
		t.Fatal(err)
	}
	noDispatch("spent")
	if got := attempts(); len(got) != 2 {
		t.Fatalf("spent grant replayed: %+v", got)
	}
}
