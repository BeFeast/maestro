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
	// The grant is consumed when the native run settles (#1233). A state
	// persistence failure at that point happens after the consumed marker was
	// fsynced: the run is reported failed, history is unchanged, and the grant
	// can never authorize another physical send.
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
	if calls.Load() != 1 {
		t.Fatal("partial claim did not run exactly once", calls.Load())
	}
	if p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
		t.Fatal("grant reusable after launched attempt with failed persistence")
	}
	p.Attempts.persist = nil
	if err := state.Save(cfg.StateDir, st); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.claimAttempt(scope, p.Lenses[0], 1); err == nil {
		t.Fatal("partial claim replayed after old daemon save")
	}
	_ = p.ProducePR(context.Background(), scope.PR)
	if calls.Load() != 1 {
		t.Fatal("consumed grant replayed", calls.Load())
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

// #1233: a typed hold before any native launch (auxiliary capacity exhausted,
// registration refused) must leave the operator grant unclaimed and append no
// attempt; only a run that reached (or may have reached) a launch spends it.
func TestNativeOperatorRearmPreLaunchHoldLeavesGrantUnclaimed(t *testing.T) {
	for _, mode := range []string{"no_receipt_store", "prelaunch_receipt", "launch_marker", "invocation_receipt", "opaque_error"} {
		t.Run(mode, func(t *testing.T) {
			p, cfg, scope, req, _ := rearmFixture(t)
			if _, err := p.Attempts.AuthorizeNativeRearm(cfg, scope, req, p.now()); err != nil {
				t.Fatal(err)
			}
			before, _ := state.Load(cfg.StateDir)
			oldTrack := before.ReviewAttempts[scope.key()]
			var calls atomic.Int32
			var claimed string
			lens := p.Lenses[0].(*NativeClaudeLens)
			lens.complete = func(_ context.Context, _ string, id string) (string, error) {
				calls.Add(1)
				claimed = id
				switch mode {
				case "prelaunch_receipt":
					writeNativeReviewFile(t, nativeReviewDir(t, cfg.StateDir), "current.json", preLaunchReceipt(id, cfg.ProjectID, p.now()))
				case "launch_marker":
					dir := nativeReviewDir(t, cfg.StateDir)
					writeNativeReviewFile(t, dir, "current.json", preLaunchReceipt(id, cfg.ProjectID, p.now()))
					writeNativeReviewFile(t, dir, "launch.json", map[string]any{"identity": supervisor.ConsultationIdentity{ID: id, ProjectID: cfg.ProjectID, CycleID: id, Role: "reviewer"}, "intent_id": uuid.NewString()})
				case "invocation_receipt":
					r := preLaunchReceipt(id, cfg.ProjectID, p.now())
					r.Invocations = []supervisor.InvocationReceipt{{ID: uuid.NewString(), Number: 1, Status: "failed"}}
					writeNativeReviewFile(t, nativeReviewDir(t, cfg.StateDir), "current.json", r)
				case "opaque_error":
					return "", errors.New("opaque CLI exit")
				}
				return "", aiexecution.Held("auxiliary_capacity_exhausted")
			}
			if err := p.ProducePR(context.Background(), scope.PR); err == nil {
				t.Fatal("held run reported success")
			}
			if calls.Load() != 1 || uuid.Validate(claimed) != nil {
				t.Fatalf("native runner calls = %d claim %q", calls.Load(), claimed)
			}
			after, _ := state.Load(cfg.StateDir)
			track := after.ReviewAttempts[scope.key()]
			_, claimedErr := os.Lstat(filepath.Join(cfg.StateDir, "review-rearms", scope.key()+".claimed"))
			_, launchingErr := os.Lstat(filepath.Join(cfg.StateDir, "review-rearms", scope.key()+".launching"))
			if !errors.Is(launchingErr, os.ErrNotExist) {
				t.Fatal("launch marker survived settlement")
			}
			preLaunch := mode == "no_receipt_store" || mode == "prelaunch_receipt"
			if preLaunch {
				if !reflect.DeepEqual(track, oldTrack) {
					t.Fatalf("pre-launch hold changed history: %+v", track)
				}
				if !errors.Is(claimedErr, os.ErrNotExist) {
					t.Fatal("pre-launch hold consumed the grant")
				}
				if !p.Attempts.NativeRearmQueued(cfg, scope, p.now()) || !p.Attempts.Due(scope, p.now(), 1) {
					t.Fatal("grant no longer queued after pre-launch hold")
				}
				// The retained grant is exercised once capacity is available.
				lens.complete = func(context.Context, string, string) (string, error) {
					calls.Add(1)
					return "", errors.New("bounded second failure")
				}
				_ = p.ProducePR(context.Background(), scope.PR)
				final, _ := state.Load(cfg.StateDir)
				if calls.Load() != 2 || len(final.ReviewAttempts[scope.key()].Attempts) != 2 || p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
					t.Fatalf("retained grant not exercised exactly once: calls=%d attempts=%d", calls.Load(), len(final.ReviewAttempts[scope.key()].Attempts))
				}
				return
			}
			if claimedErr != nil {
				t.Fatal("possible launch did not consume the grant")
			}
			if len(track.Attempts) != 2 || track.Attempts[1].ID != claimed || track.Attempts[1].Outcome != "held" || !reflect.DeepEqual(track.Attempts[0], oldTrack.Attempts[0]) {
				t.Fatalf("launched attempt not recorded as held: %+v", track.Attempts)
			}
			if p.Attempts.NativeRearmQueued(cfg, scope, p.now()) {
				t.Fatal("consumed grant remains queued")
			}
		})
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
