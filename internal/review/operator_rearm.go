package review

import (
	"encoding/json"
	"errors"
	"fmt"
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
	// NativeProofSHA256 pins the previous attempt's native receipt. Its meaning
	// depends on NativeProofKind: the default (empty) is the verified native
	// outcome digest (supervisor.NativeReviewRearmProof); RearmProofPreLaunchHold
	// is NativeReviewPreLaunchHoldProof for an attempt that was held before any
	// native launch and therefore made zero physical requests (#1233).
	NativeProofSHA256 string
	NativeProofKind   string `json:"native_proof_kind,omitempty"`
	Actor             string
	Reason            string
	ExpiresAt         time.Time
}

func validRearmProofKind(kind string) bool {
	return kind == "" || kind == RearmProofPreLaunchHold
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

// operatorRearmLaunch is the durable record that a producer is exercising the
// grant with one attempt identity. It exists from claim until the native run
// settles: while present the grant cannot be exercised again, and only a proven
// pre-launch hold removes it without spending the grant. A surviving marker
// after a crash means the launch is uncertain and requires inspection.
type operatorRearmLaunch struct {
	GrantID   string    `json:"grant_id"`
	AttemptID string    `json:"attempt_id"`
	StartedAt time.Time `json:"started_at"`
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
	return syncRearmDir(filepath.Dir(path))
}

func syncRearmDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// readRearmFile reads one private sidecar record (grant, claim or launch
// marker) with the same ownership and size discipline as the grant itself.
func readRearmFile(path string, value any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16384 || st.Uid != uint32(os.Geteuid()) {
		return ErrReviewHeld
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if json.Unmarshal(b, value) != nil {
		return ErrReviewHeld
	}
	return nil
}

func (s *AttemptStore) readRearm(scope AttemptScope) (*operatorRearm, error) {
	dir, err := s.privateDir("review-rearms", false)
	if err != nil {
		return nil, err
	}
	var r operatorRearm
	if err := readRearmFile(filepath.Join(dir, scope.key()+".json"), &r); err != nil {
		return nil, err
	}
	if r.Scope != scope || uuid.Validate(r.ID) != nil || !validRearmProofKind(r.Request.NativeProofKind) {
		return nil, ErrReviewHeld
	}
	return &r, nil
}

func (s *AttemptStore) readRearmClaim(dir string, scope AttemptScope) (*operatorRearmClaim, error) {
	var c operatorRearmClaim
	if err := readRearmFile(filepath.Join(dir, scope.key()+".claimed"), &c); err != nil {
		return nil, err
	}
	if uuid.Validate(c.GrantID) != nil || uuid.Validate(c.AttemptID) != nil {
		return nil, ErrReviewHeld
	}
	return &c, nil
}

func (s *AttemptStore) readRearmLaunch(dir string, scope AttemptScope) (*operatorRearmLaunch, error) {
	var l operatorRearmLaunch
	if err := readRearmFile(filepath.Join(dir, scope.key()+".launching"), &l); err != nil {
		return nil, err
	}
	if uuid.Validate(l.GrantID) != nil || uuid.Validate(l.AttemptID) != nil {
		return nil, ErrReviewHeld
	}
	return &l, nil
}

// rearmProof computes the proof the request kind names for the previous
// attempt. Both proofs read durable receipts only; neither launches anything.
func (s *AttemptStore) rearmProof(kind, attemptID, projectID, runID string) (string, error) {
	dir := filepath.Join(s.StateDir, "native-reviews")
	if kind == RearmProofPreLaunchHold {
		return NativeReviewPreLaunchHoldProof(dir, attemptID, projectID, runID)
	}
	return nativeReviewRearmProof(dir, attemptID, projectID, runID)
}

// AuthorizeNativeRearm queues exactly one explicitly authorized review after
// verified normal settlement. Automatic retry limits remain unchanged.
//
// A scope normally carries at most one grant. The single exception is a grant
// that was spent by an attempt which provably never launched (NativeProofKind
// RearmProofPreLaunchHold naming that attempt as the previous one): the spent
// grant and its claim marker are archived in place, never deleted, and a new
// grant is issued so the operator's one authorized review can still happen.
func (s *AttemptStore) AuthorizeNativeRearm(cfg *config.Config, scope AttemptScope, req OperatorRearmRequest, now time.Time) (string, error) {
	if s == nil || cfg == nil || cfg.StateDir != s.StateDir || cfg.Repo != scope.Repo || !scope.valid() || cfg.Supervisor.NativeSessionRegistration == nil || !cfg.AIExecution.RequireVerifiedRoute || uuid.Validate(req.PreviousAttemptID) != nil || len(req.EvidenceSHA256) != 64 || len(req.NativeProofSHA256) != 64 || !validRearmProofKind(req.NativeProofKind) || strings.TrimSpace(req.Actor) == "" || strings.TrimSpace(req.Reason) == "" || !req.ExpiresAt.After(now) || req.ExpiresAt.After(now.Add(2*time.Hour)) {
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
	grantPath := filepath.Join(dir, scope.key()+".json")
	var spent *operatorRearmClaim
	if _, err := os.Lstat(grantPath); !errors.Is(err, os.ErrNotExist) {
		if req.NativeProofKind != RearmProofPreLaunchHold {
			return "", ErrReviewHeld
		}
		if _, err := os.Lstat(filepath.Join(dir, scope.key()+".launching")); !errors.Is(err, os.ErrNotExist) {
			return "", ErrReviewHeld
		}
		existing, err := s.readRearm(scope)
		if err != nil {
			return "", ErrReviewHeld
		}
		claim, err := s.readRearmClaim(dir, scope)
		if err != nil || claim.GrantID != existing.ID || claim.AttemptID != req.PreviousAttemptID {
			return "", ErrReviewHeld
		}
		spent = claim
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
	proof, err := s.rearmProof(req.NativeProofKind, old.ID, cfg.ProjectID, runID)
	if err != nil || proof != req.NativeProofSHA256 {
		return "", ErrReviewHeld
	}
	if spent != nil {
		// Archive, never delete: the spent grant and its claim stay auditable
		// under names the producer never reads. The claim moves first so a
		// crash can only leave an unexercisable grant, never an orphan claim.
		archive := scope.key() + "." + spent.AttemptID + ".spent"
		if err := os.Rename(filepath.Join(dir, scope.key()+".claimed"), filepath.Join(dir, archive+".claimed")); err != nil {
			return "", err
		}
		if err := os.Rename(grantPath, filepath.Join(dir, archive+".json")); err != nil {
			return "", err
		}
		if err := syncRearmDir(dir); err != nil {
			return "", err
		}
	}
	r := operatorRearm{ID: uuid.NewString(), Scope: scope, Request: req, ProjectID: cfg.ProjectID, BudgetRunID: runID, AuthorizedAt: now.UTC()}
	if err := writeRearmExclusive(grantPath, r); err != nil {
		return "", err
	}
	return r.ID, nil
}

func (s *AttemptStore) operatorRearmReady(scope AttemptScope, track state.ReviewAttemptTrack, id string, lens *NativeClaudeLens, now time.Time) bool {
	r, err := s.readRearm(scope)
	if err != nil || r.ID != id || !r.Request.ExpiresAt.After(now) || len(track.Attempts) == 0 || lens == nil || !lens.policy.RequireVerifiedRoute || lens.projectID != r.ProjectID || lens.budgetRunID != r.BudgetRunID {
		return false
	}
	// A consumed grant and a grant currently being exercised are both unavailable.
	for _, suffix := range []string{".claimed", ".launching"} {
		if _, err := os.Lstat(filepath.Join(s.StateDir, "review-rearms", scope.key()+suffix)); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	old := track.Attempts[len(track.Attempts)-1]
	if old.ID != r.Request.PreviousAttemptID || old.Outcome != "held" || old.EvidenceSHA256 != r.Request.EvidenceSHA256 || !s.evidenceIntact(old) {
		return false
	}
	proof, err := s.rearmProof(r.Request.NativeProofKind, old.ID, r.ProjectID, r.BudgetRunID)
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

// claimAttempt returns the attempt identity and, for an operator grant, the
// grant it exercises. An ordinary claim records its attempt immediately. A
// grant claim only writes the durable launch marker: the grant is consumed and
// the attempt appended by settleRearmAttempt once the native run has settled,
// so a typed hold before any native launch (auxiliary capacity, registration)
// leaves the operator's authorization intact and the history unchanged (#1233).
func (p *Producer) claimAttempt(scope AttemptScope, lens Lens, max int) (string, string, error) {
	grantID := p.queuedRearm(scope, lens)
	if grantID == "" {
		id, err := p.Attempts.Claim(scope, p.now(), max)
		return id, "", err
	}
	native, ok := lens.(*NativeClaudeLens)
	if !ok {
		return "", "", ErrReviewHeld
	}
	dir, unlock, err := p.Attempts.rearmLock(scope)
	if err != nil {
		return "", "", err
	}
	defer unlock()
	st, err := state.Load(p.Attempts.StateDir)
	if err != nil {
		return "", "", err
	}
	if !p.Attempts.operatorRearmReady(scope, st.ReviewAttempts[scope.key()], grantID, native, p.now()) {
		return "", "", ErrReviewHeld
	}
	id := uuid.NewString()
	// Fsync the launch marker before any native call. Exclusive creation makes
	// the claim exactly-once; while the marker exists the grant is unavailable.
	if err := writeRearmExclusive(filepath.Join(dir, scope.key()+".launching"), operatorRearmLaunch{grantID, id, p.now().UTC()}); err != nil {
		return "", "", err
	}
	return id, grantID, nil
}

// typedNativeHold reports whether the native run ended in a typed hold (never an
// opaque CLI or parse failure), returning its code.
func typedNativeHold(err error) (string, bool) {
	var hold *aiexecution.Hold
	if errors.As(err, &hold) {
		return hold.Code, true
	}
	var consultation *supervisor.ConsultationHold
	if errors.As(err, &consultation) {
		return consultation.Code, true
	}
	return "", false
}

// settleRearmAttempt decides, after the native run returned, whether the grant
// was actually exercised. recorded=false means a typed hold occurred before any
// native launch (proven from the durable native receipt store): the launch
// marker is removed, the grant stays unclaimed and no attempt is appended.
// Otherwise the grant is consumed (claim marker fsynced first) and the attempt
// is appended as launch_intent so the caller's Finish can settle its outcome.
func (p *Producer) settleRearmAttempt(scope AttemptScope, claimID, grantID, projectID string, runErr error) (recorded bool, err error) {
	if uuid.Validate(claimID) != nil || uuid.Validate(grantID) != nil {
		return false, ErrReviewHeld
	}
	dir, unlock, err := p.Attempts.rearmLock(scope)
	if err != nil {
		return false, err
	}
	defer unlock()
	launchPath := filepath.Join(dir, scope.key()+".launching")
	startedAt := p.now().UTC()
	marker, markerErr := p.Attempts.readRearmLaunch(dir, scope)
	if markerErr == nil && marker.GrantID == grantID && marker.AttemptID == claimID {
		startedAt = marker.StartedAt
		if _, typed := typedNativeHold(runErr); typed && nativeReviewPreLaunchHoldProven(filepath.Join(p.Attempts.StateDir, "native-reviews"), claimID, projectID) {
			if err := os.Remove(launchPath); err != nil {
				return false, err
			}
			if err := syncRearmDir(dir); err != nil {
				return false, err
			}
			return false, nil
		}
	}
	// Fsync consumed authority before state persistence. Failure after this
	// point requires inspection and can never authorize another physical send.
	if err := writeRearmExclusive(filepath.Join(dir, scope.key()+".claimed"), operatorRearmClaim{grantID, claimID, p.now().UTC()}); err != nil {
		return false, err
	}
	err = p.Attempts.update(func(st *state.State) error {
		track, ok := st.ReviewAttempts[scope.key()]
		if !ok || len(track.Attempts) == 0 {
			return fmt.Errorf("review rearm track missing")
		}
		for _, a := range track.Attempts {
			if a.ID == claimID {
				return fmt.Errorf("review rearm attempt already recorded")
			}
		}
		track.Attempts = append(track.Attempts, state.ReviewAttempt{ID: claimID, StartedAt: startedAt, Outcome: "launch_intent", Reason: "outcome_unknown", NextAction: "reconcile_before_retry"})
		track.Revision++
		st.ReviewAttempts[scope.key()] = track
		return nil
	})
	if err != nil {
		return false, err
	}
	if markerErr == nil {
		// The claim marker now carries the durable record; a failed removal
		// only keeps the grant conservatively unavailable, which it already is.
		if err := os.Remove(launchPath); err == nil {
			_ = syncRearmDir(dir)
		}
	}
	return true, nil
}
