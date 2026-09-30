package review

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/befeast/maestro/internal/state"
	"github.com/google/uuid"
)

// AttemptStore uses existing state flock/CAS and merge semantics. Evidence files
// are private allowlisted metadata, not raw response bodies or output checkpoints.
type AttemptStore struct {
	StateDir string
	// persist is a per-instance fault-injection seam; nil uses durable state CAS.
	persist func(string, func(*state.State) error) error
}

func (s *AttemptStore) update(fn func(*state.State) error) error {
	if s.persist != nil {
		return s.persist(s.StateDir, fn)
	}
	return state.UpdateDurable(s.StateDir, fn)
}

type AttemptScope struct {
	Repo string
	PR   int
	Head string
	Lens string
}

func (s AttemptScope) key() string {
	return digest([]byte(s.Repo + "\x00" + strconv.Itoa(s.PR) + "\x00" + s.Head + "\x00" + s.Lens))
}
func (s AttemptScope) valid() bool { return s.Repo != "" && s.PR > 0 && s.Head != "" && s.Lens != "" }

var ErrReviewHeld = errors.New("review execution held; inspect durable review receipt")

func validMax(max int) bool { return max >= 1 && max <= 5 }
func effectiveMax(track state.ReviewAttemptTrack, max int) int {
	if track.MaxAttempts < max {
		return track.MaxAttempts
	}
	return max
}

func due(track state.ReviewAttemptTrack, now time.Time, max int) bool {
	if !validMax(max) || !validMax(track.MaxAttempts) || len(track.Attempts) == 0 || len(track.Attempts) >= effectiveMax(track, max) {
		return false
	}
	last := track.Attempts[len(track.Attempts)-1]
	return last.Outcome == "retry_wait" && last.RetryAt != nil && !now.Before(*last.RetryAt) && last.EvidenceSHA256 != ""
}

func (s *AttemptStore) available() error {
	if s == nil || s.StateDir == "" {
		return fmt.Errorf("review receipt store unavailable")
	}
	// A lost previously initialized state file is not permission to start again.
	if _, err := os.Stat(state.StatePath(s.StateDir)); err != nil {
		return fmt.Errorf("review state unavailable: %w", err)
	}
	return nil
}

// Check prevents status churn and rejects historical error/pending states with no
// authoritative attempt record. Claim revalidates the decision under the flock.
func (s *AttemptStore) Check(scope AttemptScope, now time.Time, max int, observed bool) error {
	if err := s.available(); err != nil {
		return err
	}
	if !scope.valid() || !validMax(max) {
		return ErrReviewHeld
	}
	st, err := state.Load(s.StateDir)
	if err != nil {
		return err
	}
	track, ok := st.ReviewAttempts[scope.key()]
	if !ok {
		if observed {
			return ErrReviewHeld
		}
		return nil
	}
	if !due(track, now, max) || !s.evidenceIntact(track.Attempts[len(track.Attempts)-1]) {
		return ErrReviewHeld
	}
	return nil
}

// Due is only a polling hint. Claim repeats the checks under the state lock.
func (s *AttemptStore) Due(scope AttemptScope, now time.Time, max int) bool {
	if s.available() != nil || !scope.valid() {
		return false
	}
	st, err := state.Load(s.StateDir)
	if err != nil {
		return false
	}
	track, ok := st.ReviewAttempts[scope.key()]
	return ok && due(track, now, max) && s.evidenceIntact(track.Attempts[len(track.Attempts)-1])
}

func (s *AttemptStore) Claim(scope AttemptScope, now time.Time, max int) (string, error) {
	if err := s.available(); err != nil {
		return "", err
	}
	if !scope.valid() || !validMax(max) {
		return "", fmt.Errorf("invalid review claim policy")
	}
	id := uuid.NewString()
	err := s.update(func(st *state.State) error {
		if err := s.available(); err != nil {
			return err
		}
		if st.ReviewAttempts == nil {
			st.ReviewAttempts = map[string]state.ReviewAttemptTrack{}
		}
		track, exists := st.ReviewAttempts[scope.key()]
		if exists {
			if !due(track, now, max) || !s.evidenceIntact(track.Attempts[len(track.Attempts)-1]) {
				return ErrReviewHeld
			}
		} else {
			track = state.ReviewAttemptTrack{Repo: scope.Repo, PR: scope.PR, Head: scope.Head, Lens: scope.Lens, MaxAttempts: max}
		}
		track.Revision++
		track.Attempts = append(track.Attempts, state.ReviewAttempt{ID: id, StartedAt: now.UTC(), Outcome: "launch_intent", Reason: "outcome_unknown", NextAction: "reconcile_before_retry"})
		st.ReviewAttempts[scope.key()] = track
		return nil
	})
	if err != nil {
		return "", err
	}
	if err := s.writeIntent(id, scope.key()); err != nil {
		return "", err
	}
	return id, nil
}

