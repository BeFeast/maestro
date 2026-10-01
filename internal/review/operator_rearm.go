package review

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
	"github.com/befeast/maestro/internal/supervisor"
	"github.com/google/uuid"
)

var nativeReviewRearmProof = supervisor.NativeReviewRearmProof

type OperatorRearmRequest struct {
	PreviousAttemptID string
	EvidenceSHA256    string
	NativeProofSHA256 string
	Actor             string
	Reason            string
	ExpiresAt         time.Time
}

// The immutable grant and consumed marker live outside state.json so older
// daemon schemas cannot erase operator audit or make a spent grant reusable.
type operatorRearm struct {
	ID           string               `json:"id"`
	Scope        AttemptScope         `json:"scope"`
	Request      OperatorRearmRequest `json:"authorization"`
	ProjectID    string               `json:"project_id"`
	BudgetRunID  string               `json:"budget_run_id"`
	AuthorizedAt time.Time            `json:"authorized_at"`
}

type operatorRearmClaim struct {
	GrantID   string    `json:"grant_id"`
	AttemptID string    `json:"attempt_id"`
	ClaimedAt time.Time `json:"claimed_at"`
}

func (s *AttemptStore) rearmLock(scope AttemptScope) (string, func(), error) {
	dir, err := s.privateDir("review-rearms", true)
	if err != nil {
		return "", nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, scope.key()+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return "", nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return "", nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || st.Uid != uint32(os.Geteuid()) {
		f.Close()
		return "", nil, ErrReviewHeld
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return "", nil, err
	}
	return dir, func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func writeRearmExclusive(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *AttemptStore) readRearm(scope AttemptScope) (*operatorRearm, error) {
	dir, err := s.privateDir("review-rearms", false)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, scope.key()+".json")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16384 || st.Uid != uint32(os.Geteuid()) {
		return nil, ErrReviewHeld
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r operatorRearm
	if json.Unmarshal(b, &r) != nil || r.Scope != scope || uuid.Validate(r.ID) != nil {
		return nil, ErrReviewHeld
	}
	return &r, nil
}

// AuthorizeNativeRearm queues exactly one explicitly authorized review after
// verified normal settlement. Automatic retry limits remain unchanged.
func (s *AttemptStore) AuthorizeNativeRearm(cfg *config.Config, scope AttemptScope, req OperatorRearmRequest, now time.Time) (string, error) {
	if s == nil || cfg == nil || cfg.StateDir != s.StateDir || cfg.Repo != scope.Repo || !scope.valid() || cfg.Supervisor.NativeSessionRegistration == nil || !cfg.AIExecution.RequireVerifiedRoute || uuid.Validate(req.PreviousAttemptID) != nil || len(req.EvidenceSHA256) != 64 || len(req.NativeProofSHA256) != 64 || strings.TrimSpace(req.Actor) == "" || strings.TrimSpace(req.Reason) == "" || !req.ExpiresAt.After(now) || req.ExpiresAt.After(now.Add(2*time.Hour)) {
		return "", ErrReviewHeld
	}
	if err := s.available(); err != nil {
		return "", err
	}
	dir, unlock, err := s.rearmLock(scope)
	if err != nil {
		return "", err
	}
	defer unlock()
	if _, err := os.Lstat(filepath.Join(dir, scope.key()+".json")); !errors.Is(err, os.ErrNotExist) {
		return "", ErrReviewHeld
	}
	st, err := state.Load(s.StateDir)
	if err != nil {
		return "", err
	}
	track, ok := st.ReviewAttempts[scope.key()]
	if !ok || len(track.Attempts) == 0 {
		return "", ErrReviewHeld
	}
	old := track.Attempts[len(track.Attempts)-1]
	if old.ID != req.PreviousAttemptID || old.Outcome != "held" || old.FinishedAt == nil || old.EvidenceSHA256 != req.EvidenceSHA256 || !s.evidenceIntact(old) {
		return "", ErrReviewHeld
	}
	runID := cfg.Supervisor.NativeSessionRegistration.BudgetRunID
	proof, err := nativeReviewRearmProof(filepath.Join(s.StateDir, "native-reviews"), old.ID, cfg.ProjectID, runID)
	if err != nil || proof != req.NativeProofSHA256 {
		return "", ErrReviewHeld
	}
	r := operatorRearm{ID: uuid.NewString(), Scope: scope, Request: req, ProjectID: cfg.ProjectID, BudgetRunID: runID, AuthorizedAt: now.UTC()}
	if err := writeRearmExclusive(filepath.Join(dir, scope.key()+".json"), r); err != nil {
		return "", err
	}
	return r.ID, nil
}

func (s *AttemptStore) operatorRearmReady(scope AttemptScope, track state.ReviewAttemptTrack, id string, lens *NativeClaudeLens, now time.Time) bool {
	r, err := s.readRearm(scope)
	if err != nil || r.ID != id || !r.Request.ExpiresAt.After(now) || len(track.Attempts) == 0 || lens == nil || !lens.policy.RequireVerifiedRoute || lens.projectID != r.ProjectID || lens.budgetRunID != r.BudgetRunID {
		return false
	}
	if _, err := os.Lstat(filepath.Join(s.StateDir, "review-rearms", scope.key()+".claimed")); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	old := track.Attempts[len(track.Attempts)-1]
	if old.ID != r.Request.PreviousAttemptID || old.Outcome != "held" || old.EvidenceSHA256 != r.Request.EvidenceSHA256 || !s.evidenceIntact(old) {
		return false
	}
	proof, err := nativeReviewRearmProof(filepath.Join(s.StateDir, "native-reviews"), old.ID, r.ProjectID, r.BudgetRunID)
	return err == nil && proof == r.Request.NativeProofSHA256
}

// operatorRearmDue is a polling hint only. The producer rechecks current native
// project/run bindings, and the claim repeats all proof checks under both locks.
func (s *AttemptStore) operatorRearmDue(scope AttemptScope, track state.ReviewAttemptTrack, now time.Time) bool {
	r, err := s.readRearm(scope)
	if err != nil {
		return false
	}
	lens := &NativeClaudeLens{policy: aiexecution.Policy{RequireVerifiedRoute: true}, projectID: r.ProjectID, budgetRunID: r.BudgetRunID}
	return s.operatorRearmReady(scope, track, r.ID, lens, now)
}

// NativeRearmQueued reports only explicit operator authority, never an automatic
// retry. The daemon may use this before evaluating an aggregate CI error caused
// by the prior review itself. The producer still revalidates when claiming.
func (s *AttemptStore) NativeRearmQueued(cfg *config.Config, scope AttemptScope, now time.Time) bool {
	if s.available() != nil || cfg == nil || cfg.StateDir != s.StateDir || cfg.Repo != scope.Repo || !scope.valid() || cfg.Supervisor.NativeSessionRegistration == nil {
		return false
	}
	r, err := s.readRearm(scope)
	if err != nil {
		return false
	}
	st, err := state.Load(s.StateDir)
	if err != nil {
		return false
	}
	lens := &NativeClaudeLens{policy: cfg.AIExecution, projectID: cfg.ProjectID, budgetRunID: cfg.Supervisor.NativeSessionRegistration.BudgetRunID}
	return s.operatorRearmReady(scope, st.ReviewAttempts[scope.key()], r.ID, lens, now)
}

func (p *Producer) queuedRearm(scope AttemptScope, lens Lens) string {
	native, ok := lens.(*NativeClaudeLens)
	if !ok || p.Attempts.available() != nil {
		return ""
	}
	r, err := p.Attempts.readRearm(scope)
	if err != nil {
		return ""
	}
	st, err := state.Load(p.Attempts.StateDir)
	if err != nil {
		return ""
	}
	if !p.Attempts.operatorRearmReady(scope, st.ReviewAttempts[scope.key()], r.ID, native, p.now()) {
		return ""
	}
	return r.ID
}
func (p *Producer) checkAttempt(scope AttemptScope, lens Lens, max int, observed bool) error {
	if p.queuedRearm(scope, lens) != "" {
		return nil
	}
	return p.Attempts.Check(scope, p.now(), max, observed)
}
func (p *Producer) claimAttempt(scope AttemptScope, lens Lens, max int) (string, error) {
	grantID := p.queuedRearm(scope, lens)
	if grantID == "" {
		return p.Attempts.Claim(scope, p.now(), max)
	}
	native, ok := lens.(*NativeClaudeLens)
	if !ok {
		return "", ErrReviewHeld
	}
	dir, unlock, err := p.Attempts.rearmLock(scope)
	if err != nil {
		return "", err
	}
	defer unlock()
	id := uuid.NewString()
	err = p.Attempts.update(func(st *state.State) error {
		track := st.ReviewAttempts[scope.key()]
		if !p.Attempts.operatorRearmReady(scope, track, grantID, native, p.now()) {
			return ErrReviewHeld
		}
		// Fsync consumed authority before state persistence. Failure after this
		// point requires inspection and can never authorize another physical send.
		if err := writeRearmExclusive(filepath.Join(dir, scope.key()+".claimed"), operatorRearmClaim{grantID, id, p.now().UTC()}); err != nil {
			return err
		}
		track.Attempts = append(track.Attempts, state.ReviewAttempt{ID: id, StartedAt: p.now().UTC(), Outcome: "launch_intent", Reason: "outcome_unknown", NextAction: "reconcile_before_retry"})
		track.Revision++
		st.ReviewAttempts[scope.key()] = track
		return nil
	})
	if err != nil {
		return "", err
	}
	if err := p.Attempts.writeIntent(id, scope.key()); err != nil {
		return "", err
	}
	return id, nil
}
