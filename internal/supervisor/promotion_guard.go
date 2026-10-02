package supervisor

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"

	"github.com/befeast/maestro/internal/github"
)

// Ready-label promotion guard (#1240).
//
// add_ready_label is a mutating queue action: once an issue carries the ready
// label the orchestrator may start a worker for it. The deterministic
// supervisor used to add the label to the first unlabeled open issue under the
// default queue policy, even with the supervisor LLM disabled and no ordered
// queue or dynamic wave configured, which promoted an unrelated P2 epic and
// started a worker outside the pilot scope. Promotion is now an explicit
// opt-in, and content guards apply on every promotion path.

const (
	promotionHoldPolicyDisabled = "auto_promote_disabled"
	promotionHoldExcludedLabel  = "excluded_label"
	promotionHoldEpicTitle      = "epic_title"
	promotionHoldLowerPriority  = "lower_priority"
)

// promotionHold explains why add_ready_label is withheld from one issue. Code
// is stable per hold kind and keys the once-per-issue journal line; Reason is
// the human-readable explanation.
type promotionHold struct {
	Code   string
	Reason string
}

var (
	// epicTitlePrefix matches an "Epic:" title prefix, optionally after
	// leading bracketed tags such as "[P2]" or "(web)".
	epicTitlePrefix = regexp.MustCompile(`(?i)^\s*(?:[\[(][^\])]*[\])]\s*)*epic\s*:`)
	// epicTitleTag matches an "(epic)" or "[epic]" marker anywhere in a title.
	epicTitleTag = regexp.MustCompile(`(?i)[\[(]\s*epic\s*[\])]`)
)

// titleMarksEpic reports whether an issue title marks the issue as an
// epic/parent: an "Epic:" prefix (optionally after leading bracketed tags), or
// an "(epic)" / "[epic]" marker anywhere. Matching is case-insensitive. It is
// deliberately broader than titleLooksEpic, which the handoff planner uses to
// pick its parent epic and must keep its narrower contract.
func titleMarksEpic(title string) bool {
	return epicTitlePrefix.MatchString(title) || epicTitleTag.MatchString(title)
}

// promotionExcludedLabels lists every label that must keep an issue out of
// ready-label promotion: the project's exclude_labels (plus the supervisor
// blocked label) and supervisor.excluded_labels. liftedLabel names a label
// the same decision removes (the supervisor blocked label on an unblock); it
// does not count because the issue will not carry it once the decision lands.
func (e *Engine) promotionExcludedLabels(liftedLabel string) []string {
	labels := append([]string(nil), e.excludeLabels()...)
	labels = append(labels, e.policyExcludedLabels()...)
	return excludeLabelsExcept(uniqueLabelNames(labels), liftedLabel)
}

// promotionContentHold applies the guards that hold on every promotion path,
// including ordered_queue and dynamic_wave: an excluded label or an
// epic/parent title is never promoted.
func (e *Engine) promotionContentHold(issue github.Issue, liftedLabel string) *promotionHold {
	if label, ok := firstMatchingIssueLabel(issue, e.promotionExcludedLabels(liftedLabel)); ok {
		return &promotionHold{
			Code:   promotionHoldExcludedLabel,
			Reason: fmt.Sprintf("issue carries excluded label %q", label),
		}
	}
	if titleMarksEpic(issue.Title) {
		return &promotionHold{
			Code:   promotionHoldEpicTitle,
			Reason: "issue title marks it as an epic/parent issue",
		}
	}
	return nil
}

// defaultQueuePromotionAllowed reports whether the default queue policy may
// add the ready label. Ordered queue promotion is driven by the operator's
// explicit issue list; everything else requires supervisor.auto_promote_ready.
func (e *Engine) defaultQueuePromotionAllowed(policyRule string) bool {
	if policyRule == PolicyRuleOrderedQueue {
		return true
	}
	return e.cfg != nil && e.cfg.Supervisor.AutoPromoteReady
}

