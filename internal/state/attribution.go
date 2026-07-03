package state

import (
	"fmt"
	"strings"
	"time"
)

const AttributionTrailerKey = "Maestro-Backend"

// FormatAttributionTrailer renders the durable git commit trailer for a
// session's backend timeline. It uses relative ranges from the first segment
// so the line remains stable and queryable after the live state file is gone.
// The trailer goes on commit messages only — never on PR bodies, which land on
// the target repo and may be public (#799).
func FormatAttributionTrailer(attribution []BackendAttribution, now time.Time) string {
	timeline := FormatAttributionTimeline(attribution, now)
	if timeline == "" {
		return ""
	}
	return AttributionTrailerKey + ": " + timeline
}

// FormatAttributionTimeline renders BackendAttribution segments in order as a
// compact, semicolon-separated audit line.
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
	return strings.Join(parts, " ")
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

// AppendAttributionTrailer appends the attribution trailer to text unless the
// trailer is empty or already present.
func AppendAttributionTrailer(text string, attribution []BackendAttribution, now time.Time) string {
	trailer := FormatAttributionTrailer(attribution, now)
	if trailer == "" || strings.Contains(text, AttributionTrailerKey+":") {
		return text
	}
	text = strings.TrimRight(text, " \t\r\n")
	if text == "" {
		return trailer + "\n"
	}
	return text + "\n\n" + trailer + "\n"
}
