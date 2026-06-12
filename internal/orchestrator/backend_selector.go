package orchestrator

import (
	"log"
	"sort"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/state"
)

const (
	selectionReasonProviderLimitFallback = "fallback_after_provider_limit"
	selectionReasonAuthFailureFallback   = "fallback_after_backend_auth_failure"
	// selectionReasonDispatchBlockedFallback marks a fresh dispatch whose
	// routed backend was blocked (disabled or in BackendHealth cooldown), so
	// the first healthy candidate was substituted before spawn (#695).
	selectionReasonDispatchBlockedFallback = "dispatch_fallback_blocked_backend"
)

// backendAuthFailureCooldown is how long an auth-failed backend stays gated
// before the fallback selector may pick it again. Credential failures clear
// when the operator (or the cred-sync agent) refreshes the token, and the
// provider states no reset time, so a short fixed window re-probes the
// backend a few times an hour instead of per-spawn (#693).
const backendAuthFailureCooldown = 10 * time.Minute

// recordProviderLimit marks the session's backend as rate-limited and puts it
// into cooldown. resetAt, when non-nil, is the provider-stated reset time
// parsed from the limit message ("try again at ..."); it is surfaced on the
// session and used as the backend's RetryAfter so the fallback selector treats
// the backend as available again once the reset time passes (auto-resume).
func (o *Orchestrator) recordProviderLimit(st *state.State, slotName string, sess *state.Session, pattern string, resetAt *time.Time, now time.Time) {
	if st == nil || sess == nil || sess.Backend == "" {
		return
	}
	if st.BackendHealth == nil {
		st.BackendHealth = make(map[string]state.BackendHealth)
	}
	if pattern == "" {
		pattern = state.BackendBlockProviderLimit
	}
	sess.RateLimitHit = true
	sess.ProviderLimitBackend = sess.Backend
	sess.ProviderLimitReason = pattern
	sess.ProviderLimitResetAt = resetAt
	health := state.BackendHealth{
		State:       state.BackendHealthCooldown,
		Reason:      state.BackendBlockProviderLimit,
		Pattern:     pattern,
		Since:       now.UTC(),
		LastSession: slotName,
	}
	if resetAt != nil {
		retryAfter := resetAt.UTC()
		health.RetryAfter = &retryAfter
	}
	st.BackendHealth[sess.Backend] = health
}

// recordBackendAuthFailure marks the session's backend as failing
// authentication and puts it into cooldown (#693). Unlike a provider
// capacity limit there is no provider-stated reset time, so RetryAfter is a
// fixed short window (backendAuthFailureCooldown) after which the selector
// re-probes the backend automatically. RateLimitHit is set because it is the
// shared "transient backend block" marker: FailedAttemptsForIssue excludes
// such sessions, so the auth outage does not burn the per-issue retry budget.
func (o *Orchestrator) recordBackendAuthFailure(st *state.State, slotName string, sess *state.Session, pattern string, now time.Time) {
	if st == nil || sess == nil || sess.Backend == "" {
		return
	}
	if st.BackendHealth == nil {
		st.BackendHealth = make(map[string]state.BackendHealth)
	}
	if pattern == "" {
		pattern = state.BackendBlockAuthFailure
	}
	sess.RateLimitHit = true
	sess.ProviderLimitBackend = sess.Backend
	sess.ProviderLimitReason = state.BackendBlockAuthFailure
	retryAfter := now.UTC().Add(backendAuthFailureCooldown)
	st.BackendHealth[sess.Backend] = state.BackendHealth{
		State:       state.BackendHealthCooldown,
		Reason:      state.BackendBlockAuthFailure,
		Pattern:     pattern,
		Since:       now.UTC(),
		RetryAfter:  &retryAfter,
		LastSession: slotName,
	}
}

func (o *Orchestrator) selectProviderLimitFallback(st *state.State, sess *state.Session, now time.Time) state.BackendSelection {
	return o.selectBackendFallback(st, sess, now, selectionReasonProviderLimitFallback)
}

