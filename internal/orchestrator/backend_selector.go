package orchestrator

import (
	"log"
	"sort"
	"strings"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/router"
	"github.com/befeast/maestro/internal/state"
)

const (
	selectionReasonProviderLimitFallback    = "fallback_after_provider_limit"
	selectionReasonAuthFailureFallback      = "fallback_after_backend_auth_failure"
	selectionReasonModelUnavailableFallback = "fallback_after_backend_model_unavailable"
	selectionReasonModelCooldownFallback    = "fallback_after_backend_model_cooldown"
	selectionReasonUsageLimitFallback       = "fallback_after_backend_usage_limit"
	// selectionReasonDispatchBlockedFallback marks a fresh dispatch whose
	// routed backend was blocked (disabled or in BackendHealth cooldown), so
	// the first healthy candidate was substituted before spawn (#695).
	selectionReasonDispatchBlockedFallback = "dispatch_fallback_blocked_backend"
	// selectionReasonRetryBlockedFallback marks a scheduled retry whose
	// session backend was blocked at respawn time (in BackendHealth cooldown
	// or disabled), so the first healthy fallback was substituted instead of
	// re-burning the retry budget on a dead backend (#805).
	selectionReasonRetryBlockedFallback = "retry_fallback_blocked_backend"
)

// backendFailureCopy holds the reason-dependent strings the dead-worker
// handlers use to gate a backend and respawn on a fallback (#693/#713). Auth
// failures and model-unavailability share the exact mechanism (preserve the
// retry budget, gate the backend, fall over) and differ only in operator copy
// and the audit/display tokens, so the handlers stay single-path and select
// the wording from here by gating reason.
type backendFailureCopy struct {
	selectionReason string // BackendSelection audit reason
	displayToken    string // Session.LastNotifiedStatus / display status
	desc            string // verb phrase, e.g. "failed authentication"
	noun            string // log noun, e.g. "backend auth failure"
	remedy          string // operator remediation hint
}

func backendFailureCopyFor(reason string) backendFailureCopy {
	switch reason {
	case state.BackendBlockModelUnavailable:
		return backendFailureCopy{
			selectionReason: selectionReasonModelUnavailableFallback,
			displayToken:    string(state.DisplayBackendModelUnavailable),
			desc:            "could not load its configured model (unavailable or no access)",
			noun:            "backend model unavailable",
			remedy:          "swap the model id or restore model access",
		}
	case state.BackendBlockUsageLimit:
		return backendFailureCopy{
			selectionReason: selectionReasonUsageLimitFallback,
			displayToken:    string(state.DisplayBackendUsageLimit),
			desc:            "exhausted its account usage quota",
			noun:            "backend usage limit",
			remedy:          "wait for the provider quota window to reset or raise the plan limit",
		}
	case state.BackendBlockModelCooldown:
		return backendFailureCopy{
			selectionReason: selectionReasonModelCooldownFallback,
			displayToken:    string(state.DisplayBackendModelCooldown),
			desc:            "exhausted compatible credentials for the requested model",
			noun:            "provider/model credential cooldown",
			remedy:          "wait for the route retry time or restore model access on a compatible credential",
		}
	}
	return backendFailureCopy{
		selectionReason: selectionReasonAuthFailureFallback,
		displayToken:    string(state.DisplayBackendAuthFailure),
		desc:            "failed authentication",
		noun:            "backend auth failure",
		remedy:          "fix credentials",
	}
}

type backendFailure struct {
	reason                    string
	pattern                   string
	provider                  string
	model                     string
	credentialCandidates      int
	credentialCandidatesKnown bool
	credentialUsable          int
	credentialUsableKnown     bool
	aggregateReason           string
	retryAfter                *time.Time
	modelScoped               bool
}

// backendAuthFailureCooldown is how long an auth-failed backend stays gated
// before the fallback selector may pick it again. Credential failures clear
// when the operator (or the cred-sync agent) refreshes the token, and the
// provider states no reset time, so a short fixed window re-probes the
// backend a few times an hour instead of per-spawn (#693).
const backendAuthFailureCooldown = 10 * time.Minute

// backendUsageLimitCooldown is the gate window for a quota-exhausted backend
// whose reset time could not be parsed (#805) — a quota death WITH a
// parseable reset goes through the provider-limit path and carries the
// provider's own RetryAfter instead. Quota windows reset on multi-hour
// boundaries (5-hour session windows, weekly caps), so re-probing is slower
// than the auth cooldown: fast enough to resume within half an hour of an
// unknown reset, slow enough not to spawn-die churn against a window that
// stays exhausted for hours.
const backendUsageLimitCooldown = 30 * time.Minute

