package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/google/uuid"
)

func rearmFixture(t *testing.T) (*Producer, *config.Config, AttemptScope, OperatorRearmRequest, *atomic.Int32) {
	t.Helper()
	now := time.Now().UTC()
	s := newAttemptStore(t)
	p, f := httpProducer(t, s, "unused", &now, 1)
	scope := scopeFor(f)
	id, err := s.Claim(scope, now, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(scope, id, now, errors.New("request invalid")); err != nil {
		t.Fatal(err)
	}
	old := latestAttempt(t, s, scope)
	cfg := &config.Config{Repo: scope.Repo, ProjectID: "original-project", StateDir: s.StateDir, AIExecution: aiexecution.Policy{RequireVerifiedRoute: true}}
	cfg.Supervisor.NativeSessionRegistration = &config.NativeSessionRegistrationConfig{BudgetRunID: "original-budget-run"}
	proof := strings.Repeat("a", 64)
	prior := nativeReviewRearmProof
	t.Cleanup(func() { nativeReviewRearmProof = prior })
	nativeReviewRearmProof = func(dir, attempt, project, run string) (string, error) {
		if dir != filepath.Join(s.StateDir, "native-reviews") || attempt != id || project != cfg.ProjectID || run != "original-budget-run" {
			return "", errors.New("binding changed")
		}
		return proof, nil
	}
	calls := &atomic.Int32{}
	lens := &NativeClaudeLens{Stream: scope.Lens, Model: "claude-opus-5", policy: cfg.AIExecution, projectID: cfg.ProjectID, budgetRunID: "original-budget-run", complete: func(context.Context, string, string) (supervisor.NativeReviewResult, error) {
		calls.Add(1)
		return supervisor.NativeReviewResult{}, errors.New("bounded second failure")
	}}
	p.Lenses = []Lens{lens}
	p.ExecutionPolicy = cfg.AIExecution
	req := OperatorRearmRequest{PreviousAttemptID: id, EvidenceSHA256: old.EvidenceSHA256, NativeProofSHA256: proof, Actor: "operator", Reason: "request construction repaired; one more same-run review", ExpiresAt: now.Add(time.Hour)}
	return p, cfg, scope, req, calls
}

func TestNativeOperatorRearmQueuesExactlyOnceAndPreservesOldLimitAndHistory(t *testing.T) {
	p, cfg, scope, req, calls := rearmFixture(t)
	before, _ := state.Load(cfg.StateDir)
	old := before.ReviewAttempts[scope.key()]
	_ = p.ProducePR(context.Background(), scope.PR)
	if calls.Load() != 0 || p.Attempts.Due(scope, p.now(), 1) {
		t.Fatal("held review ran without operator grant")
	}
	_, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, req, p.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Attempts.Check(scope, p.now(), 5, true); err == nil {
		t.Fatal("operator allowance changed automatic retry policy")
	}
	if _, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, req, p.now()); err == nil {
		t.Fatal("duplicate allowance")
	}
	if !p.Attempts.Due(scope, p.now(), 1) {
		t.Fatal("queued operator review is not due")
	}
	if !p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
		t.Fatal("explicit queued grant is not visible to the daemon")
	}
	cfg.Supervisor.NativeSessionRegistration.BudgetRunID = "other-run"
	if p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
		t.Fatal("daemon trigger crossed budget run")
	}
	cfg.Supervisor.NativeSessionRegistration.BudgetRunID = "original-budget-run"
	_ = p.ProducePR(context.Background(), scope.PR)
	if err := state.Save(cfg.StateDir, before); err != nil {
		t.Fatal(err)
	}
	_ = p.ProducePR(context.Background(), scope.PR)
	if calls.Load() != 1 {
		t.Fatal("grant not single-use", calls.Load())
	}
	if p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
		t.Fatal("consumed grant remains queued")
	}
	after, _ := state.Load(cfg.StateDir)
	track := after.ReviewAttempts[scope.key()]
	grant, err := p.Attempts.readRearm(scope)
	if err != nil || track.MaxAttempts != 1 || len(track.Attempts) != 2 || !reflect.DeepEqual(track.Attempts[0], old.Attempts[0]) || grant.BudgetRunID != "original-budget-run" {
		t.Fatal("history/budget/limit changed")
	}
}

