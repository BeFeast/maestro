package state

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// ErrApprovalNotExecuting is returned when a delivery result transition
// (executing → executed/execution_failed) is attempted on an approval that is
// not in status=executing — e.g. it was never claimed, or a concurrent
// executor already recorded a terminal result. It keeps the executing→terminal
// step idempotent so a double-record cannot happen.
var ErrApprovalNotExecuting = fmt.Errorf("approval is not in status=executing")

// DefaultDeliveryOutputLimit bounds runtime-only command capture. Delivery
// output is never persisted; the bound only prevents an automatic delivery or
// an executor process from retaining unbounded output in memory.
const DefaultDeliveryOutputLimit = 8 << 10 // 8 KiB

// DeliveryMirrorReconciliationPending is the only operator/client-facing
// detail exposed after the authoritative SQLite delivery transition committed
// but its state.json read mirror could not be written. The underlying error is
// deliberately omitted because os.PathError includes private absolute paths.
const DeliveryMirrorReconciliationPending = "state mirror reconciliation pending"

// deliverySecretPatterns support best-effort, runtime-only rendering for legacy
// callers. Sanitization is not a persistence boundary: unknown credential
// shapes always exist, so deploy approval state uses a strict field allow-list
// and stores no command/output/error text.
var deliverySecretPatterns = []*regexp.Regexp{
	// Authorization: <scheme> <credential> (Bearer, Basic, token, ...)
	regexp.MustCompile(`(?i)(authorization:\s*[a-z][a-z0-9_-]*\s+)\S+`),
	// key=value / key: value for secret-ish keys (token, secret, password, api_key, ...)
	regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?key|secret[_-]?key|token|secret|password|passwd|pwd|auth)\s*[:=]\s*)("?)[^\s"']+`),
	// GitHub tokens (ghp_, gho_, ghu_, ghs_, ghr_ + PAT v2 github_pat_)
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	// Slack tokens
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	// AWS access key ids
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	// JWTs commonly printed without a key label.
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	// PEM private-key blocks. Certificate/public-key blocks are intentionally
	// not matched so non-secret certificate diagnostics stay useful.
	regexp.MustCompile(`(?s)-----BEGIN (?:[A-Z0-9]+ )?PRIVATE KEY-----.*?-----END (?:[A-Z0-9]+ )?PRIVATE KEY-----`),
}

