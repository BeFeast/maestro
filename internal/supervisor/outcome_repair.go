package supervisor

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/befeast/maestro/internal/config"
	"github.com/befeast/maestro/internal/github"
	"github.com/befeast/maestro/internal/notify"
	"github.com/befeast/maestro/internal/outcome"
	"github.com/befeast/maestro/internal/state"
)

const (
	outcomeRepairExcerptLimit     = 2000
	outcomeRepairLabelColor       = "D93F0B"
	outcomeRepairLabelDescription = "Repair work filed after bounded automatic outcome recovery is exhausted"
)

var (
	errOutcomeRepairNoLongerCurrent = errors.New("outcome repair cap is no longer current")
	outcomeRepairDispatchMu         sync.Mutex
	outcomeRepairURLPattern         = regexp.MustCompile("https?://[^\\s<>\\\"'`]+")
	outcomeRepairHeaderPattern      = regexp.MustCompile(`(?im)\b(?:cookie|set-cookie|proxy-authorization|x-api-key|x-auth-token)\s*:\s*[^\r\n]+`)
)

type outcomeRepairIssueClient interface {
	ListAllOpenIssues(labels []string) ([]github.Issue, error)
	CreateIssue(title, body string, labels []string) (int, error)
	EditIssueBody(number int, body string) error
	AddIssueLabel(issueNumber int, label string) error
	EnsureLabel(name, color, description string) error
	CloseIssue(number int, comment string) error
}

type futileRecoveryNotifier interface {
	Alert(class notify.AlertClass, key, title, body string) error
}

type futileRecoveryEvent struct {
	Gate        string
	Fingerprint string
	Attempts    int
	Issue       int
	IssueLink   string
}

var (
	newOutcomeRepairIssueClient = func(repo string) outcomeRepairIssueClient {
		return github.New(repo)
	}
	newFutileRecoveryNotifier = func(cfg *config.Config) futileRecoveryNotifier {
		n := notify.NewWithToken(cfg.Telegram.BotToken, cfg.Telegram.Target, cfg.Telegram.Mode, cfg.Telegram.OpenclawURL)
		return n.WithNtfy(cfg.Notify.Ntfy.BaseURL, cfg.Notify.Ntfy.Topic, cfg.Notify.Ntfy.Token())
	}
)

