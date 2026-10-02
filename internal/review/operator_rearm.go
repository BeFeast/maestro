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

// operatorRearmLaunch is the durable marker that a producer is exercising the
// grant with one attempt identity. It is written before the attempt itself is
// appended to the state track (launch_intent plus an unresolved intent file,
// exactly like an ordinary claim) and before any native call. While present
// the grant cannot be exercised again. A marker that survives a crash leaves
// the ordinary launch_intent attempt record in place: the launch is uncertain
// and requires inspection, never a replay.
type operatorRearmLaunch struct {
	GrantID   string    `json:"grant_id"`
	AttemptID string    `json:"attempt_id"`
	StartedAt time.Time `json:"started_at"`
}

// operatorRearmHold is the durable decision that the named attempt was held
// before any native launch and made zero physical requests (#1233). It is the
// idempotent step of a retraction: once it is fsynced, rolling the attempt
// record back and removing the launch marker can be resumed after a crash.
// Code is the hold the native run reported; Observed is what the pre-launch
// preflight (lens availability, auxiliary capacity probe) saw at that moment;
// Retractions counts consecutive retractions with the same code. The grant is
// re-exercised only when the preflight observes no hold and either reproduced
// the recorded reason (so a change is observable) or at most one blind retry
// is still allowed; otherwise it stays retained until the operator re-issues
// it or it expires.
type operatorRearmHold struct {
	GrantID     string    `json:"grant_id"`
	AttemptID   string    `json:"attempt_id"`
	Code        string    `json:"code"`
	Observed    string    `json:"observed"`
	Retractions int       `json:"retractions"`
	HeldAt      time.Time `json:"held_at"`
}

// rearmPreflight observes, without launching anything, a hold that a native
// run would report before any launch: "" means nothing observable blocks a
// run. nil means no observer is available.
type rearmPreflight func() string