func TestNativeOperatorRearmCannotDispatchAdvancedPRHead(t *testing.T) {
	p, cfg, scope, req, calls := rearmFixture(t)
	if _, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, req, p.now()); err != nil {
		t.Fatal(err)
	}
	p.ExpectedHead = scope.Head
	f := p.Forge.(*fakeForge)
	f.pr.HeadSHA = "advanced-head"
	if err := p.ProducePR(context.Background(), scope.PR); err == nil || calls.Load() != 0 {
		t.Fatal("authorized dispatch crossed PR head")
	}
	if !p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
		t.Fatal("head mismatch consumed grant")
	}
}

func TestNativeOperatorRearmExpiredGrantCannotWakeOrRun(t *testing.T) {
	p, cfg, scope, req, calls := rearmFixture(t)
	if _, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, req, p.now()); err != nil {
		t.Fatal(err)
	}
	p.Now = func() time.Time { return req.ExpiresAt.Add(time.Second) }
	if p.Attempts.Due(scope, p.now(), 1) {
		t.Fatal("expired grant due")
	}
	_ = p.ProducePR(context.Background(), scope.PR)
	if calls.Load() != 0 {
		t.Fatal("expired grant ran")
	}
}

func TestNativeOperatorRearmSurvivesOldDaemonRoundTripAndPartialClaim(t *testing.T) {
	p, cfg, scope, req, calls := rearmFixture(t)
	_, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, req, p.now())
	if err != nil {
		t.Fatal(err)
	}
	before, _ := p.Attempts.readRearm(scope)
	// State retains the original schema: an old daemon's Load/Save cannot
	// remove or revive the sidecar grant and immutable consumed marker.
	st, err := state.Load(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatal(err)
	}
	after, _ := p.Attempts.readRearm(scope)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("old daemon erased grant")
	}
	// The attempt record is durable before the native call (#1233): a state
	// persistence failure while appending it leaves the fsynced launch marker
	// behind, so no native call is made, the grant stays unavailable for
	// inspection and history is unchanged. An old daemon's round trip cannot
	// revive it either.
	p.Attempts.persist = func(_ string, fn func(*state.State) error) error {
		st, _ := state.Load(cfg.StateDir)
		if err := fn(st); err != nil {
			return err
		}
		return errors.New("state fsync failed")
	}
	if err := p.ProducePR(context.Background(), scope.PR); err == nil {
		t.Fatal("partial claim reported success")
	}
	if calls.Load() != 0 {
		t.Fatal("partial claim reached the native runner", calls.Load())
	}
	if _, err := os.Lstat(filepath.Join(cfg.StateDir, "review-rearms", scope.key()+".launching")); err != nil {
		t.Fatal("partial claim lost its launch marker")
	}
	if p.Attempts.NativeRearmQueued(cfg, scope, p.now()) || p.Attempts.Due(scope, p.now(), 1) {
		t.Fatal("grant reusable after partial claim")
	}
	p.Attempts.persist = nil
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.claimAttempt(scope, p.Lenses[0], 1); err == nil {
		t.Fatal("partial claim replayed after old daemon save")
	}
	_ = p.ProducePR(context.Background(), scope.PR)
	if calls.Load() != 0 {
		t.Fatal("partial claim replayed", calls.Load())
	}
	latest, _ := state.Load(cfg.StateDir)
	if len(latest.ReviewAttempts[scope.key()].Attempts) != 1 {
		t.Fatal("failed claim changed old history")
	}
}

