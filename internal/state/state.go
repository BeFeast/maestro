package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type SessionStatus string

const (
	StatusQueued         SessionStatus = "queued"
	StatusRunning        SessionStatus = "running"
	StatusPROpen         SessionStatus = "pr_open"
	StatusDone           SessionStatus = "done"
	StatusFailed         SessionStatus = "failed"
	StatusConflictFailed SessionStatus = "conflict_failed"
	StatusDead           SessionStatus = "dead"
	StatusRetryExhausted SessionStatus = "retry_exhausted" // max retries reached, needs manual review
)

// Phase represents which pipeline phase a session is currently in.
type Phase string

const (
	PhaseNone      Phase = ""          // legacy single-phase mode (no pipeline)
	PhaseResearch  Phase = "research"  // research: scans codebase, writes context file
	PhasePlan      Phase = "plan"      // planner: creates MAESTRO_PLAN.md + VALIDATION.md
	PhaseImplement Phase = "implement" // implementer: writes code based on plan
	PhaseValidate  Phase = "validate"  // validator: checks assertions, gates PR creation
)

type Session struct {
	IssueNumber         int           `json:"issue_number"`
	IssueTitle          string        `json:"issue_title"`
	Worktree            string        `json:"worktree"`
	Branch              string        `json:"branch"`
	PID                 int           `json:"pid"`
	TmuxSession         string        `json:"tmux_session,omitempty"`
	LogFile             string        `json:"log_file"`
	StartedAt           time.Time     `json:"started_at"`
	FinishedAt          *time.Time    `json:"finished_at,omitempty"`
	Status              SessionStatus `json:"status"`
	PRNumber            int           `json:"pr_number,omitempty"`
	Backend             string        `json:"backend,omitempty"` // "claude", "codex", etc.
	LongRunning         bool          `json:"long_running,omitempty"`
	RebaseAttempted     bool          `json:"rebase_attempted,omitempty"`
	NotifiedCIFail      bool          `json:"notified_ci_fail,omitempty"`     // deprecated: use LastNotifiedStatus
	LastNotifiedStatus  string        `json:"last_notified_status,omitempty"` // dedup: last notification type sent
	RetryCount          int           `json:"retry_count,omitempty"`
	NextRetryAt         *time.Time    `json:"next_retry_at,omitempty"`
	LastOutputHash      string        `json:"last_output_hash,omitempty"`
	LastOutputChangedAt time.Time     `json:"last_output_changed_at,omitempty"`
	TokensUsed          int           `json:"tokens_used,omitempty"`         // cumulative tokens consumed by the worker
	RateLimitHit        bool          `json:"rate_limit_hit,omitempty"`      // true if worker was rate-limited (tmux detection, running worker)
	TriedBackends       []string      `json:"tried_backends,omitempty"`      // backends already attempted (for rate-limit fallback)
	Phase               Phase         `json:"phase,omitempty"`               // current pipeline phase (empty = legacy single-phase)
	ValidationFails     int           `json:"validation_fails,omitempty"`    // number of failed validation attempts
	ValidationFeedback  string        `json:"validation_feedback,omitempty"` // feedback from last failed validation
}

type State struct {
	Sessions    map[string]*Session `json:"sessions"`
	NextSlot    int                 `json:"next_slot"`
	LastMergeAt time.Time           `json:"last_merge_at,omitempty"`
}

func NewState() *State {
	return &State{
		Sessions: make(map[string]*Session),
		NextSlot: 1,
	}
}

func StatePath(stateDir string) string {
	return filepath.Join(stateDir, "state.json")
}

func LogDir(stateDir string) string {
	return filepath.Join(stateDir, "logs")
}

func Load(stateDir string) (*State, error) {
	path := StatePath(stateDir)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return NewState(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}

	s := NewState()
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	return s, nil
}

func Save(stateDir string, s *State) error {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	path := StatePath(stateDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("atomic rename state: %w", err)
	}
	return nil
}

// NextSlotName returns "{prefix}-N" for the next available slot
func (s *State) NextSlotName(prefix string) string {
	name := fmt.Sprintf("%s-%d", prefix, s.NextSlot)
	s.NextSlot++
	return name
}