// dispatchFutileOutcomeRecovery consumes a durable cap hit. It ensures one
// fingerprint-marked repair issue exists, persists its number, then emits one
// futile_recovery alert. Each success marker is written only after its side
// effect completes so transient GitHub/notify failures retry on the next
// supervisor cycle without creating a duplicate issue.
func dispatchFutileOutcomeRecovery(cfg *config.Config, now time.Time) {
	if cfg == nil || strings.TrimSpace(cfg.StateDir) == "" || strings.TrimSpace(cfg.Repo) == "" {
		return
	}
	// Cap transitions are rare, but multiple supervisor loops can observe the
	// same durable cap in one process. Serialize the GitHub side effect so both
	// cannot list-before-create and race into duplicate issues.
	outcomeRepairDispatchMu.Lock()
	defer outcomeRepairDispatchMu.Unlock()

	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	st, err := state.Load(cfg.StateDir)
	if err != nil {
		log.Printf("[outcome/recovery] futile intake state load failed: %v", err)
		return
	}
	recovery := st.OutcomeRecovery
	health := st.OutcomeHealth
	if recovery == nil || recovery.Status != outcome.RecoveryStatusCapped || health == nil || health.State != outcome.HealthFailing {
		return
	}
	fingerprint := strings.TrimSpace(recovery.FailureFingerprint)
	if fingerprint == "" || fingerprint != state.OutcomeFailureFingerprint(*health) {
		return
	}

	gate := outcomeRepairSafeInline(outcome.FailingGateName(*health), 160)
	if gate == "" {
		gate = "outcome"
	}
	attempts := recovery.ConsecutiveFailures
	issue := 0
	if recovery.RepairFingerprint == fingerprint {
		issue = recovery.RepairIssue
	}
	if issue <= 0 {
		current, err := outcomeRepairCapCurrent(cfg.StateDir, fingerprint)
		if err != nil {
			log.Printf("[outcome/recovery] futile repair intake revalidation failed: %v", err)
			return
		}
		if !current {
			return
		}

		client := newOutcomeRepairIssueClient(cfg.Repo)
		title := fmt.Sprintf("Outcome recovery blocked: %s is failing — repair delivery infrastructure", gate)
		body := outcomeRepairIssueBody(gate, fingerprint, attempts, *health)
		labels := outcomeRepairLabels(client, cfg)
		created := false
		issue, created, err = ensureOutcomeRepairIssue(client, title, body, labels, fingerprint, func() (bool, error) {
			return outcomeRepairCapCurrent(cfg.StateDir, fingerprint)
		})
		if err != nil {
			if errors.Is(err, errOutcomeRepairNoLongerCurrent) {
				return
			}
			log.Printf("[outcome/recovery] futile repair intake failed for %s: %v", gate, err)
			return
		}
		linked := false
		if err := state.Update(cfg.StateDir, func(latest *state.State) error {
			current := latest.OutcomeRecovery
			if !outcomeRepairStateCurrent(latest, fingerprint) {
				return state.ErrNoStateChange
			}
			if current.RepairIssue > 0 && current.RepairFingerprint == fingerprint {
				issue = current.RepairIssue
				linked = true
				return state.ErrNoStateChange
			}
			current.RepairIssue = issue
			current.RepairFingerprint = fingerprint
			current.UpdatedAt = now
			linked = true
			return nil
		}); err != nil {
			log.Printf("[outcome/recovery] repair issue #%d filed but linkage persistence failed: %v", issue, err)
			return
		}
		if !linked {
			// The cap changed after the final pre-create revalidation. If this call
			// created a new issue, retire that obsolete side effect; an existing
			// fingerprint-marked issue is historical and must not be closed here.
			if created {
				if err := client.CloseIssue(issue, ""); err != nil {
					log.Printf("[outcome/recovery] obsolete repair issue #%d could not be closed after cap changed: %v", issue, err)
				} else {
					log.Printf("[outcome/recovery] closed obsolete repair issue #%d after capped fingerprint changed before linkage", issue)
				}
			}
			return
		}
		log.Printf("[outcome/recovery] cap-hit fingerprint=%s filed/refreshed repair issue #%d", shortOutcomeFingerprint(fingerprint), issue)
	}

	current, err := outcomeRepairNotificationPending(cfg.StateDir, fingerprint, issue)
	if err != nil {
		log.Printf("[outcome/recovery] futile_recovery notification revalidation failed: %v", err)
		return
	}
	if !current {
		return
	}
	event := futileRecoveryEvent{
		Gate:        gate,
		Fingerprint: fingerprint,
		Attempts:    attempts,
		Issue:       issue,
		IssueLink:   outcomeRepairIssueLink(cfg.Repo, issue),
	}
	if err := notifyFutileRecovery(cfg, event); err != nil {
		log.Printf("[outcome/recovery] futile_recovery alert failed for %s: %v", gate, err)
		return
	}
	marked := false
	if err := state.Update(cfg.StateDir, func(latest *state.State) error {
		current := latest.OutcomeRecovery
		if !outcomeRepairStateCurrent(latest, fingerprint) || current.RepairIssue != issue || current.RepairFingerprint != fingerprint {
			return state.ErrNoStateChange
		}
		current.NotifiedFingerprint = fingerprint
		current.UpdatedAt = now
		marked = true
		return nil
	}); err != nil {
		log.Printf("[outcome/recovery] futile_recovery alert sent but notification marker persistence failed: %v", err)
		return
	}
	if !marked {
		return
	}
	log.Printf("[outcome/recovery] futile_recovery gate=%s attempts=%d issue=%s", gate, attempts, event.IssueLink)
}

func outcomeRepairStateCurrent(st *state.State, fingerprint string) bool {
	if st == nil || st.OutcomeRecovery == nil || st.OutcomeHealth == nil {
		return false
	}
	recovery := st.OutcomeRecovery
	return recovery.Status == outcome.RecoveryStatusCapped &&
		recovery.FailureFingerprint == fingerprint &&
		st.OutcomeHealth.State == outcome.HealthFailing &&
		state.OutcomeFailureFingerprint(*st.OutcomeHealth) == fingerprint
}

// outcomeRepairCapCurrent performs a lock-protected no-op compare-and-swap.
// The check runs immediately before a remote issue mutation, closing the stale
// snapshot window called out in PR review without holding the state lock across
// a network call.
func outcomeRepairCapCurrent(stateDir, fingerprint string) (bool, error) {
	current := false
	err := state.Update(stateDir, func(latest *state.State) error {
		current = outcomeRepairStateCurrent(latest, fingerprint)
		return state.ErrNoStateChange
	})
	return current, err
}

