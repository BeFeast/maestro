package orchestrator

import (
	"testing"

	"github.com/befeast/maestro/internal/github"
)

// fakeReadSource records which mirror-first read wrappers were routed to it.
type fakeReadSource struct {
	listIssues int
	listPRs    int
	getIssue   int
	isClosed   int
	isMerged   int
}

func (f *fakeReadSource) ListOpenIssues(labels []string) ([]github.Issue, error) {
	f.listIssues++
	return []github.Issue{{Number: 1}}, nil
}
func (f *fakeReadSource) ListOpenPRs() ([]github.PR, error) {
	f.listPRs++
	return []github.PR{{Number: 2}}, nil
}
func (f *fakeReadSource) GetIssue(number int) (github.Issue, error) {
	f.getIssue++
	return github.Issue{Number: number}, nil
}
func (f *fakeReadSource) IsIssueClosed(number int) (bool, error) {
	f.isClosed++
	return true, nil
}
func (f *fakeReadSource) IsPRMerged(prNumber int) (bool, error) {
	f.isMerged++
	return true, nil
}

// TestReadSourceRoutesReadWrappers proves SetReadSource routes the high-volume
// read wrappers through the mirror-first source instead of the direct client
// (#826). The orchestrator's gh client is left nil: if any wrapper fell through
// to gh instead of the source, it would panic or error — so a clean pass is the
// assertion that all five went to the source.
func TestReadSourceRoutesReadWrappers(t *testing.T) {
	src := &fakeReadSource{}
	o := &Orchestrator{}
	o.SetReadSource(src)

	if _, err := o.listOpenIssues(nil); err != nil {
		t.Fatalf("listOpenIssues: %v", err)
	}
	if _, err := o.listOpenPRs(); err != nil {
		t.Fatalf("listOpenPRs: %v", err)
	}
	if _, err := o.getIssue(9); err != nil {
		t.Fatalf("getIssue: %v", err)
	}
	if _, err := o.isIssueClosed(9); err != nil {
		t.Fatalf("isIssueClosed: %v", err)
	}
	if _, err := o.isPRMerged(2); err != nil {
		t.Fatalf("isPRMerged: %v", err)
	}

	if src.listIssues != 1 || src.listPRs != 1 || src.getIssue != 1 || src.isClosed != 1 || src.isMerged != 1 {
		t.Fatalf("not all read wrappers routed to the source: %+v", src)
	}
}

// TestReadSourceUnsetUsesClient confirms that without a read source the wrappers
// still consult the direct client (nil here → the pre-#826 nil-client errors),
// so wiring is purely additive.
func TestReadSourceUnsetUsesClient(t *testing.T) {
	o := &Orchestrator{}
	// No SetReadSource, no gh client: isPRMerged reports the historical
	// "no github client configured" error rather than silently succeeding.
	if _, err := o.isPRMerged(1); err == nil {
		t.Fatal("expected nil-client error when neither read source nor gh is set")
	}
}