// ActiveSessions returns sessions that are currently running
func (s *State) ActiveSessions() []*Session {
	var active []*Session
	for _, sess := range s.Sessions {
		if sess.Status == StatusRunning || sess.Status == StatusPROpen {
			active = append(active, sess)
		}
	}
	return active
}

// CountByStatus returns a map of session status → count for all non-terminal sessions.
func (s *State) CountByStatus() map[SessionStatus]int {
	counts := make(map[SessionStatus]int)
	for _, sess := range s.Sessions {
		if !IsTerminal(sess.Status) {
			counts[sess.Status]++
		}
	}
	return counts
}

// IssueInProgress returns true if the given issue is already being handled.
// This includes dead sessions with a pending retry (NextRetryAt set) to prevent
// duplicate worker spawns during backoff periods.
func (s *State) IssueInProgress(issueNum int) bool {
	for _, sess := range s.Sessions {
		if sess.IssueNumber != issueNum {
			continue
		}
		if sess.Status == StatusRunning || sess.Status == StatusPROpen || sess.Status == StatusQueued {
			return true
		}
		// Dead session with pending retry — still in progress
		if sess.Status == StatusDead && sess.NextRetryAt != nil {
			return true
		}
	}
	return false
}

// FailedAttemptsForIssue counts sessions for the given issue that ended
// without producing a PR (dead, failed, or retry_exhausted).
func (s *State) FailedAttemptsForIssue(issueNum int) int {
	count := 0
	for _, sess := range s.Sessions {
		if sess.IssueNumber == issueNum && sess.PRNumber == 0 &&
			(sess.Status == StatusDead || sess.Status == StatusFailed || sess.Status == StatusRetryExhausted) {
			count++
		}
	}
	return count
}

// IssueRetryExhausted returns true if any session for the given issue
// has been marked as retry_exhausted.
func (s *State) IssueRetryExhausted(issueNum int) bool {
	for _, sess := range s.Sessions {
		if sess.IssueNumber == issueNum && sess.Status == StatusRetryExhausted {
			return true
		}
	}
	return false
}

// MarkIssueRetryExhausted transitions the most recent dead/failed session
// for the given issue to StatusRetryExhausted.
func (s *State) MarkIssueRetryExhausted(issueNum int) {
	var latest *Session
	var latestTime time.Time
	for _, sess := range s.Sessions {
		if sess.IssueNumber == issueNum &&
			(sess.Status == StatusDead || sess.Status == StatusFailed) {
			var t time.Time
			if sess.FinishedAt != nil {
				t = *sess.FinishedAt
			} else {
				t = sess.StartedAt
			}
			if latest == nil || t.After(latestTime) {
				latest = sess
				latestTime = t
			}
		}
	}
	if latest != nil {
		latest.Status = StatusRetryExhausted
	}
}

// IsTerminal returns true if the status represents a completed/dead session.
func IsTerminal(status SessionStatus) bool {
	switch status {
	case StatusDone, StatusFailed, StatusConflictFailed, StatusDead, StatusRetryExhausted:
		return true
	}
	return false
}

// CompletedSession is a Session paired with its slot name.
type CompletedSession struct {
	SlotName string
	*Session
}

// CompletedSessions returns sessions in a terminal state, sorted by FinishedAt descending.
func (s *State) CompletedSessions() []CompletedSession {
	var result []CompletedSession
	for name, sess := range s.Sessions {
		if IsTerminal(sess.Status) {
			result = append(result, CompletedSession{SlotName: name, Session: sess})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		fi, fj := result[i].FinishedAt, result[j].FinishedAt
		if fi == nil && fj == nil {
			return result[i].StartedAt.After(result[j].StartedAt)
		}
		if fi == nil {
			return false
		}
		if fj == nil {
			return true
		}
		return fi.After(*fj)
	})
	return result
}

// PruneOldSessions removes completed sessions older than maxAge.
// Returns the number of pruned sessions.
func (s *State) PruneOldSessions(maxAge time.Duration) int {
	pruned := 0
	for name, sess := range s.Sessions {
		if !IsTerminal(sess.Status) {
			continue
		}
		finished := sess.FinishedAt
		if finished == nil {
			// Fallback: use StartedAt if FinishedAt is not set
			finished = &sess.StartedAt
		}
		if time.Since(*finished) > maxAge {
			delete(s.Sessions, name)
			pruned++
		}
	}
	return pruned
}