func TestNativeOperatorRearmRejectsDriftAndUnverifiedPrior(t *testing.T) {
	for _, mode := range []string{"attempt", "evidence", "proof", "unknown_native", "expired", "other_head", "changed_run", "corrupt_evidence"} {
		t.Run(mode, func(t *testing.T) {
			p, cfg, scope, req, calls := rearmFixture(t)
			switch mode {
			case "attempt":
				req.PreviousAttemptID = "00000000-0000-4000-8000-000000000001"
			case "evidence":
				req.EvidenceSHA256 = strings.Repeat("b", 64)
			case "proof":
				req.NativeProofSHA256 = strings.Repeat("b", 64)
			case "unknown_native":
				nativeReviewRearmProof = func(string, string, string, string) (string, error) { return "", errors.New("outcome unknown") }
			case "expired":
				req.ExpiresAt = p.now()
			case "other_head":
				scope.Head = "changed-head"
			case "changed_run":
				cfg.Supervisor.NativeSessionRegistration.BudgetRunID = "different-run"
			case "corrupt_evidence":
				if err := os.WriteFile(filepath.Join(cfg.StateDir, "review-evidence", req.PreviousAttemptID+".json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, req, p.now()); err == nil {
				t.Fatal("unsafe rearm accepted")
			}
			if calls.Load() != 0 {
				t.Fatal("authorization made model call")
			}
		})
	}
}

func TestNativeOperatorRearmClaimContendsAndRefusesNewRun(t *testing.T) {
	p, cfg, scope, req, _ := rearmFixture(t)
	_, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, req, p.now())
	if err != nil {
		t.Fatal(err)
	}
	lens := p.Lenses[0].(*NativeClaudeLens)
	lens.budgetRunID = "new-run"
	if _, _, err := p.claimAttempt(scope, lens, 1); err == nil {
		t.Fatal("grant crossed budget run")
	}
	lens.budgetRunID = "original-budget-run"
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := p.claimAttempt(scope, lens, 1); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("claim not exactly once", wins.Load())
	}
}

func nativeReviewDir(t *testing.T, stateDir string) string {
	t.Helper()
	dir := filepath.Join(stateDir, "native-reviews", "supervisor-consultations")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeNativeReviewFile(t *testing.T, dir, name string, value any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
		t.Fatal(err)
	}
}

// nativeRunnerConfig is the configured project as supervisor.CompleteNativeReview
// sees it: the review store's state dir, the test's limiter as the auxiliary
// capacity owner and a claude-kind backend that is never started because the
// reservation is refused first.
func nativeRunnerConfig(cfg *config.Config, limiter aiexecution.AuxiliaryLimiter) *config.Config {
	runner := *cfg
	runner.RuntimeAuxiliaryLimiter = limiter
	runner.Model = config.ModelConfig{Default: "native", Backends: map[string]config.BackendDef{"native": {Cmd: "/bin/false", Provider: "claude", Model: "claude-opus-5"}}}
	return &runner
}

// preLaunchReceipt is the closed pre-launch shape for the modes that simulate
// a run which reached a launch (marker or invocation added by the caller).
func preLaunchReceipt(id, projectID string, now time.Time) supervisor.ConsultationReceipt {
	ended := now
	return supervisor.ConsultationReceipt{
		SchemaVersion: 1,
		Identity:      supervisor.ConsultationIdentity{ID: id, ProjectID: projectID, CycleID: id, Role: "reviewer"},
		StartedAt:     now,
		EndedAt:       &ended,
		Status:        "failed",
		Candidates:    []supervisor.CandidateReceipt{},
		Invocations:   []supervisor.InvocationReceipt{},
	}
}

// fakeAuxiliaryLimiter stands in for the daemon's auxiliary capacity owner:
// code "" grants a reservation, anything else is the typed hold it reports.
type fakeAuxiliaryLimiter struct {
	mu       sync.Mutex
	code     string
	reserves int
}

func (f *fakeAuxiliaryLimiter) set(code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.code = code
}

func (f *fakeAuxiliaryLimiter) ReserveAuxiliary(_, id string) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserves++
	if uuid.Validate(id) != nil {
		return nil, aiexecution.Held("auxiliary_controller_unavailable")
	}
	if f.code != "" {
		return nil, aiexecution.Held(f.code)
	}
	return func() {}, nil
}