// backendFailureCooldownFor picks the fixed re-probe window for a hard
// backend failure by gating reason.
func backendFailureCooldownFor(reason string) time.Duration {
	if reason == state.BackendBlockUsageLimit {
		return backendUsageLimitCooldown
	}
	return backendAuthFailureCooldown
}

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
	sess.ProviderLimitProvider = ""
	sess.ProviderLimitModel = ""
	sess.CredentialCandidates = 0
	sess.CredentialCandidatesKnown = false
	sess.CredentialUsable = 0
	sess.CredentialUsableKnown = false
	sess.CredentialAggregateReason = ""
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

// recordBackendFailure marks the session's backend as failing for a hard,
// non-work reason (auth/credential outage #693, model unavailable #713, or
// account quota exhausted #805) and puts it into cooldown. Unlike a provider
// capacity limit there is no provider-stated reset time, so RetryAfter is a
// fixed window picked by gating reason (backendFailureCooldownFor) after
// which the selector re-probes the backend automatically. RateLimitHit is
// set because it is the shared "transient backend block" marker:
// FailedAttemptsForIssue excludes such sessions, so the outage does not burn
// the per-issue retry budget. reason is the gating reason recorded on both
// the session and BackendHealth so the supervisor and dashboard can tell an
// auth outage from a missing model from an exhausted quota.
func (o *Orchestrator) recordBackendFailure(st *state.State, slotName string, sess *state.Session, failure backendFailure, now time.Time) {
	if st == nil || sess == nil || sess.Backend == "" {
		return
	}
	reason := failure.reason
	pattern := failure.pattern
	if reason == "" {
		reason = state.BackendBlockAuthFailure
	}
	if pattern == "" {
		pattern = reason
	}
	sess.RateLimitHit = true
	sess.ProviderLimitBackend = sess.Backend
	sess.ProviderLimitReason = reason
	retryAfter := failure.retryAfter
	if retryAfter == nil {
		fallbackRetry := now.UTC().Add(backendFailureCooldownFor(reason))
		retryAfter = &fallbackRetry
	}
	sess.ProviderLimitResetAt = retryAfter
	provider, model := o.providerModelRouteForSession(sess, failure.provider, failure.model)
	sess.ProviderLimitProvider = provider
	sess.ProviderLimitModel = model
	sess.CredentialCandidates = failure.credentialCandidates
	sess.CredentialCandidatesKnown = failure.credentialCandidatesKnown
	sess.CredentialUsable = failure.credentialUsable
	sess.CredentialUsableKnown = failure.credentialUsableKnown
	sess.CredentialAggregateReason = failure.aggregateReason
	health := state.BackendHealth{
		State:       state.BackendHealthCooldown,
		Reason:      reason,
		Pattern:     pattern,
		Provider:    provider,
		Model:       model,
		Since:       now.UTC(),
		RetryAfter:  retryAfter,
		LastSession: slotName,
	}
	health.CredentialCandidates = failure.credentialCandidates
	health.CredentialCandidatesKnown = failure.credentialCandidatesKnown
	health.CredentialUsable = failure.credentialUsable
	health.CredentialUsableKnown = failure.credentialUsableKnown
	health.AggregateReason = failure.aggregateReason
	if failure.modelScoped && provider != "" && model != "" {
		if st.ProviderModelHealth == nil {
			st.ProviderModelHealth = make(map[string]map[string]state.BackendHealth)
		}
		if st.ProviderModelHealth[provider] == nil {
			st.ProviderModelHealth[provider] = make(map[string]state.BackendHealth)
		}
		st.ProviderModelHealth[provider][model] = health
		return
	}
	if st.BackendHealth == nil {
		st.BackendHealth = make(map[string]state.BackendHealth)
	}
	st.BackendHealth[sess.Backend] = health
}

