package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type SessionStatus string

const (
	StatusRunning        SessionStatus = "running"
	StatusPROpen         SessionStatus = "pr_open"
	StatusDone           SessionStatus = "done"
	StatusFailed         SessionStatus = "failed"
	StatusConflictFailed SessionStatus = "conflict_failed"
	StatusDead           SessionStatus = "dead"
)

type Session struct {
	IssueNumber           int           `json:"issue_number"`
	IssueTitle            string        `json:"issue_title"`
	Worktree              string        `json:"worktree"`
	Branch                string        `json:"branch"`
	PID                   int           `json:"pid"`
	LogFile               string        `json:"log_file"`
	StartedAt             time.Time     `json:"started_at"`
	FinishedAt            *time.Time    `json:"finished_at,omitempty"`
	Status                SessionStatus `json:"status"`
	PRNumber              int           `json:"pr_number,omitempty"`
	Backend               string        `json:"backend,omitempty"` // "claude", "codex", etc.
	LongRunning           bool          `json:"long_running,omitempty"`
	NotifiedCIFail        bool          `json:"notified_ci_fail,omitempty"`
	NotifiedGreptileBlock bool          `json:"notified_greptile_block,omitempty"`
	RetryCount            int           `json:"retry_count,omitempty"`
}

type State struct {
	Sessions map[string]*Session `json:"sessions"`
	NextSlot int                 `json:"next_slot"`
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

// IssueInProgress returns true if the given issue is already being handled
func (s *State) IssueInProgress(issueNum int) bool {
	for _, sess := range s.Sessions {
		if sess.IssueNumber == issueNum &&
			(sess.Status == StatusRunning || sess.Status == StatusPROpen) {
			return true
		}
	}
	return false
}