func rearmSidecar(t *testing.T, stateDir string, scope AttemptScope, suffix string) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(stateDir, "review-rearms", scope.key()+suffix))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// #1233: a typed hold before any native launch (auxiliary capacity exhausted,
// registration refused) proven from the durable consultation receipt store
// leaves the operator grant unclaimed and history unchanged, and the retained
// grant is not re-exercised while the preflight still observes the same
// reason; it is exercised exactly once after capacity frees. A run that
// reached (or may have reached) a launch, an opaque error, or a hold the store
// cannot prove (the store did not exist) spends the grant.
func TestNativeOperatorRearmPreLaunchHoldLeavesGrantUnclaimed(t *testing.T) {
	for _, mode := range []string{"prelaunch_receipt", "store_without_receipt", "no_receipt_store", "launch_marker", "invocation_receipt", "opaque_error"} {
		t.Run(mode, func(t *testing.T) {
			p, cfg, scope, req, _ := rearmFixture(t)
			if _, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, req, p.now()); err != nil {
				t.Fatal(err)
			}
			before, _ := state.Load(cfg.StateDir)
			oldTrack := before.ReviewAttempts[scope.key()]
			limiter := &fakeAuxiliaryLimiter{}
			cfg.RuntimeAuxiliaryLimiter = limiter
			var calls atomic.Int32
			var claimed string
			lens := p.Lenses[0].(*NativeClaudeLens)
			lens.limiter = limiter
			lens.complete = func(ctx context.Context, prompt string, id string) (supervisor.NativeReviewResult, error) {
				calls.Add(1)
				claimed = id
				switch mode {
				case "prelaunch_receipt":
					// The real native runner, refused by the limiter after the
					// claim's preflight saw capacity: the receipt it leaves is
					// the proof the settlement reads (#1233 review), not a
					// fixture.
					limiter.set("auxiliary_capacity_exhausted")
					return supervisor.CompleteNativeReview(ctx, nativeRunnerConfig(cfg, limiter), "claude-opus-5", id, prompt)
				case "store_without_receipt":
					nativeReviewDir(t, cfg.StateDir)
				case "launch_marker":
					dir := nativeReviewDir(t, cfg.StateDir)
					writeNativeReviewFile(t, dir, "current.json", preLaunchReceipt(id, cfg.ProjectID, p.now()))
					writeNativeReviewFile(t, dir, "launch.json", map[string]any{"identity": supervisor.ConsultationIdentity{ID: id, ProjectID: cfg.ProjectID, CycleID: id, Role: "reviewer"}, "intent_id": uuid.NewString()})
				case "invocation_receipt":
					r := preLaunchReceipt(id, cfg.ProjectID, p.now())
					r.Invocations = []supervisor.InvocationReceipt{{ID: uuid.NewString(), Number: 1, Status: "failed"}}
					writeNativeReviewFile(t, nativeReviewDir(t, cfg.StateDir), "current.json", r)
				case "opaque_error":
					return supervisor.NativeReviewResult{}, errors.New("opaque CLI exit")
				}
				// Capacity was taken between the preflight probe and the
				// runner's own reservation.
				limiter.set("auxiliary_capacity_exhausted")
				return supervisor.NativeReviewResult{}, aiexecution.Held("auxiliary_capacity_exhausted")
			}
			if err := p.ProducePR(context.Background(), scope.PR); err == nil {
				t.Fatal("held run reported success")
			}
			if calls.Load() != 1 || uuid.Validate(claimed) != nil {
				t.Fatalf("native runner calls = %d claim %q", calls.Load(), claimed)
			}
			if p.Attempts.intentUnresolved(claimed) {
				t.Fatal("settled attempt left an unresolved intent file")
			}
			after, _ := state.Load(cfg.StateDir)
			track := after.ReviewAttempts[scope.key()]
			if rearmSidecar(t, cfg.StateDir, scope, ".launching") {
				t.Fatal("launch marker survived settlement")
			}
			preLaunch := mode == "prelaunch_receipt" || mode == "store_without_receipt"
			if !preLaunch {
				if !rearmSidecar(t, cfg.StateDir, scope, ".claimed") {
					t.Fatal("possible launch did not consume the grant")
				}
				if len(track.Attempts) != 2 || track.Attempts[1].ID != claimed || track.Attempts[1].Outcome != "held" || !reflect.DeepEqual(track.Attempts[0], oldTrack.Attempts[0]) {
					t.Fatalf("launched attempt not recorded as held: %+v", track.Attempts)
				}
				if p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
					t.Fatal("consumed grant remains queued")
				}
				return
			}
			if !reflect.DeepEqual(track.Attempts, oldTrack.Attempts) || track.MaxAttempts != oldTrack.MaxAttempts {
				t.Fatalf("pre-launch hold changed history: %+v", track)
			}
			if rearmSidecar(t, cfg.StateDir, scope, ".claimed") {
				t.Fatal("pre-launch hold consumed the grant")
			}
			held, err := p.Attempts.readRearmHold(filepath.Join(cfg.StateDir, "review-rearms"), scope)
			if err != nil || held.AttemptID != claimed || held.Code != "auxiliary_capacity_exhausted" || held.Observed != held.Code || held.Retractions != 1 {
				t.Fatalf("hold record = %+v err=%v", held, err)
			}
			// Bounded (#1233 review): while the preflight observes the same
			// reason the grant is neither hinted nor exercised, and no
			// consultation is opened.
			if p.Attempts.NativeRearmQueued(cfg, scope, p.now()) || p.Attempts.Due(scope, p.now(), 1) {
				t.Fatal("retained grant dispatched while capacity is still exhausted")
			}
			lens.complete = func(context.Context, string, string) (supervisor.NativeReviewResult, error) {
				calls.Add(1)
				return supervisor.NativeReviewResult{}, errors.New("bounded second failure")
			}
			_ = p.ProducePR(context.Background(), scope.PR)
			if calls.Load() != 1 || rearmSidecar(t, cfg.StateDir, scope, ".launching") {
				t.Fatalf("retained grant re-exercised under the same hold reason: calls=%d", calls.Load())
			}
			// Capacity frees: exercised exactly once, then consumed.
			limiter.set("")
			if !p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
				t.Fatal("retained grant not queued after capacity freed")
			}
			_ = p.ProducePR(context.Background(), scope.PR)
			final, _ := state.Load(cfg.StateDir)
			if calls.Load() != 2 || len(final.ReviewAttempts[scope.key()].Attempts) != 2 || p.Attempts.NativeRearmQueued(cfg, scope, p.now()) || !rearmSidecar(t, cfg.StateDir, scope, ".claimed") {
				t.Fatalf("retained grant not exercised exactly once: calls=%d attempts=%d", calls.Load(), len(final.ReviewAttempts[scope.key()].Attempts))
			}
		})
	}
}

