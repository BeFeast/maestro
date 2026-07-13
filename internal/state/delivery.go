package state

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ErrApprovalNotExecuting is returned when a delivery result transition
// (executing → executed/execution_failed) is attempted on an approval that is
// not in status=executing — e.g. it was never claimed, or a concurrent
// executor already recorded a terminal result. It keeps the executing→terminal
// step idempotent so a double-record cannot happen.
var ErrApprovalNotExecuting = fmt.Errorf("approval is not in status=executing")

// DefaultDeliveryOutputLimit bounds the sanitized command/verifier output kept
// on a delivery approval so a chatty deploy cannot bloat state or the API
// payload. Output beyond this is dropped with a truncation marker.
const DefaultDeliveryOutputLimit = 8 << 10 // 8 KiB

// deliverySecretPatterns redact obvious credential shapes so no secret value
// enters the approval record, history, or Fleet API (#872 acceptance). This is
// a defense-in-depth scrub of command/verifier output — projects are expected
// not to echo secrets, but a stray token in a deploy log must never persist.
var deliverySecretPatterns = []*regexp.Regexp{
	// Authorization: Bearer <token>
	regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)\S+`),
	// key=value / key: value for secret-ish keys (token, secret, password, api_key, ...)
	regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?key|secret[_-]?key|token|secret|password|passwd|pwd|auth)\s*[:=]\s*)("?)[^\s"']+`),
	// GitHub tokens (ghp_, gho_, ghu_, ghs_, ghr_ + PAT v2 github_pat_)
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	// Slack tokens
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	// AWS access key ids
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
}

const deliveryRedaction = "[REDACTED]"

// SanitizeDeliveryOutput redacts credential-shaped substrings and bounds the
// result to limit bytes (0 uses DefaultDeliveryOutputLimit). It is applied to
// every command/verifier output and to the command preview before either is
// persisted on a delivery approval.
func SanitizeDeliveryOutput(raw string, limit int) string {
	if limit <= 0 {
		limit = DefaultDeliveryOutputLimit
	}
	out := raw
	for _, re := range deliverySecretPatterns {
		out = re.ReplaceAllStringFunc(out, func(m string) string {
			// Preserve the leading label (capture group 1) where the pattern
			// has one, so "token=..." stays readable as "token=[REDACTED]".
			loc := re.FindStringSubmatchIndex(m)
			if loc != nil && len(loc) >= 4 && loc[2] >= 0 {
				return m[loc[2]:loc[3]] + deliveryRedaction
			}
			return deliveryRedaction
		})
	}
	if len(out) > limit {
		dropped := len(out) - limit
		out = out[:limit] + fmt.Sprintf("\n…[truncated %d bytes]", dropped)
	}
	return out
}

// deliveryApprovalID is the content-addressed id for a deploy_project approval,
// keyed on (repo, project, merged SHA). Re-processing the SAME merge (a daemon
// restart replaying handleMergedPR) mints the SAME id, so the store's
// INSERT OR IGNORE seed is a no-op and no duplicate delivery is created; a NEW
// merge (different SHA) mints a different id and supersedes the stale pending.
func deliveryApprovalID(repo, project, mergedSHA string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(repo)) + "\x00" +
		strings.TrimSpace(project) + "\x00" + strings.TrimSpace(mergedSHA)))
	return "approval-deploy-" + hex.EncodeToString(h[:8])
}

// RecordDeliveryApproval mints (or idempotently returns) a pending
// deploy_project approval carrying payload, and supersedes any still-actionable
// delivery approval for the same project bound to a DIFFERENT merged revision
// so a newer merge never leaves a stale revision approvable (#872).
//
// Returns the live pending approval. Determinism:
//
//   - a pending/approved delivery approval for the same (repo, project) whose
//     MergedSHA differs is marked superseded with a visible audit entry;
//   - an EXISTING approval for the exact same (repo, project, MergedSHA) is
//     returned unchanged if it is still pending (idempotent re-mint on restart)
//     or left alone if it already reached a terminal/executing status (so a
//     completed delivery is never resurrected to pending);
//   - otherwise a fresh pending approval is appended.
func (s *State) RecordDeliveryApproval(payload DeliveryPayload, now time.Time) *Approval {
	if s == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = normalizedTime(now)
	repo := strings.TrimSpace(payload.Repo)
	project := strings.TrimSpace(payload.Project)
	sha := strings.TrimSpace(payload.MergedSHA)
	id := deliveryApprovalID(repo, project, sha)

	// Supersede stale siblings + detect an existing row for this exact revision.
	var existing *Approval
	for i := range s.Approvals {
		a := &s.Approvals[i]
		if a.Action != ApprovalActionDeployProject || a.Delivery == nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(a.Delivery.Repo), repo) ||
			strings.TrimSpace(a.Delivery.Project) != project {
			continue
		}
		if strings.TrimSpace(a.Delivery.MergedSHA) == sha {
			existing = a
			continue
		}
		// Different revision for the same project: supersede while still
		// actionable so it can never be approved into deploying the stale SHA.
		if a.Status == ApprovalStatusPending || a.Status == ApprovalStatusApproved {
			s.markDeliverySuperseded(a, now, fmt.Sprintf("superseded by newer merge %s", shortSHA(sha)))
		}
	}

	if existing != nil {
		if existing.Status == ApprovalStatusPending {
			// Idempotent re-mint (restart replayed the same merge): keep the
			// live record, refresh the informational payload preview fields
			// without churning status or audit.
			existing.Delivery = payload.Clone()
			existing.UpdatedAt = now
			existing.PayloadHash = existing.ComputePayloadHash()
			return existing
		}
		// Already approved/executing/terminal for this exact revision — never
		// resurrect it to pending. Return it as-is.
		return existing
	}

	approval := Approval{
		ID:              id,
		CreatedAt:       now,
		UpdatedAt:       now,
		Action:          ApprovalActionDeployProject,
		Target:          &SupervisorTarget{PR: payload.PR, Issue: payload.Issue, HeadSHA: sha},
		Summary:         deliverySummary(payload),
		Risk:            "delivery: runs the project deploy/install command against " + deliveryTargetLabel(payload),
		Status:          ApprovalStatusPending,
		Repo:            repo,
		Project:         project,
		Delivery:        payload.Clone(),
		TargetStateHash: s.ApprovalTargetStateHash(&SupervisorTarget{PR: payload.PR, Issue: payload.Issue, HeadSHA: sha}),
	}
	approval.PayloadHash = approval.ComputePayloadHash()
	approval.Audit = append(approval.Audit, ApprovalAudit{
		At:          now,
		Event:       ApprovalAuditCreated,
		PayloadHash: approval.PayloadHash,
	})
	s.Approvals = append(s.Approvals, approval)
	return &s.Approvals[len(s.Approvals)-1]
}

