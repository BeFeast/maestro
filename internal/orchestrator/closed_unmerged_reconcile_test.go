package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/state"
)

func TestReconcileFalseDoneSessionAdoptsSoleOpenCanonicalPR(t *testing.T) {
	finished := time.Date(2026, 7, 17, 17, 13, 56, 0, time.UTC)
	s := state.NewState()
	s.Sessions["ok-player-273"] = &state.Session{
		IssueNumber: 345,
		Status:      state.StatusDone,
		PRNumber:    389,
		Branch:      "feat/duplicate-389",
		FinishedAt:  &finished,
	}
	o := &Orchestrator{
		isPRMergedFn:    func(int) (bool, error) { return false, nil },
		isIssueClosedFn: func(int) (bool, error) { return false, nil },
	}

	o.reconcileFalseDoneSessionsWithOpenPRs(s, []github.PR{{
		Number:      388,
		HeadRefName: "feat/ok-player-345-flatpak-beta-retry",
		Title:       "Linux packaging for #345",
	}})

	sess := s.Sessions["ok-player-273"]
	if sess.Status != state.StatusPROpen {
		t.Fatalf("status = %q, want pr_open", sess.Status)
	}
	if sess.PRNumber != 388 || sess.Branch != "feat/ok-player-345-flatpak-beta-retry" {
		t.Fatalf("canonical identity = PR #%d branch %q, want PR #388 canonical branch", sess.PRNumber, sess.Branch)
	}
	if sess.LastClosedPRNumber != 389 {
		t.Fatalf("last closed PR = %d, want 389", sess.LastClosedPRNumber)
	}
	if sess.FinishedAt != nil {
		t.Fatalf("finished_at retained after reopening canonical PR: %v", sess.FinishedAt)
	}
}

func TestReconcileFalseDoneSessionRefusesAmbiguousOpenPRs(t *testing.T) {
	s := state.NewState()
	s.Sessions["slot"] = &state.Session{IssueNumber: 345, Status: state.StatusDone, PRNumber: 389}
	o := &Orchestrator{}

	o.reconcileFalseDoneSessionsWithOpenPRs(s, []github.PR{
		{Number: 388, Title: "first #345"},
		{Number: 390, Title: "second #345"},
	})

	if got := s.Sessions["slot"]; got.Status != state.StatusDone || got.PRNumber != 389 {
		t.Fatalf("ambiguous reconcile mutated session: %+v", got)
	}
}

func TestReconcileDoneSessionKeepsAuthoritativeMergedOutcome(t *testing.T) {
	s := state.NewState()
	s.Sessions["slot"] = &state.Session{IssueNumber: 345, Status: state.StatusDone, PRNumber: 389}
	o := &Orchestrator{
		isPRMergedFn: func(pr int) (bool, error) { return pr == 389, nil },
		isIssueClosedFn: func(int) (bool, error) {
			t.Fatal("merged PR is authoritative; issue state should not be queried")
			return false, nil
		},
	}

	o.reconcileFalseDoneSessionsWithOpenPRs(s, []github.PR{{Number: 388, Title: "follow-up #345"}})

	if got := s.Sessions["slot"]; got.Status != state.StatusDone || got.PRNumber != 389 {
		t.Fatalf("authoritative merged session mutated: %+v", got)
	}
}

func TestAutoMergeClosedUnmergedPRReleasesOpenIssue(t *testing.T) {
	o := &Orchestrator{
		cfg:             &config.Config{Repo: "owner/repo"},
		notifier:        &notify.Notifier{},
		listOpenPRsFn:   func() ([]github.PR, error) { return nil, nil },
		isPRMergedFn:    func(int) (bool, error) { return false, nil },
		isIssueClosedFn: func(int) (bool, error) { return false, nil },
	}
	s := state.NewState()
	s.Sessions["slot"] = &state.Session{
		IssueNumber: 345,
		Status:      state.StatusPROpen,
		PRNumber:    389,
		Branch:      "feat/duplicate-389",
	}

	o.autoMergePRs(s)

	sess := s.Sessions["slot"]
	if sess.Status != state.StatusFailed || !sess.ReleasedForRedispatch {
		t.Fatalf("session = %+v, want failed/released after closed-unmerged PR", sess)
	}
	if sess.PRNumber != 0 || sess.LastClosedPRNumber != 389 {
		t.Fatalf("PR identity = current %d last_closed %d, want 0/389", sess.PRNumber, sess.LastClosedPRNumber)
	}
}
