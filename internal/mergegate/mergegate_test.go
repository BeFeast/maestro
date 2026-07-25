package mergegate

import (
	"strings"
	"sync"
	"testing"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

func TestExecute_RefusesLateThreadAfterInitialGreenObservation(t *testing.T) {
	dir := t.TempDir()
	head := strings.Repeat("a", 40)
	reads := 0
	result := Execute(Request{StateDir: dir, Repo: "owner/repo", IssueNumber: 42, PRNumber: 7, HoldLabels: []string{"blocked"}}, ReadFuncs{
		Issue:    func(int) (github.Issue, error) { return github.Issue{Number: 42}, nil },
		PRLabels: func(int) ([]string, error) { return nil, nil },
		ReviewThreads: func(int) (string, []github.ReviewThread, error) {
			reads++
			if reads == 1 {
				return head, nil, nil
			}
			return head, []github.ReviewThread{{ID: "thread-1", Path: "merge.go", Line: 9, Author: "codex"}}, nil
		},
	}, func(string) error {
		t.Fatal("merge side effect must not run with a late unresolved thread")
		return nil
	})
	if !result.Refused || result.RefusalName != "review-thread:thread-1" || !strings.Contains(result.RefusalReason, "unresolved") {
		t.Fatalf("result = %+v", result)
	}
	st, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	control, ok := st.MergeControlForPR(7)
	if !ok || !strings.Contains(control.LastRefusalReason, "unresolved") {
		t.Fatalf("merge control = %+v", control)
	}
}

func TestExecute_ConcurrentAttemptsProduceOneMergeSideEffect(t *testing.T) {
	dir := t.TempDir()
	head := strings.Repeat("b", 40)
	reads := ReadFuncs{
		Issue:         func(int) (github.Issue, error) { return github.Issue{Number: 42}, nil },
		PRLabels:      func(int) ([]string, error) { return nil, nil },
		ReviewThreads: func(int) (string, []github.ReviewThread, error) { return head, nil, nil },
	}
	orchestratorRequest := Request{StateDir: dir, Repo: "owner/repo", IssueNumber: 42, PRNumber: 7, Owner: "orchestrator", HoldLabels: []string{"blocked"}}
	supervisorRequest := orchestratorRequest
	supervisorRequest.Owner = "supervisor"
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		merges int
	)
	merge := func(string) error {
		mu.Lock()
		merges++
		mu.Unlock()
		return nil
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		Execute(orchestratorRequest, reads, merge)
	}()
	go func() {
		defer wg.Done()
		Execute(supervisorRequest, reads, merge)
	}()
	wg.Wait()
	if merges != 1 {
		t.Fatalf("merges = %d, want exactly one claimed side effect", merges)
	}
}

func TestExecute_LateMissionControlHoldIntentWinsBeforeSideEffect(t *testing.T) {
	dir := t.TempDir()
	head := strings.Repeat("c", 40)
	firstRead := make(chan struct{})
	continueRead := make(chan struct{})
	reads := 0
	resultCh := make(chan Result, 1)
	go func() {
		resultCh <- Execute(Request{StateDir: dir, Repo: "owner/repo", IssueNumber: 42, PRNumber: 8, Owner: "orchestrator", HoldLabels: []string{"blocked"}}, ReadFuncs{
			Issue:    func(int) (github.Issue, error) { return github.Issue{Number: 42}, nil },
			PRLabels: func(int) ([]string, error) { return nil, nil },
			ReviewThreads: func(int) (string, []github.ReviewThread, error) {
				reads++
				if reads == 1 {
					close(firstRead)
					<-continueRead
				}
				return head, nil, nil
			},
		}, func(string) error {
			t.Error("merge side effect ran after late hold intent")
			return nil
		})
	}()
	<-firstRead
	SetHoldIntent("owner/repo", 8, true)
	close(continueRead)
	result := <-resultCh
	SetHoldIntent("owner/repo", 8, false)
	if !result.Refused || !strings.Contains(result.RefusalReason, "hold") {
		t.Fatalf("result = %+v", result)
	}
}