// #1233 review: a claim the native runner never received (the lens refused it,
// the execution policy changed, the transport was unsupported) is proven
// pre-launch independently of the consultation store, which need not exist.
func TestNativeOperatorRearmRefusalBeforeRunnerRetractsAttempt(t *testing.T) {
	p, cfg, scope, req, calls := rearmFixture(t)
	if _, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, req, p.now()); err != nil {
		t.Fatal(err)
	}
	before, _ := state.Load(cfg.StateDir)
	lens := p.Lenses[0].(*NativeClaudeLens)
	id, grant, err := p.claimAttempt(scope, lens, 1)
	if err != nil || grant == "" {
		t.Fatal(err)
	}
	mid, _ := state.Load(cfg.StateDir)
	if attempts := mid.ReviewAttempts[scope.key()].Attempts; len(attempts) != 2 || attempts[1].ID != id || attempts[1].Outcome != "launch_intent" || !p.Attempts.intentUnresolved(id) {
		t.Fatalf("claim did not record a durable attempt before the native call: %+v", attempts)
	}
	lens.Model = ""
	pr := p.Forge.(*fakeForge).pr
	if err := p.runLensClaimed(context.Background(), lens, pr, "prompt", "", id, grant); err == nil {
		t.Fatal("refused claim reported success")
	}
	if calls.Load() != 0 {
		t.Fatal("refused claim reached the native runner")
	}
	after, _ := state.Load(cfg.StateDir)
	if !reflect.DeepEqual(after.ReviewAttempts[scope.key()].Attempts, before.ReviewAttempts[scope.key()].Attempts) || p.Attempts.intentUnresolved(id) {
		t.Fatalf("refusal before the runner changed history: %+v", after.ReviewAttempts[scope.key()].Attempts)
	}
	if rearmSidecar(t, cfg.StateDir, scope, ".launching") || rearmSidecar(t, cfg.StateDir, scope, ".claimed") {
		t.Fatal("refusal before the runner spent or blocked the grant")
	}
	held, err := p.Attempts.readRearmHold(filepath.Join(cfg.StateDir, "review-rearms"), scope)
	if err != nil || held.AttemptID != id || held.Code != "native_reviewer_unconfigured" || held.Observed != held.Code {
		t.Fatalf("hold record = %+v err=%v", held, err)
	}
	if _, _, err := p.claimAttempt(scope, lens, 1); err == nil {
		t.Fatal("retained grant exercised while the lens still refuses")
	}
	lens.Model = "claude-opus-5"
	_ = p.ProducePR(context.Background(), scope.PR)
	final, _ := state.Load(cfg.StateDir)
	if calls.Load() != 1 || len(final.ReviewAttempts[scope.key()].Attempts) != 2 || !rearmSidecar(t, cfg.StateDir, scope, ".claimed") {
		t.Fatalf("retained grant not exercised once the refusal cleared: calls=%d", calls.Load())
	}
}

