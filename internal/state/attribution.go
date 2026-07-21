package state

import (
	"fmt"
	"strings"
	"time"
)

// FormatAttributionTimeline renders BackendAttribution segments in order as a
// compact, semicolon-separated internal audit line. The structured segments
// remain durable in Maestro state and are surfaced by Fleet Mission Control;
// this formatter must not be used to mutate product commits (#1000).
func FormatAttributionTimeline(attribution []BackendAttribution, now time.Time) string {
	if len(attribution) == 0 {
		return ""
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	var origin time.Time
	for _, seg := range attribution {
		if !seg.StartedAt.IsZero() {
			origin = seg.StartedAt.UTC()
			break
		}
	}
	if origin.IsZero() {
		origin = now
	}

	parts := make([]string, 0, len(attribution))
	for i, seg := range attribution {
		label := attributionSegmentLabel(seg)
		if label == "" {
			continue
		}
		rangeLabel := attributionSegmentRange(origin, seg, now)
		reason := ""
		if i > 0 {
			reason = strings.TrimSpace(attribution[i-1].EndReason)
		}
		if rangeLabel != "" && reason != "" {
			label += " (" + rangeLabel + ", " + reason + ")"
		} else if rangeLabel != "" {
			label += " (" + rangeLabel + ")"
		} else if reason != "" {
			label += " (" + reason + ")"
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, "; ")
}

func attributionSegmentLabel(seg BackendAttribution) string {
	values := []string{seg.Backend, seg.Provider, seg.Model, seg.Variant, seg.Effort}
	seen := make(map[string]struct{}, len(values))
	parts := make([]string, 0, len(values))
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
		parts = append(parts, value)
	}
	if seg.UsageUnreliable {
		parts = append(parts, "usage-unreliable")
	}
	return strings.Join(parts, " ")
}

// MarkActiveAttributionUsageUnreliable marks the currently-open backend
// segment as unable to provide trustworthy token usage. The marker is
// monotonic for the segment: a later good frame cannot reconstruct usage that
// was absent from an earlier provider response, so it must not clear the flag.
// It returns true only when durable attribution changed, which lets callers
// log the degradation once instead of on every polling cycle.
func MarkActiveAttributionUsageUnreliable(sess *Session, reason, scope string) bool {
	if sess == nil {
		return false
	}
	if len(sess.Attribution) == 0 {
		// Imported/legacy sessions can predate the attribution timeline. Create a
		// minimal segment from durable session identity rather than dropping the
		// reliability warning; newer sessions already have a metadata-rich segment.
		sess.Attribution = append(sess.Attribution, BackendAttribution{
			Backend:   strings.TrimSpace(sess.Backend),
			Model:     strings.TrimSpace(sess.Model),
			StartedAt: sess.StartedAt,
			Reason:    "usage_observation",
		})
	}
	seg := &sess.Attribution[len(sess.Attribution)-1]
	reason = strings.TrimSpace(reason)
	scope = strings.TrimSpace(scope)
	if scope == "" {
		scope = UsageUnreliableScopeAccounting
	}
	changed := false
	if !seg.UsageUnreliable {
		seg.UsageUnreliable = true
		changed = true
	}
	// Accounting loss is stronger than live-budget-only loss: once a terminal
	// result itself is incomplete, the session totals become a lower bound and
	// the active marker must upgrade accordingly.
	upgradeScope := seg.UsageUnreliableScope == "" ||
		(seg.UsageUnreliableScope == UsageUnreliableScopeLiveBudget && scope == UsageUnreliableScopeAccounting)
	if upgradeScope {
		seg.UsageUnreliableScope = scope
		changed = true
	}
	if reason != "" && (seg.UsageUnreliableReason == "" || upgradeScope) {
		seg.UsageUnreliableReason = reason
		changed = true
	}
	return changed
}

// SessionUsageUnreliable reports whether any backend segment in the session
// lost trustworthy usage telemetry. Session token/cost totals span all
// segments, so one unreliable segment makes the aggregate a lower bound.
func SessionUsageUnreliable(sess *Session) bool {
	if sess == nil {
		return false
	}
	for _, seg := range sess.Attribution {
		if seg.UsageUnreliable {
			return true
		}
	}
	return false
}

// SessionUsageAccountingUnreliable reports whether session-level token/cost
// totals are a lower bound. A live_budget-only marker does not qualify because
// a later terminal result can still make accounting exact even though the live
// worker ceiling could not be enforced response-by-response.
func SessionUsageAccountingUnreliable(sess *Session) bool {
	if sess == nil {
		return false
	}
	for _, seg := range sess.Attribution {
		if seg.UsageUnreliable && seg.UsageUnreliableScope != UsageUnreliableScopeLiveBudget {
			return true
		}
	}
	return false
}

func attributionSegmentRange(origin time.Time, seg BackendAttribution, now time.Time) string {
	if seg.StartedAt.IsZero() {
		return ""
	}
	start := attributionElapsedLabel(seg.StartedAt.UTC().Sub(origin))
	if seg.EndedAt == nil {
		return start + "-end"
	}
	endAt := seg.EndedAt.UTC()
	if endAt.Before(seg.StartedAt.UTC()) {
		return start + "-end"
	}
	return start + "-" + attributionElapsedLabel(endAt.Sub(origin))
}

func attributionElapsedLabel(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	seconds := int(d.Round(time.Second).Seconds())
	if seconds == 0 {
		return "0"
	}
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	hours := minutes / 60
	rem := minutes % 60
	if rem > 0 {
		return fmt.Sprintf("%dh %dm", hours, rem)
	}
	return fmt.Sprintf("%dh", hours)
}
