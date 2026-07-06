package digest

import (
	"fmt"
	"strings"
)

// Markdown renders the full report as a vault-friendly Markdown document.
// Every item links to its GitHub issue/PR when a URL is known.
func (r *Report) Markdown() string {
	var b strings.Builder
	day := r.GeneratedAt.Format("2006-01-02")
	fmt.Fprintf(&b, "# Maestro morning digest — %s\n\n", day)
	fmt.Fprintf(&b, "Generated %s · %d project(s) · **%d decision(s) needed** · %d promotable\n\n",
		r.GeneratedAt.Format("2006-01-02 15:04 MST"), len(r.Projects), r.DecideTodayCount(), r.PromotableCount())
	fmt.Fprintf(&b, "GitHub auth: %s\n\n", r.Auth.Line())
	fmt.Fprintf(&b, "GitHub reads: %s\n\n", r.GitHub.Line())

	fmt.Fprintf(&b, "## 1. Decide today (%d)\n\n", r.DecideTodayCount())
	if r.DecideTodayCount() == 0 {
		b.WriteString("Nothing needs an operator decision. ✅\n\n")
	} else {
		i := 0
		for _, p := range r.Projects {
			for _, item := range p.DecideToday {
				i++
				writeItemLine(&b, i, item)
			}
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "## 2. Promotable (%d)\n\n", r.PromotableCount())
	if r.PromotableCount() == 0 {
		b.WriteString("No promotable candidates found.\n\n")
	} else {
		b.WriteString("Open issues without the ready label that look runnable, ranked by how self-contained they appear:\n\n")
		i := 0
		for _, p := range r.Projects {
			for _, item := range p.Promotable {
				i++
				writeItemLine(&b, i, item)
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("## 3. Fleet health (24h)\n\n")
	for _, p := range r.Projects {
		fmt.Fprintf(&b, "- **%s** — %s\n", p.Name, p.Health.Line())
	}
	b.WriteString("\n")

	var errLines []string
	for _, p := range r.Projects {
		for _, e := range p.Errors {
			errLines = append(errLines, fmt.Sprintf("- **%s**: %s", p.Name, e))
		}
	}
	if len(errLines) > 0 {
		b.WriteString("## Collection warnings\n\n")
		b.WriteString("Some data sources were unavailable; the sections above may be incomplete.\n\n")
		b.WriteString(strings.Join(errLines, "\n"))
		b.WriteString("\n")
	}

	return b.String()
}

func writeItemLine(b *strings.Builder, i int, item Item) {
	title := item.Title
	if item.URL != "" {
		title = fmt.Sprintf("[%s](%s)", title, item.URL)
	}
	fmt.Fprintf(b, "%d. **[%s]** %s", i, item.Project, title)
	if item.Detail != "" {
		fmt.Fprintf(b, " — %s", item.Detail)
	}
	if item.Kind == KindPromotable {
		fmt.Fprintf(b, " _(score %.0f)_", item.Score)
	}
	b.WriteString("\n")
}

// NotifySummary renders the short notifier (Telegram) message. reportPath is
// appended when the Markdown report was written to disk; pass "" otherwise.
func (r *Report) NotifySummary(reportPath string) string {
	counts := map[ItemKind]int{}
	for _, p := range r.Projects {
		for _, item := range p.DecideToday {
			counts[item.Kind]++
		}
	}
	var parts []string
	if n := counts[KindPendingApproval]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d approval(s)", n))
	}
	if n := counts[KindRetryExhaustedPR]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d retry-exhausted PR(s)", n))
	}
	if n := counts[KindUnblockedIssue]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d unblocked issue(s)", n))
	}
	if n := counts[KindStaleReviewPR]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d stale review PR(s)", n))
	}

	msg := fmt.Sprintf("🌅 maestro digest %s: %d decision(s) need you",
		r.GeneratedAt.Format("2006-01-02"), r.DecideTodayCount())
	if len(parts) > 0 {
		msg += " (" + strings.Join(parts, ", ") + ")"
	}
	msg += fmt.Sprintf(" · %d promotable", r.PromotableCount())
	if reportPath != "" {
		msg += " · report: " + reportPath
	}
	return msg
}
