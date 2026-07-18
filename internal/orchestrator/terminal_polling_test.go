package orchestrator

import (
	"testing"
	"time"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func TestCheckSessionsBoundsSuccessfulTerminalReconciliation(t *testing.T) {
	finished := time.Now().UTC().Add(-time.Hour)
	sess := &state.Session{
		IssueNumber: 940,
		Status:      state.StatusDead,
		StartedAt:   finished.Add(-time.Hour),
		FinishedAt:  &finished,
	}
	st := &state.State{Sessions: map[string]*state.Session{"sup-1": sess}}
	issueReads := 0
	o := &Orchestrator{
		listOpenPRsFn: func() ([]github.PR, error) { return nil, nil },
		isIssueClosedFn: func(issue int) (bool, error) {
			issueReads++
			return false, nil
		},
	}

	o.checkSessions(st)
	if issueReads != 1 {
		t.Fatalf("first terminal reconciliation issue reads = %d, want 1", issueReads)
	}
	if sess.LastTerminalReconcileAt == nil {
		t.Fatal("successful terminal reconciliation did not persist its timestamp")
	}

	o.checkSessions(st)
	if issueReads != 1 {
		t.Fatalf("immediate repeated terminal reconciliation issue reads = %d, want still 1", issueReads)
	}

	due := time.Now().UTC().Add(-terminalReconcileInterval - time.Second)
	sess.LastTerminalReconcileAt = &due
	o.checkSessions(st)
	if issueReads != 2 {
		t.Fatalf("due terminal reconciliation issue reads = %d, want 2", issueReads)
	}
}

func TestMergedIssueLookupsShareOneClosedPRSnapshotPerCycle(t *testing.T) {
	closedReads := 0
	o := &Orchestrator{
		listClosedPRsFn: func() ([]github.PR, error) {
			closedReads++
			return []github.PR{
				{Number: 11, State: "closed", MergedAt: "2026-07-18T00:00:00Z", Body: "Fixes #101"},
				{Number: 12, State: "closed", MergedAt: "2026-07-18T00:01:00Z", Body: "Fixes #102", HeadRefName: "fix/102"},
			}, nil
		},
		gh: github.New("BeFeast/maestro"),
	}
	o.beginCycle()
	defer o.endCycle()

	if got, err := o.mergedPRForIssue(101); err != nil || got != 11 {
		t.Fatalf("mergedPRForIssue(101) = %d, %v; want 11, nil", got, err)
	}
	if got, err := o.mergedPRForIssue(102); err != nil || got != 12 {
		t.Fatalf("mergedPRForIssue(102) = %d, %v; want 12, nil", got, err)
	}
	if got, err := o.mergedPRForBranch("fix/102"); err != nil || got != 12 {
		t.Fatalf("mergedPRForBranch(fix/102) = %d, %v; want 12, nil", got, err)
	}
	if closedReads != 1 {
		t.Fatalf("closed PR snapshot reads = %d, want 1", closedReads)
	}
}