func (o *Orchestrator) selectProviderLimitFallback(st *state.State, sess *state.Session, now time.Time) state.BackendSelection {
	return o.selectBackendFallback(st, sess, now, selectionReasonProviderLimitFallback)
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
		provider, model := o.providerModelRouteForBackend(candidate, "")
		entry := state.BackendCandidate{Backend: candidate, Provider: provider, Model: model, Fit: 0.5, Policy: 0.5, Final: 0.5}
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
		if health, ok := providerModelHealth(st, provider, model); ok && health.State == state.BackendHealthCooldown {
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
	blockedBy, blockedRetry := o.dispatchBackendBlock(st, decision.Backend, decision.Model, now)
	if blockedBy == "" {
		return decision, true, nil
	}
	earliest := blockedRetry
	for _, candidate := range o.dispatchBackendCandidates() {
		if candidate == decision.Backend {
			continue
		}
		candidateBlock, candidateRetry := o.dispatchBackendBlock(st, candidate, "", now)
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
func (o *Orchestrator) dispatchBackendBlock(st *state.State, name, modelOverride string, now time.Time) (blockedBy string, retryAfter *time.Time) {
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
	provider, model := o.providerModelRouteForBackend(name, modelOverride)
	if health, ok := providerModelHealth(st, provider, model); ok && health.State == state.BackendHealthCooldown {
		if health.RetryAfter == nil || now.Before(*health.RetryAfter) {
			reason := health.Reason
			if reason == "" {
				reason = state.BackendBlockModelCooldown
			}
			return reason, health.RetryAfter
		}
	}
	return "", nil
}

func providerModelHealth(st *state.State, provider, model string) (state.BackendHealth, bool) {
	if st == nil || provider == "" || model == "" {
		return state.BackendHealth{}, false
	}
	models := st.ProviderModelHealth[provider]
	if models == nil {
		return state.BackendHealth{}, false
	}
	health, ok := models[model]
	return health, ok
}

func (o *Orchestrator) providerModelRouteForSession(sess *state.Session, providerOverride, modelOverride string) (string, string) {
	if sess == nil {
		return strings.TrimSpace(providerOverride), strings.TrimSpace(modelOverride)
	}
	model := strings.TrimSpace(modelOverride)
	if model == "" && sess.BackendSelection != nil {
		model = strings.TrimSpace(sess.BackendSelection.Model)
	}
	if model == "" {
		model = strings.TrimSpace(sess.Model)
	}
	provider, configuredModel := o.providerModelRouteForBackend(sess.Backend, model)
	configuredProvider := ""
	if o != nil && o.cfg != nil {
		configuredProvider = strings.TrimSpace(o.cfg.Model.Backends[sess.Backend].Provider)
	}
	if strings.TrimSpace(providerOverride) != "" && configuredProvider == "" {
		provider = strings.TrimSpace(providerOverride)
	}
	if model == "" {
		model = configuredModel
	}
	return provider, model
}

func (o *Orchestrator) providerModelRouteForBackend(name, modelOverride string) (string, string) {
	provider := strings.TrimSpace(name)
	model := strings.TrimSpace(modelOverride)
	if o == nil || o.cfg == nil {
		return provider, model
	}
	def, ok := o.cfg.Model.Backends[name]
	if !ok {
		return provider, model
	}
	if configuredProvider := strings.TrimSpace(def.Provider); configuredProvider != "" {
		provider = configuredProvider
	}
	if model == "" {
		model = backendConfiguredModel(def)
	}
	return provider, model
}

func backendConfiguredModel(def config.BackendDef) string {
	args := append(strings.Fields(def.Cmd), def.ExtraArgs...)
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		switch {
		case arg == "--model" || arg == "-m":
			if i+1 < len(args) {
				return strings.TrimSpace(args[i+1])
			}
		case strings.HasPrefix(arg, "--model="):
			return strings.TrimSpace(strings.TrimPrefix(arg, "--model="))
		}
	}
	if model := strings.TrimSpace(def.TierModel); model != "" {
		return model
	}
	return strings.TrimSpace(def.Model)
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

// earliestCandidateRetry returns the earliest cooldown expiry recorded across
// a selection's blocked candidates, or nil when none states one. Only
// candidates whose sole block is the cooldown carry a RetryAfter (see
// selectBackendFallback — disabled/unknown/current/already-tried short-circuit
// before the cooldown check), so the returned instant is one at which a
// candidate genuinely becomes selectable. An all-blocked retry defers to this
// instant when it beats the session backend's own expiry: waiting on the
// session backend alone parks the session for hours a sooner-freeing fallback
// could cover (#805).
func earliestCandidateRetry(selection state.BackendSelection) *time.Time {
	var earliest *time.Time
	for _, entry := range selection.CandidateScores {
		if entry.Available || entry.RetryAfter == "" {
			continue
		}
		expiry, err := time.Parse(time.RFC3339, entry.RetryAfter)
		if err != nil {
			continue
		}
		if earliest == nil || expiry.Before(*earliest) {
			earliest = &expiry
		}
	}
	return earliest
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