// The separately synced launch marker outlives any failed outcome rename/sync.
// Visible retry_wait state alone can never override an unresolved launch marker.
func (s *AttemptStore) writeIntent(id, scope string) error {
	dir, err := s.privateDir("review-intents", true)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.WriteString(scope); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (s *AttemptStore) intentUnresolved(id string) bool {
	if uuid.Validate(id) != nil {
		return true
	}
	dir, err := s.privateDir("review-intents", false)
	if err != nil {
		return true
	}
	_, err = os.Lstat(filepath.Join(dir, id))
	return !os.IsNotExist(err)
}
func (s *AttemptStore) finishIntent(id string) {
	dir, err := s.privateDir("review-intents", false)
	if err != nil {
		return
	}
	if err = os.Remove(filepath.Join(dir, id)); err != nil {
		return
	}
	// The outcome is already durable. A crash reintroducing an unsynced
	// removal can only restore the conservative hold, never permit replay.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

func (s *AttemptStore) evidenceDir() (string, error) { return s.privateDir("review-evidence", true) }
func (s *AttemptStore) privateDir(name string, create bool) (string, error) {
	path := filepath.Join(s.StateDir, name)
	if create {
		if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
			return "", err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("review evidence directory is not private")
	}
	return path, nil
}

func (s *AttemptStore) evidenceIntact(a state.ReviewAttempt) bool {
	if s.intentUnresolved(a.ID) {
		return false
	}
	dirInfo, err := os.Lstat(filepath.Join(s.StateDir, "review-evidence"))
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode().Perm() != 0700 {
		return false
	}
	if uuid.Validate(a.ID) != nil || a.EvidenceFile != a.ID+".json" {
		return false
	}
	path := filepath.Join(s.StateDir, "review-evidence", a.EvidenceFile)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 8192 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return false
	}
	body, err := os.ReadFile(path)
	return err == nil && digest(body) == a.EvidenceSHA256
}

func (s *AttemptStore) writeEvidence(id string, terminal *GatewayTerminalError, code string) (string, error) {
	dir, err := s.evidenceDir()
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(struct {
		AttemptID string                `json:"attempt_id"`
		Code      string                `json:"code"`
		Terminal  *GatewayTerminalError `json:"gateway,omitempty"`
	}{id, code, terminal})
	if err != nil || len(body) > 8192 {
		return "", fmt.Errorf("invalid review evidence")
	}
	path := filepath.Join(dir, id+".json")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 8192 {
			return "", fmt.Errorf("invalid existing review evidence")
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(existing, body) {
			return "", fmt.Errorf("review evidence conflict")
		}
		return digest(body), nil
	}
	if err != nil {
		return "", err
	}
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	d, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	err = d.Sync()
	d.Close()
	if err != nil {
		return "", err
	}
	return digest(body), nil
}

// Finish commits a safe outcome before publication/recovery. Any failure leaves
// the launch intent, so loss of an outcome cannot create permission to replay.
func (s *AttemptStore) Finish(scope AttemptScope, id string, now time.Time, runErr error) error {
	if !scope.valid() || uuid.Validate(id) != nil {
		return fmt.Errorf("invalid review outcome identity")
	}
	if err := s.available(); err != nil {
		return err
	}
	var terminal *GatewayTerminalError
	code := "review_completed"
	if runErr != nil {
		code = "outcome_unknown"
		if errors.As(runErr, &terminal) {
			code = terminal.Code
		}
	}
	evidence, err := s.writeEvidence(id, terminal, code)
	if err != nil {
		return err
	}
	err = s.update(func(st *state.State) error {
		track, ok := st.ReviewAttempts[scope.key()]
		if !ok || len(track.Attempts) == 0 {
			return fmt.Errorf("review claim missing")
		}
		i := len(track.Attempts) - 1
		a := track.Attempts[i]
		if a.ID != id || a.Outcome != "launch_intent" {
			return fmt.Errorf("review claim changed")
		}
		a.FinishedAt = &now
		a.Reason = code
		a.Outcome = "held"
		a.NextAction = "reconcile_before_retry"
		a.EvidenceFile = id + ".json"
		a.EvidenceSHA256 = evidence
		if runErr == nil {
			a.Outcome = "completed"
			a.NextAction = "publish_or_reconcile_review"
		}
		if terminal != nil && terminal.Terminal != nil {
			t := terminal.Terminal
			switch t.Reason {
			case "credentials_unavailable":
				a.NextAction = "repair_gateway_credentials"
			case "request_invalid":
				a.NextAction = "repair_review_request"
			case "cancelled":
				a.NextAction = "review_cancellation"
			}
			if t.StreamCommitted || t.RetryScope == "new_turn_only" {
				a.NextAction = "checkpoint_required_no_replay"
			}
			if len(track.Attempts) < track.MaxAttempts && !t.StreamCommitted && t.RetryScope == "same_request" {
				if t.Reason == "quota_cooldown" && t.RetryAt != nil && t.RetryAt.After(now) {
					at := *t.RetryAt
					a.RetryAt = &at
				}
				if t.Reason == "upstream_transient" {
					at := now.Add(time.Minute)
					if t.RetryAt != nil {
						if !t.RetryAt.After(now) {
							at = time.Time{}
						} else if t.RetryAt.After(at) {
							at = *t.RetryAt
						}
					}
					if !at.IsZero() {
						a.RetryAt = &at
					}
				}
				if a.RetryAt != nil {
					a.Outcome = "retry_wait"
					a.NextAction = "wait_for_bounded_retry"
				}
			}
		}
		track.Attempts[i] = a
		track.Revision++
		st.ReviewAttempts[scope.key()] = track
		return nil
	})
	if err != nil {
		return err
	}
	s.finishIntent(id)
	return nil
}
