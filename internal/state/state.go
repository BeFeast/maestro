package state

import (
	"crypto/md5"
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
	IssueNumber int           `json:"issue_number"`
	IssueTitle  string        `json:"issue_title"`
	Worktree    string        `json:"worktree"`
	Branch      string        `json:"branch"`
	PID         int           `json:"pid"`
	LogFile     string        `json:"log_file"`
	StartedAt   time.Time     `json:"started_at"`
	FinishedAt  *time.Time    `json:"finished_at,omitempty"`
	Status      SessionStatus `json:"status"`
	PRNumber    int           `json:"pr_number,omitempty"`
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

func StateDir(repo string) string {
	hash := fmt.Sprintf("%x", md5.Sum([]byte(repo)))[:12]
	return filepath.Join(os.Getenv("HOME"), ".maestro", hash)
}

func StatePath(repo string) string {
	return filepath.Join(StateDir(repo), "state.json")
}

func LogDir(repo string) string {
	return filepath.Join(StateDir(repo), "logs")
}

func Load(repo string) (*State, error) {
	path := StatePath(repo)
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

func Save(repo string, s *State) error {
	dir := StateDir(repo)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	path := StatePath(repo)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("atomic rename state: %w", err)
	}
	return nil
}

// NextSlotName returns "pan-N" for the next available slot
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