func outcomeRepairNotificationPending(stateDir, fingerprint string, issue int) (bool, error) {
	pending := false
	err := state.Update(stateDir, func(latest *state.State) error {
		if !outcomeRepairStateCurrent(latest, fingerprint) {
			return state.ErrNoStateChange
		}
		recovery := latest.OutcomeRecovery
		pending = recovery.RepairIssue == issue &&
			recovery.RepairFingerprint == fingerprint &&
			recovery.NotifiedFingerprint != fingerprint
		return state.ErrNoStateChange
	})
	return pending, err
}

func notifyFutileRecovery(cfg *config.Config, event futileRecoveryEvent) error {
	if cfg == nil {
		return nil
	}
	project := strings.TrimSpace(cfg.Repo)
	title := "maestro futile recovery"
	if project != "" {
		title += ": " + project
	}
	body := fmt.Sprintf("outcome gate %s is still failing after %d recovery attempts; repair issue %s", event.Gate, event.Attempts, event.IssueLink)
	key := project + ":" + outcomeRepairFingerprintID(event.Fingerprint)
	return newFutileRecoveryNotifier(cfg).Alert(notify.AlertFutileRecovery, key, title, body)
}

// ensureOutcomeRepairIssue performs GitHub-side dedup before create. This
// closes the crash window where GitHub accepted an issue but Maestro restarted
// before persisting its number: the next cycle finds the hidden fingerprint
// marker, refreshes the body/labels, and reuses that issue.
func ensureOutcomeRepairIssue(client outcomeRepairIssueClient, title, body string, labels []string, fingerprint string, stillCurrent func() (bool, error)) (int, bool, error) {
	if client == nil {
		return 0, false, fmt.Errorf("outcome repair intake: nil issue client")
	}
	marker := outcomeRepairMarker(fingerprint)
	issues, err := client.ListAllOpenIssues(nil)
	if err != nil {
		return 0, false, fmt.Errorf("list open issues for dedup: %w", err)
	}
	for _, issue := range issues {
		if !strings.Contains(issue.Body, marker) {
			continue
		}
		needsRefresh := issue.Body != body
		for _, label := range labels {
			if strings.TrimSpace(label) != "" && !github.HasLabel(issue, []string{label}) {
				needsRefresh = true
			}
		}
		if needsRefresh {
			if err := requireCurrentOutcomeRepair(stillCurrent); err != nil {
				return 0, false, err
			}
		}
		if issue.Body != body {
			if err := client.EditIssueBody(issue.Number, body); err != nil {
				return 0, false, fmt.Errorf("refresh outcome repair issue #%d: %w", issue.Number, err)
			}
		}
		for _, label := range labels {
			if strings.TrimSpace(label) == "" || github.HasLabel(issue, []string{label}) {
				continue
			}
			if err := client.AddIssueLabel(issue.Number, label); err != nil {
				return 0, false, fmt.Errorf("add label %q to outcome repair issue #%d: %w", label, issue.Number, err)
			}
		}
		return issue.Number, false, nil
	}
	if err := requireCurrentOutcomeRepair(stillCurrent); err != nil {
		return 0, false, err
	}
	number, err := client.CreateIssue(title, body, labels)
	if err != nil {
		return 0, false, fmt.Errorf("create outcome repair issue: %w", err)
	}
	return number, true, nil
}

func requireCurrentOutcomeRepair(stillCurrent func() (bool, error)) error {
	if stillCurrent == nil {
		return nil
	}
	current, err := stillCurrent()
	if err != nil {
		return fmt.Errorf("revalidate capped outcome recovery: %w", err)
	}
	if !current {
		return errOutcomeRepairNoLongerCurrent
	}
	return nil
}

func outcomeRepairLabels(client outcomeRepairIssueClient, cfg *config.Config) []string {
	labels := make([]string, 0, 2)
	if err := client.EnsureLabel(outcome.OutcomeRepairLabel, outcomeRepairLabelColor, outcomeRepairLabelDescription); err != nil {
		// The classification label is optional for intake availability. The
		// hidden fingerprint marker is also recognized by dispatch, so a repo
		// where label provisioning is forbidden still gets dispatchable repair
		// work with the mandatory project ready label.
		log.Printf("[outcome/recovery] outcome-repair label unavailable; continuing with marker-based classification: %v", err)
	} else {
		labels = append(labels, outcome.OutcomeRepairLabel)
	}
	ready := outcomeRepairReadyLabel(cfg)
	if ready != "" && !strings.EqualFold(ready, outcome.OutcomeRepairLabel) {
		labels = append(labels, ready)
	}
	return labels
}