func (s *State) markDeliverySuperseded(a *Approval, now time.Time, reason string) {
	if a.Status != ApprovalStatusPending && a.Status != ApprovalStatusApproved {
		return
	}
	a.Status = ApprovalStatusSuperseded
	a.UpdatedAt = normalizedTime(now)
	a.Audit = append(a.Audit, ApprovalAudit{
		At:          a.UpdatedAt,
		Event:       ApprovalAuditSuperseded,
		Reason:      reason,
		PayloadHash: a.PayloadHash,
	})
}

// MarkApprovalExecuting takes the durable approved→executing claim on a
// delivery approval in JSON state (the executor takes the authoritative claim
// in the SQLite store; this mirrors it for the read path). Idempotent at the
// caller boundary: an approval not in status=approved returns
// ErrApprovalNotApproved unchanged so a restart cannot re-claim.
func (s *State) MarkApprovalExecuting(id string, now time.Time, actor, reason string) (*Approval, error) {
	approval, ok := s.FindApproval(id)
	if !ok {
		return nil, ErrApprovalNotFound
	}
	if approval.Status != ApprovalStatusApproved {
		return approval, ErrApprovalNotApproved
	}
	approval.Status = ApprovalStatusExecuting
	approval.UpdatedAt = normalizedTime(now)
	approval.Audit = append(approval.Audit, ApprovalAudit{
		At:          approval.UpdatedAt,
		Event:       ApprovalAuditExecuting,
		Actor:       actor,
		Reason:      reason,
		PayloadHash: approval.PayloadHash,
	})
	return approval, nil
}

// RecordDeliveryResult transitions a claimed delivery approval
// executing → executed (success) or execution_failed, folding the executor's
// recorded result (timings, sanitized output, verified flag) onto the payload.
// Idempotent at the caller boundary via the executing-status guard.
func (s *State) RecordDeliveryResult(id string, success bool, result *DeliveryPayload, now time.Time, actor, summary string) (*Approval, error) {
	approval, ok := s.FindApproval(id)
	if !ok {
		return nil, ErrApprovalNotFound
	}
	if approval.Status != ApprovalStatusExecuting {
		return approval, ErrApprovalNotExecuting
	}
	if result != nil {
		approval.Delivery = result.Clone()
	}
	if success {
		approval.Status = ApprovalStatusExecuted
	} else {
		approval.Status = ApprovalStatusExecutionFailed
	}
	approval.UpdatedAt = normalizedTime(now)
	event := ApprovalAuditExecuted
	if !success {
		event = ApprovalAuditExecutionFailed
	}
	approval.Audit = append(approval.Audit, ApprovalAudit{
		At:          approval.UpdatedAt,
		Event:       event,
		Actor:       actor,
		Reason:      summary,
		PayloadHash: approval.PayloadHash,
	})
	return approval, nil
}

// ListExecutingDeliveries returns every delivery approval stuck in the
// executing state — the operator-reconciliation set surfaced after a daemon
// restart interrupted a delivery mid-flight (#872 safety addendum). The
// executor never replays these automatically; an operator decides.
func (s *State) ListExecutingDeliveries() []*Approval {
	if s == nil {
		return nil
	}
	out := make([]*Approval, 0)
	for i := range s.Approvals {
		a := &s.Approvals[i]
		if a.Action == ApprovalActionDeployProject && a.Status == ApprovalStatusExecuting {
			out = append(out, a)
		}
	}
	return out
}

func deliveryTargetLabel(p DeliveryPayload) string {
	if t := strings.TrimSpace(p.Target); t != "" {
		return t
	}
	return "the configured target"
}

func deliverySummary(p DeliveryPayload) string {
	proj := strings.TrimSpace(p.Project)
	if proj == "" {
		proj = strings.TrimSpace(p.Repo)
	}
	parts := []string{fmt.Sprintf("deploy %s @ %s", proj, shortSHA(p.MergedSHA))}
	if p.PR > 0 {
		parts = append(parts, fmt.Sprintf("(PR #%d)", p.PR))
	}
	if t := strings.TrimSpace(p.Target); t != "" {
		parts = append(parts, "→ "+t)
	}
	return strings.Join(parts, " ")
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "(unknown revision)"
	}
	return sha
}
