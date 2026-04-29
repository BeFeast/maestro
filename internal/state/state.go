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
	PhasePlan      Phase = "plan"      // planner: creates MAESTRO_PLAN.md + VALIDATION.md
	PhaseImplement Phase = "implement" // implementer: writes code based on plan
	PhaseValidate  Phase = "validate"  // validator: checks assertions, gates PR creation
)

type Session struct {
	IssueNumber                 int           `json:"issue_number"`
	IssueTitle                  string        `json:"issue_title"`
	Worktree                    string        `json:"worktree"`
	Branch                      string        `json:"branch"`
	PID                         int           `json:"pid"`
	TmuxSession                 string        `json:"tmux_session,omitempty"`
	LogFile                     string        `json:"log_file"`
	StartedAt                   time.Time     `json:"started_at"`
	FinishedAt                  *time.Time    `json:"finished_at,omitempty"`
	Status                      SessionStatus `json:"status"`
	PRNumber                    int           `json:"pr_number,omitempty"`
	Backend                     string        `json:"backend,omitempty"` // "claude", "codex", etc.
	LongRunning                 bool          `json:"long_running,omitempty"`
	RebaseAttempted             bool          `json:"rebase_attempted,omitempty"`
	NotifiedCIFail              bool          `json:"notified_ci_fail,omitempty"`     // deprecated: use LastNotifiedStatus
	LastNotifiedStatus          string        `json:"last_notified_status,omitempty"` // dedup: last notification type sent
	RetryCount                  int           `json:"retry_count,omitempty"`          // per-session retry counter; the global per-issue limit (max_retries_per_issue) combines this with FailedAttemptsForIssue
	NextRetryAt                 *time.Time    `json:"next_retry_at,omitempty"`
	LastOutputHash              string        `json:"last_output_hash,omitempty"`
	LastOutputChangedAt         time.Time     `json:"last_output_changed_at,omitempty"`
	TokensUsedAttempt           int           `json:"tokens_used_attempt,omitempty"`            // tokens consumed in current attempt (reset on respawn)
	TokensUsedTotal             int           `json:"tokens_used_total,omitempty"`              // cumulative tokens across the issue lifecycle
	RateLimitHit                bool          `json:"rate_limit_hit,omitempty"`                 // true if worker was rate-limited (tmux detection, running worker)
	TriedBackends               []string      `json:"tried_backends,omitempty"`                 // backends already attempted (for rate-limit fallback)
	Phase                       Phase         `json:"phase,omitempty"`                          // current pipeline phase (empty = legacy single-phase)
	ValidationFails             int           `json:"validation_fails,omitempty"`               // number of failed validation attempts
	ValidationFeedback          string        `json:"validation_feedback,omitempty"`            // feedback from last failed validation
	CIFailureOutput             string        `json:"ci_failure_output,omitempty"`              // CI failure output captured before retry (passed to next worker as context)
	PreviousAttemptFeedback     string        `json:"previous_attempt_feedback,omitempty"`      // feedback from previous failed PR attempt
	PreviousAttemptFeedbackKind string        `json:"previous_attempt_feedback_kind,omitempty"` // review_feedback, rebase_conflict
	CheckpointFile              string        `json:"checkpoint_file,omitempty"`                // path to CHECKPOINT.md saved at soft token threshold
}

// UnmarshalJSON implements custom unmarshalling to preserve the legacy
// "tokens_used" field from older state files. Before the split into
// per-attempt and total counters, a single "tokens_used" field tracked
// cumulative token usage. When loading old state, map it to both new fields.
func (s *Session) UnmarshalJSON(data []byte) error {
	// Use an alias to avoid infinite recursion.
	type SessionAlias Session
	aux := &struct {
		*SessionAlias
		LegacyTokensUsed int `json:"tokens_used,omitempty"`
	}{
		SessionAlias: (*SessionAlias)(s),
	}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	// If legacy field is set and both new fields are zero, migrate.
	if aux.LegacyTokensUsed > 0 && s.TokensUsedAttempt == 0 && s.TokensUsedTotal == 0 {
		s.TokensUsedAttempt = aux.LegacyTokensUsed
		s.TokensUsedTotal = aux.LegacyTokensUsed
	}
	return nil
}

// Mission tracks a decomposed epic and its child issues.
type Mission struct {
	ParentIssue int    `json:"parent_issue"`
	ChildIssues []int  `json:"child_issues"`
	Status      string `json:"status"` // "active", "done"
}