// queuePromotionHold is the full guard for the ordered-queue / default queue
// path (firstQueueActionCandidate). The priority guard applies only to the
// default policy: an ordered queue is an explicit operator order.
func (e *Engine) queuePromotionHold(issue github.Issue, openIssues []github.Issue, readyLabel, liftedLabel, policyRule string) *promotionHold {
	if hold := e.promotionContentHold(issue, liftedLabel); hold != nil {
		return hold
	}
	if !e.defaultQueuePromotionAllowed(policyRule) {
		return &promotionHold{
			Code:   promotionHoldPolicyDisabled,
			Reason: "supervisor.auto_promote_ready is off; the default queue policy only dispatches issues that already carry the ready label (ordered_queue or dynamic_wave drive promotion)",
		}
	}
	if policyRule == PolicyRuleOrderedQueue {
		return nil
	}
	if other, ok := higherPriorityReadyIssue(issue, openIssues, readyLabel); ok {
		_, otherLabel := issuePriority(other)
		label := "no priority label"
		if _, own := issuePriority(issue); own != "" {
			label = "priority " + own
		}
		return &promotionHold{
			Code:   promotionHoldLowerPriority,
			Reason: fmt.Sprintf("%s ranks below open ready issue #%d (%s)", label, other.Number, otherLabel),
		}
	}
	return nil
}

// higherPriorityReadyIssue returns the highest-priority open issue already
// carrying the ready label when its priority label (p0..p3, the dynamic wave
// ranking) outranks issue's. Ties break on the lower issue number.
func higherPriorityReadyIssue(issue github.Issue, openIssues []github.Issue, readyLabel string) (github.Issue, bool) {
	if strings.TrimSpace(readyLabel) == "" {
		return github.Issue{}, false
	}
	rank, _ := issuePriority(issue)
	var best github.Issue
	bestRank := rank
	found := false
	for _, other := range openIssues {
		if other.Number == issue.Number || !github.HasLabel(other, []string{readyLabel}) {
			continue
		}
		otherRank, _ := issuePriority(other)
		if otherRank < bestRank || (found && otherRank == bestRank && other.Number < best.Number) {
			best = other
			bestRank = otherRank
			found = true
		}
	}
	return best, found
}

// promotionHoldNote is the decision reason recorded for a withheld promotion.
func promotionHoldNote(issueNumber int, hold promotionHold) string {
	return fmt.Sprintf("Ready-label promotion withheld for issue #%d: %s", issueNumber, hold.Reason)
}

// journalPromotionHold logs why add_ready_label was withheld from an issue,
// once per (repo, issue, hold kind) for the life of the process, so an
// operator can see the reason in the daemon journal without a line on every
// supervise cycle.
func (e *Engine) journalPromotionHold(issue github.Issue, hold promotionHold) {
	journal := e.promotionJournal
	if journal == nil {
		journal = defaultPromotionHoldJournal
	}
	repo := ""
	if e.cfg != nil {
		repo = strings.ToLower(strings.TrimSpace(e.cfg.Repo))
	}
	key := fmt.Sprintf("%s#%d:%s", repo, issue.Number, hold.Code)
	if !journal.FirstSighting(key) {
		return
	}
	log.Printf("[supervisor] withholding %s from issue #%d: %s", MutationAddReadyLabel, issue.Number, hold.Reason)
}

// promotionHoldJournal remembers which promotion holds were already journaled.
// RunOnce builds a fresh Engine per cycle, so the default journal is
// process-wide (the same pattern as the #569 enrollment tracker).
type promotionHoldJournal interface {
	// FirstSighting records key and reports whether it was new.
	FirstSighting(key string) bool
}

type inMemoryPromotionHoldJournal struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func newInMemoryPromotionHoldJournal() *inMemoryPromotionHoldJournal {
	return &inMemoryPromotionHoldJournal{seen: make(map[string]struct{})}
}

func (j *inMemoryPromotionHoldJournal) FirstSighting(key string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, ok := j.seen[key]; ok {
		return false
	}
	j.seen[key] = struct{}{}
	return true
}

var defaultPromotionHoldJournal = newInMemoryPromotionHoldJournal()