func (o *Orchestrator) selectAuthFailureFallback(st *state.State, sess *state.Session, now time.Time) state.BackendSelection {
	return o.selectBackendFallback(st, sess, now, selectionReasonAuthFailureFallback)
}

// selectBackendFallback picks the next backend for a session whose current
// backend is blocked (provider limit or auth failure), walking
// backendFallbackCandidates and skipping disabled, already-tried, current,
// and cooling-down backends. selectionReason records which failure class
// triggered the fallover in the session's BackendSelection audit record.
func (o *Orchestrator) selectBackendFallback(st *state.State, sess *state.Session, now time.Time, selectionReason string) state.BackendSelection {
	selection := state.BackendSelection{
		SelectionReason: selectionReason,
		PreviousBackend: backendName(sess),
	}
	for _, candidate := range o.backendFallbackCandidates(sess) {
		entry := state.BackendCandidate{Backend: candidate, Fit: 0.5, Policy: 0.5, Final: 0.5}
		if candidate == "" {
			continue
		}
		backendDef, ok := o.cfg.Model.Backends[candidate]
		if !ok {
			entry.Available = false
			entry.BlockedBy = state.BackendBlockUnknown
			selection.CandidateScores = append(selection.CandidateScores, entry)
			continue
		}
		if !backendDef.IsEnabled() {
			entry.Available = false
			entry.BlockedBy = state.BackendBlockDisabled
			selection.CandidateScores = append(selection.CandidateScores, entry)
			continue
		}
		if candidate == backendName(sess) {
			entry.Available = false
			entry.BlockedBy = state.BackendBlockCurrent
			selection.CandidateScores = append(selection.CandidateScores, entry)
			continue
		}
		if stringInSlice(candidate, sess.TriedBackends) {
			entry.Available = false
			entry.BlockedBy = state.BackendBlockAlreadyTried
			selection.CandidateScores = append(selection.CandidateScores, entry)
			continue
		}
		if health, ok := st.BackendHealth[candidate]; ok && health.State == state.BackendHealthCooldown {
			if health.RetryAfter == nil || now.Before(*health.RetryAfter) {
				entry.Available = false
				entry.BlockedBy = health.Reason
				if health.RetryAfter != nil {
					entry.RetryAfter = health.RetryAfter.Format(time.RFC3339)
				}
				selection.CandidateScores = append(selection.CandidateScores, entry)
				continue
			}
		}

		entry.Available = true
		entry.Fit = backendFitScore(candidate, o.cfg)
		entry.Policy = backendPolicyScore(candidate, o.cfg)
		entry.Final = (entry.Fit + entry.Policy) / 2
		selection.CandidateScores = append(selection.CandidateScores, entry)
		if selection.SelectedBackend == "" {
			selection.SelectedBackend = candidate
		}
	}
	return selection
}

// resolveDispatchBackend resolves the backend for a fresh dispatch and gates
// the routed choice on BackendHealth (#695). Without this gate the router's
// decision never saw cooldowns, so during a credential outage every new
// dispatch spawned a doomed ~2-minute worker on the gated default backend
// once per poll cycle. When the routed backend is blocked — disabled, or
// cooling down after an auth failure / provider limit — the first healthy
// candidate from dispatchBackendCandidates is substituted, with a log line
// naming the substitution. ok=false means every candidate is blocked too;
// retryAt then carries the earliest known cooldown expiry (nil when none of
// the blocked backends states one) so the caller can pause fresh dispatch
// until a cooldown lapses instead of spawn-die churning.
func (o *Orchestrator) resolveDispatchBackend(st *state.State, issue github.Issue, now time.Time) (decision router.BackendDecision, ok bool, retryAt *time.Time) {
	decision = o.resolveBackendDecision(issue)
	blockedBy, blockedRetry := o.dispatchBackendBlock(st, decision.Backend, now)
	if blockedBy == "" {
		return decision, true, nil
	}
	earliest := blockedRetry
	for _, candidate := range o.dispatchBackendCandidates() {
		if candidate == decision.Backend {
			continue
		}
		candidateBlock, candidateRetry := o.dispatchBackendBlock(st, candidate, now)
		if candidateBlock != "" {
			if candidateRetry != nil && (earliest == nil || candidateRetry.Before(*earliest)) {
				earliest = candidateRetry
			}
			continue
		}
		log.Printf("[orch] issue #%d: backend %s blocked for fresh dispatch (%s%s) — substituting %s",
			issue.Number, decision.Backend, blockedBy, retryAfterHint(blockedRetry), candidate)
		return router.BackendDecision{
			Backend:  candidate,
			Reason:   selectionReasonDispatchBlockedFallback,
			TaskType: decision.TaskType,
		}, true, nil
	}
	return decision, false, earliest
}

