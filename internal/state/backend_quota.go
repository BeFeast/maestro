package state

import (
	"sort"
	"time"
)

// Subscription window durations for quota tracking (#704). Claude-style
// subscription plans meter a 5-hour session window that starts at first
// use plus a weekly cap; both are modeled as rolling windows anchored at
// the first tokens observed after the previous window expired.
const (
	BackendQuotaWindow = 5 * time.Hour
	BackendQuotaWeek   = 7 * 24 * time.Hour
)

// BackendQuotaUsage tracks estimated subscription-window token usage for
// one backend (#704). Counters accrue from the per-session token totals
// maestro already records (AccrueBackendQuotaUsage), so they are an
// estimate of the provider's own metering — calibrated by the operator
// via config.BackendQuota capacities. Each window is rolling: StartedAt
// anchors at the first tokens observed after the previous window
// expired, and the counter zeroes once the window duration elapses.
type BackendQuotaUsage struct {
	WindowStartedAt *time.Time `json:"window_started_at,omitempty"`
	WindowTokens    int        `json:"window_tokens,omitempty"`
	WeekStartedAt   *time.Time `json:"week_started_at,omitempty"`
	WeekTokens      int        `json:"week_tokens,omitempty"`
	// UpdatedAt orders concurrent snapshot merges (latest-write-wins);
	// the orchestrator is the only writer of these counters.
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// AccrueBackendQuotaUsage folds newly observed session tokens into the
// per-backend quota windows. It walks every session, takes the delta
// between TokensUsedTotal and the portion already accounted
// (Session.QuotaTokensAccounted), and adds it to the session backend's
// rolling windows — so it is safe to call once per orchestrator cycle
// regardless of where token counters get stamped. Expired windows are
// zeroed even when no new tokens arrived so pressure self-clears at
// reset. Returns true when anything changed.
func AccrueBackendQuotaUsage(s *State, now time.Time) bool {
	if s == nil {
		return false
	}
	now = now.UTC()
	changed := false
	for name, usage := range s.BackendQuotaUsage {
		if usage == nil {
			delete(s.BackendQuotaUsage, name)
			changed = true
			continue
		}
		if rollBackendQuotaWindows(usage, now) {
			usage.UpdatedAt = now
			changed = true
		}
	}
	for _, sess := range s.Sessions {
		if sess == nil || sess.Backend == "" {
			continue
		}
		delta := sess.TokensUsedTotal - sess.QuotaTokensAccounted
		if delta == 0 {
			continue
		}
		if delta < 0 {
			// Counter went backwards (manual state surgery); re-baseline
			// without un-counting tokens already attributed to a window.
			sess.QuotaTokensAccounted = sess.TokensUsedTotal
			changed = true
			continue
		}
		if s.BackendQuotaUsage == nil {
			s.BackendQuotaUsage = make(map[string]*BackendQuotaUsage)
		}
		usage, ok := s.BackendQuotaUsage[sess.Backend]
		if !ok {
			usage = &BackendQuotaUsage{}
			s.BackendQuotaUsage[sess.Backend] = usage
		}
		rollBackendQuotaWindows(usage, now)
		if usage.WindowStartedAt == nil {
			t := now
			usage.WindowStartedAt = &t
		}
		if usage.WeekStartedAt == nil {
			t := now
			usage.WeekStartedAt = &t
		}
		usage.WindowTokens += delta
		usage.WeekTokens += delta
		usage.UpdatedAt = now
		sess.QuotaTokensAccounted = sess.TokensUsedTotal
		changed = true
	}
	return changed
}

// rollBackendQuotaWindows zeroes any window whose duration has elapsed.
// The next accrued delta re-anchors the window at its own timestamp,
// matching "5h rolling from first use".
func rollBackendQuotaWindows(u *BackendQuotaUsage, now time.Time) bool {
	changed := false
	if u.WindowStartedAt != nil && !now.Before(u.WindowStartedAt.Add(BackendQuotaWindow)) {
		u.WindowStartedAt = nil
		u.WindowTokens = 0
		changed = true
	}
	if u.WeekStartedAt != nil && !now.Before(u.WeekStartedAt.Add(BackendQuotaWeek)) {
		u.WeekStartedAt = nil
		u.WeekTokens = 0
		changed = true
	}
	return changed
}

// BackendQuotaStatus is the evaluated quota position of one backend at a
// point in time: how full each window is against its calibrated
// capacity, when each window resets, and whether usage has crossed the
// dispatch threshold (Pressured).
type BackendQuotaStatus struct {
	WindowCapTokens  int
	WindowUsedTokens int
	WindowPercent    float64 // 0-100; 0 when no window capacity is configured
	WindowResetAt    *time.Time
	WeeklyCapTokens  int
	WeekUsedTokens   int
	WeekPercent      float64 // 0-100; 0 when no weekly capacity is configured
	WeekResetAt      *time.Time
	Threshold        float64 // used fraction (0..1) that triggers pressure
	Pressured        bool
	// PressureResetAt is when pressure is expected to relieve: the
	// latest reset among the windows that are over threshold. Nil when
	// not pressured.
	PressureResetAt *time.Time
}

// EvaluateBackendQuota computes the quota status for one backend from
// its persisted usage counters and the operator-calibrated capacities.
// usage may be nil (no tokens observed yet). Expired windows count as
// empty without mutating the stored counters, so read-only consumers
// (fleet API, supervisor) see fresh numbers between orchestrator cycles.
func EvaluateBackendQuota(usage *BackendQuotaUsage, windowCap, weeklyCap int, threshold float64, now time.Time) BackendQuotaStatus {
	now = now.UTC()
	status := BackendQuotaStatus{
		WindowCapTokens: windowCap,
		WeeklyCapTokens: weeklyCap,
		Threshold:       threshold,
	}
	if usage != nil {
		if usage.WindowStartedAt != nil {
			if resetAt := usage.WindowStartedAt.Add(BackendQuotaWindow); now.Before(resetAt) {
				status.WindowUsedTokens = usage.WindowTokens
				status.WindowResetAt = &resetAt
			}
		}
		if usage.WeekStartedAt != nil {
			if resetAt := usage.WeekStartedAt.Add(BackendQuotaWeek); now.Before(resetAt) {
				status.WeekUsedTokens = usage.WeekTokens
				status.WeekResetAt = &resetAt
			}
		}
	}
	if windowCap > 0 {
		status.WindowPercent = 100 * float64(status.WindowUsedTokens) / float64(windowCap)
	}
	if weeklyCap > 0 {
		status.WeekPercent = 100 * float64(status.WeekUsedTokens) / float64(weeklyCap)
	}
	if threshold <= 0 {
		return status
	}
	limit := threshold * 100
	if windowCap > 0 && status.WindowPercent >= limit {
		status.Pressured = true
		status.PressureResetAt = laterTime(status.PressureResetAt, status.WindowResetAt)
	}
	if weeklyCap > 0 && status.WeekPercent >= limit {
		status.Pressured = true
		status.PressureResetAt = laterTime(status.PressureResetAt, status.WeekResetAt)
	}
	return status
}

func laterTime(a, b *time.Time) *time.Time {
	if a == nil {
		return b
	}
	if b == nil || a.After(*b) {
		return a
	}
	return b
}

// mergeBackendQuotaUsage resolves concurrent snapshot writes per backend
// latest-write-wins by UpdatedAt. The orchestrator is the only writer,
// so this only arbitrates between an orchestrator save and a stale
// snapshot from another process re-saving unrelated fields.
func mergeBackendQuotaUsage(current, ours map[string]*BackendQuotaUsage) map[string]*BackendQuotaUsage {
	if len(current) == 0 && len(ours) == 0 {
		return nil
	}
	merged := make(map[string]*BackendQuotaUsage)
	for _, key := range unionBackendQuotaKeys(current, ours) {
		currentValue := current[key]
		oursValue := ours[key]
		switch {
		case currentValue != nil && oursValue != nil:
			if oursValue.UpdatedAt.After(currentValue.UpdatedAt) {
				merged[key] = oursValue
			} else {
				merged[key] = currentValue
			}
		case currentValue != nil:
			merged[key] = currentValue
		case oursValue != nil:
			merged[key] = oursValue
		}
	}
	return merged
}

func unionBackendQuotaKeys(maps ...map[string]*BackendQuotaUsage) []string {
	seen := make(map[string]struct{})
	for _, m := range maps {
		for k := range m {
			seen[k] = struct{}{}
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
