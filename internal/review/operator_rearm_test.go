package review

import (
	"context"
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
	lens := &NativeClaudeLens{Stream: scope.Lens, Model: "claude-opus-5", policy: cfg.AIExecution, projectID: cfg.ProjectID, budgetRunID: "original-budget-run", complete: func(context.Context, string, string) (string, error) {
		calls.Add(1)
		return "", errors.New("bounded second failure")
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
	p, cfg, scope, req, _ := rearmFixture(t)
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
	p.Attempts.persist = func(_ string, fn func(*state.State) error) error {
		st, _ := state.Load(cfg.StateDir)
		if err := fn(st); err != nil {
			return err
		}
		return errors.New("state fsync failed")
	}
	if _, err := p.claimAttempt(scope, p.Lenses[0], 1); err == nil {
		t.Fatal("partial claim reported success")
	}
	p.Attempts.persist = nil
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatal(err)
	}
	if _, err := p.claimAttempt(scope, p.Lenses[0], 1); err == nil {
		t.Fatal("partial claim replayed after old daemon save")
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
	if _, err := p.claimAttempt(scope, lens, 1); err == nil {
		t.Fatal("grant crossed budget run")
	}
	lens.budgetRunID = "original-budget-run"
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.claimAttempt(scope, lens, 1); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("claim not exactly once", wins.Load())
	}
}