const DefaultSupervisorDecisionLimit = 20

// SupervisorTarget identifies the primary object a supervisor decision refers to.
type SupervisorTarget struct {
	Issue   int    `json:"issue,omitempty"`
	PR      int    `json:"pr,omitempty"`
	Session string `json:"session,omitempty"`
}

// SupervisorProjectState captures the read-only state snapshot behind a decision.
type SupervisorProjectState struct {
	Sessions       int `json:"sessions"`
	Running        int `json:"running"`
	PROpen         int `json:"pr_open"`
	Queued         int `json:"queued"`
	RetryExhausted int `json:"retry_exhausted"`
	OpenIssues     int `json:"open_issues"`
	OpenPRs        int `json:"open_prs"`
	AvailableSlots int `json:"available_slots"`
}

// SupervisorDecision is a stable, machine-readable read-only orchestration record.
type SupervisorDecision struct {
	ID                string                 `json:"id"`
	CreatedAt         time.Time              `json:"created_at"`
	Project           string                 `json:"project"`
	Mode              string                 `json:"mode"`
	PolicyRule        string                 `json:"policy_rule,omitempty"`
	Summary           string                 `json:"summary"`
	RecommendedAction string                 `json:"recommended_action"`
	Target            *SupervisorTarget      `json:"target,omitempty"`
	Risk              string                 `json:"risk"`
	Confidence        float64                `json:"confidence"`
	Reasons           []string               `json:"reasons,omitempty"`
	ProjectState      SupervisorProjectState `json:"project_state"`
}

type State struct {
	Sessions            map[string]*Session  `json:"sessions"`
	Missions            map[int]*Mission     `json:"missions,omitempty"` // parent issue number → mission
	SupervisorDecisions []SupervisorDecision `json:"supervisor_decisions,omitempty"`
	NextSlot            int                  `json:"next_slot"`
	LastMergeAt         time.Time            `json:"last_merge_at,omitempty"`
}

func NewState() *State {
	return &State{
		Sessions: make(map[string]*Session),
		Missions: make(map[int]*Mission),
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

// RecordSupervisorDecision appends a supervisor decision and keeps only recent records.
func (s *State) RecordSupervisorDecision(decision SupervisorDecision, limit int) {
	if limit <= 0 {
		limit = DefaultSupervisorDecisionLimit
	}
	s.SupervisorDecisions = append(s.SupervisorDecisions, decision)
	if len(s.SupervisorDecisions) > limit {
		s.SupervisorDecisions = append([]SupervisorDecision(nil), s.SupervisorDecisions[len(s.SupervisorDecisions)-limit:]...)
	}
}

// LatestSupervisorDecision returns the newest supervisor decision by creation time.
func (s *State) LatestSupervisorDecision() *SupervisorDecision {
	if len(s.SupervisorDecisions) == 0 {
		return nil
	}
	latest := 0
	for i := 1; i < len(s.SupervisorDecisions); i++ {
		if s.SupervisorDecisions[i].CreatedAt.After(s.SupervisorDecisions[latest].CreatedAt) {
			latest = i
		}
	}
	return &s.SupervisorDecisions[latest]
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

// IssueDone returns true if the given issue already has a completed session.
func (s *State) IssueDone(issueNum int) bool {
	for _, sess := range s.Sessions {
		if sess.IssueNumber == issueNum && sess.Status == StatusDone {
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
// StatusPriority returns a sort key for session statuses.
// Lower values sort first. Running sessions appear at the top,
// followed by actionable states, then terminal states.
func StatusPriority(status SessionStatus) int {
	switch status {
	case StatusRunning:
		return 0
	case StatusPROpen:
		return 1
	case StatusQueued:
		return 2
	case StatusDead:
		return 3
	case StatusFailed:
		return 4
	case StatusConflictFailed:
		return 5
	case StatusRetryExhausted:
		return 6
	case StatusDone:
		return 7
	default:
		return 8
	}
}

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

// IsMissionParent returns true if the given issue number is a mission parent.
func (s *State) IsMissionParent(issueNum int) bool {
	if s.Missions == nil {
		return false
	}
	_, ok := s.Missions[issueNum]
	return ok
}

// IsMissionChild returns true if the given issue number is a child of any mission.
func (s *State) IsMissionChild(issueNum int) bool {
	if s.Missions == nil {
		return false
	}
	for _, m := range s.Missions {
		for _, child := range m.ChildIssues {
			if child == issueNum {
				return true
			}
		}
	}
	return false
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