// #1233 review: the attempt record survives a daemon restart between the
// launch marker and settlement. Before the retraction decision the grant is
// unavailable and the launch_intent record stays; after the decision the
// interrupted rollback is completed by the next claim.
func TestNativeOperatorRearmCrashBetweenClaimAndSettlement(t *testing.T) {
	for _, mode := range []string{"before_decision", "after_decision"} {
		t.Run(mode, func(t *testing.T) {
			p, cfg, scope, req, calls := rearmFixture(t)
			grant, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, req, p.now())
			if err != nil {
				t.Fatal(err)
			}
			before, _ := state.Load(cfg.StateDir)
			lens := p.Lenses[0].(*NativeClaudeLens)
			id, _, err := p.claimAttempt(scope, lens, 1)
			if err != nil {
				t.Fatal(err)
			}
			// Restart: nothing settles the claim.
			if mode == "after_decision" {
				hold := operatorRearmHold{GrantID: grant, AttemptID: id, Code: "auxiliary_capacity_exhausted", Observed: "auxiliary_capacity_exhausted", Retractions: 1, HeldAt: p.now().UTC()}
				if err := writeRearmReplace(filepath.Join(cfg.StateDir, "review-rearms", scope.key()+".held"), hold); err != nil {
					t.Fatal(err)
				}
			}
			restarted, _ := state.Load(cfg.StateDir)
			if attempts := restarted.ReviewAttempts[scope.key()].Attempts; len(attempts) != 2 || attempts[1].ID != id || attempts[1].Outcome != "launch_intent" {
				t.Fatalf("attempt record lost across restart: %+v", attempts)
			}
			if mode == "before_decision" {
				if p.Attempts.NativeRearmQueued(cfg, scope, p.now()) || p.Attempts.Due(scope, p.now(), 1) {
					t.Fatal("uncertain launch offered the grant again")
				}
				_ = p.ProducePR(context.Background(), scope.PR)
				after, _ := state.Load(cfg.StateDir)
				if calls.Load() != 0 || !rearmSidecar(t, cfg.StateDir, scope, ".launching") || len(after.ReviewAttempts[scope.key()].Attempts) != 2 {
					t.Fatalf("uncertain launch replayed or erased: calls=%d", calls.Load())
				}
				return
			}
			if !p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
				t.Fatal("decided retraction keeps the grant unavailable")
			}
			_ = p.ProducePR(context.Background(), scope.PR)
			after, _ := state.Load(cfg.StateDir)
			track := after.ReviewAttempts[scope.key()]
			if calls.Load() != 1 || len(track.Attempts) != 2 || track.Attempts[1].ID == id || !reflect.DeepEqual(track.Attempts[0], before.ReviewAttempts[scope.key()].Attempts[0]) || p.Attempts.intentUnresolved(id) || !rearmSidecar(t, cfg.StateDir, scope, ".claimed") {
				t.Fatalf("interrupted retraction not completed before the next exercise: calls=%d attempts=%+v", calls.Load(), track.Attempts)
			}
		})
	}
}