// auxiliaryPreflight probes auxiliary capacity through a throwaway reservation
// that is released at once. Only the in-memory reservation set is touched; the
// real claim repeats the reservation under the native runner.
func auxiliaryPreflight(limiter aiexecution.AuxiliaryLimiter, stateDir string) string {
	if limiter == nil {
		return ""
	}
	release, err := limiter.ReserveAuxiliary(filepath.Join(stateDir, "native-reviews"), uuid.NewString())
	if err != nil {
		if code, ok := typedNativeHold(err); ok {
			return code
		}
		return "auxiliary_capacity_unobservable"
	}
	if release != nil {
		release()
	}
	return ""
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

// writeRearmReplace atomically replaces one sidecar record: the value is
// fsynced under a private temporary name and renamed into place, so a reader
// never sees a partial record and a crash leaves either the old or the new one.
func writeRearmReplace(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncRearmDir(filepath.Dir(path))
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

func (s *AttemptStore) readRearmHold(dir string, scope AttemptScope) (*operatorRearmHold, error) {
	var h operatorRearmHold
	if err := readRearmFile(filepath.Join(dir, scope.key()+".held"), &h); err != nil {
		return nil, err
	}
	if uuid.Validate(h.GrantID) != nil || uuid.Validate(h.AttemptID) != nil || h.Code == "" || h.Retractions < 1 {
		return nil, ErrReviewHeld
	}
	return &h, nil
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
// A scope normally carries at most one grant. Two exceptions let the
// operator's one authorized review still happen without a second physical
// send ever having been possible (#1233):
//   - a grant spent by an attempt which provably never launched
//     (NativeProofKind RearmProofPreLaunchHold naming that attempt as the
//     previous one): the spent grant and its claim marker are archived;
//   - a grant retained after a proven pre-launch hold (<scope>.held names
//     it): the explicit re-authorization is the operator's reason change and
//     replaces the retained grant, archiving it and its hold record.
//
// Nothing is deleted: archived records keep names the producer never reads.
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
	var retained *operatorRearmHold
	var existing *operatorRearm
	if _, err := os.Lstat(grantPath); !errors.Is(err, os.ErrNotExist) {
		if err := s.completeRearmRetraction(dir, scope); err != nil {
			return "", ErrReviewHeld
		}
		if _, err := os.Lstat(filepath.Join(dir, scope.key()+".launching")); !errors.Is(err, os.ErrNotExist) {
			return "", ErrReviewHeld
		}
		existing, err = s.readRearm(scope)
		if err != nil {
			return "", ErrReviewHeld
		}
		claim, claimErr := s.readRearmClaim(dir, scope)
		held, heldErr := s.readRearmHold(dir, scope)
		switch {
		case claimErr == nil:
			if req.NativeProofKind != RearmProofPreLaunchHold || claim.GrantID != existing.ID || claim.AttemptID != req.PreviousAttemptID {
				return "", ErrReviewHeld
			}
			spent = claim
		case errors.Is(claimErr, os.ErrNotExist) && heldErr == nil && held.GrantID == existing.ID:
			retained = held
		default:
			return "", ErrReviewHeld
		}
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
	// Archive, never delete: the superseded grant and its markers stay
	// auditable under names the producer never reads. The marker moves first
	// so a crash can only leave an unexercisable grant, never an orphan marker.
	var archive string
	switch {
	case spent != nil:
		archive = scope.key() + "." + spent.AttemptID + ".spent"
		if err := os.Rename(filepath.Join(dir, scope.key()+".claimed"), filepath.Join(dir, archive+".claimed")); err != nil {
			return "", err
		}
		if err := os.Rename(filepath.Join(dir, scope.key()+".held"), filepath.Join(dir, archive+".held")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	case retained != nil:
		archive = scope.key() + "." + existing.ID + ".retained"
		if err := os.Rename(filepath.Join(dir, scope.key()+".held"), filepath.Join(dir, archive+".held")); err != nil {
			return "", err
		}
	}
	if archive != "" {
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

// operatorRearmReady reports whether grant id may be exercised now. preflight
// nil means no observer is available: a fresh grant is still ready, a retained
// one is not (its hold reason cannot be seen to have changed).
func (s *AttemptStore) operatorRearmReady(scope AttemptScope, track state.ReviewAttemptTrack, id string, lens *NativeClaudeLens, preflight rearmPreflight, now time.Time) bool {
	r, err := s.readRearm(scope)
	if err != nil || r.ID != id || !r.Request.ExpiresAt.After(now) || len(track.Attempts) == 0 || lens == nil || !lens.policy.RequireVerifiedRoute || lens.projectID != r.ProjectID || lens.budgetRunID != r.BudgetRunID {
		return false
	}
	dir := filepath.Join(s.StateDir, "review-rearms")
	// A consumed grant is unavailable.
	if _, err := os.Lstat(filepath.Join(dir, scope.key()+".claimed")); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	held, heldErr := s.readRearmHold(dir, scope)
	if heldErr != nil && !errors.Is(heldErr, os.ErrNotExist) {
		return false
	}
	retained := heldErr == nil && held.GrantID == id
	attempts := track.Attempts
	if launch, err := s.readRearmLaunch(dir, scope); !errors.Is(err, os.ErrNotExist) {
		// An attempt is being exercised, or its marker is unreadable. Only a
		// retraction already decided for that very attempt (interrupted before
		// its rollback) lets the grant continue; the claim completes it first.
		if err != nil || !retained || launch.GrantID != id || launch.AttemptID != held.AttemptID {
			return false
		}
		if last := attempts[len(attempts)-1]; last.ID == held.AttemptID && last.Outcome == "launch_intent" {
			attempts = attempts[:len(attempts)-1]
		}
		if len(attempts) == 0 {
			return false
		}
	}
	old := attempts[len(attempts)-1]
	if old.ID != r.Request.PreviousAttemptID || old.Outcome != "held" || old.EvidenceSHA256 != r.Request.EvidenceSHA256 || !s.evidenceIntact(old) {
		return false
	}
	proof, err := s.rearmProof(r.Request.NativeProofKind, old.ID, r.ProjectID, r.BudgetRunID)
	if err != nil || proof != r.Request.NativeProofSHA256 {
		return false
	}
	if preflight == nil {
		return !retained
	}
	if preflight() != "" {
		// Something observable would hold the run before any launch: do not
		// open a consultation to learn what the preflight already knows.
		return false
	}
	// Re-exercise once per observable change of the hold reason, with at most
	// one blind retry for a reason the preflight could not reproduce.
	return !retained || held.Observed == held.Code || held.Retractions < 2
}

// operatorRearmDue is a polling hint only. The producer rechecks current native
// project/run bindings, and the claim repeats all proof checks under both locks.
// Without an observer a retained grant is never hinted as due.
func (s *AttemptStore) operatorRearmDue(scope AttemptScope, track state.ReviewAttemptTrack, now time.Time) bool {
	r, err := s.readRearm(scope)
	if err != nil {
		return false
	}
	lens := &NativeClaudeLens{policy: aiexecution.Policy{RequireVerifiedRoute: true}, projectID: r.ProjectID, budgetRunID: r.BudgetRunID}
	return s.operatorRearmReady(scope, track, r.ID, lens, nil, now)
}

// NativeRearmQueued reports only explicit operator authority, never an automatic
// retry. The daemon may use this before evaluating an aggregate CI error caused
// by the prior review itself. The producer still revalidates when claiming. The
// configured auxiliary limiter is probed so a grant retained behind
// auxiliary_capacity_exhausted is not dispatched again until capacity frees.
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
	preflight := func() string { return auxiliaryPreflight(cfg.RuntimeAuxiliaryLimiter, s.StateDir) }
	return s.operatorRearmReady(scope, st.ReviewAttempts[scope.key()], r.ID, lens, preflight, now)
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
	if !p.Attempts.operatorRearmReady(scope, st.ReviewAttempts[scope.key()], r.ID, native, native.preflight(p.Attempts.StateDir), p.now()) {
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
// grant it exercises. Both claims record the attempt durably before any native
// call (launch_intent plus an unresolved intent file). A grant claim first
// fsyncs the launch marker and consumes the grant only when settleRearmAttempt
// cannot prove that the run was held before any native launch (#1233); a
// crash in between leaves the marker and the ordinary attempt record for
// inspection.
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
	if err := p.Attempts.completeRearmRetraction(dir, scope); err != nil {
		return "", "", err
	}
	st, err := state.Load(p.Attempts.StateDir)
	if err != nil {
		return "", "", err
	}
	if !p.Attempts.operatorRearmReady(scope, st.ReviewAttempts[scope.key()], grantID, native, native.preflight(p.Attempts.StateDir), p.now()) {
		return "", "", ErrReviewHeld
	}
	id := uuid.NewString()
	now := p.now().UTC()
	// Exclusive creation makes the claim exactly-once; while the marker exists
	// the grant is unavailable.
	if err := writeRearmExclusive(filepath.Join(dir, scope.key()+".launching"), operatorRearmLaunch{grantID, id, now}); err != nil {
		return "", "", err
	}
	// The attempt record is durable before the native call. A failure here
	// leaves the marker: the grant stays unavailable for inspection and no
	// native call is made.
	err = p.Attempts.update(func(st *state.State) error {
		track, ok := st.ReviewAttempts[scope.key()]
		if !ok || len(track.Attempts) == 0 {
			return fmt.Errorf("review rearm track missing")
		}
		for _, a := range track.Attempts {
			if a.ID == id {
				return fmt.Errorf("review rearm attempt already recorded")
			}
		}
		track.Attempts = append(track.Attempts, state.ReviewAttempt{ID: id, StartedAt: now, Outcome: "launch_intent", Reason: "outcome_unknown", NextAction: "reconcile_before_retry"})
		track.Revision++
		st.ReviewAttempts[scope.key()] = track
		return nil
	})
	if err == nil {
		err = p.Attempts.writeIntent(id, scope.key())
	}
	if err != nil {
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

// rearmPreLaunchHold decides whether runErr proves that the attempt was held
// before any native launch, returning the code to record. A claim the native
// runner never received is proven independently of any store. Once the runner
// was entered only a typed hold backed by the durable consultation receipt
// store counts; an opaque error or an unreadable or absent store means the
// launch is uncertain.
func rearmPreLaunchHold(runErr error, nativeDir, attemptID, projectID string, runnerEntered bool) (string, bool) {
	if runErr == nil {
		return "", false
	}
	code, typed := typedNativeHold(runErr)
	if !runnerEntered {
		if !typed {
			code = "native_runner_not_entered"
		}
		return code, true
	}
	if !typed {
		return "", false
	}
	if code == "" {
		code = "native_hold_unspecified"
	}
	return code, nativeReviewPreLaunchHoldProven(nativeDir, attemptID, projectID)
}

// completeRearmRetraction resumes a retraction whose decision (<scope>.held)
// is durable but whose rollback was interrupted: the launch marker still names
// the retracted attempt. Idempotent; callers hold the rearm lock. Any other
// surviving marker is left for inspection.
func (s *AttemptStore) completeRearmRetraction(dir string, scope AttemptScope) error {
	launch, err := s.readRearmLaunch(dir, scope)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	held, err := s.readRearmHold(dir, scope)
	if err != nil || held.GrantID != launch.GrantID || held.AttemptID != launch.AttemptID {
		return ErrReviewHeld
	}
	return s.retractRearmAttempt(dir, scope, launch.AttemptID)
}

// retractRearmAttempt rolls back the launch_intent record of an attempt that
// provably never launched, resolves its intent file and removes the launch
// marker. It only ever removes the latest, still unsettled attempt with that
// identity; anything else is left untouched and reported.
func (s *AttemptStore) retractRearmAttempt(dir string, scope AttemptScope, attemptID string) error {
	err := s.update(func(st *state.State) error {
		track, ok := st.ReviewAttempts[scope.key()]
		if !ok || len(track.Attempts) == 0 {
			return nil
		}
		last := track.Attempts[len(track.Attempts)-1]
		if last.ID != attemptID {
			for _, a := range track.Attempts {
				if a.ID == attemptID {
					return fmt.Errorf("review rearm attempt not retractable")
				}
			}
			return nil
		}
		if last.Outcome != "launch_intent" {
			return fmt.Errorf("review rearm attempt already settled")
		}
		track.Attempts = track.Attempts[:len(track.Attempts)-1]
		track.Revision++
		st.ReviewAttempts[scope.key()] = track
		return nil
	})
	if err != nil {
		return err
	}
	s.finishIntent(attemptID)
	if err := os.Remove(filepath.Join(dir, scope.key()+".launching")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncRearmDir(dir)
}

// settleRearmAttempt decides, after the native run returned, whether the grant
// was actually exercised. recorded=false means the attempt was proven held
// before any native launch (see rearmPreLaunchHold): the decision is fsynced
// as <scope>.held, then the attempt record is rolled back and the launch
// marker removed, so the grant stays unclaimed and history is unchanged.
// Otherwise the grant is consumed (claim marker fsynced first) and the
// already recorded launch_intent attempt is left for the caller's Finish.
func (p *Producer) settleRearmAttempt(scope AttemptScope, claimID, grantID, projectID string, runErr error, runnerEntered bool, preflight rearmPreflight) (recorded bool, err error) {
	if uuid.Validate(claimID) != nil || uuid.Validate(grantID) != nil {
		return false, ErrReviewHeld
	}
	dir, unlock, err := p.Attempts.rearmLock(scope)
	if err != nil {
		return false, err
	}
	defer unlock()
	launchPath := filepath.Join(dir, scope.key()+".launching")
	marker, markerErr := p.Attempts.readRearmLaunch(dir, scope)
	if markerErr == nil && marker.GrantID == grantID && marker.AttemptID == claimID {
		if code, held := rearmPreLaunchHold(runErr, filepath.Join(p.Attempts.StateDir, "native-reviews"), claimID, projectID, runnerEntered); held {
			hold := operatorRearmHold{GrantID: grantID, AttemptID: claimID, Code: code, Retractions: 1, HeldAt: p.now().UTC()}
			if preflight != nil {
				hold.Observed = preflight()
			}
			if previous, err := p.Attempts.readRearmHold(dir, scope); err == nil && previous.GrantID == grantID && previous.Code == code {
				hold.Retractions = previous.Retractions + 1
			}
			// The decision is durable before the rollback: a crash from here
			// on resumes through completeRearmRetraction.
			if err := writeRearmReplace(filepath.Join(dir, scope.key()+".held"), hold); err != nil {
				return false, err
			}
			if err := p.Attempts.retractRearmAttempt(dir, scope, claimID); err != nil {
				return false, err
			}
			return false, nil
		}
	}
	// Fsync consumed authority before anything else. Failure after this point
	// requires inspection and can never authorize another physical send.
	if err := writeRearmExclusive(filepath.Join(dir, scope.key()+".claimed"), operatorRearmClaim{grantID, claimID, p.now().UTC()}); err != nil {
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
