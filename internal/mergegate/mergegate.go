// Package mergegate owns the final compare/claim boundary immediately before
// a GitHub merge side effect. Both automatic and approval-driven merge paths
// use it, as do Mission Control hold/release actions.
package mergegate

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

type ReadFuncs struct {
	Issue         func(int) (github.Issue, error)
	PRLabels      func(int) ([]string, error)
	ReviewThreads func(int) (string, []github.ReviewThread, error)
}

type Request struct {
	StateDir     string
	Repo         string
	IssueNumber  int
	PRNumber     int
	ExpectedHead string
	Owner        string
	HoldLabels   []string
}

type Result struct {
	Merged        bool
	HeadSHA       string
	Refused       bool
	RefusalName   string
	RefusalReason string
	Err           error
}

var (
	boundaryLocks sync.Map
	holdIntents   sync.Map
	claimSequence atomic.Uint64
)

func boundaryKey(repo string, prNumber int) string {
	return fmt.Sprintf("%s#%d", strings.ToLower(strings.TrimSpace(repo)), prNumber)
}

func Lock(repo string, prNumber int) func() {
	key := boundaryKey(repo, prNumber)
	value, _ := boundaryLocks.LoadOrStore(key, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// SetHoldIntent makes a Mission Control hold visible before it waits for the
// per-PR boundary lock. An already-validating merge checks this flag again at
// the last instruction before its side effect, so a hold request that arrives
// after ordinary gates but before merge deterministically wins.
func SetHoldIntent(repo string, prNumber int, held bool) {
	key := boundaryKey(repo, prNumber)
	if held {
		holdIntents.Store(key, true)
		return
	}
	holdIntents.Delete(key)
}

func HoldIntentActive(repo string, prNumber int) bool {
	_, ok := holdIntents.Load(boundaryKey(repo, prNumber))
	return ok
}

func ConfiguredHoldLabels(cfg *config.Config) []string {
	if cfg == nil {
		return []string{"blocked", "operator-decision"}
	}
	values := []string{"blocked", "operator-decision", cfg.Supervisor.BlockedLabel}
	values = append(values, cfg.ExcludeLabels...)
	values = append(values, cfg.Supervisor.OperatorGate.Labels...)
	return normalize(values)
}

func PrimaryHoldLabel(cfg *config.Config) string {
	if cfg != nil {
		if label := strings.TrimSpace(cfg.Supervisor.BlockedLabel); label != "" {
			return label
		}
		for _, label := range cfg.ExcludeLabels {
			if strings.EqualFold(strings.TrimSpace(label), "blocked") {
				return strings.TrimSpace(label)
			}
		}
		for _, label := range cfg.Supervisor.OperatorGate.Labels {
			if strings.TrimSpace(label) != "" {
				return strings.TrimSpace(label)
			}
		}
	}
	return "blocked"
}

func normalize(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func Execute(req Request, reads ReadFuncs, mergeAtHead func(string) error) Result {
	if req.PRNumber <= 0 || req.IssueNumber <= 0 {
		return Result{Refused: true, RefusalName: "merge-boundary:missing-target", RefusalReason: "issue and PR identity are required before merge"}
	}
	if reads.Issue == nil || reads.PRLabels == nil || reads.ReviewThreads == nil || mergeAtHead == nil {
		return Result{Refused: true, RefusalName: "merge-boundary:unavailable", RefusalReason: "final merge readers are not configured"}
	}

	unlock := Lock(req.Repo, req.PRNumber)
	defer unlock()

	if HoldIntentActive(req.Repo, req.PRNumber) {
		reason := "an explicit Mission Control merge hold is pending"
		recordRefusal(req, "", reason)
		return refusal("merge-hold:pending", reason)
	}

	// Establish the current head once before claiming. The same authoritative
	// thread connection is read again after the claim; that second observation
	// is the final review-thread gate immediately before merge.
	initialHead, _, err := reads.ReviewThreads(req.PRNumber)
	if err != nil {
		reason := fmt.Sprintf("could not read current review threads before merge: %v", err)
		recordRefusal(req, "", reason)
		return refusal("merge-check:review-threads-unavailable", reason)
	}
	initialHead = strings.TrimSpace(initialHead)
	if initialHead == "" {
		reason := "current PR head is empty at the merge boundary"
		recordRefusal(req, "", reason)
		return refusal("merge-check:head-unavailable", reason)
	}
	if expected := strings.TrimSpace(req.ExpectedHead); expected != "" && expected != initialHead {
		reason := fmt.Sprintf("PR head changed after gate evaluation; expected %s, current %s", expected, initialHead)
		recordRefusal(req, "", reason)
		return refusal("merge-check:head-changed", reason)
	}

	claimID := fmt.Sprintf("merge-%d-%d", req.PRNumber, claimSequence.Add(1))
	if reason, err := claim(req, claimID, initialHead); err != nil {
		reason = fmt.Sprintf("could not claim final merge boundary: %v", err)
		recordRefusal(req, claimID, reason)
		return refusal("merge-claim:unavailable", reason)
	} else if reason != "" {
		return refusal("merge-claim:refused", reason)
	}

	finalHead, threads, err := reads.ReviewThreads(req.PRNumber)
	if err != nil {
		reason := fmt.Sprintf("could not re-read current review threads immediately before merge: %v", err)
		recordRefusal(req, claimID, reason)
		return refusal("merge-check:review-threads-unavailable", reason)
	}
	finalHead = strings.TrimSpace(finalHead)
	if finalHead != initialHead {
		reason := fmt.Sprintf("PR head changed during final merge validation; expected %s, current %s", initialHead, finalHead)
		recordRefusal(req, claimID, reason)
		return refusal("merge-check:head-changed", reason)
	}
	if len(threads) > 0 {
		reason := unresolvedThreadReason(threads)
		recordRefusal(req, claimID, reason)
		return refusal(unresolvedThreadName(threads[0]), reason)
	}

	// Label reads deliberately come after every other remote gate, making them
	// the last GitHub observations before the head-bound merge side effect.
	issue, err := reads.Issue(req.IssueNumber)
	if err != nil {
		reason := fmt.Sprintf("could not re-read issue labels immediately before merge: %v", err)
		recordRefusal(req, claimID, reason)
		return refusal("merge-check:issue-labels-unavailable", reason)
	}
	if label, ok := matchingIssueLabel(issue, req.HoldLabels); ok {
		reason := fmt.Sprintf("issue #%d carries merge hold/exclude label %q", req.IssueNumber, label)
		recordRefusal(req, claimID, reason)
		return refusal("label:"+label, reason)
	}
	prLabels, err := reads.PRLabels(req.PRNumber)
	if err != nil {
		reason := fmt.Sprintf("could not re-read PR labels immediately before merge: %v", err)
		recordRefusal(req, claimID, reason)
		return refusal("merge-check:pr-labels-unavailable", reason)
	}
	if label, ok := matchingLabel(prLabels, req.HoldLabels); ok {
		reason := fmt.Sprintf("PR #%d carries merge hold/exclude label %q", req.PRNumber, label)
		recordRefusal(req, claimID, reason)
		return refusal("label:"+label, reason)
	}

	// Hold intent is checked after the final remote reads, then the durable
	// claim is compared once more. A hold action sets intent before contending
	// on this lock and invalidates the claim in state, so either check catches it.
	if HoldIntentActive(req.Repo, req.PRNumber) {
		reason := "an explicit Mission Control merge hold arrived during final validation"
		recordRefusal(req, claimID, reason)
		return refusal("merge-hold:late", reason)
	}
	if reason, err := validateClaim(req, claimID); err != nil {
		reason = fmt.Sprintf("could not compare final merge claim: %v", err)
		recordRefusal(req, claimID, reason)
		return refusal("merge-claim:unavailable", reason)
	} else if reason != "" {
		recordRefusal(req, claimID, reason)
		return refusal("merge-claim:superseded", reason)
	}

	if err := mergeAtHead(finalHead); err != nil {
		finish(req, claimID, "failed")
		return Result{HeadSHA: finalHead, Err: err}
	}
	finish(req, claimID, "merged")
	return Result{Merged: true, HeadSHA: finalHead}
}

func refusal(name, reason string) Result {
	return Result{Refused: true, RefusalName: name, RefusalReason: reason}
}

func claim(req Request, claimID, headSHA string) (string, error) {
	if strings.TrimSpace(req.StateDir) == "" {
		return "", nil
	}
	var refusal string
	err := state.Update(req.StateDir, func(st *state.State) error {
		_, refusal = st.TryClaimMerge(req.IssueNumber, req.PRNumber, claimID, headSHA, req.Owner, time.Now().UTC())
		return nil
	})
	return refusal, err
}

func validateClaim(req Request, claimID string) (string, error) {
	if strings.TrimSpace(req.StateDir) == "" {
		return "", nil
	}
	var refusal string
	err := state.Update(req.StateDir, func(st *state.State) error {
		_, refusal = st.ValidateMergeClaim(req.PRNumber, claimID)
		if refusal != "" {
			st.RecordMergeRefusal(req.PRNumber, claimID, refusal, time.Now().UTC())
		}
		return nil
	})
	return refusal, err
}

func recordRefusal(req Request, claimID, reason string) {
	if strings.TrimSpace(req.StateDir) == "" {
		return
	}
	_ = state.Update(req.StateDir, func(st *state.State) error {
		st.RecordMergeRefusal(req.PRNumber, claimID, reason, time.Now().UTC())
		return nil
	})
}

func finish(req Request, claimID, result string) {
	if strings.TrimSpace(req.StateDir) == "" {
		return
	}
	_ = state.Update(req.StateDir, func(st *state.State) error {
		st.FinishMergeClaim(req.PRNumber, claimID, result, time.Now().UTC())
		return nil
	})
}

func matchingIssueLabel(issue github.Issue, holds []string) (string, bool) {
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		labels = append(labels, label.Name)
	}
	return matchingLabel(labels, holds)
}

func matchingLabel(labels, holds []string) (string, bool) {
	for _, label := range labels {
		for _, hold := range holds {
			if strings.EqualFold(strings.TrimSpace(label), strings.TrimSpace(hold)) {
				return strings.TrimSpace(label), true
			}
		}
	}
	return "", false
}

func unresolvedThreadName(thread github.ReviewThread) string {
	id := strings.TrimSpace(thread.ID)
	if id == "" {
		id = "current-head"
	}
	return "review-thread:" + id
}

func unresolvedThreadReason(threads []github.ReviewThread) string {
	thread := threads[0]
	where := strings.TrimSpace(thread.Path)
	if where != "" && thread.Line > 0 {
		where = fmt.Sprintf("%s:%d", where, thread.Line)
	}
	by := strings.TrimSpace(thread.Author)
	detail := ""
	if where != "" {
		detail += " at " + where
	}
	if by != "" {
		detail += " by " + by
	}
	if len(threads) == 1 {
		return "1 unresolved current-head review thread remains" + detail
	}
	return fmt.Sprintf("%d unresolved current-head review threads remain%s", len(threads), detail)
}