// #1233 review: a hold reason the preflight cannot reproduce gets at most one
// blind retry; afterwards the grant stays retained until the operator's
// explicit re-authorization replaces it (archived, never deleted).
func TestNativeOperatorRearmBoundsBlindRetryUntilOperatorReissue(t *testing.T) {
	p, cfg, scope, req, _ := rearmFixture(t)
	first, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, req, p.now())
	if err != nil {
		t.Fatal(err)
	}
	before, _ := state.Load(cfg.StateDir)
	var calls atomic.Int32
	lens := p.Lenses[0].(*NativeClaudeLens)
	lens.complete = func(_ context.Context, _ string, id string) (supervisor.NativeReviewResult, error) {
		calls.Add(1)
		nativeReviewDir(t, cfg.StateDir)
		return supervisor.NativeReviewResult{}, &supervisor.ConsultationHold{Code: "receipt_persistence_failed"}
	}
	rearmDir := filepath.Join(cfg.StateDir, "review-rearms")
	for cycle := 1; cycle <= 2; cycle++ {
		if !p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
			t.Fatalf("cycle %d: grant not queued", cycle)
		}
		_ = p.ProducePR(context.Background(), scope.PR)
		held, err := p.Attempts.readRearmHold(rearmDir, scope)
		if err != nil || held.Retractions != cycle || held.Code != "receipt_persistence_failed" || held.Observed != "" {
			t.Fatalf("cycle %d: hold record = %+v err=%v", cycle, held, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("blind retry count", calls.Load())
	}
	if p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
		t.Fatal("unobservable hold re-exercised beyond one blind retry")
	}
	_ = p.ProducePR(context.Background(), scope.PR)
	mid, _ := state.Load(cfg.StateDir)
	if calls.Load() != 2 || !reflect.DeepEqual(mid.ReviewAttempts[scope.key()].Attempts, before.ReviewAttempts[scope.key()].Attempts) {
		t.Fatalf("retained grant kept opening consultations: calls=%d", calls.Load())
	}
	second, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, req, p.now())
	if err != nil || second == first {
		t.Fatalf("operator re-issue of a retained grant refused: %v", err)
	}
	for _, archived := range []string{scope.key() + "." + first + ".retained.json", scope.key() + "." + first + ".retained.held"} {
		if _, err := os.Lstat(filepath.Join(rearmDir, archived)); err != nil {
			t.Fatalf("retained grant not archived: %s: %v", archived, err)
		}
	}
	if _, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, req, p.now()); err == nil {
		t.Fatal("duplicate allowance while the re-issued grant is fresh")
	}
	lens.complete = func(context.Context, string, string) (supervisor.NativeReviewResult, error) {
		calls.Add(1)
		return supervisor.NativeReviewResult{}, errors.New("bounded second failure")
	}
	if !p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
		t.Fatal("re-issued grant not queued")
	}
	_ = p.ProducePR(context.Background(), scope.PR)
	final, _ := state.Load(cfg.StateDir)
	if calls.Load() != 3 || len(final.ReviewAttempts[scope.key()].Attempts) != 2 || p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
		t.Fatalf("re-issued grant not exercised exactly once: calls=%d attempts=%d", calls.Load(), len(final.ReviewAttempts[scope.key()].Attempts))
	}
}