// dispatchBackendBlock reports why a backend cannot receive a fresh dispatch,
// or "" when it can. The skip rules mirror selectBackendFallback: unknown and
// disabled backends are never dispatchable, and a backend in BackendHealth
// cooldown stays blocked until its RetryAfter passes (or indefinitely while
// RetryAfter is unset). retryAfter is the cooldown expiry when one exists.
func (o *Orchestrator) dispatchBackendBlock(st *state.State, name string, now time.Time) (blockedBy string, retryAfter *time.Time) {
	backendDef, ok := o.cfg.Model.Backends[name]
	if !ok {
		return state.BackendBlockUnknown, nil
	}
	if !backendDef.IsEnabled() {
		return state.BackendBlockDisabled, nil
	}
	if st == nil {
		return "", nil
	}
	if health, ok := st.BackendHealth[name]; ok && health.State == state.BackendHealthCooldown {
		if health.RetryAfter == nil || now.Before(*health.RetryAfter) {
			reason := health.Reason
			if reason == "" {
				reason = state.BackendHealthCooldown
			}
			return reason, health.RetryAfter
		}
	}
	return "", nil
}

// dispatchBackendCandidates is the substitution order for a fresh dispatch
// whose routed backend is blocked: the default backend first, then the
// configured fallback chain — the same chain selectBackendFallback walks —
// and, only when no explicit chain is configured, the remaining configured
// backends sorted by name.
func (o *Orchestrator) dispatchBackendCandidates() []string {
	seen := make(map[string]bool)
	ordered := make([]string, 0, len(o.cfg.Model.Backends)+1)
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		ordered = append(ordered, name)
	}
	add(o.cfg.Model.Default)
	for _, name := range o.cfg.Model.FallbackBackends {
		add(name)
	}
	if len(o.cfg.Model.FallbackBackends) > 0 {
		return ordered
	}
	remaining := make([]string, 0, len(o.cfg.Model.Backends))
	for name := range o.cfg.Model.Backends {
		if !seen[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	for _, name := range remaining {
		add(name)
	}
	return ordered
}

// retryAfterHint formats a cooldown expiry for dispatch log lines.
func retryAfterHint(retryAfter *time.Time) string {
	if retryAfter == nil {
		return ""
	}
	return ", retry after " + retryAfter.UTC().Format(time.RFC3339)
}

func (o *Orchestrator) backendFallbackCandidates(sess *state.Session) []string {
	seen := make(map[string]bool)
	ordered := make([]string, 0, len(o.cfg.Model.Backends)+len(o.cfg.Model.FallbackBackends)+1)
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		ordered = append(ordered, name)
	}
	for _, name := range o.cfg.Model.FallbackBackends {
		add(name)
	}
	if len(o.cfg.Model.FallbackBackends) > 0 {
		return ordered
	}
	add(o.cfg.Model.Default)

	remaining := make([]string, 0, len(o.cfg.Model.Backends))
	for name := range o.cfg.Model.Backends {
		if !seen[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	for _, name := range remaining {
		add(name)
	}
	return ordered
}

func backendFitScore(name string, cfg *config.Config) float64 {
	if name == cfg.Model.Default {
		return 0.8
	}
	return 0.6
}

func backendPolicyScore(name string, cfg *config.Config) float64 {
	if name == cfg.Model.Default {
		return 0.9
	}
	return 0.6
}

func backendName(sess *state.Session) string {
	if sess == nil {
		return ""
	}
	return sess.Backend
}

func stringInSlice(needle string, values []string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
