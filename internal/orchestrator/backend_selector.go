package orchestrator

import (
	"sort"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/state"
)

const selectionReasonProviderLimitFallback = "fallback_after_provider_limit"

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

func (o *Orchestrator) selectProviderLimitFallback(st *state.State, sess *state.Session, now time.Time) state.BackendSelection {
	selection := state.BackendSelection{
		SelectionReason: selectionReasonProviderLimitFallback,
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