// #1233: a grant already spent by a pre-launch-held attempt (recorded before
// this fix) can be re-issued with the pre_launch_hold proof naming that attempt
// as the previous one. History is preserved; the spent grant is archived.
func TestNativeOperatorRearmPreLaunchHoldProofReissuesSpentGrant(t *testing.T) {
	p, cfg, scope, req, calls := rearmFixture(t)
	firstGrant, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, req, p.now())
	if err != nil {
		t.Fatal(err)
	}
	// Legacy spend: claim marker + held attempt with evidence, zero invocations.
	heldID := uuid.NewString()
	finished := p.now().UTC()
	evidence, err := p.Attempts.writeEvidence(heldID, nil, "outcome_unknown")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Attempts.update(func(st *state.State) error {
		track := st.ReviewAttempts[scope.key()]
		track.Attempts = append(track.Attempts, state.ReviewAttempt{ID: heldID, StartedAt: finished, FinishedAt: &finished, Outcome: "held", Reason: "outcome_unknown", NextAction: "reconcile_before_retry", EvidenceFile: heldID + ".json", EvidenceSHA256: evidence})
		track.Revision++
		st.ReviewAttempts[scope.key()] = track
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rearmDir := filepath.Join(cfg.StateDir, "review-rearms")
	if err := writeRearmExclusive(filepath.Join(rearmDir, scope.key()+".claimed"), operatorRearmClaim{firstGrant, heldID, finished}); err != nil {
		t.Fatal(err)
	}
	nativeDir := nativeReviewDir(t, cfg.StateDir)
	writeNativeReviewFile(t, nativeDir, heldID+".json", preLaunchReceipt(heldID, cfg.ProjectID, finished))
	if p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
		t.Fatal("spent grant still queued")
	}
	proof, err := NativeReviewPreLaunchHoldProof(filepath.Join(cfg.StateDir, "native-reviews"), heldID, cfg.ProjectID, "original-budget-run")
	if err != nil {
		t.Fatal(err)
	}
	held := latestAttempt(t, p.Attempts, scope)
	reissue := OperatorRearmRequest{PreviousAttemptID: heldID, EvidenceSHA256: held.EvidenceSHA256, NativeProofSHA256: proof, NativeProofKind: RearmProofPreLaunchHold, Actor: "operator", Reason: "capacity restored; the authorized review never launched", ExpiresAt: p.now().Add(time.Hour)}

	// Refused: default proof kind cannot supersede, a different previous
	// attempt cannot supersede, and a wrong proof is rejected.
	for name, bad := range map[string]OperatorRearmRequest{
		"native_kind":   {PreviousAttemptID: heldID, EvidenceSHA256: held.EvidenceSHA256, NativeProofSHA256: proof, Actor: "operator", Reason: "r", ExpiresAt: p.now().Add(time.Hour)},
		"other_attempt": func() OperatorRearmRequest { r := reissue; r.PreviousAttemptID = req.PreviousAttemptID; return r }(),
		"wrong_proof":   func() OperatorRearmRequest { r := reissue; r.NativeProofSHA256 = strings.Repeat("c", 64); return r }(),
	} {
		if _, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, bad, p.now()); err == nil {
			t.Fatalf("%s: unsafe re-issue accepted", name)
		}
	}
	// Refused: the receipt shows an invocation, so the attempt is not pre-launch.
	launched := preLaunchReceipt(heldID, cfg.ProjectID, finished)
	launched.Invocations = []supervisor.InvocationReceipt{{ID: uuid.NewString(), Number: 1, Status: "failed"}}
	writeNativeReviewFile(t, nativeDir, heldID+".json", launched)
	if _, err := NativeReviewPreLaunchHoldProof(filepath.Join(cfg.StateDir, "native-reviews"), heldID, cfg.ProjectID, "original-budget-run"); err == nil {
		t.Fatal("invocation receipt produced a pre-launch proof")
	}
	if _, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, reissue, p.now()); err == nil {
		t.Fatal("launched attempt accepted as pre-launch")
	}
	writeNativeReviewFile(t, nativeDir, heldID+".json", preLaunchReceipt(heldID, cfg.ProjectID, finished))

	secondGrant, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, reissue, p.now())
	if err != nil {
		t.Fatal(err)
	}
	if secondGrant == firstGrant {
		t.Fatal("re-issue reused grant identity")
	}
	for _, archived := range []string{scope.key() + "." + heldID + ".spent.json", scope.key() + "." + heldID + ".spent.claimed"} {
		if _, err := os.Lstat(filepath.Join(rearmDir, archived)); err != nil {
			t.Fatalf("spent grant not archived: %s: %v", archived, err)
		}
	}
	if !p.Attempts.NativeRearmQueued(cfg, scope, p.now()) || !p.Attempts.Due(scope, p.now(), 1) {
		t.Fatal("re-issued grant not queued")
	}
	if _, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, reissue, p.now()); err == nil {
		t.Fatal("duplicate re-issue accepted while grant unspent")
	}
	before, _ := state.Load(cfg.StateDir)
	_ = p.ProducePR(context.Background(), scope.PR)
	after, _ := state.Load(cfg.StateDir)
	track := after.ReviewAttempts[scope.key()]
	if calls.Load() != 1 || len(track.Attempts) != 3 || !reflect.DeepEqual(track.Attempts[:2], before.ReviewAttempts[scope.key()].Attempts) || track.MaxAttempts != 1 {
		t.Fatalf("re-issued grant not exercised exactly once with history preserved: calls=%d attempts=%d", calls.Load(), len(track.Attempts))
	}
	if p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
		t.Fatal("exercised re-issued grant remains queued")
	}
}