// Credentials embedded in URLs need to preserve the safe scheme/user/host
// context while removing only the password component.
var deliveryURLCredentialPattern = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^/@\s:]+:)[^/@\s]+(@)`)

const deliveryRedaction = "[REDACTED]"

// SanitizeDeliveryOutput redacts common credential-shaped substrings and
// bounds the result to limit bytes (0 uses DefaultDeliveryOutputLimit). It is
// not safe for durable delivery state; persisted delivery records never carry
// command, output, or error text.
func SanitizeDeliveryOutput(raw string, limit int) string {
	if limit <= 0 {
		limit = DefaultDeliveryOutputLimit
	}
	out := raw
	out = deliveryURLCredentialPattern.ReplaceAllString(out, `${1}[REDACTED]${2}`)
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
func deliveryApprovalID(repo, project, mergedSHA, configDigest string, generation int) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(repo)) + "\x00" +
		strings.TrimSpace(project) + "\x00" + strings.TrimSpace(mergedSHA) + "\x00" +
		strings.TrimSpace(configDigest) + "\x00" + fmt.Sprintf("%d", generation)))
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
	payload = *canonicalDeliveryPayload(&payload)
	repo := strings.TrimSpace(payload.Repo)
	project := strings.TrimSpace(payload.Project)
	sha := strings.TrimSpace(payload.MergedSHA)
	digest := strings.TrimSpace(payload.ConfigDigest)

	// Select the highest already-observed generation for this exact merge/spec.
	// The caller always describes the base generation; renewal is a state
	// decision, not caller-controlled input.
	var latestSame *Approval
	highestGeneration := -1
	for i := range s.Approvals {
		a := &s.Approvals[i]
		if a.Action != ApprovalActionDeployProject || a.Delivery == nil ||
			!strings.EqualFold(strings.TrimSpace(a.Delivery.Repo), repo) ||
			strings.TrimSpace(a.Delivery.Project) != project ||
			strings.TrimSpace(a.Delivery.MergedSHA) != sha ||
			strings.TrimSpace(a.Delivery.ConfigDigest) != digest {
			continue
		}
		if a.Delivery.ApprovalGeneration >= highestGeneration {
			highestGeneration = a.Delivery.ApprovalGeneration
			latestSame = a
		}
	}
	if latestSame != nil {
		renew := latestSame.Status == ApprovalStatusSuperseded
		if latestSame.Status == ApprovalStatusStale {
			switch latestSame.Delivery.StaleCause {
			case DeliveryStaleCauseConfigDrift:
				renew = true
			case DeliveryStaleCauseExpired:
				renew = latestSame.Delivery.DeliveryExpired(now) &&
					!payload.ExpiresAt.IsZero() && payload.ExpiresAt.After(now)
			}
		}
		if !renew {
			return latestSame
		}
		payload.ApprovalGeneration = highestGeneration + 1
		payload.StaleCause = ""
		// A superseded same-spec approval can be renewed when config rolls A→B→A
		// for the same merge. It must not revive an older merge behind a genuinely
		// newer GitHub generation; compare every sibling before minting so a
		// standing reconcile cannot create an endless chain of superseded rows.
		for i := range s.Approvals {
			other := &s.Approvals[i]
			if other.Action != ApprovalActionDeployProject || other.Delivery == nil ||
				!strings.EqualFold(strings.TrimSpace(other.Delivery.Repo), repo) ||
				strings.TrimSpace(other.Delivery.Project) != project {
				continue
			}
			if DeliveryGenerationsAmbiguous(other.Delivery, &payload) {
				return latestSame
			}
			if CompareDeliveryGeneration(other.Delivery, other.CreatedAt, &payload, now) > 0 {
				return latestSame
			}
		}
	} else {
		payload.ApprovalGeneration = 0
	}
	id := deliveryApprovalID(repo, project, sha, digest, payload.ApprovalGeneration)

	// Detect an existing row and determine whether this is the newest observed
	// merge/spec generation before mutating anything. A standing reconcile may
	// visit an old code_landed session after a newer one; the old generation
	// must never supersede the new approval merely because it was processed last.
	var existing *Approval
	newerGenerationExists := false
	for i := range s.Approvals {
		a := &s.Approvals[i]
		if a.Action != ApprovalActionDeployProject || a.Delivery == nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(a.Delivery.Repo), repo) ||
			strings.TrimSpace(a.Delivery.Project) != project {
			continue
		}
		if strings.TrimSpace(a.Delivery.MergedSHA) == sha && strings.TrimSpace(a.Delivery.ConfigDigest) == digest &&
			a.Delivery.ApprovalGeneration == payload.ApprovalGeneration {
			existing = a
			continue
		}
		if DeliveryGenerationsAmbiguous(a.Delivery, &payload) {
			// A tied GitHub merged_at timestamp is not an ordering oracle. Keep
			// both candidates actionable: the execution-time freshness fence has
			// an isolated remote ancestry proof and will stale the ancestor while
			// allowing the descendant. Superseding both here makes a safe
			// descendant permanently impossible to execute.
			continue
		}
		if CompareDeliveryGeneration(a.Delivery, a.CreatedAt, &payload, now) > 0 {
			newerGenerationExists = true
		}
	}

	if existing != nil {
		if existing.Status == ApprovalStatusPending {
			// Idempotent re-mint (restart replayed the same merge+spec): keep
			// the immutable payload exactly as approved. In particular, never
			// refresh ExpiresAt on a standing reconcile — doing so would make a
			// pending approval immortal as long as the daemon keeps polling.
			return existing
		}
		// Already approved/executing/terminal for this exact revision — never
		// resurrect it to pending. Return it as-is.
		return existing
	}

	if !newerGenerationExists {
		for i := range s.Approvals {
			a := &s.Approvals[i]
			if a.Action != ApprovalActionDeployProject || a.Delivery == nil ||
				!strings.EqualFold(strings.TrimSpace(a.Delivery.Repo), repo) ||
				strings.TrimSpace(a.Delivery.Project) != project {
				continue
			}
			if DeliveryGenerationsAmbiguous(a.Delivery, &payload) {
				continue
			}
			if a.Status == ApprovalStatusPending || a.Status == ApprovalStatusApproved {
				s.markDeliverySuperseded(a, now, fmt.Sprintf("superseded by newer merge/spec %s", shortSHA(sha)))
			}
		}
	}

	approval := Approval{
		ID:              id,
		CreatedAt:       now,
		UpdatedAt:       now,
		Action:          ApprovalActionDeployProject,
		Target:          &SupervisorTarget{PR: payload.PR, Issue: payload.Issue, HeadSHA: sha},
		Summary:         deliverySummary(payload),
		Risk:            deliveryRiskSummary,
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
	if newerGenerationExists {
		approval.Status = ApprovalStatusSuperseded
		approval.Audit = append(approval.Audit, ApprovalAudit{
			At:          now,
			Event:       ApprovalAuditSuperseded,
			Reason:      "newer delivery generation is already active",
			PayloadHash: approval.PayloadHash,
		})
	}
	s.Approvals = append(s.Approvals, approval)
	return &s.Approvals[len(s.Approvals)-1]
}

// DeliveryGenerationsAmbiguous reports the ordering case state/SQLite cannot
// safely resolve without a repository topology proof: distinct merge SHAs with
// the same authoritative GitHub merged_at timestamp. PR number and observation
// order are not ancestry. Both candidates remain actionable until the
// execution-time isolated-remote ancestry fence proves which one descends from
// the other (or fails closed if they are genuinely incomparable).
func DeliveryGenerationsAmbiguous(a, b *DeliveryPayload) bool {
	if a == nil || b == nil || a.MergedAt.IsZero() || b.MergedAt.IsZero() || !a.MergedAt.Equal(b.MergedAt) {
		return false
	}
	return strings.TrimSpace(a.MergedSHA) != strings.TrimSpace(b.MergedSHA)
}

// CompareDeliveryGeneration returns -1/0/+1 for a relative to b. GitHub's
// merged_at is authoritative; PR number and observation time are deterministic
// fallbacks for historical payloads and same-merge config-digest generations.
func CompareDeliveryGeneration(a *DeliveryPayload, aCreated time.Time, b *DeliveryPayload, bCreated time.Time) int {
	if a == nil || b == nil {
		return 0
	}
	if !a.MergedAt.IsZero() && !b.MergedAt.IsZero() && !a.MergedAt.Equal(b.MergedAt) {
		if a.MergedAt.Before(b.MergedAt) {
			return -1
		}
		return 1
	}
	if DeliveryGenerationsAmbiguous(a, b) {
		return 0
	}
	if a.PR != b.PR {
		if a.PR < b.PR {
			return -1
		}
		return 1
	}
	if strings.TrimSpace(a.MergedSHA) == strings.TrimSpace(b.MergedSHA) &&
		strings.TrimSpace(a.ConfigDigest) == strings.TrimSpace(b.ConfigDigest) &&
		a.ApprovalGeneration != b.ApprovalGeneration {
		if a.ApprovalGeneration < b.ApprovalGeneration {
			return -1
		}
		return 1
	}
	if !aCreated.Equal(bCreated) {
		if aCreated.Before(bCreated) {
			return -1
		}
		return 1
	}
	return 0
}

func (s *State) markDeliverySuperseded(a *Approval, now time.Time, reason string) {
	if a.Status != ApprovalStatusPending && a.Status != ApprovalStatusApproved {
		return
	}
	a.Status = ApprovalStatusSuperseded
	a.UpdatedAt = normalizedTime(now)
	reason = deliveryAuditReason(ApprovalAuditSuperseded)
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
	if approval.Action == ApprovalActionDeployProject {
		reason = deliveryAuditReason(ApprovalAuditExecuting)
	}
	approval.Audit = append(approval.Audit, ApprovalAudit{
		At:          approval.UpdatedAt,
		Event:       ApprovalAuditExecuting,
		Actor:       actor,
		Reason:      reason,
		PayloadHash: approval.PayloadHash,
	})
	return approval, nil
}

// DeliveryExpired reports whether this delivery approval has passed its
// immutable expiry. Zero means no expiry for backward-compatible historical
// records; every newly minted #872 approval sets an explicit value.
func (p *DeliveryPayload) DeliveryExpired(now time.Time) bool {
	if p == nil || p.ExpiresAt.IsZero() {
		return false
	}
	return !normalizedTime(now).Before(normalizedTime(p.ExpiresAt))
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
	approval.Delivery = MergeDeliveryResult(approval.Delivery, result)
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
	summary = deliveryAuditReason(event)
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

const deliveryRiskSummary = "approval-gated post-merge delivery"

const (
	DeliveryFailureStagePrecondition          = "precondition"
	DeliveryFailureStageCheckout              = "checkout"
	DeliveryFailureStageDeploy                = "deploy"
	DeliveryFailureStageVerify                = "verify"
	DeliveryFailureStageCleanup               = "cleanup"
	DeliveryStaleCauseConfigDrift             = "config_drift"
	DeliveryStaleCauseExpired                 = "expired"
	DeliveryStaleCauseIntegrity               = "integrity"
	DeliveryStaleCauseOther                   = "other"
	DeliveryCompletionSourceOperatorReconcile = "operator_reconcile"
	DeliveryReconcileOutcomeVerified          = "verified"
	DeliveryReconcileOutcomeNotApplied        = "not_applied"
	DeliveryReconcileOutcomeRemediatedFailed  = "remediated_failed"
)

// CanonicalDeliveryApproval returns the only shape a deploy_project approval
// may write to JSON/SQLite. It is intentionally stronger than redaction: all
// generic free-text fields and all non-delivery payloads are discarded, the
// target is reduced to numeric GitHub coordinates + the pinned SHA, and audit
// messages become fixed event labels. This makes a caller-provided secret in a
// summary/risk/reason structurally impossible to persist. It preserves the
// recorded PayloadHash so read-side projection cannot hide tampering; trusted
// mint/write paths must call CanonicalDeliveryApprovalForWrite.
func CanonicalDeliveryApproval(a *Approval) *Approval {
	if a == nil {
		return nil
	}
	cp := *a
	cp.Action = ApprovalActionDeployProject
	cp.DecisionID = ""
	cp.Evidence = nil
	cp.LessonProposalID = ""
	cp.ReviewRepair = nil
	cp.Delivery = canonicalDeliveryPayload(a.Delivery)
	if cp.Delivery != nil {
		cp.Repo = strings.TrimSpace(cp.Delivery.Repo)
		cp.Project = strings.TrimSpace(cp.Delivery.Project)
		cp.Summary = deliverySummary(*cp.Delivery)
	}
	cp.Risk = deliveryRiskSummary
	if a.Target != nil {
		cp.Target = &SupervisorTarget{
			Issue:   a.Target.Issue,
			PR:      a.Target.PR,
			HeadSHA: strings.TrimSpace(a.Target.HeadSHA),
		}
	} else if cp.Delivery != nil {
		cp.Target = &SupervisorTarget{Issue: cp.Delivery.Issue, PR: cp.Delivery.PR, HeadSHA: cp.Delivery.MergedSHA}
	}
	cp.Audit = make([]ApprovalAudit, 0, len(a.Audit))
	for _, entry := range a.Audit {
		if !canonicalDeliveryAuditEvent(entry.Event) {
			continue
		}
		entry.Actor = canonicalDeliveryActor(entry.Actor)
		entry.Reason = deliveryAuditReason(entry.Event)
		entry.TargetStateHash = cp.TargetStateHash
		cp.Audit = append(cp.Audit, entry)
	}
	return &cp
}

// CanonicalDeliveryApprovalForWrite canonicalizes a trusted mint/transition
// and deliberately rehashes the immutable payload. Never use it on an
// unverified DB/JSON read: doing so would make a tampered payload self-consistent.
func CanonicalDeliveryApprovalForWrite(a *Approval) *Approval {
	cp := CanonicalDeliveryApproval(a)
	if cp == nil {
		return nil
	}
	cp.PayloadHash = cp.ComputePayloadHash()
	for i := range cp.Audit {
		cp.Audit[i].PayloadHash = cp.PayloadHash
	}
	return cp
}

// normalizeDeliveryApprovals projects legacy state.json delivery rows onto the
// strict allow-list in memory. Load calls it after JSON decode and does not
// rewrite the file; this prevents an old free-text field from reaching Fleet
// before SQLite reconciliation while preserving non-delivery history.
func (s *State) normalizeDeliveryApprovals() {
	if s == nil {
		return
	}
	for i := range s.Approvals {
		if s.Approvals[i].Action != ApprovalActionDeployProject {
			continue
		}
		canonical := CanonicalDeliveryApproval(&s.Approvals[i])
		if canonical != nil {
			s.Approvals[i] = *canonical
		}
	}
}

func canonicalDeliveryPayload(p *DeliveryPayload) *DeliveryPayload {
	if p == nil {
		return nil
	}
	cp := p.Clone()
	cp.Project = boundedDeliveryLabel(cp.Project, 256)
	cp.Repo = boundedDeliveryLabel(cp.Repo, 256)
	cp.MergedSHA = strings.ToLower(strings.TrimSpace(cp.MergedSHA))
	if cp.ApprovalGeneration < 0 {
		cp.ApprovalGeneration = 0
	}
	cp.ExecutedRevision = strings.ToLower(strings.TrimSpace(cp.ExecutedRevision))
	cp.ConfigDigest = strings.TrimSpace(cp.ConfigDigest)
	cp.TargetLabel = boundedDeliveryLabel(cp.TargetLabel, 256)
	cp.VerificationLabel = boundedDeliveryLabel(cp.VerificationLabel, 512)
	cp.RollbackLabel = boundedDeliveryLabel(cp.RollbackLabel, 512)
	switch cp.FailureStage {
	case "", DeliveryFailureStagePrecondition, DeliveryFailureStageCheckout,
		DeliveryFailureStageDeploy, DeliveryFailureStageVerify, DeliveryFailureStageCleanup:
	default:
		cp.FailureStage = DeliveryFailureStagePrecondition
	}
	switch cp.StaleCause {
	case "", DeliveryStaleCauseConfigDrift, DeliveryStaleCauseExpired,
		DeliveryStaleCauseIntegrity, DeliveryStaleCauseOther:
	default:
		cp.StaleCause = DeliveryStaleCauseOther
	}
	switch cp.CompletionSource {
	case "", DeliveryCompletionSourceOperatorReconcile:
	default:
		cp.CompletionSource = ""
	}
	switch cp.ReconcileOutcome {
	case "", DeliveryReconcileOutcomeVerified, DeliveryReconcileOutcomeNotApplied,
		DeliveryReconcileOutcomeRemediatedFailed:
	default:
		cp.ReconcileOutcome = ""
	}
	return cp
}

func boundedDeliveryLabel(value string, limit int) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, "�"))
	// Config validation rejects controls for newly-minted approvals, but this
	// projection also handles legacy/untrusted JSON. Drop them here so an old
	// row cannot inject newlines or terminal/UI controls into durable history.
	runes := make([]rune, 0, len(value))
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		runes = append(runes, r)
	}
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return string(runes)
}

func deliveryAuditReason(event string) string {
	switch event {
	case ApprovalAuditCreated:
		return "delivery approval created"
	case ApprovalAuditApproved:
		return "delivery approved"
	case ApprovalAuditRejected:
		return "delivery rejected"
	case ApprovalAuditStale:
		return "delivery approval stale"
	case ApprovalAuditSuperseded:
		return "delivery generation superseded"
	case ApprovalAuditExecuting:
		return "delivery execution claimed"
	case ApprovalAuditExecutionReleased:
		return "delivery claim released before side effect"
	case ApprovalAuditExecuted:
		return "delivery verified"
	case ApprovalAuditExecutionFailed:
		return "delivery execution failed"
	case ApprovalAuditExecutionSkipped:
		return "delivery execution skipped"
	case ApprovalAuditAwaitingDispatch:
		return "delivery awaiting dispatch"
	case ApprovalAuditDeliveryReconciled:
		return "delivery reconciled by operator"
	default:
		return ""
	}
}

func canonicalDeliveryAuditEvent(event string) bool {
	switch event {
	case ApprovalAuditCreated, ApprovalAuditApproved, ApprovalAuditRejected,
		ApprovalAuditStale, ApprovalAuditSuperseded, ApprovalAuditExecuting,
		ApprovalAuditExecutionReleased, ApprovalAuditExecuted,
		ApprovalAuditExecutionFailed, ApprovalAuditExecutionSkipped,
		ApprovalAuditAwaitingDispatch, ApprovalAuditDeliveryReconciled:
		return true
	default:
		return false
	}
}

// canonicalDeliveryActor keeps only a compact operator/service identity. Audit
// actor is not a free-text note: invalid legacy/CLI input becomes a fixed code
// so it cannot carry control sequences or arbitrary secret-bearing prose into
// durable approval history.
func canonicalDeliveryActor(actor string) string {
	actor = strings.TrimSpace(strings.ToValidUTF8(actor, "�"))
	if actor == "" {
		return ""
	}
	runes := []rune(actor)
	if len(runes) > 64 {
		return "unknown"
	}
	for _, r := range runes {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || strings.ContainsRune("._@:/+-", r) {
			continue
		}
		return "unknown"
	}
	return actor
}

// MergeDeliveryResult preserves the immutable approval payload and copies only
// allow-listed execution metadata from result. Callers cannot smuggle changed
// labels, coordinates, or free text into the terminal record.
func MergeDeliveryResult(immutable, result *DeliveryPayload) *DeliveryPayload {
	base := canonicalDeliveryPayload(immutable)
	if base == nil || result == nil {
		return base
	}
	base.StartedAt = result.StartedAt
	base.FinishedAt = result.FinishedAt
	base.FailureStage = result.FailureStage
	base.DeployExitCode = cloneExitCode(result.DeployExitCode)
	base.VerifyExitCode = cloneExitCode(result.VerifyExitCode)
	base.TimedOut = result.TimedOut
	base.CleanupFailed = result.CleanupFailed
	base.Verified = result.Verified
	base.ExecutedRevision = result.ExecutedRevision
	base.CompletionSource = result.CompletionSource
	base.ReconcileOutcome = result.ReconcileOutcome
	return canonicalDeliveryPayload(base)
}

func cloneExitCode(code *int) *int {
	if code == nil {
		return nil
	}
	cp := *code
	return &cp
}

func deliverySummary(p DeliveryPayload) string {
	parts := []string{fmt.Sprintf("deploy pinned revision %s", shortSHA(p.MergedSHA))}
	if p.PR > 0 {
		parts = append(parts, fmt.Sprintf("(PR #%d)", p.PR))
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