func outcomeRepairReadyLabel(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if label := strings.TrimSpace(cfg.Supervisor.ReadyLabel); label != "" {
		return label
	}
	for _, label := range cfg.IssueLabels {
		if label = strings.TrimSpace(label); label != "" {
			return label
		}
	}
	return ""
}

func outcomeRepairIssueBody(gate, fingerprint string, attempts int, health outcome.HealthCheckResult) string {
	gate = outcomeRepairSafeInline(gate, 160)
	if gate == "" {
		gate = "outcome"
	}
	summary := outcomeRepairSafeInline(health.Summary, 512)
	excerpt := summary
	// URL checks persist a bounded raw response body for local diagnosis. That
	// body is not a safe publication source because an opaque value can be a
	// credential without carrying a recognizable key name. Publish only the
	// sanitized status summary for this signal; command-check Detail is already
	// an allow-listed structured projection from Checker.projectStructuredHealth.
	if !strings.EqualFold(strings.TrimSpace(health.Signal), "healthcheck_url") {
		if detail := strings.TrimSpace(health.Detail); detail != "" {
			excerpt = detail
		}
	}
	excerpt = outcomeRepairSafeText(excerpt, outcomeRepairExcerptLimit)

	var b strings.Builder
	fmt.Fprintf(&b, "Automatic outcome recovery stopped after %d consecutive attempts left the same blocking outcome failure in place.\n\n", attempts)
	b.WriteString("A normal worker should repair the product/delivery infrastructure. The project-scoped recovery actuator remains the only owner of the configured recovery command.\n\n")
	b.WriteString("## Failure\n\n")
	fmt.Fprintf(&b, "- Gate/check: `%s`\n", gate)
	if signal := outcomeRepairSafeInline(health.Signal, 160); signal != "" && signal != gate {
		fmt.Fprintf(&b, "- Signal: `%s`\n", signal)
	}
	fmt.Fprintf(&b, "- Consecutive failing verifications: %d\n", attempts)
	if summary != "" {
		fmt.Fprintf(&b, "- Summary: %s\n", summary)
	}
	if excerpt != "" {
		b.WriteString("\n## Bounded redacted excerpt\n\n")
		for _, line := range strings.Split(excerpt, "\n") {
			fmt.Fprintf(&b, "> %s\n", line)
		}
	}
	fmt.Fprintf(&b, "\n%s\n", outcomeRepairMarker(fingerprint))
	return b.String()
}

// outcomeRepairSafeText is an explicit publication boundary. Persisted health
// details are useful for local diagnosis but are not assumed safe for a public
// GitHub issue: common credentials and secret-bearing headers are redacted,
// URL userinfo/query/fragment data is stripped, control characters are
// discarded, and the result is bounded.
func outcomeRepairSafeText(text string, limit int) string {
	text = RedactSensitive(text)
	text = outcomeRepairHeaderPattern.ReplaceAllStringFunc(text, func(line string) string {
		if i := strings.Index(line, ":"); i >= 0 {
			return line[:i+1] + " [REDACTED]"
		}
		return "[REDACTED]"
	})
	text = outcomeRepairURLPattern.ReplaceAllStringFunc(text, func(raw string) string {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" {
			return "[REDACTED_URL]"
		}
		parsed.User = nil
		parsed.Path = ""
		parsed.RawPath = ""
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		return parsed.String()
	})
	text = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, text)
	return truncateText(strings.TrimSpace(text), limit)
}

func outcomeRepairSafeInline(text string, limit int) string {
	text = outcomeRepairSafeText(text, limit)
	text = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ", "`", "'").Replace(text)
	return strings.Join(strings.Fields(text), " ")
}

func outcomeRepairMarker(fingerprint string) string {
	return fmt.Sprintf("%s%s -->", outcome.OutcomeRepairMarkerPrefix, outcomeRepairFingerprintID(fingerprint))
}

func outcomeRepairFingerprintID(fingerprint string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(fingerprint)))
	return fmt.Sprintf("%x", digest[:8])
}

func shortOutcomeFingerprint(fingerprint string) string {
	return outcomeRepairFingerprintID(fingerprint)
}

func outcomeRepairIssueLink(repo string, number int) string {
	if repo = strings.TrimSpace(repo); repo != "" && number > 0 {
		return fmt.Sprintf("https://github.com/%s/issues/%d", repo, number)
	}
	return fmt.Sprintf("#%d", number)
}
