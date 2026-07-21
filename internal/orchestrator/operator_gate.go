package orchestrator

import (
	"log"
	"path/filepath"
	"strings"

	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/state"
)

const operatorGateStatus = "operator_gate"

type operatorGateHold struct {
	Name           string
	RequiredAction string
}

func (o *Orchestrator) operatorGateHoldForPR(sess *state.Session, pr github.PR, rollup github.PRCheckRollup) (operatorGateHold, bool) {
	if o == nil || o.cfg == nil || sess == nil || pr.Number <= 0 {
		return operatorGateHold{}, false
	}
	if hold, ok := o.operatorGateHoldFromLabels(sess.IssueNumber, pr.Number); ok {
		return hold, true
	}
	if hold, ok := o.operatorGateHoldFromChecks(rollup); ok {
		return hold, true
	}
	return operatorGateHold{}, false
}

func (o *Orchestrator) operatorGateHoldForRetry(sess *state.Session) (operatorGateHold, int, bool) {
	if o == nil || o.cfg == nil || sess == nil {
		return operatorGateHold{}, 0, false
	}
	prNumber := sess.PRNumber
	if hold, ok := o.operatorGateHoldFromLabelSet(sess.IssueNumber, prNumber, o.operatorGateRetryLabels()); ok {
		return hold, prNumber, true
	}
	if sess.PRNumber <= 0 {
		return operatorGateHold{}, prNumber, false
	}
	rollup, err := o.prCheckRollup(sess.PRNumber)
	if err != nil {
		log.Printf("[orch] operator gate: could not read checks for PR #%d before retry: %v", sess.PRNumber, err)
		return operatorGateHold{}, prNumber, false
	}
	if hold, ok := o.operatorGateHoldFromChecks(rollup); ok {
		return hold, sess.PRNumber, true
	}
	return operatorGateHold{}, prNumber, false
}

func (o *Orchestrator) operatorGateHoldFromLabels(issueNumber, prNumber int) (operatorGateHold, bool) {
	return o.operatorGateHoldFromLabelSet(issueNumber, prNumber, o.operatorGateLabels())
}

func (o *Orchestrator) operatorGateHoldFromLabelSet(issueNumber, prNumber int, holdLabels []string) (operatorGateHold, bool) {
	if len(holdLabels) == 0 {
		return operatorGateHold{}, false
	}
	if issueNumber > 0 {
		issue, err := o.getIssue(issueNumber)
		if err != nil {
			log.Printf("[orch] operator gate: could not read labels for issue #%d: %v", issueNumber, err)
		} else if label, ok := matchingIssueLabel(issue, holdLabels); ok {
			return o.newOperatorGateHold("label:" + label), true
		}
	}
	if prNumber > 0 {
		labels, err := o.prLabels(prNumber)
		if err != nil {
			log.Printf("[orch] operator gate: could not read labels for PR #%d: %v", prNumber, err)
		} else if label, ok := matchingLabelName(labels, holdLabels); ok {
			return o.newOperatorGateHold("label:" + label), true
		}
	}
	return operatorGateHold{}, false
}

func (o *Orchestrator) operatorGateHoldFromChecks(rollup github.PRCheckRollup) (operatorGateHold, bool) {
	patterns := o.operatorGateCheckPatterns()
	if len(patterns) == 0 || !rollup.Complete {
		return operatorGateHold{}, false
	}
	for _, signal := range rollup.Signals {
		if !operatorGateSignalActive(signal) {
			continue
		}
		for _, pattern := range patterns {
			if checkNameMatches(pattern, signal.Name) {
				return o.newOperatorGateHold("check:" + strings.TrimSpace(signal.Name)), true
			}
		}
	}
	return operatorGateHold{}, false
}

func (o *Orchestrator) newOperatorGateHold(name string) operatorGateHold {
	action := ""
	if o != nil && o.cfg != nil {
		action = strings.TrimSpace(o.cfg.Supervisor.OperatorGate.RequiredAction)
	}
	if action == "" {
		action = "Complete the required operator decision, then remove the hold label or let the gated check pass."
	}
	return operatorGateHold{Name: name, RequiredAction: action}
}

func (o *Orchestrator) applyOperatorGateHold(sess *state.Session, pr github.PR, hold operatorGateHold) {
	if sess == nil {
		return
	}
	if pr.Number > 0 {
		sess.PRNumber = pr.Number
		sess.Status = state.StatusPROpen
	}
	sess.NextRetryAt = nil
	sess.LastNotifiedStatus = operatorGateStatus
	sess.NotifiedCIFail = false
	sess.OperatorGateName = hold.Name
	sess.OperatorGateRequiredAction = hold.RequiredAction
	log.Printf("[orch] PR #%d held by operator gate %q — preserving PR and retry budget", pr.Number, hold.Name)
}

func clearOperatorGateHold(sess *state.Session) {
	if sess == nil {
		return
	}
	if sess.LastNotifiedStatus == operatorGateStatus {
		sess.LastNotifiedStatus = ""
	}
	sess.OperatorGateName = ""
	sess.OperatorGateRequiredAction = ""
}

func (o *Orchestrator) operatorGateLabels() []string {
	if o == nil || o.cfg == nil {
		return nil
	}
	labels := []string{"operator-decision", "blocked"}
	if blocked := strings.TrimSpace(o.cfg.Supervisor.BlockedLabel); blocked != "" {
		labels = append(labels, blocked)
	}
	labels = append(labels, o.cfg.Supervisor.OperatorGate.Labels...)
	return normalizeGateTokens(labels)
}

func (o *Orchestrator) operatorGateRetryLabels() []string {
	if o == nil || o.cfg == nil {
		return nil
	}
	labels := []string{"operator-decision"}
	labels = append(labels, o.cfg.Supervisor.OperatorGate.Labels...)
	return normalizeGateTokens(labels)
}

func (o *Orchestrator) operatorGateCheckPatterns() []string {
	if o == nil || o.cfg == nil {
		return nil
	}
	return normalizeGateTokens(o.cfg.Supervisor.OperatorGate.CheckNames)
}

func normalizeGateTokens(values []string) []string {
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

func matchingIssueLabel(issue github.Issue, holdLabels []string) (string, bool) {
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		labels = append(labels, label.Name)
	}
	return matchingLabelName(labels, holdLabels)
}

func matchingLabelName(labels, holdLabels []string) (string, bool) {
	for _, label := range labels {
		for _, hold := range holdLabels {
			if strings.EqualFold(strings.TrimSpace(label), strings.TrimSpace(hold)) {
				return strings.TrimSpace(label), true
			}
		}
	}
	return "", false
}

func operatorGateSignalActive(signal github.PRCheckSignal) bool {
	status := strings.ToLower(strings.TrimSpace(signal.Status))
	conclusion := strings.ToLower(strings.TrimSpace(signal.Conclusion))
	if status == "queued" || status == "in_progress" || status == "waiting" || status == "requested" || status == "pending" {
		return true
	}
	switch conclusion {
	case "failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale", "error":
		return true
	default:
		return false
	}
}

func checkNameMatches(pattern, name string) bool {
	pattern = strings.TrimSpace(pattern)
	name = strings.TrimSpace(name)
	if pattern == "" || name == "" {
		return false
	}
	if strings.EqualFold(pattern, name) {
		return true
	}
	ok, err := filepath.Match(strings.ToLower(pattern), strings.ToLower(name))
	if err != nil {
		log.Printf("[orch] invalid operator gate check pattern %q: %v", pattern, err)
		return false
	}
	return ok
}
